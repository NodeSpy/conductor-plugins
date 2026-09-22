// Command conductor-jq is the `use: jq` STEP ENGINE as an external conductor
// plugin: gojq (a pure-Go jq implementation, no cgo, no libjq) driving a jq
// program over the step's inputs.
//
// The contract is data-in, data-out, nothing else: the jq INPUT document is
// req.Inputs (the step's rendered ctx, already JSON-shaped), req.Code is a jq
// program (".items | map(.name)", "{count: (.items|length)}", …), and the
// program's results become the step's outputs. The step's env: crosses as jq
// VARIABLES — each entry a $NAME bound to its string value, the same as `jq
// --arg NAME value` — so a program can be parameterized without templating the
// value into the program text (which would break on quoting); a value that
// should be a number or JSON is converted in-program with ($NAME | tonumber) /
// ($NAME | fromjson). There is no ctx.store,
// ctx.sql or ctx.memory face here — jq has no notion of a host call, and
// bolting one on would be a second surface for the same three ops with none
// of jq's own affordances (no way to *write* an object in jq syntax back to a
// binding). A jq step that needs the data plane reaches it through a
// neighboring step; this engine only reshapes what it is given.
//
// OUTPUT CONTRACT: a jq query is a generator — one input can yield zero, one,
// or many outputs. This engine collects every result the program produces
// and then applies enginekit's single-value rule to what it collected:
//
//   - exactly one result -> enginekit.WrapValue(that result) (an object
//     becomes named outputs, anything else becomes value:)
//   - more than one result -> enginekit.WrapValue(the results, as a list) —
//     they land under value: as a list, jq's own way of saying "many things
//     came out of one input"
//   - zero results (jq's `empty`, or a `select` that never matches) -> no
//     outputs at all ({}), same as a script that produced nothing
//
// SANDBOX: gojq's compiled program touches only the value it is handed. It
// has no filesystem, no network, and this engine never enables gojq's
// optional env/input/$ENV builtins — those would reach the plugin process's
// own environment or its stdin, which is the RPC transport, not step data.
// The declared Capabilities are empty: no egress, no fs, no commands, no
// spawns.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/itchyny/gojq"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

func describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindStep,
		ABI:  plugin.EngineABI,
		Type: "jq",
		Desc: "jq code steps on gojq (pure Go, no cgo): the step inputs are the jq " +
			"input document, and the query's results become the step's outputs — " +
			"one object result becomes named outputs, one scalar/array result " +
			"becomes value:, several results collect into a value: list, and no " +
			"results at all is no outputs. The step's env: crosses as $NAME jq " +
			"variables (string-valued, like `jq --arg`).",
		Capabilities: plugin.Capabilities{},
	}
}

func run(ctx context.Context, req plugin.RunRequest, host *plugin.Host) (plugin.RunResult, error) {
	fmt.Fprintf(os.Stderr, "conductor-jq: run instance=%s inputs=%d vars=%d\n", req.Instance, len(req.Inputs), len(req.Env))
	code, lerr := enginekit.LoadCode(req.Code)
	if lerr != nil {
		return plugin.RunResult{}, lerr
	}
	outputs, err := execJQ(ctx, code, req.Inputs, req.Env)
	if err != nil {
		return plugin.RunResult{}, err
	}
	return plugin.RunResult{Outputs: outputs}, nil
}

// execJQ parses and runs one jq program against the step's inputs, and maps
// the program's result stream onto the step's output contract.
//
// Parsing and compiling happen up front so a bad program (a syntax error, or
// a reference to a function gojq doesn't have) is reported once, clearly, as
// a plugin error rather than surfacing mid-stream. Compile lets gojq
// pre-resolve the query into bytecode; Run would re-walk the AST on every
// call, which matters for nothing here (one call per step) but costs
// nothing either, and RunWithContext is the only variant that honors ctx, so
// this is the one call this engine makes.
func execJQ(ctx context.Context, code string, inputs map[string]any, env map[string]string) (map[string]any, error) {
	// The step's env: becomes jq variables — each entry a $NAME bound to its
	// string value, exactly like `jq --arg NAME value`. enginekit.StepVars
	// validates and orders the names so gojq's positional (variables, values)
	// pairing is deterministic; a program that references a $NAME the step
	// never set is gojq's own "variable not defined" compile error, not a
	// silent nil.
	names, values, err := enginekit.StepVars(env)
	if err != nil {
		return nil, fmt.Errorf("jq: %w", err)
	}

	query, err := gojq.Parse(code)
	if err != nil {
		return nil, fmt.Errorf("jq: parse: %w", err)
	}
	var opts []gojq.CompilerOption
	if len(names) > 0 {
		vars := make([]string, len(names))
		for i, n := range names {
			vars[i] = "$" + n
		}
		opts = append(opts, gojq.WithVariables(vars))
	}
	program, err := gojq.Compile(query, opts...)
	if err != nil {
		return nil, fmt.Errorf("jq: compile: %w", err)
	}

	// inputs is already the JSON-shaped map[string]any conductor renders for
	// every engine's ctx; gojq accepts it as-is (its own decoder produces the
	// same map[string]any/[]any/string/bool/float64/nil/int shapes). A nil
	// map (a step with no inputs) is still a valid, empty jq input document.
	var input any = inputs
	if inputs == nil {
		input = map[string]any{}
	}

	// RunWithContext checks ctx as it evaluates, so a runaway program (an
	// infinite `repeat`, say) is cut by the step's own timeout instead of
	// wedging the plugin — the same guarantee execLua gets from L.SetContext.
	// The variable values follow input positionally, in the same order their
	// names were handed to WithVariables above.
	vals := make([]any, len(values))
	for i, v := range values {
		vals[i] = v
	}
	iter := program.RunWithContext(ctx, input, vals...)
	var results []any
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		// gojq reports a runtime failure (a type mismatch, `error(...)`, an
		// out-of-range index, …) as a value satisfying error rather than as a
		// second return from Next — the iterator has nothing else to hand
		// back, so the failure comes down the same channel as a result.
		if e, ok := v.(error); ok {
			return nil, fmt.Errorf("jq: %w", e)
		}
		results = append(results, v)
	}

	switch len(results) {
	case 0:
		// jq's `empty` (or a select that never matches): no outputs, the
		// same shape a script that never returns produces.
		return map[string]any{}, nil
	case 1:
		return enginekit.WrapValue(results[0]), nil
	default:
		return enginekit.WrapValue(results), nil
	}
}

func main() {
	if err := plugin.Serve(plugin.EngineFunc(describe, run)); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-jq: serve: %v\n", err)
		os.Exit(1)
	}
}
