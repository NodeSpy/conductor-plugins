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
	if d.Type != "jq" {
		t.Fatalf("Type = %q, want jq", d.Type)
	}
	c := d.Capabilities
	if len(c.Egress) != 0 || len(c.Commands) != 0 || len(c.FS) != 0 || c.Spawns {
		t.Fatalf("a data-shaping engine declared capabilities: %+v", c)
	}
}

// A query that yields exactly one scalar result lands under value:.
func TestScalarResultIsValue(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   ".a + .b",
		Inputs: map[string]any{"a": 1, "b": 2},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"value": 3}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// A query that yields exactly one OBJECT result becomes named outputs —
// its own keys, not wrapped under value:.
func TestObjectResultIsNamedOutputs(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   "{x: .a}",
		Inputs: map[string]any{"a": "hi"},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"x": "hi"}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// A query that yields MULTIPLE results (here, .items[] exploding an array
// into a stream) collects them into a value: list rather than picking one.
func TestMultipleResultsCollectIntoValueList(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   ".items[]",
		Inputs: map[string]any{"items": []any{"a", "b", "c"}},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"value": []any{"a", "b", "c"}}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// length is an ordinary one-result query.
func TestLength(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   ".items | length",
		Inputs: map[string]any{"items": []any{"a", "b", "c"}},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"value": 3}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// A query that yields nothing at all (a select that never matches) is no
// outputs, not an error and not a null value:.
func TestEmptyResultIsNoOutputs(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   ".items[] | select(false)",
		Inputs: map[string]any{"items": []any{"a", "b", "c"}},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// A jq program that fails to parse is a plugin error, not a panic or a
// silently empty result.
func TestParseErrorSurfaces(t *testing.T) {
	_, err := run(context.Background(), plugin.RunRequest{
		Code:   ".a | | |",
		Inputs: map[string]any{"a": 1},
	}, &plugin.Host{})
	if err == nil {
		t.Fatal("want a parse error, got nil")
	}
}

// A jq program that parses fine but fails at RUNTIME (adding a number and a
// string) surfaces as an error too, not a partial or nil result.
func TestRuntimeErrorSurfaces(t *testing.T) {
	_, err := run(context.Background(), plugin.RunRequest{
		Code:   ".a + \"x\"",
		Inputs: map[string]any{"a": 1},
	}, &plugin.Host{})
	if err == nil {
		t.Fatal("want a runtime type error, got nil")
	}
	if !strings.Contains(err.Error(), "jq:") {
		t.Fatalf("err = %v, want it wrapped with a jq: prefix", err)
	}
}

// The step's env: crosses as $NAME jq variables, string-valued like `jq
// --arg`. Here two variables parameterize the program: a numeric compare
// (via tonumber) and a field the object output is keyed on.
func TestVariablesFromEnv(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   `{region: $REGION, over: (.usage > ($THRESHOLD | tonumber))}`,
		Inputs: map[string]any{"usage": 95},
		Env:    map[string]string{"REGION": "us-east", "THRESHOLD": "90"},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"region": "us-east", "over": true}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// A variable value is always the string, never a guessed type — "90" is the
// string "90" until the program converts it (here with tonumber).
func TestVariableIsStringUntilConverted(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   `{raw: $V, num: ($V | tonumber)}`,
		Inputs: map[string]any{},
		Env:    map[string]string{"V": "90"},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"raw": "90", "num": 90}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// Referencing a $NAME the step never set is gojq's own compile error, not a
// silent nil — a program's typo fails loudly.
func TestUndefinedVariableIsAnError(t *testing.T) {
	_, err := run(context.Background(), plugin.RunRequest{
		Code:   "$MISSING",
		Inputs: map[string]any{},
	}, &plugin.Host{})
	if err == nil {
		t.Fatal("want a compile error for an undefined variable, got nil")
	}
}

// An env: key that is not a legal variable identifier is rejected up front
// with a clear error rather than producing a broken program.
func TestInvalidVariableNameRejected(t *testing.T) {
	_, err := run(context.Background(), plugin.RunRequest{
		Code:   ".",
		Inputs: map[string]any{},
		Env:    map[string]string{"not-an-ident": "x"},
	}, &plugin.Host{})
	if err == nil {
		t.Fatal("want an error for an invalid variable name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid variable name") {
		t.Fatalf("err = %v, want an invalid-variable-name error", err)
	}
}
