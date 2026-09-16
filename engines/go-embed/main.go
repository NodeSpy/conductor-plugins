// Command conductor-go-embed is the `use: go-embed` STEP ENGINE as an
// external conductor plugin: the in-binary yaegi engine (conductor
// internal/code/goembed.go, execGoEmbed) lifted out of the daemon and put
// behind the plugin wire.
//
// It interprets a large subset of real Go — the "you need actual control flow
// and types, but installing the Go toolchain on every conductor host is a
// bridge too far" engine. The snippet's contract is unchanged:
//
//	func run(ctx map[string]any) (any, error)
//	func run(ctx map[string]any) any
//
// checked by reflection after eval rather than assumed, so a wrong signature
// fails with a message naming the two accepted shapes instead of a reflect
// panic mid-call. The returned value goes through the shared output contract:
// a map becomes the step's named outputs, anything else lands under value:.
//
// THE SANDBOX IS THE ALLOWLIST, and it is carried here byte for byte
// (goEmbedAllowlist below, identical to internal/code/goembed.go). yaegi has
// no notion of a forbidden package — it can only resolve packages it has been
// given source or Use()-registered binary symbols for — so anything off the
// list, including "os", "os/exec", "net", "net/http", "io", "reflect" and
// "unsafe", is simply never registered and an `import` of it fails with
// yaegi's ordinary "unable to find source related to" error. GoPath is
// likewise pinned to a path that never exists, so a source lookup can never
// pull host files in. That is the entire boundary; there is no second path.
//
// The three ctx faces keep their in-binary spelling — `import
// "conductor/store"` / `"conductor/sql"` / `"conductor/memory"`, with the same
// KVHandle / SQLHandle / MemHandle method sets — so a snippet written for
// `run: go-embed` compiles here unchanged. What changed is underneath: each
// method used to call the daemon's kvInvoke directly and now makes one host.*
// round trip, which conductor authorizes against the step's own guard.
//
// No egress, no fs, no spawns: the empty manifest is the claim.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

// goEmbedAllowlist is the set of stdlib import paths a `use: go-embed`
// snippet may `import`. Copied verbatim from internal/code/goembed.go —
// deliberately narrow: general-purpose data-shaping packages (string/byte/JSON
// handling, time, math, sorting, regex, basic URL parsing) that a
// template-adjacent code step plausibly needs, and nothing that touches the
// outside world.
var goEmbedAllowlist = map[string]bool{
	"bytes":           true,
	"encoding/json":   true,
	"encoding/base64": true,
	"errors":          true,
	"fmt":             true,
	"math":            true,
	"math/rand":       true,
	"net/url":         true,
	"path":            true,
	"regexp":          true,
	"sort":            true,
	"strconv":         true,
	"strings":         true,
	"time":            true,
	"unicode":         true,
	"unicode/utf8":    true,
}

// goEmbedExports builds the yaegi Exports (a filtered copy of stdlib.Symbols)
// restricted to goEmbedAllowlist. stdlib.Symbols keys are "<import
// path>/<package name>" (e.g. "encoding/json/json", "math/rand/v2/rand" for
// the v2 variant) — splitting on the *last* slash recovers the import path
// even for the nested v2 packages, so "math/rand" being allowed doesn't
// accidentally also let "math/rand/v2" through (that key's import path is
// "math/rand/v2", which fails the exact-match lookup below).
func goEmbedExports() interp.Exports {
	out := interp.Exports{}
	for key, syms := range stdlib.Symbols {
		i := strings.LastIndex(key, "/")
		if i < 0 {
			continue
		}
		if goEmbedAllowlist[key[:i]] {
			out[key] = syms
		}
	}
	return out
}

// errorType is reflect's handle on the `error` interface, used to check a
// run() function's second return value without constructing one.
var errorType = reflect.TypeOf((*error)(nil)).Elem()

// ctxMapType is the exact parameter type run() must accept.
var ctxMapType = reflect.TypeOf(map[string]any{})

// anyType is the exact (non-error) return type run() must produce.
var anyType = reflect.TypeOf((*any)(nil)).Elem()

func describe() plugin.Decl {
	return plugin.Decl{
		Kind:         plugin.KindStep,
		ABI:          plugin.EngineABI,
		Type:         "go-embed",
		Desc:         "Go code steps on yaegi, sandboxed to a data-shaping stdlib allowlist: the snippet defines func run(ctx map[string]any) (any, error), with conductor/store, conductor/sql and conductor/memory served by conductor.",
		Capabilities: plugin.Capabilities{},
	}
}

func run(ctx context.Context, req plugin.RunRequest, host *plugin.Host) (plugin.RunResult, error) {
	fmt.Fprintf(os.Stderr, "conductor-go-embed: run instance=%s inputs=%d data-plane=%v\n",
		req.Instance, len(req.Inputs), host.Available())
	outputs, err := execGoEmbed(ctx, req.Code, req.Inputs, host)
	if err != nil {
		return plugin.RunResult{}, err
	}
	return plugin.RunResult{Outputs: outputs}, nil
}

// execGoEmbed is internal/code/goembed.go's execGoEmbed with the guard
// replaced by the host client: the pinned GoPath, the four Use() registrations,
// the __step harness (so run() is invoked INSIDE EvalWithContext, which is the
// only thing yaegi's cancellation can cut), the mutex around the captured
// result and the signature check are the original.
func execGoEmbed(ctx context.Context, code string, data map[string]any, host enginekit.Host) (map[string]any, error) {
	// GoPath is pinned to a path that never exists: yaegi would otherwise
	// consult the ambient GOPATH and interpret source packages found under
	// it — `import "anything/on/disk"` pulling host files into the sandbox.
	// The allowlist above is Use()-registered binary symbols only; source
	// lookups must always fail.
	i := interp.New(interp.Options{GoPath: "/nonexistent-conductor-goembed"})
	if err := i.Use(goEmbedExports()); err != nil {
		return nil, fmt.Errorf("code: go-embed: sandbox setup: %w", err)
	}
	if err := i.Use(kvGoEmbedExports(ctx, host)); err != nil {
		return nil, fmt.Errorf("code: go-embed: kv setup: %w", err)
	}
	if err := i.Use(sqlGoEmbedExports(ctx, host)); err != nil {
		return nil, fmt.Errorf("code: go-embed: sql setup: %w", err)
	}
	if err := i.Use(memGoEmbedExports(ctx, host)); err != nil {
		return nil, fmt.Errorf("code: go-embed: memory setup: %w", err)
	}

	// call bridges the step data in and the result out of the interpreter,
	// so the run() invocation itself happens INSIDE EvalWithContext below —
	// yaegi's cancellation (a runid bump checked on every exec-loop
	// iteration) is the only thing that can cut an interpreted infinite
	// loop, and it only fires for code evaluated with a context. The mutex
	// guards the captured result against a write racing a timeout return.
	var (
		mu      sync.Mutex
		res     any
		callErr error
	)
	if err := i.Use(interp.Exports{
		"conductor/__step/__step": {
			"Ctx": reflect.ValueOf(func() map[string]any { return data }),
			"Return": reflect.ValueOf(func(v any, err error) {
				mu.Lock()
				defer mu.Unlock()
				res, callErr = v, err
			}),
		},
	}); err != nil {
		return nil, fmt.Errorf("code: go-embed: harness setup: %w", err)
	}

	// User code is evaluated with the step ctx too: top-level declarations
	// can run arbitrary code (global initializers), not just declare.
	if _, err := i.EvalWithContext(ctx, code); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("code: go-embed: %w", ctx.Err())
		}
		return nil, fmt.Errorf("code: go-embed: %w", err)
	}

	runFn, err := i.Eval("run")
	if err != nil {
		return nil, fmt.Errorf("code: go-embed: %s: %w", goEmbedContractMsg, err)
	}

	if err := checkGoEmbedSignature(runFn); err != nil {
		return nil, err
	}

	harness := `
import __step "conductor/__step"

func __conductor_call() { __step.Return(run(__step.Ctx())) }
`
	if runFn.Type().NumOut() == 1 {
		harness = `
import __step "conductor/__step"

func __conductor_call() { __step.Return(run(__step.Ctx()), nil) }
`
	}
	if _, err := i.Eval(harness); err != nil {
		return nil, fmt.Errorf("code: go-embed: harness: %w", err)
	}
	if _, err := i.EvalWithContext(ctx, "__conductor_call()"); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("code: go-embed: %w", ctx.Err())
		}
		return nil, fmt.Errorf("code: go-embed: %w", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if callErr != nil {
		return nil, fmt.Errorf("code: go-embed: run: %w", callErr)
	}
	return enginekit.WrapValue(res), nil
}

const goEmbedContractMsg = "must define `func run(ctx map[string]any) (any, error)` or `func run(ctx map[string]any) any`"

// checkGoEmbedSignature validates runFn against the two accepted `run` shapes
// before it's ever called, so a mismatch surfaces as a clear contract error
// rather than a reflect panic (wrong argument count/type) or a
// silently-ignored second return value. Verbatim from internal/code.
func checkGoEmbedSignature(runFn reflect.Value) error {
	if runFn.Kind() != reflect.Func {
		return fmt.Errorf("code: go-embed: run: %s, got %s", goEmbedContractMsg, runFn.Kind())
	}
	ft := runFn.Type()
	if ft.NumIn() != 1 || ft.In(0) != ctxMapType {
		return fmt.Errorf("code: go-embed: run: %s, got %s", goEmbedContractMsg, ft)
	}
	switch ft.NumOut() {
	case 1:
		if ft.Out(0) != anyType {
			return fmt.Errorf("code: go-embed: run: %s, got %s", goEmbedContractMsg, ft)
		}
	case 2:
		if ft.Out(0) != anyType || !ft.Out(1).Implements(errorType) {
			return fmt.Errorf("code: go-embed: run: %s, got %s", goEmbedContractMsg, ft)
		}
	default:
		return fmt.Errorf("code: go-embed: run: %s, got %s", goEmbedContractMsg, ft)
	}
	return nil
}

func main() {
	if err := plugin.Serve(plugin.EngineFunc(describe, run)); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-go-embed: serve: %v\n", err)
		os.Exit(1)
	}
}
