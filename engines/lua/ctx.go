package main

// The three ctx faces for `use: lua`, ported from conductor's internal/code
// kvbind.go (luaStoreFn), sqlbind.go (luaSQLFn) and membind.go (luaMemFn).
//
// The script-visible shape is the original's exactly: ctx.store(name) and
// ctx.sql(name) return a table of the op set, ctx.memory is a table of its
// four ops, every op takes its positional args and RAISES on error rather than
// returning one — so `local v = ctx.store("cache").get("ns", "k")` and a
// pcall around it both behave as they did in-binary.
//
// What changed: each op used to call kvInvoke/sqlInvoke/memInvoke against the
// daemon's own stores and now makes one host.* round trip, which conductor
// answers by running those same dispatchers behind the step's own DataGuard.
//
// ONE DEVIATION, forced by the move: in-binary, ctx.store("nope") resolved the
// name eagerly through kv.Use and raised at construction for an undefined
// store. A plugin has no store registry, and the only way to ask would be to
// perform a real op — which the step's guard may refuse, turning a name check
// into a policy event. The constructor therefore always returns the op table
// and an undefined store raises on the FIRST OP, with conductor's own message.

import (
	"context"

	lua "github.com/yuin/gopher-lua"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

// opTable builds the Lua table of ops for one face. invoke is what each op
// does with the script's positional args.
func opTable(L *lua.LState, ops []string, invoke func(op string, args []any) (any, error)) *lua.LTable {
	t := L.NewTable()
	for _, op := range ops {
		op := op
		t.RawSetString(op, L.NewFunction(func(L *lua.LState) int {
			n := L.GetTop()
			args := make([]any, 0, n)
			for i := 1; i <= n; i++ {
				args = append(args, luaToGo(L.Get(i)))
			}
			v, err := invoke(op, args)
			if err != nil {
				L.RaiseError("%s", err.Error())
				return 0
			}
			L.Push(goToLua(L, v))
			return 1
		}))
	}
	return t
}

// luaStoreFn is ctx.store: ctx.store("cache") names a defined store and
// returns a table of its ops; errors raise.
func luaStoreFn(ctx context.Context, L *lua.LState, host enginekit.Host) *lua.LFunction {
	return L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		L.Push(opTable(L, enginekit.KVOps, func(op string, args []any) (any, error) {
			return enginekit.KV(ctx, host, name, op, args)
		}))
		return 1
	})
}

// luaSQLFn is ctx.sql: ctx.sql("analytics") names a defined SQL store and
// returns a table of its ops; errors raise.
func luaSQLFn(ctx context.Context, L *lua.LState, host enginekit.Host) *lua.LFunction {
	return L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		L.Push(opTable(L, enginekit.SQLOps, func(op string, args []any) (any, error) {
			return enginekit.SQL(ctx, host, name, op, args)
		}))
		return 1
	})
}

// luaMemFn is ctx.memory — a table of the ops; errors raise.
func luaMemFn(ctx context.Context, L *lua.LState, host enginekit.Host) *lua.LTable {
	return opTable(L, enginekit.MemOps, func(op string, args []any) (any, error) {
		return enginekit.Memory(ctx, host, op, args)
	})
}
