package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit/hosttest"
)

func TestDescribe(t *testing.T) {
	d := describe()
	if d.Kind != plugin.KindStep {
		t.Fatalf("Kind = %q, want %q", d.Kind, plugin.KindStep)
	}
	if d.ABI != plugin.EngineABI {
		t.Fatalf("ABI = %d, want %d", d.ABI, plugin.EngineABI)
	}
	if d.Type != "lua" {
		t.Fatalf("Type = %q, want lua", d.Type)
	}
	c := d.Capabilities
	if len(c.Egress) != 0 || len(c.Commands) != 0 || len(c.FS) != 0 || c.Spawns {
		t.Fatalf("a data-shaping engine declared capabilities: %+v", c)
	}
}

// The script runs, it sees the step's inputs as the `ctx` table, and what it
// returns becomes the step's named outputs.
func TestRunInputsRoundTripAndOutputs(t *testing.T) {
	out, err := execLua(context.Background(), `
return { repo = ctx.repo, next = ctx.count + 1, first = ctx.tags[1] }
`, map[string]any{"repo": "acme/app", "count": 2, "tags": []any{"ci", "urgent"}},
		&hosttest.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"repo": "acme/app", "next": int64(3), "first": "ci"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("outputs = %v, want %v", out, want)
	}
}

// ctx.store's ops reach conductor with the op and the positional args the
// script wrote, and the answer comes back into the script.
func TestCtxStoreRoutesToHost(t *testing.T) {
	h := &hosttest.Host{Reply: func(c hosttest.Call) (any, error) {
		if c.Op == "get" {
			return 41, nil
		}
		return nil, nil
	}}
	out, err := execLua(context.Background(), `
local s = ctx.store("cache")
s.set("run", "attempts", 7)
return { attempts = s.get("run", "attempts") + 1 }
`, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]any{"attempts": int64(42)}); !reflect.DeepEqual(out, want) {
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

// ctx.sql and ctx.memory are the same road, with sql's bind values carried as
// ONE positional argument.
func TestCtxSQLAndMemoryRouteToHost(t *testing.T) {
	h := &hosttest.Host{Reply: func(c hosttest.Call) (any, error) {
		if c.Kind == "sql" {
			return []any{map[string]any{"n": 1}}, nil
		}
		return map[string]any{"id": "m1"}, nil
	}}
	if _, err := execLua(context.Background(), `
ctx.sql("analytics").query("SELECT n FROM t WHERE k = ?", {"k1"})
ctx.memory.remember("a fact", {"tag"}, "repo:o/r")
return {}
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

// A table with string keys is the named outputs, an array-like table a value:
// list, any other value lands under value:, and no return at all is no
// outputs.
func TestOutputContract(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		want       map[string]any
	}{
		{"a scalar", `return 7`, map[string]any{"value": int64(7)}},
		{"a list", `return {1, 2}`, map[string]any{"value": []any{int64(1), int64(2)}}},
		{"no return", `local x = 1`, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execLua(context.Background(), tc.code, nil, &hosttest.Host{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(out, tc.want) {
				t.Fatalf("outputs = %v, want %v", out, tc.want)
			}
		})
	}
}

// THE SANDBOX: only base, table, string and math are opened, and the base
// library's chunk loaders are removed afterwards — so neither the filesystem
// nor a new chunk assembled from data is reachable.
func TestSandboxWithholdsEscapeLibraries(t *testing.T) {
	out, err := execLua(context.Background(), `
return {
  os = type(os), io = type(io), debug = type(debug), package = type(package),
  dofile = type(dofile), loadfile = type(loadfile),
  load = type(load), loadstring = type(loadstring),
  str = type(string), math = type(math), tbl = type(table),
}
`, nil, &hosttest.Host{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"os", "io", "debug", "package", "dofile", "loadfile", "load", "loadstring"} {
		if out[k] != "nil" {
			t.Fatalf("%s is %v inside the sandbox, want nil", k, out[k])
		}
	}
	for _, k := range []string{"str", "math", "tbl"} {
		if out[k] != "table" {
			t.Fatalf("the %s library is %v, want table", k, out[k])
		}
	}
}

// A refused or failed op RAISES inside the script, as it did in-binary — so a
// script can pcall it, and one that doesn't fails the step.
func TestHostErrorRaisesInScript(t *testing.T) {
	h := &hosttest.Host{Reply: func(hosttest.Call) (any, error) { return nil, errRefused{} }}
	_, err := execLua(context.Background(), `return { v = ctx.store("cache").get("ns", "k") }`, nil, h)
	if err == nil || !strings.Contains(err.Error(), "refused by conductor") {
		t.Fatalf("err = %v, want the refusal to surface", err)
	}

	out, err := execLua(context.Background(), `
local ok, msg = pcall(function() return ctx.store("cache").get("ns", "k") end)
return { ok = ok, caught = string.find(msg, "refused by conductor") ~= nil }
`, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if out["ok"] != false || out["caught"] != true {
		t.Fatalf("pcall = %v, want a catchable raise", out)
	}
}

type errRefused struct{}

func (errRefused) Error() string { return "refused by conductor: kv.get: not allowed" }
