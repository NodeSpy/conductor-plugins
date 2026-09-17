// Command conductor-unifi is a verb-only conductor connector for a UniFi
// Network controller — devices, clients, WLANs, networks, port forwards,
// firewall rules, health, alarms, events, and a raw `api` escape hatch.
// Built ONLY against the public SDK (pkg/plugin) — no other dependency.
//
// Auth is COOKIE-SESSION, not a bearer token: the controller's login
// endpoint sets session cookies (net/http/cookiejar keeps them) and, on a
// modern UniFi OS controller, also returns an X-CSRF-Token header that must
// be echoed back on every mutating (POST/PUT/DELETE) request. The session
// (cookie jar + csrf token) is cached on the plugin, keyed by
// base_url+username+insecure_skip_verify, and is reused across Invoke calls
// until a request comes back 401, at which point the connector re-logs-in
// once and retries.
//
// Two controller shapes are supported:
//
//   - UniFi OS (UDM / UDM Pro / Cloud Key Gen2+, unifi_os: true, the
//     default): logs in at POST /api/auth/login and proxies the network
//     application under /proxy/network, so every verb's path is prefixed
//     with /proxy/network.
//   - Legacy standalone controller (unifi_os: false): logs in at
//     POST /api/login and has no /proxy/network prefix at all.
//
// Connection:
//
//	base_url:              "https://unifi.example.com"  # required; e.g. https://10.0.0.1
//	username:               "admin"                      # required
//	password:               "..."                        # required
//	site:                   "default"                    # optional, default "default"
//	unifi_os:               true                          # optional, default true
//	insecure_skip_verify:   false                         # optional, default false; self-signed certs
//
// insecure_skip_verify disables TLS certificate verification for THIS
// connection only (a per-connection tls.Config, never process-wide). Only
// set it for a controller you reach over a trusted network path (e.g. your
// own LAN/VPN) — it removes protection against a man-in-the-middle
// presenting a forged certificate.
//
// UniFi wraps every response as {"meta": {...}, "data": [...]}. This
// connector hoists `data` into `items` when it is a list, or `result`
// otherwise, and always returns `status_code`. A non-2xx response is
// returned as a CodeInternalError carrying the status code and response
// body — nothing is swallowed.
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
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type unifiPlugin struct {
	mu       sync.Mutex
	sessions map[string]*unifiSession
}

func newUnifiPlugin() *unifiPlugin {
	return &unifiPlugin{sessions: map[string]*unifiSession{}}
}

// unifiSession is one authenticated session against one controller: a cookie
// jar (holding the controller's session cookie) plus the CSRF token the
// controller handed back at login. Every request that touches this session
// (including login/re-login) is serialized through mu, so a 401-triggered
// re-login can never race with another goroutine's request on the same
// session (Run may be called concurrently — see pkg/plugin serve.go).
type unifiSession struct {
	mu       sync.Mutex
	client   *http.Client
	csrf     string
	loggedIn bool
}

func (p *unifiPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "unifi",
		Desc: "UniFi Network controller: devices, clients, WLANs, networks, port forwards, firewall rules, health, alarms, and events over the UniFi Network API. Cookie-session auth (not a bearer token) with UniFi OS CSRF echo and automatic re-login on 401. Self-hosted; declares no egress (narrow with network: per instance). No source.",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "controller root, e.g. https://unifi.example.com or https://10.0.0.1 (no trailing path)"},
			"username":             {Type: "string", Required: true, Desc: "local controller username"},
			"password":             {Type: "string", Required: true, Desc: "local controller password"},
			"site":                 {Type: "string", Desc: "site name (default \"default\")"},
			"unifi_os":             {Type: "boolean", Desc: "true (default) for UniFi OS controllers (UDM/Cloud Key gen2+): logs in at /api/auth/login and proxies the network API under /proxy/network. false for a legacy standalone controller: logs in at /api/login with no proxy prefix."},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification for this connection (default false). Only for a self-signed controller reached over a trusted network — this removes protection against a forged certificate."},
		},
		Verbs: []plugin.Verb{
			{
				Name: "sites", Desc: "list sites this account can see",
				Usage:   "GET {prefix}/api/self/sites",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "devices", Desc: "list UniFi devices (APs, switches, gateways) adopted to the site",
				Usage:   "GET {prefix}/api/s/{site}/stat/device",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "device_restart", Desc: "restart one adopted device",
				Usage: "POST {prefix}/api/s/{site}/cmd/devmgr {cmd:\"restart\", mac}",
				Options: plugin.Schema{
					"mac": {Type: "string", Required: true, Scope: "device", Desc: "device MAC address"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "clients", Desc: "list currently connected clients",
				Usage:   "GET {prefix}/api/s/{site}/stat/sta",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "client_block", Desc: "block a client from the network",
				Usage: "POST {prefix}/api/s/{site}/cmd/stamgr {cmd:\"block-sta\", mac}",
				Options: plugin.Schema{
					"mac": {Type: "string", Required: true, Scope: "device", Desc: "client MAC address"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "client_unblock", Desc: "unblock a previously blocked client",
				Usage: "POST {prefix}/api/s/{site}/cmd/stamgr {cmd:\"unblock-sta\", mac}",
				Options: plugin.Schema{
					"mac": {Type: "string", Required: true, Scope: "device", Desc: "client MAC address"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "client_reconnect", Desc: "force-reconnect (kick) a client",
				Usage: "POST {prefix}/api/s/{site}/cmd/stamgr {cmd:\"kick-sta\", mac}",
				Options: plugin.Schema{
					"mac": {Type: "string", Required: true, Scope: "device", Desc: "client MAC address"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "wlans", Desc: "list configured WLANs",
				Usage:   "GET {prefix}/api/s/{site}/rest/wlanconf",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "networks", Desc: "list configured networks (VLANs/subnets)",
				Usage:   "GET {prefix}/api/s/{site}/rest/networkconf",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "port_forwards", Desc: "list port-forwarding rules",
				Usage:   "GET {prefix}/api/s/{site}/rest/portforward",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "firewall_rules", Desc: "list firewall rules",
				Usage:   "GET {prefix}/api/s/{site}/rest/firewallrule",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "health", Desc: "per-subsystem health summary",
				Usage:   "GET {prefix}/api/s/{site}/stat/health",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "alarms", Desc: "list alarms",
				Usage:   "GET {prefix}/api/s/{site}/list/alarm",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "events", Desc: "list recent site events",
				Usage:   "GET {prefix}/api/s/{site}/stat/event",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any UniFi Network API endpoint",
				Usage: "method + path under {prefix}, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path joined after the unifi_os prefix, e.g. /api/s/default/stat/device"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// A UniFi Network controller is self-hosted: there is no fixed public
		// host to declare. The operator narrows egress to their own controller
		// with `network: ["unifi.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func (p *unifiPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "sites":
		return p.sites(conn)
	case "devices":
		return p.devices(conn)
	case "device_restart":
		return p.deviceRestart(conn, o)
	case "clients":
		return p.clients(conn)
	case "client_block":
		return p.clientCmd(conn, o, "block-sta")
	case "client_unblock":
		return p.clientCmd(conn, o, "unblock-sta")
	case "client_reconnect":
		return p.clientCmd(conn, o, "kick-sta")
	case "wlans":
		return p.wlans(conn)
	case "networks":
		return p.networks(conn)
	case "port_forwards":
		return p.portForwards(conn)
	case "firewall_rules":
		return p.firewallRules(conn)
	case "health":
		return p.health(conn)
	case "alarms":
		return p.alarms(conn)
	case "events":
		return p.events(conn)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type unifiConn struct {
	baseURL            string
	username           string
	password           string
	site               string
	unifiOS            bool
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (unifiConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return unifiConn{}, fmt.Errorf("base_url is required")
	}
	username := str(m["username"])
	if username == "" {
		return unifiConn{}, fmt.Errorf("username is required")
	}
	password := str(m["password"])
	if password == "" {
		return unifiConn{}, fmt.Errorf("password is required")
	}
	site := strOr(m["site"], "default")
	return unifiConn{
		baseURL:            strings.TrimRight(base, "/"),
		username:           username,
		password:           password,
		site:               site,
		unifiOS:            boolOr(m["unifi_os"], true),
		insecureSkipVerify: boolOr(m["insecure_skip_verify"], false),
	}, nil
}

// prefix is the path segment every verb request is prefixed with: a modern
// UniFi OS controller proxies the whole network application under
// /proxy/network; a legacy standalone controller has no such prefix.
func (c unifiConn) prefix() string {
	if c.unifiOS {
		return "/proxy/network"
	}
	return ""
}

// loginPath is where credentials are POSTed. UniFi OS moved this under
// /api/auth/login; a legacy controller still logs in at /api/login (note:
// NOT under the /proxy/network prefix in either case).
func (c unifiConn) loginPath() string {
	if c.unifiOS {
		return "/api/auth/login"
	}
	return "/api/login"
}

func (c unifiConn) sitePath(suffix string) string {
	return "/api/s/" + url.PathEscape(c.site) + suffix
}

// --- session management ---

// sessionKey identifies one cookie-jar session. Sessions are not shared
// across different controllers, users, passwords, or TLS trust settings —
// a credential change (e.g. a rotated password) transparently starts a
// fresh session rather than reusing stale cookies under the old identity.
func sessionKey(c unifiConn) string {
	return c.baseURL + "\x00" + c.username + "\x00" + c.password + "\x00" + boolStr(c.insecureSkipVerify)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (p *unifiPlugin) sessionFor(c unifiConn) *unifiSession {
	key := sessionKey(c)
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[key]; ok {
		return s
	}
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{}
	if c.insecureSkipVerify {
		// Per-connection tls.Config, never process-wide: this only affects
		// requests made through THIS session's client.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- opt-in per connection for self-signed controllers
	}
	s := &unifiSession{client: &http.Client{Jar: jar, Transport: transport, Timeout: 30 * time.Second}}
	p.sessions[key] = s
	return s
}

// login authenticates against the controller and populates the session's
// cookie jar and (on UniFi OS) CSRF token. Must be called with sess.mu held.
func (p *unifiPlugin) login(sess *unifiSession, conn unifiConn) error {
	payload := map[string]any{"username": conn.username, "password": conn.password}
	raw, err := json.Marshal(payload)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req, err := http.NewRequest(http.MethodPost, conn.baseURL+conn.loginPath(), bytes.NewReader(raw))
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sess.client.Do(req)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		sess.loggedIn = false
		return plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("login %s: %d %s", conn.loginPath(), resp.StatusCode, strings.TrimSpace(string(respBody))))
	}
	if csrf := resp.Header.Get("X-CSRF-Token"); csrf != "" {
		sess.csrf = csrf
	}
	sess.loggedIn = true
	return nil
}

// isWriteMethod reports whether method is a mutating verb that must carry
// the CSRF token, per UniFi OS's proxy requirements.
func isWriteMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

// requestOnce performs one HTTP request against fullURL using sess's cookie
// jar, echoing the session's CSRF token on mutating requests. It returns the
// raw status/body/headers without translating a non-2xx status into an
// error — the caller (do) decides how to react (e.g. re-login on 401).
func (p *unifiPlugin) requestOnce(sess *unifiSession, method, fullURL string, query url.Values, body any) (int, []byte, http.Header, error) {
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sess.csrf != "" && isWriteMethod(method) {
		req.Header.Set("X-CSRF-Token", sess.csrf)
	}
	resp, err := sess.client.Do(req)
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

// do performs one authenticated request against a verb endpoint: it ensures
// the session is logged in, echoes CSRF on writes, and re-logs-in once and
// retries on a 401 before giving up. A non-2xx status (after any retry) is
// translated into a CodeInternalError carrying the status and body.
func (p *unifiPlugin) do(conn unifiConn, method, path string, query url.Values, body any) (int, []byte, error) {
	sess := p.sessionFor(conn)
	sess.mu.Lock()
	defer sess.mu.Unlock()

	fullURL := conn.baseURL + conn.prefix() + path

	if !sess.loggedIn {
		if err := p.login(sess, conn); err != nil {
			return 0, nil, err
		}
	}

	status, respBody, hdr, err := p.requestOnce(sess, method, fullURL, query, body)
	if err != nil {
		return 0, nil, err
	}
	if csrf := hdr.Get("X-CSRF-Token"); csrf != "" {
		sess.csrf = csrf
	}

	if status == http.StatusUnauthorized {
		sess.loggedIn = false
		if err := p.login(sess, conn); err != nil {
			return status, respBody, err
		}
		status, respBody, hdr, err = p.requestOnce(sess, method, fullURL, query, body)
		if err != nil {
			return 0, nil, err
		}
		if csrf := hdr.Get("X-CSRF-Token"); csrf != "" {
			sess.csrf = csrf
		}
	}

	if status < 200 || status >= 300 {
		return status, respBody, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s %s: %d %s", method, fullURL, status, strings.TrimSpace(string(respBody))))
	}
	return status, respBody, nil
}

// --- verb implementations ---

func (p *unifiPlugin) sites(conn unifiConn) (plugin.InvokeResult, error) {
	return p.getList(conn, "/api/self/sites", nil)
}

func (p *unifiPlugin) devices(conn unifiConn) (plugin.InvokeResult, error) {
	return p.getList(conn, conn.sitePath("/stat/device"), nil)
}

func (p *unifiPlugin) clients(conn unifiConn) (plugin.InvokeResult, error) {
	return p.getList(conn, conn.sitePath("/stat/sta"), nil)
}

func (p *unifiPlugin) wlans(conn unifiConn) (plugin.InvokeResult, error) {
	return p.getList(conn, conn.sitePath("/rest/wlanconf"), nil)
}

func (p *unifiPlugin) networks(conn unifiConn) (plugin.InvokeResult, error) {
	return p.getList(conn, conn.sitePath("/rest/networkconf"), nil)
}

func (p *unifiPlugin) portForwards(conn unifiConn) (plugin.InvokeResult, error) {
	return p.getList(conn, conn.sitePath("/rest/portforward"), nil)
}

func (p *unifiPlugin) firewallRules(conn unifiConn) (plugin.InvokeResult, error) {
	return p.getList(conn, conn.sitePath("/rest/firewallrule"), nil)
}

func (p *unifiPlugin) health(conn unifiConn) (plugin.InvokeResult, error) {
	return p.getList(conn, conn.sitePath("/stat/health"), nil)
}

func (p *unifiPlugin) alarms(conn unifiConn) (plugin.InvokeResult, error) {
	return p.getList(conn, conn.sitePath("/list/alarm"), nil)
}

func (p *unifiPlugin) events(conn unifiConn) (plugin.InvokeResult, error) {
	return p.getList(conn, conn.sitePath("/stat/event"), nil)
}

// getList performs a GET against path and hoists the UniFi {"meta","data"}
// envelope's data into items.
func (p *unifiPlugin) getList(conn unifiConn, path string, query url.Values) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, path, query, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: unifiOutputs(decoded, status)}, nil
}

func (p *unifiPlugin) deviceRestart(conn unifiConn, o map[string]any) (plugin.InvokeResult, error) {
	mac := str(o["mac"])
	if mac == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "mac is required")
	}
	payload := map[string]any{"cmd": "restart", "mac": mac}
	status, body, err := p.do(conn, http.MethodPost, conn.sitePath("/cmd/devmgr"), nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: unifiOutputs(decoded, status)}, nil
}

// clientCmd issues one POST /cmd/stamgr command (block-sta, unblock-sta,
// kick-sta) against a client's MAC address; block_sta/unblock_sta/kick_sta
// share the same request shape, differing only in the cmd value.
func (p *unifiPlugin) clientCmd(conn unifiConn, o map[string]any, cmd string) (plugin.InvokeResult, error) {
	mac := str(o["mac"])
	if mac == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "mac is required")
	}
	payload := map[string]any{"cmd": cmd, "mac": mac}
	status, body, err := p.do(conn, http.MethodPost, conn.sitePath("/cmd/stamgr"), nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: unifiOutputs(decoded, status)}, nil
}

func (p *unifiPlugin) api(conn unifiConn, o map[string]any) (plugin.InvokeResult, error) {
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
	status, body, err := p.do(conn, method, path, q, o["body"])
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: unifiOutputs(decoded, status)}, nil
}

// --- response shaping ---

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

// unifiOutputs shapes a decoded UniFi response into the connector's output
// map. UniFi wraps every response as {"meta": {...}, "data": [...]}: when
// `data` is present and is a list, it is hoisted into "items" (even an empty
// list, never omitted); when `data` is present but not a list, it is hoisted
// into "result"; a response with no `data` envelope at all (or an empty
// body) falls back to "result" (or is omitted if nil). status_code is
// always present.
func unifiOutputs(decoded any, status int) map[string]any {
	out := map[string]any{"status_code": status}
	switch x := decoded.(type) {
	case map[string]any:
		if data, ok := x["data"]; ok {
			if list, ok := data.([]any); ok {
				out["items"] = list
				return out
			}
			out["result"] = data
			return out
		}
		out["result"] = x
	case []any:
		out["items"] = x
	default:
		if decoded != nil {
			out["result"] = decoded
		}
	}
	return out
}

func main() {
	if err := plugin.Serve(newUnifiPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-unifi:", err)
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

func boolOr(v any, def bool) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		if x == "" {
			return def
		}
		return x == "true" || x == "1" || x == "yes"
	}
	return def
}
