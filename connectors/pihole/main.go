// Command conductor-pihole is a verb-only conductor connector for Pi-hole v6
// (the modern REST API, /api/*): stats/summary, history, the query log,
// top-domains/top-clients/upstreams breakdowns, blocking on/off, allow/deny
// domain management, lists/groups/clients, a gravity update trigger, and a
// raw `api` escape hatch for anything a first-class verb does not cover.
// Built ONLY against the public SDK (pkg/plugin) and the standard library —
// no third-party client.
//
// Pi-hole v6 authenticates by exchanging a password for a session: a
// successful POST /api/auth returns a session ID (SID) and a CSRF token.
// Every subsequent request carries the SID via the X-FTL-SID header; a
// state-changing request (anything other than GET) also carries the CSRF
// token via X-FTL-CSRF. This connector logs in once per connector instance
// and caches the session across Invoke calls, transparently re-authenticating
// on a 401 (an expired or invalid SID) before giving up.
//
// Connection:
//
//	base_url:              "https://pihole.example.com"  # required; the Pi-hole web/API root (no trailing /api)
//	password:               "<web/app password>"         # required; exchanged for a session via POST /api/auth
//	insecure_skip_verify:   false                          # optional; skip TLS verification for self-signed certs
//
// Every request goes to base_url + "/api" + <endpoint> (the auth exchange
// itself is base_url + "/api/auth"). A non-2xx response is returned as a
// CodeInternalError carrying the status code and response body — nothing is
// swallowed.
//
// insecure_skip_verify exists because self-signed certificates are common on
// a home-lab Pi-hole box; setting it disables TLS certificate verification
// for that connector instance, which means the connection is no longer
// protected against a man-in-the-middle. Prefer importing the box's real
// certificate instead when possible.
//
// Pi-hole is self-hosted with no fixed public host, so Capabilities.Egress is
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
	"strconv"
	"strings"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- plugin ---

type piholePlugin struct {
	mu      sync.Mutex
	clients map[string]*piholeClient
}

func newPiholePlugin() *piholePlugin {
	return &piholePlugin{}
}

func (p *piholePlugin) Describe() plugin.Decl {
	res := plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}}
	list := plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}}

	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "pihole",
		Desc: "Pi-hole v6 REST API: stats summary/history/query log, top domains/clients/upstreams, blocking on/off, allow/deny domain management, lists/groups/clients, a gravity update trigger, and a raw `api` escape hatch. Session (password -> SID) auth. Self-hosted; declares no egress (narrow with network: per instance). No source.",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "Pi-hole v6 web/API root, e.g. https://pihole.example.com (no trailing /api)"},
			"password":             {Type: "string", Required: true, Desc: "Pi-hole web/app password, exchanged for a session via POST /api/auth"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification (default false) — common with self-signed certs on a home-lab Pi-hole, but disables protection against a man-in-the-middle"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "summary", Desc: "overall stats summary (queries, clients, gravity)",
				Usage:   "GET /api/stats/summary",
				Options: plugin.Schema{},
				Outputs: res,
			},
			{
				Name: "history", Desc: "query count history, bucketed over time",
				Usage: "GET /api/history",
				Options: plugin.Schema{
					"from":  {Type: "integer", Desc: "start of the range, unix seconds"},
					"until": {Type: "integer", Desc: "end of the range, unix seconds"},
				},
				Outputs: list,
			},
			{
				Name: "queries", Desc: "the raw query log, paginated",
				Usage: "GET /api/queries",
				Options: plugin.Schema{
					"from":     {Type: "integer", Desc: "start of the range, unix seconds"},
					"until":    {Type: "integer", Desc: "end of the range, unix seconds"},
					"length":   {Type: "integer", Desc: "page size"},
					"cursor":   {Type: "string", Desc: "opaque pagination cursor from a previous call"},
					"domain":   {Type: "string", Desc: "filter to this domain"},
					"client":   {Type: "string", Scope: "client", Desc: "filter to this client (IP/name)"},
					"upstream": {Type: "string", Desc: "filter to this upstream"},
					"type":     {Type: "string", Desc: "filter to this query type, e.g. A, AAAA"},
					"status":   {Type: "string", Desc: "filter to this query status, e.g. GRAVITY, FORWARDED"},
					"blocked":  {Type: "boolean", Desc: "filter to blocked (or, if false, permitted) queries"},
				},
				Outputs: list,
			},
			{
				Name: "top_domains", Desc: "the most-queried domains",
				Usage: "GET /api/stats/top_domains",
				Options: plugin.Schema{
					"count":   {Type: "integer", Desc: "number of domains to return (default 10)"},
					"blocked": {Type: "boolean", Desc: "top blocked domains instead of top permitted domains"},
				},
				Outputs: list,
			},
			{
				Name: "top_clients", Desc: "the clients generating the most queries",
				Usage: "GET /api/stats/top_clients",
				Options: plugin.Schema{
					"count":   {Type: "integer", Desc: "number of clients to return (default 10)"},
					"blocked": {Type: "boolean", Desc: "top clients by blocked queries instead of total queries"},
				},
				Outputs: list,
			},
			{
				Name: "upstreams", Desc: "the configured upstream DNS servers and their usage",
				Usage:   "GET /api/stats/upstreams",
				Options: plugin.Schema{},
				Outputs: list,
			},
			{
				Name: "blocking", Desc: "current blocking status",
				Usage:   "GET /api/dns/blocking",
				Options: plugin.Schema{},
				Outputs: res,
			},
			{
				Name: "set_blocking", Desc: "enable or disable blocking, optionally for a limited time",
				Usage: "POST /api/dns/blocking",
				Options: plugin.Schema{
					"blocking": {Type: "boolean", Required: true, Desc: "true to enable blocking, false to disable it"},
					"timer":    {Type: "integer", Desc: "seconds until blocking automatically reverts (omit for no timer)"},
				},
				Outputs: res,
			},
			{
				Name: "domains", Desc: "list allow/deny domain rules",
				Usage: "GET /api/domains",
				Options: plugin.Schema{
					"type": {Type: "string", Enum: []string{"allow", "deny"}, Desc: "filter to this rule type"},
					"kind": {Type: "string", Enum: []string{"exact", "regex"}, Desc: "filter to this rule kind"},
				},
				Outputs: list,
			},
			{
				Name: "domain_add", Desc: "add an allow/deny domain rule",
				Usage: "POST /api/domains/{type}/{kind}",
				Options: plugin.Schema{
					"type":    {Type: "string", Required: true, Enum: []string{"allow", "deny"}, Desc: "rule type"},
					"kind":    {Type: "string", Required: true, Enum: []string{"exact", "regex"}, Desc: "rule kind"},
					"domain":  {Type: "string", Required: true, Scope: "domain", Desc: "the domain (or regex) to add"},
					"comment": {Type: "string", Desc: "optional free-text comment"},
					"enabled": {Type: "boolean", Desc: "whether the rule is enabled (default true)"},
				},
				Outputs: res,
			},
			{
				Name: "domain_remove", Desc: "remove an allow/deny domain rule",
				Usage: "DELETE /api/domains/{type}/{kind}/{domain}",
				Options: plugin.Schema{
					"type":   {Type: "string", Required: true, Enum: []string{"allow", "deny"}, Desc: "rule type"},
					"kind":   {Type: "string", Required: true, Enum: []string{"exact", "regex"}, Desc: "rule kind"},
					"domain": {Type: "string", Required: true, Scope: "domain", Desc: "the domain (or regex) to remove"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "lists", Desc: "list configured adlists",
				Usage: "GET /api/lists",
				Options: plugin.Schema{
					"type": {Type: "string", Enum: []string{"allow", "block"}, Desc: "filter to this list type"},
				},
				Outputs: list,
			},
			{
				Name: "groups", Desc: "list configured groups",
				Usage:   "GET /api/groups",
				Options: plugin.Schema{},
				Outputs: list,
			},
			{
				Name: "clients", Desc: "list known clients",
				Usage:   "GET /api/clients",
				Options: plugin.Schema{},
				Outputs: list,
			},
			{
				Name: "gravity_update", Desc: "trigger a gravity (blocklist) update",
				Usage:   "POST /api/action/gravity",
				Options: plugin.Schema{},
				Outputs: res,
			},
			{
				Name: "api", Desc: "raw escape hatch: any Pi-hole API endpoint",
				Usage: "method + path under /api, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under /api, e.g. /stats/summary"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: list,
			},
		},
		// Pi-hole is self-hosted: there is no fixed public host to declare.
		// The operator narrows egress to their own instance with
		// `network: ["pihole.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

// clientFor builds (or reuses) the piholeClient for one connector instance,
// so the cached SID/CSRF session persists across Invoke calls instead of
// logging in on every single verb call. A credential change (a different
// base_url/password/insecure_skip_verify) rebuilds the client, discarding any
// stale session.
func (p *piholePlugin) clientFor(instance string, connMap map[string]any) (*piholeClient, error) {
	conn, err := parseConn(connMap)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = map[string]*piholeClient{}
	}
	if c, ok := p.clients[instance]; ok && c.conn == conn {
		return c, nil
	}
	c := &piholeClient{http: httpClientFor(conn), conn: conn}
	p.clients[instance] = c
	return c, nil
}

// httpClientFor builds the *http.Client for one connection: TLS-verified by
// default, or with certificate verification disabled when the connection
// asked for insecure_skip_verify.
func httpClientFor(conn piholeConn) *http.Client {
	if !conn.insecureSkipVerify {
		return &http.Client{Timeout: 30 * time.Second}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // opt-in per connection, for self-signed home-lab certs
	}
}

func (p *piholePlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	c, err := p.clientFor(req.Instance, req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "summary":
		return c.summary()
	case "history":
		return c.history(o)
	case "queries":
		return c.queries(o)
	case "top_domains":
		return c.topDomains(o)
	case "top_clients":
		return c.topClients(o)
	case "upstreams":
		return c.upstreams()
	case "blocking":
		return c.blocking()
	case "set_blocking":
		return c.setBlocking(o)
	case "domains":
		return c.domains(o)
	case "domain_add":
		return c.domainAdd(o)
	case "domain_remove":
		return c.domainRemove(o)
	case "lists":
		return c.lists(o)
	case "groups":
		return c.groups()
	case "clients":
		return c.clientsVerb()
	case "gravity_update":
		return c.gravityUpdate()
	case "api":
		return c.api(o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

// piholeConn is the resolved, comparable connection config for one connector
// instance — comparable so clientFor can detect a credential change and
// rebuild the client (and its cached session) instead of reusing a stale one.
type piholeConn struct {
	baseURL            string
	password           string
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (piholeConn, error) {
	base := strings.TrimRight(str(m["base_url"]), "/")
	if base == "" {
		return piholeConn{}, fmt.Errorf("base_url is required")
	}
	password := str(m["password"])
	if password == "" {
		return piholeConn{}, fmt.Errorf("password is required")
	}
	return piholeConn{baseURL: base, password: password, insecureSkipVerify: boolv(m["insecure_skip_verify"])}, nil
}

// apiBase is conn.base_url + "/api" — every first-class verb's endpoint is
// relative to this. The auth exchange itself is base_url + "/api/auth",
// i.e. apiBase() + "/auth". Overridable simply by pointing base_url at a
// httptest.Server in tests.
func (c piholeConn) apiBase() string { return c.baseURL + "/api" }

// --- session client ---

// piholeClient is the per-instance HTTP client plus its cached SID/CSRF
// session, established by exchanging the connection's password for a
// session via POST /api/auth.
type piholeClient struct {
	mu   sync.Mutex
	http *http.Client
	conn piholeConn
	sid  string
	csrf string
}

// ensureLoggedIn logs in if this client has no cached session yet.
func (c *piholeClient) ensureLoggedIn() error {
	c.mu.Lock()
	sid := c.sid
	c.mu.Unlock()
	if sid != "" {
		return nil
	}
	return c.login()
}

// relogin discards the cached session and logs in again, e.g. after a 401.
func (c *piholeClient) relogin() error {
	c.mu.Lock()
	c.sid, c.csrf = "", ""
	c.mu.Unlock()
	return c.login()
}

// login exchanges the connection's password for a session via
// POST /api/auth and caches the returned SID/CSRF. Pi-hole answers a bad
// password with a 401 and an {"error":{...}} body, so a non-2xx status (or a
// {"session":{"valid":false}} body) is an explicit login failure.
func (c *piholeClient) login() error {
	payload, err := json.Marshal(map[string]any{"password": c.conn.password})
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "pihole login: "+err.Error())
	}
	req, err := http.NewRequest(http.MethodPost, c.conn.apiBase()+"/auth", bytes.NewReader(payload))
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "pihole login: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "pihole login: "+err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "pihole login: reading response: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("pihole login failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}
	var parsed struct {
		Session struct {
			Valid bool   `json:"valid"`
			SID   string `json:"sid"`
			CSRF  string `json:"csrf"`
		} `json:"session"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "pihole login: decoding response: "+err.Error())
	}
	if !parsed.Session.Valid || parsed.Session.SID == "" {
		return plugin.Errorf(plugin.CodeInternalError, "pihole login: session not valid: "+strings.TrimSpace(string(body)))
	}
	c.mu.Lock()
	c.sid, c.csrf = parsed.Session.SID, parsed.Session.CSRF
	c.mu.Unlock()
	return nil
}

// do performs one HTTP request against the Pi-hole API through the cached
// session, re-authenticating once on a 401 (an expired/invalid SID) before
// giving up. A non-2xx status (after any retry) is translated into a
// CodeInternalError carrying the status and body — callers never have to
// check status codes themselves.
func (c *piholeClient) do(method, endpoint string, query url.Values, body any) (int, []byte, error) {
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

// send issues one request carrying the cached SID (X-FTL-SID) and, for any
// state-changing method (anything but GET), the CSRF token (X-FTL-CSRF). It
// never interprets the result — do() owns the auth-retry and error-shaping
// policy.
func (c *piholeClient) send(method, endpoint string, query url.Values, body any) (int, []byte, error) {
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
	sid, csrf := c.sid, c.csrf
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set("X-FTL-SID", sid)
	}
	if csrf != "" && method != http.MethodGet {
		req.Header.Set("X-FTL-CSRF", csrf)
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

// --- verb implementations ---

func (c *piholeClient) summary() (plugin.InvokeResult, error) {
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/stats/summary", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (c *piholeClient) history(o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	setIf(q, "from", intStr(o["from"]))
	setIf(q, "until", intStr(o["until"]))
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/history", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "history")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (c *piholeClient) queries(o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	setIf(q, "from", intStr(o["from"]))
	setIf(q, "until", intStr(o["until"]))
	setIf(q, "length", intStr(o["length"]))
	setIf(q, "cursor", str(o["cursor"]))
	setIf(q, "domain", str(o["domain"]))
	setIf(q, "client", str(o["client"]))
	setIf(q, "upstream", str(o["upstream"]))
	setIf(q, "type", str(o["type"]))
	setIf(q, "status", str(o["status"]))
	if v, ok := o["blocked"]; ok {
		q.Set("blocked", strconv.FormatBool(boolv(v)))
	}
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/queries", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "queries")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (c *piholeClient) topDomains(o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	setIf(q, "count", intStr(o["count"]))
	if v, ok := o["blocked"]; ok {
		q.Set("blocked", strconv.FormatBool(boolv(v)))
	}
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/stats/top_domains", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "domains")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (c *piholeClient) topClients(o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	setIf(q, "count", intStr(o["count"]))
	if v, ok := o["blocked"]; ok {
		q.Set("blocked", strconv.FormatBool(boolv(v)))
	}
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/stats/top_clients", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "clients")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (c *piholeClient) upstreams() (plugin.InvokeResult, error) {
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/stats/upstreams", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "upstreams")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (c *piholeClient) blocking() (plugin.InvokeResult, error) {
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/dns/blocking", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (c *piholeClient) setBlocking(o map[string]any) (plugin.InvokeResult, error) {
	if _, ok := o["blocking"]; !ok {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "blocking is required")
	}
	payload := map[string]any{"blocking": boolv(o["blocking"])}
	if v, ok := o["timer"]; ok {
		payload["timer"] = v
	}
	status, body, err := c.do(http.MethodPost, c.conn.apiBase()+"/dns/blocking", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (c *piholeClient) domains(o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	setIf(q, "type", str(o["type"]))
	setIf(q, "kind", str(o["kind"]))
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/domains", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "domains")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (c *piholeClient) domainAdd(o map[string]any) (plugin.InvokeResult, error) {
	t, k, err := requireTypeKind(o)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	domain := str(o["domain"])
	if domain == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "domain is required")
	}
	payload := map[string]any{"domain": domain}
	if v := str(o["comment"]); v != "" {
		payload["comment"] = v
	}
	if v, ok := o["enabled"]; ok {
		payload["enabled"] = boolv(v)
	}
	path := c.conn.apiBase() + "/domains/" + url.PathEscape(t) + "/" + url.PathEscape(k)
	status, body, err := c.do(http.MethodPost, path, nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (c *piholeClient) domainRemove(o map[string]any) (plugin.InvokeResult, error) {
	t, k, err := requireTypeKind(o)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	domain := str(o["domain"])
	if domain == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "domain is required")
	}
	path := c.conn.apiBase() + "/domains/" + url.PathEscape(t) + "/" + url.PathEscape(k) + "/" + url.PathEscape(domain)
	status, body, err := c.do(http.MethodDelete, path, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// requireTypeKind validates the type/kind pair shared by domain_add and
// domain_remove: type must be allow|deny, kind must be exact|regex.
func requireTypeKind(o map[string]any) (string, string, error) {
	t := str(o["type"])
	if t != "allow" && t != "deny" {
		return "", "", plugin.Errorf(plugin.CodeInvalidParams, "type must be \"allow\" or \"deny\"")
	}
	k := str(o["kind"])
	if k != "exact" && k != "regex" {
		return "", "", plugin.Errorf(plugin.CodeInvalidParams, "kind must be \"exact\" or \"regex\"")
	}
	return t, k, nil
}

func (c *piholeClient) lists(o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	setIf(q, "type", str(o["type"]))
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/lists", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "lists")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (c *piholeClient) groups() (plugin.InvokeResult, error) {
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/groups", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "groups")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (c *piholeClient) clientsVerb() (plugin.InvokeResult, error) {
	status, body, err := c.do(http.MethodGet, c.conn.apiBase()+"/clients", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "clients")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

// gravityUpdate triggers a blocklist update. Pi-hole's response body is not
// guaranteed to be JSON (it may be a plain-text status), so a body that
// fails to decode as JSON is carried through as a plain string rather than
// treated as an error.
func (c *piholeClient) gravityUpdate() (plugin.InvokeResult, error) {
	status, body, err := c.do(http.MethodPost, c.conn.apiBase()+"/action/gravity", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil {
		if decoded != nil {
			out["result"] = decoded
		}
	} else if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		out["result"] = trimmed
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (c *piholeClient) api(o map[string]any) (plugin.InvokeResult, error) {
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
	status, body, err := c.do(method, c.conn.apiBase()+path, q, o["body"])
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
	if err := plugin.Serve(newPiholePlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-pihole:", err)
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

// setIf sets a query parameter only when val is non-empty, so absent options
// never appear as empty query string keys.
func setIf(q url.Values, key, val string) {
	if val != "" {
		q.Set(key, val)
	}
}
