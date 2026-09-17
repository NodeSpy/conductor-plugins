// Command conductor-google-contacts is a verb-only conductor connector for
// Google Contacts (People API v1, https://people.googleapis.com/v1):
// listing/searching the user's contacts (connections), getting/creating/
// updating/deleting a single contact, listing "other contacts", and a raw
// `api` escape hatch for anything a first-class verb does not cover. Built
// ONLY against the public SDK (pkg/plugin) — no other dependency.
//
// Authentication is conductor's MANAGED OAuth2 (pkg/plugin v0.15.0+): this
// plugin does NOT perform any token exchange. It declares Google's OAuth2
// endpoints in Describe().Auth, the operator supplies client credentials in
// the connector's daemon-side `auth:` block, and `conductor connector auth
// google-contacts` runs the one-time browser login. The daemon then injects
// a fresh, rotated bearer token into every InvokeRequest.Connection under
// plugin.AccessTokenKey, read here with plugin.AccessToken. Because
// conductor — not this plugin — talks to oauth2.googleapis.com and
// accounts.google.com, the plugin's only egress is people.googleapis.com.
//
// AuthParams sets access_type=offline and prompt=consent: without
// access_type=offline Google never returns a refresh token, and the daemon
// would be unable to keep the connector authenticated past the first access
// token's expiry.
//
// The People API requires an explicit field mask (personFields on reads,
// updatePersonFields on updates, readMask on search/otherContacts) on every
// request that returns or touches a Person resource; when a verb's caller
// omits the corresponding option, this plugin defaults it to
// "names,emailAddresses,phoneNumbers,organizations" rather than sending an
// invalid, mask-less request.
//
// Connection:
//
//	api_base: "https://..."   # optional test override, default https://people.googleapis.com/v1
//
// Every verb's outputs include `status_code`; `connections` hoists Google's
// `connections` array into `items`; single-resource verbs return `result`. A
// non-2xx response becomes a CodeInternalError carrying the status and body
// — nothing is swallowed.
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

// defaultAPIBase is the Google People API v1's production host+prefix.
// api_base overrides it for tests.
const defaultAPIBase = "https://people.googleapis.com/v1"

// defaultPersonFields is used for personFields/readMask whenever a verb's
// caller does not supply one — the People API rejects requests that touch a
// Person resource without an explicit field mask.
const defaultPersonFields = "names,emailAddresses,phoneNumbers,organizations"

type googleContactsPlugin struct {
	client *http.Client
}

func newGoogleContactsPlugin() *googleContactsPlugin {
	return &googleContactsPlugin{client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *googleContactsPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "google-contacts",
		Desc: "Google Contacts: list/search connections, get/create/update/delete a contact, list other contacts, and a raw `api` escape hatch over the People API v1. Authenticates via conductor's managed OAuth2 — run `conductor connector auth google-contacts` after configuring an `auth:` block; this plugin never talks to Google's OAuth2 endpoints itself.",
		Connection: plugin.Schema{
			"api_base": {Type: "string", Desc: "override https://people.googleapis.com/v1 (tests only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "connections", Desc: "list the authenticated user's contacts",
				Usage: "GET /people/me/connections",
				Options: plugin.Schema{
					"personFields": {Type: "string", Desc: "comma-separated Person fields to return (default \"" + defaultPersonFields + "\")"},
					"pageSize":     {Type: "integer", Desc: "max contacts per page"},
					"pageToken":    {Type: "string", Desc: "page token from a previous response"},
					"sortOrder":    {Type: "string", Desc: "\"LAST_MODIFIED_ASCENDING\", \"LAST_MODIFIED_DESCENDING\", or \"FIRST_NAME_ASCENDING\"/\"LAST_NAME_ASCENDING\""},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "contact_get", Desc: "get one contact",
				Usage: "GET /{resource_name}",
				Options: plugin.Schema{
					"resource_name": {Type: "string", Required: true, Desc: "e.g. \"people/c1234567890\"", Scope: "contact"},
					"personFields":  {Type: "string", Desc: "comma-separated Person fields to return (default \"" + defaultPersonFields + "\")"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "contact_create", Desc: "create a contact",
				Usage: "POST /people:createContact",
				Options: plugin.Schema{
					"person":      {Type: "map", Desc: "a full Google People Person resource body; overrides the convenience fields below when set"},
					"given_name":  {Type: "string", Desc: "convenience: contact given (first) name"},
					"family_name": {Type: "string", Desc: "convenience: contact family (last) name"},
					"email":       {Type: "string", Desc: "convenience: contact email address"},
					"phone":       {Type: "string", Desc: "convenience: contact phone number"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "contact_update", Desc: "patch an existing contact",
				Usage: "PATCH /{resource_name}:updateContact",
				Options: plugin.Schema{
					"resource_name":      {Type: "string", Required: true, Desc: "e.g. \"people/c1234567890\"", Scope: "contact"},
					"person":             {Type: "map", Required: true, Desc: "the fields to patch, as a Google People Person resource fragment (must include etag)"},
					"updatePersonFields": {Type: "string", Desc: "comma-separated Person fields being updated (default \"" + defaultPersonFields + "\")"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "contact_delete", Desc: "delete a contact",
				Usage: "DELETE /{resource_name}:deleteContact",
				Options: plugin.Schema{
					"resource_name": {Type: "string", Required: true, Desc: "e.g. \"people/c1234567890\"", Scope: "contact"},
				},
				Outputs: plugin.Schema{"status_code": {Type: "integer"}},
			},
			{
				Name: "search", Desc: "search the authenticated user's contacts",
				Usage: "GET /people:searchContacts",
				Options: plugin.Schema{
					"query":    {Type: "string", Required: true, Desc: "search query text"},
					"readMask": {Type: "string", Desc: "comma-separated Person fields to return (default \"" + defaultPersonFields + "\")"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "other_contacts", Desc: "list \"other contacts\" (auto-saved from interactions, not in the user's contacts)",
				Usage: "GET /otherContacts",
				Options: plugin.Schema{
					"readMask": {Type: "string", Desc: "comma-separated Person fields to return (default \"" + defaultPersonFields + "\")"},
					"pageSize": {Type: "integer", Desc: "max contacts per page"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any Google People API v1 endpoint (enables writes)",
				Usage: "method + path under /v1, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under https://people.googleapis.com/v1, e.g. /people/me/connections"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// Conductor performs the OAuth2 token exchange with Google's own
		// endpoints on this plugin's behalf; the plugin itself only ever
		// calls people.googleapis.com.
		Capabilities: plugin.Capabilities{Egress: []string{"people.googleapis.com:443"}},
		Auth: &plugin.AuthSpec{
			Grants:   []string{"authorization_code"},
			TokenURL: "https://oauth2.googleapis.com/token",
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
			Scopes:   []string{"https://www.googleapis.com/auth/contacts"},
			// access_type=offline is REQUIRED for Google to return a refresh
			// token at all; prompt=consent forces the consent screen so a
			// re-auth (e.g. adding scopes later) still yields one.
			AuthParams: map[string]string{"access_type": "offline", "prompt": "consent"},
		},
	}
}

func (p *googleContactsPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, token, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "connections":
		return p.connections(conn, token, o)
	case "contact_get":
		return p.contactGet(conn, token, o)
	case "contact_create":
		return p.contactCreate(conn, token, o)
	case "contact_update":
		return p.contactUpdate(conn, token, o)
	case "contact_delete":
		return p.contactDelete(conn, token, o)
	case "search":
		return p.search(conn, token, o)
	case "other_contacts":
		return p.otherContacts(conn, token, o)
	case "api":
		return p.api(conn, token, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type gcontactsConn struct {
	apiBase string
}

// parseConn reads the connection map and the managed-OAuth2 bearer token
// injected by the daemon. A missing token means the operator has not
// configured an `auth:` block and logged in yet.
func parseConn(m map[string]any) (gcontactsConn, string, error) {
	token := plugin.AccessToken(m)
	if token == "" {
		return gcontactsConn{}, "", fmt.Errorf("no access token — add an auth: block and run `conductor connector auth google-contacts`")
	}
	base := strOr(str(m["api_base"]), defaultAPIBase)
	base = strings.TrimRight(base, "/")
	return gcontactsConn{apiBase: base}, token, nil
}

// --- verb implementations ---

func (p *googleContactsPlugin) connections(conn gcontactsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	q.Set("personFields", strOr(str(o["personFields"]), defaultPersonFields))
	if v := intStr(o["pageSize"]); v != "" {
		q.Set("pageSize", v)
	}
	if v := str(o["pageToken"]); v != "" {
		q.Set("pageToken", v)
	}
	if v := str(o["sortOrder"]); v != "" {
		q.Set("sortOrder", v)
	}
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+"/people/me/connections", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded, "connections"), "status_code": status}}, nil
}

func (p *googleContactsPlugin) contactGet(conn gcontactsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	name := str(o["resource_name"])
	if name == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "resource_name is required")
	}
	q := url.Values{}
	q.Set("personFields", strOr(str(o["personFields"]), defaultPersonFields))
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+"/"+resourcePath(name), q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleContactsPlugin) contactCreate(conn gcontactsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	body := personBody(o)
	status, respBody, err := p.do(token, http.MethodPost, conn.apiBase+"/people:createContact", nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleContactsPlugin) contactUpdate(conn gcontactsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	name := str(o["resource_name"])
	if name == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "resource_name is required")
	}
	patch, ok := o["person"].(map[string]any)
	if !ok || len(patch) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "person (map of fields to patch) is required")
	}
	q := url.Values{}
	q.Set("updatePersonFields", strOr(str(o["updatePersonFields"]), defaultPersonFields))
	status, respBody, err := p.do(token, http.MethodPatch, conn.apiBase+"/"+resourcePath(name)+":updateContact", q, patch)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleContactsPlugin) contactDelete(conn gcontactsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	name := str(o["resource_name"])
	if name == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "resource_name is required")
	}
	status, _, err := p.do(token, http.MethodDelete, conn.apiBase+"/"+resourcePath(name)+":deleteContact", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"status_code": status}}, nil
}

func (p *googleContactsPlugin) search(conn gcontactsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	query := str(o["query"])
	if query == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "query is required")
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("readMask", strOr(str(o["readMask"]), defaultPersonFields))
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+"/people:searchContacts", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded, "results"), "status_code": status}}, nil
}

func (p *googleContactsPlugin) otherContacts(conn gcontactsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	q.Set("readMask", strOr(str(o["readMask"]), defaultPersonFields))
	if v := intStr(o["pageSize"]); v != "" {
		q.Set("pageSize", v)
	}
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+"/otherContacts", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded, "otherContacts"), "status_code": status}}, nil
}

func (p *googleContactsPlugin) api(conn gcontactsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
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

// --- person body construction ---

// resourcePath strips any leading slash from a resource_name option (e.g.
// "people/c123" or "/people/c123") so it can be joined onto apiBase without
// a doubled slash.
func resourcePath(name string) string {
	return strings.TrimPrefix(name, "/")
}

// personBody builds a Google People Person resource for contact_create: the
// `person` map option if given, verbatim; otherwise assembled from the
// convenience given_name/family_name/email/phone options.
func personBody(o map[string]any) map[string]any {
	if pm, ok := o["person"].(map[string]any); ok {
		return pm
	}
	body := map[string]any{}
	given := str(o["given_name"])
	family := str(o["family_name"])
	if given != "" || family != "" {
		name := map[string]any{}
		if given != "" {
			name["givenName"] = given
		}
		if family != "" {
			name["familyName"] = family
		}
		body["names"] = []any{name}
	}
	if v := str(o["email"]); v != "" {
		body["emailAddresses"] = []any{map[string]any{"value": v}}
	}
	if v := str(o["phone"]); v != "" {
		body["phoneNumbers"] = []any{map[string]any{"value": v}}
	}
	return body
}

// --- HTTP plumbing ---

// do performs one HTTP request against the Google People API, attaching the
// Bearer token, and returns the status code and raw response body. A
// non-2xx status is translated into a CodeInternalError carrying the status
// and body — callers never need to check status codes themselves.
func (p *googleContactsPlugin) do(token, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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

// hoist pulls the named list out of a decoded People API list response
// (e.g. {"connections": [...], "nextPageToken": "..."}). A bare list is
// returned as-is. Anything else yields an empty (never nil) list, so callers
// get a consistent [] rather than null on the wire.
func hoist(v any, key string) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		if list, ok := x[key].([]any); ok {
			return list
		}
	}
	return []any{}
}

func main() {
	if err := plugin.Serve(newGoogleContactsPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-google-contacts:", err)
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
