// Command conductor-pfsense is a verb-only conductor connector for pfSense,
// a self-hosted firewall/router. It drives the pfSense REST API v2 (the
// jaredhendrickson13/pfsense-api package, now maintained at
// github.com/pfrest/pfSense-pkg-RESTAPI) over net/http: firewall rules,
// firewall aliases, firewall-change apply, interface status, service
// status/control, DHCP leases, system status, gateway status, and a raw
// `api` escape hatch for anything a first-class verb does not cover. Built
// ONLY against the public SDK (pkg/plugin) — no other dependency.
//
// pfSense does NOT ship this API by default: the pfSense REST API v2
// package must be installed on the target firewall (System > Package
// Manager > Available Packages > RESTAPI, or the pfSense-pkg-RESTAPI
// project) before any of these verbs will work.
//
// This connector has no source: like the REST API's other endpoints, it is
// request/response only, with no webhook or event-stream mechanism to
// listen on. It has no Events and does not implement SourceHandler.
//
// Connection:
//
//	base_url:             "https://pfsense.example.com"  # required; no trailing /api/v2
//	api_key:               "<api key>"                   # required; sent as the X-API-Key header
//	insecure_skip_verify:  false                          # optional; see risk note below
//
// Every request goes to base_url + "/api/v2" + <endpoint>, authenticated
// with the X-API-Key header. The REST API wraps every response in an
// envelope: {"code": <int>, "status": <string>, "response_id": <string>,
// "message": <string>, "data": <...>}. This connector hoists the envelope's
// "data" field into "items" (when it is a list) or "result" (when it is an
// object), so callers work with the payload directly rather than unwrapping
// the envelope themselves. A non-2xx response is returned as a
// CodeInternalError carrying the status code and the envelope's "message" —
// nothing is swallowed.
//
// insecure_skip_verify disables TLS certificate verification. pfSense
// instances commonly run behind a self-signed certificate on a LAN, so this
// exists as an explicit, greppable opt-out — but it also disables all
// protection against a man-in-the-middle on the path to the firewall. Only
// enable it for instances reached over a trusted network, and prefer
// installing a real certificate (e.g. via the built-in ACME/Let's Encrypt
// package) instead when possible.
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

type pfsensePlugin struct{}

func newPfsensePlugin() *pfsensePlugin { return &pfsensePlugin{} }

func (p *pfsensePlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "pfsense",
		Desc: "pfSense: firewall rules, firewall aliases, firewall-change apply, interface status, service status/control, DHCP leases, system status, and gateway status over the pfSense REST API v2. Requires the pfSense REST API v2 package (pfSense-pkg-RESTAPI) to be installed on the target firewall. Self-hosted; declares no egress (narrow with network: per instance). No source — the pfSense REST API is request/response only.",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "pfSense instance root, e.g. https://pfsense.example.com (no trailing /api/v2)"},
			"api_key":              {Type: "string", Required: true, Desc: "pfSense REST API key, sent as the X-API-Key header"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification (default false); self-signed certs are common on LAN deployments, but this disables protection against MITM — only enable for trusted networks"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "firewall_rules", Desc: "list all firewall rules",
				Usage:   "GET /api/v2/firewall/rules",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from the envelope's data"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "rule_get", Desc: "get one firewall rule's details",
				Usage: "GET /api/v2/firewall/rule?id=",
				Options: plugin.Schema{
					"id": {Type: "string", Required: true, Scope: "rule", Desc: "rule ID (array index)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "rule_create", Desc: "create a firewall rule",
				Usage: "POST /api/v2/firewall/rule",
				Options: plugin.Schema{
					"rule": {Type: "map", Required: true, Desc: "rule fields, e.g. {type, interface, ipprotocol, protocol, source, destination, descr}"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "rule_delete", Desc: "delete a firewall rule",
				Usage: "DELETE /api/v2/firewall/rule?id=",
				Options: plugin.Schema{
					"id": {Type: "string", Required: true, Scope: "rule", Desc: "rule ID (array index)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "firewall_apply", Desc: "apply pending firewall changes",
				Usage:   "POST /api/v2/firewall/apply",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "aliases", Desc: "list all firewall aliases",
				Usage:   "GET /api/v2/firewall/aliases",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from the envelope's data"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "alias_get", Desc: "get one firewall alias's details",
				Usage: "GET /api/v2/firewall/alias?id=",
				Options: plugin.Schema{
					"id": {Type: "string", Required: true, Scope: "alias", Desc: "alias ID (array index) or name"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "interfaces", Desc: "interface status (link state, addresses, stats)",
				Usage:   "GET /api/v2/status/interfaces",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from the envelope's data"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "services", Desc: "list known services and their running state",
				Usage:   "GET /api/v2/status/services",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from the envelope's data"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "service_control", Desc: "start, stop, or restart a service",
				Usage: "POST /api/v2/status/service",
				Options: plugin.Schema{
					"name":   {Type: "string", Required: true, Scope: "service", Desc: "service name, e.g. unbound, openvpn"},
					"action": {Type: "string", Required: true, Desc: "one of start, stop, restart"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "dhcp_leases", Desc: "list DHCP server leases",
				Usage:   "GET /api/v2/status/dhcp_server/leases",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from the envelope's data"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "system_status", Desc: "system status (versions, temp, load, memory, disk)",
				Usage:   "GET /api/v2/status/system",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "gateways", Desc: "gateway monitoring status",
				Usage:   "GET /api/v2/status/gateways",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from the envelope's data"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any pfSense REST API v2 endpoint",
				Usage: "method + path under /api/v2, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under /api/v2, e.g. /firewall/rules"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// pfSense is self-hosted: there is no fixed public host to declare.
		// The operator narrows egress to their own instance with
		// `network: ["pfsense.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func (p *pfsensePlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "firewall_rules":
		return p.get(conn, conn.apiBase()+"/firewall/rules", nil)
	case "rule_get":
		return p.ruleGet(conn, o)
	case "rule_create":
		return p.ruleCreate(conn, o)
	case "rule_delete":
		return p.ruleDelete(conn, o)
	case "firewall_apply":
		return p.post(conn, conn.apiBase()+"/firewall/apply", nil)
	case "aliases":
		return p.get(conn, conn.apiBase()+"/firewall/aliases", nil)
	case "alias_get":
		return p.aliasGet(conn, o)
	case "interfaces":
		return p.get(conn, conn.apiBase()+"/status/interfaces", nil)
	case "services":
		return p.get(conn, conn.apiBase()+"/status/services", nil)
	case "service_control":
		return p.serviceControl(conn, o)
	case "dhcp_leases":
		return p.get(conn, conn.apiBase()+"/status/dhcp_server/leases", nil)
	case "system_status":
		return p.get(conn, conn.apiBase()+"/status/system", nil)
	case "gateways":
		return p.get(conn, conn.apiBase()+"/status/gateways", nil)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type pfsenseConn struct {
	baseURL            string
	apiKey             string
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (pfsenseConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return pfsenseConn{}, fmt.Errorf("base_url is required")
	}
	key := str(m["api_key"])
	if key == "" {
		return pfsenseConn{}, fmt.Errorf("api_key is required")
	}
	return pfsenseConn{
		baseURL:            strings.TrimRight(base, "/"),
		apiKey:             key,
		insecureSkipVerify: boolv(m["insecure_skip_verify"]),
	}, nil
}

// apiBase is conn.base_url + "/api/v2" — every first-class verb's endpoint
// is relative to this. Overridable simply by pointing base_url at a
// httptest.Server in tests.
func (c pfsenseConn) apiBase() string { return c.baseURL + "/api/v2" }

// --- verb implementations ---

func (p *pfsensePlugin) ruleGet(conn pfsenseConn, o map[string]any) (plugin.InvokeResult, error) {
	id := idStr(o["id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "id is required")
	}
	q := url.Values{"id": []string{id}}
	return p.get(conn, conn.apiBase()+"/firewall/rule", q)
}

func (p *pfsensePlugin) ruleCreate(conn pfsenseConn, o map[string]any) (plugin.InvokeResult, error) {
	rule, ok := o["rule"].(map[string]any)
	if !ok || len(rule) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "rule is required")
	}
	return p.post(conn, conn.apiBase()+"/firewall/rule", rule)
}

func (p *pfsensePlugin) ruleDelete(conn pfsenseConn, o map[string]any) (plugin.InvokeResult, error) {
	id := idStr(o["id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "id is required")
	}
	q := url.Values{"id": []string{id}}
	status, body, err := p.do(conn, http.MethodDelete, conn.apiBase()+"/firewall/rule", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return decodeOutputs(status, body)
}

func (p *pfsensePlugin) aliasGet(conn pfsenseConn, o map[string]any) (plugin.InvokeResult, error) {
	id := idStr(o["id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "id is required")
	}
	q := url.Values{"id": []string{id}}
	return p.get(conn, conn.apiBase()+"/firewall/alias", q)
}

func (p *pfsensePlugin) serviceControl(conn pfsenseConn, o map[string]any) (plugin.InvokeResult, error) {
	name := str(o["name"])
	if name == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "name is required")
	}
	action := str(o["action"])
	if action == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "action is required")
	}
	return p.post(conn, conn.apiBase()+"/status/service", map[string]any{"name": name, "action": action})
}

func (p *pfsensePlugin) api(conn pfsenseConn, o map[string]any) (plugin.InvokeResult, error) {
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
func (p *pfsensePlugin) get(conn pfsenseConn, endpoint string, query url.Values) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, endpoint, query, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return decodeOutputs(status, body)
}

// post performs a POST request (with an optional JSON body) and returns the
// standard status_code + result/items outputs (see decodeOutputs).
func (p *pfsensePlugin) post(conn pfsenseConn, endpoint string, body any) (plugin.InvokeResult, error) {
	status, respBody, err := p.do(conn, http.MethodPost, endpoint, nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return decodeOutputs(status, respBody)
}

// decodeOutputs decodes a JSON response body into the connector's uniform
// output shape. Every pfSense REST API v2 response is wrapped in an
// envelope: {"code", "status", "response_id", "message", "data"}. This
// hoists the envelope's "data" field into "items" when it is a list, or
// "result" when it is an object (or any other scalar). status_code is
// always the HTTP status code (not the envelope's "code", though the two
// normally agree).
func decodeOutputs(status int, body []byte) (plugin.InvokeResult, error) {
	decoded, err := decodeJSON(body)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	out := map[string]any{"status_code": status}
	env, ok := decoded.(map[string]any)
	if !ok {
		if decoded != nil {
			out["result"] = decoded
		}
		return plugin.InvokeResult{Outputs: out}, nil
	}
	switch data := env["data"].(type) {
	case []any:
		out["items"] = data
	case map[string]any:
		out["result"] = data
	default:
		if data != nil {
			out["result"] = data
		}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// --- HTTP plumbing ---

// do performs one HTTP request against the pfSense REST API, attaching the
// X-API-Key header, and returns the status code and raw response body. A
// non-2xx status is translated into a CodeInternalError carrying the status
// and the envelope's "message" field (falling back to the raw body when the
// response isn't a decodable envelope) — the caller never has to check
// status codes itself.
func (p *pfsensePlugin) do(conn pfsenseConn, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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
	req.Header.Set("X-API-Key", conn.apiKey)
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
		msg := strings.TrimSpace(string(respBody))
		if decoded, derr := decodeJSON(respBody); derr == nil {
			if env, ok := decoded.(map[string]any); ok {
				if m, ok := env["message"].(string); ok && m != "" {
					msg = m
				}
			}
		}
		return resp.StatusCode, respBody, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s %s: %d %s", method, endpoint, resp.StatusCode, msg))
	}
	return resp.StatusCode, respBody, nil
}

// clientFor builds an *http.Client for one request. The transport is only
// customized (skip TLS verification) when the connection asks for it, so the
// common case pays no extra cost and gets normal certificate validation.
func (p *pfsensePlugin) clientFor(conn pfsenseConn) *http.Client {
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

func main() {
	if err := plugin.Serve(newPfsensePlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-pfsense:", err)
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

// idStr renders an id-ish option (string or JSON number) as a string ("" if
// absent/unrecognized). pfSense rule/alias IDs are array indices, which
// arrive as JSON numbers when supplied programmatically but are always sent
// on the wire as plain query-string text.
func idStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return ""
}
