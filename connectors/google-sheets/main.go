// Command conductor-google-sheets is a verb-only conductor connector for
// Google Sheets (API v4). It drives the Sheets API
// (https://sheets.googleapis.com/v4/spreadsheets) over net/http: spreadsheet
// metadata, reading and writing cell ranges, appending and clearing rows,
// batch reads/writes, creating new spreadsheets, and a raw `api` escape
// hatch for anything a first-class verb does not cover. Built ONLY against
// the public SDK (pkg/plugin) — no other dependency.
//
// Authentication is conductor's MANAGED OAuth2 (pkg/plugin v0.15.0+): this
// plugin does NOT perform any token exchange. It declares Google's OAuth2
// endpoints in Describe().Auth, the operator supplies client credentials in
// the connector's daemon-side `auth:` block, and `conductor connector auth
// google-sheets` runs the one-time login. The daemon then injects a fresh,
// rotated bearer token into every InvokeRequest.Connection under
// plugin.AccessTokenKey, read here with plugin.AccessToken. Because
// conductor — not this plugin — talks to accounts.google.com /
// oauth2.googleapis.com, the plugin's only egress is sheets.googleapis.com.
//
// Connection:
//
//	api_base: "https://..."  # optional; overrides https://sheets.googleapis.com/v4/spreadsheets (tests)
//
// Sheets API v4 responses are JSON objects, not bare arrays (unlike some
// other APIs' list endpoints), so every verb hoists its decoded body
// straight into `result`. Every verb also returns `status_code`; a non-2xx
// response becomes a CodeInternalError carrying the status and body —
// nothing is swallowed.
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

// defaultAPIBase is the Sheets API v4 spreadsheets resource root. api_base
// overrides it for tests.
const defaultAPIBase = "https://sheets.googleapis.com/v4/spreadsheets"

type sheetsPlugin struct {
	client *http.Client
}

func newSheetsPlugin() *sheetsPlugin {
	return &sheetsPlugin{client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *sheetsPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "google-sheets",
		Desc: "Google Sheets: spreadsheet metadata, reading/writing/appending/clearing cell ranges, batch value reads, batch structural updates, creating spreadsheets, and a raw `api` escape hatch over the Sheets API v4. Authenticates via conductor's managed OAuth2 — run `conductor connector auth google-sheets` after configuring an `auth:` block; this plugin never talks to Google's OAuth endpoints itself.",
		Connection: plugin.Schema{
			"api_base": {Type: "string", Desc: "override https://sheets.googleapis.com/v4/spreadsheets (tests only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "get", Desc: "get a spreadsheet's metadata (and optionally grid data)",
				Usage: "GET /{spreadsheet_id}",
				Options: plugin.Schema{
					"spreadsheet_id":    {Type: "string", Required: true, Scope: "spreadsheet"},
					"include_grid_data": {Type: "boolean", Desc: "include cell data in the response"},
					"ranges":            {Type: "list", Desc: "A1 ranges to limit the response to, e.g. [\"Sheet1!A1:C10\"]"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "values_get", Desc: "get the values of a single A1 range",
				Usage: "GET /{spreadsheet_id}/values/{range}",
				Options: plugin.Schema{
					"spreadsheet_id":      {Type: "string", Required: true, Scope: "spreadsheet"},
					"range":               {Type: "string", Required: true, Desc: "A1 range, e.g. Sheet1!A1:C10"},
					"value_render_option": {Type: "string", Desc: "FORMATTED_VALUE (default), UNFORMATTED_VALUE, or FORMULA"},
					"major_dimension":     {Type: "string", Desc: "ROWS (default) or COLUMNS"},
				},
				Outputs: plugin.Schema{"result": {Type: "any", Desc: "has a `values` field: a 2D array of cell values"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "values_update", Desc: "overwrite the values of a single A1 range",
				Usage: "PUT /{spreadsheet_id}/values/{range}?valueInputOption=USER_ENTERED",
				Options: plugin.Schema{
					"spreadsheet_id": {Type: "string", Required: true, Scope: "spreadsheet"},
					"range":          {Type: "string", Required: true, Desc: "A1 range, e.g. Sheet1!A1:C10"},
					"values":         {Type: "list", Required: true, Desc: "2D array of cell values (rows of columns)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "values_append", Desc: "append rows of values after the last row of a range",
				Usage: "POST /{spreadsheet_id}/values/{range}:append?valueInputOption=USER_ENTERED&insertDataOption=INSERT_ROWS",
				Options: plugin.Schema{
					"spreadsheet_id": {Type: "string", Required: true, Scope: "spreadsheet"},
					"range":          {Type: "string", Required: true, Desc: "A1 range to search for a table within, e.g. Sheet1!A1:C10"},
					"values":         {Type: "list", Required: true, Desc: "2D array of cell values (rows of columns) to append"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "values_clear", Desc: "clear the values of a single A1 range (formatting is untouched)",
				Usage: "POST /{spreadsheet_id}/values/{range}:clear",
				Options: plugin.Schema{
					"spreadsheet_id": {Type: "string", Required: true, Scope: "spreadsheet"},
					"range":          {Type: "string", Required: true, Desc: "A1 range, e.g. Sheet1!A1:C10"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "values_batch_get", Desc: "get the values of multiple A1 ranges in one call",
				Usage: "GET /{spreadsheet_id}/values:batchGet",
				Options: plugin.Schema{
					"spreadsheet_id": {Type: "string", Required: true, Scope: "spreadsheet"},
					"ranges":         {Type: "list", Desc: "A1 ranges, e.g. [\"Sheet1!A1:C10\", \"Sheet2!A:A\"]"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "batch_update", Desc: "apply one or more structural/formatting update requests atomically",
				Usage: "POST /{spreadsheet_id}:batchUpdate",
				Options: plugin.Schema{
					"spreadsheet_id": {Type: "string", Required: true, Scope: "spreadsheet"},
					"requests":       {Type: "list", Required: true, Desc: "list of Sheets API Request objects, e.g. [{\"addSheet\": {...}}]"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "create", Desc: "create a new spreadsheet",
				Usage: "POST /",
				Options: plugin.Schema{
					"title":  {Type: "string", Desc: "spreadsheet title"},
					"sheets": {Type: "list", Desc: "list of Sheets API Sheet objects to seed the spreadsheet with"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any Sheets API v4 endpoint (enables writes)",
				Usage: "method + path under the spreadsheets resource root, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under https://sheets.googleapis.com/v4/spreadsheets, e.g. /{spreadsheetId}/values/A1:B2"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// Conductor performs the OAuth2 token exchange with Google on this
		// plugin's behalf; the plugin itself only ever calls
		// sheets.googleapis.com.
		Capabilities: plugin.Capabilities{Egress: []string{"sheets.googleapis.com:443"}},
		Auth: &plugin.AuthSpec{
			Grants:     []string{"authorization_code"},
			TokenURL:   "https://oauth2.googleapis.com/token",
			AuthURL:    "https://accounts.google.com/o/oauth2/v2/auth",
			Scopes:     []string{"https://www.googleapis.com/auth/spreadsheets"},
			AuthParams: map[string]string{"access_type": "offline", "prompt": "consent"},
		},
	}
}

func (p *sheetsPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, token, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "get":
		return p.get(conn, token, o)
	case "values_get":
		return p.valuesGet(conn, token, o)
	case "values_update":
		return p.valuesUpdate(conn, token, o)
	case "values_append":
		return p.valuesAppend(conn, token, o)
	case "values_clear":
		return p.valuesClear(conn, token, o)
	case "values_batch_get":
		return p.valuesBatchGet(conn, token, o)
	case "batch_update":
		return p.batchUpdate(conn, token, o)
	case "create":
		return p.create(conn, token, o)
	case "api":
		return p.api(conn, token, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type sheetsConn struct {
	apiBase string
}

// parseConn reads the connection map and the managed-OAuth2 bearer token
// injected by the daemon. A missing token means the operator has not
// configured an `auth:` block and logged in yet.
func parseConn(m map[string]any) (sheetsConn, string, error) {
	token := plugin.AccessToken(m)
	if token == "" {
		return sheetsConn{}, "", fmt.Errorf("no access token — add an auth: block and run `conductor connector auth google-sheets`")
	}
	base := strOr(str(m["api_base"]), defaultAPIBase)
	base = strings.TrimRight(base, "/")
	return sheetsConn{apiBase: base}, token, nil
}

// --- verb implementations ---

func (p *sheetsPlugin) get(conn sheetsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	sid := str(o["spreadsheet_id"])
	if sid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "spreadsheet_id is required")
	}
	q := url.Values{}
	if boolv(o["include_grid_data"]) {
		q.Set("includeGridData", "true")
	}
	for _, r := range strList(o["ranges"]) {
		q.Add("ranges", r)
	}
	return p.doJSON(token, http.MethodGet, conn.apiBase+"/"+url.PathEscape(sid), q, nil)
}

func (p *sheetsPlugin) valuesGet(conn sheetsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	sid, rng, err := requireSpreadsheetAndRange(o)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	q := url.Values{}
	if v := str(o["value_render_option"]); v != "" {
		q.Set("valueRenderOption", v)
	}
	if v := str(o["major_dimension"]); v != "" {
		q.Set("majorDimension", v)
	}
	endpoint := conn.apiBase + "/" + url.PathEscape(sid) + "/values/" + url.PathEscape(rng)
	return p.doJSON(token, http.MethodGet, endpoint, q, nil)
}

func (p *sheetsPlugin) valuesUpdate(conn sheetsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	sid, rng, err := requireSpreadsheetAndRange(o)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	values, ok := o["values"]
	if !ok || values == nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "values is required")
	}
	q := url.Values{"valueInputOption": []string{"USER_ENTERED"}}
	endpoint := conn.apiBase + "/" + url.PathEscape(sid) + "/values/" + url.PathEscape(rng)
	return p.doJSON(token, http.MethodPut, endpoint, q, map[string]any{"values": values})
}

func (p *sheetsPlugin) valuesAppend(conn sheetsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	sid, rng, err := requireSpreadsheetAndRange(o)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	values, ok := o["values"]
	if !ok || values == nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "values is required")
	}
	q := url.Values{"valueInputOption": []string{"USER_ENTERED"}, "insertDataOption": []string{"INSERT_ROWS"}}
	endpoint := conn.apiBase + "/" + url.PathEscape(sid) + "/values/" + url.PathEscape(rng) + ":append"
	return p.doJSON(token, http.MethodPost, endpoint, q, map[string]any{"values": values})
}

func (p *sheetsPlugin) valuesClear(conn sheetsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	sid, rng, err := requireSpreadsheetAndRange(o)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	endpoint := conn.apiBase + "/" + url.PathEscape(sid) + "/values/" + url.PathEscape(rng) + ":clear"
	return p.doJSON(token, http.MethodPost, endpoint, nil, nil)
}

func (p *sheetsPlugin) valuesBatchGet(conn sheetsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	sid := str(o["spreadsheet_id"])
	if sid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "spreadsheet_id is required")
	}
	q := url.Values{}
	for _, r := range strList(o["ranges"]) {
		q.Add("ranges", r)
	}
	endpoint := conn.apiBase + "/" + url.PathEscape(sid) + "/values:batchGet"
	return p.doJSON(token, http.MethodGet, endpoint, q, nil)
}

func (p *sheetsPlugin) batchUpdate(conn sheetsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	sid := str(o["spreadsheet_id"])
	if sid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "spreadsheet_id is required")
	}
	requests, ok := o["requests"]
	if !ok || requests == nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "requests is required")
	}
	endpoint := conn.apiBase + "/" + url.PathEscape(sid) + ":batchUpdate"
	return p.doJSON(token, http.MethodPost, endpoint, nil, map[string]any{"requests": requests})
}

func (p *sheetsPlugin) create(conn sheetsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	body := map[string]any{}
	props := map[string]any{}
	if title := str(o["title"]); title != "" {
		props["title"] = title
	}
	if len(props) > 0 {
		body["properties"] = props
	}
	if sheets, ok := o["sheets"]; ok && sheets != nil {
		body["sheets"] = sheets
	}
	return p.doJSON(token, http.MethodPost, conn.apiBase, nil, body)
}

func (p *sheetsPlugin) api(conn sheetsConn, token string, o map[string]any) (plugin.InvokeResult, error) {
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

// --- shared helpers ---

// requireSpreadsheetAndRange validates the two options every single-range
// values_* verb needs.
func requireSpreadsheetAndRange(o map[string]any) (string, string, error) {
	sid := str(o["spreadsheet_id"])
	if sid == "" {
		return "", "", plugin.Errorf(plugin.CodeInvalidParams, "spreadsheet_id is required")
	}
	rng := str(o["range"])
	if rng == "" {
		return "", "", plugin.Errorf(plugin.CodeInvalidParams, "range is required")
	}
	return sid, rng, nil
}

// doJSON performs one request and, on success, decodes the (always-object)
// response body straight into `result`.
func (p *sheetsPlugin) doJSON(token, method, endpoint string, query url.Values, body any) (plugin.InvokeResult, error) {
	status, respBody, err := p.do(token, method, endpoint, query, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	if decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// --- HTTP plumbing ---

// do performs one HTTP request against the Sheets API, attaching the Bearer
// token, and returns the status code and raw response body. A non-2xx
// status is translated into a CodeInternalError carrying the status and
// body — callers never need to check status codes themselves.
func (p *sheetsPlugin) do(token, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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

func main() {
	if err := plugin.Serve(newSheetsPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-google-sheets:", err)
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
