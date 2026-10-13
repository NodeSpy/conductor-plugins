package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghplugin"
	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghsource/ghsourcetest"
	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
)

// GITHUB PARITY, from this repository's side. Conductor's github source
// conformance suite (pkg/githubkit/ghsource/ghsourcetest) is ONE table that
// conductor's own CI runs against its bundled github connector and against
// this plugin driven through the daemon's real plugin path. Here it runs
// against THIS repository's release build of the plugin, over the bare wire:
// every event kind, review folding in both delivery orders, the closed-PR
// drop, ignore_checks, the merge-state triggers, merge_ready's gate, issue
// matching, release prereleases, repo routing, a unified filter predicate, the
// sweep's four recoveries, and a nudge. A difference from the bundled
// connector fails the same assertion in both repositories.
func TestGithubConformance(t *testing.T) {
	ghsourcetest.Run(t, ghsourcetest.WireStarter(rpctest.BuildConnector(t, "github")))
}

// The built plugin describes itself with THE github declaration — the one
// conductor's bundled connector is built from — byte for byte.
func TestGithubDeclIsTheBundledDecl(t *testing.T) {
	c := rpctest.Start(t, rpctest.BuildConnector(t, "github"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := ghplugin.Decl()
	if want.ProtocolVersion == 0 {
		want.ProtocolVersion = got.ProtocolVersion // stamped by Serve
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("the plugin's declaration differs from conductor's github declaration\n got %s\nwant %s", gb, wb)
	}
}
