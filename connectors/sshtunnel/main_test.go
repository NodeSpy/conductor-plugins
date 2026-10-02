package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/exposurekit"
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

// ---- argv presets (ported from conductor's old sshArgv tests) ----

func TestSSHArgvPinggyPreset(t *testing.T) {
	for _, host := range []string{"pinggy", "a.pinggy.io"} {
		argv := sshArgv(sshArgs{preset: detectPreset(host), host: host, localPort: "8099"})
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, "-p 443") || !strings.Contains(joined, "-R 0:localhost:8099") || !strings.HasSuffix(joined, host) {
			t.Fatalf("pinggy argv for %q looks wrong: %v", host, argv)
		}
	}
}

func TestSSHArgvGenericHost(t *testing.T) {
	for _, host := range []string{"localhost.run", "serveo.net", "ssh.example.com"} {
		argv := sshArgv(sshArgs{preset: detectPreset(host), host: host, localPort: "8099"})
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, "-R 80:localhost:8099") || !strings.HasSuffix(joined, host) {
			t.Fatalf("generic argv for %q looks wrong: %v", host, argv)
		}
	}
}

func TestSSHArgvCustomPortRemotePortUserIdentity(t *testing.T) {
	argv := sshArgv(sshArgs{preset: "generic", host: "tunnel.example.com", user: "tun", identity: "/home/me/.ssh/id_ed25519", port: "2222", remotePort: "8080", localPort: "9000"})
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-i /home/me/.ssh/id_ed25519") || !strings.Contains(joined, "-p 2222") ||
		!strings.Contains(joined, "-R 8080:localhost:9000") || !strings.HasSuffix(joined, "tun@tunnel.example.com") {
		t.Fatalf("argv: %v", argv)
	}
}

func TestSSHArgvExtraArgs(t *testing.T) {
	argv := sshArgv(sshArgs{preset: "generic", host: "localhost.run", extra: []string{"-o", "ServerAliveInterval=30"}, localPort: "8099"})
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "ServerAliveInterval=30") {
		t.Fatalf("argv missing extra_args: %v", argv)
	}
}

func TestDetectPreset(t *testing.T) {
	cases := map[string]string{
		"pinggy":          "pinggy",
		"a.pinggy.io":     "pinggy",
		"sub.pinggy.io":   "pinggy",
		"localhost.run":   "generic",
		"serveo.net":      "generic",
		"ssh.example.com": "generic",
	}
	for host, want := range cases {
		if got := detectPreset(host); got != want {
			t.Errorf("detectPreset(%q) = %q, want %q", host, got, want)
		}
	}
}

// ---- generic URL regex per-line (mirrors RunAndScan's own scan) ----

func TestGenericURLRegexPerLine(t *testing.T) {
	cases := []struct{ line, want string }{
		{"Connect to https://a1b2c3.localhost.run or use the address above", "https://a1b2c3.localhost.run"},
		{"tunneled with tls termination, https://a1b2c3.localhost.run", "https://a1b2c3.localhost.run"},
		{"Web Debugger: http://127.0.0.1:4300", "http://127.0.0.1:4300"},
		{"https://rndsub.a.pinggy.link", "https://rndsub.a.pinggy.link"},
		{"no url on this line", ""},
	}
	for _, tc := range cases {
		if got := exposurekit.DefaultURL.FindString(tc.line); got != tc.want {
			t.Errorf("FindString(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// ---- end-to-end against a fake ssh ----

func TestOpenAndCloseGeneric(t *testing.T) {
	stubTool(t, "ssh", `echo "tunneled https://xyz.lhr.life"
sleep 30`)
	p := newSSHTunnelPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "tun", Verb: "open",
		Options:    map[string]any{"local_addr": "127.0.0.1:8099"},
		Connection: map[string]any{"host": "localhost.run", "start_timeout": "5s"},
	})
	if err != nil || res.Outputs["public_url"] != "https://xyz.lhr.life" {
		t.Fatalf("open: %v %v", res.Outputs, err)
	}
	lease, _ := res.Outputs["lease"].(string)
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "close", Options: map[string]any{"lease": lease}}); err != nil {
		t.Fatal(err)
	}
	if p.leases.Len() != 0 {
		t.Fatalf("leases after close = %d, want 0", p.leases.Len())
	}
}

func TestOpenPinggyPreset(t *testing.T) {
	dir := stubTool(t, "ssh", `echo "args: $@" > "$(dirname "$0")/argv"
echo "https://rndsub.a.pinggy.link"
sleep 30`)
	p := newSSHTunnelPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "open",
		Options:    map[string]any{"local_addr": "127.0.0.1:8099"},
		Connection: map[string]any{"host": "a.pinggy.io", "start_timeout": "5s"},
	})
	if err != nil || res.Outputs["public_url"] != "https://rndsub.a.pinggy.link" {
		t.Fatalf("open: %v %v", res.Outputs, err)
	}
	defer p.leases.Release(res.Outputs["lease"].(string))
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if !strings.Contains(string(argv), "-p 443") {
		t.Fatalf("argv: %s", argv)
	}
}

func TestHostRequired(t *testing.T) {
	p := newSSHTunnelPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}}); err == nil {
		t.Fatal("expected error when host is missing")
	}
}

func TestBadLocalAddrErrors(t *testing.T) {
	p := newSSHTunnelPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "bad"}, Connection: map[string]any{"host": "localhost.run"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestNoURLTimesOut(t *testing.T) {
	stubTool(t, "ssh", `echo nothing-url-shaped
sleep 5`)
	p := newSSHTunnelPlugin()
	start := time.Now()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"host": "localhost.run", "start_timeout": "200ms"}})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout took too long")
	}
}

func TestStopReleasesOnlyThatInstance(t *testing.T) {
	stubTool(t, "ssh", `echo https://a.lhr.life
sleep 30`)
	p := newSSHTunnelPlugin()
	for _, inst := range []string{"a", "b"} {
		if _, err := p.Invoke(plugin.InvokeRequest{Instance: inst, Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"host": "localhost.run", "start_timeout": "5s"}}); err != nil {
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
	p := newSSHTunnelPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "bogus"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeclarationsAreValid(t *testing.T) {
	d := newSSHTunnelPlugin().Describe()
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
