// Command conductor-backup is a conductor connector for versioned backups
// across one or more storage TIERS, each with its own driver and retention:
//
//	local   a directory (any mounted path)            — age encryption optional
//	rclone  any rclone backend (S3-compatible, SFTP, B2, WebDAV, SMB, Drive, …)
//	                                                  — age encryption required unless opted out
//	restic  any restic repository (path, sftp:, s3:, b2:, rest:, rclone:)
//	                                                  — restic's own encryption + dedup
//	borg    a borg repository (path or ssh://)        — borg's own encryption + dedup
//
// A backup SET is a named series of snapshots. `put` writes one snapshot to
// every tier (or the ones named), skipping a tier whose newest snapshot is
// byte-identical (dedup), then applies that tier's retention: the newest of
// each of the last keep_daily days, keep_weekly ISO weeks, keep_monthly
// months, plus the newest overall. Every snapshot records its plaintext
// sha256 and size, so `verify mode: restore` proves a restore returns
// exactly what was written; `verify mode: check` runs the backend's own
// consistency check.
//
// Jail: the plugin may write only under allow_paths and reach only
// allow_hosts (host:port) — declare there every local path and remote host
// your tiers use. It runs restic, rclone, borg and ssh (whichever are
// installed); ssh's own connection is tunnelled through conductor's egress
// proxy by this binary (`__connect` mode).
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

const defaultMaxRead = 64 << 20

type backupPlugin struct {
	now     func() time.Time
	timeout time.Duration
}

func newBackupPlugin() *backupPlugin { return &backupPlugin{now: time.Now, timeout: 30 * time.Minute} }

func bgCtx() context.Context { return context.Background() }

func (p *backupPlugin) Describe() plugin.Decl {
	set := plugin.Field{Type: "string", Required: true, Desc: "backup set name: letters, digits, . _ - (e.g. calendar-work)"}
	tiers := plugin.Field{Type: "list", Desc: "tier names to act on (default: every tier)"}
	tierOne := plugin.Field{Type: "string", Desc: "tier name (default: the first tier)"}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "backup",
		Desc: "Versioned backups across storage tiers (local dir, rclone, restic, borg) with per-tier daily/weekly/monthly retention, encryption, dedup and restore verification.",
		Connection: plugin.Schema{
			"tiers": {Type: "list", Desc: "destinations, each {name, driver: local|rclone|restic|borg, keep_daily/weekly/monthly, encrypt, …driver fields}: " +
				"local {dir, age_recipients, age_identity}; rclone {remote, env, ssh, age_recipients, age_identity}; " +
				"restic {repo, password, env, ssh, init, check_read_data}; borg {repo, passphrase, env, ssh, init, check_data}. " +
				"ssh: {key, known_hosts, port, insecure_accept_host_key}. Put secrets in the vault."},
			"dir":          {Type: "string", Desc: "shorthand for a single local tier at this path"},
			"keep_daily":   {Type: "integer", Desc: "default for every tier (7)"},
			"keep_weekly":  {Type: "integer", Desc: "default for every tier (2)"},
			"keep_monthly": {Type: "integer", Desc: "default for every tier (6)"},
			"allow_paths":  {Type: "list", Desc: "local paths the plugin may write: every local dir and local restic/borg repo must be under one"},
			"allow_hosts":  {Type: "list", Desc: "host:port destinations the plugin may reach (S3 endpoint, SFTP/SSH server, …)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "put", Desc: "write one snapshot to the tiers, skip tiers where it is unchanged, then apply each tier's retention",
				Options: plugin.Schema{
					"set": set, "tiers": tiers,
					"text":           {Type: "string", Desc: "content as text"},
					"base64":         {Type: "string", Desc: "content as base64; exactly one of text/base64"},
					"ext":            {Type: "string", Desc: "file extension, e.g. ics, json, tar.gz (default txt)"},
					"skip_unchanged": {Type: "boolean", Desc: "skip a tier whose newest snapshot has the same content (default true)"},
				},
				Outputs: plugin.Schema{
					"sha256": {Type: "string"}, "size": {Type: "integer"},
					"tiers": {Type: "list", Desc: "[{tier, status: written|unchanged, name, pruned}]"},
				},
			},
			{
				Name: "list", Desc: "a set's snapshots on one tier, newest first",
				Options: plugin.Schema{"set": set, "tier": tierOne},
				Outputs: plugin.Schema{"items": {Type: "list", Desc: "[{name, time, size, sha256, encryption}]"}},
			},
			{
				Name: "read", Desc: "restore a snapshot's content (newest unless name is given), integrity-checked against its recorded sha256",
				Options: plugin.Schema{
					"set": set, "tier": tierOne,
					"name":      {Type: "string", Desc: "snapshot name from list (default newest)"},
					"encoding":  {Type: "string", Enum: []string{"text", "base64"}, Desc: "default text"},
					"max_bytes": {Type: "integer", Desc: "refuse larger content (default 64 MiB)"},
				},
				Outputs: plugin.Schema{"name": {Type: "string"}, "content": {Type: "string"}, "size": {Type: "integer"}, "sha256": {Type: "string"}},
			},
			{
				Name: "prune", Desc: "apply retention without writing",
				Options: plugin.Schema{"set": set, "tiers": tiers, "dry_run": {Type: "boolean", Desc: "report, delete nothing"}},
				Outputs: plugin.Schema{"tiers": {Type: "list", Desc: "[{tier, pruned, kept}]"}},
			},
			{
				Name: "verify", Desc: "prove the backups are good: check runs the backend's consistency check; restore restores snapshots and compares them to their recorded sha256/size",
				Options: plugin.Schema{
					"set": set, "tiers": tiers,
					"mode":   {Type: "string", Enum: []string{"check", "restore", "both"}, Desc: "default restore"},
					"sample": {Type: "integer", Desc: "restore: how many snapshots, newest first (default 1; 0 = all)"},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}, "tiers": {Type: "list", Desc: "[{tier, checked, restored: [{name, ok, error}], error}]"}},
			},
		},
		Capabilities: plugin.Capabilities{
			Commands: []string{"restic", "rclone", "borg", "ssh"},
			Write:    []string{"config:allow_paths"},
			Egress:   []string{"config:allow_hosts"},
		},
	}
}

// Validate reports every config problem at once (plugin.validate).
func (p *backupPlugin) Validate(_ context.Context, req plugin.ValidateRequest) (plugin.ValidateResult, error) {
	_, probs := parseConf(req.Config)
	var out []plugin.Problem
	for _, pr := range probs {
		out = append(out, plugin.Problem{Path: pr.path, Message: pr.msg})
	}
	return plugin.ValidateResult{Problems: out}, nil
}

func (p *backupPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	c, probs := parseConf(req.Connection)
	if len(probs) > 0 {
		return fail(probs[0].Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	set := str(o["set"])
	if !setRe.MatchString(set) {
		return fail(fmt.Sprintf("set %q: letters, digits, '.', '_' or '-' (max 100, no leading punctuation)", set))
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	var out map[string]any
	var err error
	switch req.Verb {
	case "put":
		out, err = p.put(ctx, c, set, o)
	case "list":
		out, err = p.list(ctx, c, set, o)
	case "read":
		out, err = p.read(ctx, c, set, o)
	case "prune":
		out, err = p.prune(ctx, c, set, o)
	case "verify":
		out, err = p.verify(ctx, c, set, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
	if err != nil {
		if pe, ok := err.(*plugin.Error); ok {
			return plugin.InvokeResult{}, pe
		}
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (c *conf) pick(o map[string]any) ([]*tier, error) {
	names := strList(o["tiers"])
	if len(names) == 0 {
		return c.tiers, nil
	}
	var out []*tier
	for _, n := range names {
		t, err := c.tier(n)
		if err != nil {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		out = append(out, t)
	}
	return out, nil
}

// withMeta fills sha256/size for a file-tier snapshot (others carry them
// from their listing already).
func withMeta(ctx context.Context, t *tier, set string, s *snapshot) error {
	if t.file == nil || s.SHA256 != "" {
		return nil
	}
	return t.file.meta(ctx, set, s)
}

// --- put ---

func (p *backupPlugin) put(ctx context.Context, c *conf, set string, o map[string]any) (map[string]any, error) {
	text, hasText := o["text"].(string)
	b64, hasB64 := o["base64"].(string)
	if hasText == hasB64 {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, "exactly one of text or base64 is required")
	}
	data := []byte(text)
	if hasB64 {
		var err error
		if data, err = base64.StdEncoding.DecodeString(b64); err != nil {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "base64: "+err.Error())
		}
	}
	ext := strings.TrimPrefix(strOr(str(o["ext"]), "txt"), ".")
	if !extRe.MatchString(ext) {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, fmt.Sprintf("ext %q: letters and digits, at most one inner dot", ext))
	}
	skip := o["skip_unchanged"] == nil || boolv(o["skip_unchanged"])
	tiers, err := c.pick(o)
	if err != nil {
		return nil, err
	}
	digest := sum(data)
	at := p.now().UTC().Truncate(time.Second)

	results := []any{}
	var failed []string
	for _, t := range tiers {
		r, err := putTier(ctx, t, set, at, ext, digest, data, skip)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", t.name, err))
			r = map[string]any{"tier": t.name, "status": "error", "error": err.Error()}
		}
		results = append(results, r)
	}
	if len(failed) > 0 {
		return nil, fmt.Errorf("put failed on %d of %d tier(s): %s", len(failed), len(tiers), strings.Join(failed, "; "))
	}
	return map[string]any{"sha256": digest, "size": len(data), "tiers": results}, nil
}

func putTier(ctx context.Context, t *tier, set string, at time.Time, ext, digest string, data []byte, skip bool) (map[string]any, error) {
	existing, err := t.store.list(ctx, set)
	if err != nil {
		return nil, err
	}
	if skip && len(existing) > 0 {
		newest := existing[0]
		if err := withMeta(ctx, t, set, &newest); err == nil && newest.SHA256 == digest {
			return map[string]any{"tier": t.name, "status": "unchanged", "name": newest.Name, "pruned": []any{}}, nil
		}
	}
	// The timestamp must be unique within the set on this tier, whatever the
	// extension, or two snapshots would tie for "newest".
	taken := map[time.Time]bool{}
	for _, s := range existing {
		taken[s.At] = true
	}
	for taken[at] {
		at = at.Add(time.Second)
	}
	s := snapshot{Name: snapName(set, at, ext), At: at, Size: int64(len(data)), SHA256: digest}
	if err := t.store.put(ctx, set, s, data); err != nil {
		return nil, err
	}
	pruned, _, err := applyRetention(ctx, t.store, set, t.keep, false)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s written, but retention failed: %w", s.Name, err)
	}
	return map[string]any{"tier": t.name, "status": "written", "name": s.Name, "pruned": pruned}, nil
}

// --- list / read ---

func (p *backupPlugin) list(ctx context.Context, c *conf, set string, o map[string]any) (map[string]any, error) {
	t, err := c.tier(str(o["tier"]))
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	ss, err := t.store.list(ctx, set)
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(ss))
	for _, s := range ss {
		_ = withMeta(ctx, t, set, &s) // best effort: an encrypted meta needs the identity
		items = append(items, s.out())
	}
	return map[string]any{"items": items}, nil
}

func find(ss []snapshot, name string) (snapshot, error) {
	if len(ss) == 0 {
		return snapshot{}, fmt.Errorf("no snapshots")
	}
	if name == "" {
		return ss[0], nil
	}
	for _, s := range ss {
		if s.Name == name {
			return s, nil
		}
	}
	return snapshot{}, fmt.Errorf("no snapshot %q", name)
}

// restore fetches s and checks it against its recorded digest and size.
func restore(ctx context.Context, t *tier, set string, s snapshot) ([]byte, error) {
	if err := withMeta(ctx, t, set, &s); err != nil {
		return nil, err
	}
	data, err := t.store.get(ctx, set, s)
	if err != nil {
		return nil, err
	}
	if s.SHA256 == "" {
		return nil, fmt.Errorf("%s has no recorded sha256 to verify against", s.Name)
	}
	if got := sum(data); got != s.SHA256 {
		return nil, fmt.Errorf("%s: restored content sha256 %s does not match the recorded %s", s.Name, got, s.SHA256)
	}
	if s.Size >= 0 && int64(len(data)) != s.Size {
		return nil, fmt.Errorf("%s: restored %d bytes, recorded %d", s.Name, len(data), s.Size)
	}
	return data, nil
}

func (p *backupPlugin) read(ctx context.Context, c *conf, set string, o map[string]any) (map[string]any, error) {
	t, err := c.tier(str(o["tier"]))
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	ss, err := t.store.list(ctx, set)
	if err != nil {
		return nil, err
	}
	s, err := find(ss, str(o["name"]))
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, fmt.Sprintf("set %s on tier %s: %v", set, t.name, err))
	}
	max := int64(defaultMaxRead)
	if v := intv(o["max_bytes"]); v > 0 {
		max = int64(v)
	}
	_ = withMeta(ctx, t, set, &s)
	if s.Size > max {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, fmt.Sprintf("%s is %d bytes, over max_bytes %d", s.Name, s.Size, max))
	}
	data, err := restore(ctx, t, set, s)
	if err != nil {
		return nil, err
	}
	content := string(data)
	if str(o["encoding"]) == "base64" {
		content = base64.StdEncoding.EncodeToString(data)
	}
	return map[string]any{"name": s.Name, "content": content, "size": len(data), "sha256": s.SHA256}, nil
}

// --- prune / verify ---

func (p *backupPlugin) prune(ctx context.Context, c *conf, set string, o map[string]any) (map[string]any, error) {
	tiers, err := c.pick(o)
	if err != nil {
		return nil, err
	}
	results := []any{}
	for _, t := range tiers {
		pruned, kept, err := applyRetention(ctx, t.store, set, t.keep, boolv(o["dry_run"]))
		if err != nil {
			return nil, fmt.Errorf("tier %s: %w", t.name, err)
		}
		results = append(results, map[string]any{"tier": t.name, "pruned": pruned, "kept": kept})
	}
	return map[string]any{"tiers": results}, nil
}

func (p *backupPlugin) verify(ctx context.Context, c *conf, set string, o map[string]any) (map[string]any, error) {
	tiers, err := c.pick(o)
	if err != nil {
		return nil, err
	}
	mode := strOr(str(o["mode"]), "restore")
	if mode != "check" && mode != "restore" && mode != "both" {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, "mode: check, restore or both")
	}
	sample := 1
	if v, ok := o["sample"]; ok && v != nil {
		sample = intv(v)
	}
	results := []any{}
	var bad []string
	for _, t := range tiers {
		r := map[string]any{"tier": t.name}
		if mode != "restore" {
			if err := t.store.check(ctx, set); err != nil {
				r["check_error"] = err.Error()
				bad = append(bad, t.name+" check: "+err.Error())
			}
			r["checked"] = true
		}
		if mode != "check" {
			ss, err := t.store.list(ctx, set)
			if err != nil {
				r["error"] = err.Error()
				bad = append(bad, t.name+": "+err.Error())
				results = append(results, r)
				continue
			}
			if len(ss) == 0 {
				r["error"] = "no snapshots to restore"
				bad = append(bad, t.name+": no snapshots to restore")
			}
			if sample > 0 && len(ss) > sample {
				ss = ss[:sample]
			}
			restored := []any{}
			for _, s := range ss {
				e := map[string]any{"name": s.Name, "ok": true}
				if _, err := restore(ctx, t, set, s); err != nil {
					e["ok"], e["error"] = false, err.Error()
					bad = append(bad, t.name+" "+s.Name+": "+err.Error())
				}
				restored = append(restored, e)
			}
			r["restored"] = restored
		}
		results = append(results, r)
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("verify FAILED (%d problem(s)): %s", len(bad), strings.Join(bad, "; "))
	}
	return map[string]any{"ok": true, "tiers": results}, nil
}

func fail(msg string) (plugin.InvokeResult, error) {
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, msg)
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == connectArg {
		os.Exit(connectMain(os.Args[2], os.Args[3]))
	}
	if err := plugin.Serve(newBackupPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-backup:", err)
		os.Exit(1)
	}
}
