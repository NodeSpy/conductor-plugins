// Command conductor-lua is the `use: lua` STEP ENGINE as an external
// conductor plugin: the in-binary gopher-lua engine (conductor
// internal/code/lua.go, execLua) lifted out of the daemon and put behind the
// plugin wire.
//
// gopher-lua is a pure-Go Lua 5.1 VM, chosen over the cgo bindings
// specifically to keep a zero-cgo cross-compiled release — which is as true
// for a fetched plugin binary as it was in the daemon. The snippet contract is
// unchanged: the step's inputs are the `ctx` global (a Lua table), and the
// script `return`s its result — a table with string keys becomes the step's
// named outputs, an array-like table a value: list, any other value lands
// under value:.
//
// THE SANDBOX IS THE OPENED LIBRARIES. Only base, table, string and math are
// opened (no os, io, debug, or package), and the base library's file/chunk
// loaders — dofile, loadfile, load, loadstring — are then removed, so a script
// cannot reach the filesystem or assemble new chunks from data.
//
// The three ctx faces keep their in-binary spelling — ctx.store("cache"),
// ctx.sql("analytics"), ctx.memory — so a script written for `run: lua` runs
// here unchanged, including the raise-on-error behaviour. Each op is now one
// host.* round trip that conductor authorizes against the step's own guard.
//
// No egress, no fs, no spawns: the empty manifest is the claim.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"fmt"
	"os"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	lua "github.com/yuin/gopher-lua"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

func describe() plugin.Decl {
	return plugin.Decl{
		Kind:         plugin.KindStep,
		ABI:          plugin.EngineABI,
		Type:         "lua",
		Desc:         "Lua 5.1 code steps on gopher-lua (pure Go, no cgo): ctx is the step inputs, the script's return value is the step's outputs, with ctx.store/ctx.sql/ctx.memory served by conductor.",
		Capabilities: plugin.Capabilities{},
	}
}

func run(ctx context.Context, req plugin.RunRequest, host *plugin.Host) (plugin.RunResult, error) {
	fmt.Fprintf(os.Stderr, "conductor-lua: run instance=%s inputs=%d data-plane=%v\n",
		req.Instance, len(req.Inputs), host.Available())
	outputs, err := execLua(ctx, req.Code, req.Inputs, host)
	if err != nil {
		return plugin.RunResult{}, err
	}
	return plugin.RunResult{Outputs: outputs}, nil
}

// execLua is internal/code/lua.go's execLua with the guard replaced by the
// host client: the library set, the loader removal, the ctx binding for
// cancellation and the output contract are the original's.
func execLua(ctx context.Context, code string, data map[string]any, host enginekit.Host) (map[string]any, error) {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()
	// Bind the run ctx so the step's `timeout:` actually cuts a runaway
	// script — the VM checks the context in its exec loop, so `while true do
	// end` returns a context error instead of wedging the plugin. (gopher-lua
	// has no heap cap; ctx cancellation is the available limit.)
	L.SetContext(ctx)
	for _, o := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(o.fn))
		L.Push(lua.LString(o.name))
		L.Call(1, 0)
	}
	for _, g := range []string{"dofile", "loadfile", "load", "loadstring"} {
		L.SetGlobal(g, lua.LNil)
	}
	ctxTbl := goToLua(L, data)
	if t, ok := ctxTbl.(*lua.LTable); ok {
		t.RawSetString("store", luaStoreFn(ctx, L, host)) // ctx.store("cache").get(…)
		t.RawSetString("sql", luaSQLFn(ctx, L, host))     // ctx.sql("analytics").query(…)
		t.RawSetString("memory", luaMemFn(ctx, L, host))  // ctx.memory.remember(…)
	}
	L.SetGlobal("ctx", ctxTbl)

	if err := L.DoString(code); err != nil {
		return nil, fmt.Errorf("lua: %w", err)
	}
	if L.GetTop() == 0 {
		return map[string]any{}, nil
	}
	return enginekit.WrapValue(luaToGo(L.Get(-1))), nil
}

// goToLua converts a Go value (the JSON-shaped step data) into a Lua value.
// Verbatim from internal/code/lua.go.
func goToLua(L *lua.LState, v any) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(x)
	case string:
		return lua.LString(x)
	case int:
		return lua.LNumber(x)
	case int64:
		return lua.LNumber(x)
	case float64:
		return lua.LNumber(x)
	case map[string]any:
		t := L.NewTable()
		for k, e := range x {
			t.RawSetString(k, goToLua(L, e))
		}
		return t
	case []any:
		t := L.NewTable()
		for i, e := range x {
			t.RawSetInt(i+1, goToLua(L, e))
		}
		return t
	case []string:
		t := L.NewTable()
		for i, e := range x {
			t.RawSetInt(i+1, lua.LString(e))
		}
		return t
	}
	return lua.LString(fmt.Sprintf("%v", v))
}

// luaToGo converts a Lua return value back into JSON-shaped Go data. A table
// is a map when it has any string key, else a 1..n array. Verbatim from
// internal/code/lua.go.
func luaToGo(v lua.LValue) any {
	switch x := v.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		return bool(x)
	case lua.LString:
		return string(x)
	case lua.LNumber:
		f := float64(x)
		if f == float64(int64(f)) {
			return int64(f)
		}
		return f
	case *lua.LTable:
		asMap := map[string]any{}
		var asList []any
		listOK := true
		n := 0
		x.ForEach(func(k, e lua.LValue) {
			n++
			if ks, ok := k.(lua.LString); ok {
				asMap[string(ks)] = luaToGo(e)
				listOK = false
				return
			}
			if kn, ok := k.(lua.LNumber); ok && float64(kn) == float64(n) {
				asList = append(asList, luaToGo(e))
				return
			}
			listOK = false
		})
		if listOK && len(asMap) == 0 {
			return asList
		}
		// Mixed tables keep their numeric entries under stringified keys.
		x.ForEach(func(k, e lua.LValue) {
			if kn, ok := k.(lua.LNumber); ok {
				asMap[kn.String()] = luaToGo(e)
			}
		})
		return asMap
	}
	return v.String()
}

func main() {
	if err := plugin.Serve(plugin.EngineFunc(describe, run)); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-lua: serve: %v\n", err)
		os.Exit(1)
	}
}
