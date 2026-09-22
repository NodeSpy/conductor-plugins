// Command conductor-starlark is the `use: starlark` STEP ENGINE as an
// external conductor plugin. Starlark (go.starlark.net) is Bazel's
// configuration language: a small, deterministic, Python-like dialect with no
// recursion, no arbitrary object model and — the reason it is here — no
// standard library face onto the filesystem, the network or the process.
// There is nothing to withhold: a bare *starlark.Thread with a hand-picked
// predeclared environment already has zero ambient authority.
//
// THE SANDBOX IS THE LANGUAGE. Starlark's spec has no `import os`, no `open`,
// no `exec`; the only names a script sees are what this engine predeclares.
// That predeclared set is `ctx` (the step's inputs) and `json` (encode/decode/
// indent, go.starlark.net/lib/json — pure data shaping, no I/O). Nothing
// nondeterministic is offered either: there is deliberately no `time` module,
// since a wall-clock read would make two runs of the same script disagree.
//
// The step's inputs (`req.Inputs`, the ctx document) are converted to a
// Starlark dict and predeclared as the global `ctx`; the script is run as a
// module with starlark.ExecFile, and the OUTPUT CONTRACT is its `output`
// global afterward, if it set one: a dict becomes the step's named outputs
// (enginekit.WrapValue), anything else lands under value:, and a script that
// never assigns `output` produces no outputs at all rather than an error.
// Unlike lua/risor's "last expression" or "return", Starlark's ExecFile has no
// return value from the top level — a global is the only channel a module has
// to hand anything back to its host.
//
// Timeout/cancel: a goroutine watches the run's context and calls
// thread.Cancel once it's Done, so a runaway `for` loop (Starlark has no
// built-in step limit here) is cut with "Starlark computation cancelled: …"
// instead of wedging the plugin.
//
// No egress, no fs, no spawns, no ctx.store/ctx.sql/ctx.memory: the empty
// manifest is the claim, and this engine has no host-callback surface at all
// — a hermetic script is the whole point of choosing Starlark over the other
// engines.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"fmt"
	"os"

	"go.starlark.net/lib/json"
	"go.starlark.net/starlark"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

func describe() plugin.Decl {
	return plugin.Decl{
		Kind:         plugin.KindStep,
		ABI:          plugin.EngineABI,
		Type:         "starlark",
		Desc:         "Starlark code steps (Bazel's deterministic, hermetic config language): ctx is the step inputs, the module's `output` global is the step's outputs.",
		Capabilities: plugin.Capabilities{},
	}
}

func run(ctx context.Context, req plugin.RunRequest, host *plugin.Host) (plugin.RunResult, error) {
	fmt.Fprintf(os.Stderr, "conductor-starlark: run instance=%s inputs=%d data-plane=%v\n",
		req.Instance, len(req.Inputs), host.Available())
	code, lerr := enginekit.LoadCode(req.Code)
	if lerr != nil {
		return plugin.RunResult{}, lerr
	}
	outputs, err := execStarlark(ctx, code, req.Inputs)
	if err != nil {
		return plugin.RunResult{}, err
	}
	return plugin.RunResult{Outputs: outputs}, nil
}

// execStarlark runs one script as a Starlark module: `ctx` (the step's
// inputs) and `json` are the whole predeclared environment, and the module's
// `output` global — if the script sets one — is read back afterward and run
// through the shared output contract.
func execStarlark(ctx context.Context, code string, data map[string]any) (map[string]any, error) {
	thread := &starlark.Thread{Name: "conductor-starlark"}

	// Bind the run ctx so the step's `timeout:` actually cuts a runaway
	// script: Starlark has no heap/step cap configured here, so ctx
	// cancellation reaching thread.Cancel is the only thing that stops a
	// `while True: pass`-style loop.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel(ctx.Err().Error())
		case <-done:
		}
	}()

	predeclared := starlark.StringDict{
		"ctx":  goToStarlark(data),
		"json": json.Module,
	}

	globals, err := starlark.ExecFile(thread, "step.star", code, predeclared)
	if err != nil {
		return nil, fmt.Errorf("starlark: %w", err)
	}

	output, ok := globals["output"]
	if !ok {
		return map[string]any{}, nil
	}
	v, err := starlarkToGo(output)
	if err != nil {
		return nil, fmt.Errorf("starlark: output: %w", err)
	}
	return enginekit.WrapValue(v), nil
}

// goToStarlark converts a Go value (the JSON-shaped step data) into a
// Starlark value: a map becomes a dict, a slice a list, and the JSON scalars
// map onto their natural Starlark counterparts.
func goToStarlark(v any) starlark.Value {
	switch x := v.(type) {
	case nil:
		return starlark.None
	case bool:
		return starlark.Bool(x)
	case string:
		return starlark.String(x)
	case int:
		return starlark.MakeInt(x)
	case int64:
		return starlark.MakeInt64(x)
	case float64:
		return starlark.Float(x)
	case map[string]any:
		d := starlark.NewDict(len(x))
		for k, e := range x {
			// A string key is always hashable, so SetKey cannot fail here.
			_ = d.SetKey(starlark.String(k), goToStarlark(e))
		}
		return d
	case []any:
		elems := make([]starlark.Value, len(x))
		for i, e := range x {
			elems[i] = goToStarlark(e)
		}
		return starlark.NewList(elems)
	case []string:
		elems := make([]starlark.Value, len(x))
		for i, e := range x {
			elems[i] = starlark.String(e)
		}
		return starlark.NewList(elems)
	}
	return starlark.String(fmt.Sprintf("%v", v))
}

// starlarkToGo converts a Starlark value back into JSON-shaped Go data: a
// dict becomes a map[string]any (non-string keys are stringified, mirroring
// how a mixed Lua table's numeric keys are kept), a list or tuple becomes a
// []any, and the scalar types map onto their natural Go counterparts.
func starlarkToGo(v starlark.Value) (any, error) {
	switch x := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(x), nil
	case starlark.String:
		return string(x), nil
	case starlark.Int:
		if i, ok := x.Int64(); ok {
			return i, nil
		}
		// Wider than int64 (a script that built a huge literal): keep it
		// exact by falling back to its decimal text rather than truncating.
		return x.BigInt().String(), nil
	case starlark.Float:
		return float64(x), nil
	case *starlark.List:
		out := make([]any, x.Len())
		for i := 0; i < x.Len(); i++ {
			e, err := starlarkToGo(x.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	case starlark.Tuple:
		out := make([]any, len(x))
		for i, e := range x {
			g, err := starlarkToGo(e)
			if err != nil {
				return nil, err
			}
			out[i] = g
		}
		return out, nil
	case *starlark.Dict:
		out := map[string]any{}
		for _, item := range x.Items() {
			k, v := item[0], item[1]
			g, err := starlarkToGo(v)
			if err != nil {
				return nil, err
			}
			if ks, ok := k.(starlark.String); ok {
				out[string(ks)] = g
			} else {
				out[k.String()] = g
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported starlark value %s (%s)", v, v.Type())
}

func main() {
	if err := plugin.Serve(plugin.EngineFunc(describe, run)); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-starlark: serve: %v\n", err)
		os.Exit(1)
	}
}
