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

// stubTool writes an executable named name into a dir prepended to PATH.
func stubTool(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// ---- URL extraction (fed captured sample output, no real binary/network) ----

func TestCloudflareURLRegexExtractsFromSampleOutput(t *testing.T) {
	sample := `2026-09-02T12:00:00Z INF Requesting new quick Tunnel on trycloudflare.com...
2026-09-02T12:00:01Z INF +--------------------------------------------------------------------------------------------+
2026-09-02T12:00:01Z INF |  Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):   |
2026-09-02T12:00:01Z INF |  https://random-words-here.trycloudflare.com                                               |
2026-09-02T12:00:01Z INF +--------------------------------------------------------------------------------------------+`
	got := cloudflareURLRe.FindString(sample)
	want := "https://random-words-here.trycloudflare.com"
	if got != want {
		t.Fatalf("cloudflareURLRe: got %q, want %q", got, want)
	}
}

func TestCloudflareURLRegexNoMatch(t *testing.T) {
	if got := cloudflareURLRe.FindString("still starting up, no url yet"); got != "" {
		t.Fatalf("expected no match, got %q", got)
	}
}

// ---- quick mode end-to-end against a fake cloudflared ----

func TestQuickModeOpenAndClose(t *testing.T) {
	dir := stubTool(t, "cloudflared", `echo "args: $@" > "$(dirname "$0")/argv"
echo "ready at https://demo.trycloudflare.com/x"
sleep 30`)
	p := newCloudflaredPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "tun", Verb: "open",
		Options:    map[string]any{"local_addr": "127.0.0.1:8099"},
		Connection: map[string]any{"start_timeout": "5s"},
	})
	if err != nil || res.Outputs["public_url"] != "https://demo.trycloudflare.com/x" {
		t.Fatalf("open: %v %v", res.Outputs, err)
	}
	if p.leases.Len() != 1 {
		t.Fatalf("leases = %d, want 1", p.leases.Len())
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if !strings.Contains(string(argv), "--url http://127.0.0.1:8099") {
		t.Fatalf("argv: %s", argv)
	}
	lease, _ := res.Outputs["lease"].(string)
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "close", Options: map[string]any{"lease": lease}}); err != nil {
		t.Fatal(err)
	}
	if p.leases.Len() != 0 {
		t.Fatalf("leases after close = %d, want 0", p.leases.Len())
	}
	// close is idempotent.
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "close", Options: map[string]any{"lease": lease}}); err != nil {
		t.Fatal(err)
	}
}

func TestQuickModeExtraArgsAndConfig(t *testing.T) {
	dir := stubTool(t, "cloudflared", `echo "args: $@" > "$(dirname "$0")/argv"
echo "https://x.trycloudflare.com"
sleep 30`)
	p := newCloudflaredPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "tun", Verb: "open",
		Options: map[string]any{"local_addr": "127.0.0.1:9100"},
		Connection: map[string]any{
			"start_timeout": "5s",
			"config":        "/etc/cloudflared/config.yml",
			"extra_args":    []any{"--loglevel", "debug"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.leases.Release(res.Outputs["lease"].(string))
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	s := string(argv)
	if !strings.Contains(s, "--loglevel debug") || !strings.Contains(s, "--config /etc/cloudflared/config.yml") {
		t.Fatalf("argv missing extra_args/config: %s", s)
	}
}

// ---- named mode: URL comes from config, not from output ----

func TestNamedModeUsesHostnameNotOutput(t *testing.T) {
	dir := stubTool(t, "cloudflared", `echo "args: $@" > "$(dirname "$0")/argv"
echo "Registered tunnel connection 0 connIndex=0"
sleep 30`)
	p := newCloudflaredPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "tun", Verb: "open",
		Options: map[string]any{"local_addr": "127.0.0.1:8099"},
		Connection: map[string]any{
			"mode":             "named",
			"tunnel":           "my-tunnel",
			"hostname":         "app.example.com",
			"credentials_file": "/creds/my-tunnel.json",
			"start_timeout":    "5s",
		},
	})
	if err != nil || res.Outputs["public_url"] != "https://app.example.com" {
		t.Fatalf("open: %v %v", res.Outputs, err)
	}
	defer p.leases.Release(res.Outputs["lease"].(string))
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	s := string(argv)
	if !strings.Contains(s, "run") || !strings.Contains(s, "my-tunnel") || !strings.Contains(s, "--cred-file /creds/my-tunnel.json") {
		t.Fatalf("argv: %s", s)
	}
}

func TestNamedModeRequiresTunnelAndHostname(t *testing.T) {
	p := newCloudflaredPlugin()
	for name, conn := range map[string]map[string]any{
		"no tunnel":   {"mode": "named", "hostname": "app.example.com"},
		"no hostname": {"mode": "named", "tunnel": "my-tunnel"},
		"neither":     {"mode": "named"},
	} {
		if _, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: conn}); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestUnknownModeErrors(t *testing.T) {
	p := newCloudflaredPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"mode": "carrier-pigeon"}}); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestOpenBadLocalAddrErrors(t *testing.T) {
	p := newCloudflaredPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "not-an-addr"}}); err == nil {
		t.Fatal("expected error for unparseable local_addr")
	}
}

func TestMissingBinaryErrorsClearly(t *testing.T) {
	p := newCloudflaredPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"binary": "no-such-cloudflared-xyz", "start_timeout": "300ms"}})
	if err == nil || !strings.Contains(err.Error(), "no-such-cloudflared-xyz") {
		t.Fatalf("expected a clear missing-binary error, got %v", err)
	}
}

func TestQuickModeNoURLTimesOut(t *testing.T) {
	stubTool(t, "cloudflared", `echo nothing-url-shaped-here
sleep 5`)
	p := newCloudflaredPlugin()
	start := time.Now()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"start_timeout": "200ms"}})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout took too long")
	}
}

func TestUnknownVerbErrors(t *testing.T) {
	p := newCloudflaredPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "bogus"}); err == nil {
		t.Fatal("expected error for unknown verb")
	}
}

func TestStopReleasesOnlyThatInstance(t *testing.T) {
	stubTool(t, "cloudflared", `echo https://a.trycloudflare.com
sleep 30`)
	p := newCloudflaredPlugin()
	openFor := func(inst string) {
		res, err := p.Invoke(plugin.InvokeRequest{Instance: inst, Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"start_timeout": "5s"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = res
	}
	openFor("a")
	openFor("b")
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

// ---- declarations ----

func TestDeclarationsAreValid(t *testing.T) {
	d := newCloudflaredPlugin().Describe()
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
