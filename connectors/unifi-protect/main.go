// Command conductor-unifi-protect is a verb-only conductor connector for a
// UniFi Protect NVR/camera system — cameras, PTZ control, snapshots, NVR
// details, viewers, lights, sensors, chimes, and a raw `api` escape hatch.
// Built ONLY against the public SDK (pkg/plugin) — no other dependency.
//
// This targets the MODERN UniFi Protect Integration API (UniFi OS 4.x /
// Protect 5.x), which authenticates with a static API KEY rather than a
// cookie session (unlike the sibling `unifi` Network connector). Every
// request carries the key as an `X-API-KEY` header, and every endpoint is
// relative to:
//
//	{base_url}/proxy/protect/integration/v1
//
// Connection:
//
//	base_url:             "https://192.168.1.1"  # required; the UDM/UniFi OS console root
//	api_key:              "<api key>"            # required; sent as X-API-KEY
//	insecure_skip_verify: false                   # optional, default false; self-signed certs
//
// insecure_skip_verify disables TLS certificate verification for THIS
// connection only (a per-connection tls.Config, never process-wide). UDMs
// commonly present a self-signed certificate on the LAN; only set this for a
// console reached over a trusted network path (e.g. your own LAN/VPN) — it
// removes protection against a man-in-the-middle presenting a forged
// certificate.
//
// Protect's list endpoints (cameras, viewers, lights, sensors, chimes) return
// a bare JSON array, hoisted into `items`. Singular/object endpoints (meta,
// camera_get, nvrs) return `result`. camera_snapshot is the one exception:
// its response is a JPEG image, not JSON, so the raw bytes are base64-encoded
// into `image_base64` alongside `content_type`. Every verb also returns
// `status_code`. A non-2xx response is returned as a CodeInternalError
// carrying the status code and response body — nothing is swallowed.
//
// No source: the Integration API's live smart-detection events (person,
// vehicle, motion, ...) are delivered over a WebSocket subscription, not a
// plain webhook or a pollable REST endpoint. A stdlib-only plugin can't
// cleanly speak that protocol (no net/http WebSocket client in the standard
// library), so this connector is verb-only for v1: it declares no Events and
// does not implement StartSource. A detection-event source is a planned
// follow-up that needs a real WebSocket client.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
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

type protectPlugin struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

func newProtectPlugin() *protectPlugin {
	return &protectPlugin{clients: map[string]*http.Client{}}
}

func (p *protectPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "unifi-protect",
		Desc: "UniFi Protect NVR/cameras: list/inspect cameras, PTZ control, snapshots, NVR details, viewers, lights, sensors, chimes over the Protect Integration API (API-key auth). Self-hosted; declares no egress (narrow with network: per instance). No source — smart-detection events are WebSocket-only, out of scope for v1 (planned follow-up).",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "UniFi OS console root, e.g. https://192.168.1.1 (no trailing path)"},
			"api_key":              {Type: "string", Required: true, Desc: "Protect Integration API key, sent as X-API-KEY"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification for this connection (default false). Only for a self-signed console reached over a trusted network — this removes protection against a forged certificate."},
		},
		Verbs: []plugin.Verb{
			{
				Name: "meta", Desc: "Protect application/API version info",
				Usage:   "GET /meta/info",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "cameras", Desc: "list all cameras",
				Usage:   "GET /cameras",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "camera_get", Desc: "get one camera's details",
				Usage: "GET /cameras/{id}",
				Options: plugin.Schema{
					"camera_id": {Type: "string", Required: true, Scope: "camera"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "camera_snapshot", Desc: "capture a still snapshot from a camera",
				Usage: "GET /cameras/{id}/snapshot",
				Options: plugin.Schema{
					"camera_id":    {Type: "string", Required: true, Scope: "camera"},
					"high_quality": {Type: "boolean", Desc: "request a higher-resolution snapshot (?highQuality=true)"},
				},
				Outputs: plugin.Schema{
					"image_base64": {Type: "string", Desc: "the JPEG snapshot, base64-encoded"},
					"content_type": {Type: "string"},
					"status_code":  {Type: "integer"},
				},
			},
			{
				Name: "camera_ptz", Desc: "control a PTZ camera: move to a preset, or start/stop a patrol",
				Usage: "POST /cameras/{id}/ptz/goto/{slot} | /ptz/patrol/start/{slot} | /ptz/patrol/stop",
				Options: plugin.Schema{
					"camera_id": {Type: "string", Required: true, Scope: "camera"},
					"action":    {Type: "string", Required: true, Enum: []string{"goto", "patrol_start", "patrol_stop"}, Desc: "goto and patrol_start require slot; patrol_stop does not"},
					"slot":      {Type: "integer", Desc: "PTZ preset slot number (required for goto and patrol_start)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "nvrs", Desc: "this console's NVR details (arm mode, doorbell settings, identification)",
				Usage:   "GET /nvrs",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "viewers", Desc: "list all Protect viewers (view-only displays)",
				Usage:   "GET /viewers",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "lights", Desc: "list all Protect lights",
				Usage:   "GET /lights",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "sensors", Desc: "list all Protect sensors",
				Usage:   "GET /sensors",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "chimes", Desc: "list all Protect chimes",
				Usage:   "GET /chimes",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any Protect Integration API endpoint",
				Usage: "method + path under /proxy/protect/integration/v1, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under the integration v1 base, e.g. /viewers/abc123"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// A UniFi Protect console is self-hosted: there is no fixed public host
		// to declare. The operator narrows egress to their own console with
		// `network: ["192.168.1.1:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func (p *protectPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "meta":
		return p.meta(conn)
	case "cameras":
		return p.cameras(conn)
	case "camera_get":
		return p.cameraGet(conn, o)
	case "camera_snapshot":
		return p.cameraSnapshot(conn, o)
	case "camera_ptz":
		return p.cameraPTZ(conn, o)
	case "nvrs":
		return p.nvrs(conn)
	case "viewers":
		return p.viewers(conn)
	case "lights":
		return p.lights(conn)
	case "sensors":
		return p.sensors(conn)
	case "chimes":
		return p.chimes(conn)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type protectConn struct {
	baseURL            string
	apiKey             string
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (protectConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return protectConn{}, fmt.Errorf("base_url is required")
	}
	apiKey := str(m["api_key"])
	if apiKey == "" {
		return protectConn{}, fmt.Errorf("api_key is required")
	}
	return protectConn{
		baseURL:            strings.TrimRight(base, "/"),
		apiKey:             apiKey,
		insecureSkipVerify: boolOr(m["insecure_skip_verify"], false),
	}, nil
}

// apiBase is conn.base_url + the Integration API's fixed v1 prefix — every
// first-class verb's endpoint is relative to this.
func (c protectConn) apiBase() string { return c.baseURL + "/proxy/protect/integration/v1" }

// --- HTTP client (per-connection TLS config, cached) ---

// clientFor returns the http.Client for this connection's base_url +
// insecure_skip_verify pair, building (and caching) one on first use. Like
// the sibling `unifi` connector, insecure_skip_verify is wired into a
// PER-CONNECTION tls.Config, never process-wide.
func (p *protectPlugin) clientFor(conn protectConn) *http.Client {
	key := conn.baseURL + "\x00" + boolStr(conn.insecureSkipVerify)
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[key]; ok {
		return c
	}
	transport := &http.Transport{}
	if conn.insecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- opt-in per connection for self-signed consoles
	}
	c := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	p.clients[key] = c
	return c
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// --- verb implementations ---

func (p *protectPlugin) meta(conn protectConn) (plugin.InvokeResult, error) {
	status, body, _, err := p.do(conn, http.MethodGet, "/meta/info", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *protectPlugin) cameras(conn protectConn) (plugin.InvokeResult, error) {
	return p.getItems(conn, "/cameras")
}

func (p *protectPlugin) cameraGet(conn protectConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["camera_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "camera_id is required")
	}
	status, body, _, err := p.do(conn, http.MethodGet, "/cameras/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

// cameraSnapshot returns a JPEG image, not JSON: the raw response bytes are
// base64-encoded into image_base64, alongside the response's content_type.
func (p *protectPlugin) cameraSnapshot(conn protectConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["camera_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "camera_id is required")
	}
	q := url.Values{}
	if boolv(o["high_quality"]) {
		q.Set("highQuality", "true")
	}
	status, body, contentType, err := p.do(conn, http.MethodGet, "/cameras/"+url.PathEscape(id)+"/snapshot", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{
		"image_base64": base64.StdEncoding.EncodeToString(body),
		"content_type": contentType,
		"status_code":  status,
	}}, nil
}

// cameraPTZ issues one PTZ command. goto and patrol_start move/patrol from a
// saved preset slot; patrol_stop takes no slot.
func (p *protectPlugin) cameraPTZ(conn protectConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["camera_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "camera_id is required")
	}
	action := str(o["action"])
	var path string
	switch action {
	case "goto":
		slot := intStr(o["slot"])
		if slot == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slot is required for action=goto")
		}
		path = "/cameras/" + url.PathEscape(id) + "/ptz/goto/" + slot
	case "patrol_start":
		slot := intStr(o["slot"])
		if slot == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slot is required for action=patrol_start")
		}
		path = "/cameras/" + url.PathEscape(id) + "/ptz/patrol/start/" + slot
	case "patrol_stop":
		path = "/cameras/" + url.PathEscape(id) + "/ptz/patrol/stop"
	case "":
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "action is required (goto, patrol_start, or patrol_stop)")
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown action "+action+" (want goto, patrol_start, or patrol_stop)")
	}

	status, body, _, err := p.do(conn, http.MethodPost, path, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// nvrs returns this console's single NVR object (the Integration API has no
// list endpoint here — one console has exactly one NVR).
func (p *protectPlugin) nvrs(conn protectConn) (plugin.InvokeResult, error) {
	status, body, _, err := p.do(conn, http.MethodGet, "/nvrs", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *protectPlugin) viewers(conn protectConn) (plugin.InvokeResult, error) {
	return p.getItems(conn, "/viewers")
}

func (p *protectPlugin) lights(conn protectConn) (plugin.InvokeResult, error) {
	return p.getItems(conn, "/lights")
}

func (p *protectPlugin) sensors(conn protectConn) (plugin.InvokeResult, error) {
	return p.getItems(conn, "/sensors")
}

func (p *protectPlugin) chimes(conn protectConn) (plugin.InvokeResult, error) {
	return p.getItems(conn, "/chimes")
}

// getItems performs a GET against path, which Protect returns as a bare JSON
// array, and hoists it into items (never nil, even when empty).
func (p *protectPlugin) getItems(conn protectConn, path string) (plugin.InvokeResult, error) {
	status, body, _, err := p.do(conn, http.MethodGet, path, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoistItems(decoded), "status_code": status}}, nil
}

func (p *protectPlugin) api(conn protectConn, o map[string]any) (plugin.InvokeResult, error) {
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
	status, body, _, err := p.do(conn, method, path, q, o["body"])
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

// do performs one HTTP request against the Protect Integration API, attaching
// the X-API-KEY header, and returns the status code, raw response body, and
// response Content-Type. A non-2xx status is translated into a
// CodeInternalError carrying the status and body — the caller never has to
// check status codes itself.
func (p *protectPlugin) do(conn protectConn, method, path string, query url.Values, body any) (int, []byte, string, error) {
	full := conn.apiBase() + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, "", plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return 0, nil, "", plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req.Header.Set("X-API-KEY", conn.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.clientFor(conn).Do(req)
	if err != nil {
		return 0, nil, "", plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	contentType := resp.Header.Get("Content-Type")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, respBody, contentType, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody))))
	}
	return resp.StatusCode, respBody, contentType, nil
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

// hoistItems reads a decoded Protect list response, which is always a bare
// JSON array, into a []any. Anything else (including nil) is an empty
// list — never nil, so callers get a consistent [] rather than null on the
// wire.
func hoistItems(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return []any{}
}

func main() {
	if err := plugin.Serve(newProtectPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-unifi-protect:", err)
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
