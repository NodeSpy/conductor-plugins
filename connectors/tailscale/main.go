// Command conductor-tailscale is a verb-only conductor connector for
// Tailscale, the mesh VPN. It drives the Tailscale API
// (https://api.tailscale.com/api/v2) over net/http: devices, auth keys, the
// tailnet ACL, DNS settings, and a raw `api` escape hatch for anything a
// first-class verb does not cover. Built ONLY against the public SDK
// (pkg/plugin) — no other dependency.
//
// AUTH SHOWCASE — this connector supports BOTH of conductor's credential
// paths at once, and prefers the managed one:
//
//  1. Conductor's MANAGED OAuth2 (pkg/plugin v0.15.0+), client_credentials
//     grant: this plugin does NOT perform any token exchange. It declares
//     Tailscale's OAuth2 token endpoint in Describe().Auth, the operator
//     supplies an OAuth client's id/secret in the connector's daemon-side
//     `auth:` block, and `conductor connector auth tailscale` (no browser
//     needed — client_credentials is machine-to-machine) has the daemon
//     mint and rotate a bearer token, injected into every InvokeRequest under
//     plugin.AccessTokenKey and read here with plugin.AccessToken.
//  2. A plain API-key fallback: if no managed token is present, the
//     connection's `api_key` field (a Tailscale API key / access token
//     minted in the admin console) is sent as the bearer credential instead.
//
// If neither is configured, every verb fails fast with a CodeInvalidParams
// error rather than sending an unauthenticated request. Because conductor —
// not this plugin — talks to Tailscale's OAuth token endpoint, the plugin's
// only egress is api.tailscale.com.
//
// Connection:
//
//	tailnet:  "example.com"    # required; the tailnet name, or "-" for the default tailnet
//	api_key:  "tskey-api-..."  # optional; fallback bearer credential when no managed auth: is configured
//	api_base: "https://..."    # optional; overrides https://api.tailscale.com (tests)
//
// Every request sends `Authorization: Bearer <token>` where <token> is the
// managed OAuth2 token if present, else api_key. A non-2xx response becomes a
// CodeInternalError carrying the status and body — nothing is swallowed.
// Collection verbs (devices, keys) hoist their envelope's list into `items`;
// single-resource verbs return `result`.
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
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// defaultAPIBase is Tailscale's production API host. api_base overrides it
// for tests.
const defaultAPIBase = "https://api.tailscale.com"

// apiPath is the Tailscale API's fixed path prefix.
const apiPath = "/api/v2"

type tailscalePlugin struct {
	client *http.Client
}

func newTailscalePlugin() *tailscalePlugin {
	return &tailscalePlugin{client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *tailscalePlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "tailscale",
		Desc: "Tailscale: devices, auth keys, tailnet ACL, and DNS settings over the Tailscale API, plus a raw `api` escape hatch. Authenticates via conductor's managed OAuth2 (client_credentials) — run `conductor connector auth tailscale` after configuring an `auth:` block — or, as a fallback, a plain api_key connection field.",
		Connection: plugin.Schema{
			"tailnet":  {Type: "string", Required: true, Desc: "tailnet name, e.g. example.com, or \"-\" for the default tailnet", Scope: "tailnet"},
			"api_key":  {Type: "string", Desc: "Tailscale API key, used as a bearer credential fallback when no managed auth: token is configured"},
			"api_base": {Type: "string", Desc: "override https://api.tailscale.com (tests only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "devices", Desc: "list devices in the tailnet",
				Usage: "GET /api/v2/tailnet/{tailnet}/devices",
				Options: plugin.Schema{
					"fields": {Type: "string", Desc: "pass \"all\" to include all device fields"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "device_get", Desc: "get one device",
				Usage: "GET /api/v2/device/{id}",
				Options: plugin.Schema{
					"device_id": {Type: "string", Required: true, Scope: "device"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "device_delete", Desc: "remove a device from the tailnet",
				Usage: "DELETE /api/v2/device/{id}",
				Options: plugin.Schema{
					"device_id": {Type: "string", Required: true, Scope: "device"},
				},
				Outputs: plugin.Schema{"status_code": {Type: "integer"}},
			},
			{
				Name: "device_authorize", Desc: "authorize (or deauthorize) a device",
				Usage: "POST /api/v2/device/{id}/authorized",
				Options: plugin.Schema{
					"device_id":  {Type: "string", Required: true, Scope: "device"},
					"authorized": {Type: "boolean", Required: true},
				},
				Outputs: plugin.Schema{"status_code": {Type: "integer"}},
			},
			{
				Name: "device_set_tags", Desc: "set a device's ACL tags",
				Usage: "POST /api/v2/device/{id}/tags",
				Options: plugin.Schema{
					"device_id": {Type: "string", Required: true, Scope: "device"},
					"tags":      {Type: "list", Required: true, Desc: "e.g. [\"tag:server\"]"},
				},
				Outputs: plugin.Schema{"status_code": {Type: "integer"}},
			},
			{
				Name: "device_routes", Desc: "get a device's advertised/enabled subnet routes",
				Usage: "GET /api/v2/device/{id}/routes",
				Options: plugin.Schema{
					"device_id": {Type: "string", Required: true, Scope: "device"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "device_set_routes", Desc: "set a device's enabled subnet routes",
				Usage: "POST /api/v2/device/{id}/routes",
				Options: plugin.Schema{
					"device_id": {Type: "string", Required: true, Scope: "device"},
					"routes":    {Type: "list", Required: true, Desc: "e.g. [\"10.0.0.0/24\"]"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "keys", Desc: "list the tailnet's auth keys",
				Usage:   "GET /api/v2/tailnet/{tailnet}/keys",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "key_get", Desc: "get one auth key",
				Usage: "GET /api/v2/tailnet/{tailnet}/keys/{id}",
				Options: plugin.Schema{
					"key_id": {Type: "string", Required: true, Scope: "key"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "key_create", Desc: "create a new auth key",
				Usage: "POST /api/v2/tailnet/{tailnet}/keys",
				Options: plugin.Schema{
					"capabilities":   {Type: "any", Required: true, Desc: "the key's `capabilities` object, per the Tailscale API"},
					"expiry_seconds": {Type: "integer", Desc: "key lifetime in seconds"},
					"description":    {Type: "string"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "key_delete", Desc: "delete an auth key",
				Usage: "DELETE /api/v2/tailnet/{tailnet}/keys/{id}",
				Options: plugin.Schema{
					"key_id": {Type: "string", Required: true, Scope: "key"},
				},
				Outputs: plugin.Schema{"status_code": {Type: "integer"}},
			},
			{
				Name: "acl_get", Desc: "get the tailnet's ACL",
				Usage:   "GET /api/v2/tailnet/{tailnet}/acl",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "acl_set", Desc: "replace the tailnet's ACL",
				Usage: "POST /api/v2/tailnet/{tailnet}/acl",
				Options: plugin.Schema{
					"acl": {Type: "any", Required: true, Desc: "the new ACL document (HuJSON/JSON)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "dns_nameservers", Desc: "get the tailnet's DNS nameservers",
				Usage:   "GET /api/v2/tailnet/{tailnet}/dns/nameservers",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "dns_set_nameservers", Desc: "set the tailnet's DNS nameservers",
				Usage: "POST /api/v2/tailnet/{tailnet}/dns/nameservers",
				Options: plugin.Schema{
					"dns": {Type: "list", Required: true, Desc: "list of nameserver IPs"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "dns_preferences", Desc: "get the tailnet's DNS preferences (MagicDNS)",
				Usage:   "GET /api/v2/tailnet/{tailnet}/dns/preferences",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any Tailscale API endpoint (enables writes)",
				Usage: "method + path under /api/v2, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under /api/v2, e.g. /tailnet/example.com/devices"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// Conductor performs the OAuth2 client_credentials exchange with
		// api.tailscale.com's token endpoint on this plugin's behalf; the
		// plugin itself only ever calls the Tailscale API host.
		Capabilities: plugin.Capabilities{Egress: []string{"api.tailscale.com:443"}},
		Auth: &plugin.AuthSpec{
			Grants:   []string{"client_credentials"},
			TokenURL: "https://api.tailscale.com/api/v2/oauth/token",
			Scopes:   []string{"all:read"},
		},
	}
}

func (p *tailscalePlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, token, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "devices":
		return p.devices(conn, token, o)
	case "device_get":
		return p.deviceGet(conn, token, o)
	case "device_delete":
		return p.deviceDelete(conn, token, o)
	case "device_authorize":
		return p.deviceAuthorize(conn, token, o)
	case "device_set_tags":
		return p.deviceSetTags(conn, token, o)
	case "device_routes":
		return p.deviceRoutes(conn, token, o)
	case "device_set_routes":
		return p.deviceSetRoutes(conn, token, o)
	case "keys":
		return p.keys(conn, token)
	case "key_get":
		return p.keyGet(conn, token, o)
	case "key_create":
		return p.keyCreate(conn, token, o)
	case "key_delete":
		return p.keyDelete(conn, token, o)
	case "acl_get":
		return p.aclGet(conn, token)
	case "acl_set":
		return p.aclSet(conn, token, o)
	case "dns_nameservers":
		return p.dnsNameservers(conn, token)
	case "dns_set_nameservers":
		return p.dnsSetNameservers(conn, token, o)
	case "dns_preferences":
		return p.dnsPreferences(conn, token)
	case "api":
		return p.api(conn, token, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type tailscaleConn struct {
	apiBase string
	tailnet string
}

// parseConn reads the connection map, resolving the bearer credential in
// priority order: conductor's managed OAuth2 token first (plugin.AccessToken),
// then the plain api_key connection field. If neither is present, no request
// is sent — the operator is pointed at how to configure either path.
func parseConn(m map[string]any) (tailscaleConn, string, error) {
	tailnet := str(m["tailnet"])
	if tailnet == "" {
		return tailscaleConn{}, "", fmt.Errorf("tailnet is required")
	}
	token := plugin.AccessToken(m)
	if token == "" {
		token = str(m["api_key"])
	}
	if token == "" {
		return tailscaleConn{}, "", fmt.Errorf("no credentials — configure an auth: block (OAuth client) and run `conductor connector auth tailscale`, or set api_key")
	}
	base := strOr(str(m["api_base"]), defaultAPIBase)
	base = strings.TrimRight(base, "/")
	return tailscaleConn{apiBase: base, tailnet: tailnet}, token, nil
}

// --- verb implementations ---

func (p *tailscalePlugin) devices(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["fields"]); v != "" {
		q.Set("fields", v)
	}
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+apiPath+"/tailnet/"+url.PathEscape(conn.tailnet)+"/devices", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "devices")
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *tailscalePlugin) deviceGet(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["device_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "device_id is required")
	}
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+apiPath+"/device/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *tailscalePlugin) deviceDelete(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["device_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "device_id is required")
	}
	status, _, err := p.do(token, http.MethodDelete, conn.apiBase+apiPath+"/device/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"status_code": status}}, nil
}

func (p *tailscalePlugin) deviceAuthorize(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["device_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "device_id is required")
	}
	authorized, ok := o["authorized"].(bool)
	if !ok {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "authorized is required")
	}
	status, _, err := p.do(token, http.MethodPost, conn.apiBase+apiPath+"/device/"+url.PathEscape(id)+"/authorized", nil,
		map[string]any{"authorized": authorized})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"status_code": status}}, nil
}

func (p *tailscalePlugin) deviceSetTags(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["device_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "device_id is required")
	}
	tags := strList(o["tags"])
	if len(tags) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "tags is required")
	}
	status, _, err := p.do(token, http.MethodPost, conn.apiBase+apiPath+"/device/"+url.PathEscape(id)+"/tags", nil,
		map[string]any{"tags": tags})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"status_code": status}}, nil
}

func (p *tailscalePlugin) deviceRoutes(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["device_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "device_id is required")
	}
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+apiPath+"/device/"+url.PathEscape(id)+"/routes", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *tailscalePlugin) deviceSetRoutes(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["device_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "device_id is required")
	}
	routes := strList(o["routes"])
	if len(routes) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "routes is required")
	}
	status, body, err := p.do(token, http.MethodPost, conn.apiBase+apiPath+"/device/"+url.PathEscape(id)+"/routes", nil,
		map[string]any{"routes": routes})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *tailscalePlugin) keys(conn tailscaleConn, token string) (plugin.InvokeResult, error) {
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+apiPath+"/tailnet/"+url.PathEscape(conn.tailnet)+"/keys", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "keys")
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *tailscalePlugin) keyGet(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["key_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "key_id is required")
	}
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+apiPath+"/tailnet/"+url.PathEscape(conn.tailnet)+"/keys/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *tailscalePlugin) keyCreate(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	caps, ok := o["capabilities"]
	if !ok {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "capabilities is required")
	}
	payload := map[string]any{"capabilities": caps}
	if v := intStr(o["expiry_seconds"]); v != "" {
		payload["expirySeconds"] = o["expiry_seconds"]
	}
	if v := str(o["description"]); v != "" {
		payload["description"] = v
	}
	status, body, err := p.do(token, http.MethodPost, conn.apiBase+apiPath+"/tailnet/"+url.PathEscape(conn.tailnet)+"/keys", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *tailscalePlugin) keyDelete(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["key_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "key_id is required")
	}
	status, _, err := p.do(token, http.MethodDelete, conn.apiBase+apiPath+"/tailnet/"+url.PathEscape(conn.tailnet)+"/keys/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"status_code": status}}, nil
}

func (p *tailscalePlugin) aclGet(conn tailscaleConn, token string) (plugin.InvokeResult, error) {
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+apiPath+"/tailnet/"+url.PathEscape(conn.tailnet)+"/acl", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *tailscalePlugin) aclSet(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	acl, ok := o["acl"]
	if !ok {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "acl is required")
	}
	status, body, err := p.do(token, http.MethodPost, conn.apiBase+apiPath+"/tailnet/"+url.PathEscape(conn.tailnet)+"/acl", nil, acl)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *tailscalePlugin) dnsNameservers(conn tailscaleConn, token string) (plugin.InvokeResult, error) {
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+apiPath+"/tailnet/"+url.PathEscape(conn.tailnet)+"/dns/nameservers", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *tailscalePlugin) dnsSetNameservers(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	dns := strList(o["dns"])
	if len(dns) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "dns is required")
	}
	status, body, err := p.do(token, http.MethodPost, conn.apiBase+apiPath+"/tailnet/"+url.PathEscape(conn.tailnet)+"/dns/nameservers", nil,
		map[string]any{"dns": dns})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *tailscalePlugin) dnsPreferences(conn tailscaleConn, token string) (plugin.InvokeResult, error) {
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+apiPath+"/tailnet/"+url.PathEscape(conn.tailnet)+"/dns/preferences", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *tailscalePlugin) api(conn tailscaleConn, token string, o map[string]any) (plugin.InvokeResult, error) {
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
	status, body, err := p.do(token, method, conn.apiBase+apiPath+path, q, o["body"])
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

// do performs one HTTP request against the Tailscale API, attaching the
// Bearer token, and returns the status code and raw response body. A
// non-2xx status is translated into a CodeInternalError carrying the status
// and body — callers never need to check status codes themselves.
func (p *tailscalePlugin) do(token, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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
// body decodes to nil rather than an error (e.g. a 200/204 with no body).
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

// hoist pulls the list out of a decoded Tailscale API envelope: a map with
// the given key holding a list (e.g. {"devices": [...]}). A bare list is
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
	if err := plugin.Serve(newTailscalePlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-tailscale:", err)
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
