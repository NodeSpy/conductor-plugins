package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func call(t *testing.T, p *backupPlugin, conn map[string]any, verb string, o map[string]any) (map[string]any, error) {
	t.Helper()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: verb, Connection: conn, Options: o})
	return res.Outputs, err
}

func mustCall(t *testing.T, p *backupPlugin, conn map[string]any, verb string, o map[string]any) map[string]any {
	t.Helper()
	out, err := call(t, p, conn, verb, o)
	if err != nil {
		t.Fatalf("%s: %v", verb, err)
	}
	return out
}

func clock(start time.Time) (func() time.Time, func(time.Duration)) {
	now := start
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

func localConn(dir string, extra map[string]any) map[string]any {
	tier := map[string]any{"name": "disk", "driver": "local", "dir": filepath.Join(dir, "b")}
	for k, v := range extra {
		tier[k] = v
	}
	return map[string]any{"tiers": []any{tier}, "allow_paths": []any{dir}}
}

func tierResult(out map[string]any, i int) map[string]any {
	return out["tiers"].([]any)[i].(map[string]any)
}

func TestPutDedupReadVerifyLocal(t *testing.T) {
	dir := t.TempDir()
	p := newBackupPlugin()
	var tick func(time.Duration)
	p.now, tick = clock(time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC))
	conn := localConn(dir, nil)

	out := mustCall(t, p, conn, "put", map[string]any{"set": "cal", "text": "v1", "ext": "ics"})
	if r := tierResult(out, 0); r["status"] != "written" || r["name"] != "cal-20261120T030000Z.ics" {
		t.Fatalf("first put: %v", r)
	}
	tick(time.Hour)
	if r := tierResult(mustCall(t, p, conn, "put", map[string]any{"set": "cal", "text": "v1", "ext": "ics"}), 0); r["status"] != "unchanged" {
		t.Fatalf("identical content must dedup: %v", r)
	}
	tick(24 * time.Hour)
	mustCall(t, p, conn, "put", map[string]any{"set": "cal", "text": "v2", "ext": "ics"})

	items := mustCall(t, p, conn, "list", map[string]any{"set": "cal"})["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["sha256"] != sum([]byte("v2")) {
		t.Fatalf("list: %v", items)
	}
	if got := mustCall(t, p, conn, "read", map[string]any{"set": "cal"}); got["content"] != "v2" {
		t.Fatalf("read newest: %v", got)
	}
	if got := mustCall(t, p, conn, "read", map[string]any{"set": "cal", "name": "cal-20261120T030000Z.ics"}); got["content"] != "v1" {
		t.Fatalf("read by name: %v", got)
	}
	v := mustCall(t, p, conn, "verify", map[string]any{"set": "cal", "mode": "both", "sample": 0})
	if v["ok"] != true || len(tierResult(v, 0)["restored"].([]any)) != 2 {
		t.Fatalf("verify: %v", v)
	}

	// Tamper with the stored bytes: read and verify must both refuse.
	f := filepath.Join(dir, "b", "cal", "cal-20261121T040000Z.ics")
	if err := os.WriteFile(f, []byte("v3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, p, conn, "verify", map[string]any{"set": "cal"}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered snapshot must fail verify, got %v", err)
	}
	if _, err := call(t, p, conn, "read", map[string]any{"set": "cal"}); err == nil {
		t.Fatal("tampered snapshot must fail read")
	}
}

func TestAgeEncryptedLocalTier(t *testing.T) {
	dir := t.TempDir()
	id, _ := age.GenerateX25519Identity()
	p := newBackupPlugin()
	p.now, _ = clock(time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC))

	// A writer that holds only the public key can put but not read back.
	writer := localConn(dir, map[string]any{"age_recipients": []any{id.Recipient().String()}})
	mustCall(t, p, writer, "put", map[string]any{"set": "s", "text": "secret calendar", "ext": "ics"})
	raw, err := os.ReadFile(filepath.Join(dir, "b", "s", "s-20261120T030000Z.ics.age"))
	if err != nil || strings.Contains(string(raw), "secret") {
		t.Fatalf("stored object must be ciphertext: %v %q", err, raw)
	}
	meta, _ := os.ReadFile(filepath.Join(dir, "b", "s", ".s-20261120T030000Z.ics.meta"))
	if strings.Contains(string(meta), sum([]byte("secret calendar"))) {
		t.Fatal("an encrypted tier must not leak the plaintext hash in its meta")
	}
	if _, err := call(t, p, writer, "read", map[string]any{"set": "s"}); err == nil || !strings.Contains(err.Error(), "age_identity") {
		t.Fatalf("read without identity: %v", err)
	}
	// Dedup can't see through an encrypted meta without the identity: it writes.
	// The reader (identity) restores and verifies.
	reader := localConn(dir, map[string]any{"age_identity": id.String()})
	if got := mustCall(t, p, reader, "read", map[string]any{"set": "s"}); got["content"] != "secret calendar" {
		t.Fatalf("decrypt: %v", got)
	}
	if v := mustCall(t, p, reader, "verify", map[string]any{"set": "s", "mode": "both"}); v["ok"] != true {
		t.Fatalf("verify: %v", v)
	}
	if r := tierResult(mustCall(t, p, reader, "put", map[string]any{"set": "s", "text": "secret calendar", "ext": "ics"}), 0); r["status"] != "unchanged" {
		t.Fatalf("with the identity, dedup sees the same content: %v", r)
	}
}

func TestMultiTierRetentionIsPerTier(t *testing.T) {
	dir := t.TempDir()
	p := newBackupPlugin()
	var tick func(time.Duration)
	p.now, tick = clock(time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC))
	conn := map[string]any{
		"allow_paths": []any{dir},
		"tiers": []any{
			map[string]any{"name": "hot", "driver": "local", "dir": filepath.Join(dir, "hot"), "keep_daily": 3, "keep_weekly": 0, "keep_monthly": 0},
			map[string]any{"name": "cold", "driver": "local", "dir": filepath.Join(dir, "cold"), "keep_daily": 0, "keep_weekly": 0, "keep_monthly": 12},
		},
	}
	for i := 0; i < 100; i++ {
		mustCall(t, p, conn, "put", map[string]any{"set": "x", "text": "day " + itoa(i)})
		tick(24 * time.Hour)
	}
	hot := mustCall(t, p, conn, "list", map[string]any{"set": "x", "tier": "hot"})["items"].([]any)
	cold := mustCall(t, p, conn, "list", map[string]any{"set": "x", "tier": "cold"})["items"].([]any)
	if len(hot) != 3 {
		t.Fatalf("hot keeps 3 dailies, has %d", len(hot))
	}
	// Jan 1 .. Apr 10: one per month (Jan, Feb, Mar, Apr) = 4.
	if len(cold) != 4 {
		t.Fatalf("cold keeps one per month, has %d: %v", len(cold), cold)
	}
	// put to one tier only.
	out := mustCall(t, p, conn, "put", map[string]any{"set": "x", "text": "only hot", "tiers": []any{"hot"}})
	if len(out["tiers"].([]any)) != 1 {
		t.Fatalf("tiers filter: %v", out)
	}
}

func TestRetentionGFSExact(t *testing.T) {
	base := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	var ss []snapshot
	ss = append(ss, snapshot{Name: "s-" + base.Add(time.Hour).Format(stampLayout) + ".x", At: base.Add(time.Hour)})
	for i := 0; i < 400; i++ {
		at := base.AddDate(0, 0, -i)
		ss = append(ss, snapshot{Name: "s-" + at.Format(stampLayout) + ".x", At: at})
	}
	sortNewest(ss)
	// 7 dailies (Nov 20 04:00, Nov 19..14); weekly adds none (Nov 20, Nov 15
	// already kept); monthly adds Oct 31, Sep 30, Aug 31, Jul 31, Jun 30.
	if k := keep(ss, retention{7, 2, 6}); len(k) != 12 {
		t.Fatalf("kept %d", len(k))
	}
}

func TestSameSecondPutsGetDistinctTimestamps(t *testing.T) {
	dir := t.TempDir()
	p := newBackupPlugin()
	p.now, _ = clock(time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC))
	conn := localConn(dir, map[string]any{"keep_daily": 0, "keep_weekly": 0, "keep_monthly": 0})
	mustCall(t, p, conn, "put", map[string]any{"set": "a", "text": "1", "ext": "ics"})
	r := tierResult(mustCall(t, p, conn, "put", map[string]any{"set": "a", "base64": base64.StdEncoding.EncodeToString([]byte{0, 1}), "ext": "bin"}), 0)
	if r["name"] != "a-20261120T030001Z.bin" {
		t.Fatalf("second put in the same second: %v", r)
	}
}

func TestStrayFilesAndPartialPutsAreIgnored(t *testing.T) {
	dir := t.TempDir()
	set := filepath.Join(dir, "b", "s")
	_ = os.MkdirAll(set, 0o700)
	_ = os.WriteFile(filepath.Join(set, "notes.txt"), []byte("mine"), 0o600)
	_ = os.WriteFile(filepath.Join(set, "s-20200101T000000Z.ics"), []byte("no meta = half-written"), 0o600)
	_ = os.WriteFile(filepath.Join(set, "s-other-20200101T000000Z.ics"), []byte("x"), 0o600)
	p := newBackupPlugin()
	conn := localConn(dir, map[string]any{"keep_daily": 0, "keep_weekly": 0, "keep_monthly": 0})
	if items := mustCall(t, p, conn, "list", map[string]any{"set": "s"})["items"].([]any); len(items) != 0 {
		t.Fatalf("strays listed: %v", items)
	}
	mustCall(t, p, conn, "put", map[string]any{"set": "s", "text": "real"})
	for _, f := range []string{"notes.txt", "s-20200101T000000Z.ics", "s-other-20200101T000000Z.ics"} {
		if _, err := os.Stat(filepath.Join(set, f)); err != nil {
			t.Errorf("%s must never be touched: %v", f, err)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	p := newBackupPlugin()
	dir := t.TempDir()
	cases := map[string]map[string]any{
		"no tiers":                {},
		"unknown driver":          {"tiers": []any{map[string]any{"driver": "ftp"}}},
		"relative local dir":      {"tiers": []any{map[string]any{"driver": "local", "dir": "rel"}}, "allow_paths": []any{dir}},
		"dir outside allow_paths": {"tiers": []any{map[string]any{"driver": "local", "dir": "/elsewhere"}}, "allow_paths": []any{dir}},
		"duplicate names":         {"tiers": []any{map[string]any{"name": "a", "driver": "local", "dir": dir}, map[string]any{"name": "a", "driver": "local", "dir": dir}}, "allow_paths": []any{dir}},
		"restic no password":      {"tiers": []any{map[string]any{"driver": "restic", "repo": "s3:http://h:9000/b"}}, "allow_hosts": []any{"h:9000"}},
		"host not allowed":        {"tiers": []any{map[string]any{"driver": "restic", "repo": "s3:http://h:9000/b", "password": "p"}}},
		"borg no passphrase":      {"tiers": []any{map[string]any{"driver": "borg", "repo": "ssh://u@h/r"}}},
		"rclone unencrypted":      {"tiers": []any{map[string]any{"driver": "rclone", "remote": "b2:bucket"}}},
		"ssh no host key":         {"tiers": []any{map[string]any{"driver": "restic", "repo": "sftp:u@h:/r", "password": "p", "ssh": map[string]any{"key": "k"}}}},
		"negative keep":           {"tiers": []any{map[string]any{"driver": "local", "dir": dir, "keep_daily": -1}}, "allow_paths": []any{dir}},
		"bad age recipient":       {"tiers": []any{map[string]any{"driver": "local", "dir": dir, "age_recipients": []any{"nope"}}}, "allow_paths": []any{dir}},
	}
	for name, conn := range cases {
		res, _ := p.Validate(context.Background(), plugin.ValidateRequest{Config: conn})
		if len(res.Problems) == 0 {
			t.Errorf("%s: want a problem", name)
		}
	}
	ok := map[string]any{"allow_paths": []any{dir}, "tiers": []any{
		map[string]any{"driver": "local", "dir": filepath.Join(dir, "x")},
		map[string]any{"driver": "rclone", "remote": "crypted:", "encrypt": false},
		map[string]any{"driver": "borg", "repo": "ssh://u@h/r", "encrypt": false, "ssh": map[string]any{"insecure_accept_host_key": true}},
	}, "allow_hosts": []any{"h:22"}}
	if res, _ := p.Validate(context.Background(), plugin.ValidateRequest{Config: ok}); len(res.Problems) != 0 {
		t.Errorf("valid config: %+v", res.Problems)
	}
	if res, _ := p.Validate(context.Background(), plugin.ValidateRequest{Config: map[string]any{"dir": filepath.Join(dir, "y"), "allow_paths": []any{dir}}}); len(res.Problems) != 0 {
		t.Errorf("dir shorthand: %+v", res.Problems)
	}
	// Bad options.
	conn := localConn(dir, nil)
	for name, o := range map[string]map[string]any{
		"traversal set": {"set": "../etc", "text": "x"},
		"both bodies":   {"set": "a", "text": "x", "base64": "eA=="},
		"bad ext":       {"set": "a", "text": "x", "ext": "../x"},
		"unknown tier":  {"set": "a", "text": "x", "tiers": []any{"nope"}},
	} {
		if _, err := call(t, p, conn, "put", o); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := call(t, p, conn, "verify", map[string]any{"set": "empty"}); err == nil {
		t.Error("verify of an empty set must fail (nothing proven)")
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"sftp:u@nas:/backups":         "nas:22",
		"sftp://u@nas:2222//backups":  "nas:2222",
		"ssh://u@nas:2200/./repo":     "nas:2200",
		"s3:http://127.0.0.1:9000/b":  "127.0.0.1:9000",
		"s3:https://s3.example.com/b": "s3.example.com:443",
		"/srv/restic":                 "",
	} {
		if got := hostOf("", in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllowHostsUsesSSHPort(t *testing.T) {
	p := newBackupPlugin()
	tier := func(m map[string]any) map[string]any {
		return map[string]any{"allow_hosts": []any{"nas:2222"}, "tiers": []any{m}}
	}
	ssh := map[string]any{"insecure_accept_host_key": true, "port": 2222}
	for name, conn := range map[string]map[string]any{
		"restic sftp": tier(map[string]any{"driver": "restic", "repo": "sftp:u@nas:/r", "password": "p", "ssh": ssh}),
		"rclone sftp": tier(map[string]any{"driver": "rclone", "remote": ":sftp,host=nas,user=u:/r", "encrypt": false, "ssh": ssh}),
	} {
		if res, _ := p.Validate(context.Background(), plugin.ValidateRequest{Config: conn}); len(res.Problems) != 0 {
			t.Errorf("%s: %+v", name, res.Problems)
		}
	}
	bad := tier(map[string]any{"driver": "rclone", "remote": ":sftp,host=other,user=u:/r", "encrypt": false, "ssh": ssh})
	if res, _ := p.Validate(context.Background(), plugin.ValidateRequest{Config: bad}); len(res.Problems) == 0 {
		t.Error("an unlisted rclone sftp host must be flagged")
	}
}
