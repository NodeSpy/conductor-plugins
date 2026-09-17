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
	if d.Type != "starlark" {
		t.Fatalf("Type = %q, want starlark", d.Type)
	}
	c := d.Capabilities
	if len(c.Egress) != 0 || len(c.Commands) != 0 || len(c.FS) != 0 || c.Spawns {
		t.Fatalf("a data-shaping engine declared capabilities: %+v", c)
	}
}

// The script runs through plugin.run, it sees the step's inputs as the `ctx`
// dict, and its `output` global becomes the step's named outputs.
func TestRunInputsRoundTripAndOutputs(t *testing.T) {
	req := plugin.RunRequest{
		Code: `
output = {"repo": ctx["repo"], "next": ctx["count"] + 1, "first": ctx["tags"][0]}
`,
		Inputs: map[string]any{"repo": "acme/app", "count": 2, "tags": []any{"ci", "urgent"}},
	}
	res, err := run(context.Background(), req, &plugin.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"repo": "acme/app", "next": int64(3), "first": "ci"}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %v, want %v", res.Outputs, want)
	}
}

// A dict `output` becomes the step's named outputs, a scalar or list lands
// under value:, and a script that never assigns `output` produces no outputs.
func TestOutputContract(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		want       map[string]any
	}{
		{"a scalar", `output = 7`, map[string]any{"value": int64(7)}},
		{"a list", `output = [1, 2]`, map[string]any{"value": []any{int64(1), int64(2)}}},
		{"a string", `output = "hi"`, map[string]any{"value": "hi"}},
		{"a dict", `output = {"a": 1, "b": "two"}`, map[string]any{"a": int64(1), "b": "two"}},
		{"no output", `x = 1`, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execStarlark(context.Background(), tc.code, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(out, tc.want) {
				t.Fatalf("outputs = %v, want %v", out, tc.want)
			}
		})
	}
}

// The predeclared json module (encode/decode) is available and round-trips
// through the same Starlark<->Go conversion as everything else.
func TestJSONModuleWorks(t *testing.T) {
	out, err := execStarlark(context.Background(), `
s = json.encode({"a": 1, "b": [1, 2, 3]})
output = {"decoded": json.decode(s)}
`, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"decoded": map[string]any{"a": int64(1), "b": []any{int64(1), int64(2), int64(3)}},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("outputs = %v, want %v", out, want)
	}
}

// A syntax error and a resolution/eval error both come back as an error, not
// a panic or a zero-value success.
func TestErrorsReturnedAsErrors(t *testing.T) {
	for _, tc := range []struct {
		name, code string
	}{
		{"syntax error", `def f(:`},
		{"undefined name", `output = this_name_does_not_exist`},
		{"runtime error", `output = 1 // 0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := execStarlark(context.Background(), tc.code, nil); err == nil {
				t.Fatal("err = nil, want an error")
			}
		})
	}
}

// A cancelled context reaches thread.Cancel, so a runaway loop is cut instead
// of wedging the plugin.
func TestCancelledContextCutsLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := execStarlark(ctx, `
def loop():
    i = 0
    for x in range(1, 100000000):
        i = i + 1
    return i

output = loop()
`, nil)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v, want a cancellation error", err)
	}
}
