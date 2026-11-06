package e2e

import (
	"context"
	"reflect"
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
				// Inbound only: it serves its listen address, reaches only the
				// optional smee relay, and spawns nothing.
				if !reflect.DeepEqual(c.Listen, []string{"config:listen"}) || !reflect.DeepEqual(c.Egress, []string{"config:smee"}) || len(c.Commands) != 0 {
					t.Fatalf("a source-only connector should declare its listener and relay only: %+v", c)
				}
			},
		},
		{
			kind: "connectors", name: "pagerduty",
			wantKind: plugin.KindConnector, wantType: "pagerduty",
			check: func(t *testing.T, c plugin.Capabilities) {
				// Inbound only: it serves its listen address, reaches only the
				// optional smee relay, and spawns nothing.
				if !reflect.DeepEqual(c.Listen, []string{"config:listen"}) || !reflect.DeepEqual(c.Egress, []string{"config:smee"}) || len(c.Commands) != 0 {
					t.Fatalf("a source-only connector should declare its listener and relay only: %+v", c)
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
				// The commands it runs: the write chain's `gh auth token` (the
				// decision on the confined write credential), and pr_snapshot's git.
				if len(c.Commands) != 2 || c.Commands[0] != "gh" || c.Commands[1] != "git" {
					t.Fatalf("the github connector should declare exactly the gh and git commands: %+v", c)
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
				// Every verb is a `paseo ...` shell-out. The paseo CLI's own
				// launcher is a `#!/usr/bin/env -S node ...` script, so "env"
				// and "node" are on the exec chain too — all three, and only
				// those three, are the commands conductor confines its PATH to.
				wantCommands := map[string]bool{"paseo": true, "env": true, "node": true}
				if len(c.Commands) != len(wantCommands) {
					t.Fatalf("declared commands = %v, want exactly %v", c.Commands, wantCommands)
				}
				for _, cmd := range c.Commands {
					if !wantCommands[cmd] {
						t.Fatalf("unexpected declared command %q: %v", cmd, c.Commands)
					}
				}
				// Egress cannot be honestly declared: the CLI dials the local
				// daemon on a port it reads at call time from its own pid-lock
				// file (ephemeral unless pinned), or an arbitrary host via
				// PASEO_HOST — neither is a static value this plugin's own
				// declaration can name. See the runtime's Describe() comment.
				if len(c.Egress) != 0 {
					t.Fatalf("the paseo runtime's egress cannot be statically declared: %v", c.Egress)
				}
				wantEnv := map[string]bool{"PASEO_HOME": true, "PASEO_HOST": true, "PASEO_PASSWORD": true}
				if len(c.Env) != len(wantEnv) {
					t.Fatalf("declared env = %v, want exactly %v", c.Env, wantEnv)
				}
				for _, e := range c.Env {
					if !wantEnv[e] {
						t.Fatalf("unexpected declared env %q: %v", e, c.Env)
					}
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
