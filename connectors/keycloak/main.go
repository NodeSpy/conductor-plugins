// Command conductor-keycloak is a verb-only conductor connector (#59) for
// Keycloak, a self-hosted identity/SSO provider. It drives the Keycloak Admin
// REST API: realms, users, groups, clients, roles, sessions, events, and a
// raw `api` escape hatch for anything without a first-class verb. Built ONLY
// against the public SDK (pkg/plugin) — no other dependency, no conductor
// internals.
//
// Authentication is a SELF-CONTAINED OAuth2 client_credentials admin-token
// fetch, not conductor's managed OAuth2 (Decl.Auth): Keycloak's token
// endpoint is deployment- AND realm-specific
// (base_url + /realms/{auth_realm}/protocol/openid-connect/token), so there
// is no single provider-wide endpoint conductor's managed-OAuth2 flow could
// bake in. This connector fetches and caches its own admin token instead,
// the same way the wiz connector self-manages its OAuth2 client-credentials
// token (see connectors/wiz/main.go) — and reuses audiobookshelf's plain
// net/http request plumbing (connectors/audiobookshelf/main.go) for the
// admin API calls themselves.
//
// Connection:
//
//	base_url:             "https://keycloak.example.com" # required; Keycloak server root
//	realm:                "master"                        # target realm for operations (default "master")
//	auth_realm:            "<realm>"                       # realm whose token endpoint authenticates the client (default = realm)
//	client_id:            "<oauth client id>"             # required
//	client_secret:        "<oauth client secret>"          # required
//	insecure_skip_verify:  false                           # skip TLS certificate verification (self-signed certs)
//
// Every admin verb calls base_url + "/admin/realms/{realm}" + <endpoint>,
// authenticated with `Authorization: Bearer <admin token>`. The admin token
// is fetched via the OAuth2 client_credentials grant against
// base_url + "/realms/{auth_realm}/protocol/openid-connect/token", cached
// until shortly before it expires, and transparently refreshed and the
// request retried once if the admin API ever answers 401 (e.g. the cached
// token was revoked server-side before its stated expiry). A non-2xx
// response (after that one retry) is returned as a CodeInternalError
// carrying the status code and response body — nothing is swallowed.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type keycloakPlugin struct {
	mu     sync.Mutex
	tokens map[string]*cachedToken // keyed by base_url+auth_realm+client_id
}

type cachedToken struct {
	value   string
	expires time.Time
}

func newKeycloakPlugin() *keycloakPlugin {
	return &keycloakPlugin{tokens: map[string]*cachedToken{}}
}

func (p *keycloakPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "keycloak",
		Desc: "Keycloak identity/SSO admin: realms, users, groups, clients, roles, sessions, and events over the Keycloak Admin REST API, plus a raw api escape hatch. Self-managed OAuth2 client_credentials admin-token fetch (Keycloak's token endpoint is deployment+realm specific). Self-hosted; declares no egress (narrow with network: per instance). No source.",
		Connection: plugin.Schema{
			"base_url": {Type: "string", Required: true, Desc: "Keycloak server root, e.g. https://keycloak.example.com"},
			"realm":    {Type: "string", Desc: `target realm for admin operations (default "master")`},
			"auth_realm": {
				Type: "string",
				Desc: "realm whose token endpoint authenticates client_id/client_secret (default = realm); rarely differs from realm, but some deployments issue admin service-account clients out of a dedicated realm",
			},
			"client_id":     {Type: "string", Required: true, Desc: "OAuth2 client_credentials client_id (a Keycloak client with service-account roles granting the admin operations you call)"},
			"client_secret": {Type: "string", Required: true, Desc: "OAuth2 client_credentials client_secret"},
			"insecure_skip_verify": {
				Type: "boolean",
				Desc: "skip TLS certificate verification. Self-hosted Keycloak commonly uses self-signed certs, but this disables verification ENTIRELY (a man-in-the-middle can impersonate your Keycloak host undetected) — only set true when base_url is reachable exclusively over a network you trust (LAN/VPN), and prefer installing a real certificate where possible",
			},
		},
		Verbs: keycloakVerbs(),
		// Keycloak is self-hosted: there is no fixed public host to declare.
		// The operator narrows egress to their own instance with
		// `network: ["keycloak.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

// --- verb schema ---

var (
	resultOut = plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}}
	itemsOut  = plugin.Schema{"status_code": {Type: "integer"}, "items": {Type: "list"}}

	userIDField = plugin.Field{Type: "string", Required: true, Scope: "user", Desc: "Keycloak user id (UUID)"}
)

func keycloakVerbs() []plugin.Verb {
	return []plugin.Verb{
		{
			Name: "realms", Desc: "list realms visible to this client",
			Usage:   "GET /admin/realms",
			Outputs: itemsOut,
		},
		{
			Name: "realm_get", Desc: "get the connection's target realm's details",
			Usage:   "GET /admin/realms/{realm}",
			Outputs: resultOut,
		},
		{
			Name: "users", Desc: "list/search users in the target realm",
			Usage: "GET /admin/realms/{realm}/users",
			Options: plugin.Schema{
				"search":   {Type: "string", Desc: "substring match across username/first/last/email"},
				"username": {Type: "string", Desc: "exact username filter"},
				"email":    {Type: "string", Desc: "exact email filter"},
				"first":    {Type: "integer", Desc: "pagination offset"},
				"max":      {Type: "integer", Desc: "page size"},
			},
			Outputs: itemsOut,
		},
		{
			Name: "user_get", Desc: "get one user by id",
			Usage:   "GET /admin/realms/{realm}/users/{id}",
			Options: plugin.Schema{"id": userIDField},
			Outputs: resultOut,
		},
		{
			Name: "user_create", Desc: "create a user",
			Usage: "POST /admin/realms/{realm}/users",
			Options: plugin.Schema{
				"body": {Type: "map", Required: true, Desc: "Keycloak UserRepresentation, e.g. {username, email, enabled, firstName, lastName, credentials, attributes, ...}"},
			},
			Outputs: plugin.Schema{
				"status_code": {Type: "integer"},
				"id":          {Type: "string", Desc: "the created user's id, parsed from the response's Location header (Keycloak's 201 carries no body)"},
			},
		},
		{
			Name: "user_update", Desc: "update a user (full replace of the given fields)",
			Usage: "PUT /admin/realms/{realm}/users/{id}",
			Options: plugin.Schema{
				"id":   userIDField,
				"body": {Type: "map", Required: true, Desc: "Keycloak UserRepresentation fields to update"},
			},
			Outputs: plugin.Schema{"status_code": {Type: "integer"}},
		},
		{
			Name: "user_delete", Desc: "delete a user",
			Usage:   "DELETE /admin/realms/{realm}/users/{id}",
			Options: plugin.Schema{"id": userIDField},
			Outputs: plugin.Schema{"status_code": {Type: "integer"}},
		},
		{
			Name: "user_reset_password", Desc: "set (or reset) a user's password",
			Usage: "PUT /admin/realms/{realm}/users/{id}/reset-password",
			Options: plugin.Schema{
				"id":        userIDField,
				"value":     {Type: "string", Required: true, Desc: "the new password"},
				"temporary": {Type: "boolean", Desc: "force the user to change it at next login (default false)"},
				"type":      {Type: "string", Desc: `credential type (default "password")`},
			},
			Outputs: plugin.Schema{"status_code": {Type: "integer"}},
		},
		{
			Name: "user_logout", Desc: "invalidate a user's active sessions",
			Usage:   "POST /admin/realms/{realm}/users/{id}/logout",
			Options: plugin.Schema{"id": userIDField},
			Outputs: plugin.Schema{"status_code": {Type: "integer"}},
		},
		{
			Name: "groups", Desc: "list top-level groups in the target realm",
			Usage:   "GET /admin/realms/{realm}/groups",
			Outputs: itemsOut,
		},
		{
			Name: "clients", Desc: "list clients in the target realm",
			Usage:   "GET /admin/realms/{realm}/clients",
			Outputs: itemsOut,
		},
		{
			Name: "roles", Desc: "list realm-level roles in the target realm",
			Usage:   "GET /admin/realms/{realm}/roles",
			Outputs: itemsOut,
		},
		{
			Name: "sessions", Desc: "per-client session counts in the target realm",
			Usage:   "GET /admin/realms/{realm}/client-session-stats",
			Outputs: itemsOut,
		},
		{
			Name: "events", Desc: "login/admin event log for the target realm",
			Usage: "GET /admin/realms/{realm}/events",
			Options: plugin.Schema{
				"type": {Type: "string", Desc: "event type filter, e.g. LOGIN, LOGIN_ERROR"},
				"user": {Type: "string", Desc: "user id filter"},
				"max":  {Type: "integer", Desc: "page size"},
			},
			Outputs: itemsOut,
		},
		{
			Name: "api", Desc: "raw escape hatch: any Keycloak endpoint",
			Usage: "method + path under base_url (e.g. /admin/realms/... or /realms/...), for anything without a first-class verb",
			Options: plugin.Schema{
				"method": {Type: "string", Desc: "HTTP method (default GET)"},
				"path":   {Type: "string", Required: true, Desc: "path relative to base_url, e.g. /admin/realms/master/users/count"},
				"query":  {Type: "map", Desc: "query string parameters"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
	}
}

// --- connection ---

type keycloakConn struct {
	baseURL            string
	realm              string
	authRealm          string
	clientID           string
	clientSecret       string
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (keycloakConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return keycloakConn{}, fmt.Errorf("base_url is required")
	}
	clientID := str(m["client_id"])
	if clientID == "" {
		return keycloakConn{}, fmt.Errorf("client_id is required")
	}
	clientSecret := str(m["client_secret"])
	if clientSecret == "" {
		return keycloakConn{}, fmt.Errorf("client_secret is required")
	}
	realm := strOr(m["realm"], "master")
	c := keycloakConn{
		baseURL:            strings.TrimRight(base, "/"),
		realm:              realm,
		authRealm:          strOr(m["auth_realm"], realm),
		clientID:           clientID,
		clientSecret:       clientSecret,
		insecureSkipVerify: boolv(m["insecure_skip_verify"]),
	}
	return c, nil
}

// adminBase is base_url + "/admin/realms/{realm}" — every first-class verb
// except `realms` (which lists ALL realms, not one) is relative to this.
func (c keycloakConn) adminBase() string {
	return c.baseURL + "/admin/realms/" + url.PathEscape(c.realm)
}

// tokenURL is the OAuth2 client_credentials token endpoint for c.authRealm —
// deployment- and realm-specific, hence self-managed rather than conductor's
// managed OAuth2.
func (c keycloakConn) tokenURL() string {
	return c.baseURL + "/realms/" + url.PathEscape(c.authRealm) + "/protocol/openid-connect/token"
}

// httpClient builds the client for one request. insecure_skip_verify is
// per-connection (a homelab operator's self-signed cert is common), so the
// TLS config cannot be a package-level singleton.
func (c keycloakConn) httpClient() *http.Client {
	client := &http.Client{Timeout: 30 * time.Second}
	if c.insecureSkipVerify {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return client
}

// --- OAuth2 client-credentials admin token, with in-memory caching ---

func (p *keycloakPlugin) tokenCacheKey(c keycloakConn) string {
	return c.baseURL + "\x00" + c.authRealm + "\x00" + c.clientID
}

// token returns a cached, still-valid admin token, or fetches and caches a
// fresh one. Mutex-guarded so concurrent invocations against the same
// instance never race the cache, and a valid cache entry never triggers a
// second fetch.
func (p *keycloakPlugin) token(ctx context.Context, c keycloakConn) (string, error) {
	key := p.tokenCacheKey(c)

	p.mu.Lock()
	if t, ok := p.tokens[key]; ok && time.Now().Before(t.expires) {
		tok := t.value
		p.mu.Unlock()
		return tok, nil
	}
	p.mu.Unlock()

	tok, expiresIn, err := p.fetchToken(ctx, c)
	if err != nil {
		return "", err
	}
	p.cacheToken(c, tok, expiresIn)
	return tok, nil
}

func (p *keycloakPlugin) cacheToken(c keycloakConn, tok string, expiresIn int64) {
	// A small safety margin so a token doesn't expire mid-flight between the
	// cache check and the request that uses it.
	margin := 30 * time.Second
	if expiresIn <= 0 {
		expiresIn = 60
	}
	exp := time.Now().Add(time.Duration(expiresIn)*time.Second - margin)

	p.mu.Lock()
	p.tokens[p.tokenCacheKey(c)] = &cachedToken{value: tok, expires: exp}
	p.mu.Unlock()
}

// invalidateToken drops the cached token for c, forcing the next token() call
// to fetch a fresh one — used when the admin API answers 401 even though the
// cache believed the token was still valid (e.g. it was revoked server-side).
func (p *keycloakPlugin) invalidateToken(c keycloakConn) {
	p.mu.Lock()
	delete(p.tokens, p.tokenCacheKey(c))
	p.mu.Unlock()
}

// fetchToken POSTs the OAuth2 client-credentials grant to c's token endpoint,
// form-encoded, and parses {access_token, expires_in} from the response.
func (p *keycloakPlugin) fetchToken(ctx context.Context, c keycloakConn) (token string, expiresIn int64, err error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("keycloak: token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("keycloak: token %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", 0, fmt.Errorf("keycloak: token response decode: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("keycloak: token response had no access_token: %s", string(body))
	}
	return out.AccessToken, out.ExpiresIn, nil
}

// --- HTTP plumbing ---

// requestOnce performs one HTTP request against fullURL with the given
// bearer token, returning the raw status/body/headers without translating a
// non-2xx status into an error — the caller (do) decides how to react (e.g.
// refresh-and-retry on a 401).
func (p *keycloakPlugin) requestOnce(c keycloakConn, method, fullURL string, query url.Values, body any, token string) (int, []byte, http.Header, error) {
	full := fullURL
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return 0, nil, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	return resp.StatusCode, respBody, resp.Header, nil
}

// do performs one authenticated admin-API request: it fetches (or reuses a
// cached) admin token, and refreshes + retries the request exactly once if
// the first attempt comes back 401. A non-2xx status (after that retry) is
// translated into a CodeInternalError carrying the status and body.
func (p *keycloakPlugin) do(ctx context.Context, c keycloakConn, method, fullURL string, query url.Values, body any) (int, []byte, http.Header, error) {
	tok, err := p.token(ctx, c)
	if err != nil {
		return 0, nil, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}

	status, respBody, hdr, err := p.requestOnce(c, method, fullURL, query, body, tok)
	if err != nil {
		return 0, nil, nil, err
	}

	if status == http.StatusUnauthorized {
		p.invalidateToken(c)
		tok, err = p.token(ctx, c)
		if err != nil {
			return status, respBody, hdr, plugin.Errorf(plugin.CodeInternalError, err.Error())
		}
		status, respBody, hdr, err = p.requestOnce(c, method, fullURL, query, body, tok)
		if err != nil {
			return 0, nil, nil, err
		}
	}

	if status < 200 || status >= 300 {
		return status, respBody, hdr, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s %s: %d %s", method, fullURL, status, strings.TrimSpace(string(respBody))))
	}
	return status, respBody, hdr, nil
}

// decodeJSON decodes a JSON response body into a generic value. An empty
// body decodes to nil rather than an error (e.g. a 204/201 with no body).
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

// asItems normalizes a decoded value into a list: itself when it already is
// one, an empty list otherwise. Never nil — Keycloak's list endpoints return
// bare JSON arrays, and callers should always get a consistent [] on the wire.
func asItems(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return []any{}
}

// locationID parses the resource id off the trailing path segment of a
// Location response header (Keycloak's 201 Created responses carry no body,
// only e.g. ".../admin/realms/master/users/<id>"). Returns "" if location is
// empty or unparsable.
func locationID(location string) string {
	if location == "" {
		return ""
	}
	u, err := url.Parse(location)
	path := location
	if err == nil && u.Path != "" {
		path = u.Path
	}
	path = strings.TrimRight(path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// --- Invoke ---

func (p *keycloakPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	c, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch req.Verb {
	case "realms":
		return p.listVerb(ctx, c, http.MethodGet, c.baseURL+"/admin/realms", nil)
	case "realm_get":
		return p.objectVerb(ctx, c, http.MethodGet, c.adminBase(), nil, nil)
	case "users":
		q := url.Values{}
		setStr(q, "search", o["search"])
		setStr(q, "username", o["username"])
		setStr(q, "email", o["email"])
		setInt(q, "first", o["first"])
		setInt(q, "max", o["max"])
		return p.listVerb(ctx, c, http.MethodGet, c.adminBase()+"/users", q)
	case "user_get":
		id, err := requiredStr(o, "id")
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		return p.objectVerb(ctx, c, http.MethodGet, c.adminBase()+"/users/"+url.PathEscape(id), nil, nil)
	case "user_create":
		body, ok := o["body"].(map[string]any)
		if !ok || len(body) == 0 {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "body is required")
		}
		return p.createVerb(ctx, c, c.adminBase()+"/users", body)
	case "user_update":
		id, err := requiredStr(o, "id")
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		body, ok := o["body"].(map[string]any)
		if !ok || len(body) == 0 {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "body is required")
		}
		return p.statusOnlyVerb(ctx, c, http.MethodPut, c.adminBase()+"/users/"+url.PathEscape(id), body)
	case "user_delete":
		id, err := requiredStr(o, "id")
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		return p.statusOnlyVerb(ctx, c, http.MethodDelete, c.adminBase()+"/users/"+url.PathEscape(id), nil)
	case "user_reset_password":
		id, err := requiredStr(o, "id")
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		value, err := requiredStr(o, "value")
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		body := map[string]any{
			"type":      strOr(o["type"], "password"),
			"value":     value,
			"temporary": boolv(o["temporary"]),
		}
		return p.statusOnlyVerb(ctx, c, http.MethodPut, c.adminBase()+"/users/"+url.PathEscape(id)+"/reset-password", body)
	case "user_logout":
		id, err := requiredStr(o, "id")
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		return p.statusOnlyVerb(ctx, c, http.MethodPost, c.adminBase()+"/users/"+url.PathEscape(id)+"/logout", nil)
	case "groups":
		return p.listVerb(ctx, c, http.MethodGet, c.adminBase()+"/groups", nil)
	case "clients":
		return p.listVerb(ctx, c, http.MethodGet, c.adminBase()+"/clients", nil)
	case "roles":
		return p.listVerb(ctx, c, http.MethodGet, c.adminBase()+"/roles", nil)
	case "sessions":
		return p.listVerb(ctx, c, http.MethodGet, c.adminBase()+"/client-session-stats", nil)
	case "events":
		q := url.Values{}
		setStr(q, "type", o["type"])
		setStr(q, "user", o["user"])
		setInt(q, "max", o["max"])
		return p.listVerb(ctx, c, http.MethodGet, c.adminBase()+"/events", q)
	case "api":
		return p.apiVerb(ctx, c, o)
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
}

// listVerb calls a GET endpoint that returns a bare JSON array, hoisting it
// to outputs.items.
func (p *keycloakPlugin) listVerb(ctx context.Context, c keycloakConn, method, fullURL string, query url.Values) (plugin.InvokeResult, error) {
	status, body, _, err := p.do(ctx, c, method, fullURL, query, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "keycloak: decode response: "+derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": asItems(decoded), "status_code": status}}, nil
}

// objectVerb calls an endpoint that returns a single JSON object, surfacing
// it as outputs.result.
func (p *keycloakPlugin) objectVerb(ctx context.Context, c keycloakConn, method, fullURL string, query url.Values, body any) (plugin.InvokeResult, error) {
	status, respBody, _, err := p.do(ctx, c, method, fullURL, query, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "keycloak: decode response: "+derr.Error())
	}
	out := map[string]any{"status_code": status}
	if decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// statusOnlyVerb calls a write endpoint (update/delete/reset-password/logout)
// that normally answers 204 No Content: outputs.status_code, plus
// outputs.result if Keycloak did send a body back.
func (p *keycloakPlugin) statusOnlyVerb(ctx context.Context, c keycloakConn, method, fullURL string, body any) (plugin.InvokeResult, error) {
	status, respBody, _, err := p.do(ctx, c, method, fullURL, nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(respBody); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// createVerb POSTs body to fullURL. Keycloak's create endpoints answer 201
// Created with NO response body, carrying the new resource's id only in the
// Location header — this parses that id out rather than losing it.
func (p *keycloakPlugin) createVerb(ctx context.Context, c keycloakConn, fullURL string, body any) (plugin.InvokeResult, error) {
	status, respBody, hdr, err := p.do(ctx, c, http.MethodPost, fullURL, nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if hdr != nil {
		if id := locationID(hdr.Get("Location")); id != "" {
			out["id"] = id
		}
	}
	if decoded, derr := decodeJSON(respBody); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// apiVerb is the raw escape hatch: method + path relative to base_url (not
// adminBase — this can reach any Keycloak endpoint, admin or otherwise).
func (p *keycloakPlugin) apiVerb(ctx context.Context, c keycloakConn, o map[string]any) (plugin.InvokeResult, error) {
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
	status, respBody, _, err := p.do(ctx, c, strings.ToUpper(method), c.baseURL+path, q, o["body"])
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "keycloak: decode response: "+derr.Error())
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

func main() {
	if err := plugin.Serve(newKeycloakPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-keycloak:", err)
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

func requiredStr(o map[string]any, key string) (string, error) {
	s := str(o[key])
	if s == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return s, nil
}

func setStr(q url.Values, key string, v any) {
	if s := str(v); s != "" {
		q.Set(key, s)
	}
}

// setInt sets key on q from an integer-ish option (JSON number, Go int, or a
// numeric string), ignoring anything absent/unrecognized.
func setInt(q url.Values, key string, v any) {
	switch x := v.(type) {
	case nil:
		return
	case string:
		if x != "" {
			q.Set(key, x)
		}
	case int:
		q.Set(key, fmt.Sprintf("%d", x))
	case int64:
		q.Set(key, fmt.Sprintf("%d", x))
	case float64:
		q.Set(key, fmt.Sprintf("%d", int64(x)))
	}
}
