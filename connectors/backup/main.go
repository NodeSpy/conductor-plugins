// Command conductor-backup is a conductor connector that keeps versioned
// backups in a local directory, with grandfather-father-son retention.
//
// A backup is a named SET (e.g. "calendar-ednition"); every `put` writes one
// immutable snapshot file into <dir>/<set>/ and then prunes that set:
//
//	<dir>/<set>/<set>-20261120T031500Z.ics
//
// Retention keeps the newest snapshot of each of the last keep_daily days,
// keep_weekly ISO weeks and keep_monthly months, plus the newest snapshot
// overall. Everything else in the set is deleted. Files in the set directory
// that do not match the snapshot naming are never touched.
//
// Connection:
//
//	dir: /srv/backups/conductor   # required; the plugin may write only here
//	keep_daily: 7                 # defaults 7 / 2 / 6
//	keep_weekly: 2
//	keep_monthly: 6
//
// The directory may be a mounted or synced path (NFS, a Syncthing/rclone
// mount) to get backups off the machine.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

const stampLayout = "20060102T150405Z"

var (
	setRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	extRe = regexp.MustCompile(`^[A-Za-z0-9]{1,16}(\.[A-Za-z0-9]{1,16})?$`)
)

type backupPlugin struct {
	now func() time.Time
}

func newBackupPlugin() *backupPlugin { return &backupPlugin{now: time.Now} }

func (p *backupPlugin) Describe() plugin.Decl {
	keep := plugin.Schema{
		"keep_daily":   {Type: "integer", Desc: "days to keep one snapshot for (default: the connection's, else 7)"},
		"keep_weekly":  {Type: "integer", Desc: "ISO weeks to keep one snapshot for (default 2)"},
		"keep_monthly": {Type: "integer", Desc: "months to keep one snapshot for (default 6)"},
	}
	with := func(base plugin.Schema, extra plugin.Schema) plugin.Schema {
		out := plugin.Schema{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	set := plugin.Field{Type: "string", Required: true, Desc: "backup set name: letters, digits, . _ - (e.g. calendar-ednition)"}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "backup",
		Desc: "Versioned backups to a local directory with daily/weekly/monthly retention: put a snapshot, list, read one back, prune.",
		Connection: plugin.Schema{
			"dir":          {Type: "string", Required: true, Desc: "backup root directory; the plugin can write only here"},
			"keep_daily":   {Type: "integer", Desc: "default 7"},
			"keep_weekly":  {Type: "integer", Desc: "default 2"},
			"keep_monthly": {Type: "integer", Desc: "default 6"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "put", Desc: "write one snapshot to a set, then apply the set's retention",
				Options: with(keep, plugin.Schema{
					"set":    set,
					"text":   {Type: "string", Desc: "snapshot content as text"},
					"base64": {Type: "string", Desc: "snapshot content, base64 (binary); exactly one of text/base64"},
					"ext":    {Type: "string", Desc: "file extension, e.g. ics, json, tar.gz (default txt)"},
				}),
				Outputs: plugin.Schema{
					"path": {Type: "string"}, "size": {Type: "integer"}, "sha256": {Type: "string"},
					"pruned": {Type: "list", Desc: "snapshot file names deleted by retention"},
				},
			},
			{
				Name: "list", Desc: "list a set's snapshots, newest first",
				Options: plugin.Schema{"set": set},
				Outputs: plugin.Schema{"items": {Type: "list", Desc: "[{name, path, time, size}]"}},
			},
			{
				Name: "read", Desc: "read a snapshot back (the newest unless name is given)",
				Options: plugin.Schema{
					"set":       set,
					"name":      {Type: "string", Desc: "snapshot file name from list (default: newest)"},
					"encoding":  {Type: "string", Enum: []string{"text", "base64"}, Desc: "default text"},
					"max_bytes": {Type: "integer", Desc: "refuse snapshots larger than this (default 16 MiB)"},
				},
				Outputs: plugin.Schema{"name": {Type: "string"}, "content": {Type: "string"}, "size": {Type: "integer"}},
			},
			{
				Name: "prune", Desc: "apply retention to a set without writing",
				Options: with(keep, plugin.Schema{
					"set":     set,
					"dry_run": {Type: "boolean", Desc: "report what would be deleted, delete nothing"},
				}),
				Outputs: plugin.Schema{"pruned": {Type: "list"}, "kept": {Type: "list"}},
			},
		},
		Capabilities: plugin.Capabilities{Write: []string{"config:dir"}},
	}
}

func (p *backupPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	dir := str(req.Connection["dir"])
	if dir == "" || !filepath.IsAbs(dir) {
		return fail("connection.dir must be an absolute path")
	}
	setName := str(o["set"])
	if !setRe.MatchString(setName) {
		return fail(fmt.Sprintf("set %q: use letters, digits, '.', '_' or '-' (max 100, no leading punctuation)", setName))
	}
	setDir := filepath.Join(dir, setName)
	pol, err := policy(req.Connection, o)
	if err != nil {
		return fail(err.Error())
	}
	switch req.Verb {
	case "put":
		return p.put(setDir, setName, pol, o)
	case "list":
		snaps, err := scan(setDir, setName)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
		}
		items := make([]any, 0, len(snaps))
		for _, s := range snaps {
			items = append(items, map[string]any{"name": s.name, "path": filepath.Join(setDir, s.name), "time": s.at.Format(time.RFC3339), "size": s.size})
		}
		return plugin.InvokeResult{Outputs: map[string]any{"items": items}}, nil
	case "read":
		return p.read(setDir, setName, o)
	case "prune":
		pruned, kept, err := prune(setDir, setName, pol, boolv(o["dry_run"]))
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
		}
		return plugin.InvokeResult{Outputs: map[string]any{"pruned": pruned, "kept": kept}}, nil
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
}

func (p *backupPlugin) put(setDir, setName string, pol retention, o map[string]any) (plugin.InvokeResult, error) {
	text, hasText := o["text"].(string)
	b64, hasB64 := o["base64"].(string)
	if hasText == hasB64 {
		return fail("exactly one of text or base64 is required")
	}
	data := []byte(text)
	if hasB64 {
		var err error
		if data, err = base64.StdEncoding.DecodeString(b64); err != nil {
			return fail("base64: " + err.Error())
		}
	}
	ext := strings.TrimPrefix(strOr(str(o["ext"]), "txt"), ".")
	if !extRe.MatchString(ext) {
		return fail(fmt.Sprintf("ext %q: letters and digits, at most one inner dot", ext))
	}
	if err := os.MkdirAll(setDir, 0o700); err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	// A unique, sortable name; bump by a second on the rare same-second put.
	at := p.now().UTC().Truncate(time.Second)
	// The timestamp must be unique within the set whatever the extension,
	// or two snapshots would tie for "newest".
	existing, err := scan(setDir, setName)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	taken := map[time.Time]bool{}
	for _, s := range existing {
		taken[s.at] = true
	}
	for taken[at] {
		at = at.Add(time.Second)
	}
	name := fmt.Sprintf("%s-%s.%s", setName, at.Format(stampLayout), ext)
	final := filepath.Join(setDir, name)
	// Write to a temp file and rename, so a crash never leaves a partial
	// snapshot that retention would count as a real one.
	tmp, err := os.CreateTemp(setDir, ".put-*")
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	_, werr := tmp.Write(data)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if werr != nil || serr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("write snapshot: %v %v %v", werr, serr, cerr))
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		_ = os.Remove(tmp.Name())
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	pruned, _, err := prune(setDir, setName, pol, false)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "snapshot written, but retention failed: "+err.Error())
	}
	sum := sha256.Sum256(data)
	return plugin.InvokeResult{Outputs: map[string]any{
		"path": final, "size": len(data), "sha256": hex.EncodeToString(sum[:]), "pruned": pruned,
	}}, nil
}

func (p *backupPlugin) read(setDir, setName string, o map[string]any) (plugin.InvokeResult, error) {
	snaps, err := scan(setDir, setName)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if len(snaps) == 0 {
		return fail("set " + setName + " has no snapshots")
	}
	pick := snaps[0]
	if want := str(o["name"]); want != "" {
		found := false
		for _, s := range snaps {
			if s.name == want {
				pick, found = s, true
				break
			}
		}
		if !found {
			return fail(fmt.Sprintf("no snapshot %q in set %s", want, setName))
		}
	}
	max := int64(16 << 20)
	if v := intv(o["max_bytes"]); v > 0 {
		max = int64(v)
	}
	if pick.size > max {
		return fail(fmt.Sprintf("%s is %d bytes, over max_bytes %d", pick.name, pick.size, max))
	}
	data, err := os.ReadFile(filepath.Join(setDir, pick.name))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	content := string(data)
	if str(o["encoding"]) == "base64" {
		content = base64.StdEncoding.EncodeToString(data)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"name": pick.name, "content": content, "size": len(data)}}, nil
}

// --- retention ---

type retention struct{ daily, weekly, monthly int }

func policy(conn, o map[string]any) (retention, error) {
	get := func(key string, def int) (int, error) {
		v := def
		if c := conn[key]; c != nil {
			v = intv(c)
		}
		if c := o[key]; c != nil {
			v = intv(c)
		}
		if v < 0 {
			return 0, fmt.Errorf("%s must be >= 0", key)
		}
		return v, nil
	}
	var r retention
	var err error
	if r.daily, err = get("keep_daily", 7); err != nil {
		return r, err
	}
	if r.weekly, err = get("keep_weekly", 2); err != nil {
		return r, err
	}
	if r.monthly, err = get("keep_monthly", 6); err != nil {
		return r, err
	}
	return r, nil
}

type snapshot struct {
	name string
	at   time.Time
	size int64
}

// scan lists the set's snapshot files, newest first. Only files named
// <set>-<stamp>.<ext> count; anything else in the directory is ignored.
func scan(setDir, setName string) ([]snapshot, error) {
	ents, err := os.ReadDir(setDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := setName + "-"
	var out []snapshot
	for _, e := range ents {
		n := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(n, prefix) {
			continue
		}
		rest := n[len(prefix):]
		if len(rest) < len(stampLayout)+2 || rest[len(stampLayout)] != '.' {
			continue
		}
		at, err := time.Parse(stampLayout, rest[:len(stampLayout)])
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, snapshot{name: n, at: at, size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.After(out[j].at)
		}
		return out[i].name > out[j].name
	})
	return out, nil
}

// keep marks, newest first, the newest snapshot of each of the first
// r.daily distinct days, r.weekly ISO weeks and r.monthly months (the
// classic GFS rule), plus the newest snapshot overall.
func keep(snaps []snapshot, r retention) map[string]bool {
	kept := map[string]bool{}
	if len(snaps) > 0 {
		kept[snaps[0].name] = true
	}
	bucket := func(n int, key func(time.Time) string) {
		seen := map[string]bool{}
		for _, s := range snaps {
			if len(seen) >= n {
				return
			}
			k := key(s.at)
			if !seen[k] {
				seen[k] = true
				kept[s.name] = true
			}
		}
	}
	bucket(r.daily, func(t time.Time) string { return t.Format("2006-01-02") })
	bucket(r.weekly, func(t time.Time) string { y, w := t.ISOWeek(); return strconv.Itoa(y) + "-W" + strconv.Itoa(w) })
	bucket(r.monthly, func(t time.Time) string { return t.Format("2006-01") })
	return kept
}

func prune(setDir, setName string, r retention, dry bool) (pruned, kept []any, err error) {
	snaps, err := scan(setDir, setName)
	if err != nil {
		return nil, nil, err
	}
	k := keep(snaps, r)
	pruned, kept = []any{}, []any{}
	for _, s := range snaps {
		if k[s.name] {
			kept = append(kept, s.name)
			continue
		}
		if !dry {
			if err := os.Remove(filepath.Join(setDir, s.name)); err != nil && !os.IsNotExist(err) {
				return pruned, kept, err
			}
		}
		pruned = append(pruned, s.name)
	}
	return pruned, kept, nil
}

// --- helpers ---

func fail(msg string) (plugin.InvokeResult, error) {
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, msg)
}

func str(v any) string { s, _ := v.(string); return s }

func strOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func boolv(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	}
	return false
}

func intv(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func main() {
	if err := plugin.Serve(newBackupPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-backup:", err)
		os.Exit(1)
	}
}
