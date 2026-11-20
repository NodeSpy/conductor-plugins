package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func invoke(t *testing.T, p *backupPlugin, dir, verb string, o map[string]any) (map[string]any, error) {
	t.Helper()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: verb, Connection: map[string]any{"dir": dir}, Options: o})
	return res.Outputs, err
}

func TestPutWritesSnapshotAtomicallyAndReadsBack(t *testing.T) {
	dir := t.TempDir()
	p := newBackupPlugin()
	p.now = func() time.Time { return time.Date(2026, 11, 20, 3, 15, 0, 0, time.UTC) }
	out, err := invoke(t, p, dir, "put", map[string]any{"set": "cal-a", "text": "BEGIN:VCALENDAR", "ext": "ics"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "cal-a", "cal-a-20261120T031500Z.ics")
	if out["path"] != want || out["size"] != 15 {
		t.Fatalf("put: %v", out)
	}
	r, err := invoke(t, p, dir, "read", map[string]any{"set": "cal-a"})
	if err != nil || r["content"] != "BEGIN:VCALENDAR" {
		t.Fatalf("read: %v %v", r, err)
	}
	// Same second again: a distinct timestamp, not an overwrite.
	out2, err := invoke(t, p, dir, "put", map[string]any{"set": "cal-a", "base64": base64.StdEncoding.EncodeToString([]byte{0, 1, 2}), "ext": "bin"})
	if err != nil || out2["path"] == want {
		t.Fatalf("second put: %v %v", out2, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "cal-a"))
	for _, e := range entries {
		if e.Name()[0] == '.' {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	l, _ := invoke(t, p, dir, "list", map[string]any{"set": "cal-a"})
	// Daily retention keeps one snapshot per day: the newer same-day put wins.
	items := l["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "cal-a-20261120T031501Z.bin" {
		t.Fatalf("list: %v", items)
	}
}

func TestRetentionGFS(t *testing.T) {
	dir := t.TempDir()
	set := filepath.Join(dir, "s")
	_ = os.MkdirAll(set, 0o700)
	// One snapshot a day for 400 days, two on the newest day, plus a foreign file.
	base := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 400; i++ {
		at := base.AddDate(0, 0, -i)
		_ = os.WriteFile(filepath.Join(set, "s-"+at.Format(stampLayout)+".ics"), []byte("x"), 0o600)
	}
	_ = os.WriteFile(filepath.Join(set, "s-"+base.Add(time.Hour).Format(stampLayout)+".ics"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(set, "notes.txt"), []byte("mine"), 0o600)

	p := newBackupPlugin()
	dry, err := invoke(t, p, dir, "prune", map[string]any{"set": "s", "dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := os.ReadDir(set); len(n) != 402 {
		t.Fatalf("dry run deleted files: %d left", len(n))
	}
	out, err := invoke(t, p, dir, "prune", map[string]any{"set": "s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out["pruned"].([]any)) != len(dry["pruned"].([]any)) {
		t.Fatal("dry run and real prune disagree")
	}
	kept := out["kept"].([]any)
	// Expected: 7 dailies (Nov 20 at 04:00, then Nov 19..14), weekly adds
	// nothing new (Nov 20 and Nov 15 are already dailies: ISO weeks Nov
	// 16-22 and 9-15), monthly adds Oct 31, Sep 30, Aug 31, Jul 31, Jun 30.
	if len(kept) != 12 {
		t.Fatalf("kept %d: %v", len(kept), kept)
	}
	if kept[0] != "s-"+base.Add(time.Hour).Format(stampLayout)+".ics" {
		t.Fatalf("newest must be kept first: %v", kept[0])
	}
	if _, err := os.Stat(filepath.Join(set, "notes.txt")); err != nil {
		t.Fatal("non-snapshot files must never be touched")
	}
}

func TestValidation(t *testing.T) {
	p := newBackupPlugin()
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		dir string
		o   map[string]any
	}{
		"relative dir":  {"rel", map[string]any{"set": "a", "text": "x"}},
		"traversal set": {dir, map[string]any{"set": "../etc", "text": "x"}},
		"slash set":     {dir, map[string]any{"set": "a/b", "text": "x"}},
		"both bodies":   {dir, map[string]any{"set": "a", "text": "x", "base64": "eA=="}},
		"no body":       {dir, map[string]any{"set": "a"}},
		"bad ext":       {dir, map[string]any{"set": "a", "text": "x", "ext": "../x"}},
		"negative keep": {dir, map[string]any{"set": "a", "text": "x", "keep_daily": -1}},
	} {
		if _, err := invoke(t, p, tc.dir, "put", tc.o); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := invoke(t, p, dir, "read", map[string]any{"set": "empty"}); err == nil {
		t.Error("read of an empty set should fail")
	}
}
