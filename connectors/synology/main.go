// Command conductor-synology is a verb-only conductor connector for a
// Synology DSM NAS: system info and utilization, storage/volume info,
// File Station (list/getinfo/search), Download Station tasks, and a raw
// `api` escape hatch for any endpoint a first-class verb does not cover.
// Built ONLY against the public SDK (pkg/plugin) and the standard library —
// no third-party client.
//
// Synology's WebAPI is NOT a clean REST API: every request — including
// login — is a GET (or form-encoded POST) against one of two fixed CGI
// entry points, with the actual "endpoint" named by api/method/version
// query parameters:
//
//   - Login:   GET {base_url}/webapi/auth.cgi?api=SYNO.API.Auth&version=6&method=login&account=...&passwd=...&session=Core&format=sid[&otp_code=...]
//   - Verbs:   GET {base_url}/webapi/entry.cgi?api=<SYNO.X>&version=<n>&method=<m>&_sid=<sid>&<params...>
//
// Every response — success or failure — is HTTP 200 with a JSON envelope
// {"success": bool, "data": {...}, "error": {"code": n}}; API-level failure
// is signaled in that envelope, not in the HTTP status line.
//
// Auth is SESSION (sid) based: a successful login returns a session ID
// (`sid`) that must be echoed back as `_sid` on every subsequent entry.cgi
// call. This connector logs in once per connection (base_url + username +
// password + insecure_skip_verify identifies one cached session) and reuses
// the sid across Invoke calls. If a call's envelope comes back with
// success:false and one of the well-known "session expired" error codes
// (105 permission/session, 106 session timeout, 119 sid not found), the
// connector clears the cached sid, logs in again, and retries the call once
// before surfacing an error.
//
// Connection:
//
//	base_url:             "https://nas.example.com:5001"  # required
//	username:              "admin"                        # required
//	password:              "..."                          # required
//	otp_code:              "123456"                        # optional; 2FA one-time code, sent on login only
//	insecure_skip_verify:  false                           # optional, default false; per-connection tls.Config
//
// insecure_skip_verify disables TLS certificate verification for THIS
// connection only (a per-connection tls.Config, never process-wide). Only
// set it for a NAS reached over a trusted network path (e.g. your own
// LAN/VPN) — it removes protection against a man-in-the-middle presenting a
// forged certificate.
//
// Synology is self-hosted with no fixed public host, so Capabilities.Egress
// is declared empty; the operator's `network:` allowlist on the connector
// instance is the actual scope.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
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

type synologyPlugin struct {
	mu       sync.Mutex
	sessions map[string]*synologySession
}

func newSynologyPlugin() *synologyPlugin {
	return &synologyPlugin{sessions: map[string]*synologySession{}}
}

// synologySession is one authenticated session against one DSM host: an
// http.Client plus the cached sid. Every request that touches this session
// (including login/re-login) is serialized through mu, so a session-expired
// retry can never race with another goroutine's request on the same session.
type synologySession struct {
	mu     sync.Mutex
	client *http.Client
	sid    string
}

func (p *synologyPlugin) Describe() plugin.Decl {
	res := plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}}
	list := plugin.Schema{"items": {Type: "list"}, "result": {Type: "any"}, "status_code": {Type: "integer"}}

	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "synology",
		Desc: "Synology DSM: system info/utilization, storage, File Station (list/getinfo/search), Download Station tasks, and a raw `api` escape hatch, over the Synology WebAPI (session/sid auth, not REST). Self-hosted; declares no egress (narrow with network: per instance). No source.",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "DSM base URL, e.g. https://nas.example.com:5001"},
			"username":             {Type: "string", Required: true, Desc: "DSM account username"},
			"password":             {Type: "string", Required: true, Desc: "DSM account password"},
			"otp_code":             {Type: "string", Desc: "2-step verification (OTP) code, sent on login only"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification for this connection (default false). Only for a NAS reached over a trusted network — this removes protection against a forged certificate."},
		},
		Verbs: []plugin.Verb{
			{
				Name: "system_info", Desc: "DSM system information",
				Usage:   "GET entry.cgi?api=SYNO.Core.System&method=info",
				Options: plugin.Schema{},
				Outputs: res,
			},
			{
				Name: "utilization", Desc: "CPU/memory/network/disk utilization",
				Usage:   "GET entry.cgi?api=SYNO.Core.System.Utilization&method=get",
				Options: plugin.Schema{},
				Outputs: res,
			},
			{
				Name: "storage", Desc: "storage pools, volumes, and disks",
				Usage:   "GET entry.cgi?api=SYNO.Storage.CGI.Storage&method=load_info",
				Options: plugin.Schema{},
				Outputs: list,
			},
			{
				Name: "fs_list", Desc: "list files/folders under a File Station path",
				Usage: "GET entry.cgi?api=SYNO.FileStation.List&method=list",
				Options: plugin.Schema{
					"folder_path": {Type: "string", Required: true, Scope: "path", Desc: "e.g. /volume1/photo"},
					"additional":  {Type: "any", Desc: "extra info fields to include (a value or list), e.g. real_path,size,time"},
				},
				Outputs: list,
			},
			{
				Name: "fs_info", Desc: "get info for one or more File Station paths",
				Usage: "GET entry.cgi?api=SYNO.FileStation.List&method=getinfo",
				Options: plugin.Schema{
					"path":       {Type: "any", Required: true, Scope: "path", Desc: "a path, or a list of paths"},
					"additional": {Type: "any", Desc: "extra info fields to include (a value or list)"},
				},
				Outputs: list,
			},
			{
				Name: "fs_search", Desc: "start an asynchronous File Station search under a path",
				Usage: "GET entry.cgi?api=SYNO.FileStation.Search&method=start; returns a taskid — poll/stop it via the `api` escape hatch (method=list / method=stop, same taskid)",
				Options: plugin.Schema{
					"folder_path": {Type: "string", Required: true, Scope: "path"},
					"pattern":     {Type: "string", Desc: "filename search pattern, e.g. *.mp4"},
					"recursive":   {Type: "boolean", Desc: "search subfolders (default true)"},
				},
				Outputs: res,
			},
			{
				Name: "dl_tasks", Desc: "list Download Station tasks",
				Usage: "GET entry.cgi?api=SYNO.DownloadStation.Task&method=list",
				Options: plugin.Schema{
					"additional": {Type: "any", Desc: "extra info fields to include (a value or list), e.g. detail,transfer"},
				},
				Outputs: list,
			},
			{
				Name: "dl_create", Desc: "create a Download Station task",
				Usage: "POST entry.cgi?api=SYNO.DownloadStation.Task&method=create",
				Options: plugin.Schema{
					"uri":         {Type: "any", Required: true, Desc: "a download URI (http/ftp/magnet), or a list of them"},
					"destination": {Type: "string", Desc: "destination shared-folder-relative path"},
				},
				Outputs: res,
			},
			{
				Name: "dl_delete", Desc: "delete one or more Download Station tasks",
				Usage: "POST entry.cgi?api=SYNO.DownloadStation.Task&method=delete",
				Options: plugin.Schema{
					"id":             {Type: "any", Required: true, Scope: "task", Desc: "a task id, or a list of them"},
					"force_complete": {Type: "boolean", Desc: "treat the task as complete instead of just removing it"},
				},
				Outputs: list,
			},
			{
				Name: "api", Desc: "raw escape hatch: any Synology WebAPI endpoint via entry.cgi",
				Usage: "api + method (+ version, params, http_method), for anything without a first-class verb",
				Options: plugin.Schema{
					"api":         {Type: "string", Required: true, Desc: "the SYNO.* API namespace, e.g. SYNO.FileStation.List"},
					"method":      {Type: "string", Required: true, Desc: "the API method, e.g. list"},
					"version":     {Type: "integer", Desc: "API version (default 1)"},
					"params":      {Type: "map", Desc: "extra query/form parameters"},
					"http_method": {Type: "string", Desc: "GET or POST (default GET)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// A Synology NAS is self-hosted: there is no fixed public host to
		// declare. The operator narrows egress to their own NAS with
		// `network: ["nas.example.com:5001"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func (p *synologyPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "system_info":
		return p.systemInfo(conn)
	case "utilization":
		return p.utilization(conn)
	case "storage":
		return p.storage(conn)
	case "fs_list":
		return p.fsList(conn, o)
	case "fs_info":
		return p.fsInfo(conn, o)
	case "fs_search":
		return p.fsSearch(conn, o)
	case "dl_tasks":
		return p.dlTasks(conn, o)
	case "dl_create":
		return p.dlCreate(conn, o)
	case "dl_delete":
		return p.dlDelete(conn, o)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type synConn struct {
	baseURL            string
	username           string
	password           string
	otpCode            string
	insecureSkipVerify bool
}

func parseConn(m map[string]any) (synConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return synConn{}, fmt.Errorf("base_url is required")
	}
	username := str(m["username"])
	if username == "" {
		return synConn{}, fmt.Errorf("username is required")
	}
	password := str(m["password"])
	if password == "" {
		return synConn{}, fmt.Errorf("password is required")
	}
	return synConn{
		baseURL:            strings.TrimRight(base, "/"),
		username:           username,
		password:           password,
		otpCode:            str(m["otp_code"]),
		insecureSkipVerify: boolOr(m["insecure_skip_verify"], false),
	}, nil
}

// --- session management ---

// sessionKey identifies one cached sid session. otp_code is deliberately
// excluded: it is a one-shot login credential, not part of a session's
// ongoing identity, and once logged in the session no longer needs it.
func sessionKey(c synConn) string {
	return c.baseURL + "\x00" + c.username + "\x00" + c.password + "\x00" + boolStr(c.insecureSkipVerify)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (p *synologyPlugin) sessionFor(c synConn) *synologySession {
	key := sessionKey(c)
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[key]; ok {
		return s
	}
	transport := &http.Transport{}
	if c.insecureSkipVerify {
		// Per-connection tls.Config, never process-wide: this only affects
		// requests made through THIS session's client.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- opt-in per connection for self-signed NAS certs
	}
	s := &synologySession{client: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
	p.sessions[key] = s
	return s
}

// --- Synology WebAPI envelope ---

// synoEnvelope is the {"success","data","error"} wrapper every Synology
// WebAPI response is shaped as — including a FAILED call, which still
// answers HTTP 200 with success:false and an error.code.
type synoEnvelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   *synoErrorBody  `json:"error,omitempty"`
}

type synoErrorBody struct {
	Code int `json:"code"`
}

// synoAPIError is a Synology-level (success:false) failure, distinct from a
// transport/HTTP failure, so the retry logic can decide whether the code
// means "session expired" (one re-login + retry) versus any other failure.
type synoAPIError struct {
	code int
}

func (e *synoAPIError) Error() string {
	return fmt.Sprintf("SYNO error %d: %s", e.code, synoErrorDesc(e.code))
}

// isSessionExpiredCode reports whether code is one of the well-known
// "your session is no longer valid" codes: 105 (the logged-in session lacks
// permission — commonly surfaced for a stale/foreign sid), 106 (session
// timeout), and 119 (sid not found / invalid session).
func isSessionExpiredCode(code int) bool {
	switch code {
	case 105, 106, 119:
		return true
	}
	return false
}

// synoErrorDesc maps a handful of well-known Synology WebAPI error codes to
// a human description. Codes not in this table still produce a useful
// error — just without a friendly description.
func synoErrorDesc(code int) string {
	switch code {
	case 100:
		return "unknown error"
	case 101:
		return "no parameter of api, method or version"
	case 102:
		return "the requested api does not exist"
	case 103:
		return "the requested method does not exist"
	case 104:
		return "the requested version does not support the functionality"
	case 105:
		return "the logged in session does not have permission"
	case 106:
		return "session timeout"
	case 107:
		return "session interrupted by duplicate login"
	case 119:
		return "sid not found (invalid session)"
	case 400:
		return "no such account or incorrect password"
	case 401:
		return "account disabled"
	case 402:
		return "permission denied"
	case 403:
		return "2-step verification code required"
	case 404:
		return "failed to authenticate 2-step verification code"
	default:
		return "unknown"
	}
}

// login authenticates against auth.cgi and populates the session's sid.
// Must be called with sess.mu held.
func (p *synologyPlugin) login(sess *synologySession, conn synConn) error {
	q := url.Values{
		"api":     {"SYNO.API.Auth"},
		"version": {"6"},
		"method":  {"login"},
		"account": {conn.username},
		"passwd":  {conn.password},
		"session": {"Core"},
		"format":  {"sid"},
	}
	if conn.otpCode != "" {
		q.Set("otp_code", conn.otpCode)
	}
	env, _, err := p.rawRequest(sess, conn, http.MethodGet, "/webapi/auth.cgi", q)
	if err != nil {
		return err
	}
	if !env.Success {
		code := 0
		if env.Error != nil {
			code = env.Error.Code
		}
		return plugin.Errorf(plugin.CodeInternalError, "synology login failed: "+(&synoAPIError{code: code}).Error())
	}
	var data struct {
		Sid string `json:"sid"`
	}
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return plugin.Errorf(plugin.CodeInternalError, "synology login: decoding response: "+err.Error())
		}
	}
	if data.Sid == "" {
		return plugin.Errorf(plugin.CodeInternalError, "synology login: no sid in response")
	}
	sess.sid = data.Sid
	return nil
}

// rawRequest performs one HTTP request against a Synology CGI path (auth.cgi
// or entry.cgi) and decodes the {"success","data","error"} envelope. A
// non-2xx HTTP status is translated into a CodeInternalError — that is a
// transport failure, never a "success:false" API-level error.
func (p *synologyPlugin) rawRequest(sess *synologySession, conn synConn, httpMethod, cgiPath string, query url.Values) (*synoEnvelope, int, error) {
	full := conn.baseURL + cgiPath
	var req *http.Request
	var err error
	if strings.EqualFold(httpMethod, http.MethodPost) {
		req, err = http.NewRequest(http.MethodPost, full, strings.NewReader(query.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		req, err = http.NewRequest(http.MethodGet, full+"?"+query.Encode(), nil)
	}
	if err != nil {
		return nil, 0, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	resp, err := sess.client.Do(req)
	if err != nil {
		return nil, 0, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s %s: %d %s", httpMethod, cgiPath, resp.StatusCode, strings.TrimSpace(string(raw))))
	}
	var env synoEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, plugin.Errorf(plugin.CodeInternalError, "decoding SYNO response: "+err.Error())
	}
	return &env, resp.StatusCode, nil
}

// entryOnce performs one entry.cgi call with the session's current sid
// injected as _sid. It does NOT retry — entry (below) owns the
// session-expired retry policy. A success:false envelope is returned as
// *synoAPIError so the caller can inspect the code before deciding whether
// to translate it into a plugin.Error.
func (p *synologyPlugin) entryOnce(sess *synologySession, conn synConn, httpMethod, api string, version int, synoMethod string, params url.Values) (any, int, error) {
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set("api", api)
	q.Set("version", strconv.Itoa(version))
	q.Set("method", synoMethod)
	q.Set("_sid", sess.sid)

	env, status, err := p.rawRequest(sess, conn, httpMethod, "/webapi/entry.cgi", q)
	if err != nil {
		return nil, status, err
	}
	if !env.Success {
		code := 0
		if env.Error != nil {
			code = env.Error.Code
		}
		return nil, status, &synoAPIError{code: code}
	}
	if len(env.Data) == 0 {
		return nil, status, nil
	}
	var data any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, status, plugin.Errorf(plugin.CodeInternalError, "decoding data: "+err.Error())
	}
	return data, status, nil
}

// entry ensures the session is logged in, calls entryOnce, and — if the
// call failed with a well-known session-expired code — clears the cached
// sid, logs in again, and retries exactly once before giving up. Any other
// failure (transport, or a non-session SYNO error code) is surfaced as-is.
func (p *synologyPlugin) entry(conn synConn, httpMethod, api string, version int, synoMethod string, params url.Values) (any, int, error) {
	sess := p.sessionFor(conn)
	sess.mu.Lock()
	defer sess.mu.Unlock()

	if sess.sid == "" {
		if err := p.login(sess, conn); err != nil {
			return nil, 0, err
		}
	}

	data, status, callErr := p.entryOnce(sess, conn, httpMethod, api, version, synoMethod, params)
	if apiErr, ok := callErr.(*synoAPIError); ok && isSessionExpiredCode(apiErr.code) {
		sess.sid = ""
		if err := p.login(sess, conn); err != nil {
			return nil, status, err
		}
		data, status, callErr = p.entryOnce(sess, conn, httpMethod, api, version, synoMethod, params)
	}

	if callErr != nil {
		if apiErr, ok := callErr.(*synoAPIError); ok {
			return nil, status, plugin.Errorf(plugin.CodeInternalError, apiErr.Error())
		}
		return nil, status, callErr
	}
	return data, status, nil
}

// --- verb implementations ---

func (p *synologyPlugin) systemInfo(conn synConn) (plugin.InvokeResult, error) {
	data, status, err := p.entry(conn, http.MethodGet, "SYNO.Core.System", 1, "info", url.Values{})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": data, "status_code": status}}, nil
}

func (p *synologyPlugin) utilization(conn synConn) (plugin.InvokeResult, error) {
	data, status, err := p.entry(conn, http.MethodGet, "SYNO.Core.System.Utilization", 1, "get", url.Values{})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": data, "status_code": status}}, nil
}

func (p *synologyPlugin) storage(conn synConn) (plugin.InvokeResult, error) {
	data, status, err := p.entry(conn, http.MethodGet, "SYNO.Storage.CGI.Storage", 1, "load_info", url.Values{})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	items := hoist(data, "volumes")
	return plugin.InvokeResult{Outputs: map[string]any{"result": data, "items": items, "status_code": status}}, nil
}

func (p *synologyPlugin) fsList(conn synConn, o map[string]any) (plugin.InvokeResult, error) {
	folderPath := str(o["folder_path"])
	if folderPath == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "folder_path is required")
	}
	q := url.Values{"folder_path": {folderPath}}
	if v := joinAny(o["additional"], ","); v != "" {
		q.Set("additional", v)
	}
	data, status, err := p.entry(conn, http.MethodGet, "SYNO.FileStation.List", 2, "list", q)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	items := hoist(data, "files")
	return plugin.InvokeResult{Outputs: map[string]any{"result": data, "items": items, "status_code": status}}, nil
}

func (p *synologyPlugin) fsInfo(conn synConn, o map[string]any) (plugin.InvokeResult, error) {
	path := joinAny(o["path"], ",")
	if path == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "path is required")
	}
	q := url.Values{"path": {path}}
	if v := joinAny(o["additional"], ","); v != "" {
		q.Set("additional", v)
	}
	data, status, err := p.entry(conn, http.MethodGet, "SYNO.FileStation.List", 2, "getinfo", q)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	items := hoist(data, "files")
	return plugin.InvokeResult{Outputs: map[string]any{"result": data, "items": items, "status_code": status}}, nil
}

// fsSearch starts an asynchronous File Station search and returns its
// taskid. Synology's search API is inherently async (start/list/stop/clean)
// — polling or stopping the task by taskid is left to the `api` escape
// hatch rather than baked into this verb, keeping it a single request.
func (p *synologyPlugin) fsSearch(conn synConn, o map[string]any) (plugin.InvokeResult, error) {
	folderPath := str(o["folder_path"])
	if folderPath == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "folder_path is required")
	}
	q := url.Values{"folder_path": {folderPath}}
	if v := str(o["pattern"]); v != "" {
		q.Set("pattern", v)
	}
	if _, ok := o["recursive"]; ok {
		q.Set("recursive", strconv.FormatBool(boolv(o["recursive"])))
	}
	data, status, err := p.entry(conn, http.MethodGet, "SYNO.FileStation.Search", 1, "start", q)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": data, "status_code": status}}, nil
}

func (p *synologyPlugin) dlTasks(conn synConn, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := joinAny(o["additional"], ","); v != "" {
		q.Set("additional", v)
	}
	data, status, err := p.entry(conn, http.MethodGet, "SYNO.DownloadStation.Task", 1, "list", q)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	items := hoist(data, "tasks")
	return plugin.InvokeResult{Outputs: map[string]any{"result": data, "items": items, "status_code": status}}, nil
}

func (p *synologyPlugin) dlCreate(conn synConn, o map[string]any) (plugin.InvokeResult, error) {
	uri := joinAny(o["uri"], ",")
	if uri == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uri is required")
	}
	q := url.Values{"uri": {uri}}
	if v := str(o["destination"]); v != "" {
		q.Set("destination", v)
	}
	data, status, err := p.entry(conn, http.MethodPost, "SYNO.DownloadStation.Task", 1, "create", q)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if data != nil {
		out["result"] = data
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *synologyPlugin) dlDelete(conn synConn, o map[string]any) (plugin.InvokeResult, error) {
	id := joinAny(o["id"], ",")
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "id is required")
	}
	q := url.Values{"id": {id}}
	if _, ok := o["force_complete"]; ok {
		q.Set("force_complete", strconv.FormatBool(boolv(o["force_complete"])))
	}
	data, status, err := p.entry(conn, http.MethodPost, "SYNO.DownloadStation.Task", 1, "delete", q)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	items := hoist(data)
	return plugin.InvokeResult{Outputs: map[string]any{"result": data, "items": items, "status_code": status}}, nil
}

func (p *synologyPlugin) api(conn synConn, o map[string]any) (plugin.InvokeResult, error) {
	apiName := str(o["api"])
	if apiName == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "api is required")
	}
	method := str(o["method"])
	if method == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "method is required")
	}
	version := intOr(o["version"], 1)
	httpMethod := strOr(o["http_method"], http.MethodGet)
	q := url.Values{}
	if m, ok := o["params"].(map[string]any); ok {
		for k, v := range m {
			q.Set(k, fmt.Sprintf("%v", v))
		}
	}
	data, status, err := p.entry(conn, httpMethod, apiName, version, method, q)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	switch v := data.(type) {
	case []any:
		out["items"] = v
	default:
		if data != nil {
			out["result"] = v
		}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// --- response shaping ---

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
	if err := plugin.Serve(newSynologyPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-synology:", err)
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

func boolv(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	}
	return false
}

// intOr renders an integer-ish option, or def if absent/unparseable.
func intOr(v any, def int) int {
	switch x := v.(type) {
	case nil:
		return def
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		if n, err := strconv.Atoi(x); err == nil {
			return n
		}
		return def
	}
	return def
}

// joinAny accepts a bare string, []string, or []any and joins it with sep;
// nil/absent yields "". This is how uri/id/path/additional accept either a
// single value or a list.
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
