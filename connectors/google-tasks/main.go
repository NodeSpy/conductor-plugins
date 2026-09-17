// Command conductor-google-tasks is a verb-only conductor connector for
// Google Tasks (API v1, https://tasks.googleapis.com/tasks/v1): task lists
// (list/get/create), tasks (list/get/create/update/delete/complete/move), and
// a raw `api` escape hatch for anything a first-class verb does not cover.
// Built ONLY against the public SDK (pkg/plugin) — no other dependency.
//
// Authentication is conductor's MANAGED OAuth2 (pkg/plugin v0.15.0+): this
// plugin does NOT perform any token exchange. It declares Google's OAuth2
// endpoints in Describe().Auth, the operator supplies client credentials in
// the connector's daemon-side `auth:` block, and `conductor connector auth
// google-tasks` runs the one-time browser login. The daemon then injects a
// fresh, rotated bearer token into every InvokeRequest.Connection under
// plugin.AccessTokenKey, read here with plugin.AccessToken. Because
// conductor — not this plugin — talks to oauth2.googleapis.com and
// accounts.google.com, the plugin's only egress is tasks.googleapis.com.
//
// AuthParams sets access_type=offline and prompt=consent: without
// access_type=offline Google never returns a refresh token, and the daemon
// would be unable to keep the connector authenticated past the first access
// token's expiry.
//
// Connection:
//
//	api_base: "https://..."     # optional test override, default https://tasks.googleapis.com/tasks/v1
//
// Every verb that needs a task list id accepts a `tasklist` option that
// defaults to "@default" (the user's default list) when omitted.
//
// Every verb's outputs include `status_code`; collection verbs (`tasklists`,
// `tasks`) hoist Google's `items` array into `items`; single-resource verbs
// return `result`. A non-2xx response becomes a CodeInternalError carrying
// the status and body — nothing is swallowed.
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

// defaultAPIBase is the Google Tasks API v1's production host+prefix.
// api_base overrides it for tests.
const defaultAPIBase = "https://tasks.googleapis.com/tasks/v1"

// defaultTasklist is the special id Google Tasks recognizes as "the
// authenticated user's default task list" — used whenever a verb's
// `tasklist` option is omitted.
const defaultTasklist = "@default"

type googleTasksPlugin struct {
	client *http.Client
}

func newGoogleTasksPlugin() *googleTasksPlugin {
	return &googleTasksPlugin{client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *googleTasksPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "google-tasks",
		Desc: "Google Tasks: task lists (list/get/create) and tasks (list/get/create/update/delete/complete/move), plus a raw `api` escape hatch over the Tasks API v1. Authenticates via conductor's managed OAuth2 — run `conductor connector auth google-tasks` after configuring an `auth:` block; this plugin never talks to Google's OAuth2 endpoints itself.",
		Connection: plugin.Schema{
			"api_base": {Type: "string", Desc: "override https://tasks.googleapis.com/tasks/v1 (tests only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "tasklists", Desc: "list the user's task lists",
				Usage:   "GET /users/@me/lists",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "tasklist_get", Desc: "get one task list",
				Usage: "GET /users/@me/lists/{tasklist}",
				Options: plugin.Schema{
					"tasklist": {Type: "string", Desc: "task list id (default \"@default\")", Scope: "tasklist"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "tasklist_create", Desc: "create a task list",
				Usage: "POST /users/@me/lists",
				Options: plugin.Schema{
					"title": {Type: "string", Required: true, Desc: "task list title"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "tasks", Desc: "list tasks on a task list",
				Usage: "GET /lists/{tasklist}/tasks",
				Options: plugin.Schema{
					"tasklist":      {Type: "string", Desc: "task list id (default \"@default\")", Scope: "tasklist"},
					"showCompleted": {Type: "boolean", Desc: "include completed tasks (Google default true)"},
					"showHidden":    {Type: "boolean", Desc: "include hidden (completed and no longer visible) tasks"},
					"maxResults":    {Type: "integer", Desc: "max tasks per page"},
					"dueMin":        {Type: "string", Desc: "RFC3339 lower bound (inclusive) on due date"},
					"dueMax":        {Type: "string", Desc: "RFC3339 upper bound (exclusive) on due date"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "task_get", Desc: "get one task",
				Usage: "GET /lists/{tasklist}/tasks/{task}",
				Options: plugin.Schema{
					"tasklist": {Type: "string", Desc: "task list id (default \"@default\")", Scope: "tasklist"},
					"task_id":  {Type: "string", Required: true, Scope: "task"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "task_create", Desc: "create a task",
				Usage: "POST /lists/{tasklist}/tasks",
				Options: plugin.Schema{
					"tasklist": {Type: "string", Desc: "task list id (default \"@default\")", Scope: "tasklist"},
					"title":    {Type: "string", Required: true, Desc: "task title"},
					"notes":    {Type: "string", Desc: "task notes/description"},
					"due":      {Type: "string", Desc: "RFC3339 due date/time"},
					"status":   {Type: "string", Desc: "\"needsAction\" or \"completed\""},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "task_update", Desc: "patch an existing task",
				Usage: "PATCH /lists/{tasklist}/tasks/{task}",
				Options: plugin.Schema{
					"tasklist": {Type: "string", Desc: "task list id (default \"@default\")", Scope: "tasklist"},
					"task_id":  {Type: "string", Required: true, Scope: "task"},
					"task":     {Type: "map", Desc: "a Google Tasks Task resource fragment; overrides the convenience fields below when set"},
					"title":    {Type: "string", Desc: "convenience: task title"},
					"notes":    {Type: "string", Desc: "convenience: task notes/description"},
					"due":      {Type: "string", Desc: "convenience: RFC3339 due date/time"},
					"status":   {Type: "string", Desc: "convenience: \"needsAction\" or \"completed\""},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "task_delete", Desc: "delete a task",
				Usage: "DELETE /lists/{tasklist}/tasks/{task}",
				Options: plugin.Schema{
					"tasklist": {Type: "string", Desc: "task list id (default \"@default\")", Scope: "tasklist"},
					"task_id":  {Type: "string", Required: true, Scope: "task"},
				},
				Outputs: plugin.Schema{"status_code": {Type: "integer"}},
			},
			{
				Name: "task_complete", Desc: "mark a task completed",
				Usage: "PATCH /lists/{tasklist}/tasks/{task} with {status: \"completed\"}",
				Options: plugin.Schema{
					"tasklist": {Type: "string", Desc: "task list id (default \"@default\")", Scope: "tasklist"},
					"task_id":  {Type: "string", Required: true, Scope: "task"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "task_move", Desc: "move a task to a new position and/or parent within its list",
				Usage: "POST /lists/{tasklist}/tasks/{task}/move",
				Options: plugin.Schema{
					"tasklist": {Type: "string", Desc: "task list id (default \"@default\")", Scope: "tasklist"},
					"task_id":  {Type: "string", Required: true, Scope: "task"},
					"parent":   {Type: "string", Desc: "new parent task id (omit to move to the top level)"},
					"previous": {Type: "string", Desc: "new previous-sibling task id (omit to move to the first position)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any Google Tasks API v1 endpoint (enables writes)",
				Usage: "method + path under /tasks/v1, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under https://tasks.googleapis.com/tasks/v1, e.g. /users/@me/lists"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// Conductor performs the OAuth2 token exchange with Google's own
		// endpoints on this plugin's behalf; the plugin itself only ever
		// calls tasks.googleapis.com.
		Capabilities: plugin.Capabilities{Egress: []string{"tasks.googleapis.com:443"}},
		Auth: &plugin.AuthSpec{
			Grants:   []string{"authorization_code"},
			TokenURL: "https://oauth2.googleapis.com/token",
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
			Scopes:   []string{"https://www.googleapis.com/auth/tasks"},
			// access_type=offline is REQUIRED for Google to return a refresh
			// token at all; prompt=consent forces the consent screen so a
			// re-auth (e.g. adding scopes later) still yields one.
			AuthParams: map[string]string{"access_type": "offline", "prompt": "consent"},
		},
	}
}

func (p *googleTasksPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, token, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "tasklists":
		return p.tasklists(conn, token)
	case "tasklist_get":
		return p.tasklistGet(conn, token, o)
	case "tasklist_create":
		return p.tasklistCreate(conn, token, o)
	case "tasks":
		return p.tasks(conn, token, o)
	case "task_get":
		return p.taskGet(conn, token, o)
	case "task_create":
		return p.taskCreate(conn, token, o)
	case "task_update":
		return p.taskUpdate(conn, token, o)
	case "task_delete":
		return p.taskDelete(conn, token, o)
	case "task_complete":
		return p.taskComplete(conn, token, o)
	case "task_move":
		return p.taskMove(conn, token, o)
	case "api":
		return p.api(conn, token, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type gtasksConn struct {
	apiBase string
}

// parseConn reads the connection map and the managed-OAuth2 bearer token
// injected by the daemon. A missing token means the operator has not
// configured an `auth:` block and logged in yet.
func parseConn(m map[string]any) (gtasksConn, string, error) {
	token := plugin.AccessToken(m)
	if token == "" {
		return gtasksConn{}, "", fmt.Errorf("no access token — add an auth: block and run `conductor connector auth google-tasks`")
	}
	base := strOr(str(m["api_base"]), defaultAPIBase)
	base = strings.TrimRight(base, "/")
	return gtasksConn{apiBase: base}, token, nil
}

// tasklistID resolves the task list to operate on: the verb's own tasklist
// option if set, otherwise "@default".
func tasklistID(o map[string]any) string {
	if v := str(o["tasklist"]); v != "" {
		return v
	}
	return defaultTasklist
}

// --- verb implementations ---

func (p *googleTasksPlugin) tasklists(conn gtasksConn, token string) (plugin.InvokeResult, error) {
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+"/users/@me/lists", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *googleTasksPlugin) tasklistGet(conn gtasksConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	path := "/users/@me/lists/" + url.PathEscape(tasklistID(o))
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+path, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleTasksPlugin) tasklistCreate(conn gtasksConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	title := str(o["title"])
	if title == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "title is required")
	}
	body := map[string]any{"title": title}
	status, respBody, err := p.do(token, http.MethodPost, conn.apiBase+"/users/@me/lists", nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleTasksPlugin) tasks(conn gtasksConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v, ok := o["showCompleted"]; ok {
		q.Set("showCompleted", boolStr(v))
	}
	if v, ok := o["showHidden"]; ok {
		q.Set("showHidden", boolStr(v))
	}
	if v := intStr(o["maxResults"]); v != "" {
		q.Set("maxResults", v)
	}
	if v := str(o["dueMin"]); v != "" {
		q.Set("dueMin", v)
	}
	if v := str(o["dueMax"]); v != "" {
		q.Set("dueMax", v)
	}
	path := "/lists/" + url.PathEscape(tasklistID(o)) + "/tasks"
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+path, q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *googleTasksPlugin) taskGet(conn gtasksConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["task_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "task_id is required")
	}
	path := "/lists/" + url.PathEscape(tasklistID(o)) + "/tasks/" + url.PathEscape(id)
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+path, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleTasksPlugin) taskCreate(conn gtasksConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	title := str(o["title"])
	if title == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "title is required")
	}
	body := map[string]any{"title": title}
	if v := str(o["notes"]); v != "" {
		body["notes"] = v
	}
	if v := str(o["due"]); v != "" {
		body["due"] = v
	}
	if v := str(o["status"]); v != "" {
		body["status"] = v
	}
	path := "/lists/" + url.PathEscape(tasklistID(o)) + "/tasks"
	status, respBody, err := p.do(token, http.MethodPost, conn.apiBase+path, nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleTasksPlugin) taskUpdate(conn gtasksConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["task_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "task_id is required")
	}
	patch := taskPatchBody(o)
	if len(patch) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "task (map of fields to patch) or at least one convenience field (title, notes, due, status) is required")
	}
	path := "/lists/" + url.PathEscape(tasklistID(o)) + "/tasks/" + url.PathEscape(id)
	status, respBody, err := p.do(token, http.MethodPatch, conn.apiBase+path, nil, patch)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleTasksPlugin) taskDelete(conn gtasksConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["task_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "task_id is required")
	}
	path := "/lists/" + url.PathEscape(tasklistID(o)) + "/tasks/" + url.PathEscape(id)
	status, _, err := p.do(token, http.MethodDelete, conn.apiBase+path, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"status_code": status}}, nil
}

func (p *googleTasksPlugin) taskComplete(conn gtasksConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["task_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "task_id is required")
	}
	path := "/lists/" + url.PathEscape(tasklistID(o)) + "/tasks/" + url.PathEscape(id)
	status, respBody, err := p.do(token, http.MethodPatch, conn.apiBase+path, nil, map[string]any{"status": "completed"})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleTasksPlugin) taskMove(conn gtasksConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["task_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "task_id is required")
	}
	q := url.Values{}
	if v := str(o["parent"]); v != "" {
		q.Set("parent", v)
	}
	if v := str(o["previous"]); v != "" {
		q.Set("previous", v)
	}
	path := "/lists/" + url.PathEscape(tasklistID(o)) + "/tasks/" + url.PathEscape(id) + "/move"
	status, respBody, err := p.do(token, http.MethodPost, conn.apiBase+path, q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *googleTasksPlugin) api(conn gtasksConn, token string, o map[string]any) (plugin.InvokeResult, error) {
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
	status, body, err := p.do(token, method, conn.apiBase+path, q, o["body"])
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

// --- task body construction ---

// taskPatchBody builds the PATCH body for task_update: the `task` map option
// if given, verbatim; otherwise assembled from whichever convenience
// title/notes/due/status options are set (a partial patch, since unset
// convenience fields must not clobber existing values).
func taskPatchBody(o map[string]any) map[string]any {
	if tm, ok := o["task"].(map[string]any); ok && len(tm) > 0 {
		return tm
	}
	body := map[string]any{}
	if v := str(o["title"]); v != "" {
		body["title"] = v
	}
	if v := str(o["notes"]); v != "" {
		body["notes"] = v
	}
	if v := str(o["due"]); v != "" {
		body["due"] = v
	}
	if v := str(o["status"]); v != "" {
		body["status"] = v
	}
	return body
}

// --- HTTP plumbing ---

// do performs one HTTP request against the Google Tasks API, attaching the
// Bearer token, and returns the status code and raw response body. A non-2xx
// status is translated into a CodeInternalError carrying the status and
// body — callers never need to check status codes themselves.
func (p *googleTasksPlugin) do(token, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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

// hoist pulls the `items` list out of a decoded Google Tasks list response
// (e.g. {"items": [...], "nextPageToken": "..."}). A bare list is returned
// as-is. Anything else yields an empty (never nil) list, so callers get a
// consistent [] rather than null on the wire.
func hoist(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		if list, ok := x["items"].([]any); ok {
			return list
		}
	}
	return []any{}
}

func main() {
	if err := plugin.Serve(newGoogleTasksPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-google-tasks:", err)
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

func boolv(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	}
	return false
}

// boolStr renders an option as a literal "true"/"false" query value.
func boolStr(v any) string {
	if boolv(v) {
		return "true"
	}
	return "false"
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
