// Command conductor-authentik is a verb-only conductor connector for
// authentik, a self-hosted identity provider / SSO platform. It drives the
// authentik REST API (v3) over net/http: users (list/get/create/update/
// delete), groups, applications, providers, flows, events, tokens, and a raw
// `api` escape hatch for anything a first-class verb does not cover. Built
// ONLY against the public SDK (pkg/plugin) — no other dependency.
//
// authentik authenticates its API with a bearer API token (a "token" created
// under Directory > Tokens, or a service-account token): every request
// carries `Authorization: Bearer <api_token>`. This connector has no source:
// while authentik can emit outpost/webhook-style notifications in some
// deployments, there is no first-class, stable event-stream endpoint this
// connector can commit to parsing generically. It has no Events and does not
// implement SourceHandler.
//
// Connection:
//
//	base_url:              "https://authentik.example.com"  # required; no trailing /api
//	api_token:             "<api token>"                    # required; sent as Authorization: Bearer <api_token>
//	insecure_skip_verify:  false                             # optional; see risk note below
//
// Every request goes to base_url + "/api/v3" + <endpoint>, authenticated
// with the Bearer token. A non-2xx response is returned as a
// CodeInternalError carrying the status code and response body — nothing is
// swallowed.
//
// authentik paginates list endpoints as {"pagination": {...}, "results": [...]}.
// Every list verb hoists `results` into `items` for convenience, while also
// returning the full decoded body as `result` so pagination metadata
// (count/next/previous/etc.) survives. Single-object verbs return `result`
// only. authentik's DRF routers require a trailing slash on collection
// (and single-resource) paths; every first-class verb's endpoint already
// carries it — callers of the `api` escape hatch must add their own.
//
// insecure_skip_verify disables TLS certificate verification. Self-hosted
// authentik instances sometimes run behind a self-signed certificate on a
// LAN, so this exists as an explicit, greppable opt-out — but it also
// disables all protection against a man-in-the-middle on the path to the
// identity provider (which then sees the API token). Only enable it for
// instances reached over a trusted network, and prefer installing a real
// certificate (e.g. via a reverse proxy with ACME/Let's Encrypt) instead
// when possible.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type authentikPlugin struct{}

func newAuthentikPlugin() *authentikPlugin { return &authentikPlugin{} }

func (p *authentikPlugin) Describe() plugin.Decl {
	userFields := plugin.Schema{
		"name":       {Type: "string", Desc: "display name"},
		"email":      {Type: "string"},
		"is_active":  {Type: "boolean"},
		"groups":     {Type: "list", Desc: "group PKs/UUIDs this user belongs to"},
		"path":       {Type: "string", Desc: "authentik user path (default \"users\")"},
		"type":       {Type: "string", Desc: "user type, e.g. internal, external, service_account"},
		"attributes": {Type: "map", Desc: "arbitrary user attributes"},
	}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "authentik",
		Desc: "authentik: users, groups, applications, providers, flows, events, and tokens over the authentik REST API (v3), plus a raw `api` escape hatch. Self-hosted; declares no egress (narrow with network: per instance). No source — no stable event-stream endpoint to commit to.",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "authentik instance root, e.g. https://authentik.example.com (no trailing /api)"},
			"api_token":            {Type: "string", Required: true, Desc: "authentik API token, sent as Authorization: Bearer <api_token>"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification (default false); self-signed certs happen on LAN deployments, but this disables protection against MITM — only enable for trusted networks"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "users", Desc: "list users",
				Usage: "GET /core/users/",
				Options: plugin.Schema{
					"search":    {Type: "string", Desc: "free-text search"},
					"is_active": {Type: "boolean", Desc: "filter by active state"},
					"ordering":  {Type: "string", Desc: "field to order by, e.g. username or -username"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.results"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "user_get", Desc: "get one user's details",
				Usage: "GET /core/users/{id}/",
				Options: plugin.Schema{
					"user_id": {Type: "string", Required: true, Scope: "user"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "user_create", Desc: "create a user",
				Usage: "POST /core/users/",
				Options: merge(plugin.Schema{
					"username": {Type: "string", Required: true},
				}, userFields),
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "user_update", Desc: "partially update a user",
				Usage: "PATCH /core/users/{id}/",
				Options: merge(plugin.Schema{
					"user_id":  {Type: "string", Required: true, Scope: "user"},
					"username": {Type: "string"},
				}, userFields),
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "user_delete", Desc: "delete a user",
				Usage: "DELETE /core/users/{id}/",
				Options: plugin.Schema{
					"user_id": {Type: "string", Required: true, Scope: "user"},
				},
				Outputs: plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any", Desc: "present only if the response carries a body"}},
			},
			{
				Name: "groups", Desc: "list groups",
				Usage:   "GET /core/groups/",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.results"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "applications", Desc: "list applications",
				Usage:   "GET /core/applications/",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.results"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "providers", Desc: "list all providers (any type)",
				Usage:   "GET /providers/all/",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.results"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "flows", Desc: "list flow instances",
				Usage:   "GET /flows/instances/",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.results"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "events", Desc: "list events (audit log)",
				Usage: "GET /events/events/",
				Options: plugin.Schema{
					"action":   {Type: "string", Desc: "filter by event action"},
					"username": {Type: "string", Desc: "filter by the acting user's username"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.results"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "tokens", Desc: "list tokens",
				Usage:   "GET /core/tokens/",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.results"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any authentik API v3 endpoint",
				Usage: "method + path under /api/v3, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under /api/v3, e.g. /core/users/ (include the trailing slash authentik's routers require)"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.results when present"}, "status_code": {Type: "integer"}},
			},
		},
		// authentik is self-hosted: there is no fixed public host to declare.
		// The operator narrows egress to their own instance with
		// `network: ["authentik.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

// merge returns a new plugin.Schema combining base with extra (extra's keys
// win on conflict, though callers here never actually overlap).
func merge(base, extra plugin.Schema) plugin.Schema {
	out := plugin.Schema{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (p *authentikPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "users":
		return p.users(conn, o)
	case "user_get":
		return p.userGet(conn, o)
	case "user_create":
		return p.userCreate(conn, o)
	case "user_update":
		return p.userUpdate(conn, o)
	case "user_delete":
		return p.userDelete(conn, o)
	case "groups":
		return p.list(conn, "/core/groups/", nil)
	case "applications":
		return p.list(conn, "/core/applications/", nil)
	case "providers":
		return p.list(conn, "/providers/all/", nil)
	case "flows":
		return p.list(conn, "/flows/instances/", nil)
	case "events":
		return p.events(conn, o)
	case "tokens":
		return p.list(conn, "/core/tokens/", nil)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type authentikConn struct {
	baseURL            string
	apiToken           string
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (authentikConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return authentikConn{}, fmt.Errorf("base_url is required")
	}
	token := str(m["api_token"])
	if token == "" {
		return authentikConn{}, fmt.Errorf("api_token is required")
	}
	return authentikConn{
		baseURL:            strings.TrimRight(base, "/"),
		apiToken:           token,
		insecureSkipVerify: boolv(m["insecure_skip_verify"]),
	}, nil
}

// apiBase is conn.base_url + "/api/v3" — every first-class verb's endpoint
// is relative to this. Overridable simply by pointing base_url at a
// httptest.Server in tests.
func (c authentikConn) apiBase() string { return c.baseURL + "/api/v3" }

// --- verb implementations ---

// list performs a GET against a collection endpoint and hoists its
// pagination shape: {"pagination": {...}, "results": [...]} into `items`,
// while returning the full decoded body as `result` so pagination metadata
// survives.
func (p *authentikPlugin) list(conn authentikConn, endpoint string, query url.Values) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+endpoint, query, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "results")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (p *authentikPlugin) users(conn authentikConn, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["search"]); v != "" {
		q.Set("search", v)
	}
	if v, ok := o["is_active"]; ok {
		q.Set("is_active", boolStr(boolv(v)))
	}
	if v := str(o["ordering"]); v != "" {
		q.Set("ordering", v)
	}
	return p.list(conn, "/core/users/", q)
}

func (p *authentikPlugin) events(conn authentikConn, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["action"]); v != "" {
		q.Set("action", v)
	}
	if v := str(o["username"]); v != "" {
		q.Set("username", v)
	}
	return p.list(conn, "/events/events/", q)
}

func (p *authentikPlugin) userGet(conn authentikConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["user_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "user_id is required")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/core/users/"+url.PathEscape(id)+"/", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

// userOptionalFields are the mutable User fields shared by user_create and
// user_update, copied through verbatim (preserving whatever JSON type the
// caller supplied: bool, list, map, string) so authentik does the actual
// validation.
var userOptionalFields = []string{"name", "email", "is_active", "groups", "path", "type", "attributes"}

func (p *authentikPlugin) userCreate(conn authentikConn, o map[string]any) (plugin.InvokeResult, error) {
	username := str(o["username"])
	if username == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "username is required")
	}
	payload := map[string]any{"username": username}
	for _, f := range userOptionalFields {
		if v, ok := o[f]; ok {
			payload[f] = v
		}
	}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/core/users/", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *authentikPlugin) userUpdate(conn authentikConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["user_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "user_id is required")
	}
	payload := map[string]any{}
	if v, ok := o["username"]; ok {
		payload["username"] = v
	}
	for _, f := range userOptionalFields {
		if v, ok := o[f]; ok {
			payload[f] = v
		}
	}
	status, body, err := p.do(conn, http.MethodPatch, conn.apiBase()+"/core/users/"+url.PathEscape(id)+"/", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *authentikPlugin) userDelete(conn authentikConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["user_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "user_id is required")
	}
	status, body, err := p.do(conn, http.MethodDelete, conn.apiBase()+"/core/users/"+url.PathEscape(id)+"/", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *authentikPlugin) api(conn authentikConn, o map[string]any) (plugin.InvokeResult, error) {
	path := str(o["path"])
	if path == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "path is required")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	method := strOr(o["method"], http.MethodGet)
	q := url.Values{}
	if m, ok := o["query"].(map[string]any); ok {
		for k, v := range m {
			q.Set(k, fmt.Sprintf("%v", v))
		}
	}
	status, body, err := p.do(conn, method, conn.apiBase()+path, q, o["body"])
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
	case map[string]any:
		out["result"] = v
		if results, ok := v["results"].([]any); ok {
			out["items"] = results
		}
	default:
		if decoded != nil {
			out["result"] = decoded
		}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// --- HTTP plumbing ---

// do performs one HTTP request against the authentik API, attaching the
// Bearer token, and returns the status code and raw response body. A
// non-2xx status is translated into a CodeInternalError carrying the status
// and body — the caller never has to check status codes itself.
func (p *authentikPlugin) do(conn authentikConn, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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
	req.Header.Set("Authorization", "Bearer "+conn.apiToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.clientFor(conn).Do(req)
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

// clientFor builds an *http.Client for one request. The transport is only
// customized (skip TLS verification) when the connection asks for it, so the
// common case pays no extra cost and gets normal certificate validation.
func (p *authentikPlugin) clientFor(conn authentikConn) *http.Client {
	client := &http.Client{Timeout: 30 * time.Second}
	if conn.insecureSkipVerify {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- explicit, documented opt-out for self-signed LAN instances
		}
	}
	return client
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

// hoist pulls a list out of a decoded JSON value: if it is already a list,
// return it as-is; if it is an object, return the first of the given keys
// that holds a list. Otherwise, an empty list — never nil, so callers get a
// consistent [] rather than null on the wire.
func hoist(v any, keys ...string) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		for _, k := range keys {
			if list, ok := x[k].([]any); ok {
				return list
			}
		}
	}
	return []any{}
}

func main() {
	if err := plugin.Serve(newAuthentikPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-authentik:", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) ---

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strOr(v any, def string) string {
	if s := str(v); s != "" {
		return s
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

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
