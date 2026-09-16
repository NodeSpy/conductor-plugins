// Command conductor-js is the `use: js` STEP ENGINE as an external conductor
// plugin: the in-binary QuickJS engine (conductor internal/code/js.go,
// execJS) lifted out of the daemon and put behind the plugin wire, so the
// daemon ships without an embedded JavaScript VM and an operator who wants one
// fetches it.
//
// Same engine, same snippet contract, different address space:
//
//	inputs   RunRequest.Inputs is `ctx` — JSON round-tripped in, exactly as
//	         in-binary (no functions, no cycles: only JSON-shaped data)
//	code     RunRequest.Code is the BODY of an IIFE; its return value is the
//	         step's result. `return 5` works because of the wrapper; no
//	         return at all is null, not the string "undefined"
//	data     ctx.store / ctx.sql / ctx.memory, which used to be a Go binding
//	         onto the daemon's stores and is now one host.* round trip per op
//
// The JS-visible shape is byte-for-byte the shims conductor builds in-binary
// (jsKVShim/jsSQLShim in internal/code, jsMemShim in membind.go) — a snippet
// written for `run: js` runs here unchanged, including throwing an Error when
// an op fails.
//
// The VM is fastschema/qjs: QuickJS compiled to WASM and run under wazero, so
// no cgo and no system quickjs. It has no filesystem and no network into this
// process's world; the only hole punched into it is the three __conductor_*
// host functions below, and each of those is a request conductor answers or
// refuses. Hence the empty capability manifest: this plugin dials nothing and
// spawns nothing.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/fastschema/qjs"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

// jsMemoryLimit caps the QuickJS heap (bytes): a huge-alloc snippet gets an
// out-of-memory error inside the VM instead of OOMing the process. Mirrors
// internal/code/js.go.
const jsMemoryLimit = 256 << 20

func describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindStep,
		ABI:  plugin.EngineABI,
		Type: "js",
		Desc: "JavaScript code steps on QuickJS (WASM, no cgo): ctx is the step inputs, the snippet body is an IIFE whose return value is the step's outputs, with ctx.store/ctx.sql/ctx.memory served by conductor.",
		// A data-shaping engine: no egress, no fs, no spawns. Everything it
		// can reach, it reaches by ASKING conductor, which decides per op.
		Capabilities: plugin.Capabilities{},
	}
}

func run(ctx context.Context, req plugin.RunRequest, host *plugin.Host) (out plugin.RunResult, err error) {
	fmt.Fprintf(os.Stderr, "conductor-js: run instance=%s inputs=%d data-plane=%v\n",
		req.Instance, len(req.Inputs), host.Available())
	outputs, err := execJS(ctx, req.Code, req.Inputs, host)
	if err != nil {
		return plugin.RunResult{}, err
	}
	return plugin.RunResult{Outputs: outputs}, nil
}

// execJS is internal/code/js.go's execJS, with spec.DataGuard replaced by the
// host client: the source it builds, the IIFE wrapper, the `?? null`, the
// JSON.stringify round trip and the recover-wrapped teardown are the original.
func execJS(ctx context.Context, code string, data map[string]any, host enginekit.Host) (out map[string]any, err error) {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("code: js: marshal ctx: %w", err)
	}
	if string(dataJSON) == "null" { // no data → an empty ctx, so ctx.store still attaches
		dataJSON = []byte("{}")
	}
	src := "globalThis.ctx = " + string(dataJSON) + ";\n" +
		jsKVShim() +
		jsSQLShim() +
		jsMemShim() +
		"JSON.stringify((function(){\n" + code + "\n})() ?? null)"

	// The run ctx binds the step's `timeout:` to actual execution:
	// CloseOnContextDone makes wazero halt the WASM module when ctx expires,
	// so a while(1) is cut instead of wedging the plugin. qjs surfaces that
	// halt (and any call into the then-closed module, including Close) as a
	// panic, so the whole interaction is recover-wrapped.
	defer func() {
		if r := recover(); r != nil {
			out = nil
			if ctx.Err() != nil {
				err = fmt.Errorf("code: js: %w", ctx.Err())
				return
			}
			err = fmt.Errorf("code: js: runtime fault: %v", r)
		}
	}()
	rt, err := qjs.New(qjs.Option{
		Context:            ctx,
		CloseOnContextDone: true,
		MemoryLimit:        jsMemoryLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("code: js: create runtime: %w", err)
	}
	defer func() {
		defer func() { recover() }() // Close panics if ctx already halted the module
		rt.Close()
	}()

	// The three faces bridge through ONE host function each, taking and
	// returning JSON — values cross the WASM boundary as strings, so there is
	// no Value plumbing per type. In-binary these called kvInvokeJSON /
	// sqlInvokeJSON / memInvokeJSON; here the same payload shape is answered
	// by a host.* round trip.
	qctx := rt.Context()
	bridge := func(name string, invoke func(payload string) string) {
		fn := qctx.Function(func(this *qjs.This) (*qjs.Value, error) {
			payload := ""
			if args := this.Args(); len(args) > 0 {
				payload = args[0].String()
			}
			return this.Context().NewString(invoke(payload)), nil
		})
		qctx.Global().SetPropertyStr(name, fn)
	}
	bridge("__conductor_kv", func(p string) string { return kvInvokeJSON(ctx, host, p) })
	bridge("__conductor_sql", func(p string) string { return sqlInvokeJSON(ctx, host, p) })
	bridge("__conductor_memory", func(p string) string { return memInvokeJSON(ctx, host, p) })

	ret, err := qctx.Eval("step.js", qjs.Code(src))
	if err != nil {
		return nil, fmt.Errorf("code: js: %w", err)
	}
	defer ret.Free()

	resultJSON := ret.String()
	var v any
	if err := json.Unmarshal([]byte(resultJSON), &v); err != nil {
		return nil, fmt.Errorf("code: js: decode result %q: %w", resultJSON, err)
	}
	return enginekit.WrapValue(v), nil
}

// jsKVShim builds ctx.store over the __conductor_kv host bridge:
// ctx.store("cache") returns an object whose ops serialize their args to
// JSON; a bridge error becomes a thrown Error. Absent reads come back null.
// Verbatim from internal/code/js.go.
func jsKVShim() string {
	ops, _ := json.Marshal(enginekit.KVOps)
	return `ctx.store = (store) => {
  const call = (op, args) => {
    const r = JSON.parse(__conductor_kv(JSON.stringify({ store, op, args })));
    if (r.err) throw new Error(r.err);
    return r.v ?? null;
  };
  const o = {};
  for (const op of ` + string(ops) + `) o[op] = (...args) => call(op, args);
  return o;
};
`
}

// jsSQLShim builds ctx.sql over the __conductor_sql host bridge:
// ctx.sql("analytics") returns { query, exec }, each taking (sql, args?); a
// bridge error becomes a thrown Error. Verbatim from internal/code/js.go.
func jsSQLShim() string {
	ops, _ := json.Marshal(enginekit.SQLOps)
	return `ctx.sql = (store) => {
  const call = (op, args) => {
    const r = JSON.parse(__conductor_sql(JSON.stringify({ store, op, args })));
    if (r.err) throw new Error(r.err);
    return r.v ?? null;
  };
  const o = {};
  for (const op of ` + string(ops) + `) o[op] = (...args) => call(op, args);
  return o;
};
`
}

// jsMemShim builds ctx.memory over the __conductor_memory host bridge.
// Verbatim from internal/code/membind.go.
func jsMemShim() string {
	ops, _ := json.Marshal(enginekit.MemOps)
	return `ctx.memory = (() => {
  const call = (op, args) => {
    const r = JSON.parse(__conductor_memory(JSON.stringify({ op, args })));
    if (r.err) throw new Error(r.err);
    return r.v ?? null;
  };
  const o = {};
  for (const op of ` + string(ops) + `) o[op] = (...args) => call(op, args);
  return o;
})();
`
}

// storeReq is the {store, op, args} payload the kv/sql shims send. memory's
// payload is the same minus store, and decodes into this fine.
type storeReq struct {
	Store string `json:"store"`
	Op    string `json:"op"`
	Args  []any  `json:"args"`
}

// encJS is the {"v": …} / {"err": …} reply every bridge returns, mirroring
// kvInvokeJSON's enc in internal/code/kvbind.go.
func encJS(kind string, v any, err error) string {
	var out struct {
		V   any    `json:"v"`
		Err string `json:"err,omitempty"`
	}
	out.V = v
	if err != nil {
		out.Err = err.Error()
	}
	b, merr := json.Marshal(out)
	if merr != nil {
		return `{"err":"` + kind + `: unencodable result"}`
	}
	return string(b)
}

func kvInvokeJSON(ctx context.Context, host enginekit.Host, payload string) string {
	var req storeReq
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return encJS("kv", nil, fmt.Errorf("kv: bad bridge payload: %w", err))
	}
	v, err := enginekit.KV(ctx, host, req.Store, req.Op, req.Args)
	return encJS("kv", v, err)
}

func sqlInvokeJSON(ctx context.Context, host enginekit.Host, payload string) string {
	var req storeReq
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return encJS("sql", nil, fmt.Errorf("sql: bad bridge payload: %w", err))
	}
	v, err := enginekit.SQL(ctx, host, req.Store, req.Op, req.Args)
	return encJS("sql", v, err)
}

func memInvokeJSON(ctx context.Context, host enginekit.Host, payload string) string {
	var req storeReq
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return encJS("memory", nil, fmt.Errorf("memory: bad bridge payload: %w", err))
	}
	v, err := enginekit.Memory(ctx, host, req.Op, req.Args)
	return encJS("memory", v, err)
}

func main() {
	if err := plugin.Serve(plugin.EngineFunc(describe, run)); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-js: serve: %v\n", err)
		os.Exit(1)
	}
}
