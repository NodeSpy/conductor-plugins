package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestPaseoPluginVerbRoundTrip builds the real conductor-paseo plugin and
// round-trips its verbs over the protocol: list_agents and archive_agent go
// out as plugin.invoke calls, the plugin shells to a stub `paseo` CLI (pointed
// at via the paseo_bin connection field), and the CLI's JSON comes back as
// typed verb outputs.
//
// Provenance: this replaces the real-binary half of conductor's
// internal/dispatch.TestRPCBackendRoundTripsListAgentsAndArchive, which built
// this plugin out of conductor's tree. The DAEMON half of that round trip
// (dispatch.rpcBackend mapping Backend methods onto these verbs, retries, and
// output parsing) stays in conductor, where it now drives an in-repo stub
// plugin instead — see conductor's test/plugins/acme-paseo.
func TestPaseoPluginVerbRoundTrip(t *testing.T) {
	bin := rpctest.BuildRuntime(t, "paseo")

	stubDir := t.TempDir()
	stubBin := filepath.Join(stubDir, "paseo")
	script := `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
echo "$@" >> "$dir/calls.log"
case "$1" in
  ls) cat "$dir/ls.json" 2>/dev/null || echo '[]' ;;
  archive) exit 0 ;;
  *) echo '{}' ;;
esac
`
	if err := os.WriteFile(stubBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "ls.json"),
		[]byte(`[{"id":"a-1","cwd":"/wt/one","status":"idle"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	c := rpctest.Start(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	decl, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if decl.Type != "paseo" {
		t.Fatalf("decl.Type = %q, want paseo", decl.Type)
	}
	// Every operation conductor's dispatch.Backend interface needs must be
	// declared, or the daemon's rpcBackend has nothing to call.
	wantVerbs := map[string]bool{
		"run": false, "list_agents": false, "inspect": false, "archive_agent": false,
		"archive_workspace": false, "create_worktree": false, "create_workspace": false,
		"list_workspaces": false, "clone": false, "send": false, "wait": false,
	}
	for _, v := range decl.Verbs {
		if _, ok := wantVerbs[v.Name]; ok {
			wantVerbs[v.Name] = true
		}
	}
	for name, seen := range wantVerbs {
		if !seen {
			t.Errorf("decl missing verb %q", name)
		}
	}

	conn := map[string]any{"paseo_bin": stubBin}

	out, err := c.Invoke(ctx, plugin.InvokeRequest{
		Instance: "paseo1", Verb: "list_agents", Connection: conn,
		Options: map[string]any{"labels": map[string]any{"conductor": "1"}},
	})
	if err != nil {
		t.Fatalf("list_agents: %v", err)
	}
	agents, _ := out["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("list_agents outputs = %+v, want 1 agent", out)
	}
	a, _ := agents[0].(map[string]any)
	if a["id"] != "a-1" || a["cwd"] != "/wt/one" || a["status"] != "idle" {
		t.Fatalf("agent = %+v", a)
	}

	if _, err := c.Invoke(ctx, plugin.InvokeRequest{
		Instance: "paseo1", Verb: "archive_agent", Connection: conn,
		Options: map[string]any{"id": "a-1"},
	}); err != nil {
		t.Fatalf("archive_agent: %v", err)
	}

	calls, _ := os.ReadFile(filepath.Join(stubDir, "calls.log"))
	if !strings.Contains(string(calls), "--label conductor=1") {
		t.Errorf("list_agents should have forwarded the label filter to the CLI, got:\n%s", calls)
	}
	if !strings.Contains(string(calls), "archive a-1") {
		t.Errorf("archive_agent should have shelled `archive a-1`, got:\n%s", calls)
	}
}

// TestPaseoPluginUnknownVerbErrors proves the plugin returns a structured
// JSON-RPC error (not a crash or a silent empty result) for a verb it does not
// implement — the shape the daemon surfaces to the operator.
func TestPaseoPluginUnknownVerbErrors(t *testing.T) {
	bin := rpctest.BuildRuntime(t, "paseo")
	c := rpctest.Start(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := c.Invoke(ctx, plugin.InvokeRequest{
		Instance: "paseo1", Verb: "definitely_not_a_verb",
		Connection: map[string]any{"paseo_bin": "/nonexistent"},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown verb")
	}
	if !strings.Contains(err.Error(), "plugin error") {
		t.Fatalf("want a structured plugin error, got %v", err)
	}
}
