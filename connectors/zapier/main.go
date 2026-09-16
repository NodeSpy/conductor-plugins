// Command conductor-zapier is the Zapier connector as a standalone external
// conductor plugin (#59). Zapier is a generic automation hub rather than a
// single API, so the connector surface mirrors that: as a VERB, `send` POSTs a
// JSON body to a Zapier "Catch Hook" webhook URL, which is how a conductor
// trigger kicks off a Zap; as a SOURCE, it receives the JSON a Zap's own
// "Webhooks by Zapier" action posts back to conductor, and streams it as a
// single normalized "event" so a conductor trigger can react to whatever the
// Zap produced.
//
// Built ONLY against the public SDK (pkg/plugin) and the connector-kit
// (pkg/sourcekit) — no other internal daemon package, no third-party
// dependency.
//
// Connection (used for both Invoke and StartSource):
//
//	hook_url: "https://hooks.zapier.com/hooks/catch/.../..."  # default for `send`
//	webhook:
//	  listen: ":9096"                    # HTTP listener address (StartSource only)
//	  path: "/zapier"                    # request path (default /zapier)
//	  secret: "<shared token>"           # compared to X-Conductor-Token / ?token=
//	  allow_unsigned: false              # explicitly accept an unauthenticated listener
//	  smee: "https://smee.io/xyz"        # optional smee.io-style SSE relay, for
//	                                     # when the listener has no public URL
//
// Zapier's inbound "Webhooks by Zapier" action cannot compute an HMAC
// signature — it can only POST a JSON body, optionally with a custom header
// or query string an operator types into the Zap's URL/header fields. So,
// like Datadog, authentication here is a shared token compared in constant
// time against either the X-Conductor-Token header or a `?token=` query
// parameter, never an HMAC. The shared sourcekit.Listener.ServeReq hands the
// callback the full request (headers, query, body), so the ?token= query
// case is checked directly — and the same Listener transparently accepts
// deliveries relayed over webhook.smee. See requireWebhookSecret and
// verifyToken below.
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
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// zapierHookHost is the only host `send` is willing to POST to. Zapier's
// Catch Hook URLs always live under hooks.zapier.com; refusing anything else
// keeps a misconfigured or hijacked `hook_url` from turning this connector
// into an open POST-to-anywhere relay.
const zapierHookHost = "hooks.zapier.com"

type zapierPlugin struct{}

func (zapierPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "zapier",
		Desc: "Zapier: POST a JSON body to a Catch Hook webhook to kick off a Zap (`send`); receive the JSON a Zap's Webhooks-by-Zapier action posts back as a source event.",
		Connection: plugin.Schema{
			"hook_url": {Type: "string", Desc: "default Catch Hook URL used by `send` when the verb's own hook_url option is omitted; must be a hooks.zapier.com URL"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path (default /zapier), secret, allow_unsigned, smee"},
		},
		Events: []plugin.Event{
			{
				Name: "event",
				Desc: "a Zap's Webhooks-by-Zapier action posted its output back to conductor",
				// Dynamic: a Zap's payload shape is entirely operator-defined —
				// there is no fixed schema to declare up front. Every top-level
				// key of the posted JSON is flattened straight into context, in
				// addition to the full object under context.payload.
				Dynamic: true,
				Filters: plugin.Schema{
					"events": {Type: "list", Desc: "list-contains match against the posted payload's `events` key, if present (empty = any)"},
					"event":  {Type: "string", Desc: "exact match against the posted payload's `event` key, if present (empty = any)"},
				},
				Context: plugin.Schema{
					"payload": {Type: "any", Desc: "the full posted JSON object (also flattened into context's other keys)"},
					"body":    {Type: "string", Desc: "the raw request body, set instead of payload when it was not a JSON object"},
				},
			},
		},
		Verbs: []plugin.Verb{
			{
				Name: "send", Desc: "POST a JSON body to a Zapier Catch Hook webhook",
				Usage: "kick off a Zap; hook_url defaults to connection.hook_url when omitted",
				Options: plugin.Schema{
					"hook_url": {Type: "string", Desc: "overrides connection.hook_url for this call; must be a hooks.zapier.com URL"},
					"body":     {Type: "any", Required: true, Desc: "JSON body to POST — a map, a list, or a bare string"},
				},
				Outputs: plugin.Schema{
					"status_code": {Type: "integer"},
					"result":      {Type: "any", Desc: "the response body, parsed as JSON if it was valid JSON"},
				},
			},
		},
		Capabilities: plugin.Capabilities{Egress: []string{zapierHookHost + ":443"}},
	}
}

func (zapierPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	if req.Verb != "send" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	hookURL := str(o["hook_url"])
	if hookURL == "" {
		hookURL = str(req.Connection["hook_url"])
	}
	if hookURL == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "hook_url is required (set connection.hook_url or options.hook_url)")
	}
	if err := validateHookURL(hookURL); err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}

	body, present := o["body"]
	if !present {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "body is required")
	}

	return postToHook(hookURL, body)
}

// validateHookURL refuses any hook_url whose host is not hooks.zapier.com.
// Without this, `send` would happily POST an arbitrary (potentially
// attacker-supplied) body to an arbitrary host, which is exactly the SSRF
// shape a `network:`-scoped egress declaration exists to prevent — the
// declared capability (hooks.zapier.com:443) only means something if the
// verb itself enforces it too.
func validateHookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("hook_url: invalid URL: %w", err)
	}
	if !strings.EqualFold(u.Hostname(), zapierHookHost) {
		return fmt.Errorf("hook_url must be a %s URL (got host %q) — refusing to POST to an untrusted host", zapierHookHost, u.Hostname())
	}
	return nil
}

// postToHook issues the actual HTTP POST — split out from Invoke so tests can
// drive it directly against an httptest.Server without going through the
// hooks.zapier.com host check.
func postToHook(hookURL string, body any) (plugin.InvokeResult, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "encoding body: "+err.Error())
	}
	httpReq, err := http.NewRequest(http.MethodPost, hookURL, bytes.NewReader(b))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "reading response: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("zapier send: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
	}
	out := map[string]any{"status_code": resp.StatusCode}
	if len(bytes.TrimSpace(raw)) > 0 {
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err == nil {
			out["result"] = parsed
		}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// --- source: inbound Zap webhook ---

func (zapierPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	addr, path, secret, smeeURL := "", "/zapier", "", ""
	allowUnsigned := false
	if webhook != nil {
		addr = str(webhook["listen"])
		if p := str(webhook["path"]); p != "" {
			path = p
		}
		secret = str(webhook["secret"])
		allowUnsigned, _ = webhook["allow_unsigned"].(bool)
		smeeURL = str(webhook["smee"])
	}
	if addr == "" && smeeURL == "" {
		return fmt.Errorf("zapier: no webhook.listen address or webhook.smee relay configured")
	}
	if err := requireWebhookSecret("zapier", secret, allowUnsigned, "webhook.secret"); err != nil {
		return err
	}
	dedup := sourcekit.NewDedup(2048)
	if addr != "" {
		fmt.Fprintf(os.Stderr, "zapier[%s]: listening on %s%s\n", req.Instance, addr, path)
	}
	if smeeURL != "" {
		fmt.Fprintf(os.Stderr, "zapier[%s]: relaying via smee channel %s\n", req.Instance, smeeURL)
	}
	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smeeURL}
	return ln.ServeReq(ctx, func(rq *sourcekit.Request) {
		if secret != "" && !verifyToken(secret, rq) {
			return
		}
		if ev, ok := parseEvent(rq.Body); ok {
			if dk, _ := ev["dedup"].(string); dk == "" || dedup.Add(dk) {
				_ = emit(ev)
			}
		}
	})
}

// verifyToken compares the request's X-Conductor-Token header (falling back
// to the `?token=` query parameter when the header is absent) against secret
// in constant time. Zapier's inbound webhook action has no way to attach an
// HMAC signature — a shared token pasted into the Zap's header or URL field
// is the only authentication a Zap can produce, so this (not
// sourcekit.VerifyHMAC) is the whole check; requireWebhookSecret ensures a
// token is always configured unless the operator explicitly opts out.
func verifyToken(secret string, rq *sourcekit.Request) bool {
	got := rq.Header.Get("X-Conductor-Token")
	if got == "" {
		got = rq.Query.Get("token")
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// parseEvent builds the emitted event map from one webhook delivery. A valid
// JSON object's top-level keys are flattened directly into context (so
// `filters: { events: [...] }` matches the daemon's generic list-contains
// filter evaluator against a `events` key the Zap posted) alongside a full
// copy under context.payload; a non-JSON-object body is carried as
// context.body instead. Reports false for an empty delivery worth ignoring.
func parseEvent(body []byte) (map[string]any, bool) {
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		s := strings.TrimSpace(string(body))
		if s == "" {
			return nil, false
		}
		return map[string]any{
			"event":   "event",
			"kind":    "event",
			"title":   "zapier webhook",
			"context": map[string]any{"body": s},
		}, true
	}
	if len(top) == 0 {
		return nil, false
	}
	context := make(map[string]any, len(top)+1)
	payload := make(map[string]any, len(top))
	for k, v := range top {
		context[k] = v
		payload[k] = v
	}
	context["payload"] = payload

	title := "zapier event"
	if t, ok := top["title"].(string); ok && t != "" {
		title = t
	}
	ev := map[string]any{
		"event":   "event",
		"kind":    "event",
		"title":   title,
		"context": context,
	}
	if id, ok := top["id"]; ok {
		if dk := fmt.Sprintf("%v", id); dk != "" {
			ev["dedup"] = dk
		}
	}
	return ev, true
}

// requireWebhookSecret refuses to start an unauthenticated webhook listener.
//
// Zapier cannot sign its webhook deliveries (Webhooks by Zapier only POSTs a
// JSON body), so the ONLY authentication available is a shared token the
// operator embeds in the Zap's URL or a custom header. Treating a missing
// token the same way the HMAC-based connectors treat a missing signing
// secret keeps the fail-closed posture consistent: a missing token is far
// more often a mistake than a choice, so it fails closed; `allow_unsigned:
// true` is the explicit, greppable way to say you meant it (e.g. the
// listener sits behind something else that authenticates).
func requireWebhookSecret(who, secret string, allowUnsigned bool, allowKey string) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintf(os.Stderr, "%s: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers\n", who)
		return nil
	}
	return fmt.Errorf("%s: no webhook token configured (%s) — Zapier cannot sign webhooks, so an unauthenticated listener accepts any POST on the listen address as a real event. Set it, or set `allow_unsigned: true` if you genuinely front this with something else that authenticates", who, allowKey)
}

func str(v any) string { s, _ := v.(string); return s }

func main() {
	if err := plugin.Serve(zapierPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-zapier: %v\n", err)
		os.Exit(1)
	}
}
