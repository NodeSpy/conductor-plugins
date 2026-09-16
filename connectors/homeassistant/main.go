// Command conductor-homeassistant is the Home Assistant connector as a
// standalone external conductor plugin (#59). It drives a Home Assistant
// instance's REST API (call_service, states, events, templates, config,
// history, logbook, and a raw `api` escape hatch) over a long-lived token,
// and — as a source — receives inbound webhook POSTs from a Home Assistant
// automation (the `rest_command`/webhook trigger side of an automation),
// verifying a shared token before treating the payload as a real event.
//
// Built ONLY against the public SDK (pkg/plugin) and the connector-kit
// (pkg/sourcekit) — no other internal daemon package, no third-party
// dependency.
//
// Connection (used for both Invoke and StartSource):
//
//	base_url: "http://homeassistant.local:8123"  # required
//	token:    "<long-lived access token>"         # required
//	webhook:
//	  listen:        ":9097"          # HTTP listener address (StartSource only)
//	  path:          "/homeassistant" # request path (default /homeassistant)
//	  secret:        "<shared token>" # compared against X-Conductor-Token / ?token=
//	  allow_unsigned: false           # explicitly accept unverified deliveries
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
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type haPlugin struct{}

func (haPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "homeassistant",
		Desc: "Home Assistant: call services, read/set state, fire events, render templates, and read history/logbook over the REST API; an inbound webhook (HA automation POST) is the source.",
		Connection: plugin.Schema{
			"base_url": {Type: "string", Required: true, Desc: "Home Assistant base URL, e.g. http://homeassistant.local:8123 (the REST API is served at base_url + /api)"},
			"token":    {Type: "string", Required: true, Desc: "long-lived access token, sent as Authorization: Bearer <token>"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path (default /homeassistant), secret, allow_unsigned"},
		},
		Events: []plugin.Event{
			{
				Name: "event",
				Desc: "a Home Assistant automation posted a webhook payload",
				Filters: plugin.Schema{
					"event_types": {Type: "list", Desc: "match against the payload's event_type (empty = any)"},
					"event_type":  {Type: "string", Desc: "scalar alias of event_types"},
				},
				Context: plugin.Schema{
					"event_type": {Type: "string", Desc: "the payload's event_type key, if present"},
					"payload":    {Type: "map", Desc: "the full posted JSON body"},
				},
				// The payload shape is whatever the operator's automation posts —
				// there is no fixed schema beyond event_type, so the context is
				// declared dynamic rather than enumerated field by field.
				Dynamic: true,
			},
		},
		Verbs: haVerbs(),
		// Egress is operator-specific (the Home Assistant instance's own
		// base_url/host, which varies per install) and is documented rather
		// than declared, so the manifest makes no claim here.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func haVerbs() []plugin.Verb {
	return []plugin.Verb{
		{
			Name: "call_service", Desc: "call a Home Assistant service (e.g. light.turn_on)",
			Options: plugin.Schema{
				"domain":    {Type: "string", Required: true, Desc: "service domain, e.g. light"},
				"service":   {Type: "string", Required: true, Desc: "service name, e.g. turn_on"},
				"entity_id": {Type: "any", Desc: "target entity id, or a list of entity ids"},
				"data":      {Type: "map", Desc: "additional service data, merged with entity_id"},
			},
			Outputs: plugin.Schema{"result": {Type: "list", Desc: "changed entity states"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "get_state", Desc: "read one entity's current state",
			Options: plugin.Schema{"entity_id": {Type: "string", Required: true}},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name:    "list_states",
			Desc:    "list every entity's current state",
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "set_state", Desc: "set (or create) an entity's state",
			Options: plugin.Schema{
				"entity_id":  {Type: "string", Required: true},
				"state":      {Type: "string", Required: true},
				"attributes": {Type: "map"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "fire_event", Desc: "fire a Home Assistant event bus event",
			Options: plugin.Schema{
				"event_type": {Type: "string", Required: true},
				"data":       {Type: "map"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "render_template", Desc: "render a Home Assistant Jinja2 template",
			Options: plugin.Schema{"template": {Type: "string", Required: true}},
			Outputs: plugin.Schema{"text": {Type: "string"}, "status_code": {Type: "integer"}},
		},
		{
			Name:    "get_services",
			Desc:    "list every domain's available services",
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name:    "get_config",
			Desc:    "read the running Home Assistant configuration",
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "history", Desc: "state history for a period",
			Options: plugin.Schema{
				"entity_id": {Type: "string", Desc: "filter to one entity (filter_entity_id)"},
				"start":     {Type: "string", Desc: "period start, ISO-8601 (path segment; default: 1 day before end)"},
				"end":       {Type: "string", Desc: "period end, ISO-8601 (end_time)"},
			},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "logbook", Desc: "logbook entries for a period",
			Options: plugin.Schema{
				"entity_id": {Type: "string", Desc: "filter to one entity"},
				"start":     {Type: "string", Desc: "period start, ISO-8601 (path segment)"},
				"end":       {Type: "string", Desc: "period end, ISO-8601 (end_time)"},
			},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "api", Desc: "raw escape hatch: call any Home Assistant REST API path",
			Usage: "for anything without a first-class verb; path is relative to /api",
			Options: plugin.Schema{
				"method": {Type: "string", Enum: []string{"GET", "POST", "PUT", "DELETE"}, Desc: "default GET"},
				"path":   {Type: "string", Required: true, Desc: "path relative to /api, e.g. /states/sensor.foo"},
				"query":  {Type: "map", Desc: "query string parameters"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
	}
}

// httpClient is shared across invocations; Home Assistant instances are
// typically reached over a LAN or a reverse proxy, so a generous timeout
// tolerates a slow automation-heavy install without hanging forever.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// baseURL resolves the REST API base for one connection: base_url + "/api",
// unless api_base is set — an undocumented override so tests can point the
// client at an httptest.Server without touching base_url's public meaning.
func baseURL(conn map[string]any) string {
	if b := str(conn["api_base"]); b != "" {
		return strings.TrimRight(b, "/")
	}
	return strings.TrimRight(str(conn["base_url"]), "/") + "/api"
}

// do issues one Home Assistant REST API call and returns the raw response
// body and status code. A non-2xx status is returned as an error carrying
// the status and body, so the caller can wrap it uniformly.
func (haPlugin) do(conn map[string]any, method, path string, query url.Values, body any) ([]byte, int, error) {
	full := baseURL(conn) + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+str(conn["token"]))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, resp.StatusCode, fmt.Errorf("%d: %s", resp.StatusCode, string(raw))
	}
	return raw, resp.StatusCode, nil
}

func (h haPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	conn := req.Connection

	switch req.Verb {
	case "call_service":
		domain, service := str(o["domain"]), str(o["service"])
		if domain == "" || service == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "domain and service are required")
		}
		body := map[string]any{}
		if d, ok := o["data"].(map[string]any); ok {
			for k, v := range d {
				body[k] = v
			}
		}
		if eid, ok := o["entity_id"]; ok && eid != nil {
			body["entity_id"] = eid
		}
		return h.call(conn, "POST", "/services/"+domain+"/"+service, nil, body)

	case "get_state":
		entity := str(o["entity_id"])
		if entity == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "entity_id is required")
		}
		return h.call(conn, "GET", "/states/"+entity, nil, nil)

	case "list_states":
		return h.call(conn, "GET", "/states", nil, nil)

	case "set_state":
		entity := str(o["entity_id"])
		if entity == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "entity_id is required")
		}
		body := map[string]any{"state": o["state"]}
		if a, ok := o["attributes"].(map[string]any); ok {
			body["attributes"] = a
		}
		return h.call(conn, "POST", "/states/"+entity, nil, body)

	case "fire_event":
		eventType := str(o["event_type"])
		if eventType == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "event_type is required")
		}
		var body any
		if d, ok := o["data"].(map[string]any); ok {
			body = d
		}
		return h.call(conn, "POST", "/events/"+eventType, nil, body)

	case "render_template":
		tmpl := str(o["template"])
		if tmpl == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "template is required")
		}
		raw, status, err := h.do(conn, "POST", "/template", nil, map[string]any{"template": tmpl})
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
		}
		return plugin.InvokeResult{Outputs: map[string]any{"text": string(raw), "status_code": status}}, nil

	case "get_services":
		return h.call(conn, "GET", "/services", nil, nil)

	case "get_config":
		return h.call(conn, "GET", "/config", nil, nil)

	case "history":
		path := "/history/period"
		if start := str(o["start"]); start != "" {
			path += "/" + start
		}
		q := url.Values{}
		if entity := str(o["entity_id"]); entity != "" {
			q.Set("filter_entity_id", entity)
		}
		if end := str(o["end"]); end != "" {
			q.Set("end_time", end)
		}
		return h.call(conn, "GET", path, q, nil)

	case "logbook":
		path := "/logbook"
		if start := str(o["start"]); start != "" {
			path += "/" + start
		}
		q := url.Values{}
		if entity := str(o["entity_id"]); entity != "" {
			q.Set("entity", entity)
		}
		if end := str(o["end"]); end != "" {
			q.Set("end_time", end)
		}
		return h.call(conn, "GET", path, q, nil)

	case "api":
		method := strOr(o["method"], "GET")
		p := str(o["path"])
		if p == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "path is required")
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		var q url.Values
		if qm, ok := o["query"].(map[string]any); ok && len(qm) > 0 {
			q = url.Values{}
			for k, v := range qm {
				q.Set(k, fmt.Sprintf("%v", v))
			}
		}
		return h.call(conn, method, p, q, o["body"])

	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// call issues one REST request and shapes the decoded JSON body into the
// verb's Outputs: "items" when the response is a JSON array, "result"
// otherwise (a single object, or the `api` escape hatch's passthrough).
func (h haPlugin) call(conn map[string]any, method, path string, query url.Values, body any) (plugin.InvokeResult, error) {
	raw, status, err := h.do(conn, method, path, query, body)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	out := map[string]any{"status_code": status}
	if v := decodeJSON(raw); v != nil {
		if list, ok := v.([]any); ok {
			out["items"] = list
		} else {
			out["result"] = v
		}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func decodeJSON(raw []byte) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// --- source: inbound webhook from a Home Assistant automation ---

func (haPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)

	addr, path, secret := "", "/homeassistant", ""
	allowUnsigned := false
	if webhook != nil {
		addr = str(webhook["listen"])
		if p := str(webhook["path"]); p != "" {
			path = p
		}
		secret = str(webhook["secret"])
		allowUnsigned = boolv(webhook["allow_unsigned"])
	}
	if addr == "" {
		return fmt.Errorf("homeassistant: no webhook.listen address configured")
	}
	if err := requireWebhookSecret(secret, allowUnsigned); err != nil {
		return err
	}

	dedup := sourcekit.NewDedup(2048)
	fmt.Fprintf(os.Stderr, "homeassistant[%s]: listening on %s%s\n", req.Instance, addr, path)

	// A shared-token check needs both the request header AND the query
	// string (?token=), and sourcekit.Listener's callback only forwards
	// headers + body — not the request URL — so the listener here is a
	// small direct net/http server (stdlib only) rather than
	// sourcekit.Listener. sourcekit.Dedup still does the delivery dedup.
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if !verifyToken(secret, r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		ev := buildEvent(payload)
		if ev.Dedup == "" || dedup.Add(ev.Dedup) {
			_ = emit(ev)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// verifyToken checks the shared webhook token against X-Conductor-Token or
// the ?token= query parameter, in constant time. When no secret is
// configured, the caller (StartSource) only got this far because
// allow_unsigned was set, so every request is accepted.
func verifyToken(secret string, r *http.Request) bool {
	if secret == "" {
		return true
	}
	got := r.Header.Get("X-Conductor-Token")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	if got == "" {
		return false
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

// buildEvent turns one posted JSON body into a wireEvent: every top-level
// key becomes a context field, plus a "payload" copy of the whole body;
// event_type (if present) becomes the event's kind and is aliased into
// event_types for the daemon's generic list-contains filter evaluator;
// id (if present) is the dedup key.
func buildEvent(payload map[string]any) wireEvent {
	ctx := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		ctx[k] = v
	}
	ctx["payload"] = payload

	kind := ""
	if et, ok := payload["event_type"]; ok {
		kind = fmt.Sprintf("%v", et)
		ctx["event_type"] = et
		ctx["event_types"] = et
	}

	dedup := ""
	if id, ok := payload["id"]; ok {
		dedup = fmt.Sprintf("%v", id)
	}

	title := "home assistant event"
	if kind != "" {
		title = "home assistant event: " + kind
	}

	return wireEvent{Event: "event", Kind: kind, Title: title, Context: ctx, Dedup: dedup}
}

// requireWebhookSecret refuses to start an unauthenticated webhook
// listener.
//
// A shared-token check against an empty secret would accept ANY POST on the
// listen address as a real automation event, which is remote trigger
// injection with no signal that it happened. A missing secret is far more
// often a mistake than a choice, so it fails closed; `allow_unsigned: true`
// is the explicit, greppable way to say you meant it.
func requireWebhookSecret(secret string, allowUnsigned bool) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintln(os.Stderr, "homeassistant: allow_unsigned is set — accepting UNSIGNED webhooks; anyone who can reach the listen address can fire triggers")
		return nil
	}
	return fmt.Errorf("homeassistant: no webhook secret configured (webhook.secret) — an unsigned listener accepts any POST on the listen address as a real event. Set it, or set `webhook.allow_unsigned: true` if you genuinely front this with something else that authenticates")
}

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func boolv(v any) bool {
	b, _ := v.(bool)
	return b
}

func main() {
	if err := plugin.Serve(haPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-homeassistant:", err)
		os.Exit(1)
	}
}
