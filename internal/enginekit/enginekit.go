// Package enginekit is the shared ctx DATA PLANE for this repo's step-engine
// plugins (engines/js, engines/go-embed, engines/risor, engines/lua).
//
// It is the out-of-process twin of conductor's internal/code kvbind.go /
// sqlbind.go / membind.go. In-binary, those files hold the ONE dispatcher
// (kvInvoke/sqlInvoke/memInvoke) every engine's ctx.store/ctx.sql/ctx.memory
// binding funnels through, and that dispatcher reaches straight into the
// daemon's stores after consulting the step's DataGuard. A plugin holds no
// store, no connection string and no guard — so the same three dispatchers
// live here as three functions that do exactly one thing: forward the op to
// conductor over host.kv / host.sql / host.memory and hand back what comes
// out.
//
// ENFORCEMENT IS HOST-SIDE, and that is the whole reason this file is thin.
// Conductor answers a host.* call by running internal/code's CtxHandler.Invoke,
// which calls the very same kvInvoke/sqlInvoke/memInvoke an in-process engine
// calls, carrying the very same Spec.DataGuard (see internal/code/ctxhost.go).
// So arity checks, arg coercion, absent-read-folds-to-nil, the per-store
// capability gates and the plan write barrier all still happen — once, on
// conductor's side. Re-implementing any of them here would be a second
// enforcement point that can only drift.
//
// ARGS CROSS VERBATIM, which is what makes that work. HostRequest.Args follows
// the POSITIONAL convention of the in-process bindings — `kv get ns key` is
// ["ns","key"], `kv set ns key v` is ["ns","key",v], `sql query` is ["SELECT
// …",[bind…]], memory's ops take their own positional args — and a snippet's
// call site produces exactly that list. Passing it through untouched means a
// two-arg `ctx.store("c").set("ns","k")` gets conductor's own "kv.set: want 3
// args, got 2" rather than a plugin-invented message, and a `pop(ns,key,
// "front")` keeps its third argument, which the SDK's typed HostKV.Pop has no
// parameter for.
//
// On the typed SDK client: plugin.Host's KV()/SQL()/Memory() faces are
// fixed-arity wrappers that each build one Host.Call with a fixed tuple. These
// engines call Host.Call directly — the same wire bytes — because a snippet's
// (op, args…) is dynamic and must survive unreshaped, and because plugin.Host
// is a concrete struct with unexported fields, so an interface over Call is
// also the only seam a test can drive. Host below is that seam, and
// *plugin.Host satisfies it.
package enginekit

import (
	"context"
	"fmt"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// Host is the one call an engine needs from the SDK's host client: one
// data-plane op, exactly as the wire carries it. *plugin.Host satisfies it
// (plugin.Host.Call), and a test double can too.
type Host interface {
	Call(ctx context.Context, kind, op, resource string, args ...any) (any, error)
}

// compile-time proof that the SDK's host client is usable as our seam.
var _ Host = (*plugin.Host)(nil)

// KVOps is the ctx.store method set every engine exposes — the same list, in
// the same order, as internal/code's kvOps. The engines build their bindings
// by looping over it, so an engine cannot quietly offer a different surface
// than its siblings.
var KVOps = []string{
	"get", "set", "setnx", "merge", "delete", "incr",
	"append", "remove", "contains", "list",
	"first", "last", "index", "slice", "len", "pop",
}

// SQLOps is the ctx.sql method set (internal/code sqlOps).
var SQLOps = []string{"query", "exec"}

// MemOps is the ctx.memory method set (internal/code memOps).
var MemOps = []string{"remember", "recall", "forget", "list"}

func inSet(ops []string, op string) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

// KV runs one ctx.store op against the named DEFINED store. Args are the
// snippet's own positional args, forwarded untouched; ns is args[0], key is
// args[1], per the in-process convention.
func KV(ctx context.Context, h Host, store, op string, args []any) (any, error) {
	if !inSet(KVOps, op) {
		return nil, fmt.Errorf("kv: no operation %q", op)
	}
	return call(ctx, h, plugin.HostKindKV, op, store, args)
}

// SQL runs one ctx.sql op against the named DEFINED SQL store. Args are
// (sql, bind?) — one statement and one list of bind values, never values
// spliced into the statement text.
func SQL(ctx context.Context, h Host, store, op string, args []any) (any, error) {
	if !inSet(SQLOps, op) {
		return nil, fmt.Errorf("sql: no operation %q", op)
	}
	return call(ctx, h, plugin.HostKindSQL, op, store, args)
}

// Memory runs one ctx.memory op. It has no store dimension — conductor
// resolves the scope each op touches itself (internal/code memScopeOf) — so
// the resource is empty.
func Memory(ctx context.Context, h Host, op string, args []any) (any, error) {
	if !inSet(MemOps, op) {
		return nil, fmt.Errorf("memory: no operation %q", op)
	}
	return call(ctx, h, plugin.HostKindMemory, op, "", args)
}

// call is the single point every op goes through, so "this run has no data
// plane" reads the same from all three faces. plugin.Host answers that case
// itself; a nil interface (an engine constructed without a host at all) would
// otherwise panic, and a panic is a much worse report than a sentence.
func call(ctx context.Context, h Host, kind, op, resource string, args []any) (any, error) {
	if h == nil {
		return nil, fmt.Errorf("%s.%s: this run was granted no ctx data plane", kind, op)
	}
	if args == nil {
		args = []any{}
	}
	return h.Call(ctx, kind, op, resource, args...)
}
