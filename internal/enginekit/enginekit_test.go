package enginekit_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
	"github.com/NodeSpy/conductor-plugins/internal/enginekit/hosttest"
)

// The whole contract of this package is that a snippet's op and its
// POSITIONAL args reach conductor unreshaped — that is what lets the daemon
// run its own kvInvoke against them and produce the same answers, and the same
// errors, an in-process engine got.
func TestArgsCrossVerbatim(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		do   func(h *hosttest.Host) (any, error)
		want hosttest.Call
	}{
		{
			name: "kv get",
			do: func(h *hosttest.Host) (any, error) {
				return enginekit.KV(ctx, h, "cache", "get", []any{"run", "attempts"})
			},
			want: hosttest.Call{Kind: "kv", Op: "get", Resource: "cache", Args: []any{"run", "attempts"}},
		},
		{
			// The third arg the SDK's typed HostKV.Pop has no parameter for.
			name: "kv pop keeps its from argument",
			do: func(h *hosttest.Host) (any, error) {
				return enginekit.KV(ctx, h, "cache", "pop", []any{"ns", "k", "front"})
			},
			want: hosttest.Call{Kind: "kv", Op: "pop", Resource: "cache", Args: []any{"ns", "k", "front"}},
		},
		{
			// A short call stays short, so conductor answers with its own
			// "kv.set: want 3 args, got 2" rather than a padded-out op.
			name: "kv set with too few args is not padded",
			do: func(h *hosttest.Host) (any, error) {
				return enginekit.KV(ctx, h, "cache", "set", []any{"ns"})
			},
			want: hosttest.Call{Kind: "kv", Op: "set", Resource: "cache", Args: []any{"ns"}},
		},
		{
			name: "sql query carries the bind list as one arg",
			do: func(h *hosttest.Host) (any, error) {
				return enginekit.SQL(ctx, h, "analytics", "query",
					[]any{"SELECT v FROM t WHERE k = ?", []any{"k1"}})
			},
			want: hosttest.Call{Kind: "sql", Op: "query", Resource: "analytics",
				Args: []any{"SELECT v FROM t WHERE k = ?", []any{"k1"}}},
		},
		{
			name: "memory has no store dimension",
			do: func(h *hosttest.Host) (any, error) {
				return enginekit.Memory(ctx, h, "remember", []any{"a fact", []any{"tag"}, "repo:o/r"})
			},
			want: hosttest.Call{Kind: "memory", Op: "remember", Resource: "",
				Args: []any{"a fact", []any{"tag"}, "repo:o/r"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &hosttest.Host{}
			if _, err := tc.do(h); err != nil {
				t.Fatal(err)
			}
			got, err := h.Only()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("host call = %v, want %v", got, tc.want)
			}
		})
	}
}

// An op outside the set never reaches the wire: the engines build their
// bindings from these lists, so a call naming something else is a bug in the
// engine rather than a question for conductor.
func TestUnknownOpNeverLeaves(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		do   func(h *hosttest.Host) (any, error)
		want string
	}{
		{"kv", func(h *hosttest.Host) (any, error) {
			return enginekit.KV(ctx, h, "cache", "drop", nil)
		}, `kv: no operation "drop"`},
		{"sql", func(h *hosttest.Host) (any, error) {
			return enginekit.SQL(ctx, h, "analytics", "truncate", nil)
		}, `sql: no operation "truncate"`},
		{"memory", func(h *hosttest.Host) (any, error) {
			return enginekit.Memory(ctx, h, "purge", nil)
		}, `memory: no operation "purge"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &hosttest.Host{}
			_, err := tc.do(h)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if got := h.Calls(); len(got) != 0 {
				t.Fatalf("an unknown op reached the host: %v", got)
			}
		})
	}
}

// A refusal or a failure comes back as the error it is; nothing here retries
// or reinterprets it.
func TestHostErrorPropagates(t *testing.T) {
	boom := errors.New("refused by conductor: kv.set: no_secret_egress")
	h := &hosttest.Host{Reply: func(hosttest.Call) (any, error) { return nil, boom }}
	_, err := enginekit.KV(context.Background(), h, "cache", "set", []any{"ns", "k", "v"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

// A run granted no data plane says so rather than panicking on a nil client.
func TestNoDataPlane(t *testing.T) {
	_, err := enginekit.KV(context.Background(), nil, "cache", "get", []any{"ns", "k"})
	if err == nil || err.Error() != "kv.get: this run was granted no ctx data plane" {
		t.Fatalf("err = %v", err)
	}
}

func TestWrapValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want map[string]any
	}{
		{"nil is no outputs", nil, map[string]any{}},
		{"an object is the outputs", map[string]any{"a": 1}, map[string]any{"a": 1}},
		{"a scalar is the value output", 7, map[string]any{"value": 7}},
		{"a list is the value output", []any{1, 2}, map[string]any{"value": []any{1, 2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := enginekit.WrapValue(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("WrapValue(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
