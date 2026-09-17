package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

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
	if d.Type != "yq" {
		t.Fatalf("Type = %q, want yq", d.Type)
	}
	c := d.Capabilities
	if len(c.Egress) != 0 || len(c.Commands) != 0 || len(c.FS) != 0 || c.Spawns {
		t.Fatalf("a data-shaping engine declared capabilities: %+v", c)
	}
}

// A mapping result becomes named outputs (not wrapped under value:), and the
// yaml: output carries the same document as rendered text.
func TestMappingResultIsNamedOutputsPlusYAML(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   ".",
		Inputs: map[string]any{"a": "hi"},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Outputs["a"]; got != "hi" {
		t.Fatalf("outputs[a] = %#v, want %q", got, "hi")
	}
	rendered, ok := res.Outputs["yaml"].(string)
	if !ok {
		t.Fatalf("outputs[yaml] = %#v, want a string", res.Outputs["yaml"])
	}
	var roundTrip map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &roundTrip); err != nil {
		t.Fatalf("outputs[yaml] did not parse as YAML: %v\n%s", err, rendered)
	}
	if !reflect.DeepEqual(roundTrip, map[string]any{"a": "hi"}) {
		t.Fatalf("round-tripped yaml = %#v, want %#v", roundTrip, map[string]any{"a": "hi"})
	}
}

// A query that yields exactly one SCALAR result lands under value:, and the
// same scalar (rendered as YAML) is present in the yaml: output.
func TestScalarResultIsValue(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   ".a",
		Inputs: map[string]any{"a": 42},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"value": 42, "yaml": "42\n"}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// An assignment round-trips through the mapping result, and the yaml: output
// reflects the write, not the original value.
func TestAssignmentRoundTripsIntoYAML(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   `.a.b = "x"`,
		Inputs: map[string]any{"a": map[string]any{"b": "orig"}},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": map[string]any{"b": "x"}}
	got := map[string]any{}
	for k, v := range res.Outputs {
		if k != "yaml" {
			got[k] = v
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outputs (minus yaml:) = %#v, want %#v", got, want)
	}
	rendered, _ := res.Outputs["yaml"].(string)
	if !strings.Contains(rendered, "b: x") {
		t.Fatalf("outputs[yaml] = %q, want it to contain the write (b: x)", rendered)
	}
	if strings.Contains(rendered, "orig") {
		t.Fatalf("outputs[yaml] = %q, still holds the pre-assignment value", rendered)
	}
}

// head_comment attaches a comment inside yq's own evaluation, before the
// result is rendered back to text — this is yq's edge over jq: the rendered
// yaml: output preserves it, something no round-trip through Go values
// (map[string]any has no notion of a comment) could reproduce.
func TestYAMLOutputPreservesComments(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   `.a = "x" | .a head_comment="a note"`,
		Inputs: map[string]any{"a": "orig"},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	rendered, ok := res.Outputs["yaml"].(string)
	if !ok {
		t.Fatalf("outputs[yaml] = %#v, want a string", res.Outputs["yaml"])
	}
	if !strings.Contains(rendered, "# a note") {
		t.Fatalf("outputs[yaml] = %q, want it to contain the head_comment", rendered)
	}
	// And it still has to be valid YAML once the comment is stripped by any
	// ordinary parser.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("outputs[yaml] did not parse as YAML: %v\n%s", err, rendered)
	}
	if doc["a"] != "x" {
		t.Fatalf("doc[a] = %#v, want %q", doc["a"], "x")
	}
}

// splitDocuments explodes one input document into a "---"-joined stream — a
// yq-native way of saying "many things came out of one input", collected
// into a value: list the same way jq's own multi-result queries are.
func TestMultipleDocumentsCollectIntoValueList(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   ".items[]",
		Inputs: map[string]any{"items": []any{"a", "b", "c"}},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"value": []any{"a", "b", "c"}}
	got := map[string]any{}
	for k, v := range res.Outputs {
		if k != "yaml" {
			got[k] = v
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outputs (minus yaml:) = %#v, want %#v", got, want)
	}
	// yq's own printer only inserts a "---" separator between results that
	// trace back to distinct INPUT documents (see the renderYAML comment in
	// main.go); several results exploded out of the SAME input document
	// print back to back with none at all — this is genuine `yq` CLI
	// behavior, not a gap in this engine, which is exactly why the Go-value
	// side of the contract (asserted above) counts results off the matched
	// node list rather than off "---" splits in this text.
	rendered, _ := res.Outputs["yaml"].(string)
	if rendered != "a\nb\nc\n" {
		t.Fatalf("outputs[yaml] = %q, want %q", rendered, "a\nb\nc\n")
	}
}

// A yq expression that fails to parse is a plugin error, not a panic or a
// silently empty result.
func TestParseErrorSurfaces(t *testing.T) {
	_, err := run(context.Background(), plugin.RunRequest{
		Code:   ".a | | |",
		Inputs: map[string]any{"a": 1},
	}, &plugin.Host{})
	if err == nil {
		t.Fatal("want a parse error, got nil")
	}
	if !strings.Contains(err.Error(), "yq:") {
		t.Fatalf("err = %v, want it wrapped with a yq: prefix", err)
	}
}

// select(false) (yq borrows jq's select) yields nothing at all: no outputs,
// not an error, and no yaml: key either — nothing rendered, nothing to keep.
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

// A no-inputs step is still a valid (empty) YAML input document: identity
// over it is ONE result (an empty mapping), so the yaml: output is present
// (there was something to render) even though it carries no named outputs
// of its own.
func TestNilInputsIsEmptyMapping(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code: ".",
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"yaml": "{}\n"}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// select(false) against a genuinely absent input yields nothing at all: no
// outputs, and no yaml: key either — nothing rendered, nothing to keep.
func TestNilInputsWithNoMatchIsNoOutputs(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code: "select(false)",
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v, want %#v", res.Outputs, want)
	}
}

// env(...) is one of the two yq operators that reach outside the value it
// was handed; this engine disables it in init() (see the SANDBOX note in
// main.go), so a step's code calling it gets yq's own refusal, not a live
// read of the plugin process's environment.
func TestEnvOperatorIsDisabled(t *testing.T) {
	t.Setenv("CONDUCTOR_YQ_TEST_PROBE", "leaked")
	_, err := run(context.Background(), plugin.RunRequest{
		Code:   `env(CONDUCTOR_YQ_TEST_PROBE)`,
		Inputs: map[string]any{},
	}, &plugin.Host{})
	if err == nil {
		t.Fatal("want env() to be refused, got nil error")
	}
}

// load(...) is the other reach-outside operator (arbitrary file reads);
// disabled the same way as env().
func TestLoadOperatorIsDisabled(t *testing.T) {
	_, err := run(context.Background(), plugin.RunRequest{
		Code:   `load("/etc/hostname")`,
		Inputs: map[string]any{},
	}, &plugin.Host{})
	if err == nil {
		t.Fatal("want load() to be refused, got nil error")
	}
}

// The step's env: crosses as $NAME yq variables, string-valued like `jq
// --arg`. Here a variable supplies the value assigned into the document, and
// the mapping result (whole doc) comes back as named outputs plus yaml text.
func TestVariablesFromEnv(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   `.name = $LABEL`,
		Inputs: map[string]any{"name": "old"},
		Env:    map[string]string{"LABEL": "prod"},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Outputs["name"]; got != "prod" {
		t.Fatalf("outputs[name] = %#v, want %q", got, "prod")
	}
	if rendered, _ := res.Outputs["yaml"].(string); rendered != "name: prod\n" {
		t.Fatalf("outputs[yaml] = %q, want %q", rendered, "name: prod\n")
	}
}

// A string variable converts to a number in-expression with to_number — the
// operator the docs point at for numeric parameters. Locks that spelling.
func TestVariableToNumber(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   `.replicas = ($COUNT | to_number)`,
		Inputs: map[string]any{"replicas": 1},
		Env:    map[string]string{"COUNT": "3"},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	if rendered, _ := res.Outputs["yaml"].(string); rendered != "replicas: 3\n" {
		t.Fatalf("outputs[yaml] = %q, want %q", rendered, "replicas: 3\n")
	}
}

// A variable drives a select — the expression is parameterized by the env
// value without splicing it into the expression text.
func TestVariableInSelect(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   `select(.env == $WANT)`,
		Inputs: map[string]any{"env": "prod", "app": "web"},
		Env:    map[string]string{"WANT": "prod"},
	}, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Outputs["app"]; got != "web" {
		t.Fatalf("outputs[app] = %#v, want %q (the doc should have matched)", got, "web")
	}
}

// yq is lenient where jq is strict: a $NAME the step never set resolves to no
// match rather than an error, so the step simply produces no outputs. (This is
// the one variable-behavior difference between the two engines.)
func TestUndefinedVariableIsEmptyNotError(t *testing.T) {
	res, err := run(context.Background(), plugin.RunRequest{
		Code:   `$MISSING`,
		Inputs: map[string]any{},
	}, &plugin.Host{})
	if err != nil {
		t.Fatalf("want no error for an undefined yq variable, got %v", err)
	}
	if len(res.Outputs) != 0 {
		t.Fatalf("outputs = %#v, want none", res.Outputs)
	}
}

// An env: key that is not a legal variable identifier is rejected up front,
// the same rule the jq engine enforces.
func TestInvalidVariableNameRejected(t *testing.T) {
	_, err := run(context.Background(), plugin.RunRequest{
		Code:   `.`,
		Inputs: map[string]any{},
		Env:    map[string]string{"bad-name": "x"},
	}, &plugin.Host{})
	if err == nil {
		t.Fatal("want an error for an invalid variable name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid variable name") {
		t.Fatalf("err = %v, want an invalid-variable-name error", err)
	}
}
