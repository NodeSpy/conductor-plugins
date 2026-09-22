package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func TestParseWatches(t *testing.T) {
	ws, err := parseWatches(map[string]any{
		"errs": map[string]any{"path": "/var/log/app.log", "pattern": "ERROR (?P<msg>.*)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].source() != "/var/log/app.log" {
		t.Fatalf("decode wrong: %+v", ws)
	}
	// exactly-one-of path/command
	if _, err := parseWatches(map[string]any{"x": map[string]any{"path": "/a", "command": []any{"j"}, "pattern": "E"}}); err == nil {
		t.Error("both path and command must error")
	}
	if _, err := parseWatches(map[string]any{"x": map[string]any{"pattern": "E"}}); err == nil {
		t.Error("neither path nor command must error")
	}
	if _, err := parseWatches(map[string]any{"x": map[string]any{"path": "/a"}}); err == nil {
		t.Error("missing pattern must error")
	}
	if _, err := parseWatches(map[string]any{"x": map[string]any{"path": "/a", "pattern": "("}}); err == nil {
		t.Error("bad regexp must error")
	}
}

func TestStartSourceCommandEmits(t *testing.T) {
	req := plugin.StartSourceRequest{
		Instance: "logs",
		Config: map[string]any{"watches": map[string]any{
			"errs": map[string]any{
				"command":  []any{"bash", "-c", "echo noise; echo 'ERROR boom'; sleep 30"},
				"pattern":  `ERROR (?P<msg>.*)`,
				"debounce": "40ms",
			},
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
	go logwatchPlugin{}.StartSource(ctx, req, emit)

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no event for a matching line")
		case <-time.After(20 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	ev := got[0]
	if ev["event"] != "errs" {
		t.Fatalf("event should be watch name, got %v", ev["event"])
	}
	cxt, _ := ev["context"].(map[string]any)
	if cxt["line"] != "ERROR boom" {
		t.Fatalf("line should be 'ERROR boom', got %v", cxt["line"])
	}
	groups, _ := cxt["groups"].(map[string]string)
	if groups["msg"] != "boom" {
		t.Fatalf("capture msg should be boom, got %v", cxt["groups"])
	}
}

func TestStartSourceFileTailEmits(t *testing.T) {
	dir := t.TempDir()
	logfile := filepath.Join(dir, "app.log")
	if err := os.WriteFile(logfile, []byte("startup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := plugin.StartSourceRequest{
		Instance: "logs",
		Config: map[string]any{"watches": map[string]any{
			"fatal": map[string]any{"path": logfile, "pattern": "FATAL", "debounce": "40ms"},
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
	go logwatchPlugin{}.StartSource(ctx, req, emit)
	time.Sleep(300 * time.Millisecond) // let tail -F attach

	f, err := os.OpenFile(logfile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("info\nFATAL disk gone\n")
	f.Close()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no event for a matching appended line")
		case <-time.After(20 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	cxt, _ := got[0]["context"].(map[string]any)
	if cxt["line"] != "FATAL disk gone" {
		t.Fatalf("expected the FATAL line, got %v", cxt["line"])
	}
	if cxt["source"] != logfile {
		t.Fatalf("source should be the file path, got %v", cxt["source"])
	}
}
