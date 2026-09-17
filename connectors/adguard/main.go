// Command conductor-adguard is a verb-only conductor connector for AdGuard
// Home, a self-hosted network-wide DNS ad/tracker blocker. It drives the
// AdGuard Home REST API over net/http: server status, stats, query log,
// protection toggle, DNS filtering (status/allow-list/block-list/custom
// rules), DNS rewrites, clients, DNS info/config, safe browsing and
// parental-control toggles, and a raw `api` escape hatch for anything a
// first-class verb does not cover. Built ONLY against the public SDK
// (pkg/plugin) — no other dependency.
//
// AdGuard Home authenticates its API with HTTP Basic auth: the username and
// password of an AdGuard Home user (the same credentials used to log into
// its web UI) are sent as the Basic-auth username/password — there is no
// bearer token or API key. This connector has no source: the AdGuard Home
// API is request/response only, with no webhook or event-stream mechanism to
// listen on. It has no Events and does not implement SourceHandler.
//
// Connection:
//
//	base_url:             "http://adguard.example.com"  # required; instance root (no trailing /control)
//	username:              "<username>"                 # required; Basic-auth username
//	password:              "<password>"                 # required; Basic-auth password
//	insecure_skip_verify:  false                         # optional; see risk note below
//
// Every request goes to base_url + "/control" + <endpoint>, authenticated
// with HTTP Basic auth (username/password). A non-2xx response is returned
// as a CodeInternalError carrying the status code and response body —
// nothing is swallowed.
//
// insecure_skip_verify disables TLS certificate verification. AdGuard Home
// instances commonly run behind a self-signed certificate on a LAN, so this
// exists as an explicit, greppable opt-out — but it also disables all
// protection against a man-in-the-middle on the path to the instance. Only
// enable it for instances reached over a trusted network, and prefer
// installing a real certificate instead when possible.
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
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type adguardPlugin struct{}

func newAdguardPlugin() *adguardPlugin { return &adguardPlugin{} }

func (p *adguardPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "adguard",
		Desc: "AdGuard Home: server status/stats, query log, protection toggle, DNS filtering (status/allow-list/block-list/custom rules), DNS rewrites, clients, DNS info/config, and safe-browsing/parental-control toggles over the AdGuard Home REST API. Self-hosted; declares no egress (narrow with network: per instance). No source — the AdGuard Home API is request/response only.",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "AdGuard Home instance root, e.g. http://adguard.example.com (no trailing /control)"},
			"username":             {Type: "string", Required: true, Desc: "AdGuard Home username, sent as the HTTP Basic auth username"},
			"password":             {Type: "string", Required: true, Desc: "AdGuard Home password, sent as the HTTP Basic auth password"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification (default false); self-signed certs are common on LAN deployments, but this disables protection against MITM — only enable for trusted networks"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "status", Desc: "server status (version, ports, protection state, running)",
				Usage:   "GET /control/status",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "stats", Desc: "query statistics (counts, top domains/clients, processing time)",
				Usage:   "GET /control/stats",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "querylog", Desc: "the DNS query log",
				Usage: "GET /control/querylog",
				Options: plugin.Schema{
					"older_than": {Type: "string", Desc: "return entries older than this timestamp (RFC3339)"},
					"limit":      {Type: "integer", Desc: "max number of entries"},
					"search":     {Type: "string", Desc: "search/filter term"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.data"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "protection", Desc: "enable/disable protection, optionally for a fixed duration",
				Usage: "POST /control/protection",
				Options: plugin.Schema{
					"enabled":  {Type: "boolean", Required: true, Desc: "true to enable protection, false to disable"},
					"duration": {Type: "integer", Desc: "pause duration in milliseconds (0/omitted = indefinite)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "filtering_status", Desc: "filtering config: enabled, update interval, filter lists, custom rules",
				Usage:   "GET /control/filtering/status",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "filtering_add_url", Desc: "add a filter (block-list or allow-list) subscription",
				Usage: "POST /control/filtering/add_url",
				Options: plugin.Schema{
					"name":      {Type: "string", Required: true, Desc: "display name for the filter list"},
					"url":       {Type: "string", Required: true, Desc: "filter list URL"},
					"whitelist": {Type: "boolean", Desc: "true to add as an allow-list (whitelist) instead of a block-list"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "filtering_remove_url", Desc: "remove a filter subscription",
				Usage: "POST /control/filtering/remove_url",
				Options: plugin.Schema{
					"url":       {Type: "string", Required: true, Desc: "filter list URL to remove"},
					"whitelist": {Type: "boolean", Desc: "true if the URL is an allow-list (whitelist) filter"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "filtering_set_rules", Desc: "replace the custom (user-defined) filtering rules",
				Usage: "POST /control/filtering/set_rules",
				Options: plugin.Schema{
					"rules": {Type: "list", Required: true, Desc: "full list of custom filtering rules, replacing the current set"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "rewrites", Desc: "list DNS rewrites",
				Usage:   "GET /control/rewrite/list",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "rewrite_add", Desc: "add a DNS rewrite",
				Usage: "POST /control/rewrite/add",
				Options: plugin.Schema{
					"domain": {Type: "string", Required: true, Desc: "domain to rewrite"},
					"answer": {Type: "string", Required: true, Desc: "IP address or CNAME target to answer with"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "rewrite_delete", Desc: "delete a DNS rewrite",
				Usage: "POST /control/rewrite/delete",
				Options: plugin.Schema{
					"domain": {Type: "string", Required: true, Desc: "domain of the rewrite to delete"},
					"answer": {Type: "string", Required: true, Desc: "answer of the rewrite to delete"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "clients", Desc: "list configured and auto-discovered clients",
				Usage:   "GET /control/clients",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.clients"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "dns_info", Desc: "current DNS server configuration",
				Usage:   "GET /control/dns_info",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "dns_config", Desc: "update DNS server configuration",
				Usage: "POST /control/dns_config",
				Options: plugin.Schema{
					"config": {Type: "map", Required: true, Desc: "DNS config fields to update, e.g. {upstream_dns, bootstrap_dns, ratelimit, blocking_mode, cache_enabled, dnssec_enabled, ...}"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "safebrowsing_toggle", Desc: "enable/disable the safe browsing filter",
				Usage: "POST /control/safebrowsing/{enable|disable}",
				Options: plugin.Schema{
					"enabled": {Type: "boolean", Required: true, Desc: "true to enable, false to disable"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "parental_toggle", Desc: "enable/disable AdGuard parental control",
				Usage: "POST /control/parental/{enable|disable}",
				Options: plugin.Schema{
					"enabled": {Type: "boolean", Required: true, Desc: "true to enable, false to disable"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any AdGuard Home API endpoint",
				Usage: "method + path under /control, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under /control, e.g. /filtering/status"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// AdGuard Home is self-hosted: there is no fixed public host to
		// declare. The operator narrows egress to their own instance with
		// `network: ["adguard.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func (p *adguardPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "status":
		return p.status(conn)
	case "stats":
		return p.stats(conn)
	case "querylog":
		return p.querylog(conn, o)
	case "protection":
		return p.protection(conn, o)
	case "filtering_status":
		return p.filteringStatus(conn)
	case "filtering_add_url":
		return p.filteringAddURL(conn, o)
	case "filtering_remove_url":
		return p.filteringRemoveURL(conn, o)
	case "filtering_set_rules":
		return p.filteringSetRules(conn, o)
	case "rewrites":
		return p.rewrites(conn)
	case "rewrite_add":
		return p.rewriteAdd(conn, o)
	case "rewrite_delete":
		return p.rewriteDelete(conn, o)
	case "clients":
		return p.clients(conn)
	case "dns_info":
		return p.dnsInfo(conn)
	case "dns_config":
		return p.dnsConfig(conn, o)
	case "safebrowsing_toggle":
		return p.safebrowsingToggle(conn, o)
	case "parental_toggle":
		return p.parentalToggle(conn, o)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type adguardConn struct {
	baseURL            string
	username           string
	password           string
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (adguardConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return adguardConn{}, fmt.Errorf("base_url is required")
	}
	username := str(m["username"])
	if username == "" {
		return adguardConn{}, fmt.Errorf("username is required")
	}
	password := str(m["password"])
	if password == "" {
		return adguardConn{}, fmt.Errorf("password is required")
	}
	return adguardConn{
		baseURL:            strings.TrimRight(base, "/"),
		username:           username,
		password:           password,
		insecureSkipVerify: boolv(m["insecure_skip_verify"]),
	}, nil
}

// apiBase is conn.base_url + "/control" — every first-class verb's endpoint
// is relative to this. Overridable simply by pointing base_url at a
// httptest.Server in tests.
func (c adguardConn) apiBase() string { return c.baseURL + "/control" }

// --- verb implementations ---

func (p *adguardPlugin) status(conn adguardConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/status", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *adguardPlugin) stats(conn adguardConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/stats", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *adguardPlugin) querylog(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["older_than"]); v != "" {
		q.Set("older_than", v)
	}
	if v := intStr(o["limit"]); v != "" {
		q.Set("limit", v)
	}
	if v := str(o["search"]); v != "" {
		q.Set("search", v)
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/querylog", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "data")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (p *adguardPlugin) protection(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
	enabledVal, ok := o["enabled"]
	if !ok {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "enabled is required")
	}
	payload := map[string]any{"enabled": boolv(enabledVal)}
	if v, ok := o["duration"]; ok {
		payload["duration"] = v
	}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/protection", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *adguardPlugin) filteringStatus(conn adguardConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/filtering/status", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *adguardPlugin) filteringAddURL(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
	name := str(o["name"])
	if name == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "name is required")
	}
	furl := str(o["url"])
	if furl == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "url is required")
	}
	payload := map[string]any{"name": name, "url": furl, "whitelist": boolv(o["whitelist"])}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/filtering/add_url", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *adguardPlugin) filteringRemoveURL(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
	furl := str(o["url"])
	if furl == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "url is required")
	}
	payload := map[string]any{"url": furl, "whitelist": boolv(o["whitelist"])}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/filtering/remove_url", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *adguardPlugin) filteringSetRules(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
	rules := strList(o["rules"])
	if rules == nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "rules is required")
	}
	payload := map[string]any{"rules": rules}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/filtering/set_rules", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *adguardPlugin) rewrites(conn adguardConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/rewrite/list", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded)
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *adguardPlugin) rewriteAdd(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
	domain := str(o["domain"])
	if domain == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "domain is required")
	}
	answer := str(o["answer"])
	if answer == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "answer is required")
	}
	payload := map[string]any{"domain": domain, "answer": answer}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/rewrite/add", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *adguardPlugin) rewriteDelete(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
	domain := str(o["domain"])
	if domain == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "domain is required")
	}
	answer := str(o["answer"])
	if answer == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "answer is required")
	}
	payload := map[string]any{"domain": domain, "answer": answer}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/rewrite/delete", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *adguardPlugin) clients(conn adguardConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/clients", nil, nil)
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

func (p *adguardPlugin) dnsInfo(conn adguardConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/dns_info", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *adguardPlugin) dnsConfig(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
	config, ok := o["config"].(map[string]any)
	if !ok || len(config) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "config is required")
	}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/dns_config", nil, config)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *adguardPlugin) safebrowsingToggle(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
	enabledVal, ok := o["enabled"]
	if !ok {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "enabled is required")
	}
	action := "disable"
	if boolv(enabledVal) {
		action = "enable"
	}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/safebrowsing/"+action, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *adguardPlugin) parentalToggle(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
	enabledVal, ok := o["enabled"]
	if !ok {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "enabled is required")
	}
	action := "disable"
	if boolv(enabledVal) {
		action = "enable"
	}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/parental/"+action, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *adguardPlugin) api(conn adguardConn, o map[string]any) (plugin.InvokeResult, error) {
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
	default:
		if decoded != nil {
			out["result"] = decoded
		}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// --- HTTP plumbing ---

// do performs one HTTP request against the AdGuard Home API, attaching HTTP
// Basic auth (username/password), and returns the status code and raw
// response body. A non-2xx status is translated into a CodeInternalError
// carrying the status and body — the caller never has to check status codes
// itself.
func (p *adguardPlugin) do(conn adguardConn, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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
	req.SetBasicAuth(conn.username, conn.password)
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
func (p *adguardPlugin) clientFor(conn adguardConn) *http.Client {
	client := &http.Client{Timeout: 30 * time.Second}
	if conn.insecureSkipVerify {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- explicit, documented opt-out for self-signed LAN instances
		}
	}
	return client
}

// decodeJSON decodes a JSON response body into a generic value. An empty
// body decodes to nil rather than an error (e.g. a 204/202 with no body).
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
	if err := plugin.Serve(newAdguardPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-adguard:", err)
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

// strList reads a list-of-strings option: a []any of strings (the wire shape),
// a []string, or a single string. Returns nil (not an empty slice) when the
// option is absent, so callers can distinguish "not provided" from "provided
// but empty" when the option is required.
func strList(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
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
