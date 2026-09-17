// Command conductor-google-drive is a verb-only conductor connector for
// Google Drive (API v3). It drives https://www.googleapis.com/drive/v3 (and
// the separate upload host for multipart uploads) over net/http: listing and
// fetching files, downloading and uploading file content, folder creation,
// metadata updates, deletion, and permissions, plus a raw `api` escape hatch
// for anything a first-class verb does not cover. Built ONLY against the
// public SDK (pkg/plugin) — no other dependency.
//
// Authentication is conductor's MANAGED OAuth2 (pkg/plugin v0.15.0+): this
// plugin does NOT perform any token exchange. It declares Google's OAuth2
// endpoints in Describe().Auth, the operator supplies client credentials in
// the connector's daemon-side `auth:` block, and `conductor connector auth
// google-drive` runs the one-time login. The daemon then injects a fresh,
// rotated bearer token into every InvokeRequest.Connection under
// plugin.AccessTokenKey, read here with plugin.AccessToken. Because
// conductor — not this plugin — talks to accounts.google.com /
// oauth2.googleapis.com, the plugin's only egress is www.googleapis.com.
//
// Connection:
//
//	api_base:    "https://..."  # optional; overrides https://www.googleapis.com/drive/v3 (tests)
//	upload_base: "https://..."  # optional; overrides https://www.googleapis.com/upload/drive/v3 (tests)
//
// Every verb returns `status_code`; a non-2xx response becomes a
// CodeInternalError carrying the status and body — nothing is swallowed.
// Collection verbs hoist Drive's `{"files": [...]}` / `{"permissions": [...]}`
// envelopes into `items`.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// defaultAPIBase is the Drive v3 REST API host. api_base overrides it for tests.
const defaultAPIBase = "https://www.googleapis.com/drive/v3"

// defaultUploadBase is the Drive v3 upload host (a separate host from the
// REST API per Google's API design). upload_base overrides it for tests.
const defaultUploadBase = "https://www.googleapis.com/upload/drive/v3"

// folderMimeType is the special MIME type Drive uses to represent a folder.
const folderMimeType = "application/vnd.google-apps.folder"

type gdrivePlugin struct {
	client *http.Client
}

func newGdrivePlugin() *gdrivePlugin {
	return &gdrivePlugin{client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *gdrivePlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "google-drive",
		Desc: "Google Drive: list/get/download/upload files, create folders, update metadata, delete, manage permissions over the Drive v3 API, plus a raw `api` escape hatch. Authenticates via conductor's managed OAuth2 — run `conductor connector auth google-drive` after configuring an `auth:` block; this plugin never talks to Google's OAuth2 endpoints itself.",
		Connection: plugin.Schema{
			"api_base":    {Type: "string", Desc: "override https://www.googleapis.com/drive/v3 (tests only)"},
			"upload_base": {Type: "string", Desc: "override https://www.googleapis.com/upload/drive/v3 (tests only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "files", Desc: "list/search files",
				Usage: "GET /files",
				Options: plugin.Schema{
					"q":         {Type: "string", Desc: "Drive query string, e.g. \"'root' in parents and trashed = false\""},
					"pageSize":  {Type: "integer", Desc: "max results per page"},
					"fields":    {Type: "string", Desc: "partial-response fields mask"},
					"orderBy":   {Type: "string", Desc: "sort expression, e.g. \"modifiedTime desc\""},
					"spaces":    {Type: "string", Desc: "comma-separated spaces to search, e.g. \"drive\""},
					"pageToken": {Type: "string", Desc: "continuation token from a previous call's nextPageToken"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list", Desc: "hoisted from result.files"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "file_get", Desc: "get one file's metadata",
				Usage: "GET /files/{id}",
				Options: plugin.Schema{
					"file_id": {Type: "string", Required: true, Scope: "file"},
					"fields":  {Type: "string", Desc: "partial-response fields mask"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "file_download", Desc: "download a file's raw content",
				Usage: "GET /files/{id}?alt=media",
				Options: plugin.Schema{
					"file_id": {Type: "string", Required: true, Scope: "file"},
				},
				Outputs: plugin.Schema{
					"content_base64": {Type: "string", Desc: "base64-encoded file content"},
					"content_type":   {Type: "string", Desc: "response Content-Type header"},
					"status_code":    {Type: "integer"},
				},
			},
			{
				Name: "file_upload", Desc: "upload a local file's content (multipart/related), creating a new Drive file",
				Usage: "POST {upload_base}/files?uploadType=multipart",
				Options: plugin.Schema{
					"path":      {Type: "string", Required: true, Desc: "local file path to read and upload"},
					"name":      {Type: "string", Desc: "Drive file name (defaults to the local file's base name)"},
					"parents":   {Type: "list", Desc: "parent folder ids"},
					"mime_type": {Type: "string", Desc: "MIME type of the file content (guessed from the path's extension if omitted)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "file_create_folder", Desc: "create a folder",
				Usage: "POST /files",
				Options: plugin.Schema{
					"name":    {Type: "string", Required: true},
					"parents": {Type: "list", Desc: "parent folder ids"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "file_update_metadata", Desc: "update a file's metadata",
				Usage: "PATCH /files/{id}",
				Options: plugin.Schema{
					"file_id":  {Type: "string", Required: true, Scope: "file"},
					"metadata": {Type: "map", Required: true, Desc: "Drive file metadata fields to update, e.g. {\"name\": \"new name.txt\"}"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "file_delete", Desc: "delete a file",
				Usage: "DELETE /files/{id}",
				Options: plugin.Schema{
					"file_id": {Type: "string", Required: true, Scope: "file"},
				},
				Outputs: plugin.Schema{"status_code": {Type: "integer"}},
			},
			{
				Name: "permissions", Desc: "list a file's permissions",
				Usage: "GET /files/{id}/permissions",
				Options: plugin.Schema{
					"file_id": {Type: "string", Required: true, Scope: "file"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "permission_create", Desc: "grant a permission on a file",
				Usage: "POST /files/{id}/permissions",
				Options: plugin.Schema{
					"file_id":      {Type: "string", Required: true, Scope: "file"},
					"role":         {Type: "string", Required: true, Desc: "e.g. reader, writer, commenter, owner"},
					"type":         {Type: "string", Required: true, Desc: "e.g. user, group, domain, anyone"},
					"emailAddress": {Type: "string", Desc: "required when type is user or group"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any Drive v3 API endpoint (enables writes)",
				Usage: "method + path under the Drive v3 API base, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under api_base, e.g. /files"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// Conductor performs the OAuth2 token exchange with Google on this
		// plugin's behalf; the plugin itself only ever calls www.googleapis.com
		// (both the Drive v3 REST API and its upload host share this domain).
		Capabilities: plugin.Capabilities{Egress: []string{"www.googleapis.com:443"}},
		Auth: &plugin.AuthSpec{
			Grants:   []string{"authorization_code"},
			TokenURL: "https://oauth2.googleapis.com/token",
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
			Scopes:   []string{"https://www.googleapis.com/auth/drive"},
			AuthParams: map[string]string{
				"access_type": "offline",
				"prompt":      "consent",
			},
		},
	}
}

func (p *gdrivePlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, token, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "files":
		return p.files(conn, token, o)
	case "file_get":
		return p.fileGet(conn, token, o)
	case "file_download":
		return p.fileDownload(conn, token, o)
	case "file_upload":
		return p.fileUpload(conn, token, o)
	case "file_create_folder":
		return p.fileCreateFolder(conn, token, o)
	case "file_update_metadata":
		return p.fileUpdateMetadata(conn, token, o)
	case "file_delete":
		return p.fileDelete(conn, token, o)
	case "permissions":
		return p.permissions(conn, token, o)
	case "permission_create":
		return p.permissionCreate(conn, token, o)
	case "api":
		return p.api(conn, token, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type gdriveConn struct {
	apiBase    string
	uploadBase string
}

// parseConn reads the connection map and the managed-OAuth2 bearer token
// injected by the daemon. A missing token means the operator has not
// configured an `auth:` block and logged in yet.
func parseConn(m map[string]any) (gdriveConn, string, error) {
	token := plugin.AccessToken(m)
	if token == "" {
		return gdriveConn{}, "", fmt.Errorf("no access token — add an auth: block and run `conductor connector auth google-drive`")
	}
	return gdriveConn{
		apiBase:    strings.TrimRight(strOr(str(m["api_base"]), defaultAPIBase), "/"),
		uploadBase: strings.TrimRight(strOr(str(m["upload_base"]), defaultUploadBase), "/"),
	}, token, nil
}

// --- verb implementations ---

func (p *gdrivePlugin) files(conn gdriveConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["q"]); v != "" {
		q.Set("q", v)
	}
	if v := intStr(o["pageSize"]); v != "" {
		q.Set("pageSize", v)
	}
	if v := str(o["fields"]); v != "" {
		q.Set("fields", v)
	}
	if v := str(o["orderBy"]); v != "" {
		q.Set("orderBy", v)
	}
	if v := str(o["spaces"]); v != "" {
		q.Set("spaces", v)
	}
	if v := str(o["pageToken"]); v != "" {
		q.Set("pageToken", v)
	}
	status, body, _, err := p.do(token, http.MethodGet, conn.apiBase+"/files", q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "files")
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "items": items, "status_code": status}}, nil
}

func (p *gdrivePlugin) fileGet(conn gdriveConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["file_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "file_id is required")
	}
	q := url.Values{}
	if v := str(o["fields"]); v != "" {
		q.Set("fields", v)
	}
	status, body, _, err := p.do(token, http.MethodGet, conn.apiBase+"/files/"+url.PathEscape(id), q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *gdrivePlugin) fileDownload(conn gdriveConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["file_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "file_id is required")
	}
	q := url.Values{"alt": []string{"media"}}
	status, body, headers, err := p.do(token, http.MethodGet, conn.apiBase+"/files/"+url.PathEscape(id), q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString(body),
		"content_type":   headers.Get("Content-Type"),
		"status_code":    status,
	}}, nil
}

func (p *gdrivePlugin) fileUpload(conn gdriveConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	path := str(o["path"])
	if path == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "path is required")
	}
	name := str(o["name"])
	if name == "" {
		name = filepath.Base(path)
	}
	metadata := map[string]any{"name": name}
	if parents := strList(o["parents"]); len(parents) > 0 {
		metadata["parents"] = parents
	}
	mimeType := str(o["mime_type"])
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(path))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	metadata["mimeType"] = mimeType

	f, err := os.Open(path)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	defer f.Close()

	status, body, err := p.doMultipartRelated(token, conn.uploadBase+"/files?uploadType=multipart", metadata, mimeType, f)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *gdrivePlugin) fileCreateFolder(conn gdriveConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	name := str(o["name"])
	if name == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "name is required")
	}
	payload := map[string]any{"name": name, "mimeType": folderMimeType}
	if parents := strList(o["parents"]); len(parents) > 0 {
		payload["parents"] = parents
	}
	status, body, _, err := p.do(token, http.MethodPost, conn.apiBase+"/files", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *gdrivePlugin) fileUpdateMetadata(conn gdriveConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["file_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "file_id is required")
	}
	metadata, ok := o["metadata"].(map[string]any)
	if !ok || len(metadata) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "metadata is required")
	}
	status, body, _, err := p.do(token, http.MethodPatch, conn.apiBase+"/files/"+url.PathEscape(id), nil, metadata)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *gdrivePlugin) fileDelete(conn gdriveConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["file_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "file_id is required")
	}
	status, _, _, err := p.do(token, http.MethodDelete, conn.apiBase+"/files/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"status_code": status}}, nil
}

func (p *gdrivePlugin) permissions(conn gdriveConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["file_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "file_id is required")
	}
	status, body, _, err := p.do(token, http.MethodGet, conn.apiBase+"/files/"+url.PathEscape(id)+"/permissions", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "permissions")
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *gdrivePlugin) permissionCreate(conn gdriveConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["file_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "file_id is required")
	}
	role := str(o["role"])
	if role == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "role is required")
	}
	typ := str(o["type"])
	if typ == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "type is required")
	}
	payload := map[string]any{"role": role, "type": typ}
	if v := str(o["emailAddress"]); v != "" {
		payload["emailAddress"] = v
	}
	status, body, _, err := p.do(token, http.MethodPost, conn.apiBase+"/files/"+url.PathEscape(id)+"/permissions", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *gdrivePlugin) api(conn gdriveConn, token string, o map[string]any) (plugin.InvokeResult, error) {
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
	status, body, _, err := p.do(token, method, conn.apiBase+path, q, o["body"])
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

// do performs one HTTP request against the Drive API, attaching the Bearer
// token, and returns the status code, raw response body, and response
// headers. A non-2xx status is translated into a CodeInternalError carrying
// the status and body — callers never need to check status codes themselves.
func (p *gdrivePlugin) do(token, method, endpoint string, query url.Values, body any) (int, []byte, http.Header, error) {
	full := endpoint
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return 0, nil, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, respBody, resp.Header, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s %s: %d %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(respBody))))
	}
	return resp.StatusCode, respBody, resp.Header, nil
}

// doMultipartRelated performs the Drive v3 multipart upload: a
// multipart/related body whose first part is the JSON metadata and second
// part is the file content, per
// https://developers.google.com/drive/api/guides/manage-uploads#multipart.
// Unlike multipart/form-data (used by e.g. the audiobookshelf connector's
// upload verb), each part here carries only a Content-Type header — no
// Content-Disposition/form-field name — so the parts are built with
// multipart.Writer.CreatePart rather than CreateFormFile.
func (p *gdrivePlugin) doMultipartRelated(token, endpoint string, metadata map[string]any, contentType string, file io.Reader) (int, []byte, error) {
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	metaPart, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if _, err := metaPart.Write(metaJSON); err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}

	filePart, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {contentType}})
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if _, err := io.Copy(filePart, file); err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if err := mw.Close(); err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, &buf)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
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
			fmt.Sprintf("POST %s: %d %s", endpoint, resp.StatusCode, strings.TrimSpace(string(respBody))))
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

// hoist pulls a list out of a decoded Drive API envelope: a map with the
// given key holding a list (e.g. {"files": [...]}). A bare list is returned
// as-is. Anything else yields an empty (never nil) list, so callers get a
// consistent [] rather than null on the wire.
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
	if err := plugin.Serve(newGdrivePlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-google-drive:", err)
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
