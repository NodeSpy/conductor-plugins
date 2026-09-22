// Command conductor-redis is the Redis / Valkey connector as an external
// conductor plugin. It exposes key/pub-sub verbs (get, set, del, incr, expire,
// publish, and a raw `command` escape hatch) and one source: a live pub/sub
// SUBSCRIBE / PSUBSCRIBE that emits one event per received message. Built on
// redis/go-redis/v9 (pure Go), which speaks the same protocol to Redis and to
// its Valkey fork — this connector works with either.
//
// Connection (used for both Invoke and StartSource):
//
//	url: "redis://:pass@redis.local:6379/0"  # redis:// or rediss:// (TLS)
//	# or, instead of url:
//	address: "redis.local:6379"
//	username: "default"
//	password: "<pass>"
//	db: 0
//	insecure_skip_verify: false              # skip TLS verify for rediss:// self-signed
//	subscribe: ["events"]                    # channels to SUBSCRIBE (StartSource only)
//	psubscribe: ["cache:*"]                  # patterns to PSUBSCRIBE (StartSource only)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type redisPlugin struct{}

func (redisPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "redis",
		Desc: "Redis / Valkey: key verbs (get/set/del/incr/expire), publish, and a raw command escape hatch, plus a live pub/sub source (SUBSCRIBE / PSUBSCRIBE) that emits one event per message.",
		Connection: plugin.Schema{
			"url":                  {Type: "string", Desc: "connection URL: redis://[user:pass@]host:port/db or rediss:// for TLS (takes precedence over the fields below)"},
			"address":              {Type: "string", Desc: "host:port (when not using url; default localhost:6379)"},
			"username":             {Type: "string", Desc: "ACL username (Redis 6+)"},
			"password":             {Type: "string", Desc: "password / ACL secret"},
			"db":                   {Type: "integer", Desc: "database number (default 0)"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS verification for rediss:// with a self-signed cert"},
			"subscribe":            {Type: "list", Desc: "channels to SUBSCRIBE to (StartSource only)"},
			"psubscribe":           {Type: "list", Desc: "glob patterns to PSUBSCRIBE to, e.g. cache:* (StartSource only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "get", Desc: "get a key's value",
				Options: plugin.Schema{"key": {Type: "string", Required: true, Scope: "key"}},
				Outputs: plugin.Schema{"value": {Type: "string"}, "found": {Type: "boolean", Desc: "false when the key does not exist"}},
			},
			{
				Name: "set", Desc: "set a key's value, optionally with a TTL",
				Options: plugin.Schema{
					"key":   {Type: "string", Required: true, Scope: "key"},
					"value": {Type: "string", Required: true},
					"ttl":   {Type: "integer", Desc: "expiry in seconds (0 / omitted = no expiry)"},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "del", Desc: "delete one or more keys",
				Options: plugin.Schema{"keys": {Type: "list", Required: true, Scope: "key", Desc: "keys to delete (a single key string is accepted too)"}},
				Outputs: plugin.Schema{"deleted": {Type: "integer", Desc: "number of keys removed"}},
			},
			{
				Name: "incr", Desc: "atomically increment a key (creating it at 0 first)",
				Options: plugin.Schema{"key": {Type: "string", Required: true, Scope: "key"}},
				Outputs: plugin.Schema{"value": {Type: "integer", Desc: "the value after incrementing"}},
			},
			{
				Name: "expire", Desc: "set a key's TTL in seconds",
				Options: plugin.Schema{
					"key": {Type: "string", Required: true, Scope: "key"},
					"ttl": {Type: "integer", Required: true, Desc: "expiry in seconds"},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean", Desc: "false when the key does not exist"}},
			},
			{
				Name: "publish", Desc: "publish a message to a pub/sub channel",
				Options: plugin.Schema{
					"channel": {Type: "string", Required: true, Scope: "channel"},
					"message": {Type: "string", Required: true},
				},
				Outputs: plugin.Schema{"receivers": {Type: "integer", Desc: "number of clients that received the message"}},
			},
			{
				Name: "command", Desc: "run an arbitrary Redis command (escape hatch)",
				Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "the command and its arguments, e.g. [\"LPUSH\", \"q\", \"job1\"]"}},
				Outputs: plugin.Schema{"result": {Type: "any", Desc: "the raw reply"}},
			},
		},
		Events: []plugin.Event{
			{
				Name: "message",
				Desc: "a message was received on a subscribed channel or pattern",
				Context: plugin.Schema{
					"channel": {Type: "string", Desc: "the concrete channel the message arrived on"},
					"payload": {Type: "string"},
					"pattern": {Type: "string", Desc: "the PSUBSCRIBE pattern that matched (empty for a plain SUBSCRIBE)"},
				},
				Filters: plugin.Schema{
					"channels": {Type: "list", Desc: "match any of these exact channels"},
					"channel":  {Type: "string"},
				},
			},
		},
		// The server host is operator-specific (config), so no fixed egress is
		// declared — narrow it per instance with network:. Spawns nothing.
		Capabilities: plugin.Capabilities{Spawns: false},
	}
}

func (redisPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	client, err := newClient(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "redis: "+err.Error())
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	fail := func(e error) (plugin.InvokeResult, error) {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+e.Error())
	}

	switch req.Verb {
	case "get":
		v, err := client.Get(ctx, str(o["key"])).Result()
		if errors.Is(err, redis.Nil) {
			return plugin.InvokeResult{Outputs: map[string]any{"value": "", "found": false}}, nil
		}
		if err != nil {
			return fail(err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"value": v, "found": true}}, nil

	case "set":
		if err := client.Set(ctx, str(o["key"]), str(o["value"]), ttlDur(o["ttl"])).Err(); err != nil {
			return fail(err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"ok": true}}, nil

	case "del":
		keys := strList(o["keys"])
		if len(keys) == 0 {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "del: keys is required")
		}
		n, err := client.Del(ctx, keys...).Result()
		if err != nil {
			return fail(err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"deleted": n}}, nil

	case "incr":
		n, err := client.Incr(ctx, str(o["key"])).Result()
		if err != nil {
			return fail(err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"value": n}}, nil

	case "expire":
		ok, err := client.Expire(ctx, str(o["key"]), ttlDur(o["ttl"])).Result()
		if err != nil {
			return fail(err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"ok": ok}}, nil

	case "publish":
		n, err := client.Publish(ctx, str(o["channel"]), str(o["message"])).Result()
		if err != nil {
			return fail(err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"receivers": n}}, nil

	case "command":
		args := anyList(o["args"])
		if len(args) == 0 {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "command: args is required")
		}
		res, err := client.Do(ctx, args...).Result()
		if errors.Is(err, redis.Nil) {
			return plugin.InvokeResult{Outputs: map[string]any{"result": nil}}, nil
		}
		if err != nil {
			return fail(err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"result": res}}, nil
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "redis: unknown verb "+req.Verb)
}

func (redisPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	client, err := newClient(req.Config)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer client.Close()

	channels := strList(req.Config["subscribe"])
	patterns := strList(req.Config["psubscribe"])
	if len(channels) == 0 && len(patterns) == 0 {
		return fmt.Errorf("redis: subscribe or psubscribe must name at least one channel/pattern")
	}

	sub := client.Subscribe(ctx, channels...)
	if len(patterns) > 0 {
		if err := sub.PSubscribe(ctx, patterns...); err != nil {
			return fmt.Errorf("redis: psubscribe: %w", err)
		}
	}
	defer sub.Close()
	fmt.Fprintf(os.Stderr, "redis[%s]: subscribed channels=%v patterns=%v\n", req.Instance, channels, patterns)

	// go-redis's Channel() health-checks and reconnects the subscription
	// internally, so a plain range over it is the whole source loop.
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			_ = emit(messageEvent(msg.Channel, msg.Payload, msg.Pattern))
		}
	}
}

func main() {
	if err := plugin.Serve(redisPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-redis: %v\n", err)
		os.Exit(1)
	}
}

// messageEvent builds the emit payload for one pub/sub message: the event name
// plus the channel/payload/pattern context the daemon filters and templates on.
// Pure and testable. `channels` is a singular alias so filters: {channels: [...]}
// matches the daemon's list-contains evaluator.
func messageEvent(channel, payload, pattern string) map[string]any {
	return map[string]any{
		"event": "message",
		"kind":  "message",
		"title": "redis: " + channel,
		"context": map[string]any{
			"channel":  channel,
			"payload":  payload,
			"pattern":  pattern,
			"channels": channel, // singular alias for list-contains filtering
		},
	}
}

// newClient builds a *redis.Client from the connection map: a url takes
// precedence; otherwise address/username/password/db are used. rediss:// (or an
// explicit insecure_skip_verify) enables TLS.
func newClient(cfg map[string]any) (*redis.Client, error) {
	var opt *redis.Options
	if u := str(cfg["url"]); u != "" {
		parsed, err := redis.ParseURL(u)
		if err != nil {
			return nil, fmt.Errorf("parse url: %w", err)
		}
		opt = parsed
	} else {
		opt = &redis.Options{
			Addr:     strOr(cfg["address"], "localhost:6379"),
			Username: str(cfg["username"]),
			Password: str(cfg["password"]),
			DB:       intOr(cfg["db"], 0),
		}
	}
	if boolv(cfg["insecure_skip_verify"]) {
		if opt.TLSConfig == nil {
			opt.TLSConfig = &tls.Config{} //nolint:gosec // populated on the next line
		}
		opt.TLSConfig.InsecureSkipVerify = true //nolint:gosec // opt-in, documented, for self-signed servers
	}
	return redis.NewClient(opt), nil
}

// ttlDur turns a seconds option into a time.Duration; 0 / missing means "no
// expiry" (redis treats a zero Duration as persistent).
func ttlDur(v any) time.Duration {
	if n, ok := toInt(v); ok && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
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

func anyList(v any) []any {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		return x
	default:
		return []any{x}
	}
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
			} else if e != nil {
				out = append(out, strings.TrimSpace(fmt.Sprintf("%v", e)))
			}
		}
		return out
	}
	return nil
}
