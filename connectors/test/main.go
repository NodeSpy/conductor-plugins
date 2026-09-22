// Command conductor-test is a diagnostic connector for exercising conductor
// itself — no external service, no dependencies. It provides a synthetic SOURCE
// (a `tick` event emitted on an interval, with a rotating label so filters have
// something to match) and a handful of action VERBS: ping, echo, sleep, fail
// (a forced error, to test error handling / gates / retries), counter, random,
// and now. Use it to smoke-test triggers, filters, grouping, step wiring, and
// hooks without wiring up a real integration.
//
// Connection / Config (the source reads these; verbs are self-contained):
//
//	interval: 2s        # time between ticks (default 2s; accepts "500ms", "2s", or a number of seconds)
//	count: 0            # stop after N ticks (0 = forever)
//	message: "hello"    # a static string carried in every tick
//	labels: [a, b]      # rotated across ticks so `filter: { label: a }` has something to match
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// counter backs the `counter` verb — per plugin process (i.e. per connector
// instance the daemon keeps alive), so it survives across Invoke calls.
var counter atomic.Int64

type testPlugin struct{}

func (testPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "test",
		Desc: "Diagnostic connector: a synthetic tick source and action verbs (ping/echo/sleep/fail/counter/random/now) for smoke-testing triggers, filters, steps, and gates without a real service.",
		Connection: plugin.Schema{
			"interval": {Type: "any", Desc: "time between ticks (default 2s; \"500ms\"/\"2s\" or a number of seconds) — source only"},
			"count":    {Type: "integer", Desc: "stop after N ticks (0 = forever) — source only"},
			"message":  {Type: "string", Desc: "a static string carried in every tick — source only"},
			"labels":   {Type: "list", Desc: "labels rotated across ticks so filters have something to match — source only"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "ping", Desc: "return pong (the simplest liveness check)",
				Options: plugin.Schema{"message": {Type: "string", Desc: "echoed back in the reply"}},
				Outputs: plugin.Schema{"pong": {Type: "boolean"}, "message": {Type: "string"}, "instance": {Type: "string"}, "time": {Type: "integer"}},
			},
			{
				Name: "echo", Desc: "return the options back verbatim (handy for templating tests)",
				Options: plugin.Schema{"data": {Type: "any", Desc: "any value — returned under data"}},
				Outputs: plugin.Schema{"data": {Type: "any"}, "received": {Type: "map", Desc: "every option passed, echoed back"}},
			},
			{
				Name: "sleep", Desc: "block for a while (to test timeouts and long steps); capped at 60s",
				Options: plugin.Schema{"seconds": {Type: "any", Desc: "how long to sleep (number of seconds, or \"500ms\"/\"2s\")"}},
				Outputs: plugin.Schema{"slept_seconds": {Type: "number"}},
			},
			{
				Name: "fail", Desc: "always return an error (to exercise error handling, gates, and retries)",
				Options: plugin.Schema{
					"message": {Type: "string", Desc: "the error message (default \"test: forced failure\")"},
					"code":    {Type: "string", Enum: []string{"invalid_params", "internal"}, Desc: "the error code (default internal)"},
				},
			},
			{
				Name: "counter", Desc: "increment and return a per-instance counter",
				Options: plugin.Schema{"reset": {Type: "boolean", Desc: "reset the counter to 0 before returning (returns 0)"}},
				Outputs: plugin.Schema{"count": {Type: "integer"}},
			},
			{
				Name: "random", Desc: "return random values (uuid, hex, int)",
				Options: plugin.Schema{"max": {Type: "integer", Desc: "upper bound for int (exclusive; default 1000000)"}},
				Outputs: plugin.Schema{"uuid": {Type: "string"}, "hex": {Type: "string"}, "int": {Type: "integer"}},
			},
			{
				Name: "now", Desc: "return the current time",
				Outputs: plugin.Schema{"unix": {Type: "integer"}, "unix_ms": {Type: "integer"}, "iso": {Type: "string", Desc: "RFC3339"}},
			},
		},
		Events: []plugin.Event{
			{
				Name: "tick",
				Desc: "a synthetic event emitted every interval",
				Context: plugin.Schema{
					"n":       {Type: "integer", Desc: "the tick number, starting at 1"},
					"label":   {Type: "string", Desc: "the rotating label for this tick (empty if no labels configured)"},
					"message": {Type: "string", Desc: "the static message from config"},
					"time":    {Type: "integer", Desc: "unix seconds"},
				},
				Filters: plugin.Schema{
					"labels": {Type: "list", Desc: "match any of these labels"},
					"label":  {Type: "string"},
					"n":      {Type: "integer"},
				},
			},
		},
		// Purely synthetic: no network, no filesystem, no processes.
		Capabilities: plugin.Capabilities{Spawns: false},
	}
}

func (testPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	switch req.Verb {
	case "ping":
		return plugin.InvokeResult{Outputs: map[string]any{
			"pong":     true,
			"message":  strOr(o["message"], "pong"),
			"instance": req.Instance,
			"time":     time.Now().Unix(),
		}}, nil

	case "echo":
		return plugin.InvokeResult{Outputs: map[string]any{"data": o["data"], "received": o}}, nil

	case "sleep":
		d := durationOf(o["seconds"], time.Second)
		if d > 60*time.Second {
			d = 60 * time.Second
		}
		if d < 0 {
			d = 0
		}
		time.Sleep(d)
		return plugin.InvokeResult{Outputs: map[string]any{"slept_seconds": d.Seconds()}}, nil

	case "fail":
		msg := strOr(o["message"], "test: forced failure")
		return plugin.InvokeResult{}, plugin.Errorf(failCode(str(o["code"])), msg)

	case "counter":
		if boolv(o["reset"]) {
			counter.Store(0)
			return plugin.InvokeResult{Outputs: map[string]any{"count": int64(0)}}, nil
		}
		return plugin.InvokeResult{Outputs: map[string]any{"count": counter.Add(1)}}, nil

	case "random":
		return plugin.InvokeResult{Outputs: randomOutputs(intOr(o["max"], 1000000))}, nil

	case "now":
		now := time.Now()
		return plugin.InvokeResult{Outputs: map[string]any{
			"unix":    now.Unix(),
			"unix_ms": now.UnixMilli(),
			"iso":     now.UTC().Format(time.RFC3339),
		}}, nil
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "test: unknown verb "+req.Verb)
}

func (testPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	interval := durationOf(cfg["interval"], 2*time.Second)
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	maxTicks := intOr(cfg["count"], 0)
	message := str(cfg["message"])
	labels := strList(cfg["labels"])

	fmt.Fprintf(os.Stderr, "test[%s]: ticking every %s (count=%d labels=%v)\n", req.Instance, interval, maxTicks, labels)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var n int
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n++
			label := ""
			if len(labels) > 0 {
				label = labels[(n-1)%len(labels)]
			}
			_ = emit(tickEvent(n, label, message))
			if maxTicks > 0 && n >= maxTicks {
				fmt.Fprintf(os.Stderr, "test[%s]: reached count=%d, stopping\n", req.Instance, maxTicks)
				return nil
			}
		}
	}
}

func main() {
	if err := plugin.Serve(testPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-test: %v\n", err)
		os.Exit(1)
	}
}

// tickEvent builds the emit payload for one synthetic tick. Pure and testable.
// `labels` is a singular-label alias so filters: {labels: [...]} matches the
// daemon's list-contains evaluator.
func tickEvent(n int, label, message string) map[string]any {
	return map[string]any{
		"event": "tick",
		"kind":  "tick",
		"title": fmt.Sprintf("tick %d", n),
		"context": map[string]any{
			"n":       n,
			"label":   label,
			"message": message,
			"time":    time.Now().Unix(),
			"labels":  label, // singular alias for list-contains filtering
		},
	}
}

// randomOutputs builds the `random` verb's reply from crypto/rand.
func randomOutputs(max int) map[string]any {
	var b [16]byte
	_, _ = rand.Read(b[:])
	uuid := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	n := 0
	if max > 0 {
		// 8 bytes -> uint64, then bound. Reuse the first 8 bytes.
		var u uint64
		for i := 0; i < 8; i++ {
			u = u<<8 | uint64(b[i])
		}
		n = int(u % uint64(max))
	}
	return map[string]any{"uuid": uuid, "hex": hex.EncodeToString(b[:]), "int": n}
}

func failCode(s string) int {
	if s == "invalid_params" {
		return plugin.CodeInvalidParams
	}
	return plugin.CodeInternalError
}

// --- option helpers ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func boolv(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	}
	return false
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

func intOr(v any, d int) int {
	if n, ok := toInt(v); ok {
		return n
	}
	return d
}

// durationOf accepts a Go duration string ("500ms", "2s"), a number of seconds
// (int/float), or falls back to d.
func durationOf(v any, d time.Duration) time.Duration {
	switch x := v.(type) {
	case string:
		if x == "" {
			return d
		}
		if parsed, err := time.ParseDuration(x); err == nil {
			return parsed
		}
		return d
	case int:
		return time.Duration(x) * time.Second
	case int64:
		return time.Duration(x) * time.Second
	case float64:
		return time.Duration(x * float64(time.Second))
	}
	return d
}

func strList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
