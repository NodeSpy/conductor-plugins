package e2e

import (
	"context"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
)

// Every plugin in this repo must describe its OWN KIND and its OWN PERMISSION
// MANIFEST, over the real transport.
//
// Conductor derives the kind from the config block a `use:` reference appears
// in and refuses the plugin when the two disagree — so a connector can never be
// wired as a runtime. That check only bites if the plugin actually declares its
// kind, which is what this asserts.
//
// The manifest is what conductor records at install, shows the operator when
// they add the plugin, and confines the subprocess to. Declaring MORE than a
// plugin needs quietly widens what the operator is asked to accept, so these
// assertions are deliberately exact.
func TestPluginsDeclareKindAndManifest(t *testing.T) {
	for _, tc := range []struct {
		kind     string
		name     string
		wantKind plugin.Kind
		wantType string
		check    func(t *testing.T, c plugin.Capabilities)
	}{
		{
			kind: "connectors", name: "sentry",
			wantKind: plugin.KindConnector, wantType: "sentry",
			check: func(t *testing.T, c plugin.Capabilities) {
				// Inbound only: it listens, it never dials out, it spawns
				// nothing. The empty manifest is the claim.
				if len(c.Egress) != 0 || len(c.Commands) != 0 || c.Spawns {
					t.Fatalf("a source-only connector declared capabilities: %+v", c)
				}
			},
		},
		{
			kind: "connectors", name: "pagerduty",
			wantKind: plugin.KindConnector, wantType: "pagerduty",
			check: func(t *testing.T, c plugin.Capabilities) {
				if len(c.Egress) != 0 || len(c.Commands) != 0 || c.Spawns {
					t.Fatalf("a source-only connector declared capabilities: %+v", c)
				}
			},
		},
		{
			kind: "connectors", name: "github",
			wantKind: plugin.KindConnector, wantType: "github",
			check: func(t *testing.T, c plugin.Capabilities) {
				if len(c.Egress) == 0 {
					t.Fatal("the github connector calls the API but declares no egress")
				}
				if len(c.Commands) != 0 || c.Spawns {
					t.Fatalf("the github connector should spawn nothing: %+v", c)
				}
				var sawAPI bool
				for _, e := range c.Egress {
					if e == "api.github.com:443" {
						sawAPI = true
					}
				}
				if !sawAPI {
					t.Fatalf("declared egress does not include api.github.com:443: %v", c.Egress)
				}
			},
		},
		{
			kind: "runtimes", name: "paseo",
			wantKind: plugin.KindRuntime, wantType: "paseo",
			check: func(t *testing.T, c plugin.Capabilities) {
				// Every verb is a `paseo ...` shell-out, so that is exactly the
				// one command conductor confines its PATH to.
				if len(c.Commands) != 1 || c.Commands[0] != "paseo" {
					t.Fatalf("declared commands = %v, want exactly [paseo]", c.Commands)
				}
				if len(c.Egress) != 0 {
					t.Fatalf("the paseo runtime dials nothing itself: %v", c.Egress)
				}
			},
		},
	} {
		t.Run(tc.kind+"/"+tc.name, func(t *testing.T) {
			c := rpctest.Start(t, rpctest.Build(t, tc.kind, tc.name))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			decl, err := c.Describe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if decl.Kind != tc.wantKind {
				t.Fatalf("decl.Kind = %q, want %q — conductor cannot enforce the kind of a plugin that does not declare one", decl.Kind, tc.wantKind)
			}
			if decl.Type != tc.wantType {
				t.Fatalf("decl.Type = %q, want %q", decl.Type, tc.wantType)
			}
			tc.check(t, decl.Capabilities)
		})
	}
}

// The directory a plugin lives in IS its kind, and conductor's `use:` resolver
// relies on that: a bare `use: sentry` resolves to connectors/sentry in this
// repo, and `use: paseo` under runtimes: to runtimes/paseo. A plugin filed
// under the wrong directory would be fetched for the wrong block.
func TestLayoutMatchesDeclaredKind(t *testing.T) {
	for _, tc := range []struct {
		dir, name string
		want      plugin.Kind
	}{
		{"connectors", "sentry", plugin.KindConnector},
		{"connectors", "pagerduty", plugin.KindConnector},
		{"connectors", "github", plugin.KindConnector},
		{"runtimes", "paseo", plugin.KindRuntime},
	} {
		t.Run(tc.dir+"/"+tc.name, func(t *testing.T) {
			c := rpctest.Start(t, rpctest.Build(t, tc.dir, tc.name))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			decl, err := c.Describe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if decl.Kind != tc.want {
				t.Fatalf("%s/%s declares kind %q — it is filed under a directory that means %q",
					tc.dir, tc.name, decl.Kind, tc.want)
			}
		})
	}
}
