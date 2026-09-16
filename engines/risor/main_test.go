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
	if d.Type != "risor" {
		t.Fatalf("Type = %q, want risor", d.Type)
	}
	c := d.Capabilities
	if len(c.Egress) != 0 || len(c.Commands) != 0 || len(c.FS) != 0 || c.Spawns {
		t.Fatalf("a data-shaping engine declared capabilities: %+v", c)
	}
}

// The script runs, it sees the step's inputs as `ctx`, and its FINAL
// EXPRESSION becomes the step's named outputs.
func TestRunInputsRoundTripAndOutputs(t *testing.T) {
	out, err := execRisor(context.Background(), `
owner := strings.split(ctx["repo"], "/")[0]
{"owner": owner, "next": ctx["count"] + 1}
`, map[string]any{"repo": "acme/app", "count": 2}, &hosttest.Host{})
	if err != nil {
		t.Fatal(err)
	}
	if got := out["owner"]; got != "acme" {
		t.Fatalf("outputs.owner = %v, want acme", got)
	}
	if got := out["next"]; got != int64(3) {
		t.Fatalf("outputs.next = %v (%T), want 3", got, got)
	}
}

// store()'s ops reach conductor with the op and the positional args the
// script wrote, and the answer comes back into the script.
func TestStoreRoutesToHost(t *testing.T) {
	h := &hosttest.Host{Reply: func(c hosttest.Call) (any, error) {
		if c.Op == "get" {
			return 41, nil
		}
		return nil, nil
	}}
	out, err := execRisor(context.Background(), `
s := store("cache")
s.set("run", "attempts", 7)
{"attempts": s.get("run", "attempts")}
`, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if got := out["attempts"]; got != int64(41) && got != float64(41) {
		t.Fatalf("outputs.attempts = %v (%T), want 41", got, got)
	}
	want := []hosttest.Call{
		{Kind: "kv", Op: "set", Resource: "cache", Args: []any{"run", "attempts", float64(7)}},
		{Kind: "kv", Op: "get", Resource: "cache", Args: []any{"run", "attempts"}},
	}
	if got := h.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("host calls = %v, want %v", got, want)
	}
}

// sql() and memory are the same road, with sql's bind values carried as ONE
// positional argument.
func TestSQLAndMemoryRouteToHost(t *testing.T) {
	h := &hosttest.Host{Reply: func(c hosttest.Call) (any, error) {
		if c.Kind == "sql" {
			return []any{map[string]any{"n": 1}}, nil
		}
		return map[string]any{"id": "m1"}, nil
	}}
	if _, err := execRisor(context.Background(), `
sql("analytics").query("SELECT n FROM t WHERE k = ?", ["k1"])
memory.remember("a fact", ["tag"], "repo:o/r")
{}
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

// A result that is not a map is the step's single `value` output.
func TestOutputContract(t *testing.T) {
	out, err := execRisor(context.Background(), `7`, nil, &hosttest.Host{})
	if err != nil {
		t.Fatal(err)
	}
	if got := out["value"]; got != int64(7) {
		t.Fatalf("outputs = %v, want value:7", out)
	}
}

// THE SANDBOX: risor's default globals include os, exec, http, dns, net and
// filepath. The engine opts out of the defaults and grants an allowlist, so a
// script reaching for any of those finds nothing.
func TestSandboxWithholdsEscapeModules(t *testing.T) {
	for _, mod := range []string{"os", "exec", "http", "net", "filepath", "fetch"} {
		t.Run(mod, func(t *testing.T) {
			if _, err := execRisor(context.Background(), mod+".x", nil, &hosttest.Host{}); err == nil {
				t.Fatalf("%q resolved inside the sandbox — the allowlist has a hole", mod)
			}
		})
	}
	// …while the data-shaping modules that ARE granted still work.
	if _, err := execRisor(context.Background(),
		`{"v": json.marshal({"a": 1})}`, nil, &hosttest.Host{}); err != nil {
		t.Fatalf("an allowed module failed: %v", err)
	}
}

// A refused or failed op surfaces as a risor error and fails the step.
func TestHostErrorFailsTheStep(t *testing.T) {
	h := &hosttest.Host{Reply: func(hosttest.Call) (any, error) { return nil, errRefused{} }}
	_, err := execRisor(context.Background(), `store("cache").get("ns", "k")`, nil, h)
	if err == nil || !strings.Contains(err.Error(), "refused by conductor") {
		t.Fatalf("err = %v, want the refusal to surface", err)
	}
}

type errRefused struct{}

func (errRefused) Error() string { return "refused by conductor: kv.get: not allowed" }
