// Command conductor-google-calendar is a verb-only conductor connector for
// Google Calendar (API v3, https://www.googleapis.com/calendar/v3): calendar
// list, events (list/get/create/update/delete), quick-add natural-language
// events, free/busy queries, and a raw `api` escape hatch for anything a
// first-class verb does not cover. Built ONLY against the public SDK
// (pkg/plugin) — no other dependency.
//
// Authentication is conductor's MANAGED OAuth2 (pkg/plugin v0.15.0+): this
// plugin does NOT perform any token exchange. It declares Google's OAuth2
// endpoints in Describe().Auth, the operator supplies client credentials in
// the connector's daemon-side `auth:` block, and `conductor connector auth
// google-calendar` runs the one-time browser login. The daemon then injects
// a fresh, rotated bearer token into every InvokeRequest.Connection under
// plugin.AccessTokenKey, read here with plugin.AccessToken. Because
// conductor — not this plugin — talks to oauth2.googleapis.com and
// accounts.google.com, the plugin's only egress is www.googleapis.com.
//
// AuthParams sets access_type=offline and prompt=consent: without
// access_type=offline Google never returns a refresh token, and the daemon
// would be unable to keep the connector authenticated past the first access
// token's expiry.
//
// Connection:
//
//	calendar_id: "primary"      # optional; default calendar for every verb (default "primary")
//	api_base: "https://..."     # optional test override, default https://www.googleapis.com/calendar/v3
//
// Every verb's outputs include `status_code`; collection verbs (`calendars`,
// `events`) hoist Google's `items` array into `items`; single-resource verbs
// return `result`. A non-2xx response becomes a CodeInternalError carrying
// the status and body — nothing is swallowed.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
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
)

// defaultAPIBase is the Google Calendar API v3's production host+prefix.
// api_base overrides it for tests.
const defaultAPIBase = "https://www.googleapis.com/calendar/v3"

type googleCalendarPlugin struct {
	client *http.Client
}

func newGoogleCalendarPlugin() *googleCalendarPlugin {
	return &googleCalendarPlugin{client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *googleCalendarPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "google-calendar",
		Desc: "Google Calendar: calendar list, events (list/get/create/update/delete), quick-add, free/busy, and a raw `api` escape hatch over the Calendar API v3. Authenticates via conductor's managed OAuth2 — run `conductor connector auth google-calendar` after configuring an `auth:` block; this plugin never talks to Google's OAuth2 endpoints itself.",
		Connection: plugin.Schema{
			"calendar_id": {Type: "string", Desc: "default calendar id for every verb (default \"primary\"); a verb's own calendar_id option overrides it", Scope: "calendar"},
			"api_base":    {Type: "string", Desc: "override https://www.googleapis.com/calendar/v3 (tests only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "calendars", Desc: "list the calendars on the user's calendar list",
				Usage:   "GET /users/me/calendarList",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "events", Desc: "list events on a calendar",
				Usage: "GET /calendars/{calendar_id}/events",
				Options: plugin.Schema{
					"calendar_id":  {Type: "string", Desc: "override the connection's calendar_id for this call", Scope: "calendar"},
					"timeMin":      {Type: "string", Desc: "RFC3339 lower bound (inclusive) on event end time"},
					"timeMax":      {Type: "string", Desc: "RFC3339 upper bound (exclusive) on event start time"},
					"q":            {Type: "string", Desc: "free text search terms"},
					"maxResults":   {Type: "integer", Desc: "max events per page (Google default 250, max 2500)"},
					"singleEvents": {Type: "boolean", Desc: "expand recurring events into single instances"},
					"orderBy":      {Type: "string", Desc: "\"startTime\" (requires singleEvents=true) or \"updated\""},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "event_get", Desc: "get one event",
				Usage: "GET /calendars/{calendar_id}/events/{event_id}",
				Options: plugin.Schema{
					"calendar_id": {Type: "string", Desc: "override the connection's calendar_id for this call", Scope: "calendar"},
					"event_id":    {Type: "string", Required: true, Scope: "event"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "event_create", Desc: "create an event",
				Usage: "POST /calendars/{calendar_id}/events",
				Options: plugin.Schema{
					"calendar_id": {Type: "string", Desc: "override the connection's calendar_id for this call", Scope: "calendar"},
					"event":       {Type: "map", Desc: "a full Google Calendar Event resource body; overrides the convenience fields below when set"},
					"summary":     {Type: "string", Desc: "convenience: event title"},
					"description": {Type: "string", Desc: "convenience: event description"},
					"start":       {Type: "any", Desc: "convenience: RFC3339 dateTime string, or a {dateTime|date, timeZone} map"},
					"end":         {Type: "any", Desc: "convenience: RFC3339 dateTime string, or a {dateTime|date, timeZone} map"},
					"attendees":   {Type: "list", Desc: "convenience: list of attendee email addresses"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "event_update", Desc: "patch an existing event",
				Usage: "PATCH /calendars/{calendar_id}/events/{event_id}",
				Options: plugin.Schema{
					"calendar_id": {Type: "string", Desc: "override the connection's calendar_id for this call", Scope: "calendar"},
					"event_id":    {Type: "string", Required: true, Scope: "event"},
					"event":       {Type: "map", Required: true, Desc: "the fields to patch, as a Google Calendar Event resource fragment"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "event_delete", Desc: "delete an event",
				Usage: "DELETE /calendars/{calendar_id}/events/{event_id}",
				Options: plugin.Schema{
					"calendar_id": {Type: "string", Desc: "override the connection's calendar_id for this call", Scope: "calendar"},
					"event_id":    {Type: "string", Required: true, Scope: "event"},
				},
				Outputs: plugin.Schema{"status_code": {Type: "integer"}},
			},
			{
				Name: "quick_add", Desc: "create an event from a natural-language description",
				Usage: "POST /calendars/{calendar_id}/events/quickAdd",
				Options: plugin.Schema{
					"calendar_id": {Type: "string", Desc: "override the connection's calendar_id for this call", Scope: "calendar"},
					"text":        {Type: "string", Required: true, Desc: "natural-language event text, e.g. \"Lunch with Sam tomorrow 1pm\""},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "freebusy", Desc: "query free/busy information for one or more calendars",
				Usage: "POST /freeBusy",
				Options: plugin.Schema{
					"timeMin": {Type: "string", Required: true, Desc: "RFC3339 start of the query interval"},
					"timeMax": {Type: "string", Required: true, Desc: "RFC3339 end of the query interval"},
					"items":   {Type: "list", Desc: "calendar ids to query (default: the connection's calendar_id)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any Google Calendar API v3 endpoint (enables writes)",
				Usage: "method + path under /calendar/v3, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under https://www.googleapis.com/calendar/v3, e.g. /users/me/calendarList"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// Conductor performs the OAuth2 token exchange with Google's own
		// endpoints on this plugin's behalf; the plugin itself only ever
		// calls www.googleapis.com.
		Capabilities: plugin.Capabilities{Egress: []string{"www.googleapis.com:443"}},
		Auth: &plugin.AuthSpec{
			Grants:   []string{"authorization_code"},
			TokenURL: "https://oauth2.googleapis.com/token",
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
			Scopes:   []string{"https://www.googleapis.com/auth/calendar"},
			// access_type=offline is REQUIRED for Google to return a refresh
			// token at all; prompt=consent forces the consent screen so a
			// re-auth (e.g. adding scopes later) still yields one.
			AuthParams: map[string]string{"access_type": "offline", "prompt": "consent"},
		},
	}
}

func (p *googleCalendarPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, token, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "calendars":
		return p.calendars(conn, token)
	case "events":
		return p.events(conn, token, o)
	case "event_get":
		return p.eventGet(conn, token, o)
	case "event_create":
		return p.eventCreate(conn, token, o)
	case "event_update":
		return p.eventUpdate(conn, token, o)
	case "event_delete":
		return p.eventDelete(conn, token, o)
	case "quick_add":
		return p.quickAdd(conn, token, o)
	case "freebusy":
		return p.freebusy(conn, token, o)
	case "api":
		return p.api(conn, token, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type gcalConn struct {
	apiBase      string
	defaultCalID string // connection's calendar_id, default "primary"
}

// parseConn reads the connection map and the managed-OAuth2 bearer token
// injected by the daemon. A missing token means the operator has not
// configured an `auth:` block and logged in yet.
func parseConn(m map[string]any) (gcalConn, string, error) {
	token := plugin.AccessToken(m)
	if token == "" {
		return gcalConn{}, "", fmt.Errorf("no access token — add an auth: block and run `conductor connector auth google-calendar`")
	}
	base := strOr(str(m["api_base"]), defaultAPIBase)
	base = strings.TrimRight(base, "/")
	calID := strOr(str(m["calendar_id"]), "primary")
	return gcalConn{apiBase: base, defaultCalID: calID}, token, nil
}

// calendarID resolves the calendar to operate on: the verb's own calendar_id
// option if set, otherwise the connection's default.
func calendarID(conn gcalConn, o map[string]any) string {
	if v := str(o["calendar_id"]); v != "" {
		return v
	}
	return conn.defaultCalID
}

// --- verb implementations ---

func (p *googleCalendarPlugin) calendars(conn gcalConn, token string) (plugin.InvokeResult, error) {
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+"/users/me/calendarList", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *googleCalendarPlugin) events(conn gcalConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["timeMin"]); v != "" {
		q.Set("timeMin", v)
	}
	if v := str(o["timeMax"]); v != "" {
		q.Set("timeMax", v)
	}
	if v := str(o["q"]); v != "" {
		q.Set("q", v)
	}
	if v := intStr(o["maxResults"]); v != "" {
		q.Set("maxResults", v)
	}
	if v, ok := o["singleEvents"]; ok {
		q.Set("singleEvents", boolStr(v))
	}
	if v := str(o["orderBy"]); v != "" {
		q.Set("orderBy", v)
	}
	path := "/calendars/" + url.PathEscape(calendarID(conn, o)) + "/events"
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+path, q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *googleCalendarPlugin) eventGet(conn gcalConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["event_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "event_id is required")
	}
	path := "/calendars/" + url.PathEscape(calendarID(conn, o)) + "/events/" + url.PathEscape(id)
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+path, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleCalendarPlugin) eventCreate(conn gcalConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	body := eventBody(o)
	path := "/calendars/" + url.PathEscape(calendarID(conn, o)) + "/events"
	status, respBody, err := p.do(token, http.MethodPost, conn.apiBase+path, nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleCalendarPlugin) eventUpdate(conn gcalConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["event_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "event_id is required")
	}
	patch, ok := o["event"].(map[string]any)
	if !ok || len(patch) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "event (map of fields to patch) is required")
	}
	path := "/calendars/" + url.PathEscape(calendarID(conn, o)) + "/events/" + url.PathEscape(id)
	status, respBody, err := p.do(token, http.MethodPatch, conn.apiBase+path, nil, patch)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleCalendarPlugin) eventDelete(conn gcalConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["event_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "event_id is required")
	}
	path := "/calendars/" + url.PathEscape(calendarID(conn, o)) + "/events/" + url.PathEscape(id)
	status, _, err := p.do(token, http.MethodDelete, conn.apiBase+path, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"status_code": status}}, nil
}

func (p *googleCalendarPlugin) quickAdd(conn gcalConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	text := str(o["text"])
	if text == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "text is required")
	}
	q := url.Values{"text": []string{text}}
	path := "/calendars/" + url.PathEscape(calendarID(conn, o)) + "/events/quickAdd"
	status, body, err := p.do(token, http.MethodPost, conn.apiBase+path, q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleCalendarPlugin) freebusy(conn gcalConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	timeMin := str(o["timeMin"])
	if timeMin == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "timeMin is required")
	}
	timeMax := str(o["timeMax"])
	if timeMax == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "timeMax is required")
	}
	ids := strList(o["items"])
	if len(ids) == 0 {
		ids = []string{calendarID(conn, o)}
	}
	items := make([]any, len(ids))
	for i, id := range ids {
		items[i] = map[string]any{"id": id}
	}
	body := map[string]any{"timeMin": timeMin, "timeMax": timeMax, "items": items}
	status, respBody, err := p.do(token, http.MethodPost, conn.apiBase+"/freeBusy", nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleCalendarPlugin) api(conn gcalConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	path := str(o["path"])
	if path == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "path is required")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	method := strOr(str(o["method"]), http.MethodGet)
	q := url.Values{}
	if m, ok := o["query"].(map[string]any); ok {
		for k, v := range m {
			q.Set(k, fmt.Sprintf("%v", v))
		}
	}
	status, body, err := p.do(token, method, conn.apiBase+path, q, o["body"])
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	switch v := decoded.(type) {
	case []any:
		out["items"] = v
	default:
		if decoded != nil {
			out["result"] = decoded
		}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// --- event body construction ---

// eventBody builds a Google Calendar Event resource for event_create: the
// `event` map option if given, verbatim; otherwise assembled from the
// convenience summary/description/start/end/attendees options.
func eventBody(o map[string]any) map[string]any {
	if em, ok := o["event"].(map[string]any); ok {
		return em
	}
	body := map[string]any{}
	if v := str(o["summary"]); v != "" {
		body["summary"] = v
	}
	if v := str(o["description"]); v != "" {
		body["description"] = v
	}
	if v := eventTime(o["start"]); v != nil {
		body["start"] = v
	}
	if v := eventTime(o["end"]); v != nil {
		body["end"] = v
	}
	if v := attendeesList(o["attendees"]); v != nil {
		body["attendees"] = v
	}
	return body
}

// eventTime normalizes a start/end convenience option: a map is passed
// through as-is (the caller already used Google's {dateTime|date, timeZone}
// shape), a non-empty string is treated as an RFC3339 dateTime shorthand.
func eventTime(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case string:
		if x == "" {
			return nil
		}
		return map[string]any{"dateTime": x}
	}
	return nil
}

// attendeesList converts a list of email address strings into Google's
// []{"email": "..."} attendee shape.
func attendeesList(v any) []any {
	emails := strList(v)
	if len(emails) == 0 {
		return nil
	}
	out := make([]any, len(emails))
	for i, e := range emails {
		out[i] = map[string]any{"email": e}
	}
	return out
}

// --- HTTP plumbing ---

// do performs one HTTP request against the Google Calendar API, attaching
// the Bearer token, and returns the status code and raw response body. A
// non-2xx status is translated into a CodeInternalError carrying the status
// and body — callers never need to check status codes themselves.
func (p *googleCalendarPlugin) do(token, method, endpoint string, query url.Values, body any) (int, []byte, error) {
	full := endpoint
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, respBody, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s %s: %d %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(respBody))))
	}
	return resp.StatusCode, respBody, nil
}

// decodeJSON decodes a JSON response body into a generic value. An empty
// body decodes to nil rather than an error (e.g. a 204 with no body).
func decodeJSON(body []byte) (any, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// hoist pulls the `items` list out of a decoded Google Calendar list
// response (e.g. {"items": [...], "nextPageToken": "..."}). A bare list is
// returned as-is. Anything else yields an empty (never nil) list, so callers
// get a consistent [] rather than null on the wire.
func hoist(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		if list, ok := x["items"].([]any); ok {
			return list
		}
	}
	return []any{}
}

func main() {
	if err := plugin.Serve(newGoogleCalendarPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-google-calendar:", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) ---

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
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

// boolStr renders an option as a literal "true"/"false" query value.
func boolStr(v any) string {
	if boolv(v) {
		return "true"
	}
	return "false"
}

// strList reads a list-of-strings option: a []any of strings (the wire
// shape), a []string, or a single string. Empty entries are dropped.
func strList(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return x
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	}
	return nil
}

// intStr renders an integer-ish option as a string ("" if absent).
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
	return fmt.Sprintf("%v", v)
}
