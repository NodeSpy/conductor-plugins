package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func TestDescribe(t *testing.T) {
	d := describe()
	if d.Kind != plugin.KindStep {
		t.Fatalf("Kind = %q, want %q", d.Kind, plugin.KindStep)
	}
	if d.ABI != plugin.EngineABI {
		t.Fatalf("ABI = %d, want %d", d.ABI, plugin.EngineABI)
	}
	if d.Type != "cel" {
		t.Fatalf("Type = %q, want cel", d.Type)
	}
	c := d.Capabilities
	if len(c.Egress) != 0 || len(c.Commands) != 0 || len(c.FS) != 0 || c.Spawns {
		t.Fatalf("a data-shaping engine declared capabilities: %+v", c)
	}
}

// A scalar-producing expression lands under the shared value: key.
func TestRunScalarProducesValue(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   "ctx.a + ctx.b",
		Inputs: map[string]any{"a": int64(2), "b": int64(3)},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"value": int64(5)}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// A map-producing expression's keys become the step's named outputs.
func TestRunMapProducesNamedOutputs(t *testing.T) {
	out, err := evalCEL(context.Background(),
		`{"repo": ctx.repo, "next": ctx.count + 1}`,
		map[string]any{"repo": "acme/app", "count": int64(2)})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"repo": "acme/app", "next": int64(3)}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("outputs = %#v, want %#v", out, want)
	}
}

// A list-producing expression (here, the filter macro) lands under value: as
// a Go []any, in filtered order.
func TestRunListProducesValueList(t *testing.T) {
	out, err := evalCEL(context.Background(),
		"ctx.items.filter(x, x.active)",
		map[string]any{"items": []any{
			map[string]any{"name": "a", "active": true},
			map[string]any{"name": "b", "active": false},
			map[string]any{"name": "c", "active": true},
		}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"value": []any{
		map[string]any{"name": "a", "active": true},
		map[string]any{"name": "c", "active": true},
	}}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("outputs = %#v, want %#v", out, want)
	}
}

// A bool-producing expression lands under value: as a Go bool.
func TestRunBoolProducesValue(t *testing.T) {
	out, err := evalCEL(context.Background(), "ctx.count > 1", map[string]any{"count": int64(2)})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"value": true}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("outputs = %#v, want %#v", out, want)
	}
}

// A compile error (bad syntax) is surfaced as a returned error, not a panic
// or a silent empty result.
func TestRunCompileErrorIsReturned(t *testing.T) {
	_, err := evalCEL(context.Background(), "ctx.a +", map[string]any{"a": int64(1)})
	if err == nil {
		t.Fatal("want a compile error, got nil")
	}
	if !strings.Contains(err.Error(), "compile") {
		t.Fatalf("error = %v, want it to mention compile", err)
	}
}

// A type error only detectable at eval time (ctx is dyn, so the compiler
// cannot reject this ahead of time) is also surfaced as a returned error.
func TestRunEvalTypeErrorIsReturned(t *testing.T) {
	_, err := evalCEL(context.Background(), "ctx.a + ctx.b",
		map[string]any{"a": "not-a-number", "b": int64(1)})
	if err == nil {
		t.Fatal("want an eval error, got nil")
	}
	if !strings.Contains(err.Error(), "eval") {
		t.Fatalf("error = %v, want it to mention eval", err)
	}
}

// No inputs at all (nil map) is a valid, empty ctx, not a nil-pointer panic.
func TestRunNilInputs(t *testing.T) {
	out, err := evalCEL(context.Background(), "1 + 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"value": int64(2)}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("outputs = %#v, want %#v", out, want)
	}
}
