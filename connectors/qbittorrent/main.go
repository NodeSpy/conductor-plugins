// Command conductor-qbittorrent is a verb-only conductor connector (#59) that
// drives a self-hosted qBittorrent instance over its WebUI API v2
// (base_url + "/api/v2"). It exposes the torrent lifecycle (add, delete,
// pause, resume, recheck), categories/tags, transfer stats and global speed
// limits as verbs, plus an `api` escape hatch for any endpoint a first-class
// verb does not cover. Built ONLY against the public SDK (pkg/plugin) and the
// standard library — no third-party client.
//
// qBittorrent's WebUI API is cookie-authenticated: a successful
// POST /api/v2/auth/login sets an SID session cookie that every subsequent
// request must send back (the login endpoint itself answers with HTTP 200 and
// a plain-text body — "Ok." or "Fails." — rather than a 401 on bad
// credentials, so the body has to be checked explicitly). This connector logs
// in once per connector instance and caches the SID across Invoke calls,
// transparently re-authenticating if the session has expired (a 403 on a
// later call).
//
// qBittorrent is self-hosted with no fixed public host, so Capabilities.Egress
// is declared empty; the operator's `network:` allowlist on the connector
// instance is the actual scope (see docs/connectors/qbittorrent.md).
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type qbittorrentPlugin struct {
	mu      sync.Mutex
	clients map[string]*qbitClient
}

func (*qbittorrentPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "qbittorrent",
		Desc: "qBittorrent WebUI API v2: torrent lifecycle (add/delete/pause/resume/recheck), categories/tags, transfer stats and global speed limits, as verbs. Cookie-session (SID) auth against a self-hosted instance.",
		Connection: plugin.Schema{
			"base_url": {Type: "string", Required: true, Desc: "qBittorrent WebUI base URL, e.g. http://qbit:8080 (no trailing path)"},
			"username": {Type: "string", Desc: "WebUI username (omit if the host has authentication bypass for local/whitelisted clients)"},
			"password": {Type: "string", Desc: "WebUI password"},
		},
		Verbs: qbitVerbs(),
		// qBittorrent is self-hosted software with no fixed public host: an
		// empty manifest is the strongest thing this plugin can say up front.
		// Scope the actual egress with the connector instance's `network:`
		// allowlist — see docs/connectors/qbittorrent.md.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func resultOutputs() plugin.Schema {
	return plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}}
}

func listOutputs() plugin.Schema {
	return plugin.Schema{"items": {Type: "list"}, "result": {Type: "any"}, "status_code": {Type: "integer"}}
}

func qbitVerbs() []plugin.Verb {
	res := resultOutputs()
	list := listOutputs()
	hashesOpt := plugin.Field{Type: "any", Required: true, Scope: "torrent", Desc: "a hash, a list of hashes, or \"all\""}
	return []plugin.Verb{
		{
			Name: "torrents", Desc: "list torrents",
			Options: plugin.Schema{
				"filter":   {Type: "string", Desc: "all|downloading|seeding|completed|paused|active|inactive|resumed|stalled|stalled_uploading|stalled_downloading|errored"},
				"category": {Type: "string", Desc: "filter to this category"},
				"tag":      {Type: "string", Desc: "filter to this tag"},
				"sort":     {Type: "string", Desc: "sort key, e.g. added_on, name, size"},
				"hashes":   {Type: "any", Scope: "torrent", Desc: "restrict to a hash or list of hashes"},
			},
			Outputs: list,
		},
		{
			Name: "torrent_properties", Desc: "a single torrent's detailed properties",
			Options: plugin.Schema{"hash": {Type: "string", Required: true, Scope: "torrent"}},
			Outputs: res,
		},
		{
			Name: "add", Desc: "add torrents by magnet link or URL",
			Usage: "magnet/URL adds only; uploading a .torrent file's bytes is out of scope for this verb",
			Options: plugin.Schema{
				"urls":     {Type: "any", Required: true, Desc: "a magnet/URL, or a list of them (sent newline-joined)"},
				"category": {Type: "string"},
				"tags":     {Type: "any", Desc: "a tag or list of tags"},
				"paused":   {Type: "boolean", Desc: "add without starting"},
				"savepath": {Type: "string", Desc: "download destination path"},
				"rename":   {Type: "string", Desc: "rename the added torrent (single-URL adds only)"},
			},
			Outputs: res,
		},
		{
			Name: "delete", Desc: "delete torrents",
			Options: plugin.Schema{
				"hashes":       hashesOpt,
				"delete_files": {Type: "boolean", Desc: "also delete the downloaded files"},
			},
			Outputs: res,
		},
		{
			Name: "pause", Desc: "pause torrents",
			Options: plugin.Schema{"hashes": hashesOpt},
			Outputs: res,
		},
		{
			Name: "resume", Desc: "resume torrents",
			Options: plugin.Schema{"hashes": hashesOpt},
			Outputs: res,
		},
		{
			Name: "recheck", Desc: "force-recheck torrents",
			Options: plugin.Schema{"hashes": hashesOpt},
			Outputs: res,
		},
		{
			Name: "set_category", Desc: "set torrents' category",
			Options: plugin.Schema{
				"hashes":   hashesOpt,
				"category": {Type: "string", Required: true},
			},
			Outputs: res,
		},
		{
			Name: "add_tags", Desc: "add tags to torrents",
			Options: plugin.Schema{
				"hashes": hashesOpt,
				"tags":   {Type: "any", Required: true, Desc: "a tag or list of tags"},
			},
			Outputs: res,
		},
		{
			Name: "remove_tags", Desc: "remove tags from torrents",
			Options: plugin.Schema{
				"hashes": hashesOpt,
				"tags":   {Type: "any", Required: true, Desc: "a tag or list of tags"},
			},
			Outputs: res,
		},
		{
			Name: "set_speed_limits", Desc: "set the global download and/or upload speed limit",
			Usage: "at least one of download/upload is required; each provided limit is applied with its own API call",
			Options: plugin.Schema{
				"download": {Type: "integer", Desc: "bytes/sec, 0 = unlimited"},
				"upload":   {Type: "integer", Desc: "bytes/sec, 0 = unlimited"},
			},
			Outputs: res,
		},
		{
			Name:    "transfer_info",
			Desc:    "global transfer statistics and current speed limits",
			Outputs: res,
		},
		{
			Name:    "app_version",
			Desc:    "the qBittorrent application version",
			Outputs: res,
		},
		{
			Name: "api", Desc: "call any qBittorrent WebUI API v2 endpoint not covered by a first-class verb",
			Usage: "escape hatch: method + path (relative to /api/v2), optional params",
			Options: plugin.Schema{
				"method": {Type: "string", Required: true, Enum: []string{"GET", "POST"}},
				"path":   {Type: "string", Required: true, Desc: "path relative to /api/v2, e.g. \"sync/maindata\""},
				"params": {Type: "map", Desc: "GET query parameters, or POST form fields"},
			},
			Outputs: res,
		},
	}
}

// qbitConn is the resolved, comparable connection config for one connector
// instance — comparable so clientFor can detect a credential change and
// rebuild the client (and its cached session) instead of reusing a stale one.
type qbitConn struct {
	base     string // origin, e.g. http://qbit:8080 (no trailing slash)
	username string
	password string
}

func parseConn(m map[string]any) (qbitConn, error) {
	base := strings.TrimRight(str(m["base_url"]), "/")
	if base == "" {
		return qbitConn{}, fmt.Errorf("connection.base_url is required")
	}
	return qbitConn{base: base, username: str(m["username"]), password: str(m["password"])}, nil
}

func (c qbitConn) apiBase() string { return c.base + "/api/v2" }

// clientFor builds (or reuses) the qbitClient for one connector instance, so
// the cached SID session persists across Invoke calls instead of logging in
// on every single verb call.
func (p *qbittorrentPlugin) clientFor(instance string, connMap map[string]any) (*qbitClient, error) {
	conn, err := parseConn(connMap)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = map[string]*qbitClient{}
	}
	if c, ok := p.clients[instance]; ok && c.conn == conn {
		return c, nil
	}
	c := &qbitClient{http: &http.Client{}, conn: conn}
	p.clients[instance] = c
	return c, nil
}

func (p *qbittorrentPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	c, err := p.clientFor(req.Instance, req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	// set_speed_limits fans out to up to two endpoints (setDownloadLimit /
	// setUploadLimit), so it does not fit the one apiCall per verb shape.
	if req.Verb == "set_speed_limits" {
		outputs, err := c.setSpeedLimits(o)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return plugin.InvokeResult{Outputs: outputs}, nil
	}

	call, err := verbCall(req.Verb, o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}
	outputs, err := c.do(call)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

// apiCall is the resolved HTTP request one verb builds, before it is sent.
// query drives a GET; form drives a form-encoded POST body. Never both.
type apiCall struct {
	method string
	path   string
	query  url.Values
	form   url.Values
}

// verbCall builds the HTTP call for one verb. Pure and hermetically
// testable — no request is sent here.
func verbCall(verb string, o map[string]any) (apiCall, error) {
	switch verb {
	case "torrents":
		q := url.Values{}
		setIf(q, "filter", str(o["filter"]))
		setIf(q, "category", str(o["category"]))
		setIf(q, "tag", str(o["tag"]))
		setIf(q, "sort", str(o["sort"]))
		setIf(q, "hashes", joinAny(o["hashes"], "|"))
		return apiCall{method: http.MethodGet, path: "torrents/info", query: q}, nil
	case "torrent_properties":
		hash := str(o["hash"])
		if hash == "" {
			return apiCall{}, fmt.Errorf("hash is required")
		}
		return apiCall{method: http.MethodGet, path: "torrents/properties", query: url.Values{"hash": {hash}}}, nil
	case "add":
		return addCall(o)
	case "delete":
		hashes, err := requireHashes(o)
		if err != nil {
			return apiCall{}, err
		}
		form := url.Values{"hashes": {hashes}}
		form.Set("deleteFiles", strconv.FormatBool(boolv(o["delete_files"])))
		return apiCall{method: http.MethodPost, path: "torrents/delete", form: form}, nil
	case "pause":
		return hashesOnlyCall("torrents/pause", o)
	case "resume":
		return hashesOnlyCall("torrents/resume", o)
	case "recheck":
		return hashesOnlyCall("torrents/recheck", o)
	case "set_category":
		hashes, err := requireHashes(o)
		if err != nil {
			return apiCall{}, err
		}
		category := str(o["category"])
		if category == "" {
			return apiCall{}, fmt.Errorf("category is required")
		}
		return apiCall{method: http.MethodPost, path: "torrents/setCategory", form: url.Values{"hashes": {hashes}, "category": {category}}}, nil
	case "add_tags":
		return tagsCall("torrents/addTags", o)
	case "remove_tags":
		return tagsCall("torrents/removeTags", o)
	case "transfer_info":
		return apiCall{method: http.MethodGet, path: "transfer/info"}, nil
	case "app_version":
		return apiCall{method: http.MethodGet, path: "app/version"}, nil
	case "api":
		return apiVerbCall(o)
	}
	return apiCall{}, fmt.Errorf("unknown verb")
}

func hashesOnlyCall(path string, o map[string]any) (apiCall, error) {
	hashes, err := requireHashes(o)
	if err != nil {
		return apiCall{}, err
	}
	return apiCall{method: http.MethodPost, path: path, form: url.Values{"hashes": {hashes}}}, nil
}

func requireHashes(o map[string]any) (string, error) {
	hashes := joinAny(o["hashes"], "|")
	if hashes == "" {
		return "", fmt.Errorf("hashes is required")
	}
	return hashes, nil
}

func tagsCall(path string, o map[string]any) (apiCall, error) {
	hashes, err := requireHashes(o)
	if err != nil {
		return apiCall{}, err
	}
	tags := joinAny(o["tags"], ",")
	if tags == "" {
		return apiCall{}, fmt.Errorf("tags is required")
	}
	return apiCall{method: http.MethodPost, path: path, form: url.Values{"hashes": {hashes}, "tags": {tags}}}, nil
}

func addCall(o map[string]any) (apiCall, error) {
	urls := joinAny(o["urls"], "\n")
	if urls == "" {
		return apiCall{}, fmt.Errorf("urls is required")
	}
	form := url.Values{"urls": {urls}}
	setIf(form, "category", str(o["category"]))
	setIf(form, "savepath", str(o["savepath"]))
	setIf(form, "rename", str(o["rename"]))
	if tags := joinAny(o["tags"], ","); tags != "" {
		form.Set("tags", tags)
	}
	if v, ok := o["paused"]; ok {
		form.Set("paused", strconv.FormatBool(boolv(v)))
	}
	return apiCall{method: http.MethodPost, path: "torrents/add", form: form}, nil
}

func apiVerbCall(o map[string]any) (apiCall, error) {
	method := strings.ToUpper(str(o["method"]))
	if method == "" {
		return apiCall{}, fmt.Errorf("method is required")
	}
	path := strings.TrimPrefix(str(o["path"]), "/")
	if path == "" {
		return apiCall{}, fmt.Errorf("path is required")
	}
	params, _ := o["params"].(map[string]any)
	switch method {
	case http.MethodGet:
		q := url.Values{}
		for k, v := range params {
			q.Set(k, fmt.Sprintf("%v", v))
		}
		return apiCall{method: method, path: path, query: q}, nil
	case http.MethodPost:
		f := url.Values{}
		for k, v := range params {
			f.Set(k, fmt.Sprintf("%v", v))
		}
		return apiCall{method: method, path: path, form: f}, nil
	default:
		return apiCall{}, fmt.Errorf("method must be GET or POST")
	}
}

// qbitClient is the per-instance HTTP client plus its cached SID session.
type qbitClient struct {
	mu   sync.Mutex
	http *http.Client
	conn qbitConn
	sid  string
}

// setSpeedLimits applies download and/or upload as separate calls, since
// qBittorrent has one endpoint per direction.
func (c *qbitClient) setSpeedLimits(o map[string]any) (map[string]any, error) {
	_, hasDown := o["download"]
	_, hasUp := o["upload"]
	if !hasDown && !hasUp {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, "set_speed_limits: download or upload is required")
	}
	result := map[string]any{}
	status := 0
	if hasDown {
		out, err := c.do(apiCall{method: http.MethodPost, path: "transfer/setDownloadLimit", form: url.Values{"limit": {intStr(o["download"])}}})
		if err != nil {
			return nil, err
		}
		result["download"] = out["result"]
		status, _ = out["status_code"].(int)
	}
	if hasUp {
		out, err := c.do(apiCall{method: http.MethodPost, path: "transfer/setUploadLimit", form: url.Values{"limit": {intStr(o["upload"])}}})
		if err != nil {
			return nil, err
		}
		result["upload"] = out["result"]
		status, _ = out["status_code"].(int)
	}
	return map[string]any{"result": result, "status_code": status}, nil
}

// do sends one HTTP request through the cached session, re-authenticating
// once on a 403 (an expired SID) before giving up. A non-2xx response is
// returned as a plugin.CodeInternalError carrying the status and response
// body; verb callers never see a raw *http.Response.
func (c *qbitClient) do(call apiCall) (map[string]any, error) {
	if err := c.ensureLoggedIn(); err != nil {
		return nil, err
	}
	status, raw, err := c.send(call)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if status == http.StatusForbidden && c.conn.username != "" {
		if err := c.relogin(); err != nil {
			return nil, err
		}
		status, raw, err = c.send(call)
		if err != nil {
			return nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
		}
	}
	if status < 200 || status >= 300 {
		return nil, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("qbittorrent API %d %s: %s", status, http.StatusText(status), strings.TrimSpace(string(raw))))
	}
	return shapeOutputs(raw, status), nil
}

// send issues one request and returns the raw status/body. It never
// interprets the result — do() owns the auth-retry and error-shaping policy.
func (c *qbitClient) send(call apiCall) (int, []byte, error) {
	method := call.method
	if method == "" {
		method = http.MethodGet
	}
	u := strings.TrimRight(c.conn.apiBase(), "/") + "/" + call.path
	if len(call.query) > 0 {
		u += "?" + call.query.Encode()
	}
	var body io.Reader
	if call.form != nil {
		body = strings.NewReader(call.form.Encode())
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return 0, nil, err
	}
	if call.form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// qBittorrent's WebUI rejects requests whose Referer/Origin doesn't match
	// its own host as a CSRF precaution, so every request carries them.
	req.Header.Set("Referer", c.conn.base)
	req.Header.Set("Origin", c.conn.base)
	c.mu.Lock()
	sid := c.sid
	c.mu.Unlock()
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: "SID", Value: sid})
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

func (c *qbitClient) ensureLoggedIn() error {
	if c.conn.username == "" {
		return nil
	}
	c.mu.Lock()
	sid := c.sid
	c.mu.Unlock()
	if sid != "" {
		return nil
	}
	return c.login()
}

func (c *qbitClient) relogin() error {
	c.mu.Lock()
	c.sid = ""
	c.mu.Unlock()
	if c.conn.username == "" {
		return plugin.Errorf(plugin.CodeInternalError, "qbittorrent: session expired and no username/password configured to re-authenticate")
	}
	return c.login()
}

// login authenticates against /auth/login and captures the SID session
// cookie from the response. qBittorrent's WebUI answers with HTTP 200 and a
// plain-text body — "Ok." or "Fails." — even on bad credentials, so the body
// must be checked explicitly rather than trusting the status code alone.
func (c *qbitClient) login() error {
	form := url.Values{"username": {c.conn.username}, "password": {c.conn.password}}
	u := strings.TrimRight(c.conn.apiBase(), "/") + "/auth/login"
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "qbittorrent login: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", c.conn.base)
	req.Header.Set("Origin", c.conn.base)
	resp, err := c.http.Do(req)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "qbittorrent login: "+err.Error())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "qbittorrent login: reading response: "+err.Error())
	}
	body := strings.TrimSpace(string(raw))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || body == "Fails." {
		return plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("qbittorrent login failed (%d): %s", resp.StatusCode, body))
	}
	var sid string
	for _, ck := range resp.Cookies() {
		if ck.Name == "SID" {
			sid = ck.Value
		}
	}
	if sid == "" {
		return plugin.Errorf(plugin.CodeInternalError, "qbittorrent login: no SID cookie in response")
	}
	c.mu.Lock()
	c.sid = sid
	c.mu.Unlock()
	return nil
}

// shapeOutputs turns a raw response body into the outputs every verb
// returns: a JSON array decodes into both result and items; a JSON object
// decodes into result; anything else (qBittorrent's plain-text responses like
// "Ok." or a bare version string) is passed through as a trimmed string.
func shapeOutputs(raw []byte, status int) map[string]any {
	outputs := map[string]any{"status_code": status}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		outputs["result"] = ""
		return outputs
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		outputs["result"] = v
		if items, ok := v.([]any); ok {
			outputs["items"] = items
		}
		return outputs
	}
	outputs["result"] = trimmed
	return outputs
}

func main() {
	if err := plugin.Serve(&qbittorrentPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-qbittorrent:", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) ---

func str(v any) string {
	s, _ := v.(string)
	return s
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

// intStr renders an integer-ish option as a string flag value ("0" if absent).
func intStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "0"
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

func setIf(vals url.Values, key, val string) {
	if val != "" {
		vals.Set(key, val)
	}
}

// joinAny accepts a bare string, []string, or []any and joins it with sep;
// nil/absent yields "". This is how hashes/tags/urls accept either a single
// value or a list.
func joinAny(v any, sep string) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []string:
		return strings.Join(x, sep)
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			if s := fmt.Sprintf("%v", e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, sep)
	default:
		return fmt.Sprintf("%v", x)
	}
}
