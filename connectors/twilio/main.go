// Command conductor-twilio is a conductor connector plugin (#59) for Twilio:
// it verbs the Twilio REST API (SMS/WhatsApp/voice) over net/http, and
// sources inbound SMS/voice webhooks Twilio POSTs to a configured URL.
// Built ONLY against the public SDK (pkg/plugin) and the standard library —
// no third-party client.
//
// Twilio's REST API takes application/x-www-form-urlencoded request bodies
// (not JSON) and returns JSON; auth is HTTP Basic (account_sid:auth_token).
// Verbs build the form/query themselves and decode the JSON response into
// `result` (or `messages` for the list verb), mirroring the other REST
// connectors in this repo (see connectors/notion).
//
// As a source, Twilio's webhook signature scheme is its own thing — HMAC-SHA1
// over the request URL Twilio hit plus the sorted POST params concatenated,
// base64-encoded — not the HMAC-SHA256-over-raw-body scheme sourcekit.Listener
// verifies. So the listener's own Secret is left empty (which disables
// sourcekit's verification) and this plugin does the Twilio-specific check
// itself, once the body is parsed into form params. See verifyTwilioSignature.
// Note: over a smee relay, the URL Twilio actually signed is whatever it was
// configured to POST to (typically the relay channel URL), not public_url —
// see webhook.smee below.
//
// Config (per start_source, under `webhook:`):
//
//	account_sid: "AC..."             # connection: Twilio account SID
//	auth_token: "..."                # connection: Twilio auth token
//	webhook:
//	  listen: ":9097"                # HTTP listener address (optional if smee is set)
//	  path: "/twilio"                # request path (default /twilio)
//	  validate: true                 # verify X-Twilio-Signature (default true)
//	  public_url: "https://example.com" # externally-reachable base URL Twilio posts to
//	  smee: "https://smee.io/AbC123" # optional smee.io-style SSE relay for endpoints with no public URL
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// defaultAPIBase is Twilio's REST API origin, up to but excluding the
// Account SID segment. Overridable via connection `api_base` so tests point
// this at an httptest.Server instead.
const defaultAPIBase = "https://api.twilio.com/2010-04-01"

type twilioPlugin struct{}

func (twilioPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "twilio",
		Desc: "Twilio: send/receive SMS and WhatsApp messages, place calls, and drive the REST API generically; source inbound SMS/voice webhooks (X-Twilio-Signature verified).",
		Connection: plugin.Schema{
			"account_sid": {Type: "string", Required: true, Desc: "Twilio Account SID"},
			"auth_token":  {Type: "string", Required: true, Desc: "Twilio Auth Token (also the webhook signing secret)"},
			"webhook":     {Type: "map", Desc: "source transport: listen, path, validate, public_url, smee (smee.io-style SSE relay URL — when set, note that the relayed request URL differs from public_url, so set validate: false or configure public_url to match what the relay reports)"},
			"api_base":    {Type: "string", Desc: "override the Twilio API base URL (tests, or a private gateway)"},
		},
		Events: []plugin.Event{
			{
				Name: "sms", Desc: "an incoming SMS/MMS message",
				Filters: plugin.Schema{
					"froms": {Type: "list", Desc: "sender numbers (E.164), any match"},
					"tos":   {Type: "list", Desc: "recipient numbers (E.164), any match"},
					"from":  {Type: "string"},
					"to":    {Type: "string"},
				},
				Context: plugin.Schema{
					"from": {Type: "string"}, "to": {Type: "string"},
					"body": {Type: "string"}, "message_sid": {Type: "string"},
					"num_media": {Type: "integer"},
				},
			},
			{
				Name: "call", Desc: "an incoming voice call",
				Filters: plugin.Schema{
					"froms": {Type: "list", Desc: "caller numbers (E.164), any match"},
					"tos":   {Type: "list", Desc: "called numbers (E.164), any match"},
					"from":  {Type: "string"},
					"to":    {Type: "string"},
				},
				Context: plugin.Schema{
					"from": {Type: "string"}, "to": {Type: "string"},
					"call_sid": {Type: "string"}, "call_status": {Type: "string"},
				},
			},
		},
		Verbs: twilioVerbs(),
		// The permission manifest conductor records at install: this plugin
		// only ever calls the Twilio REST API, and spawns nothing.
		Capabilities: plugin.Capabilities{Egress: []string{"api.twilio.com:443"}},
	}
}

func twilioVerbs() []plugin.Verb {
	sidStatus := plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}}
	return []plugin.Verb{
		{
			Name: "send_sms", Desc: "send an SMS message",
			Options: plugin.Schema{
				"from":      {Type: "string", Required: true, Desc: "sending number (E.164) or Messaging Service SID"},
				"to":        {Type: "string", Required: true, Desc: "recipient number (E.164)"},
				"body":      {Type: "string", Required: true},
				"media_url": {Type: "list", Desc: "MMS media URLs"},
			},
			Outputs: sidStatus,
		},
		{
			Name: "send_whatsapp", Desc: "send a WhatsApp message (from/to are auto-prefixed whatsapp: if missing)",
			Options: plugin.Schema{
				"from": {Type: "string", Required: true, Desc: "WhatsApp-enabled sender number"},
				"to":   {Type: "string", Required: true, Desc: "recipient WhatsApp number"},
				"body": {Type: "string", Required: true},
			},
			Outputs: sidStatus,
		},
		{
			Name: "get_message", Desc: "read a message's status/body by SID",
			Options: plugin.Schema{"sid": {Type: "string", Required: true, Scope: "message"}},
			Outputs: sidStatus,
		},
		{
			Name: "list_messages", Desc: "list recent messages, optionally filtered",
			Options: plugin.Schema{
				"to":        {Type: "string"},
				"from":      {Type: "string"},
				"date_sent": {Type: "string", Desc: "YYYY-MM-DD"},
				"page_size": {Type: "integer"},
			},
			Outputs: plugin.Schema{"messages": {Type: "list"}, "result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "make_call", Desc: "place an outbound call",
			Usage: "provide exactly one of url (TwiML URL Twilio fetches) or twiml (inline TwiML document)",
			Options: plugin.Schema{
				"from":  {Type: "string", Required: true},
				"to":    {Type: "string", Required: true},
				"url":   {Type: "string", Desc: "TwiML URL Twilio requests to control the call"},
				"twiml": {Type: "string", Desc: "inline TwiML document"},
			},
			Outputs: sidStatus,
		},
		{
			Name: "get_call", Desc: "read a call's status by SID",
			Options: plugin.Schema{"sid": {Type: "string", Required: true, Scope: "call"}},
			Outputs: sidStatus,
		},
		{
			Name: "api", Desc: "call any Twilio REST endpoint not covered by a first-class verb",
			Usage: "escape hatch: method + path relative to /Accounts/{account_sid}, params form-encoded for POST / query for GET",
			Options: plugin.Schema{
				"method": {Type: "string", Required: true, Enum: []string{"GET", "POST", "DELETE"}},
				"path":   {Type: "string", Required: true, Desc: "path relative to /Accounts/{account_sid}, e.g. \"Messages.json\""},
				"params": {Type: "map", Desc: "form-encoded for POST, query string for GET"},
			},
			Outputs: sidStatus,
		},
	}
}

// twilioConn is the resolved connection config for one invocation.
type twilioConn struct {
	accountSID string
	authToken  string
	base       string
}

func parseConn(m map[string]any) (twilioConn, error) {
	sid := str(m["account_sid"])
	token := str(m["auth_token"])
	if sid == "" {
		return twilioConn{}, fmt.Errorf("connection.account_sid is required")
	}
	if token == "" {
		return twilioConn{}, fmt.Errorf("connection.auth_token is required")
	}
	return twilioConn{
		accountSID: sid,
		authToken:  token,
		base:       strOr(m["api_base"], defaultAPIBase),
	}, nil
}

func (twilioPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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

	outputs, err := conn.do(call.method, call.path, call.params)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	if call.resultsKey != "" {
		if m, ok := outputs["result"].(map[string]any); ok {
			if v, ok := m[call.resultsKey]; ok {
				outputs[call.resultsKey] = v
			}
		}
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

// apiCall is the resolved HTTP request one verb builds, before it is sent.
type apiCall struct {
	method string
	path   string
	params url.Values
	// resultsKey, if set, hoists that key out of the decoded JSON object
	// (Twilio wraps list endpoints as {"messages": [...], ...}) into a
	// top-level output alongside `result`.
	resultsKey string
}

// verbCall builds the HTTP call for one verb. Pure and hermetically
// testable — no request is sent here.
func verbCall(verb string, o map[string]any) (apiCall, error) {
	switch verb {
	case "send_sms":
		return sendMessageCall(o, false)
	case "send_whatsapp":
		return sendMessageCall(o, true)
	case "get_message":
		sid := str(o["sid"])
		if sid == "" {
			return apiCall{}, fmt.Errorf("sid is required")
		}
		return apiCall{method: http.MethodGet, path: "Messages/" + sid + ".json"}, nil
	case "list_messages":
		p := url.Values{}
		if v := str(o["to"]); v != "" {
			p.Set("To", v)
		}
		if v := str(o["from"]); v != "" {
			p.Set("From", v)
		}
		if v := str(o["date_sent"]); v != "" {
			p.Set("DateSent", v)
		}
		if v := intStr(o["page_size"]); v != "" {
			p.Set("PageSize", v)
		}
		return apiCall{method: http.MethodGet, path: "Messages.json", params: p, resultsKey: "messages"}, nil
	case "make_call":
		return makeCallCall(o)
	case "get_call":
		sid := str(o["sid"])
		if sid == "" {
			return apiCall{}, fmt.Errorf("sid is required")
		}
		return apiCall{method: http.MethodGet, path: "Calls/" + sid + ".json"}, nil
	case "api":
		return apiVerbCall(o)
	}
	return apiCall{}, fmt.Errorf("unknown verb")
}

func sendMessageCall(o map[string]any, whatsapp bool) (apiCall, error) {
	from, to := str(o["from"]), str(o["to"])
	body := str(o["body"])
	if from == "" {
		return apiCall{}, fmt.Errorf("from is required")
	}
	if to == "" {
		return apiCall{}, fmt.Errorf("to is required")
	}
	if body == "" {
		return apiCall{}, fmt.Errorf("body is required")
	}
	if whatsapp {
		from, to = withWhatsAppPrefix(from), withWhatsAppPrefix(to)
	}
	p := url.Values{"From": {from}, "To": {to}, "Body": {body}}
	for _, m := range strList(o["media_url"]) {
		p.Add("MediaUrl", m)
	}
	return apiCall{method: http.MethodPost, path: "Messages.json", params: p}, nil
}

// withWhatsAppPrefix adds the "whatsapp:" scheme Twilio requires on both
// endpoints of a WhatsApp message, unless it is already present.
func withWhatsAppPrefix(number string) string {
	if strings.HasPrefix(number, "whatsapp:") {
		return number
	}
	return "whatsapp:" + number
}

func makeCallCall(o map[string]any) (apiCall, error) {
	from, to := str(o["from"]), str(o["to"])
	if from == "" {
		return apiCall{}, fmt.Errorf("from is required")
	}
	if to == "" {
		return apiCall{}, fmt.Errorf("to is required")
	}
	twiURL, twiml := str(o["url"]), str(o["twiml"])
	if (twiURL == "") == (twiml == "") {
		return apiCall{}, fmt.Errorf("exactly one of url or twiml is required")
	}
	p := url.Values{"From": {from}, "To": {to}}
	if twiURL != "" {
		p.Set("Url", twiURL)
	} else {
		p.Set("Twiml", twiml)
	}
	return apiCall{method: http.MethodPost, path: "Calls.json", params: p}, nil
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

// do sends one HTTP request to the Twilio REST API, attaching Basic auth, and
// decodes the JSON response into outputs. Twilio's request bodies are
// form-encoded for POST; GET/DELETE carry params as a query string instead. A
// non-2xx response is a plugin.CodeInternalError carrying the status and
// response body; verb callers never see a raw *http.Response.
func (c twilioConn) do(method, path string, params url.Values) (map[string]any, error) {
	base := strings.TrimRight(c.base, "/") + "/Accounts/" + c.accountSID + "/" + path

	var reqURL string
	var reader io.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		reqURL = base
		if len(params) > 0 {
			reqURL += "?" + params.Encode()
		}
	} else {
		reqURL = base
		reader = bytes.NewReader([]byte(params.Encode()))
	}

	httpReq, err := http.NewRequest(method, reqURL, reader)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	httpReq.SetBasicAuth(c.accountSID, c.authToken)
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
		return nil, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("twilio API %s: %s", resp.Status, strings.TrimSpace(string(raw))))
	}

	var result any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, plugin.Errorf(plugin.CodeInternalError, "decoding response: "+err.Error())
		}
	}
	return map[string]any{"result": result, "status_code": resp.StatusCode}, nil
}

// --- source: inbound SMS/voice webhooks ---

func (twilioPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	authToken := str(cfg["auth_token"])

	addr, path, publicURL, relay := "", "/twilio", "", ""
	validate := true
	if webhook != nil {
		addr = str(webhook["listen"])
		if p := str(webhook["path"]); p != "" {
			path = p
		}
		publicURL = str(webhook["public_url"])
		relay = str(webhook["smee"])
		if v, ok := webhook["validate"]; ok {
			validate = boolv(v)
		}
	}
	if addr == "" && relay == "" {
		return fmt.Errorf("twilio: no webhook.listen address or webhook.smee relay configured")
	}
	if err := requireSignatureConfig(validate, authToken, publicURL); err != nil {
		return err
	}
	requestURL := strings.TrimRight(publicURL, "/") + path

	// sourcekit.Listener's own HMAC verification is HMAC-SHA256 over the raw
	// body (GitHub/Sentry/PagerDuty shape); Twilio's scheme is HMAC-SHA1 over
	// the request URL plus the sorted form params, so it does not fit. Leave
	// Secret empty (VerifyHMAC then passes everything through) and verify the
	// Twilio way ourselves below, once the body is parsed into form params.
	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: relay}
	dedup := sourcekit.NewDedup(2048)
	if addr != "" {
		fmt.Fprintf(os.Stderr, "twilio[%s]: listening on %s%s\n", req.Instance, ln.Addr, ln.Path)
	}
	if relay != "" {
		fmt.Fprintf(os.Stderr, "twilio[%s]: relaying via smee channel %s\n", req.Instance, relay)
	}
	return ln.Serve(ctx, func(h http.Header, body []byte) {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return
		}
		if validate {
			sig := h.Get("X-Twilio-Signature")
			if !verifyTwilioSignature(authToken, requestURL, form, sig) {
				fmt.Fprintf(os.Stderr, "twilio[%s]: rejected webhook with bad X-Twilio-Signature\n", req.Instance)
				return
			}
		} else {
			fmt.Fprintf(os.Stderr, "twilio[%s]: validate is false — accepting UNSIGNED webhooks; anyone who can reach the listen address can fire triggers\n", req.Instance)
		}
		ev, dk := twilioEvent(form)
		if ev == nil {
			return
		}
		if !dedup.Add(dk) {
			return
		}
		_ = emit(ev)
	})
}

// requireSignatureConfig refuses to start an unauthenticated webhook
// listener.
//
// Twilio validation needs BOTH the auth token (the HMAC key) and the exact
// public URL Twilio hit (part of the signed data) — missing either makes
// `validate: true` (the default) impossible to honor. Failing closed here
// mirrors requireWebhookSecret in the other source connectors in this repo:
// a missing prerequisite is far more often a mistake than a choice, so
// `validate: false` is the explicit, greppable way to say you meant it.
func requireSignatureConfig(validate bool, authToken, publicURL string) error {
	if !validate {
		return nil
	}
	var missing []string
	if authToken == "" {
		missing = append(missing, "auth_token")
	}
	if publicURL == "" {
		missing = append(missing, "webhook.public_url")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("twilio: webhook.validate is true but missing %s — an unverified listener accepts any POST on the listen address as a real event. Set them, or set `webhook.validate: false` if you genuinely front this with something else that authenticates", strings.Join(missing, ", "))
}

// verifyTwilioSignature reimplements Twilio's request-validation scheme:
// HMAC-SHA1, keyed by the auth token, over the exact URL Twilio requested
// with each POST parameter's key+value (sorted by key, no separators)
// appended, base64-encoded, compared constant-time to X-Twilio-Signature.
// https://www.twilio.com/docs/usage/webhooks/webhooks-security
func verifyTwilioSignature(authToken, requestURL string, form url.Values, signature string) bool {
	if signature == "" {
		return false
	}
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var data strings.Builder
	data.WriteString(requestURL)
	for _, k := range keys {
		data.WriteString(k)
		data.WriteString(form.Get(k))
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(data.String()))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// twilioEvent classifies a parsed webhook body as an "sms" or "call" event
// (distinguished by the presence of Body/MessageSid vs CallSid, per Twilio's
// two request shapes) and returns its normalized emit payload plus its dedup
// key. Returns (nil, "") for a body that matches neither shape.
func twilioEvent(form url.Values) (map[string]any, string) {
	messageSid := firstNonEmpty(form.Get("MessageSid"), form.Get("SmsSid"))
	callSid := form.Get("CallSid")
	switch {
	case messageSid != "" || form.Get("Body") != "":
		from, to := form.Get("From"), form.Get("To")
		return map[string]any{
			"event": "sms",
			"kind":  "sms",
			"title": fmt.Sprintf("twilio sms from %s to %s", from, to),
			"dedup": messageSid,
			"context": map[string]any{
				"from": from, "to": to, "body": form.Get("Body"),
				"message_sid": messageSid, "num_media": atoi(form.Get("NumMedia")),
				// Plural aliases for the daemon's generic list-contains filter
				// evaluator (filters: {froms/tos: [...]}).
				"froms": from, "tos": to,
			},
		}, messageSid
	case callSid != "":
		from, to := form.Get("From"), form.Get("To")
		return map[string]any{
			"event": "call",
			"kind":  "call",
			"title": fmt.Sprintf("twilio call from %s to %s", from, to),
			"dedup": callSid,
			"context": map[string]any{
				"from": from, "to": to,
				"call_sid": callSid, "call_status": form.Get("CallStatus"),
				"froms": from, "tos": to,
			},
		}, callSid
	}
	return nil, ""
}

func main() {
	if err := plugin.Serve(twilioPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-twilio:", err)
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

func intStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return fmt.Sprintf("%d", int64(x))
	case string:
		return x
	}
	return ""
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// strList accepts a string, []string, or []any and returns a non-empty slice.
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
			if s := fmt.Sprintf("%v", e); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
