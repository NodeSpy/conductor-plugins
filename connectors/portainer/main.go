// Command conductor-portainer is a verb-only conductor connector (#59) for
// Portainer (container management): environments (endpoints), stacks, and
// Docker containers/images proxied through Portainer's docker-proxy, over
// the Portainer CE REST API. Built ONLY against the public SDK (pkg/plugin)
// — no other dependency.
//
// Connection:
//
//	base_url:             "https://portainer.example.com"  # required; Portainer server root (no trailing /api)
//	api_key:               "<api key>"                      # required; sent as X-API-Key: <api_key>
//	insecure_skip_verify:  false                             # optional; skip TLS verification for self-signed certs
//
// Every request goes to base_url + "/api" + <endpoint>, with the API key as
// the X-API-Key header. A non-2xx response is returned as a
// CodeInternalError carrying the status code and response body — nothing is
// swallowed.
//
// insecure_skip_verify exists because self-signed certificates are the norm
// on a home-lab Portainer box; setting it disables TLS certificate
// verification for that connector instance, which means the connection is no
// longer protected against a man-in-the-middle. Prefer importing the box's
// real CA certificate (or its Let's Encrypt cert) instead when possible.
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

type portainerPlugin struct {
	client         *http.Client // default: TLS verified
	insecureClient *http.Client // insecure_skip_verify: true
}

func newPortainerPlugin() *portainerPlugin {
	return &portainerPlugin{
		client: &http.Client{Timeout: 30 * time.Second},
		insecureClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // opt-in per connection, for self-signed home-lab certs
		},
	}
}

// httpClient picks the TLS-verified or TLS-skipping client for one
// connection, so InsecureSkipVerify is scoped to the connector instance that
// asked for it rather than to the whole process.
func (p *portainerPlugin) httpClient(conn portainerConn) *http.Client {
	if conn.insecureSkipVerify {
		return p.insecureClient
	}
	return p.client
}

func (p *portainerPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "portainer",
		Desc: "Portainer: environments (endpoints), stacks, and Docker containers/images proxied through Portainer's docker-proxy, over the Portainer CE REST API, plus a raw `api` escape hatch. Self-hosted; declares no egress (narrow with network: per instance).",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "Portainer server root, e.g. https://portainer.example.com (no trailing /api)"},
			"api_key":              {Type: "string", Required: true, Desc: "Portainer API key, sent as X-API-Key: <api_key>"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification (default false) — common with self-signed certs, but disables protection against a man-in-the-middle"},
		},
		Verbs: portainerVerbs(),
		// Portainer is self-hosted: there is no fixed public host to declare.
		// The operator narrows egress to their own instance with
		// `network: ["portainer.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func portainerVerbs() []plugin.Verb {
	return []plugin.Verb{
		{
			Name: "status", Desc: "Portainer server status/version",
			Usage:   "GET /api/status",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "endpoints", Desc: "list environments (endpoints)",
			Usage:   "GET /api/endpoints",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "endpoint_get", Desc: "get one environment's details",
			Usage: "GET /api/endpoints/{id}",
			Options: plugin.Schema{
				"endpoint_id": {Type: "string", Required: true, Scope: "endpoint"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "stacks", Desc: "list stacks",
			Usage:   "GET /api/stacks",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "stack_get", Desc: "get one stack's details",
			Usage: "GET /api/stacks/{id}",
			Options: plugin.Schema{
				"stack_id": {Type: "string", Required: true, Scope: "stack"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "stack_start", Desc: "start a stopped stack",
			Usage: "POST /api/stacks/{id}/start",
			Options: plugin.Schema{
				"stack_id": {Type: "string", Required: true, Scope: "stack"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "stack_stop", Desc: "stop a running stack",
			Usage: "POST /api/stacks/{id}/stop",
			Options: plugin.Schema{
				"stack_id": {Type: "string", Required: true, Scope: "stack"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "stack_delete", Desc: "delete a stack",
			Usage: "DELETE /api/stacks/{id}?endpointId=",
			Options: plugin.Schema{
				"stack_id":    {Type: "string", Required: true, Scope: "stack"},
				"endpoint_id": {Type: "string", Scope: "endpoint", Desc: "the stack's environment id (required by Portainer for most stack types)"},
			},
			Outputs: plugin.Schema{"status_code": {Type: "integer"}},
		},
		{
			Name: "containers", Desc: "list Docker containers on an environment",
			Usage: "GET /api/endpoints/{endpoint_id}/docker/containers/json?all=",
			Options: plugin.Schema{
				"endpoint_id": {Type: "string", Required: true, Scope: "endpoint"},
				"all":         {Type: "boolean", Desc: "include stopped containers (default false: running only)"},
			},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "container_action", Desc: "start/stop/restart/kill/pause/unpause a Docker container",
			Usage: "POST /api/endpoints/{endpoint_id}/docker/containers/{container_id}/{action}",
			Options: plugin.Schema{
				"endpoint_id":  {Type: "string", Required: true, Scope: "endpoint"},
				"container_id": {Type: "string", Required: true, Scope: "container"},
				"action":       {Type: "string", Required: true, Enum: []string{"start", "stop", "restart", "kill", "pause", "unpause"}},
			},
			Outputs: plugin.Schema{"status_code": {Type: "integer"}},
		},
		{
			Name: "container_logs", Desc: "fetch a Docker container's logs",
			Usage: "GET /api/endpoints/{endpoint_id}/docker/containers/{container_id}/logs?stdout=1&tail=",
			Options: plugin.Schema{
				"endpoint_id":  {Type: "string", Required: true, Scope: "endpoint"},
				"container_id": {Type: "string", Required: true, Scope: "container"},
				"stdout":       {Type: "boolean", Desc: "include stdout (default true)"},
				"stderr":       {Type: "boolean", Desc: "include stderr"},
				"tail":         {Type: "string", Desc: "number of lines from the end, or \"all\" (default all)"},
			},
			Outputs: plugin.Schema{"logs": {Type: "string"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "images", Desc: "list Docker images on an environment",
			Usage: "GET /api/endpoints/{endpoint_id}/docker/images/json",
			Options: plugin.Schema{
				"endpoint_id": {Type: "string", Required: true, Scope: "endpoint"},
			},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "api", Desc: "raw escape hatch: any Portainer API endpoint",
			Usage: "method + path under /api, for anything without a first-class verb",
			Options: plugin.Schema{
				"method": {Type: "string", Desc: "HTTP method (default GET)"},
				"path":   {Type: "string", Required: true, Desc: "path under /api, e.g. /endpoints/1/docker/containers/json"},
				"query":  {Type: "map", Desc: "query string parameters"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
	}
}

func (p *portainerPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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
	case "endpoints":
		return p.endpoints(conn)
	case "endpoint_get":
		return p.endpointGet(conn, o)
	case "stacks":
		return p.stacks(conn)
	case "stack_get":
		return p.stackGet(conn, o)
	case "stack_start":
		return p.stackStart(conn, o)
	case "stack_stop":
		return p.stackStop(conn, o)
	case "stack_delete":
		return p.stackDelete(conn, o)
	case "containers":
		return p.containers(conn, o)
	case "container_action":
		return p.containerAction(conn, o)
	case "container_logs":
		return p.containerLogs(conn, o)
	case "images":
		return p.images(conn, o)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type portainerConn struct {
	baseURL            string
	apiKey             string
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (portainerConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return portainerConn{}, fmt.Errorf("base_url is required")
	}
	apiKey := str(m["api_key"])
	if apiKey == "" {
		return portainerConn{}, fmt.Errorf("api_key is required")
	}
	return portainerConn{
		baseURL:            strings.TrimRight(base, "/"),
		apiKey:             apiKey,
		insecureSkipVerify: boolv(m["insecure_skip_verify"]),
	}, nil
}

// apiBase is conn.base_url + "/api" — every first-class verb's endpoint is
// relative to this. Overridable simply by pointing base_url at a
// httptest.Server in tests.
func (c portainerConn) apiBase() string { return c.baseURL + "/api" }

// --- verb implementations ---

func (p *portainerPlugin) status(conn portainerConn) (plugin.InvokeResult, error) {
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

func (p *portainerPlugin) endpoints(conn portainerConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/endpoints", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *portainerPlugin) endpointGet(conn portainerConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["endpoint_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "endpoint_id is required")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/endpoints/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *portainerPlugin) stacks(conn portainerConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/stacks", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *portainerPlugin) stackGet(conn portainerConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["stack_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "stack_id is required")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/stacks/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *portainerPlugin) stackStart(conn portainerConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["stack_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "stack_id is required")
	}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/stacks/"+url.PathEscape(id)+"/start", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *portainerPlugin) stackStop(conn portainerConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["stack_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "stack_id is required")
	}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/stacks/"+url.PathEscape(id)+"/stop", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *portainerPlugin) stackDelete(conn portainerConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["stack_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "stack_id is required")
	}
	q := url.Values{}
	if v := str(o["endpoint_id"]); v != "" {
		q.Set("endpointId", v)
	}
	status, body, err := p.do(conn, http.MethodDelete, conn.apiBase()+"/stacks/"+url.PathEscape(id), q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *portainerPlugin) containers(conn portainerConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["endpoint_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "endpoint_id is required")
	}
	q := url.Values{}
	if boolv(o["all"]) {
		q.Set("all", "1")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/endpoints/"+url.PathEscape(id)+"/docker/containers/json", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *portainerPlugin) containerAction(conn portainerConn, o map[string]any) (plugin.InvokeResult, error) {
	endpointID := str(o["endpoint_id"])
	if endpointID == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "endpoint_id is required")
	}
	containerID := str(o["container_id"])
	if containerID == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "container_id is required")
	}
	action := str(o["action"])
	switch action {
	case "start", "stop", "restart", "kill", "pause", "unpause":
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "action must be one of start|stop|restart|kill|pause|unpause")
	}
	endpoint := conn.apiBase() + "/endpoints/" + url.PathEscape(endpointID) +
		"/docker/containers/" + url.PathEscape(containerID) + "/" + action
	status, body, err := p.do(conn, http.MethodPost, endpoint, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// containerLogs fetches raw container logs. The docker-proxy log endpoint
// returns plain text (or, when the container was created without a TTY, an
// 8-byte-framed multiplexed stream) rather than JSON, so the body is
// returned verbatim as a string rather than JSON-decoded.
func (p *portainerPlugin) containerLogs(conn portainerConn, o map[string]any) (plugin.InvokeResult, error) {
	endpointID := str(o["endpoint_id"])
	if endpointID == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "endpoint_id is required")
	}
	containerID := str(o["container_id"])
	if containerID == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "container_id is required")
	}
	q := url.Values{}
	if _, ok := o["stdout"]; ok {
		if boolv(o["stdout"]) {
			q.Set("stdout", "1")
		}
	} else {
		q.Set("stdout", "1")
	}
	if boolv(o["stderr"]) {
		q.Set("stderr", "1")
	}
	if v := str(o["tail"]); v != "" {
		q.Set("tail", v)
	}
	endpoint := conn.apiBase() + "/endpoints/" + url.PathEscape(endpointID) +
		"/docker/containers/" + url.PathEscape(containerID) + "/logs"
	status, body, err := p.do(conn, http.MethodGet, endpoint, q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"logs": string(body), "status_code": status}}, nil
}

func (p *portainerPlugin) images(conn portainerConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["endpoint_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "endpoint_id is required")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/endpoints/"+url.PathEscape(id)+"/docker/images/json", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *portainerPlugin) api(conn portainerConn, o map[string]any) (plugin.InvokeResult, error) {
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

// do performs one HTTP request against the Portainer API, attaching the
// X-API-Key header, and returns the status code and raw response body. A
// non-2xx status is translated into a CodeInternalError carrying the status
// and body — the caller never has to check status codes itself.
func (p *portainerPlugin) do(conn portainerConn, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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
	resp, err := p.httpClient(conn).Do(req)
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

// hoist pulls a list out of a decoded JSON value: if it is already a list,
// return it as-is; if it is an object, return the first of the given keys
// that holds a list. Otherwise, an empty list — never nil, so callers get a
// consistent [] rather than null on the wire. Every Portainer/Docker list
// endpoint used here returns a bare JSON array, so hoist() with no keys is
// the common case.
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
	if err := plugin.Serve(newPortainerPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-portainer:", err)
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
