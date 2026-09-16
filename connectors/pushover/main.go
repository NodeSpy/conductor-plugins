// Command conductor-pushover is a conductor connector plugin (#59) for
// Pushover: it verbs the Pushover REST API (send notifications, validate
// users/groups, check emergency-priority delivery receipts, glances, and a
// generic API escape hatch) over net/http. Built ONLY against the public SDK
// (pkg/plugin) and the standard library — no third-party client.
//
// Pushover is outbound only: there is no webhook to receive, so this is a
// verb-only connector (no Events, no StartSource).
//
// Pushover's REST API takes application/x-www-form-urlencoded request bodies
// for POST (query strings for GET) and returns JSON; auth is a token+user pair
// carried as regular request parameters (not a header), mirroring the other
// form-encoded REST connectors in this repo (see connectors/twilio).
//
// Connection:
//
//	token: "<application API token>"  # required
//	user:  "<user or group key>"      # required
//	api_base: "https://api.pushover.net/1" # override (tests, or a proxy)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// defaultAPIBase is Pushover's REST API origin. Overridable via connection
// api_base so tests point this at an httptest.Server instead.
const defaultAPIBase = "https://api.pushover.net/1"

type pushoverPlugin struct{}

func (pushoverPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "pushover",
		Desc: "Pushover: send push notifications (including emergency priority), validate users/groups, check delivery receipts, post glances, and a generic API escape hatch.",
		Connection: plugin.Schema{
			"token":    {Type: "string", Required: true, Desc: "Pushover application API token"},
			"user":     {Type: "string", Required: true, Desc: "Pushover user or group key"},
			"api_base": {Type: "string", Desc: "override the Pushover API base URL (tests, or a private gateway)"},
		},
		Verbs: pushoverVerbs(),
		// The permission manifest conductor records at install: this plugin
		// only ever calls the Pushover API, and spawns nothing.
		Capabilities: plugin.Capabilities{Egress: []string{"api.pushover.net:443"}},
	}
}

func pushoverVerbs() []plugin.Verb {
	resultStatus := plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}}
	return []plugin.Verb{
		{
			Name: "send", Desc: "send a push notification",
			Usage: "priority 2 (emergency) requires retry and expire; the response's `result` includes a receipt for priority 2",
			Options: plugin.Schema{
				"message":   {Type: "string", Required: true},
				"title":     {Type: "string"},
				"priority":  {Type: "integer", Desc: "-2 (lowest) .. 2 (emergency)"},
				"url":       {Type: "string"},
				"url_title": {Type: "string"},
				"sound":     {Type: "string"},
				"device":    {Type: "string"},
				"html":      {Type: "boolean", Desc: "enable HTML formatting in message"},
				"monospace": {Type: "boolean", Desc: "render message in a monospace font"},
				"timestamp": {Type: "integer", Desc: "unix timestamp to display instead of the receipt time"},
				"retry":     {Type: "integer", Desc: "seconds between retries; required (with expire) when priority is 2"},
				"expire":    {Type: "integer", Desc: "seconds until Pushover stops retrying; required (with retry) when priority is 2"},
				"tags":      {Type: "string", Desc: "comma-separated tags, usable to cancel a priority-2 notification by tag"},
			},
			Outputs: resultStatus,
		},
		{
			Name: "validate_user", Desc: "validate a user or group key (and optional device)",
			Options: plugin.Schema{
				"user":   {Type: "string", Required: true, Desc: "user or group key to validate"},
				"device": {Type: "string"},
			},
			Outputs: resultStatus,
		},
		{
			Name: "get_receipt", Desc: "check the delivery status of an emergency-priority (priority 2) notification",
			Options: plugin.Schema{"receipt": {Type: "string", Required: true, Scope: "receipt"}},
			Outputs: resultStatus,
		},
		{
			Name: "cancel_receipt", Desc: "cancel further retries of an emergency-priority (priority 2) notification",
			Options: plugin.Schema{"receipt": {Type: "string", Required: true, Scope: "receipt"}},
			Outputs: resultStatus,
		},
		{
			Name: "sounds", Desc: "list the notification sound names Pushover supports",
			Outputs: resultStatus,
		},
		{
			Name: "glances", Desc: "post a glance update (Apple Watch / lock-screen style widget)",
			Options: plugin.Schema{
				"title":   {Type: "string"},
				"text":    {Type: "string"},
				"subtext": {Type: "string"},
				"count":   {Type: "integer"},
				"percent": {Type: "integer"},
				"device":  {Type: "string"},
			},
			Outputs: resultStatus,
		},
		{
			Name: "api", Desc: "call any Pushover REST endpoint not covered by a first-class verb",
			Usage: "escape hatch: method + path relative to the API base, params form-encoded for POST / query for GET (token/user are NOT auto-included — add them via params if the endpoint needs them)",
			Options: plugin.Schema{
				"method": {Type: "string", Required: true, Enum: []string{"GET", "POST"}},
				"path":   {Type: "string", Required: true, Desc: "path relative to the API base, e.g. \"messages.json\""},
				"params": {Type: "map", Desc: "form-encoded for POST, query string for GET"},
			},
			Outputs: resultStatus,
		},
	}
}

// pushoverConn is the resolved connection config for one invocation.
type pushoverConn struct {
	token string
	user  string
	base  string
}

func parseConn(m map[string]any) (pushoverConn, error) {
	token := str(m["token"])
	user := str(m["user"])
	if token == "" {
		return pushoverConn{}, fmt.Errorf("connection.token is required")
	}
	if user == "" {
		return pushoverConn{}, fmt.Errorf("connection.user is required")
	}
	return pushoverConn{token: token, user: user, base: strOr(m["api_base"], defaultAPIBase)}, nil
}

func (pushoverPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	call, err := verbCall(req.Verb, o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}

	outputs, err := conn.do(call)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

// apiCall is the resolved HTTP request one verb builds, before it is sent.
type apiCall struct {
	method string
	path   string
	params url.Values
	// withUser, when true, makes do() inject the connection's user into
	// params. token is injected unconditionally — every Pushover endpoint
	// used by a first-class verb requires it — but only send/glances also
	// need the connection's own user; validate_user takes its own `user`
	// option instead (a caller can validate ANY user/group, not just the
	// connection's own), and get_receipt/cancel_receipt/sounds need neither.
	withUser bool
}

// verbCall builds the HTTP call for one verb. Pure and hermetically
// testable — no request is sent here.
func verbCall(verb string, o map[string]any) (apiCall, error) {
	switch verb {
	case "send":
		return sendCall(o)
	case "validate_user":
		return validateUserCall(o)
	case "get_receipt":
		return getReceiptCall(o)
	case "cancel_receipt":
		return cancelReceiptCall(o)
	case "sounds":
		return apiCall{method: http.MethodGet, path: "sounds.json"}, nil
	case "glances":
		return glancesCall(o)
	case "api":
		return apiVerbCall(o)
	}
	return apiCall{}, fmt.Errorf("unknown verb")
}

func sendCall(o map[string]any) (apiCall, error) {
	message := str(o["message"])
	if message == "" {
		return apiCall{}, fmt.Errorf("message is required")
	}
	p := url.Values{"message": {message}}
	if v := str(o["title"]); v != "" {
		p.Set("title", v)
	}

	hasPriority2 := false
	if raw, ok := o["priority"]; ok {
		n, err := toInt(raw)
		if err != nil {
			return apiCall{}, fmt.Errorf("priority: %w", err)
		}
		if n < -2 || n > 2 {
			return apiCall{}, fmt.Errorf("priority must be between -2 and 2")
		}
		p.Set("priority", strconv.Itoa(n))
		hasPriority2 = n == 2
	}
	if v := str(o["url"]); v != "" {
		p.Set("url", v)
	}
	if v := str(o["url_title"]); v != "" {
		p.Set("url_title", v)
	}
	if v := str(o["sound"]); v != "" {
		p.Set("sound", v)
	}
	if v := str(o["device"]); v != "" {
		p.Set("device", v)
	}
	if boolv(o["html"]) {
		p.Set("html", "1")
	}
	if boolv(o["monospace"]) {
		p.Set("monospace", "1")
	}
	if v := intStr(o["timestamp"]); v != "" {
		p.Set("timestamp", v)
	}
	retry := intStr(o["retry"])
	expire := intStr(o["expire"])
	if retry != "" {
		p.Set("retry", retry)
	}
	if expire != "" {
		p.Set("expire", expire)
	}
	if v := str(o["tags"]); v != "" {
		p.Set("tags", v)
	}

	if hasPriority2 && (retry == "" || expire == "") {
		return apiCall{}, fmt.Errorf("priority 2 requires retry and expire")
	}

	return apiCall{method: http.MethodPost, path: "messages.json", params: p, withUser: true}, nil
}

func validateUserCall(o map[string]any) (apiCall, error) {
	user := str(o["user"])
	if user == "" {
		return apiCall{}, fmt.Errorf("user is required")
	}
	p := url.Values{"user": {user}}
	if d := str(o["device"]); d != "" {
		p.Set("device", d)
	}
	return apiCall{method: http.MethodPost, path: "users/validate.json", params: p}, nil
}

func getReceiptCall(o map[string]any) (apiCall, error) {
	receipt := str(o["receipt"])
	if receipt == "" {
		return apiCall{}, fmt.Errorf("receipt is required")
	}
	return apiCall{method: http.MethodGet, path: "receipts/" + receipt + ".json"}, nil
}

func cancelReceiptCall(o map[string]any) (apiCall, error) {
	receipt := str(o["receipt"])
	if receipt == "" {
		return apiCall{}, fmt.Errorf("receipt is required")
	}
	return apiCall{method: http.MethodPost, path: "receipts/" + receipt + "/cancel.json"}, nil
}

func glancesCall(o map[string]any) (apiCall, error) {
	p := url.Values{}
	if v := str(o["title"]); v != "" {
		p.Set("title", v)
	}
	if v := str(o["text"]); v != "" {
		p.Set("text", v)
	}
	if v := str(o["subtext"]); v != "" {
		p.Set("subtext", v)
	}
	if v := intStr(o["count"]); v != "" {
		p.Set("count", v)
	}
	if v := intStr(o["percent"]); v != "" {
		p.Set("percent", v)
	}
	if v := str(o["device"]); v != "" {
		p.Set("device", v)
	}
	return apiCall{method: http.MethodPost, path: "glances.json", params: p, withUser: true}, nil
}

func apiVerbCall(o map[string]any) (apiCall, error) {
	method := strings.ToUpper(str(o["method"]))
	if method == "" {
		return apiCall{}, fmt.Errorf("method is required")
	}
	path := strings.TrimPrefix(str(o["path"]), "/")
	if path == "" {
		return apiCall{}, fmt.Errorf("path is required")
	}
	p := url.Values{}
	if m, ok := o["params"].(map[string]any); ok {
		for k, v := range m {
			p.Set(k, fmt.Sprintf("%v", v))
		}
	}
	return apiCall{method: method, path: path, params: p}, nil
}

// do sends one HTTP request to the Pushover REST API and decodes the JSON
// response into outputs. Pushover's request bodies are form-encoded for POST;
// GET carries params as a query string instead. token is injected
// unconditionally; user is injected only when call.withUser is set. A
// non-2xx response is a plugin.CodeInternalError carrying the status and
// response body; verb callers never see a raw *http.Response.
func (c pushoverConn) do(call apiCall) (map[string]any, error) {
	params := call.params
	if params == nil {
		params = url.Values{}
	}
	params.Set("token", c.token)
	if call.withUser {
		params.Set("user", c.user)
	}

	base := strings.TrimRight(c.base, "/") + "/" + call.path

	var reqURL string
	var reader io.Reader
	if call.method == http.MethodGet {
		reqURL = base
		if len(params) > 0 {
			reqURL += "?" + params.Encode()
		}
	} else {
		reqURL = base
		reader = strings.NewReader(params.Encode())
	}

	httpReq, err := http.NewRequest(call.method, reqURL, reader)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if reader != nil {
		httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, "reading response: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("pushover API %s: %s", resp.Status, strings.TrimSpace(string(raw))))
	}

	var result any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, plugin.Errorf(plugin.CodeInternalError, "decoding response: "+err.Error())
		}
	}
	return map[string]any{"result": result, "status_code": resp.StatusCode}, nil
}

func main() {
	if err := plugin.Serve(pushoverPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-pushover:", err)
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

// intStr renders an integer-ish option as a string flag value ("" if absent).
func intStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case string:
		return x
	}
	return ""
}

// toInt parses an integer-ish option, erroring on anything that isn't one
// (used where the value's validity, not just its presence, matters).
func toInt(v any) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case float64:
		return int(x), nil
	case string:
		n, err := strconv.Atoi(x)
		if err != nil {
			return 0, fmt.Errorf("invalid integer %q", x)
		}
		return n, nil
	}
	return 0, fmt.Errorf("invalid integer %v", v)
}
