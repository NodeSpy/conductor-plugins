package main

// The three ctx faces for `use: risor`, ported from conductor's
// internal/code kvbind.go (kvRisorStoreFn), sqlbind.go (sqlRisorFn) and
// membind.go (memRisorFn).
//
// The script-visible shape is the original's exactly: store("cache") and
// sql("analytics") are top-level builtins returning a module whose members are
// the op set, and memory is a module of its four ops. Args are converted with
// object.Interface() and results with object.FromGoType, with nil becoming
// object.Nil, so an absent read reads as nil in a script — unchanged.
//
// What changed: each op used to call kvInvoke/sqlInvoke/memInvoke against the
// daemon's own stores and now makes one host.* round trip, which conductor
// answers by running those same dispatchers behind the step's own DataGuard.
//
// ONE DEVIATION, forced by the move: in-binary, store("nope") resolved the
// name eagerly through kv.Use and errored at construction for an undefined
// store. A plugin has no store registry, and the only way to ask would be to
// perform a real op — which the step's guard may refuse, turning a name check
// into a policy event. The builtin therefore always returns the op module and
// an undefined store surfaces on the FIRST OP, with conductor's own message.

import (
	"context"

	"github.com/risor-io/risor/object"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

// goArgs converts a risor call's arguments to the JSON-shaped positional args
// the wire carries, exactly as the in-binary bindings did.
func goArgs(args []object.Object) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a.Interface()
	}
	return out
}

// result converts one op's answer back into a risor value.
func result(v any, err error) object.Object {
	if err != nil {
		return object.NewError(err)
	}
	if v == nil {
		return object.Nil
	}
	return object.FromGoType(v)
}

// kvRisorStoreFn is the top-level `store("name")` builtin: it names a defined
// store and returns a module of its ops — s := store("cache"); s.get("ns",
// "k").
func kvRisorStoreFn(ctx context.Context, host enginekit.Host) object.Object {
	return object.NewBuiltin("store", func(_ context.Context, args ...object.Object) object.Object {
		if len(args) != 1 {
			return object.Errorf("store() takes the store name")
		}
		name, ok := args[0].Interface().(string)
		if !ok {
			return object.Errorf("store() takes a string name")
		}
		contents := map[string]object.Object{}
		for _, op := range enginekit.KVOps {
			op := op
			contents[op] = object.NewBuiltin("store."+op, func(_ context.Context, args ...object.Object) object.Object {
				return result(enginekit.KV(ctx, host, name, op, goArgs(args)))
			})
		}
		return object.NewBuiltinsModule("store:"+name, contents)
	})
}

// sqlRisorFn is the top-level `sql("name")` builtin: it names a defined SQL
// store and returns a module of its ops — db := sql("analytics");
// db.query("SELECT …", [args]).
func sqlRisorFn(ctx context.Context, host enginekit.Host) object.Object {
	return object.NewBuiltin("sql", func(_ context.Context, args ...object.Object) object.Object {
		if len(args) != 1 {
			return object.Errorf("sql() takes the store name")
		}
		name, ok := args[0].Interface().(string)
		if !ok {
			return object.Errorf("sql() takes a string name")
		}
		contents := map[string]object.Object{}
		for _, op := range enginekit.SQLOps {
			op := op
			contents[op] = object.NewBuiltin("sql."+op, func(_ context.Context, args ...object.Object) object.Object {
				return result(enginekit.SQL(ctx, host, name, op, goArgs(args)))
			})
		}
		return object.NewBuiltinsModule("sql:"+name, contents)
	})
}

// memRisorFn is the top-level `memory` module: memory.remember("txt",
// ["tag"], "repo:o/r"), memory.recall({...}).
func memRisorFn(ctx context.Context, host enginekit.Host) object.Object {
	contents := map[string]object.Object{}
	for _, op := range enginekit.MemOps {
		op := op
		contents[op] = object.NewBuiltin("memory."+op, func(_ context.Context, args ...object.Object) object.Object {
			return result(enginekit.Memory(ctx, host, op, goArgs(args)))
		})
	}
	return object.NewBuiltinsModule("memory", contents)
}
