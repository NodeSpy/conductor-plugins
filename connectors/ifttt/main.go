// Command conductor-ifttt is the IFTTT Maker Webhooks connector as a
// standalone external conductor plugin (#59). As a set of VERBS, it fires
// Maker Webhooks events (https://ifttt.com/maker_webhooks) over HTTPS —
// `trigger` for the classic value1/value2/value3 shape, `trigger_json` for an
// arbitrary JSON body. As a SOURCE, it receives inbound webhook POSTs from an
// IFTTT applet's "Make a web request" action and streams a normalized `event`
// event per delivery, guarded by a shared token (not an HMAC — Maker Webhooks
// has no signing story) checked against the `X-Conductor-Token` header or a
// `?token=` query parameter. The shared sourcekit.Listener.ServeReq hands the
// callback the full request (headers, query, body), so the ?token= query case
// is checked directly — and the same Listener transparently accepts
// deliveries over a smee.io-style relay (webhook.smee) for endpoints with no
// public URL.
//
// Built ONLY against the public SDK (pkg/plugin) and the connector-kit
// (pkg/sourcekit) — no other internal daemon package.
//
// Connection (verbs):
//
//	key: "<maker-key>"                  # required: your IFTTT Maker Webhooks key
//	base_url: "https://maker.ifttt.com" # override (GHES-style: tests / self-hosted proxies)
//
// Connection (source; delivered per start_source):
//
//	webhook:
//	  listen: ":9100"                   # HTTP listener address
//	  path: "/ifttt"                    # request path (default /ifttt)
//	  secret: "<shared token>"          # required unless allow_unsigned
//	  allow_unsigned: false             # explicit opt-out of the shared-token check
//	  smee: "<smee.io channel URL>"     # optional relay for endpoints with no public URL
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// defaultBaseURL is the real IFTTT Maker Webhooks endpoint. Tests override it
// via connection.base_url to point at an httptest.Server; nothing here ever
// hits the network in a test.
const defaultBaseURL = "https://maker.ifttt.com"

type iftttPlugin struct{}

func (iftttPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "ifttt",
		Desc: "IFTTT Maker Webhooks: fire applet triggers (trigger/trigger_json) and receive applet-posted webhook events as a source.",
		Connection: plugin.Schema{
			"key":      {Type: "string", Required: true, Desc: "IFTTT Maker Webhooks key"},
			"base_url": {Type: "string", Desc: "override the Maker Webhooks base URL (default https://maker.ifttt.com; for tests)"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path (default /ifttt), secret, allow_unsigned, smee"},
		},
		Events: []plugin.Event{
			{
				Name: "event",
				Desc: "an IFTTT applet posted a webhook event",
				Filters: plugin.Schema{
					"events": {Type: "list", Desc: "event names to match (empty = any)"},
					"event":  {Type: "string", Desc: "scalar alias of events"},
				},
				Context: plugin.Schema{
					"event":   {Type: "string", Desc: "the event name, when the posted JSON carries one"},
					"payload": {Type: "map", Desc: "the full parsed JSON payload (when the body was JSON)"},
					"body":    {Type: "string", Desc: "the raw request body (when the body was not JSON)"},
				},
			},
		},
		Verbs: []plugin.Verb{
			{
				Name: "trigger", Desc: "fire an IFTTT Maker Webhooks event with up to three values",
				Usage: "the classic Maker Webhooks shape: value1/value2/value3 map straight to the applet's Ingredients",
				Options: plugin.Schema{
					"event":  {Type: "string", Required: true, Desc: "the Maker Webhooks event name"},
					"value1": {Type: "string"},
					"value2": {Type: "string"},
					"value3": {Type: "string"},
				},
				Outputs: plugin.Schema{"result": {Type: "string"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "trigger_json", Desc: "fire an IFTTT Maker Webhooks event with an arbitrary JSON body",
				Usage: "use when your applet's ingredients don't fit value1/value2/value3",
				Options: plugin.Schema{
					"event": {Type: "string", Required: true, Desc: "the Maker Webhooks event name"},
					"data":  {Type: "map", Required: true, Desc: "posted as the request's JSON body verbatim"},
				},
				Outputs: plugin.Schema{"result": {Type: "string"}, "status_code": {Type: "integer"}},
			},
		},
		// The permission manifest conductor records at install and confines
		// this plugin to: the Maker Webhooks host, and nothing else. It spawns
		// no commands.
		Capabilities: plugin.Capabilities{Egress: []string{"maker.ifttt.com:443"}},
	}
}

func (iftttPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	key := str(req.Connection["key"])
	if key == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.key is required")
	}
	base := strOr(req.Connection["base_url"], defaultBaseURL)

	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	event := str(o["event"])
	if event == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": event is required")
	}

	var path string
	var body map[string]any
	switch req.Verb {
	case "trigger":
		path = fmt.Sprintf("/trigger/%s/with/key/%s", url.PathEscape(event), url.PathEscape(key))
		body = map[string]any{}
		if v, ok := valueStr(o["value1"]); ok {
			body["value1"] = v
		}
		if v, ok := valueStr(o["value2"]); ok {
			body["value2"] = v
		}
		if v, ok := valueStr(o["value3"]); ok {
			body["value3"] = v
		}
	case "trigger_json":
		data, ok := o["data"].(map[string]any)
		if !ok || len(data) == 0 {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "trigger_json: data is required")
		}
		path = fmt.Sprintf("/trigger/%s/json/with/key/%s", url.PathEscape(event), url.PathEscape(key))
		body = data
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}

	return doTrigger(base, path, body)
}

// doTrigger POSTs the JSON body to base+path and translates the response into
// the verb's outputs, or a CodeInternalError carrying the status and body on
// any non-2xx response.
func doTrigger(base, path string, body map[string]any) (plugin.InvokeResult, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	httpReq, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("ifttt: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))))
	}

	return plugin.InvokeResult{Outputs: map[string]any{
		"result":      strings.TrimSpace(string(respBody)),
		"status_code": resp.StatusCode,
	}}, nil
}

func (iftttPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	addr, path, secret, allowUnsigned, smeeURL := "", "/ifttt", "", false, ""
	if webhook != nil {
		addr = str(webhook["listen"])
		if p := str(webhook["path"]); p != "" {
			path = p
		}
		secret = str(webhook["secret"])
		allowUnsigned = boolv(webhook["allow_unsigned"])
		smeeURL = str(webhook["smee"])
	}
	if addr == "" && smeeURL == "" {
		return fmt.Errorf("ifttt: no webhook.listen address or webhook.smee relay configured")
	}
	if err := requireWebhookSecret("ifttt", secret, allowUnsigned); err != nil {
		return err
	}

	dedup := sourcekit.NewDedup(4096)
	if addr != "" {
		fmt.Fprintf(os.Stderr, "ifttt[%s]: listening on %s%s\n", req.Instance, addr, path)
	}
	if smeeURL != "" {
		fmt.Fprintf(os.Stderr, "ifttt[%s]: relaying via smee channel %s\n", req.Instance, smeeURL)
	}

	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smeeURL}
	return ln.ServeReq(ctx, func(rq *sourcekit.Request) {
		if !checkToken(rq, secret, allowUnsigned) {
			return
		}
		ev := parseInbound(rq.Body)
		if ev.Dedup == "" || dedup.Add(ev.Dedup) {
			_ = emit(ev)
		}
	})
}

// checkToken verifies the shared token conveyed by the IFTTT applet: the
// `X-Conductor-Token` header, falling back to a `?token=` query parameter
// (Maker Webhooks' web-request action can set a custom header OR only a URL,
// depending on the applet), compared in constant time against webhook.secret.
//
// A secret was required unless allow_unsigned — requireWebhookSecret already
// refused to start the listener otherwise — so an empty secret here only
// happens when the operator explicitly opted into accepting everything.
func checkToken(rq *sourcekit.Request, secret string, allowUnsigned bool) bool {
	if secret == "" {
		return allowUnsigned
	}
	got := rq.Header.Get("X-Conductor-Token")
	if got == "" {
		got = rq.Query.Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// wireEvent is the normalized event streamed to the daemon's plugin source
// adapter.
type wireEvent struct {
	Event   string         `json:"event"`
	Kind    string         `json:"kind,omitempty"`
	Title   string         `json:"title,omitempty"`
	Context map[string]any `json:"context,omitempty"`
	Dedup   string         `json:"dedup,omitempty"`
}

// parseInbound builds the event context from one webhook delivery: the posted
// JSON's top-level keys, spread into context, plus a `payload` key holding the
// whole parsed object — or, when the body isn't JSON, a `body` string key
// holding the raw text. Deliveries carrying a top-level `id` dedup on it.
func parseInbound(body []byte) wireEvent {
	ctx := map[string]any{}
	var evName, dedupKey string

	var top map[string]any
	if err := json.Unmarshal(body, &top); err == nil && top != nil {
		for k, v := range top {
			ctx[k] = v
		}
		ctx["payload"] = top
		evName = gs(top, "event")
		if evName != "" {
			ctx["events"] = evName
		}
		dedupKey = idString(top["id"])
	} else {
		ctx["body"] = string(body)
	}

	title := "ifttt event"
	if evName != "" {
		title = "ifttt event: " + evName
	}
	return wireEvent{
		Event:   "event",
		Kind:    "event",
		Title:   title,
		Context: ctx,
		Dedup:   dedupKey,
	}
}

func gs(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

// idString renders a JSON `id` field (string or number) as a dedup key; any
// other shape (missing, object, bool) yields "" — no dedup for that delivery.
func idString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func main() {
	if err := plugin.Serve(iftttPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-ifttt:", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) ---

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

// valueStr renders a trigger value option as a string, reporting whether it
// should be included at all: nil and "" are omitted (the "omit empty"
// contract for value1/value2/value3), everything else — including "0" or
// "false" — is kept.
func valueStr(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return "", false
		}
		return s, true
	}
	return fmt.Sprintf("%v", v), true
}

// requireWebhookSecret refuses to start an unauthenticated webhook listener.
//
// Maker Webhooks has no signing story — there is no HMAC to verify — so the
// only authentication available is a shared token the operator picks and the
// applet is configured to send back (as a header or a query parameter). A
// missing secret is far more often a mistake than a choice, so it fails
// closed; `webhook.allow_unsigned: true` is the explicit, greppable way to say
// you meant it.
func requireWebhookSecret(who, secret string, allowUnsigned bool) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintf(os.Stderr, "%s: allow_unsigned is set — accepting UNSIGNED webhooks; anyone who can reach the listen address can fire triggers\n", who)
		return nil
	}
	return fmt.Errorf("%s: no webhook secret configured (webhook.secret) — an unsigned listener accepts any POST on the listen address as a real event. Set it, or set `webhook.allow_unsigned: true` if you genuinely front this with something else that authenticates", who)
}
