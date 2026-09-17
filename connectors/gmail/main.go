// Command conductor-gmail is a verb-only conductor connector for Gmail
// (Google's Gmail API v1). It drives https://gmail.googleapis.com/gmail/v1
// over net/http for the authenticated user ("me"): messages, sending mail,
// labels, drafts, threads, label modification, trash/untrash, and a raw `api`
// escape hatch for anything a first-class verb does not cover. Built ONLY
// against the public SDK (pkg/plugin) — no other dependency.
//
// Authentication is conductor's MANAGED OAuth2 (pkg/plugin v0.15.0+): this
// plugin does NOT perform any token exchange. It declares Google's OAuth2
// endpoints in Describe().Auth, the operator supplies client credentials in
// the connector's daemon-side `auth:` block, and `conductor connector auth
// gmail` runs the one-time login (Google requires access_type=offline and
// prompt=consent, baked into Auth.AuthParams, to hand back a refresh token).
// The daemon then injects a fresh, rotated bearer token into every
// InvokeRequest.Connection under plugin.AccessTokenKey, read here with
// plugin.AccessToken. Because conductor — not this plugin — talks to
// oauth2.googleapis.com, the plugin's only egress is gmail.googleapis.com.
//
// Connection:
//
//	api_base: "https://..."  # optional; overrides
//	                          # https://gmail.googleapis.com/gmail/v1/users/me
//	                          # (tests only)
//
// Gmail's collection endpoints wrap their list under a named key, e.g.
// {"messages": [...]}. Collection verbs (messages, labels, threads, drafts)
// hoist that list into `items`; single-resource verbs return the decoded
// body as `result`. Every verb also returns `status_code`; a non-2xx
// response becomes a CodeInternalError carrying the status and body —
// nothing is swallowed.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// defaultAPIBase is the Gmail API v1 base for the authenticated user ("me").
// api_base overrides it for tests.
const defaultAPIBase = "https://gmail.googleapis.com/gmail/v1/users/me"

type gmailPlugin struct {
	client *http.Client
}

func newGmailPlugin() *gmailPlugin {
	return &gmailPlugin{client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *gmailPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "gmail",
		Desc: "Gmail: list/get messages, send mail, labels, drafts, threads, modify/trash/untrash, and a raw `api` escape hatch over the Gmail API v1. Authenticates via conductor's managed OAuth2 — run `conductor connector auth gmail` after configuring an `auth:` block; this plugin never talks to oauth2.googleapis.com itself.",
		Connection: plugin.Schema{
			"api_base": {Type: "string", Desc: "override https://gmail.googleapis.com/gmail/v1/users/me (tests only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "messages", Desc: "list messages",
				Usage: "GET /messages",
				Options: plugin.Schema{
					"q":          {Type: "string", Desc: "Gmail search query, e.g. is:unread from:a@b.com"},
					"labelIds":   {Type: "list", Desc: "restrict to messages with all of these label ids"},
					"maxResults": {Type: "integer", Desc: "page size"},
					"pageToken":  {Type: "string", Desc: "page token from a previous call"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "message_get", Desc: "get one message",
				Usage: "GET /messages/{id}",
				Options: plugin.Schema{
					"message_id": {Type: "string", Required: true, Scope: "message"},
					"format":     {Type: "string", Desc: "full (default) | metadata | minimal", Enum: []string{"full", "metadata", "minimal"}},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "send", Desc: "send an email",
				Usage: "POST /messages/send",
				Options: plugin.Schema{
					"to":      {Type: "list", Required: true, Desc: "recipient addresses"},
					"cc":      {Type: "list", Desc: "cc addresses"},
					"bcc":     {Type: "list", Desc: "bcc addresses"},
					"subject": {Type: "string", Required: true},
					"text":    {Type: "string", Desc: "plain-text body; multipart/alternative with html if both are set"},
					"html":    {Type: "string", Desc: "HTML body; multipart/alternative with text if both are set"},
					"from":    {Type: "string", Desc: "From header; omitted defaults to the authenticated account"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "labels", Desc: "list labels",
				Usage:   "GET /labels",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "label_create", Desc: "create a label",
				Usage: "POST /labels",
				Options: plugin.Schema{
					"name":                  {Type: "string", Required: true},
					"labelListVisibility":   {Type: "string", Desc: "labelShow | labelShowIfUnread | labelHide"},
					"messageListVisibility": {Type: "string", Desc: "show | hide"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "drafts", Desc: "list drafts",
				Usage:   "GET /drafts",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "draft_create", Desc: "create a draft",
				Usage: "POST /drafts",
				Options: plugin.Schema{
					"to":      {Type: "list", Required: true, Desc: "recipient addresses"},
					"cc":      {Type: "list", Desc: "cc addresses"},
					"bcc":     {Type: "list", Desc: "bcc addresses"},
					"subject": {Type: "string", Required: true},
					"text":    {Type: "string", Desc: "plain-text body; multipart/alternative with html if both are set"},
					"html":    {Type: "string", Desc: "HTML body; multipart/alternative with text if both are set"},
					"from":    {Type: "string", Desc: "From header; omitted defaults to the authenticated account"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "threads", Desc: "list threads",
				Usage: "GET /threads",
				Options: plugin.Schema{
					"q": {Type: "string", Desc: "Gmail search query"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "thread_get", Desc: "get one thread",
				Usage: "GET /threads/{id}",
				Options: plugin.Schema{
					"thread_id": {Type: "string", Required: true, Scope: "thread"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "modify", Desc: "add/remove labels on a message",
				Usage: "POST /messages/{id}/modify",
				Options: plugin.Schema{
					"message_id":    {Type: "string", Required: true, Scope: "message"},
					"add_labels":    {Type: "list", Desc: "label ids to add"},
					"remove_labels": {Type: "list", Desc: "label ids to remove"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "trash", Desc: "move a message to trash",
				Usage: "POST /messages/{id}/trash",
				Options: plugin.Schema{
					"message_id": {Type: "string", Required: true, Scope: "message"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "untrash", Desc: "remove a message from trash",
				Usage: "POST /messages/{id}/untrash",
				Options: plugin.Schema{
					"message_id": {Type: "string", Required: true, Scope: "message"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any Gmail API endpoint (enables writes)",
				Usage: "method + path under gmail/v1/users/me, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under the connection's api_base, e.g. /messages"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// Conductor performs the OAuth2 token exchange with oauth2.googleapis.com
		// on this plugin's behalf; the plugin itself only ever calls
		// gmail.googleapis.com.
		Capabilities: plugin.Capabilities{Egress: []string{"gmail.googleapis.com:443"}},
		Auth: &plugin.AuthSpec{
			Grants:   []string{"authorization_code"},
			TokenURL: "https://oauth2.googleapis.com/token",
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
			Scopes:   []string{"https://www.googleapis.com/auth/gmail.modify"},
			// Google only returns a refresh token when access_type=offline is
			// requested, and only on the first consent unless prompt=consent
			// forces the consent screen again — both baked in here so the
			// operator's `auth:` block needn't know Google's quirks.
			AuthParams: map[string]string{"access_type": "offline", "prompt": "consent"},
		},
	}
}

func (p *gmailPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, token, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "messages":
		return p.messages(conn, token, o)
	case "message_get":
		return p.messageGet(conn, token, o)
	case "send":
		return p.send(conn, token, o)
	case "labels":
		return p.labels(conn, token)
	case "label_create":
		return p.labelCreate(conn, token, o)
	case "drafts":
		return p.drafts(conn, token)
	case "draft_create":
		return p.draftCreate(conn, token, o)
	case "threads":
		return p.threads(conn, token, o)
	case "thread_get":
		return p.threadGet(conn, token, o)
	case "modify":
		return p.modify(conn, token, o)
	case "trash":
		return p.trash(conn, token, o)
	case "untrash":
		return p.untrash(conn, token, o)
	case "api":
		return p.api(conn, token, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type gmailConn struct {
	apiBase string
}

// parseConn reads the connection map and the managed-OAuth2 bearer token
// injected by the daemon. A missing token means the operator has not
// configured an `auth:` block and logged in yet.
func parseConn(m map[string]any) (gmailConn, string, error) {
	token := plugin.AccessToken(m)
	if token == "" {
		return gmailConn{}, "", fmt.Errorf("no access token — add an auth: block and run `conductor connector auth gmail`")
	}
	base := strOr(str(m["api_base"]), defaultAPIBase)
	base = strings.TrimRight(base, "/")
	return gmailConn{apiBase: base}, token, nil
}

// --- verb implementations ---

func (p *gmailPlugin) messages(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["q"]); v != "" {
		q.Set("q", v)
	}
	for _, id := range strList(o["labelIds"]) {
		q.Add("labelIds", id)
	}
	if v := intStr(o["maxResults"]); v != "" {
		q.Set("maxResults", v)
	}
	if v := str(o["pageToken"]); v != "" {
		q.Set("pageToken", v)
	}
	return p.collection(conn, token, http.MethodGet, "/messages", q, nil, "messages")
}

func (p *gmailPlugin) messageGet(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["message_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "message_id is required")
	}
	q := url.Values{}
	if v := str(o["format"]); v != "" {
		q.Set("format", v)
	}
	return p.single(conn, token, http.MethodGet, "/messages/"+url.PathEscape(id), q, nil)
}

func (p *gmailPlugin) send(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	raw, err := buildRawMessage(o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	return p.single(conn, token, http.MethodPost, "/messages/send", nil, map[string]any{"raw": raw})
}

func (p *gmailPlugin) labels(conn gmailConn, token string) (plugin.InvokeResult, error) {
	return p.collection(conn, token, http.MethodGet, "/labels", nil, nil, "labels")
}

func (p *gmailPlugin) labelCreate(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	name := str(o["name"])
	if name == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "name is required")
	}
	body := map[string]any{"name": name}
	if v := str(o["labelListVisibility"]); v != "" {
		body["labelListVisibility"] = v
	}
	if v := str(o["messageListVisibility"]); v != "" {
		body["messageListVisibility"] = v
	}
	return p.single(conn, token, http.MethodPost, "/labels", nil, body)
}

func (p *gmailPlugin) drafts(conn gmailConn, token string) (plugin.InvokeResult, error) {
	return p.collection(conn, token, http.MethodGet, "/drafts", nil, nil, "drafts")
}

func (p *gmailPlugin) draftCreate(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	raw, err := buildRawMessage(o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	body := map[string]any{"message": map[string]any{"raw": raw}}
	return p.single(conn, token, http.MethodPost, "/drafts", nil, body)
}

func (p *gmailPlugin) threads(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["q"]); v != "" {
		q.Set("q", v)
	}
	return p.collection(conn, token, http.MethodGet, "/threads", q, nil, "threads")
}

func (p *gmailPlugin) threadGet(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["thread_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "thread_id is required")
	}
	return p.single(conn, token, http.MethodGet, "/threads/"+url.PathEscape(id), nil, nil)
}

func (p *gmailPlugin) modify(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["message_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "message_id is required")
	}
	body := map[string]any{}
	if add := strList(o["add_labels"]); len(add) > 0 {
		body["addLabelIds"] = add
	}
	if rm := strList(o["remove_labels"]); len(rm) > 0 {
		body["removeLabelIds"] = rm
	}
	return p.single(conn, token, http.MethodPost, "/messages/"+url.PathEscape(id)+"/modify", nil, body)
}

func (p *gmailPlugin) trash(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["message_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "message_id is required")
	}
	return p.single(conn, token, http.MethodPost, "/messages/"+url.PathEscape(id)+"/trash", nil, nil)
}

func (p *gmailPlugin) untrash(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["message_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "message_id is required")
	}
	return p.single(conn, token, http.MethodPost, "/messages/"+url.PathEscape(id)+"/untrash", nil, nil)
}

func (p *gmailPlugin) api(conn gmailConn, token string, o map[string]any) (plugin.InvokeResult, error) {
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

// collection performs one request against the Gmail API and hoists the
// envelope's named list (e.g. {"messages": [...]}) into `items`.
func (p *gmailPlugin) collection(conn gmailConn, token, method, path string, query url.Values, body any, envelopeKey string) (plugin.InvokeResult, error) {
	status, respBody, err := p.do(token, method, conn.apiBase+path, query, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, envelopeKey)
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

// single performs one request against the Gmail API and returns the decoded
// body as `result` (nil-safe: a decoded nil is simply omitted).
func (p *gmailPlugin) single(conn gmailConn, token, method, path string, query url.Values, body any) (plugin.InvokeResult, error) {
	status, respBody, err := p.do(token, method, conn.apiBase+path, query, body)
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

// --- RFC 5322 message building (send, draft_create) ---

// buildRawMessage renders an RFC 5322 message from verb options — to*
// (list), cc, bcc, subject*, text, html, from — as multipart/alternative
// when both text and html are set, otherwise a single text/plain or
// text/html part, and returns it base64url-encoded (unpadded, per the Gmail
// API's `raw` field). It touches no network, so it is exercised directly by
// tests with no server involved.
func buildRawMessage(o map[string]any) (string, error) {
	to := strList(o["to"])
	if len(to) == 0 {
		return "", fmt.Errorf("to is required (list of recipients)")
	}
	subject := str(o["subject"])
	if subject == "" {
		return "", fmt.Errorf("subject is required")
	}
	cc := strList(o["cc"])
	bcc := strList(o["bcc"])
	text := str(o["text"])
	html := str(o["html"])
	from := str(o["from"])
	if text == "" && html == "" {
		return "", fmt.Errorf("text or html is required")
	}

	var buf bytes.Buffer
	if from != "" {
		buf.WriteString("From: " + from + "\r\n")
	}
	buf.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	if len(cc) > 0 {
		buf.WriteString("Cc: " + strings.Join(cc, ", ") + "\r\n")
	}
	if len(bcc) > 0 {
		buf.WriteString("Bcc: " + strings.Join(bcc, ", ") + "\r\n")
	}
	buf.WriteString("Subject: " + subject + "\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")
	switch {
	case text != "" && html != "":
		boundary := generateBoundary()
		buf.WriteString("Content-Type: multipart/alternative; boundary=" + boundary + "\r\n\r\n")
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(text + "\r\n\r\n")
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(html + "\r\n\r\n")
		buf.WriteString("--" + boundary + "--\r\n")
	case html != "":
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(html)
	default:
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(text)
	}
	return base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

func generateBoundary() string {
	return fmt.Sprintf("conductor-gmail-%d-%d", time.Now().UnixNano(), rand.Int63())
}

// --- HTTP plumbing ---

// do performs one HTTP request against the Gmail API, attaching the Bearer
// token, and returns the status code and raw response body. A non-2xx
// status is translated into a CodeInternalError carrying the status and
// body — callers never need to check status codes themselves.
func (p *gmailPlugin) do(token, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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

// hoist pulls the list out of a decoded Gmail API envelope: a map with the
// given key holding a list (e.g. {"messages": [...]}). A bare list is
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
	if err := plugin.Serve(newGmailPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-gmail:", err)
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
