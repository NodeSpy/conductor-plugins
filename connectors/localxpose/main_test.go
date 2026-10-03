package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func stubTool(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestLoclxURLRegexExtractsFromSampleOutput(t *testing.T) {
	sample := "INFO[0000] Tunnel started at https://abc123.loclx.io -> localhost:8099"
	got := loclxURLRe.FindString(sample)
	want := "https://abc123.loclx.io"
	if got != want {
		t.Fatalf("loclxURLRe: got %q, want %q", got, want)
	}
}

func TestLoclxURLRegexNoMatch(t *testing.T) {
	if got := loclxURLRe.FindString("connecting..."); got != "" {
		t.Fatalf("expected no match, got %q", got)
	}
}

func TestOpenAndClose(t *testing.T) {
	dir := stubTool(t, "loclx", `echo "args: $@" > "$(dirname "$0")/argv"
echo "Tunnel started at https://abc123.loclx.io -> localhost:8099"
sleep 30`)
	p := newLocalxposePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "tun", Verb: "open",
		Options:    map[string]any{"local_addr": "127.0.0.1:8099"},
		Connection: map[string]any{"start_timeout": "5s"},
	})
	if err != nil || res.Outputs["public_url"] != "https://abc123.loclx.io" {
		t.Fatalf("open: %v %v", res.Outputs, err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if !strings.Contains(string(argv), "--to 127.0.0.1:8099") {
		t.Fatalf("argv: %s", argv)
	}
	lease, _ := res.Outputs["lease"].(string)
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "close", Options: map[string]any{"lease": lease}}); err != nil {
		t.Fatal(err)
	}
	if p.leases.Len() != 0 {
		t.Fatalf("leases after close = %d, want 0", p.leases.Len())
	}
}

func TestExtraArgsInsertedBeforeTo(t *testing.T) {
	dir := stubTool(t, "loclx", `echo "args: $@" > "$(dirname "$0")/argv"
echo "https://sub.loclx.io"
sleep 30`)
	p := newLocalxposePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "open",
		Options:    map[string]any{"local_addr": "127.0.0.1:9100"},
		Connection: map[string]any{"start_timeout": "5s", "extra_args": []any{"--subdomain", "sub"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.leases.Release(res.Outputs["lease"].(string))
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if !strings.Contains(string(argv), "--subdomain sub --to 127.0.0.1:9100") {
		t.Fatalf("argv: %s", argv)
	}
}

func TestBadLocalAddrErrors(t *testing.T) {
	p := newLocalxposePlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "bad"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestMissingBinaryErrorsClearly(t *testing.T) {
	p := newLocalxposePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"binary": "no-such-loclx-xyz", "start_timeout": "300ms"}})
	if err == nil || !strings.Contains(err.Error(), "no-such-loclx-xyz") {
		t.Fatalf("expected a clear missing-binary error, got %v", err)
	}
}

func TestNoURLTimesOut(t *testing.T) {
	stubTool(t, "loclx", `echo nothing-url-shaped
sleep 5`)
	p := newLocalxposePlugin()
	start := time.Now()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"start_timeout": "200ms"}})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout took too long")
	}
}

func TestStopReleasesOnlyThatInstance(t *testing.T) {
	stubTool(t, "loclx", `echo https://a.loclx.io
sleep 30`)
	p := newLocalxposePlugin()
	for _, inst := range []string{"a", "b"} {
		if _, err := p.Invoke(plugin.InvokeRequest{Instance: inst, Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"start_timeout": "5s"}}); err != nil {
			t.Fatal(err)
		}
	}
	if p.leases.Len() != 2 {
		t.Fatalf("leases = %d, want 2", p.leases.Len())
	}
	if err := p.Stop(context.Background(), plugin.StopRequest{Instance: "a"}); err != nil {
		t.Fatal(err)
	}
	if p.leases.Len() != 1 {
		t.Fatalf("leases after stop a = %d, want 1", p.leases.Len())
	}
}

func TestUnknownVerbErrors(t *testing.T) {
	p := newLocalxposePlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "bogus"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeclarationsAreValid(t *testing.T) {
	d := newLocalxposePlugin().Describe()
	d.ProtocolVersion = plugin.ProtocolVersion
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if p := plugin.CheckSemantics(raw); len(p) > 0 {
		t.Fatalf("CheckSemantics: %v", p)
	}
	if p := plugin.ValidateSemantics(d); len(p) > 0 {
		t.Fatalf("ValidateSemantics: %v", p)
	}
}
