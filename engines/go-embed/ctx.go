package main

// The three ctx faces for `use: go-embed`, ported from conductor's
// internal/code kvbind.go / sqlbind.go / membind.go (KVHandle, SQLHandle,
// MemHandle and their *GoEmbedExports).
//
// The METHOD SETS are the original's, so a snippet's call sites and its
// handling of the returned Go types are unchanged. What differs is one line
// inside each: where the in-binary handle called kvInvoke/sqlInvoke/memInvoke
// against the daemon's own stores, this one makes a host.* round trip and
// conductor runs those same dispatchers on its side, against the same
// DataGuard.
//
// ONE DEVIATION, and it is forced. In-binary, store.Use("cache") /
// sql.Use("analytics") resolved the name eagerly through kv.Use / sqlstore.Use
// and returned an error for an undefined store before any op ran. A plugin has
// no store registry to consult, and the only way to ask would be to perform a
// real op — which the step's guard may refuse, turning a name check into a
// policy event. So Use here always returns a handle and an undefined store
// surfaces on the FIRST OP instead, with conductor's own "no such store"
// message. The error text is the daemon's either way; only its timing moved.

import (
	"context"
	"reflect"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

// KVHandle is the go-embed face of one defined store: `import
// "conductor/store"`, then `st, err := store.Use("cache")` and call the typed
// methods. Reads fold "absent" into nil; Slice's end is exclusive (use Len for
// "to the end").
type KVHandle struct {
	name string
	ctx  context.Context
	host enginekit.Host
}

func (h KVHandle) call(op string, args ...any) (any, error) {
	return enginekit.KV(h.ctx, h.host, h.name, op, args)
}

func (h KVHandle) Get(ns, key string) (any, error) { return h.call("get", ns, key) }
func (h KVHandle) Set(ns, key string, v any) error {
	_, err := h.call("set", ns, key, v)
	return err
}
func (h KVHandle) SetNX(ns, key string, v any) (any, bool, error) {
	r, err := h.call("setnx", ns, key, v)
	if err != nil {
		return nil, false, err
	}
	m, _ := r.(map[string]any)
	created, _ := m["created"].(bool)
	return m["value"], created, nil
}
func (h KVHandle) Merge(ns, key string, patch map[string]any) (map[string]any, error) {
	r, err := h.call("merge", ns, key, patch)
	if err != nil {
		return nil, err
	}
	m, _ := r.(map[string]any)
	return m, nil
}
func (h KVHandle) Delete(ns, key string) error {
	_, err := h.call("delete", ns, key)
	return err
}
func (h KVHandle) Incr(ns, key string, by int64) (int64, error) {
	r, err := h.call("incr", ns, key, by)
	if err != nil {
		return 0, err
	}
	return asInt64(r), nil
}
func (h KVHandle) Append(ns, key string, items any, unique bool) ([]any, error) {
	r, err := h.call("append", ns, key, items, unique)
	if err != nil {
		return nil, err
	}
	l, _ := r.([]any)
	return l, nil
}
func (h KVHandle) Remove(ns, key string, items any) ([]any, error) {
	r, err := h.call("remove", ns, key, items)
	if err != nil {
		return nil, err
	}
	l, _ := r.([]any)
	return l, nil
}
func (h KVHandle) Contains(ns, key string, item any) (bool, error) {
	r, err := h.call("contains", ns, key, item)
	if err != nil {
		return false, err
	}
	b, _ := r.(bool)
	return b, nil
}
func (h KVHandle) List(ns, prefix string) (map[string]any, error) {
	r, err := h.call("list", ns, prefix)
	if err != nil {
		return nil, err
	}
	m, _ := r.(map[string]any)
	return m, nil
}
func (h KVHandle) First(ns, key string) (any, error) { return h.call("first", ns, key) }
func (h KVHandle) Last(ns, key string) (any, error)  { return h.call("last", ns, key) }
func (h KVHandle) Index(ns, key string, i int) (any, error) {
	return h.call("index", ns, key, i)
}
func (h KVHandle) Slice(ns, key string, start, end int) ([]any, error) {
	r, err := h.call("slice", ns, key, start, end)
	if err != nil {
		return nil, err
	}
	l, _ := r.([]any)
	return l, nil
}
func (h KVHandle) Len(ns, key string) (int, error) {
	r, err := h.call("len", ns, key)
	if err != nil {
		return 0, err
	}
	return int(asInt64(r)), nil
}
func (h KVHandle) Pop(ns, key, from string) (any, error) { return h.call("pop", ns, key, from) }

// asInt64 reads a count back off the wire. In-binary these methods type-
// asserted (r.(int64), r.(int)) because the value came straight out of the
// store as a Go int; across JSON it arrives as a float64, so the assertion
// would panic on a perfectly good answer. Tolerating both is the port.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// kvGoEmbedExports is the `import "conductor/store"` virtual package:
// store.Use("cache") names a defined store and returns a KVHandle.
func kvGoEmbedExports(ctx context.Context, host enginekit.Host) map[string]map[string]reflect.Value {
	return map[string]map[string]reflect.Value{
		"conductor/store/store": {
			"Use": reflect.ValueOf(func(name string) (KVHandle, error) {
				return KVHandle{name: name, ctx: ctx, host: host}, nil
			}),
			"KVHandle": reflect.ValueOf((*KVHandle)(nil)),
		},
	}
}

// SQLHandle is the go-embed face of one defined SQL store: `import
// "conductor/sql"`, then `db, err := sql.Use("analytics")` and call Query or
// Exec.
type SQLHandle struct {
	name string
	ctx  context.Context
	host enginekit.Host
}

// Query runs a row-returning statement; each row is a column→value map.
func (h SQLHandle) Query(query string, args []any) ([]any, error) {
	r, err := enginekit.SQL(h.ctx, h.host, h.name, "query", []any{query, args})
	if err != nil {
		return nil, err
	}
	l, _ := r.([]any)
	return l, nil
}

// Exec runs a mutating statement, returning {rows_affected, last_insert_id?}.
func (h SQLHandle) Exec(query string, args []any) (map[string]any, error) {
	r, err := enginekit.SQL(h.ctx, h.host, h.name, "exec", []any{query, args})
	if err != nil {
		return nil, err
	}
	m, _ := r.(map[string]any)
	return m, nil
}

// sqlGoEmbedExports is the `import "conductor/sql"` virtual package:
// sql.Use("analytics") names a defined SQL store and returns a SQLHandle.
func sqlGoEmbedExports(ctx context.Context, host enginekit.Host) map[string]map[string]reflect.Value {
	return map[string]map[string]reflect.Value{
		"conductor/sql/sql": {
			"Use": reflect.ValueOf(func(name string) (SQLHandle, error) {
				return SQLHandle{name: name, ctx: ctx, host: host}, nil
			}),
			"SQLHandle": reflect.ValueOf((*SQLHandle)(nil)),
		},
	}
}

// MemHandle is the go-embed face of the configured memory: `import
// "conductor/memory"`, then `memory.Remember(…)` / `memory.Recall(…)`.
type MemHandle struct {
	ctx  context.Context
	host enginekit.Host
}

// Remember stores one memory and returns it as a map.
func (h MemHandle) Remember(text string, tags []string, scope string) (map[string]any, error) {
	anyTags := make([]any, len(tags))
	for i, t := range tags {
		anyTags[i] = t
	}
	r, err := enginekit.Memory(h.ctx, h.host, "remember", []any{text, anyTags, scope})
	if err != nil {
		return nil, err
	}
	m, _ := r.(map[string]any)
	return m, nil
}

// Recall filters memories ({tags, scope, substring, limit}), newest first.
func (h MemHandle) Recall(q map[string]any) ([]any, error) {
	r, err := enginekit.Memory(h.ctx, h.host, "recall", []any{q})
	if err != nil {
		return nil, err
	}
	l, _ := r.([]any)
	return l, nil
}

// Forget removes one memory by id; reports whether it existed.
func (h MemHandle) Forget(id string) (bool, error) {
	r, err := enginekit.Memory(h.ctx, h.host, "forget", []any{id})
	if err != nil {
		return false, err
	}
	b, _ := r.(bool)
	return b, nil
}

// List returns this execution's own scope's entries, newest first.
func (h MemHandle) List() ([]any, error) {
	r, err := enginekit.Memory(h.ctx, h.host, "list", nil)
	if err != nil {
		return nil, err
	}
	l, _ := r.([]any)
	return l, nil
}

// memGoEmbedExports is the `import "conductor/memory"` virtual package.
func memGoEmbedExports(ctx context.Context, host enginekit.Host) map[string]map[string]reflect.Value {
	h := MemHandle{ctx: ctx, host: host}
	return map[string]map[string]reflect.Value{
		"conductor/memory/memory": {
			"Remember": reflect.ValueOf(h.Remember),
			"Recall":   reflect.ValueOf(h.Recall),
			"Forget":   reflect.ValueOf(h.Forget),
			"List":     reflect.ValueOf(h.List),
		},
	}
}
