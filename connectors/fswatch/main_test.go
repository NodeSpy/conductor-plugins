package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/fsnotify/fsnotify"
)

func TestMatches(t *testing.T) {
	w := watch{match: "*.m4b", ops: fsnotify.Create | fsnotify.Write | fsnotify.Rename}
	cases := []struct {
		name string
		op   fsnotify.Op
		want bool
	}{
		{"book.m4b", fsnotify.Create, true},
		{"book.m4b", fsnotify.Write, true},
		{"book.m4b", fsnotify.Chmod, false},
		{"cover.jpg", fsnotify.Create, false},
	}
	for _, c := range cases {
		if got := w.matches(c.name, c.op); got != c.want {
			t.Errorf("matches(%q,%v)=%v want %v", c.name, c.op, got, c.want)
		}
	}
	all := watch{ops: fsnotify.Create}
	if !all.matches("anything", fsnotify.Create) {
		t.Error("empty match should accept anything")
	}
}

func TestParseWatches(t *testing.T) {
	ws, err := parseWatches(map[string]any{
		"staged": map[string]any{"path": "/d", "match": "*.m4b", "debounce": "30s", "recursive": false},
		"other":  map[string]any{"path": "/e"}, // defaults
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 || ws[0].name != "other" || ws[1].name != "staged" {
		t.Fatalf("expected 2 watches sorted by name, got %+v", ws)
	}
	if ws[1].debounce != 30*time.Second || ws[1].recursive {
		t.Fatalf("staged decode wrong: %+v", ws[1])
	}
	if !ws[0].recursive || ws[0].debounce != defaultDebounce {
		t.Fatalf("defaults wrong: %+v", ws[0])
	}
	if _, err := parseWatches(map[string]any{"bad": map[string]any{}}); err == nil {
		t.Error("a watch with no path must error")
	}
}

func TestStartSourceEmitsOnMatch(t *testing.T) {
	dir := t.TempDir()
	req := plugin.StartSourceRequest{
		Instance: "files",
		Config: map[string]any{"watches": map[string]any{
			"staged": map[string]any{"path": dir, "match": "*.m4b", "debounce": "40ms"},
		}},
	}
	var mu sync.Mutex
	var got []map[string]any
	emit := func(p any) error {
		mu.Lock()
		got = append(got, p.(map[string]any))
		mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fswatchPlugin{}.StartSource(ctx, req, emit)
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(dir, "book.m4b"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// non-matching file must not emit
	_ = os.WriteFile(filepath.Join(dir, "cover.jpg"), []byte("x"), 0o644)

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no event emitted for a matching file")
		case <-time.After(20 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	ev := got[0]
	if ev["event"] != "staged" {
		t.Fatalf("event should be the watch name, got %v", ev["event"])
	}
	cxt, _ := ev["context"].(map[string]any)
	if cxt["file"] != "book.m4b" {
		t.Fatalf("context.file should be book.m4b, got %v", cxt["file"])
	}
	if cxt["watch"] != "staged" {
		t.Fatalf("context.watch should be staged, got %v", cxt["watch"])
	}
	for _, e := range got {
		c, _ := e["context"].(map[string]any)
		if c["file"] == "cover.jpg" {
			t.Fatal("a non-matching file must not emit")
		}
	}
}
