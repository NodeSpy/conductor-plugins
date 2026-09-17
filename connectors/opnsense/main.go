// Command conductor-opnsense is a verb-only conductor connector for OPNsense,
// a self-hosted firewall/router. It drives the OPNsense REST API over
// net/http: firmware status/upgrade, service search/restart/start/stop,
// firewall alias search/get/add/toggle/apply, interfaces, DHCPv4 leases,
// gateway status, Unbound settings, system reboot/status, and a raw `api`
// escape hatch for anything a first-class verb does not cover. Built ONLY
// against the public SDK (pkg/plugin) — no other dependency.
//
// OPNsense authenticates its API with HTTP Basic auth: the "API key" and
// "API secret" a user generates under System > Access > Users are sent as
// the Basic-auth username and password respectively (there is no bearer
// token). This connector has no source: OPNsense's API is request/response
// only, with no webhook or event-stream mechanism to listen on. It has no
// Events and does not implement SourceHandler.
//
// Connection:
//
//	base_url:             "https://opnsense.example.com"  # required; no trailing /api
//	api_key:               "<api key>"                    # required; Basic-auth username
//	api_secret:            "<api secret>"                 # required; Basic-auth password
//	insecure_skip_verify:  false                           # optional; see risk note below
//
// Every request goes to base_url + "/api" + <endpoint>, authenticated with
// HTTP Basic auth (api_key/api_secret). A non-2xx response is returned as a
// CodeInternalError carrying the status code and response body — nothing is
// swallowed.
//
// insecure_skip_verify disables TLS certificate verification. OPNsense
// instances commonly run behind a self-signed certificate on a LAN, so this
// exists as an explicit, greppable opt-out — but it also disables all
// protection against a man-in-the-middle on the path to the firewall. Only
// enable it for instances reached over a trusted network, and prefer
// installing a real certificate (e.g. via the built-in ACME/Let's Encrypt
// plugin) instead when possible.
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

type opnsensePlugin struct{}

func newOpnsensePlugin() *opnsensePlugin { return &opnsensePlugin{} }

func (p *opnsensePlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "opnsense",
		Desc: "OPNsense: firmware, services, firewall aliases, interfaces, DHCPv4 leases, gateway status, Unbound settings, and system reboot/status over the OPNsense REST API. Self-hosted; declares no egress (narrow with network: per instance). No source — the OPNsense API is request/response only.",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "OPNsense instance root, e.g. https://opnsense.example.com (no trailing /api)"},
			"api_key":              {Type: "string", Required: true, Desc: "OPNsense API key, sent as the HTTP Basic auth username"},
			"api_secret":           {Type: "string", Required: true, Desc: "OPNsense API secret, sent as the HTTP Basic auth password"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification (default false); self-signed certs are common on LAN deployments, but this disables protection against MITM — only enable for trusted networks"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "firmware_status", Desc: "check for available firmware/package updates",
				Usage:   "GET /api/core/firmware/status",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "firmware_upgrade", Desc: "run a firmware upgrade",
				Usage:   "POST /api/core/firmware/upgrade",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "services", Desc: "list known services and their running state",
				Usage:   "GET /api/core/service/search",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.rows"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "service_restart", Desc: "restart a service",
				Usage: "POST /api/core/service/restart/{name}",
				Options: plugin.Schema{
					"name": {Type: "string", Required: true, Scope: "service", Desc: "service name, e.g. unbound, dpinger"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "service_start", Desc: "start a service",
				Usage: "POST /api/core/service/start/{name}",
				Options: plugin.Schema{
					"name": {Type: "string", Required: true, Scope: "service", Desc: "service name, e.g. unbound, dpinger"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "service_stop", Desc: "stop a service",
				Usage: "POST /api/core/service/stop/{name}",
				Options: plugin.Schema{
					"name": {Type: "string", Required: true, Scope: "service", Desc: "service name, e.g. unbound, dpinger"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "firewall_aliases", Desc: "search/list firewall aliases",
				Usage:   "GET /api/firewall/alias/searchItem",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.rows"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "alias_get", Desc: "get one firewall alias's details",
				Usage: "GET /api/firewall/alias/getItem/{uuid}",
				Options: plugin.Schema{
					"uuid": {Type: "string", Required: true, Scope: "alias", Desc: "alias UUID"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "alias_add", Desc: "create a firewall alias",
				Usage: "POST /api/firewall/alias/addItem",
				Options: plugin.Schema{
					"alias": {Type: "map", Required: true, Desc: "alias fields, e.g. {name, type, content, description, enabled}"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "alias_toggle", Desc: "enable/disable (or toggle) a firewall alias",
				Usage: "POST /api/firewall/alias/toggleItem/{uuid}[/{enabled}]",
				Options: plugin.Schema{
					"uuid":    {Type: "string", Required: true, Scope: "alias", Desc: "alias UUID"},
					"enabled": {Type: "boolean", Desc: "1 to enable, 0 to disable; omit to toggle the current state"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "firewall_apply", Desc: "apply pending alias changes (reconfigure filter/aliases)",
				Usage:   "POST /api/firewall/alias/reconfigure",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "interfaces", Desc: "interface overview info (physical name, description, enabled state, identifier)",
				Usage:   "GET /api/interfaces/overview/interfacesInfo",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "dhcp_leases", Desc: "search DHCPv4 leases",
				Usage:   "GET /api/dhcpv4/leases/searchLease",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.rows"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "gateway_status", Desc: "gateway monitoring status",
				Usage:   "GET /api/routes/gateway/status",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "unbound_settings", Desc: "current Unbound (DNS resolver) settings",
				Usage:   "GET /api/unbound/settings/get",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "system_reboot", Desc: "reboot the firewall",
				Usage:   "POST /api/core/system/reboot",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "system_status", Desc: "system status (uptime, versions, health)",
				Usage:   "GET /api/core/system/status",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any OPNsense API endpoint",
				Usage: "method + path under /api, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under /api, e.g. /core/firmware/status"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// OPNsense is self-hosted: there is no fixed public host to declare.
		// The operator narrows egress to their own instance with
		// `network: ["opnsense.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func (p *opnsensePlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "firmware_status":
		return p.firmwareStatus(conn)
	case "firmware_upgrade":
		return p.firmwareUpgrade(conn)
	case "services":
		return p.services(conn)
	case "service_restart":
		return p.serviceAction(conn, o, "restart")
	case "service_start":
		return p.serviceAction(conn, o, "start")
	case "service_stop":
		return p.serviceAction(conn, o, "stop")
	case "firewall_aliases":
		return p.firewallAliases(conn)
	case "alias_get":
		return p.aliasGet(conn, o)
	case "alias_add":
		return p.aliasAdd(conn, o)
	case "alias_toggle":
		return p.aliasToggle(conn, o)
	case "firewall_apply":
		return p.firewallApply(conn)
	case "interfaces":
		return p.interfaces(conn)
	case "dhcp_leases":
		return p.dhcpLeases(conn)
	case "gateway_status":
		return p.gatewayStatus(conn)
	case "unbound_settings":
		return p.unboundSettings(conn)
	case "system_reboot":
		return p.systemReboot(conn)
	case "system_status":
		return p.systemStatus(conn)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type opnsenseConn struct {
	baseURL            string
	apiKey             string
	apiSecret          string
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (opnsenseConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return opnsenseConn{}, fmt.Errorf("base_url is required")
	}
	key := str(m["api_key"])
	if key == "" {
		return opnsenseConn{}, fmt.Errorf("api_key is required")
	}
	secret := str(m["api_secret"])
	if secret == "" {
		return opnsenseConn{}, fmt.Errorf("api_secret is required")
	}
	return opnsenseConn{
		baseURL:            strings.TrimRight(base, "/"),
		apiKey:             key,
		apiSecret:          secret,
		insecureSkipVerify: boolv(m["insecure_skip_verify"]),
	}, nil
}

// apiBase is conn.base_url + "/api" — every first-class verb's endpoint is
// relative to this. Overridable simply by pointing base_url at a
// httptest.Server in tests.
func (c opnsenseConn) apiBase() string { return c.baseURL + "/api" }

// --- verb implementations ---

func (p *opnsensePlugin) firmwareStatus(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.get(conn, conn.apiBase()+"/core/firmware/status", nil)
}

func (p *opnsensePlugin) firmwareUpgrade(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.post(conn, conn.apiBase()+"/core/firmware/upgrade", nil)
}

func (p *opnsensePlugin) services(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.get(conn, conn.apiBase()+"/core/service/search", nil)
}

func (p *opnsensePlugin) serviceAction(conn opnsenseConn, o map[string]any, action string) (plugin.InvokeResult, error) {
	name := str(o["name"])
	if name == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "name is required")
	}
	return p.post(conn, conn.apiBase()+"/core/service/"+action+"/"+url.PathEscape(name), nil)
}

func (p *opnsensePlugin) firewallAliases(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.get(conn, conn.apiBase()+"/firewall/alias/searchItem", nil)
}

func (p *opnsensePlugin) aliasGet(conn opnsenseConn, o map[string]any) (plugin.InvokeResult, error) {
	uuid := str(o["uuid"])
	if uuid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uuid is required")
	}
	return p.get(conn, conn.apiBase()+"/firewall/alias/getItem/"+url.PathEscape(uuid), nil)
}

func (p *opnsensePlugin) aliasAdd(conn opnsenseConn, o map[string]any) (plugin.InvokeResult, error) {
	alias, ok := o["alias"].(map[string]any)
	if !ok || len(alias) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "alias is required")
	}
	return p.post(conn, conn.apiBase()+"/firewall/alias/addItem", map[string]any{"alias": alias})
}

func (p *opnsensePlugin) aliasToggle(conn opnsenseConn, o map[string]any) (plugin.InvokeResult, error) {
	uuid := str(o["uuid"])
	if uuid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uuid is required")
	}
	endpoint := conn.apiBase() + "/firewall/alias/toggleItem/" + url.PathEscape(uuid)
	if v, ok := o["enabled"]; ok {
		if boolv(v) {
			endpoint += "/1"
		} else {
			endpoint += "/0"
		}
	}
	return p.post(conn, endpoint, nil)
}

func (p *opnsensePlugin) firewallApply(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.post(conn, conn.apiBase()+"/firewall/alias/reconfigure", nil)
}

func (p *opnsensePlugin) interfaces(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.get(conn, conn.apiBase()+"/interfaces/overview/interfacesInfo", nil)
}

func (p *opnsensePlugin) dhcpLeases(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.get(conn, conn.apiBase()+"/dhcpv4/leases/searchLease", nil)
}

func (p *opnsensePlugin) gatewayStatus(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.get(conn, conn.apiBase()+"/routes/gateway/status", nil)
}

func (p *opnsensePlugin) unboundSettings(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.get(conn, conn.apiBase()+"/unbound/settings/get", nil)
}

func (p *opnsensePlugin) systemReboot(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.post(conn, conn.apiBase()+"/core/system/reboot", nil)
}

func (p *opnsensePlugin) systemStatus(conn opnsenseConn) (plugin.InvokeResult, error) {
	return p.get(conn, conn.apiBase()+"/core/system/status", nil)
}

func (p *opnsensePlugin) api(conn opnsenseConn, o map[string]any) (plugin.InvokeResult, error) {
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
	return decodeOutputs(status, body)
}

// --- shared GET/POST helpers ---

// get performs a GET request and returns the standard status_code +
// result/items outputs (see decodeOutputs).
func (p *opnsensePlugin) get(conn opnsenseConn, endpoint string, query url.Values) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, endpoint, query, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return decodeOutputs(status, body)
}

// post performs a POST request (with an optional JSON body) and returns the
// standard status_code + result/items outputs (see decodeOutputs).
func (p *opnsensePlugin) post(conn opnsenseConn, endpoint string, body any) (plugin.InvokeResult, error) {
	status, respBody, err := p.do(conn, http.MethodPost, endpoint, nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return decodeOutputs(status, respBody)
}

// decodeOutputs decodes a JSON response body into the connector's uniform
// output shape: status_code always; items when the decoded value is itself
// a list, or (many OPNsense "search" endpoints) an object wrapping its rows
// under a "rows" key; result holding the full decoded object/value whenever
// it is present, alongside items when both are available (e.g. rows plus
// rowCount/total metadata).
func decodeOutputs(status int, body []byte) (plugin.InvokeResult, error) {
	decoded, err := decodeJSON(body)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	out := map[string]any{"status_code": status}
	switch v := decoded.(type) {
	case []any:
		out["items"] = v
	case map[string]any:
		if rows, ok := v["rows"].([]any); ok {
			out["items"] = rows
		}
		out["result"] = v
	default:
		if decoded != nil {
			out["result"] = decoded
		}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// --- HTTP plumbing ---

// do performs one HTTP request against the OPNsense API, attaching HTTP
// Basic auth (api_key/api_secret), and returns the status code and raw
// response body. A non-2xx status is translated into a CodeInternalError
// carrying the status and body — the caller never has to check status codes
// itself.
func (p *opnsensePlugin) do(conn opnsenseConn, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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
	req.SetBasicAuth(conn.apiKey, conn.apiSecret)
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
func (p *opnsensePlugin) clientFor(conn opnsenseConn) *http.Client {
	client := &http.Client{Timeout: 30 * time.Second}
	if conn.insecureSkipVerify {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- explicit, documented opt-out for self-signed LAN instances
		}
	}
	return client
}

// decodeJSON decodes a JSON response body into a generic value. An empty
// body decodes to nil rather than an error (e.g. a 202 with no body).
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

func main() {
	if err := plugin.Serve(newOpnsensePlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-opnsense:", err)
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
