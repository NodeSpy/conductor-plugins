// Command conductor-datadog is the Datadog connector as a standalone external
// conductor plugin (#59). It calls the Datadog API (events, monitors, metrics)
// as verbs, and, as a SOURCE, receives Datadog webhooks and streams a
// normalized "alert" event per delivery to the daemon, which matches it to the
// operator's triggers and resolves the action. Built ONLY against the standard
// library, the public SDK (pkg/plugin), and the connector-kit (pkg/sourcekit)
// — no other internal daemon package, no third-party dependency.
//
// Connection (used for both Invoke and StartSource):
//
//	api_key: "<Datadog API key>"         # DD-API-KEY (required)
//	app_key: "<Datadog application key>" # DD-APPLICATION-KEY (required for monitor/metric verbs)
//	site: "datadoghq.com"                # default; also datadoghq.eu, us5.datadoghq.com, ...
//	api_base: "https://127.0.0.1:PORT"   # override the API base (tests only; overrides site)
//	webhook:
//	  listen: ":9097"                    # HTTP listener address (StartSource only)
//	  path: "/datadog"                   # request path (default /datadog)
//	  secret: "<shared token>"           # compared to X-Conductor-Token header / ?token= query param
//
// Datadog webhooks are user-templated and UNSIGNED: there is no HMAC to
// verify. Instead, the operator pastes a shared token into the webhook URL
// (?token=<secret>) or a custom header (X-Conductor-Token: <secret>), and this
// plugin compares it (constant-time) to webhook.secret. The shared
// sourcekit.Listener.ServeReq hands the callback the full request (headers,
// query, body), so the ?token= query case is checked directly — and the same
// Listener transparently accepts deliveries over a smee.io-style relay
// (webhook.smee) for endpoints with no public URL. See verifyToken below and
// docs/connectors/datadog.md for the exact payload template the operator
// configures in Datadog.
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

type datadogPlugin struct{}

func (datadogPlugin) Describe() plugin.Decl {
	ctx := plugin.Schema{
		"alert_id":   {Type: "string"},
		"alert_type": {Type: "string", Desc: "error|warning|success|info|recovery"},
		"title":      {Type: "string"},
		"body":       {Type: "string"},
		"priority":   {Type: "string"},
		"tags":       {Type: "list", Desc: "the alert's tags, split from Datadog's comma/space-separated $TAGS"},
		"url":        {Type: "string"},
		"event_id":   {Type: "string"},
		"org_name":   {Type: "string"},
		"hostname":   {Type: "string"},
		"scope":      {Type: "string", Desc: "the monitor scope, e.g. host:foo"},
		"transition": {Type: "string", Desc: "the monitor's alert-state transition"},
	}
	filters := plugin.Schema{
		"alert_types": {Type: "list", Desc: "error|warning|success|info|recovery (empty = any)"},
		"priorities":  {Type: "list", Desc: "e.g. P1, P2, normal, low (empty = any)"},
		"tags":        {Type: "list", Desc: "matches if any of the alert's tags is in this list"},
		"scopes":      {Type: "list", Desc: "the monitor scope, e.g. host:foo (empty = any)"},
		"alert_type":  {Type: "string"},
		"priority":    {Type: "string"},
		"scope":       {Type: "string"},
	}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "datadog",
		Desc: "Datadog: post events, mute/unmute monitors, list/read monitors, submit metrics, and a generic API escape hatch as verbs; monitor alert webhooks as a source. External plugin (#59).",
		Connection: plugin.Schema{
			"api_key":  {Type: "string", Desc: "DD-API-KEY", Required: true},
			"app_key":  {Type: "string", Desc: "DD-APPLICATION-KEY (required for monitor/metric verbs)"},
			"site":     {Type: "string", Desc: "Datadog site, e.g. datadoghq.com (default), datadoghq.eu, us5.datadoghq.com"},
			"api_base": {Type: "string", Desc: "override the full API base URL (tests only; overrides site)"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path, secret, smee"},
		},
		Events: []plugin.Event{{
			Name:    "alert",
			Desc:    "a Datadog monitor alert fired, per the documented webhook payload template (see docs/connectors/datadog.md)",
			Filters: filters,
			Context: ctx,
		}},
		Verbs: []plugin.Verb{
			{
				Name: "post_event", Desc: "post an event to the Datadog event stream",
				Options: plugin.Schema{
					"title":           {Type: "string", Required: true},
					"text":            {Type: "string", Required: true},
					"tags":            {Type: "list"},
					"alert_type":      {Type: "string", Enum: []string{"error", "warning", "success", "info"}},
					"priority":        {Type: "string", Enum: []string{"normal", "low"}},
					"aggregation_key": {Type: "string"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "mute_monitor", Desc: "mute a monitor (optionally scoped, optionally until a unix time)",
				Options: plugin.Schema{
					"monitor_id": {Type: "string", Required: true, Scope: "monitor"},
					"scope":      {Type: "string", Desc: "mute only this scope, e.g. host:foo"},
					"end":        {Type: "integer", Desc: "unix timestamp to auto-unmute (omit = indefinite)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "unmute_monitor", Desc: "unmute a monitor (optionally scoped)",
				Options: plugin.Schema{
					"monitor_id": {Type: "string", Required: true, Scope: "monitor"},
					"scope":      {Type: "string"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "get_monitor", Desc: "read a monitor's definition and status",
				Options: plugin.Schema{"monitor_id": {Type: "string", Required: true, Scope: "monitor"}},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "list_monitors", Desc: "list monitors, optionally filtered by name/tags",
				Options: plugin.Schema{
					"name":         {Type: "string"},
					"tags":         {Type: "list", Desc: "tags query param"},
					"monitor_tags": {Type: "list", Desc: "monitor_tags query param"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "mute_all", Desc: "mute all monitors org-wide",
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "unmute_all", Desc: "unmute all monitors org-wide",
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "submit_metric", Desc: "submit a custom metric (v2 series API)",
				Options: plugin.Schema{
					"metric": {Type: "string", Required: true},
					"points": {Type: "list", Required: true, Desc: "list of [timestamp, value] pairs, or bare values (now is used as the timestamp)"},
					"type":   {Type: "string", Enum: []string{"count", "gauge", "rate"}, Desc: "default gauge"},
					"tags":   {Type: "list"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "generic escape hatch: call any Datadog API path",
				Usage: "for endpoints without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Required: true, Enum: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
					"path":   {Type: "string", Required: true, Desc: "e.g. /api/v1/events"},
					"query":  {Type: "map"},
					"body":   {Type: "any"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
		},
		// Egress is scoped to the DEFAULT site only. A non-default `site`
		// (datadoghq.eu, us5.datadoghq.com, ...) reaches a different host, so an
		// operator using one MUST widen `network:` on the connector instance to
		// match — Capabilities can only be narrowed, never widened, so the
		// default manifest cannot cover every site up front.
		Capabilities: plugin.Capabilities{Egress: []string{"api.datadoghq.com:443"}},
	}
}

// apiBase derives the Datadog API base URL from `site` (default
// datadoghq.com). Broken out as its own func so tests can point it at an
// httptest.Server instead of the real API.
func apiBase(site string) string {
	if site == "" {
		site = "datadoghq.com"
	}
	return "https://api." + site
}

func (datadogPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	apiKey, appKey := str(conn["api_key"]), str(conn["app_key"])
	base := strOr(conn["api_base"], apiBase(str(conn["site"])))
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	c := &client{base: base, apiKey: apiKey, appKey: appKey}

	switch req.Verb {
	case "post_event":
		return c.postEvent(o)
	case "mute_monitor":
		return c.muteMonitor(o)
	case "unmute_monitor":
		return c.unmuteMonitor(o)
	case "get_monitor":
		return c.getMonitor(o)
	case "list_monitors":
		return c.listMonitors(o)
	case "mute_all":
		return c.do(http.MethodPost, "/api/v1/monitor/mute_all", nil, nil)
	case "unmute_all":
		return c.do(http.MethodPost, "/api/v1/monitor/unmute_all", nil, nil)
	case "submit_metric":
		return c.submitMetric(o)
	case "api":
		return c.genericAPI(o)
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
}

// --- client: a thin wrapper around net/http for the Datadog API ---

type client struct {
	base, apiKey, appKey string
}

// do issues one request, decodes a non-2xx as CodeInternalError (status +
// body), and returns the parsed body under "result" on success.
func (c *client) do(method, path string, query url.Values, body any) (plugin.InvokeResult, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "encoding request body: "+err.Error())
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	req.Header.Set("DD-API-KEY", c.apiKey)
	req.Header.Set("DD-APPLICATION-KEY", c.appKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
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
			fmt.Sprintf("datadog %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw))))
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

func (c *client) postEvent(o map[string]any) (plugin.InvokeResult, error) {
	title, text := str(o["title"]), str(o["text"])
	if title == "" || text == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "title and text are required")
	}
	body := map[string]any{"title": title, "text": text}
	if tags := strList(o["tags"]); len(tags) > 0 {
		body["tags"] = tags
	}
	if v := str(o["alert_type"]); v != "" {
		body["alert_type"] = v
	}
	if v := str(o["priority"]); v != "" {
		body["priority"] = v
	}
	if v := str(o["aggregation_key"]); v != "" {
		body["aggregation_key"] = v
	}
	return c.do(http.MethodPost, "/api/v1/events", nil, body)
}

func (c *client) muteMonitor(o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["monitor_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "monitor_id is required")
	}
	body := map[string]any{}
	if v := str(o["scope"]); v != "" {
		body["scope"] = v
	}
	if v, ok := intVal(o["end"]); ok {
		body["end"] = v
	}
	return c.do(http.MethodPost, "/api/v1/monitor/"+id+"/mute", nil, body)
}

func (c *client) unmuteMonitor(o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["monitor_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "monitor_id is required")
	}
	body := map[string]any{}
	if v := str(o["scope"]); v != "" {
		body["scope"] = v
	}
	return c.do(http.MethodPost, "/api/v1/monitor/"+id+"/unmute", nil, body)
}

func (c *client) getMonitor(o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["monitor_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "monitor_id is required")
	}
	return c.do(http.MethodGet, "/api/v1/monitor/"+id, nil, nil)
}

func (c *client) listMonitors(o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["name"]); v != "" {
		q.Set("name", v)
	}
	if tags := strList(o["tags"]); len(tags) > 0 {
		q.Set("tags", strings.Join(tags, ","))
	}
	if tags := strList(o["monitor_tags"]); len(tags) > 0 {
		q.Set("monitor_tags", strings.Join(tags, ","))
	}
	res, err := c.do(http.MethodGet, "/api/v1/monitor", q, nil)
	if err != nil {
		return res, err
	}
	out := map[string]any{"status_code": res.Outputs["status_code"]}
	if list, ok := res.Outputs["result"].([]any); ok {
		out["items"] = list
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (c *client) submitMetric(o map[string]any) (plugin.InvokeResult, error) {
	metric := str(o["metric"])
	if metric == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "metric is required")
	}
	rawPoints, ok := o["points"].([]any)
	if !ok || len(rawPoints) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "points is required")
	}
	now := time.Now().Unix()
	points := make([]map[string]any, 0, len(rawPoints))
	for _, p := range rawPoints {
		switch v := p.(type) {
		case []any:
			if len(v) != 2 {
				return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "each points entry must be [timestamp, value] or a bare value")
			}
			ts, _ := intVal(v[0])
			points = append(points, map[string]any{"timestamp": ts, "value": v[1]})
		default:
			points = append(points, map[string]any{"timestamp": now, "value": v})
		}
	}
	mtype := strOr(o["type"], "gauge")
	series := map[string]any{
		"metric": metric,
		"type":   metricTypeCode(mtype),
		"points": points,
	}
	if tags := strList(o["tags"]); len(tags) > 0 {
		series["tags"] = tags
	}
	body := map[string]any{"series": []any{series}}
	return c.do(http.MethodPost, "/api/v2/series", nil, body)
}

// metricTypeCode renders the v2 series API's numeric type code from the
// friendly name (0=unspecified is never used here; count=1, rate=2, gauge=3).
func metricTypeCode(name string) int {
	switch name {
	case "count":
		return 1
	case "rate":
		return 2
	default:
		return 3 // gauge
	}
}

func (c *client) genericAPI(o map[string]any) (plugin.InvokeResult, error) {
	method := strings.ToUpper(str(o["method"]))
	path := str(o["path"])
	if method == "" || path == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "method and path are required")
	}
	q := url.Values{}
	if m, ok := o["query"].(map[string]any); ok {
		for k, v := range m {
			q.Set(k, fmt.Sprintf("%v", v))
		}
	}
	return c.do(method, path, q, o["body"])
}

// --- source: Datadog webhook ---

func (datadogPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	addr, path, secret, smeeURL := "", "/datadog", "", ""
	if webhook != nil {
		addr = str(webhook["listen"])
		if p := str(webhook["path"]); p != "" {
			path = p
		}
		secret = str(webhook["secret"])
		smeeURL = str(webhook["smee"])
	}
	if addr == "" && smeeURL == "" {
		return fmt.Errorf("datadog: no webhook.listen address or webhook.smee relay configured")
	}
	if err := requireWebhookSecret("datadog", secret, cfg, "webhook.secret"); err != nil {
		return err
	}
	dedup := sourcekit.NewDedup(2048)
	if addr != "" {
		fmt.Fprintf(os.Stderr, "datadog[%s]: listening on %s%s\n", req.Instance, addr, path)
	}
	if smeeURL != "" {
		fmt.Fprintf(os.Stderr, "datadog[%s]: relaying via smee channel %s\n", req.Instance, smeeURL)
	}
	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smeeURL}
	return ln.ServeReq(ctx, func(rq *sourcekit.Request) {
		if secret != "" && !verifyToken(secret, rq) {
			return
		}
		f, ok := parseAlert(rq.Body)
		if !ok {
			return
		}
		dk := dedupKey(f)
		if !dedup.Add(dk) {
			return
		}
		tags := splitTags(f.tags)
		_ = emit(map[string]any{
			"event": "alert",
			"kind":  nonEmpty(f.alertType, "alert"),
			"title": fmt.Sprintf("datadog %s: %s", nonEmpty(f.alertType, "alert"), f.title),
			"dedup": dk,
			"context": map[string]any{
				"alert_id": f.alertID, "alert_type": f.alertType, "title": f.title, "body": f.body,
				"priority": f.priority, "tags": tags, "url": f.url, "event_id": f.eventID,
				"org_name": f.orgName, "hostname": f.hostname, "scope": f.scope, "transition": f.transition,
				// Plural aliases so the documented filter vocabulary
				// (filters: {alert_types/priorities/tags/scopes: [...]}) matches
				// the daemon's generic list-contains filter evaluator.
				"alert_types": f.alertType, "priorities": f.priority, "scopes": f.scope,
			},
		})
	})
}

// verifyToken compares the request's X-Conductor-Token header, or (when the
// header is absent) its ?token= query parameter, against secret in constant
// time. Datadog cannot sign webhooks, so this shared-token check is the only
// authentication available; requireWebhookSecret ensures a token is always
// configured unless the operator explicitly opts out with allow_unsigned.
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

// facts is the parsed shape of the documented Datadog webhook payload
// template (see docs/connectors/datadog.md). Datadog webhook bodies are
// entirely operator-defined via $-variables, so this plugin only understands
// the ONE template it documents; anything else fails to parse.
type facts struct {
	alertID, alertType, title, body, priority, tags, url, eventID string
	orgName, hostname, scope, transition                          string
}

type wire struct {
	AlertID    string `json:"alert_id"`
	AlertType  string `json:"alert_type"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Priority   string `json:"priority"`
	Tags       string `json:"tags"`
	URL        string `json:"url"`
	EventID    string `json:"event_id"`
	OrgName    string `json:"org_name"`
	Hostname   string `json:"hostname"`
	Scope      string `json:"scope"`
	Transition string `json:"transition"`
}

func parseAlert(body []byte) (facts, bool) {
	var w wire
	if err := json.Unmarshal(body, &w); err != nil {
		return facts{}, false
	}
	f := facts{
		alertID: w.AlertID, alertType: w.AlertType, title: w.Title, body: w.Body,
		priority: w.Priority, tags: w.Tags, url: w.URL, eventID: w.EventID,
		orgName: w.OrgName, hostname: w.Hostname, scope: w.Scope, transition: w.Transition,
	}
	if f.alertID == "" && f.eventID == "" && f.title == "" {
		return facts{}, false
	}
	return f, true
}

// dedupKey is alert_id + alert_type, falling back to event_id/title when
// alert_id is empty (Datadog omits $ALERT_ID for some monitor types).
func dedupKey(f facts) string {
	id := nonEmpty(f.alertID, f.eventID, f.title)
	return id + "\x00" + f.alertType
}

// splitTags splits Datadog's comma-or-space separated $TAGS string into a
// list, so the documented `tags` filter (a list) has something to match
// against alongside the raw string context field.
func splitTags(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
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
			out = append(out, fmt.Sprintf("%v", e))
		}
		return out
	}
	return nil
}

// intVal coerces a JSON-decoded number/string into an int, reporting whether
// v was present and coercible.
func intVal(v any) (int, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case string:
		i, err := strconv.Atoi(x)
		return i, err == nil
	}
	return 0, false
}

func main() {
	if err := plugin.Serve(datadogPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-datadog: %v\n", err)
		os.Exit(1)
	}
}

// requireWebhookSecret refuses to start an unauthenticated webhook listener.
//
// Datadog cannot sign its webhook payloads (they are plain operator-templated
// JSON), so the ONLY authentication available is a shared token the operator
// embeds in the webhook URL or a header. Treating a missing token the same
// way the HMAC-based connectors treat a missing signing secret keeps the
// fail-closed posture consistent: a missing token is far more often a mistake
// than a choice, so it fails closed; `allow_unsigned: true` is the explicit,
// greppable way to say you meant it (e.g. the listener sits behind something
// else that authenticates, such as a private network or a reverse proxy).
func requireWebhookSecret(who, secret string, cfg map[string]any, allowKey string) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if b, _ := cfg["allow_unsigned"].(bool); b {
		fmt.Fprintf(os.Stderr, "%s: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers\n", who)
		return nil
	}
	return fmt.Errorf("%s: no webhook token configured (%s) — Datadog cannot sign webhooks, so an unauthenticated listener accepts any POST on the listen address as a real event. Set it, or set `allow_unsigned: true` if you genuinely front this with something else that authenticates", who, allowKey)
}
