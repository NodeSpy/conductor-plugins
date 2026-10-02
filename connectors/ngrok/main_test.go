package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// ---- response parsing (ported from conductor's old tunnel tests) ----

func TestNgrokTunnelsResponseParsing(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{
			name: "one tunnel",
			body: `{"tunnels":[{"name":"command_line","uri":"/api/tunnels/command_line","public_url":"https://abcd1234.ngrok-free.app","proto":"https","config":{"addr":"http://localhost:8099","inspect":true}}],"uri":"/api/tunnels"}`,
			want: "https://abcd1234.ngrok-free.app",
		},
		{
			name: "http and https variants, first wins",
			body: `{"tunnels":[{"public_url":"http://abcd1234.ngrok-free.app"},{"public_url":"https://abcd1234.ngrok-free.app"}]}`,
			want: "http://abcd1234.ngrok-free.app",
		},
		{
			name:    "no tunnels yet",
			body:    `{"tunnels":[]}`,
			wantErr: true,
		},
		{
			name:    "not json",
			body:    `not json at all`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNgrokTunnelsResponse([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got url %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- end-to-end: a real httptest server stands in for ngrok's local API,
// and the fake "ngrok" binary just sleeps (its own stdout is never scanned
// for this plugin — only the API is polled). ----

func TestOpenAndClosePollsTheLocalAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tunnels": []map[string]any{{"public_url": "https://abcd1234.ngrok-free.app"}},
		})
	}))
	defer srv.Close()
	apiAddr := strings.TrimPrefix(srv.URL, "http://")

	dir := stubTool(t, "ngrok", `echo "args: $@" > "$(dirname "$0")/argv"
sleep 30`)
	p := newNgrokPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "tun", Verb: "open",
		Options:    map[string]any{"local_addr": "127.0.0.1:8099"},
		Connection: map[string]any{"api_addr": apiAddr, "start_timeout": "5s", "authtoken": "tok123"},
	})
	if err != nil || res.Outputs["public_url"] != "https://abcd1234.ngrok-free.app" {
		t.Fatalf("open: %v %v", res.Outputs, err)
	}
	// open() only polls the API; it doesn't wait on the fake process's own
	// output, so give the stub script a moment to write the file it was
	// asked to produce.
	var s string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(dir, "argv"))
		if len(b) > 0 {
			s = string(b)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(s, "--web-addr "+apiAddr) || !strings.Contains(s, "--authtoken tok123") || !strings.Contains(s, "127.0.0.1:8099") {
		t.Fatalf("argv: %q", s)
	}
	lease, _ := res.Outputs["lease"].(string)
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "close", Options: map[string]any{"lease": lease}}); err != nil {
		t.Fatal(err)
	}
	if p.leases.Len() != 0 {
		t.Fatalf("leases after close = %d, want 0", p.leases.Len())
	}
}

// TestOpenPollTimeout: with no local ngrok API answering, open fails after
// the poll deadline instead of hanging. Ported from conductor's old
// TestNgrokOpenPollTimeout.
func TestOpenPollTimeout(t *testing.T) {
	stubTool(t, "ngrok", `exec sleep 30`)
	p := newNgrokPlugin()
	start := time.Now()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "open",
		Options:    map[string]any{"local_addr": "127.0.0.1:8099"},
		Connection: map[string]any{"api_addr": "127.0.0.1:1", "start_timeout": "400ms"},
	})
	if err == nil || !strings.Contains(err.Error(), "no tunnel URL") {
		t.Fatalf("poll deadline: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("poll did not respect its deadline")
	}
	if p.leases.Len() != 0 {
		t.Fatal("a failed open left a lease")
	}
}

func TestBadLocalAddrErrors(t *testing.T) {
	p := newNgrokPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "bad"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestMissingBinaryErrorsClearly(t *testing.T) {
	p := newNgrokPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"binary": "no-such-ngrok-xyz"}})
	if err == nil || !strings.Contains(err.Error(), "no-such-ngrok-xyz") {
		t.Fatalf("expected a clear missing-binary error, got %v", err)
	}
}

func TestStopReleasesOnlyThatInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tunnels": []map[string]any{{"public_url": "https://a.ngrok-free.app"}}})
	}))
	defer srv.Close()
	apiAddr := strings.TrimPrefix(srv.URL, "http://")
	stubTool(t, "ngrok", `sleep 30`)
	p := newNgrokPlugin()
	for _, inst := range []string{"a", "b"} {
		if _, err := p.Invoke(plugin.InvokeRequest{Instance: inst, Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"api_addr": apiAddr, "start_timeout": "5s"}}); err != nil {
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
	p := newNgrokPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "bogus"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeclarationsAreValid(t *testing.T) {
	d := newNgrokPlugin().Describe()
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
