// Command conductor-uptimerobot is the UptimeRobot connector as a standalone
// external conductor plugin (#59). It calls the UptimeRobot v2 REST API
// (monitor CRUD, account details, alert contacts, and a generic API escape
// hatch) as verbs, and, as a SOURCE, receives UptimeRobot "Web-Hook" alert
// contact deliveries and streams a normalized "alert" event per delivery to
// the daemon, which matches it to the operator's triggers and resolves the
// action. Built ONLY against the public SDK (pkg/plugin) and the
// connector-kit (pkg/sourcekit) — no other internal daemon package, no
// third-party dependency.
//
// Connection (used for both Invoke and StartSource):
//
//	api_key: "<UptimeRobot API key>"   # main key, or a monitor-specific key
//	api_base: "https://api.example.com" # override the API base (tests)
//	webhook:
//	  listen: ":9096"                  # HTTP listener address (StartSource only)
//	  path: "/uptimerobot"             # request path (default /uptimerobot)
//	  secret: "<shared token>"         # compared to X-Conductor-Token / ?token=
//	  allow_unsigned: false            # true = accept unauthenticated POSTs
//	  smee: "https://smee.io/xyz"      # optional smee.io-style SSE relay, for
//	                                   # when the listener has no public URL
//
// UptimeRobot's REST API takes application/x-www-form-urlencoded request
// bodies (api_key + format=json + the verb's own fields) and always answers
// 200 with a JSON body carrying "stat": "ok" or "stat": "fail" — a
// provider-level failure is NOT a non-2xx HTTP status, so both cases (a
// non-2xx transport error, or a 200 with "stat":"fail") are surfaced as a
// plugin.CodeInternalError carrying the failure message and the raw body.
//
// UptimeRobot alert contacts of type "Web-Hook" are entirely operator
// templated: there is no signature scheme at all, only whatever the operator
// pastes into the POST value/URL. So, like the datadog connector,
// sourcekit.Listener.Secret is left EMPTY (its HMAC check does not apply) and
// a shared token — from either the X-Conductor-Token header or a ?token=
// query parameter — is checked by hand. The shared sourcekit.Listener.ServeReq
// hands the callback the full request (headers, query, body), so the ?token=
// query case is checked directly — and the same Listener transparently
// accepts deliveries relayed over webhook.smee for endpoints with no public
// URL.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
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

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type uptimerobotPlugin struct{}

func (uptimerobotPlugin) Describe() plugin.Decl {
	ctx := plugin.Schema{
		"alert_type":    {Type: "string", Desc: "up|down"},
		"monitor_id":    {Type: "string"},
		"monitor_url":   {Type: "string"},
		"monitor_name":  {Type: "string"},
		"alert_details": {Type: "string"},
		"datetime":      {Type: "string"},
	}
	filters := plugin.Schema{
		"alert_types":  {Type: "list", Desc: "up|down (empty = any)"},
		"monitors":     {Type: "list", Desc: "matches monitor_id or monitor_name (empty = any)"},
		"alert_type":   {Type: "string", Desc: "up|down"},
		"monitor_name": {Type: "string"},
	}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "uptimerobot",
		Desc: "UptimeRobot: monitor CRUD, account/alert-contact reads, and a generic API escape hatch as verbs; monitor up/down alerts (Web-Hook alert contact) as a source.",
		Connection: plugin.Schema{
			"api_key":  {Type: "string", Required: true, Desc: "UptimeRobot API key (main, or monitor-specific)"},
			"api_base": {Type: "string", Desc: "override the API base URL (tests, or a private gateway)"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path, secret, allow_unsigned, smee"},
		},
		Events: []plugin.Event{{
			Name: "alert", Desc: "an UptimeRobot monitor alert fired (Web-Hook alert contact delivery)",
			Context: ctx, Filters: filters,
		}},
		Verbs: []plugin.Verb{
			{
				Name: "get_monitors", Desc: "list monitors, optionally filtered by id/status/type/search",
				Options: plugin.Schema{
					"monitors": {Type: "list", Desc: "monitor ids to restrict to (empty = all)"},
					"statuses": {Type: "list", Desc: "status codes to filter on, e.g. 2 (up), 9 (down)"},
					"types":    {Type: "list", Desc: "monitor type codes to filter on, e.g. 1 (http), 3 (ping)"},
					"search":   {Type: "string", Desc: "free-text search over friendly_name/url"},
					"limit":    {Type: "integer", Desc: "max results (default 50)"},
					"offset":   {Type: "integer", Desc: "pagination offset"},
					"logs":     {Type: "boolean", Desc: "include each monitor's response-time log entries"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "new_monitor", Desc: "create a new monitor",
				Options: plugin.Schema{
					"friendly_name": {Type: "string", Required: true},
					"url":           {Type: "string", Required: true},
					"type":          {Type: "integer", Required: true, Desc: "1=http, 2=keyword, 3=ping, 4=port"},
					"interval":      {Type: "integer", Desc: "check interval in seconds (default 300)"},
					"sub_type":      {Type: "string", Desc: "port monitor sub-type, e.g. http, https, custom port number"},
					"port":          {Type: "integer", Desc: "port monitor: the port to check"},
					"keyword_type":  {Type: "string", Desc: "keyword monitor: alert_exists|alert_not_exists"},
					"keyword_value": {Type: "string", Desc: "keyword monitor: the keyword/pattern to match"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "edit_monitor", Desc: "edit a monitor's definition, or pause/resume it via status",
				Options: plugin.Schema{
					"id":            {Type: "string", Required: true, Scope: "monitor"},
					"friendly_name": {Type: "string"},
					"url":           {Type: "string"},
					"interval":      {Type: "integer"},
					"status":        {Type: "integer", Desc: "0=pause, 1=resume"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "delete_monitor", Desc: "permanently delete a monitor",
				Options: plugin.Schema{"id": {Type: "string", Required: true, Scope: "monitor"}},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "pause_monitor", Desc: "pause a monitor (edit_monitor with status=0)",
				Options: plugin.Schema{"id": {Type: "string", Required: true, Scope: "monitor"}},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "resume_monitor", Desc: "resume a paused monitor (edit_monitor with status=1)",
				Options: plugin.Schema{"id": {Type: "string", Required: true, Scope: "monitor"}},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name:    "get_account_details",
				Desc:    "read the account's limits and usage",
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name:    "get_alert_contacts",
				Desc:    "list the account's alert contacts",
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "generic escape hatch: call any UptimeRobot v2 API method",
				Usage: "for endpoints without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Required: true, Desc: "the API method path segment, e.g. getMonitors"},
					"params": {Type: "map", Desc: "additional form fields"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
		},
		Capabilities: plugin.Capabilities{Egress: []string{"api.uptimerobot.com:443"}},
	}
}

// --- client: a thin wrapper around net/http for the UptimeRobot v2 API ---

const defaultAPIBase = "https://api.uptimerobot.com/v2"

type client struct {
	base, apiKey string
}

// post sends one x-www-form-urlencoded POST to {base}/{method}, decodes the
// JSON response, and treats EITHER a non-2xx HTTP status OR a 200 with
// "stat":"fail" as a plugin.CodeInternalError carrying the failure message
// plus the raw response body — UptimeRobot signals provider-level failures
// in the body, not the status line.
func (c *client) post(method string, form url.Values) (map[string]any, int, error) {
	if form == nil {
		form = url.Values{}
	}
	form.Set("api_key", c.apiKey)
	form.Set("format", "json")

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(c.base, "/")+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, plugin.Errorf(plugin.CodeInternalError, "reading response: "+err.Error())
	}
	body := strings.TrimSpace(string(raw))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("uptimerobot %s: status %d: %s", method, resp.StatusCode, body))
	}

	var parsed map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, resp.StatusCode, plugin.Errorf(plugin.CodeInternalError, "decoding response: "+err.Error())
		}
	}
	if stat, _ := parsed["stat"].(string); stat == "fail" {
		return nil, resp.StatusCode, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("uptimerobot %s: %s: %s", method, errMessage(parsed), body))
	}
	return parsed, resp.StatusCode, nil
}

// errMessage extracts the human-readable message from UptimeRobot's
// {"stat":"fail","error":{"message":"..."}} error shape, falling back to a
// generic label when the shape is unrecognized.
func errMessage(parsed map[string]any) string {
	if e, ok := parsed["error"].(map[string]any); ok {
		if m, ok := e["message"].(string); ok && m != "" {
			return m
		}
		if t, ok := e["type"].(string); ok && t != "" {
			return t
		}
	}
	return "request failed"
}

func (uptimerobotPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	apiKey := str(conn["api_key"])
	base := strOr(conn["api_base"], defaultAPIBase)
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	c := &client{base: base, apiKey: apiKey}

	switch req.Verb {
	case "get_monitors":
		return c.getMonitors(o)
	case "new_monitor":
		return c.newMonitor(o)
	case "edit_monitor":
		return c.editMonitor(o)
	case "delete_monitor":
		return c.deleteMonitor(o)
	case "pause_monitor":
		return c.setMonitorStatus(o, "0")
	case "resume_monitor":
		return c.setMonitorStatus(o, "1")
	case "get_account_details":
		return c.getAccountDetails()
	case "get_alert_contacts":
		return c.getAlertContacts()
	case "api":
		return c.genericAPI(o)
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
}

func (c *client) getMonitors(o map[string]any) (plugin.InvokeResult, error) {
	form := url.Values{}
	if ids := strList(o["monitors"]); len(ids) > 0 {
		form.Set("monitors", strings.Join(ids, "-"))
	}
	if v := strList(o["statuses"]); len(v) > 0 {
		form.Set("statuses", strings.Join(v, "-"))
	}
	if v := strList(o["types"]); len(v) > 0 {
		form.Set("types", strings.Join(v, "-"))
	}
	if v := str(o["search"]); v != "" {
		form.Set("search", v)
	}
	if v, ok := intVal(o["limit"]); ok {
		form.Set("limit", strconv.Itoa(v))
	}
	if v, ok := intVal(o["offset"]); ok {
		form.Set("offset", strconv.Itoa(v))
	}
	if boolv(o["logs"]) {
		form.Set("logs", "1")
	}
	parsed, status, err := c.post("getMonitors", form)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if items, ok := parsed["monitors"].([]any); ok {
		out["items"] = items
	} else {
		out["items"] = []any{}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (c *client) newMonitor(o map[string]any) (plugin.InvokeResult, error) {
	friendlyName, u := str(o["friendly_name"]), str(o["url"])
	typ, hasType := intVal(o["type"])
	if friendlyName == "" || u == "" || !hasType {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "friendly_name, url, and type are required")
	}
	form := url.Values{"friendly_name": {friendlyName}, "url": {u}, "type": {strconv.Itoa(typ)}}
	if v, ok := intVal(o["interval"]); ok {
		form.Set("interval", strconv.Itoa(v))
	}
	if v := str(o["sub_type"]); v != "" {
		form.Set("sub_type", v)
	}
	if v, ok := intVal(o["port"]); ok {
		form.Set("port", strconv.Itoa(v))
	}
	if v := str(o["keyword_type"]); v != "" {
		form.Set("keyword_type", v)
	}
	if v := str(o["keyword_value"]); v != "" {
		form.Set("keyword_value", v)
	}
	return c.doResult("newMonitor", form)
}

func (c *client) editMonitor(o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "id is required")
	}
	form := url.Values{"id": {id}}
	if v := str(o["friendly_name"]); v != "" {
		form.Set("friendly_name", v)
	}
	if v := str(o["url"]); v != "" {
		form.Set("url", v)
	}
	if v, ok := intVal(o["interval"]); ok {
		form.Set("interval", strconv.Itoa(v))
	}
	if v, ok := intVal(o["status"]); ok {
		form.Set("status", strconv.Itoa(v))
	}
	return c.doResult("editMonitor", form)
}

func (c *client) deleteMonitor(o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "id is required")
	}
	return c.doResult("deleteMonitor", url.Values{"id": {id}})
}

// setMonitorStatus implements pause_monitor/resume_monitor as editMonitor
// with the given status (0=pause, 1=resume) — UptimeRobot has no dedicated
// pause/resume endpoint.
func (c *client) setMonitorStatus(o map[string]any, status string) (plugin.InvokeResult, error) {
	id := str(o["id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "id is required")
	}
	return c.doResult("editMonitor", url.Values{"id": {id}, "status": {status}})
}

func (c *client) getAccountDetails() (plugin.InvokeResult, error) {
	return c.doResult("getAccountDetails", url.Values{})
}

func (c *client) getAlertContacts() (plugin.InvokeResult, error) {
	parsed, status, err := c.post("getAlertContacts", url.Values{})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if items, ok := parsed["alert_contacts"].([]any); ok {
		out["items"] = items
	} else {
		out["items"] = []any{}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (c *client) genericAPI(o map[string]any) (plugin.InvokeResult, error) {
	method := str(o["method"])
	if method == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "method is required")
	}
	form := url.Values{}
	if m, ok := o["params"].(map[string]any); ok {
		for k, v := range m {
			form.Set(k, fmt.Sprintf("%v", v))
		}
	}
	return c.doResult(method, form)
}

// doResult POSTs and hoists the whole decoded response under "result".
func (c *client) doResult(method string, form url.Values) (plugin.InvokeResult, error) {
	parsed, status, err := c.post(method, form)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	var result any
	if parsed != nil {
		result = parsed
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": result, "status_code": status}}, nil
}

// --- source: UptimeRobot Web-Hook alert contact ---

func (uptimerobotPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	addr, path, secret, smeeURL, allowUnsigned := "", "/uptimerobot", "", "", false
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
		return fmt.Errorf("uptimerobot: no webhook.listen address or webhook.smee relay configured")
	}
	if err := requireWebhookSecret("uptimerobot", secret, allowUnsigned); err != nil {
		return err
	}
	dedup := sourcekit.NewDedup(2048)
	if addr != "" {
		fmt.Fprintf(os.Stderr, "uptimerobot[%s]: listening on %s%s\n", req.Instance, addr, path)
	}
	if smeeURL != "" {
		fmt.Fprintf(os.Stderr, "uptimerobot[%s]: relaying via smee channel %s\n", req.Instance, smeeURL)
	}
	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smeeURL}
	return ln.ServeReq(ctx, func(rq *sourcekit.Request) {
		if secret != "" && !verifyTokenFromRequest(secret, rq) {
			return
		}
		f, ok := parseAlert(rq.Header, rq.Body)
		if !ok {
			return
		}
		dk := dedupKey(f)
		if !dedup.Add(dk) {
			return
		}
		_ = emit(map[string]any{
			"event": "alert",
			"kind":  f.alertKind,
			"title": fmt.Sprintf("uptimerobot %s: %s", f.alertKind, nonEmpty(f.monitorFriendlyName, f.monitorURL)),
			"dedup": dk,
			"context": map[string]any{
				"alert_type":    f.alertKind,
				"monitor_id":    f.monitorID,
				"monitor_url":   f.monitorURL,
				"monitor_name":  f.monitorFriendlyName,
				"alert_details": f.alertDetails,
				"datetime":      f.alertDateTime,
				// Plural aliases so the documented filter vocabulary
				// (filters: {alert_types/monitors: [...]}) matches the daemon's
				// generic list-contains filter evaluator.
				"alert_types": f.alertKind,
				"monitors":    []any{f.monitorID, f.monitorFriendlyName},
			},
		})
	})
}

// --- UptimeRobot alert payload parsing ---
//
// The operator configures a "Web-Hook" alert contact with a POST value
// template using UptimeRobot's *variable* substitutions:
//
//	monitorID=*monitorID*&monitorURL=*monitorURL*&monitorFriendlyName=*monitorFriendlyName*&alertType=*alertType*&alertDetails=*alertDetails*&alertDateTime=*alertDateTime*
//
// or the equivalent as a JSON body (Content-Type: application/json). Both
// shapes are accepted; JSON is tried first (a form-encoded body is never
// valid JSON, so this is unambiguous), falling back to
// application/x-www-form-urlencoded parsing.

type alertFacts struct {
	monitorID, monitorURL, monitorFriendlyName string
	alertType, alertDetails, alertDateTime     string
	alertKind                                  string // "up" or "down", derived from alertType
}

func parseAlert(_ http.Header, body []byte) (alertFacts, bool) {
	fields := map[string]string{}
	var asJSON map[string]any
	if err := json.Unmarshal(body, &asJSON); err == nil && asJSON != nil {
		for k, v := range asJSON {
			fields[k] = toStr(v)
		}
	} else {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return alertFacts{}, false
		}
		for k := range form {
			fields[k] = form.Get(k)
		}
	}

	f := alertFacts{
		monitorID:           fields["monitorID"],
		monitorURL:          fields["monitorURL"],
		monitorFriendlyName: fields["monitorFriendlyName"],
		alertType:           fields["alertType"],
		alertDetails:        fields["alertDetails"],
		alertDateTime:       fields["alertDateTime"],
	}
	if f.monitorID == "" && f.monitorURL == "" && f.monitorFriendlyName == "" {
		return alertFacts{}, false
	}
	switch f.alertType {
	case "1":
		f.alertKind = "down"
	case "2":
		f.alertKind = "up"
	default:
		f.alertKind = nonEmpty(f.alertType, "alert")
	}
	return f, true
}

// dedupKey identifies one alert delivery: monitorID + alertType +
// alertDateTime, so a redelivery of the same alert is dropped but a genuine
// down->up transition (or a later, distinct down alert) is not.
func dedupKey(f alertFacts) string {
	return f.monitorID + "\x00" + f.alertType + "\x00" + f.alertDateTime
}

func toStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// --- webhook transport: header+query token auth ---

// verifyTokenFromRequest compares a shared token — from the X-Conductor-Token
// header, or failing that a ?token= query parameter — against secret in
// constant time. UptimeRobot's Web-Hook alert contact cannot sign its
// deliveries, so this shared-token check is the only authentication
// available; requireWebhookSecret ensures a token is always configured unless
// the operator explicitly opts out with allow_unsigned.
func verifyTokenFromRequest(secret string, rq *sourcekit.Request) bool {
	got := rq.Header.Get("X-Conductor-Token")
	if got == "" {
		got = rq.Query.Get("token")
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// requireWebhookSecret refuses to start an unauthenticated webhook listener.
//
// UptimeRobot's Web-Hook alert contact cannot sign its deliveries at all —
// the POST value template is entirely operator-defined plain text — so the
// only authentication available is a shared token the operator embeds in the
// webhook URL or a custom header. Treating a missing token the same way the
// HMAC-based connectors treat a missing signing secret keeps the fail-closed
// posture consistent: a missing token is far more often a mistake than a
// choice, so it fails closed; `allow_unsigned: true` is the explicit,
// greppable way to say you meant it (e.g. the listener sits behind something
// else that authenticates, such as a private network or a reverse proxy).
func requireWebhookSecret(who, secret string, allowUnsigned bool) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintf(os.Stderr, "%s: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers\n", who)
		return nil
	}
	return fmt.Errorf("%s: no webhook token configured (webhook.secret) — UptimeRobot cannot sign webhooks, so an unauthenticated listener accepts any POST on the listen address as a real alert. Set it, or set `webhook.allow_unsigned: true` if you genuinely front this with something else that authenticates", who)
}

// --- shared helpers ---

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

func boolv(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		b, _ := strconv.ParseBool(x)
		return b
	}
	return false
}

func main() {
	if err := plugin.Serve(uptimerobotPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-uptimerobot: %v\n", err)
		os.Exit(1)
	}
}
