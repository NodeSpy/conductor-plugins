// Command conductor-aws-ses is a verb-only conductor connector (#59) for
// Amazon SES v2 (Simple Email Service — email API): sending simple and
// templated email, managing email identities, the account send quota, and
// the suppression list, plus a raw `api` escape hatch. It drives the SES v2
// REST API over net/http, authenticating every request with a hand-rolled
// AWS Signature Version 4 (SigV4) — no AWS SDK, no AWS CLI. Built ONLY
// against the public SDK (pkg/plugin) and the standard library.
//
// Connection:
//
//	access_key_id:     "AKIA..."          # required
//	secret_access_key: "..."              # required
//	region:            "us-east-1"        # required
//	session_token:     "..."              # optional, for STS temporary creds
//	endpoint:          "https://..."      # optional override, default
//	                                       # https://email.{region}.amazonaws.com
//	                                       # (mainly for pointing tests at an
//	                                       # httptest.Server)
//
// Every request is signed with AWS SigV4 for service "ses" in the configured
// region: a canonical request is built from the method, path, query string,
// and a fixed set of headers (Host, X-Amz-Date, X-Amz-Content-Sha256, and
// X-Amz-Security-Token when a session token is set), hashed, turned into a
// string-to-sign, and signed with the HMAC-SHA256 key-derivation chain
// (kDate -> kRegion -> kService -> kSigning). See signV4 and its helpers
// below — they are pure functions, unit-tested directly against the AWS
// documented "get-vanilla" SigV4 test vector (real access key, real secret
// key, real expected canonical request / string-to-sign / signature), not
// merely self-consistency. Signing needs a timestamp; nowFunc is an
// unexported var (default time.Now) so tests can inject a fixed time and get
// a deterministic signature.
//
// A non-2xx response is returned as a CodeInternalError carrying the status
// code and the AWS error body — nothing is swallowed.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// nowFunc returns the current time used for SigV4 signing. It is a var
// (rather than a bare time.Now() call) purely so tests can inject a fixed
// time and get a deterministic signature.
var nowFunc = time.Now

type sesPlugin struct {
	client *http.Client
}

func newSESPlugin() *sesPlugin {
	return &sesPlugin{client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *sesPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "aws-ses",
		Desc: "Amazon SES v2 (email): send simple/templated email, manage email identities, read the account send quota, and manage the suppression list, plus a raw `api` escape hatch. Every request is authenticated with a hand-rolled AWS SigV4 signature (no AWS SDK, no AWS CLI).",
		Connection: plugin.Schema{
			"access_key_id":     {Type: "string", Required: true, Desc: "AWS access key ID"},
			"secret_access_key": {Type: "string", Required: true, Desc: "AWS secret access key"},
			"region":            {Type: "string", Required: true, Desc: "AWS region, e.g. us-east-1"},
			"session_token":     {Type: "string", Desc: "AWS STS session token, for temporary credentials"},
			"endpoint":          {Type: "string", Desc: "override the default SES v2 endpoint https://email.{region}.amazonaws.com (mainly for tests)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "send_email", Desc: "send a simple (non-template) email",
				Usage: "POST /v2/email/outbound-emails",
				Options: plugin.Schema{
					"from":     {Type: "string", Required: true, Desc: "FromEmailAddress"},
					"to":       {Type: "list", Required: true, Desc: "Destination.ToAddresses"},
					"cc":       {Type: "list", Desc: "Destination.CcAddresses"},
					"bcc":      {Type: "list", Desc: "Destination.BccAddresses"},
					"subject":  {Type: "string", Required: true, Desc: "Content.Simple.Subject.Data"},
					"text":     {Type: "string", Desc: "Content.Simple.Body.Text.Data (text or html required)"},
					"html":     {Type: "string", Desc: "Content.Simple.Body.Html.Data (text or html required)"},
					"reply_to": {Type: "list", Desc: "ReplyToAddresses"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "send_templated_email", Desc: "send an email rendered from an SES template",
				Usage: "POST /v2/email/outbound-emails (Content.Template)",
				Options: plugin.Schema{
					"from":          {Type: "string", Required: true, Desc: "FromEmailAddress"},
					"to":            {Type: "list", Required: true, Desc: "Destination.ToAddresses"},
					"cc":            {Type: "list", Desc: "Destination.CcAddresses"},
					"bcc":           {Type: "list", Desc: "Destination.BccAddresses"},
					"template_name": {Type: "string", Required: true, Desc: "Content.Template.TemplateName"},
					"template_data": {Type: "any", Desc: "Content.Template.TemplateData: a JSON object (encoded to a JSON string) or a JSON string, used as-is"},
					"reply_to":      {Type: "list", Desc: "ReplyToAddresses"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "identities", Desc: "list email identities",
				Usage:   "GET /v2/email/identities",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "identity_get", Desc: "get one email identity's details",
				Usage: "GET /v2/email/identities/{EmailIdentity}",
				Options: plugin.Schema{
					"email_identity": {Type: "string", Required: true, Scope: "identity"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "create_identity", Desc: "create (start verifying) an email identity",
				Usage: "POST /v2/email/identities",
				Options: plugin.Schema{
					"email_identity": {Type: "string", Required: true, Scope: "identity"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "get_send_quota", Desc: "the account's sending quota and stats",
				Usage:   "GET /v2/email/account",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "suppressed_list", Desc: "list the account-level suppressed destinations",
				Usage:   "GET /v2/email/suppression/addresses",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "suppress", Desc: "add an address to the account-level suppression list",
				Usage: "PUT /v2/email/suppression/addresses/{email}",
				Options: plugin.Schema{
					"email":  {Type: "string", Required: true, Scope: "email"},
					"reason": {Type: "string", Required: true, Enum: []string{"BOUNCE", "COMPLAINT"}},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "unsuppress", Desc: "remove an address from the account-level suppression list",
				Usage: "DELETE /v2/email/suppression/addresses/{email}",
				Options: plugin.Schema{
					"email": {Type: "string", Required: true, Scope: "email"},
				},
				Outputs: plugin.Schema{"status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any SES v2 endpoint",
				Usage: "method + path under the SES v2 endpoint, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under the SES v2 endpoint, e.g. /v2/email/identities"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// SES v2 is reached at https://email.{region}.amazonaws.com — every
		// region's host matches email.*.amazonaws.com.
		Capabilities: plugin.Capabilities{Egress: []string{"email.*.amazonaws.com:443"}},
	}
}

func (p *sesPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "send_email":
		return p.sendEmail(conn, o)
	case "send_templated_email":
		return p.sendTemplatedEmail(conn, o)
	case "identities":
		return p.identities(conn)
	case "identity_get":
		return p.identityGet(conn, o)
	case "create_identity":
		return p.createIdentity(conn, o)
	case "get_send_quota":
		return p.getSendQuota(conn)
	case "suppressed_list":
		return p.suppressedList(conn)
	case "suppress":
		return p.suppress(conn, o)
	case "unsuppress":
		return p.unsuppress(conn, o)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type sesConn struct {
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
	region          string
	endpoint        string // e.g. https://email.us-east-1.amazonaws.com, no trailing slash
	host            string // endpoint's host[:port], used as the SigV4 Host
}

func parseConn(m map[string]any) (sesConn, error) {
	ak := str(m["access_key_id"])
	if ak == "" {
		return sesConn{}, fmt.Errorf("access_key_id is required")
	}
	sk := str(m["secret_access_key"])
	if sk == "" {
		return sesConn{}, fmt.Errorf("secret_access_key is required")
	}
	region := str(m["region"])
	if region == "" {
		return sesConn{}, fmt.Errorf("region is required")
	}
	endpoint := str(m["endpoint"])
	if endpoint == "" {
		endpoint = "https://email." + region + ".amazonaws.com"
	}
	endpoint = strings.TrimRight(endpoint, "/")
	u, err := url.Parse(endpoint)
	if err != nil {
		return sesConn{}, fmt.Errorf("invalid endpoint: %w", err)
	}
	return sesConn{
		accessKeyID:     ak,
		secretAccessKey: sk,
		region:          region,
		sessionToken:    str(m["session_token"]),
		endpoint:        endpoint,
		host:            u.Host,
	}, nil
}

// --- verb implementations ---

func (p *sesPlugin) sendEmail(conn sesConn, o map[string]any) (plugin.InvokeResult, error) {
	from := str(o["from"])
	if from == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "from is required")
	}
	to := strList(o["to"])
	if len(to) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "to is required")
	}
	subject := str(o["subject"])
	if subject == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "subject is required")
	}
	text := str(o["text"])
	html := str(o["html"])
	if text == "" && html == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "text or html is required")
	}
	cc := strList(o["cc"])
	bcc := strList(o["bcc"])
	replyTo := strList(o["reply_to"])

	dest := map[string]any{"ToAddresses": to}
	if len(cc) > 0 {
		dest["CcAddresses"] = cc
	}
	if len(bcc) > 0 {
		dest["BccAddresses"] = bcc
	}

	payload := map[string]any{
		"FromEmailAddress": from,
		"Destination":      dest,
		"Content":          simpleContent(subject, text, html),
	}
	if len(replyTo) > 0 {
		payload["ReplyToAddresses"] = replyTo
	}

	status, body, err := p.do(conn, http.MethodPost, "/v2/email/outbound-emails", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

// simpleContent builds the Content.Simple shape shared by send_email: a
// Subject plus whichever of Text/Html bodies were supplied.
func simpleContent(subject, text, html string) map[string]any {
	body := map[string]any{}
	if text != "" {
		body["Text"] = map[string]any{"Data": text}
	}
	if html != "" {
		body["Html"] = map[string]any{"Data": html}
	}
	return map[string]any{"Simple": map[string]any{
		"Subject": map[string]any{"Data": subject},
		"Body":    body,
	}}
}

func (p *sesPlugin) sendTemplatedEmail(conn sesConn, o map[string]any) (plugin.InvokeResult, error) {
	from := str(o["from"])
	if from == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "from is required")
	}
	to := strList(o["to"])
	if len(to) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "to is required")
	}
	templateName := str(o["template_name"])
	if templateName == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "template_name is required")
	}
	templateData, err := templateDataString(o["template_data"])
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "template_data: "+err.Error())
	}
	cc := strList(o["cc"])
	bcc := strList(o["bcc"])
	replyTo := strList(o["reply_to"])

	dest := map[string]any{"ToAddresses": to}
	if len(cc) > 0 {
		dest["CcAddresses"] = cc
	}
	if len(bcc) > 0 {
		dest["BccAddresses"] = bcc
	}

	payload := map[string]any{
		"FromEmailAddress": from,
		"Destination":      dest,
		"Content": map[string]any{"Template": map[string]any{
			"TemplateName": templateName,
			"TemplateData": templateData,
		}},
	}
	if len(replyTo) > 0 {
		payload["ReplyToAddresses"] = replyTo
	}

	status, body, err := p.do(conn, http.MethodPost, "/v2/email/outbound-emails", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

// templateDataString renders the template_data option into the JSON-string
// shape SES v2 requires for Content.Template.TemplateData: a map is encoded
// to a JSON string; a string is assumed to already be JSON and used as-is;
// nil/absent becomes "{}".
func templateDataString(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "{}", nil
	case string:
		if x == "" {
			return "{}", nil
		}
		return x, nil
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

func (p *sesPlugin) identities(conn sesConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, "/v2/email/identities", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "EmailIdentities")
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *sesPlugin) identityGet(conn sesConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["email_identity"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "email_identity is required")
	}
	status, body, err := p.do(conn, http.MethodGet, "/v2/email/identities/"+id, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *sesPlugin) createIdentity(conn sesConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["email_identity"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "email_identity is required")
	}
	status, body, err := p.do(conn, http.MethodPost, "/v2/email/identities", nil, map[string]any{"EmailIdentity": id})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *sesPlugin) getSendQuota(conn sesConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, "/v2/email/account", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *sesPlugin) suppressedList(conn sesConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, "/v2/email/suppression/addresses", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, "SuppressedDestinationSummaries")
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *sesPlugin) suppress(conn sesConn, o map[string]any) (plugin.InvokeResult, error) {
	email := str(o["email"])
	if email == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "email is required")
	}
	reason := str(o["reason"])
	if reason == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "reason is required")
	}
	status, body, err := p.do(conn, http.MethodPut, "/v2/email/suppression/addresses/"+email, nil, map[string]any{"Reason": reason})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *sesPlugin) unsuppress(conn sesConn, o map[string]any) (plugin.InvokeResult, error) {
	email := str(o["email"])
	if email == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "email is required")
	}
	status, body, err := p.do(conn, http.MethodDelete, "/v2/email/suppression/addresses/"+email, nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *sesPlugin) api(conn sesConn, o map[string]any) (plugin.InvokeResult, error) {
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
	status, body, err := p.do(conn, method, path, q, o["body"])
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

// do performs one SigV4-signed HTTP request against the SES v2 endpoint and
// returns the status code and raw response body. path is the RAW (unescaped)
// logical path — e.g. "/v2/email/identities/[email protected]" — do (via signV4)
// takes care of correctly percent-encoding it for both the outgoing request
// and the SigV4 canonical request. A non-2xx status is translated into a
// CodeInternalError carrying the status and body.
func (p *sesPlugin) do(conn sesConn, method, path string, query url.Values, body any) (int, []byte, error) {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return 0, nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
	}

	sig := signV4(sigV4Input{
		Method:          method,
		Host:            conn.host,
		Path:            path,
		Query:           query,
		Body:            raw,
		AccessKeyID:     conn.accessKeyID,
		SecretAccessKey: conn.secretAccessKey,
		SessionToken:    conn.sessionToken,
		Region:          conn.region,
		Service:         "ses",
		Time:            nowFunc(),
	})

	full := conn.endpoint + sig.EscapedPath
	if sig.CanonicalQueryString != "" {
		full += "?" + sig.CanonicalQueryString
	}
	var reader io.Reader
	if raw != nil {
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	for k, v := range sig.Headers {
		// "host" is carried by req.Host/req.URL (already correct, since we
		// derived conn.host from the same endpoint URL); Header.Set("Host",
		// ...) has no effect on the wire and would just be misleading here.
		if strings.EqualFold(k, "host") {
			continue
		}
		req.Header.Set(k, v)
	}
	if raw != nil {
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
			fmt.Sprintf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody))))
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
	if err := plugin.Serve(newSESPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-aws-ses:", err)
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

// =====================================================================
// AWS Signature Version 4 (SigV4) — hand-rolled, stdlib crypto only.
//
// Every function below is pure (no I/O, no globals besides nowFunc for the
// timestamp) so it can be unit-tested directly, including against the AWS
// documented "get-vanilla" SigV4 test vector — see main_test.go.
// =====================================================================

// sigV4Input is everything signV4 needs to sign one request.
type sigV4Input struct {
	Method          string
	Host            string
	Path            string // RAW (unescaped) path, e.g. "/v2/email/identities/[email protected]"
	Query           url.Values
	Body            []byte
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // optional; adds X-Amz-Security-Token
	Region          string
	Service         string
	Time            time.Time
}

// sigV4Output is what the caller needs to actually send the signed request:
// the (correctly percent-encoded) path and query string to build the request
// URL from, and the full set of headers to set (including Authorization).
type sigV4Output struct {
	EscapedPath          string
	CanonicalQueryString string
	Headers              map[string]string // lowercase header names -> values
}

// signV4 computes an AWS SigV4 signature for one request and returns
// everything needed to send it. It is the seam between the pure crypto
// helpers (canonicalRequestString, stringToSign, deriveSigningKey, ...) and
// the plugin's HTTP plumbing (do()).
func signV4(in sigV4Input) sigV4Output {
	amzDate := in.Time.UTC().Format("20060102T150405Z")
	dateStamp := in.Time.UTC().Format("20060102")
	payloadHash := sha256Hex(in.Body)

	// Single-encode for the actual request path; double-encode (AWS's
	// documented rule for every service except S3) for the canonical
	// request used in signing.
	escapedPath := awsURIEncode(in.Path, false)
	canonicalURI := awsURIEncode(escapedPath, false)
	canonicalQueryString := canonicalQuery(in.Query)

	headers := map[string]string{
		"host":                 in.Host,
		"x-amz-date":           amzDate,
		"x-amz-content-sha256": payloadHash,
	}
	if in.SessionToken != "" {
		headers["x-amz-security-token"] = in.SessionToken
	}

	canonicalHeadersStr, signedHeadersStr := canonicalHeaders(headers)
	cr := canonicalRequestString(in.Method, canonicalURI, canonicalQueryString, canonicalHeadersStr, signedHeadersStr, payloadHash)
	crHash := sha256Hex([]byte(cr))

	credentialScope := dateStamp + "/" + in.Region + "/" + in.Service + "/aws4_request"
	sts := stringToSign(amzDate, credentialScope, crHash)

	signingKey := deriveSigningKey(in.SecretAccessKey, dateStamp, in.Region, in.Service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(sts)))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		in.AccessKeyID, credentialScope, signedHeadersStr, signature)

	outHeaders := make(map[string]string, len(headers)+1)
	for k, v := range headers {
		outHeaders[k] = v
	}
	outHeaders["authorization"] = authHeader

	return sigV4Output{
		EscapedPath:          escapedPath,
		CanonicalQueryString: canonicalQueryString,
		Headers:              outHeaders,
	}
}

// canonicalRequestString builds the AWS SigV4 canonical request:
//
//	METHOD\nCanonicalURI\nCanonicalQueryString\nCanonicalHeaders\nSignedHeaders\nHashedPayload
//
// canonicalHeadersStr must already end in "\n" per header (canonicalHeaders
// below produces exactly that), which is what produces the blank line AWS's
// own examples show between the headers block and the signed-headers line.
func canonicalRequestString(method, canonicalURI, canonicalQueryString, canonicalHeadersStr, signedHeadersStr, payloadHash string) string {
	return strings.Join([]string{
		method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeadersStr,
		signedHeadersStr,
		payloadHash,
	}, "\n")
}

// stringToSign builds the AWS SigV4 string-to-sign.
func stringToSign(amzDate, credentialScope, hashedCanonicalRequest string) string {
	return strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hashedCanonicalRequest,
	}, "\n")
}

// deriveSigningKey computes the SigV4 key-derivation chain:
//
//	kDate    = HMAC-SHA256("AWS4" + secret, date)
//	kRegion  = HMAC-SHA256(kDate, region)
//	kService = HMAC-SHA256(kRegion, service)
//	kSigning = HMAC-SHA256(kService, "aws4_request")
func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// canonicalHeaders builds the CanonicalHeaders block and SignedHeaders list
// from a set of headers: names are lowercased and sorted, values trimmed.
// The returned canonicalHeadersStr ends with a trailing "\n" (one per
// header), matching AWS's own worked examples.
func canonicalHeaders(headers map[string]string) (canonicalHeadersStr, signedHeadersStr string) {
	keys := make([]string, 0, len(headers))
	normalized := make(map[string]string, len(headers))
	for k, v := range headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		normalized[lk] = strings.TrimSpace(v)
		keys = append(keys, lk)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(normalized[k])
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(keys, ";")
}

// canonicalQuery builds the AWS SigV4 canonical query string: each name and
// value URI-encoded individually (including '/'), sorted by key then value,
// joined with "&". An empty/nil Values yields "".
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(q))
	for _, k := range keys {
		vals := append([]string{}, q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsURIEncode(k, true)+"="+awsURIEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// awsURIEncode implements AWS's exact URI-encoding rule (which is stricter
// than, and must not be confused with, net/url's escaping): every byte is
// percent-encoded except the unreserved set 'A'-'Z' 'a'-'z' '0'-'9' '-' '_'
// '.' '~', using uppercase hex. When encodeSlash is false, '/' is also left
// unescaped (used for path segments — never for query names/values, where
// encodeSlash must be true).
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreservedByte(c) || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isUnreservedByte(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.' || c == '~'
}
