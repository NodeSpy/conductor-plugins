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
	if d.Type != "go-embed" {
		t.Fatalf("Type = %q, want go-embed", d.Type)
	}
	c := d.Capabilities
	if len(c.Egress) != 0 || len(c.Commands) != 0 || len(c.FS) != 0 || c.Spawns {
		t.Fatalf("a data-shaping engine declared capabilities: %+v", c)
	}
}

// The snippet defines run(ctx map[string]any), it is actually called, it sees
// the step's inputs, and what it returns becomes the step's named outputs.
func TestRunInputsRoundTripAndOutputs(t *testing.T) {
	out, err := execGoEmbed(context.Background(), `
import "strings"

func run(ctx map[string]any) (any, error) {
	repo := ctx["repo"].(string)
	return map[string]any{
		"owner": strings.SplitN(repo, "/", 2)[0],
		"count": ctx["count"],
	}, nil
}
`, map[string]any{"repo": "acme/app", "count": 2}, &hosttest.Host{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"owner": "acme", "count": 2}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("outputs = %v, want %v", out, want)
	}
}

// conductor/store's ops reach conductor with the op and the positional args
// the snippet wrote, and the answer comes back into the snippet.
func TestCtxStoreRoutesToHost(t *testing.T) {
	h := &hosttest.Host{Reply: func(c hosttest.Call) (any, error) {
		if c.Op == "get" {
			return 41, nil
		}
		return nil, nil
	}}
	out, err := execGoEmbed(context.Background(), `
import "conductor/store"

func run(ctx map[string]any) (any, error) {
	st, err := store.Use("cache")
	if err != nil {
		return nil, err
	}
	if err := st.Set("run", "attempts", 7); err != nil {
		return nil, err
	}
	v, err := st.Get("run", "attempts")
	if err != nil {
		return nil, err
	}
	return map[string]any{"attempts": v}, nil
}
`, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	// 41 crosses the wire and comes back as JSON's number type.
	if want := (map[string]any{"attempts": float64(41)}); !reflect.DeepEqual(out, want) {
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

// conductor/sql and conductor/memory are the same road.
func TestCtxSQLAndMemoryRouteToHost(t *testing.T) {
	h := &hosttest.Host{Reply: func(c hosttest.Call) (any, error) {
		if c.Kind == "sql" {
			return []any{map[string]any{"n": 1}}, nil
		}
		return map[string]any{"id": "m1"}, nil
	}}
	if _, err := execGoEmbed(context.Background(), `
import (
	"conductor/sql"
	"conductor/memory"
)

func run(ctx map[string]any) (any, error) {
	db, err := sql.Use("analytics")
	if err != nil {
		return nil, err
	}
	if _, err := db.Query("SELECT n FROM t WHERE k = ?", []any{"k1"}); err != nil {
		return nil, err
	}
	if _, err := memory.Remember("a fact", []string{"tag"}, "repo:o/r"); err != nil {
		return nil, err
	}
	return nil, nil
}
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

// THE SANDBOX. Anything off goEmbedAllowlist is never registered, so an
// `import` of it cannot resolve — there is no other path to the process, the
// filesystem or the network, so this test is the boundary itself.
func TestAllowlistRejectsNonAllowedImports(t *testing.T) {
	for _, pkg := range []string{"os", "os/exec", "net/http", "net", "io", "reflect", "unsafe", "path/filepath"} {
		t.Run(pkg, func(t *testing.T) {
			_, err := execGoEmbed(context.Background(),
				"import \""+pkg+"\"\n\nfunc run(ctx map[string]any) (any, error) { return nil, nil }\n",
				nil, &hosttest.Host{})
			if err == nil {
				t.Fatalf("import %q was allowed — the sandbox has a hole", pkg)
			}
			if !strings.Contains(err.Error(), "code: go-embed:") {
				t.Fatalf("import %q failed with an unexpected error: %v", pkg, err)
			}
		})
	}
}

// …and everything ON the list still resolves, so the sandbox is narrow rather
// than empty.
func TestAllowlistAdmitsDataShapingPackages(t *testing.T) {
	for pkg := range goEmbedAllowlist {
		t.Run(pkg, func(t *testing.T) {
			_, err := execGoEmbed(context.Background(),
				"import _ \""+pkg+"\"\n\nfunc run(ctx map[string]any) (any, error) { return nil, nil }\n",
				nil, &hosttest.Host{})
			if err != nil {
				t.Fatalf("import %q is on the allowlist but did not resolve: %v", pkg, err)
			}
		})
	}
}

// "math/rand" being allowed must not also admit "math/rand/v2" — the
// last-slash split in goEmbedExports is what keeps those apart.
func TestAllowlistDoesNotLeakNestedVariants(t *testing.T) {
	ex := goEmbedExports()
	if _, ok := ex["math/rand/rand"]; !ok {
		t.Fatal("math/rand is on the allowlist but was not exported")
	}
	for key := range ex {
		if strings.HasPrefix(key, "math/rand/v2/") {
			t.Fatalf("math/rand/v2 leaked into the sandbox as %q", key)
		}
	}
}

// A snippet that gets the run() contract wrong is told what the contract is.
func TestRunContractIsChecked(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"no run at all", `func other(ctx map[string]any) (any, error) { return nil, nil }`},
		{"wrong parameter", `func run(s string) (any, error) { return nil, nil }`},
		{"wrong return arity", `func run(ctx map[string]any) (any, any, error) { return nil, nil, nil }`},
		{"second return is not error", `func run(ctx map[string]any) (any, string) { return nil, "" }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execGoEmbed(context.Background(), tc.code, nil, &hosttest.Host{})
			if err == nil || !strings.Contains(err.Error(), "must define `func run(ctx map[string]any)") {
				t.Fatalf("err = %v, want the contract message", err)
			}
		})
	}
}

// The one-return form is accepted too, and a non-object result is the step's
// single `value` output.
func TestOutputContract(t *testing.T) {
	out, err := execGoEmbed(context.Background(),
		`func run(ctx map[string]any) any { return 7 }`, nil, &hosttest.Host{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]any{"value": 7}); !reflect.DeepEqual(out, want) {
		t.Fatalf("outputs = %v, want %v", out, want)
	}
}

// An error the snippet returns fails the step.
func TestRunErrorFailsTheStep(t *testing.T) {
	_, err := execGoEmbed(context.Background(), `
import "errors"

func run(ctx map[string]any) (any, error) { return nil, errors.New("nope") }
`, nil, &hosttest.Host{})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want the snippet's error", err)
	}
}
