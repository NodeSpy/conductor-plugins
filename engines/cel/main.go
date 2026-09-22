// Command conductor-cel is the `use: cel` STEP ENGINE as an external
// conductor plugin: Google's Common Expression Language, on cel-go
// (cel.dev/cel-go — the module moved from github.com/google/cel-go),
// evaluating ONE expression against the step's inputs.
//
// Unlike this repo's other engines (lua, js, risor, go-embed), which run a
// script — statements, control flow, an explicit return — CEL's contract is
// narrower, and that narrowness is the point: a `code:` body is a single CEL
// EXPRESSION (`ctx.a + ctx.b`, `ctx.items.filter(x, x.active)`), evaluated
// against exactly one variable, `ctx`, bound to the step's inputs as a
// map(string, dyn). There is no statement separator, no assignment, no
// function definition, and no recursion CEL will let you write, because CEL
// is deliberately not Turing-complete: every well-typed expression it accepts
// is guaranteed to terminate on its own.
//
// THE SANDBOX IS THE LANGUAGE. CEL has no I/O primitive at all — no file,
// socket, process, or even print — so there is no library surface to strip
// the way the other engines strip Lua's os/io or JS's fs. ctx.store /
// ctx.sql / ctx.memory, wired into every other engine's ctx, have no
// equivalent here: an expression can only read the ctx it was given and
// produce a value: it cannot call back into conductor's data plane at all.
//
// The result becomes the step's outputs the same way every engine's does
// (enginekit.WrapValue): an expression that produces a map gets its keys as
// named outputs; anything else — a list, a number, a string, a bool — lands
// under value:.
//
// ctx cancellation still applies even though CEL cannot infinite-loop: eval
// runs through cel's ContextEval, so the step's own `timeout:` still cuts a
// pathologically expensive expression (say, a huge list comprehension)
// rather than letting it run to completion unbounded. A cel.CostLimit is set
// as a second, cheaper backstop against the same class of expression.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"fmt"
	"os"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

func describe() plugin.Decl {
	return plugin.Decl{
		Kind:         plugin.KindStep,
		ABI:          plugin.EngineABI,
		Type:         "cel",
		Desc:         "Common Expression Language steps on cel-go: ctx is the step inputs as a map(string, dyn), code: is one CEL expression, and its result is the step's outputs. Non-Turing-complete and I/O-free by design, so there is no ctx.store/ctx.sql/ctx.memory and nothing to sandbox beyond the language itself.",
		Capabilities: plugin.Capabilities{},
	}
}

func run(ctx context.Context, req plugin.RunRequest, host *plugin.Host) (plugin.RunResult, error) {
	fmt.Fprintf(os.Stderr, "conductor-cel: run instance=%s inputs=%d data-plane=%v\n",
		req.Instance, len(req.Inputs), host.Available())
	code, lerr := enginekit.LoadCode(req.Code)
	if lerr != nil {
		return plugin.RunResult{}, lerr
	}
	outputs, err := evalCEL(ctx, code, req.Inputs)
	if err != nil {
		return plugin.RunResult{}, err
	}
	return plugin.RunResult{Outputs: outputs}, nil
}

// costLimit backstops ContextEval's own ctx-cancellation cut with cel-go's
// own cost accounting, so a single pathological expression is bounded two
// ways rather than only by however promptly the runtime notices ctx.Done().
const costLimit = 1_000_000

// evalCEL compiles req.Code as one CEL expression, evaluates it with ctx
// bound to inputs as the expression's single map(string, dyn) variable, and
// converts the result into this repo's shared output contract
// (enginekit.WrapValue).
func evalCEL(ctx context.Context, code string, inputs map[string]any) (map[string]any, error) {
	env, err := cel.NewEnv(cel.Variable("ctx", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		return nil, fmt.Errorf("cel: new env: %w", err)
	}
	ast, iss := env.Compile(code)
	if iss.Err() != nil {
		return nil, fmt.Errorf("cel: compile: %w", iss.Err())
	}
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptOptimize), cel.CostLimit(costLimit))
	if err != nil {
		return nil, fmt.Errorf("cel: program: %w", err)
	}
	if inputs == nil {
		inputs = map[string]any{}
	}
	// ContextEval, not Eval: it checks ctx between evaluation steps, so the
	// step's own `timeout:` can still cut an expensive expression even though
	// CEL itself has no loop construct to run away with.
	out, _, err := prg.ContextEval(ctx, map[string]any{"ctx": inputs})
	if err != nil {
		return nil, fmt.Errorf("cel: eval: %w", err)
	}
	v, err := toGo(out)
	if err != nil {
		return nil, fmt.Errorf("cel: result: %w", err)
	}
	return enginekit.WrapValue(v), nil
}

// toGo converts one CEL result (ref.Val) into a plain, JSON-shaped Go value:
// nil, bool, int64, uint64, float64, string, []byte, []any or map[string]any.
//
// It walks CEL's own concrete value types and the two aggregate traits
// (traits.Lister, traits.Mapper) rather than going through ref.Val's
// ConvertToNative, because those two interfaces cover every list and map
// cel-go can hand back regardless of which adapter produced it — a
// Go-native slice or map bound in as ctx, a value built fresh by a macro
// like filter/map, or (in principle) a protobuf list/struct — whereas
// ConvertToNative's reflect.Type target has to already name the shape it
// expects, which the step's `code:` does not commit to ahead of time.
func toGo(v ref.Val) (any, error) {
	switch x := v.(type) {
	case *types.Err:
		return nil, x
	case types.Null:
		return nil, nil
	case types.String:
		return string(x), nil
	case types.Int:
		return int64(x), nil
	case types.Uint:
		return uint64(x), nil
	case types.Double:
		return float64(x), nil
	case types.Bool:
		return bool(x), nil
	case types.Bytes:
		return []byte(x), nil
	case traits.Mapper:
		return mapperToGo(x)
	case traits.Lister:
		return listerToGo(x)
	default:
		return nil, fmt.Errorf("unsupported result type %s (%T)", v.Type().TypeName(), v)
	}
}

// listerToGo walks a CEL list front-to-back via the Lister/Indexer/Sizer
// traits, converting each element in turn.
func listerToGo(l traits.Lister) (any, error) {
	sz, ok := l.Size().(types.Int)
	if !ok {
		return nil, fmt.Errorf("list size: unexpected type %T", l.Size())
	}
	out := make([]any, 0, int(sz))
	for i := types.Int(0); i < sz; i++ {
		ev, err := toGo(l.Get(i))
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// mapperToGo walks a CEL map via its Iterable/Iterator trait, converting
// both the key (stringified if it is not already a CEL string — CEL permits
// int/uint/bool/string map keys) and the value.
func mapperToGo(m traits.Mapper) (any, error) {
	out := map[string]any{}
	it := m.Iterator()
	for it.HasNext() == types.True {
		k := it.Next()
		val, found := m.Find(k)
		if !found {
			continue
		}
		gv, err := toGo(val)
		if err != nil {
			return nil, err
		}
		gk, err := toGo(k)
		if err != nil {
			return nil, err
		}
		ks, ok := gk.(string)
		if !ok {
			ks = fmt.Sprintf("%v", gk)
		}
		out[ks] = gv
	}
	return out, nil
}

func main() {
	if err := plugin.Serve(plugin.EngineFunc(describe, run)); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-cel: serve: %v\n", err)
		os.Exit(1)
	}
}
