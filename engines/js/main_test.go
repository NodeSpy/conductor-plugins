package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit/hosttest"
)

// The declaration is what makes conductor treat this binary as an engine at
// all: Kind AND ABI, plus a manifest that claims nothing.
func TestDescribe(t *testing.T) {
	d := describe()
	if d.Kind != plugin.KindStep {
		t.Fatalf("Kind = %q, want %q", d.Kind, plugin.KindStep)
	}
	if d.ABI != plugin.EngineABI {
		t.Fatalf("ABI = %d, want %d", d.ABI, plugin.EngineABI)
	}
	if d.Type != "js" {
		t.Fatalf("Type = %q, want js", d.Type)
	}
	c := d.Capabilities
	if len(c.Egress) != 0 || len(c.Commands) != 0 || len(c.FS) != 0 || c.Spawns {
		t.Fatalf("a data-shaping engine declared capabilities: %+v", c)
	}
}

// The snippet runs, it sees the step's inputs as `ctx`, and what it returns
// becomes the step's named outputs.
func TestRunInputsRoundTripAndOutputs(t *testing.T) {
	out, err := execJS(context.Background(),
		`return {repo: ctx.repo, next: ctx.count + 1, first: ctx.tags[0]}`,
		map[string]any{"repo": "acme/app", "count": 2, "tags": []any{"ci", "urgent"}},
		&hosttest.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"repo": "acme/app", "next": float64(3), "first": "ci"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("outputs = %v, want %v", out, want)
	}
}

// ctx.store's ops reach conductor with the op and the positional args the
// snippet wrote, and the answer comes back into the script.
func TestCtxStoreRoutesToHost(t *testing.T) {
	h := &hosttest.Host{Reply: func(c hosttest.Call) (any, error) {
		if c.Op == "get" {
			return 41, nil
		}
		return nil, nil
	}}
	out, err := execJS(context.Background(), `
const s = ctx.store("cache");
s.set("run", "attempts", 7);
return { attempts: s.get("run", "attempts") + 1 };
`, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]any{"attempts": float64(42)}); !reflect.DeepEqual(out, want) {
		t.Fatalf("outputs = %v, want %v", out, want)
	}
	want := []hosttest.Call{
		{Kind: "kv", Op: "set", Resource: "cache", Args: []any{"run", "attempts", float64(7)}},
		{Kind: "kv", Op: "get", Resource: "cache", Args: []any{"run", "attempts"}},
	}
	if got := h.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("host calls = %v, want %v", got, want)
	}
}

// ctx.sql and ctx.memory are the same road: one host round trip per op, with
// sql's bind values carried as ONE positional argument.
func TestCtxSQLAndMemoryRouteToHost(t *testing.T) {
	h := &hosttest.Host{Reply: func(c hosttest.Call) (any, error) {
		if c.Kind == "sql" {
			return []any{map[string]any{"n": 1}}, nil
		}
		return []any{}, nil
	}}
	if _, err := execJS(context.Background(), `
ctx.sql("analytics").query("SELECT n FROM t WHERE k = ?", ["k1"]);
ctx.memory.remember("a fact", ["tag"], "repo:o/r");
return {};
`, nil, h); err != nil {
		t.Fatal(err)
	}
	want := []hosttest.Call{
		{Kind: "sql", Op: "query", Resource: "analytics",
			Args: []any{"SELECT n FROM t WHERE k = ?", []any{"k1"}}},
		{Kind: "memory", Op: "remember", Resource: "",
			Args: []any{"a fact", []any{"tag"}, "repo:o/r"}},
	}
	if got := h.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("host calls = %v, want %v", got, want)
	}
}

// A result that is not an object is the step's single `value` output, and no
// return at all is no outputs — the shared wrapValue contract.
func TestOutputContract(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		want       map[string]any
	}{
		{"a scalar", `return 7`, map[string]any{"value": float64(7)}},
		{"a list", `return [1]`, map[string]any{"value": []any{float64(1)}}},
		{"no return", `const x = 1;`, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execJS(context.Background(), tc.code, nil, &hosttest.Host{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(out, tc.want) {
				t.Fatalf("outputs = %v, want %v", out, tc.want)
			}
		})
	}
}

// A refused or failed op throws inside the VM, exactly as it did in-binary,
// and the thrown Error fails the step rather than being swallowed.
func TestHostErrorThrowsInScript(t *testing.T) {
	h := &hosttest.Host{Reply: func(hosttest.Call) (any, error) {
		return nil, errRefused{}
	}}
	_, err := execJS(context.Background(), `return {v: ctx.store("cache").get("ns", "k")}`, nil, h)
	if err == nil || !strings.Contains(err.Error(), "refused by conductor") {
		t.Fatalf("err = %v, want the refusal to surface", err)
	}
}

type errRefused struct{}

func (errRefused) Error() string { return "refused by conductor: kv.get: not allowed" }

// The VM reaches nothing this process can see: node/browser globals a snippet
// might reach for simply are not there, and the only holes are the three
// __conductor_* bridges.
func TestNoAmbientHostAccess(t *testing.T) {
	out, err := execJS(context.Background(), `
return {
  process: typeof process,
  require: typeof require,
  fetch: typeof fetch,
  kv: typeof __conductor_kv,
};
`, nil, &hosttest.Host{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"process", "require", "fetch"} {
		if out[k] != "undefined" {
			t.Fatalf("%s is %v inside the sandbox, want undefined", k, out[k])
		}
	}
	if out["kv"] != "function" {
		t.Fatalf("the kv bridge is %v, want function", out["kv"])
	}
}
