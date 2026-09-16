package e2e

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
)

// The four step engines, over the REAL WIRE: each is built, spawned, and
// driven with a plugin.describe and a plugin.run on its stdin/stdout, with
// this test standing in for the daemon on the host.* callbacks the engine
// makes mid-run.
//
// The per-engine unit tests call run() in process, which proves the
// interpreter and the ctx bridge. This proves the thing those cannot: that the
// step's code, inputs and env survive JSON-RPC in both directions, that the
// engine's host.kv request comes back in on the SAME stream while its
// plugin.run is still in flight, and that its outputs decode into the map
// conductor turns into {{.steps.<id>.outputs.*}}.
//
// Every snippet does the same three things in its own language: read an input,
// write one kv key, read it back. Identical inputs, identical outputs — which
// is the point of one shared code-step ABI across four engines.
var engineCases = []struct {
	name     string
	wantType string
	code     string
}{
	{
		name: "js", wantType: "js",
		code: `
const s = ctx.store("cache");
s.set("run", "attempts", 1);
return { repo: ctx.repo, attempts: s.get("run", "attempts") };
`,
	},
	{
		name: "go-embed", wantType: "go-embed",
		code: `
import "conductor/store"

func run(ctx map[string]any) (any, error) {
	st, err := store.Use("cache")
	if err != nil {
		return nil, err
	}
	if err := st.Set("run", "attempts", 1); err != nil {
		return nil, err
	}
	v, err := st.Get("run", "attempts")
	if err != nil {
		return nil, err
	}
	return map[string]any{"repo": ctx["repo"], "attempts": v}, nil
}
`,
	},
	{
		name: "risor", wantType: "risor",
		code: `
s := store("cache")
s.set("run", "attempts", 1)
{"repo": ctx["repo"], "attempts": s.get("run", "attempts")}
`,
	},
	{
		name: "lua", wantType: "lua",
		code: `
local s = ctx.store("cache")
s.set("run", "attempts", 1)
return { repo = ctx.repo, attempts = s.get("run", "attempts") }
`,
	},
}

func TestEnginesRunOverTheWire(t *testing.T) {
	for _, tc := range engineCases {
		t.Run(tc.name, func(t *testing.T) {
			c := rpctest.Start(t, rpctest.BuildEngine(t, tc.name))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			decl, err := c.Describe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if decl.Kind != plugin.KindStep {
				t.Fatalf("decl.Kind = %q, want %q — conductor cannot wire a step engine that does not declare one", decl.Kind, plugin.KindStep)
			}
			if decl.ABI != plugin.EngineABI {
				t.Fatalf("decl.ABI = %d, want %d — the ABI is what selects the plugin.run / host.* shape both sides speak", decl.ABI, plugin.EngineABI)
			}
			if decl.Type != tc.wantType {
				t.Fatalf("decl.Type = %q, want %q", decl.Type, tc.wantType)
			}
			if cap := decl.Capabilities; len(cap.Egress) != 0 || len(cap.Commands) != 0 || len(cap.FS) != 0 || cap.Spawns {
				t.Fatalf("a data-shaping engine declared capabilities: %+v", cap)
			}

			// Stand in for the daemon's ctx data plane: a one-key store,
			// recording what the engine asked for.
			var (
				mu    sync.Mutex
				seen  []plugin.HostRequest
				store = map[string]any{}
			)
			c.SetHost(func(req plugin.HostRequest) plugin.HostResult {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, req)
				if req.RunID != "e2e-run" {
					return plugin.HostResult{Error: "unauthenticated run_id", Refused: true}
				}
				key, _ := req.Args[0].(string)
				k2, _ := req.Args[1].(string)
				switch req.Op {
				case "set":
					store[key+"/"+k2] = req.Args[2]
					return plugin.HostResult{OK: true}
				case "get":
					return plugin.HostResult{OK: true, Value: store[key+"/"+k2]}
				}
				return plugin.HostResult{Error: "unexpected op " + req.Op}
			})

			outputs, err := c.Run(ctx, plugin.RunRequest{
				Instance: tc.name,
				RunID:    "e2e-run",
				Code:     tc.code,
				Inputs:   map[string]any{"repo": "acme/app"},
			})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"repo": "acme/app", "attempts": float64(1)}
			if !reflect.DeepEqual(outputs, want) {
				t.Fatalf("outputs = %v, want %v", outputs, want)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 2 {
				t.Fatalf("host requests = %d, want 2 (a set and a get): %+v", len(seen), seen)
			}
			for i, w := range []struct{ kind, op, resource string }{
				{plugin.HostKindKV, "set", "cache"},
				{plugin.HostKindKV, "get", "cache"},
			} {
				got := seen[i]
				if got.Kind != w.kind || got.Op != w.op || got.Resource != w.resource {
					t.Fatalf("host request %d = %s.%s(%s), want %s.%s(%s)",
						i, got.Kind, got.Op, got.Resource, w.kind, w.op, w.resource)
				}
				if len(got.Args) < 2 || got.Args[0] != "run" || got.Args[1] != "attempts" {
					t.Fatalf("host request %d args = %v, want ns/key first", i, got.Args)
				}
				if got.RunID != "e2e-run" {
					t.Fatalf("host request %d carried run_id %q — the per-run capability did not cross", i, got.RunID)
				}
			}
		})
	}
}

// An engine has no verbs. A config that wired one under connectors: is a
// mistake worth hearing about rather than an empty outputs map.
func TestEnginesHaveNoVerbs(t *testing.T) {
	for _, tc := range engineCases {
		t.Run(tc.name, func(t *testing.T) {
			c := rpctest.Start(t, rpctest.BuildEngine(t, tc.name))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, err := c.Invoke(ctx, plugin.InvokeRequest{Verb: "anything"}); err == nil {
				t.Fatal("plugin.invoke succeeded on a step engine")
			}
		})
	}
}

// The directory a plugin lives in IS its kind: `use: js` under engines:
// resolves to engines/js in this repo, and conductor refuses a plugin whose
// declared kind disagrees with the block it was referenced from.
func TestEngineLayoutMatchesDeclaredKind(t *testing.T) {
	for _, tc := range engineCases {
		t.Run(tc.name, func(t *testing.T) {
			c := rpctest.Start(t, rpctest.BuildEngine(t, tc.name))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			decl, err := c.Describe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if decl.Kind != plugin.KindStep {
				t.Fatalf("engines/%s declares kind %q — it is filed under a directory that means %q",
					tc.name, decl.Kind, plugin.KindStep)
			}
		})
	}
}
