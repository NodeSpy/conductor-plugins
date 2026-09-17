// Command conductor-nginx-proxy-manager is a verb-only conductor connector
// for Nginx Proxy Manager (NPM), a self-hosted reverse-proxy admin UI/API.
// It drives NPM's REST API over net/http: proxy hosts (list/get/create/
// update/delete/enable/disable), redirection hosts, streams, dead (404)
// hosts, access lists, certificates, a host report summary, and a raw `api`
// escape hatch for anything a first-class verb does not cover. Built ONLY
// against the public SDK (pkg/plugin) and the standard library — no
// third-party client.
//
// NPM authenticates by exchanging an email/password identity pair for a JWT:
// a successful POST /api/tokens returns {"token": "<jwt>", "expires": ...}.
// Every subsequent request carries the token via
// Authorization: Bearer <jwt>. This connector logs in once per connector
// instance and caches the token across Invoke calls, transparently
// re-authenticating on a 401 (an expired or invalid token) before giving up.
//
// Connection:
//
//	base_url:             "http://npm.example.com:81"  # required; the NPM admin root (no trailing /api)
//	email:                "admin@example.com"          # required; NPM user identity
//	password:             "<password>"                 # required; exchanged (with email) for a JWT via POST /api/tokens
//	insecure_skip_verify: false                          # optional; skip TLS verification for self-signed certs
//
// Every request goes to base_url + "/api" + <endpoint> (the token exchange
// itself is base_url + "/api/tokens"). A non-2xx response is returned as a
// CodeInternalError carrying the status code and response body — nothing is
// swallowed.
//
// insecure_skip_verify exists because self-signed certificates are common on
// a home-lab NPM box; setting it disables TLS certificate verification for
// that connector instance, which means the connection is no longer protected
// against a man-in-the-middle. Prefer importing the box's real certificate
// instead when possible.
//
// NPM is self-hosted with no fixed public host, so Capabilities.Egress is
// declared empty; the operator's `network:` allowlist on the connector
// instance is the actual scope. This connector is verb-only: it has no
// Events and does not implement SourceHandler.
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
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- plugin ---

type npmPlugin struct {
	mu      sync.Mutex
	clients map[string]*npmClient
}

func newNPMPlugin() *npmPlugin {
	return &npmPlugin{}
}

func (p *npmPlugin) Describe() plugin.Decl {
	res := plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}}
	list := plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}}
	idOpt := plugin.Schema{"id": {Type: "integer", Required: true, Scope: "proxy_host", Desc: "proxy host ID"}}

	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "nginx-proxy-manager",
		Desc: "Nginx Proxy Manager: proxy hosts (list/get/create/update/delete/enable/disable), redirection hosts, streams, dead (404) hosts, access lists, certificates, a host report summary, and a raw `api` escape hatch. Token (JWT) auth. Self-hosted; declares no egress (narrow with network: per instance). No source.",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "NPM admin root, e.g. http://npm.example.com:81 (no trailing /api)"},
			"email":                {Type: "string", Required: true, Desc: "NPM user identity, exchanged (with password) for a JWT via POST /api/tokens"},
			"password":             {Type: "string", Required: true, Desc: "NPM user password"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification (default false) — common with self-signed certs on a home-lab NPM box, but disables protection against a man-in-the-middle"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "proxy_hosts", Desc: "list proxy hosts",
				Usage:   "GET /api/nginx/proxy-hosts",
				Options: plugin.Schema{},
				Outputs: list,
			},
			{
				Name: "proxy_host_get", Desc: "get one proxy host",
				Usage:   "GET /api/nginx/proxy-hosts/{id}",
				Options: idOpt,
				Outputs: res,
			},
			{
				Name: "proxy_host_create", Desc: "create a proxy host",
				Usage: "POST /api/nginx/proxy-hosts",
				Options: plugin.Schema{
					"body": {Type: "map", Required: true, Desc: "NPM proxy host object, e.g. domain_names, forward_scheme, forward_host, forward_port, certificate_id, access_list_id, ssl_forced, block_exploits, allow_websocket_upgrade, ..."},
				},
				Outputs: res,
			},
			{
				Name: "proxy_host_update", Desc: "update a proxy host",
				Usage: "PUT /api/nginx/proxy-hosts/{id}",
				Options: plugin.Schema{
					"id":   {Type: "integer", Required: true, Scope: "proxy_host", Desc: "proxy host ID"},
					"body": {Type: "map", Required: true, Desc: "fields to update, same shape as proxy_host_create's body"},
				},
				Outputs: res,
			},
			{
				Name: "proxy_host_delete", Desc: "delete a proxy host",
				Usage:   "DELETE /api/nginx/proxy-hosts/{id}",
				Options: idOpt,
				Outputs: res,
			},
			{
				Name: "proxy_host_enable", Desc: "enable a proxy host",
				Usage:   "POST /api/nginx/proxy-hosts/{id}/enable",
				Options: idOpt,
				Outputs: res,
			},
			{
				Name: "proxy_host_disable", Desc: "disable a proxy host",
				Usage:   "POST /api/nginx/proxy-hosts/{id}/disable",
				Options: idOpt,
				Outputs: res,
			},
			{
				Name: "redirection_hosts", Desc: "list redirection hosts",
				Usage:   "GET /api/nginx/redirection-hosts",
				Options: plugin.Schema{},
				Outputs: list,
			},
			{
				Name: "streams", Desc: "list TCP/UDP streams",
				Usage:   "GET /api/nginx/streams",
				Options: plugin.Schema{},
				Outputs: list,
			},
			{
				Name: "dead_hosts", Desc: "list dead (404) hosts",
				Usage:   "GET /api/nginx/dead-hosts",
				Options: plugin.Schema{},
				Outputs: list,
			},
			{
				Name: "access_lists", Desc: "list access lists",
				Usage:   "GET /api/nginx/access-lists",
				Options: plugin.Schema{},
				Outputs: list,
			},
			{
				Name: "certificates", Desc: "list certificates",
				Usage:   "GET /api/nginx/certificates",
				Options: plugin.Schema{},
				Outputs: list,
			},
			{
				Name: "reports", Desc: "host counts report summary",
				Usage:   "GET /api/reports/hosts",
				Options: plugin.Schema{},
				Outputs: res,
			},
			{
				Name: "api", Desc: "raw escape hatch: any NPM API endpoint",
				Usage: "method + path under /api, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under /api, e.g. /nginx/proxy-hosts"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: list,
			},
		},
		// NPM is self-hosted: there is no fixed public host to declare. The
		// operator narrows egress to their own instance with
		// `network: ["npm.example.com:81"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

// clientFor builds (or reuses) the npmClient for one connector instance, so
// the cached JWT session persists across Invoke calls instead of logging in
// on every single verb call. A credential change (a different
// base_url/email/password/insecure_skip_verify) rebuilds the client,
// discarding any stale session.
func (p *npmPlugin) clientFor(instance string, connMap map[string]any) (*npmClient, error) {
	conn, err := parseConn(connMap)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = map[string]*npmClient{}
	}
	if c, ok := p.clients[instance]; ok && c.conn == conn {
		return c, nil
	}
	c := &npmClient{http: httpClientFor(conn), conn: conn}
	p.clients[instance] = c
	return c, nil
}

// httpClientFor builds the *http.Client for one connection: TLS-verified by
// default, or with certificate verification disabled when the connection
// asked for insecure_skip_verify.
func httpClientFor(conn npmConn) *http.Client {
	if !conn.insecureSkipVerify {
		return &http.Client{Timeout: 30 * time.Second}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // opt-in per connection, for self-signed home-lab certs
	}
}

func (p *npmPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	c, err := p.clientFor(req.Instance, req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	base := c.conn.apiBase()

	switch req.Verb {
	case "proxy_hosts":
		return c.call(http.MethodGet, base+"/nginx/proxy-hosts", nil, nil)
	case "proxy_host_get":
		id, err := requireID(o)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return c.call(http.MethodGet, base+"/nginx/proxy-hosts/"+id, nil, nil)
	case "proxy_host_create":
		body, err := requireBody(o)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return c.call(http.MethodPost, base+"/nginx/proxy-hosts", nil, body)
	case "proxy_host_update":
		id, err := requireID(o)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		body, err := requireBody(o)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return c.call(http.MethodPut, base+"/nginx/proxy-hosts/"+id, nil, body)
	case "proxy_host_delete":
		id, err := requireID(o)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return c.call(http.MethodDelete, base+"/nginx/proxy-hosts/"+id, nil, nil)
	case "proxy_host_enable":
		id, err := requireID(o)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return c.call(http.MethodPost, base+"/nginx/proxy-hosts/"+id+"/enable", nil, nil)
	case "proxy_host_disable":
		id, err := requireID(o)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return c.call(http.MethodPost, base+"/nginx/proxy-hosts/"+id+"/disable", nil, nil)
	case "redirection_hosts":
		return c.call(http.MethodGet, base+"/nginx/redirection-hosts", nil, nil)
	case "streams":
		return c.call(http.MethodGet, base+"/nginx/streams", nil, nil)
	case "dead_hosts":
		return c.call(http.MethodGet, base+"/nginx/dead-hosts", nil, nil)
	case "access_lists":
		return c.call(http.MethodGet, base+"/nginx/access-lists", nil, nil)
	case "certificates":
		return c.call(http.MethodGet, base+"/nginx/certificates", nil, nil)
	case "reports":
		return c.call(http.MethodGet, base+"/reports/hosts", nil, nil)
	case "api":
		return c.api(o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// requireID reads the required "id" option (a proxy host ID) as a string,
// accepting either a JSON number (the common case, e.g. an ID copied from a
// list verb's output) or a string.
func requireID(o map[string]any) (string, error) {
	id := intStr(o["id"])
	if id == "" {
		return "", plugin.Errorf(plugin.CodeInvalidParams, "id is required")
	}
	return url.PathEscape(id), nil
}

// requireBody reads the required "body" option: the caller's JSON payload,
// passed straight through to NPM. NPM's proxy host schema has many fields
// (domain_names, forward_scheme/host/port, certificate_id, access_list_id,
// ssl/hsts/websocket/exploit-blocking toggles, locations, advanced_config,
// ...), so this connector does not re-model it field-by-field; the caller
// supplies the object NPM expects and NPM validates it.
func requireBody(o map[string]any) (any, error) {
	body, ok := o["body"]
	if !ok || body == nil {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, "body is required")
	}
	return body, nil
}

// --- connection ---

// npmConn is the resolved, comparable connection config for one connector
// instance — comparable so clientFor can detect a credential change and
// rebuild the client (and its cached session) instead of reusing a stale one.
type npmConn struct {
	baseURL            string
	email              string
	password           string
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (npmConn, error) {
	base := strings.TrimRight(str(m["base_url"]), "/")
	if base == "" {
		return npmConn{}, fmt.Errorf("base_url is required")
	}
	email := str(m["email"])
	if email == "" {
		return npmConn{}, fmt.Errorf("email is required")
	}
	password := str(m["password"])
	if password == "" {
		return npmConn{}, fmt.Errorf("password is required")
	}
	return npmConn{baseURL: base, email: email, password: password, insecureSkipVerify: boolv(m["insecure_skip_verify"])}, nil
}

// apiBase is conn.base_url + "/api" — every first-class verb's endpoint is
// relative to this. The token exchange itself is base_url + "/api/tokens",
// i.e. apiBase() + "/tokens". Overridable simply by pointing base_url at a
// httptest.Server in tests.
func (c npmConn) apiBase() string { return c.baseURL + "/api" }

// --- session client ---

// npmClient is the per-instance HTTP client plus its cached JWT, obtained by
// exchanging the connection's email/password for a token via
// POST /api/tokens.
type npmClient struct {
	mu    sync.Mutex
	http  *http.Client
	conn  npmConn
	token string
}

// ensureLoggedIn logs in if this client has no cached token yet.
func (c *npmClient) ensureLoggedIn() error {
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token != "" {
		return nil
	}
	return c.login()
}

// relogin discards the cached token and logs in again, e.g. after a 401.
func (c *npmClient) relogin() error {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
	return c.login()
}

// login exchanges the connection's email/password for a JWT via
// POST /api/tokens ({"identity": email, "secret": password}) and caches the
// returned token. A bad identity/secret pair (or another login failure) is
// returned as an error carrying NPM's status code and response body.
func (c *npmClient) login() error {
	payload, err := json.Marshal(map[string]any{"identity": c.conn.email, "secret": c.conn.password})
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "nginx-proxy-manager login: "+err.Error())
	}
	req, err := http.NewRequest(http.MethodPost, c.conn.apiBase()+"/tokens", bytes.NewReader(payload))
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "nginx-proxy-manager login: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "nginx-proxy-manager login: "+err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "nginx-proxy-manager login: reading response: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("nginx-proxy-manager login failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}
	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "nginx-proxy-manager login: decoding response: "+err.Error())
	}
	if parsed.Token == "" {
		return plugin.Errorf(plugin.CodeInternalError, "nginx-proxy-manager login: no token in response: "+strings.TrimSpace(string(body)))
	}
	c.mu.Lock()
	c.token = parsed.Token
	c.mu.Unlock()
	return nil
}

// call performs one HTTP request against the NPM API and shapes the decoded
// response into a verb's outputs (see shapeOutputs).
func (c *npmClient) call(method, endpoint string, query url.Values, body any) (plugin.InvokeResult, error) {
	status, raw, err := c.do(method, endpoint, query, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(raw)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: shapeOutputs(decoded, status)}, nil
}

// do performs one HTTP request against the NPM API through the cached JWT,
// re-authenticating once on a 401 (an expired or invalid token) before
// giving up. A non-2xx status (after any retry) is translated into a
// CodeInternalError carrying the status and body — callers never have to
// check status codes themselves.
func (c *npmClient) do(method, endpoint string, query url.Values, body any) (int, []byte, error) {
	if err := c.ensureLoggedIn(); err != nil {
		return 0, nil, err
	}
	status, raw, err := c.send(method, endpoint, query, body)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if status == http.StatusUnauthorized {
		if err := c.relogin(); err != nil {
			return 0, nil, err
		}
		status, raw, err = c.send(method, endpoint, query, body)
		if err != nil {
			return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
		}
	}
	if status < 200 || status >= 300 {
		return status, raw, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s %s: %d %s", method, endpoint, status, strings.TrimSpace(string(raw))))
	}
	return status, raw, nil
}

// send issues one request carrying the cached JWT as
// Authorization: Bearer <token>. It never interprets the result — do() owns
// the auth-retry and error-shaping policy.
func (c *npmClient) send(method, endpoint string, query url.Values, body any) (int, []byte, error) {
	full := endpoint
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// --- api escape hatch ---

func (c *npmClient) api(o map[string]any) (plugin.InvokeResult, error) {
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
	return c.call(method, c.conn.apiBase()+path, q, o["body"])
}

// --- decoding helpers ---

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

// shapeOutputs turns a decoded JSON value into a verb's outputs: NPM's list
// endpoints answer with a bare JSON array, hoisted into items; anything else
// (an object, a bare boolean like NPM's enable/disable response, ...) is
// carried in result. status_code is always present.
func shapeOutputs(decoded any, status int) map[string]any {
	out := map[string]any{"status_code": status}
	switch v := decoded.(type) {
	case []any:
		out["items"] = v
	default:
		if decoded != nil {
			out["result"] = decoded
		}
	}
	return out
}

func main() {
	if err := plugin.Serve(newNPMPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-nginx-proxy-manager:", err)
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

// intStr renders an integer-ish option as a string ("" if absent).
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
	return fmt.Sprintf("%v", v)
}
