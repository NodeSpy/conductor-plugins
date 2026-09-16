// Package hosttest is a TEST DOUBLE for the conductor side of an engine
// plugin's ctx data plane.
//
// The SDK's plugin.Host is a concrete struct whose call table is unexported —
// a working one can only be built by the Serve loop with a live daemon on the
// other end — so an engine's run() is made testable through the one-method
// enginekit.Host seam instead, and this is what stands in for the daemon
// behind it.
//
// It records every op an engine issues (so a test can assert the KIND, the
// OP, the STORE and the exact positional ARGS that crossed) and answers from
// a caller-supplied Reply. Both directions go through a JSON round trip on
// purpose: the real path is newline-delimited JSON-RPC, so a value an engine
// cannot encode, or a Go type that only survives in-process (an int64 that
// comes back a float64), fails here rather than in production.
package hosttest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Call is one recorded data-plane op, as it crossed the wire.
type Call struct {
	Kind     string // kv | sql | memory
	Op       string
	Resource string // the store name; empty for memory
	Args     []any
}

// String renders a call the way an assertion failure reads best.
func (c Call) String() string {
	b, _ := json.Marshal(c.Args)
	return fmt.Sprintf("%s.%s(%s)%s", c.Kind, c.Op, c.Resource, b)
}

// Host is the recorder. Reply decides what each op answers; a nil Reply
// answers nil, which is what an absent read looks like.
type Host struct {
	Reply func(Call) (any, error)

	mu    sync.Mutex
	calls []Call
}

// Call implements enginekit.Host.
func (h *Host) Call(_ context.Context, kind, op, resource string, args ...any) (any, error) {
	wire, err := roundTrip(args)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: args will not encode: %w", kind, op, err)
	}
	c := Call{Kind: kind, Op: op, Resource: resource}
	if wire != nil {
		c.Args = wire.([]any)
	}
	h.mu.Lock()
	h.calls = append(h.calls, c)
	reply := h.Reply
	h.mu.Unlock()
	if reply == nil {
		return nil, nil
	}
	v, err := reply(c)
	if err != nil {
		return nil, err
	}
	out, err := roundTrip(v)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: value will not encode: %w", kind, op, err)
	}
	return out, nil
}

// Calls is everything recorded so far.
func (h *Host) Calls() []Call {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Call(nil), h.calls...)
}

// Only returns the single recorded call, or an error naming what was actually
// recorded — the common assertion, without the boilerplate.
func (h *Host) Only() (Call, error) {
	got := h.Calls()
	if len(got) != 1 {
		return Call{}, fmt.Errorf("want exactly 1 host call, got %d: %v", len(got), got)
	}
	return got[0], nil
}

// roundTrip is the wire: encode, decode, so only JSON-shaped values survive
// and they arrive as the types JSON produces.
func roundTrip(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
