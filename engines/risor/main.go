// Command conductor-risor is the `use: risor` STEP ENGINE as an external
// conductor plugin: the in-binary risor engine (conductor
// internal/code/risor.go, execRisor) lifted out of the daemon and put behind
// the plugin wire.
//
// risor is a pure-Go embeddable scripting language with Go-flavored syntax and
// no cgo. The snippet contract is unchanged: the step's inputs are the `ctx`
// global, and the script's FINAL EXPRESSION is its result — a risor map
// becomes the step's named outputs, anything else lands under value:.
//
// THE SANDBOX IS THE GLOBAL SET (risorGlobals below, the original's list).
// risor's default globals include os, exec, http, dns, net and filepath, so
// the engine opts out of the defaults entirely (WithoutDefaultGlobals) and
// grants an explicit allowlist: the core builtins (len, keys, sprintf, …) plus
// the data-shaping modules, and nothing that leaves the process.
//
// The three ctx faces keep their in-binary spelling — the top-level
// store("cache") and sql("analytics") builtins and the memory module — so a
// script written for `run: risor` runs here unchanged. Each op is now one
// host.* round trip that conductor authorizes against the step's own guard.
//
// No egress, no fs, no spawns: the empty manifest is the claim.
//
// The module identity is github.com/risor-io/risor: the repository moved to
// github.com/deepnoodle-ai/risor, but every released tag still declares the
// original module path, so that is the importable name.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"fmt"
	"os"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/risor-io/risor"
	"github.com/risor-io/risor/builtins"
	modBase64 "github.com/risor-io/risor/modules/base64"
	modBytes "github.com/risor-io/risor/modules/bytes"
	modErrors "github.com/risor-io/risor/modules/errors"
	modJSON "github.com/risor-io/risor/modules/json"
	modMath "github.com/risor-io/risor/modules/math"
	modRegexp "github.com/risor-io/risor/modules/regexp"
	modStrconv "github.com/risor-io/risor/modules/strconv"
	modStrings "github.com/risor-io/risor/modules/strings"
	modTime "github.com/risor-io/risor/modules/time"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

func describe() plugin.Decl {
	return plugin.Decl{
		Kind:         plugin.KindStep,
		ABI:          plugin.EngineABI,
		Type:         "risor",
		Desc:         "risor code steps (pure-Go scripting, Go-flavored syntax): ctx is the step inputs, the final expression is the step's outputs, with store()/sql()/memory served by conductor.",
		Capabilities: plugin.Capabilities{},
	}
}

func run(ctx context.Context, req plugin.RunRequest, host *plugin.Host) (plugin.RunResult, error) {
	fmt.Fprintf(os.Stderr, "conductor-risor: run instance=%s inputs=%d data-plane=%v\n",
		req.Instance, len(req.Inputs), host.Available())
	outputs, err := execRisor(ctx, req.Code, req.Inputs, host)
	if err != nil {
		return plugin.RunResult{}, err
	}
	return plugin.RunResult{Outputs: outputs}, nil
}

// execRisor is internal/code/risor.go's execRisor with the guard replaced by
// the host client. The eval options, the nil-result case and the output
// contract are the original's.
func execRisor(ctx context.Context, code string, data map[string]any, host enginekit.Host) (map[string]any, error) {
	result, err := risor.Eval(ctx, code,
		risor.WithoutDefaultGlobals(),
		risor.WithGlobals(risorGlobals(ctx, host, data)))
	if err != nil {
		return nil, fmt.Errorf("risor: %w", err)
	}
	if result == nil {
		return map[string]any{}, nil
	}
	return enginekit.WrapValue(result.Interface()), nil
}

// risorGlobals is the sandbox: risor's core builtins plus the data-shaping
// modules, and NOTHING that leaves the process — no os, exec, http, dns, net,
// or filepath. Verbatim from internal/code/risor.go apart from where the three
// data faces resolve to.
func risorGlobals(ctx context.Context, host enginekit.Host, data map[string]any) map[string]any {
	globals := map[string]any{}
	for k, v := range builtins.Builtins() {
		globals[k] = v
	}
	globals["base64"] = modBase64.Module()
	globals["bytes"] = modBytes.Module()
	globals["errors"] = modErrors.Module()
	globals["json"] = modJSON.Module()
	globals["math"] = modMath.Module()
	globals["regexp"] = modRegexp.Module()
	globals["strconv"] = modStrconv.Module()
	globals["strings"] = modStrings.Module()
	globals["time"] = modTime.Module()
	globals["store"] = kvRisorStoreFn(ctx, host) // defined stores: s := store("cache"); s.get(…)
	globals["sql"] = sqlRisorFn(ctx, host)       // defined SQL stores: db := sql("analytics"); db.query(…)
	globals["memory"] = memRisorFn(ctx, host)    // shared agent memory: memory.remember(…), memory.recall(…)
	globals["ctx"] = data
	return globals
}

func main() {
	if err := plugin.Serve(plugin.EngineFunc(describe, run)); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-risor: serve: %v\n", err)
		os.Exit(1)
	}
}
