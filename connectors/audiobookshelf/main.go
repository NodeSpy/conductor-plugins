// Command conductor-audiobookshelf is a verb-only conductor connector (#59)
// for Audiobookshelf (ABS), a self-hosted audiobook/podcast server. It drives
// the ABS REST API over net/http: libraries, items, search, scanning,
// series/collections, the "me" user profile, playback progress and
// listening sessions, and a raw `api` escape hatch for anything a
// first-class verb does not cover. Built ONLY against the public SDK
// (pkg/plugin) — no other dependency.
//
// Audiobookshelf also has a live-events channel (socket.io, not a plain
// webhook or SSE stream), which this connector deliberately does NOT
// implement as a source: socket.io is a stateful, bidirectional protocol
// with its own handshake/upgrade framing, well outside "parse an HTTP
// request body" territory that the SDK's sourcekit-style webhook listeners
// are built for. If ABS live events are needed, they belong in a dedicated
// plugin (or the bundled daemon) with a real socket.io client, not bolted
// onto this one. This connector is verb-only: it has no Events and does not
// implement SourceHandler.
//
// Connection:
//
//	base_url: "https://abs.example.com"  # required; the ABS server root (no trailing /api)
//	token: "<api token>"                 # required; sent as Authorization: Bearer <token>
//
// Every request goes to base_url + "/api" + <endpoint>, with the token as a
// Bearer credential. A non-2xx response is returned as a CodeInternalError
// carrying the status code and response body — nothing is swallowed.
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
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type audiobookshelfPlugin struct {
	client *http.Client
}

func newAudiobookshelfPlugin() *audiobookshelfPlugin {
	return &audiobookshelfPlugin{client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *audiobookshelfPlugin) Describe() plugin.Decl {
	as := plugin.Schema{"as": {Type: "string"}} // reserved for parity; unused (no identity split in ABS)
	_ = as
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "audiobookshelf",
		Desc: "Audiobookshelf: libraries, items, search, scanning, series/collections, user profile, and playback progress over the ABS REST API. Self-hosted; declares no egress (narrow with network: per instance). No source — ABS live events are socket.io, out of scope for a stateless HTTP plugin.",
		Connection: plugin.Schema{
			"base_url": {Type: "string", Required: true, Desc: "ABS server root, e.g. https://abs.example.com (no trailing /api)"},
			"token":    {Type: "string", Required: true, Desc: "ABS API token, sent as Authorization: Bearer <token>"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "libraries", Desc: "list libraries",
				Usage:   "GET /api/libraries",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "library_get", Desc: "get one library's details",
				Usage: "GET /api/libraries/{id}",
				Options: plugin.Schema{
					"library_id": {Type: "string", Required: true, Scope: "library"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "library_items", Desc: "list items in a library, paginated",
				Usage: "GET /api/libraries/{id}/items",
				Options: plugin.Schema{
					"library_id": {Type: "string", Required: true, Scope: "library"},
					"limit":      {Type: "integer", Desc: "page size"},
					"page":       {Type: "integer", Desc: "page number (0-based)"},
					"sort":       {Type: "string", Desc: "sort field"},
					"filter":     {Type: "string", Desc: "ABS filter expression"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.results"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "get_item", Desc: "get one library item",
				Usage: "GET /api/items/{id}",
				Options: plugin.Schema{
					"item_id":  {Type: "string", Required: true, Scope: "item"},
					"expanded": {Type: "boolean", Desc: "include expanded media/library-file details"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "search", Desc: "search a library",
				Usage: "GET /api/libraries/{id}/search",
				Options: plugin.Schema{
					"library_id": {Type: "string", Required: true, Scope: "library"},
					"q":          {Type: "string", Required: true, Desc: "search query"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "scan", Desc: "trigger a library scan",
				Usage: "POST /api/libraries/{id}/scan",
				Options: plugin.Schema{
					"library_id": {Type: "string", Required: true, Scope: "library"},
					"force":      {Type: "boolean", Desc: "force a full re-scan"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "series", Desc: "list a library's series",
				Usage: "GET /api/libraries/{id}/series",
				Options: plugin.Schema{
					"library_id": {Type: "string", Required: true, Scope: "library"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "collections", Desc: "list a library's collections",
				Usage: "GET /api/libraries/{id}/collections",
				Options: plugin.Schema{
					"library_id": {Type: "string", Required: true, Scope: "library"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "me", Desc: "the authenticated user's profile",
				Usage:   "GET /api/me",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "get_progress", Desc: "get playback/reading progress for an item",
				Usage: "GET /api/me/progress/{id}",
				Options: plugin.Schema{
					"item_id": {Type: "string", Required: true, Scope: "item"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "update_progress", Desc: "update playback/reading progress for an item",
				Usage: "PATCH /api/me/progress/{id}",
				Options: plugin.Schema{
					"item_id":      {Type: "string", Required: true, Scope: "item"},
					"progress":     {Type: "number", Desc: "0..1 fraction complete"},
					"current_time": {Type: "number", Desc: "playback position in seconds"},
					"is_finished":  {Type: "boolean"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "playback_sessions", Desc: "the user's listening sessions",
				Usage:   "GET /api/me/listening-sessions",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "authorize", Desc: "validate the token / refresh identity",
				Usage:   "POST /api/authorize",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any ABS API endpoint",
				Usage: "method + path under /api, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under /api, e.g. /libraries/123/items"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// Audiobookshelf is self-hosted: there is no fixed public host to
		// declare. The operator narrows egress to their own instance with
		// `network: ["abs.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func (p *audiobookshelfPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "libraries":
		return p.libraries(conn)
	case "library_get":
		return p.libraryGet(conn, o)
	case "library_items":
		return p.libraryItems(conn, o)
	case "get_item":
		return p.getItem(conn, o)
	case "search":
		return p.search(conn, o)
	case "scan":
		return p.scan(conn, o)
	case "series":
		return p.series(conn, o)
	case "collections":
		return p.collections(conn, o)
	case "me":
		return p.me(conn)
	case "get_progress":
		return p.getProgress(conn, o)
	case "update_progress":
		return p.updateProgress(conn, o)
	case "playback_sessions":
		return p.playbackSessions(conn)
	case "authorize":
		return p.authorize(conn)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type absConn struct {
	baseURL string
	token   string
}

func parseConn(m map[string]any) (absConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return absConn{}, fmt.Errorf("base_url is required")
	}
	token := str(m["token"])
	if token == "" {
		return absConn{}, fmt.Errorf("token is required")
	}
	return absConn{baseURL: strings.TrimRight(base, "/"), token: token}, nil
}

// apiBase is conn.base_url + "/api" — every first-class verb's endpoint is
// relative to this. Overridable simply by pointing base_url at a
// httptest.Server in tests.
func (c absConn) apiBase() string { return c.baseURL + "/api" }

// --- verb implementations ---

func (p *audiobookshelfPlugin) libraries(conn absConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/libraries", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "libraries")
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) libraryGet(conn absConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["library_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "library_id is required")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/libraries/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) libraryItems(conn absConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["library_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "library_id is required")
	}
	q := url.Values{}
	if v := intStr(o["limit"]); v != "" {
		q.Set("limit", v)
	}
	if v := intStr(o["page"]); v != "" {
		q.Set("page", v)
	}
	if v := str(o["sort"]); v != "" {
		q.Set("sort", v)
	}
	if v := str(o["filter"]); v != "" {
		q.Set("filter", v)
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/libraries/"+url.PathEscape(id)+"/items", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "results")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) getItem(conn absConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["item_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "item_id is required")
	}
	q := url.Values{}
	if boolv(o["expanded"]) {
		q.Set("expanded", "1")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/items/"+url.PathEscape(id), q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) search(conn absConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["library_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "library_id is required")
	}
	q := str(o["q"])
	if q == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "q is required")
	}
	qs := url.Values{"q": []string{q}}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/libraries/"+url.PathEscape(id)+"/search", qs, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) scan(conn absConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["library_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "library_id is required")
	}
	q := url.Values{}
	if boolv(o["force"]) {
		q.Set("force", "1")
	}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/libraries/"+url.PathEscape(id)+"/scan", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *audiobookshelfPlugin) series(conn absConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["library_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "library_id is required")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/libraries/"+url.PathEscape(id)+"/series", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "results", "series")
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) collections(conn absConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["library_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "library_id is required")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/libraries/"+url.PathEscape(id)+"/collections", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "results", "collections")
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) me(conn absConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/me", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) getProgress(conn absConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["item_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "item_id is required")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/me/progress/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) updateProgress(conn absConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["item_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "item_id is required")
	}
	payload := map[string]any{}
	if v, ok := o["progress"]; ok {
		payload["progress"] = v
	}
	if v, ok := o["current_time"]; ok {
		payload["currentTime"] = v
	}
	if v, ok := o["is_finished"]; ok {
		payload["isFinished"] = v
	}
	status, body, err := p.do(conn, http.MethodPatch, conn.apiBase()+"/me/progress/"+url.PathEscape(id), nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *audiobookshelfPlugin) playbackSessions(conn absConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/me/listening-sessions", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) authorize(conn absConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/authorize", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *audiobookshelfPlugin) api(conn absConn, o map[string]any) (plugin.InvokeResult, error) {
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

// do performs one HTTP request against the ABS API, attaching the Bearer
// token, and returns the status code and raw response body. A non-2xx
// status is translated into a CodeInternalError carrying the status and
// body — the caller never has to check status codes itself.
func (p *audiobookshelfPlugin) do(conn absConn, method, endpoint string, query url.Values, body any) (int, []byte, error) {
	full := endpoint
	if query != nil && len(query) > 0 {
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
	req.Header.Set("Authorization", "Bearer "+conn.token)
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
	if err := plugin.Serve(newAudiobookshelfPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-audiobookshelf:", err)
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
