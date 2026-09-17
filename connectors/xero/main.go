// Command conductor-xero is a verb-only conductor connector for Xero, the
// cloud accounting platform. It drives the Xero Accounting API
// (https://api.xero.com/api.xro/2.0) over net/http: organisation details,
// invoices, contacts, accounts, payments, bank transactions, items, and a
// raw `api` escape hatch for anything a first-class verb does not cover.
// Built ONLY against the public SDK (pkg/plugin) — no other dependency.
//
// Authentication is conductor's MANAGED OAuth2 (pkg/plugin v0.15.0+): this
// plugin does NOT perform any token exchange. It declares Xero's OAuth2
// endpoints in Describe().Auth, the operator supplies client credentials in
// the connector's daemon-side `auth:` block, and `conductor connector auth
// xero` runs the one-time login. The daemon then injects a fresh, rotated
// bearer token into every InvokeRequest.Connection under
// plugin.AccessTokenKey, read here with plugin.AccessToken. Because
// conductor — not this plugin — talks to identity.xero.com, the plugin's
// only egress is api.xero.com.
//
// Connection:
//
//	tenant_id: "<xero org id>"  # optional; sent as Xero-tenant-id. If omitted,
//	                            # the connector calls GET /connections and uses
//	                            # the first tenant, caching it for the process.
//	api_base: "https://..."    # optional; overrides https://api.xero.com (tests)
//
// Xero's Accounting API wraps collection responses under a PascalCase key,
// e.g. {"Invoices": [...]}. Collection verbs hoist that list into `items`;
// single-resource verbs (organisation, invoice_get, contact_get) hoist the
// same envelope and lift its first element into `result`. Every verb also
// returns `status_code`; a non-2xx response becomes a CodeInternalError
// carrying the status and body — nothing is swallowed.
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
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// defaultAPIBase is Xero's production API host. api_base overrides it for
// tests (both the Accounting API and /connections are derived from it).
const defaultAPIBase = "https://api.xero.com"

// accountingPath is the Xero Accounting API's fixed path prefix.
const accountingPath = "/api.xro/2.0"

type xeroPlugin struct {
	client *http.Client

	mu          sync.Mutex
	tenantCache map[string]string // apiBase+"|"+token -> resolved tenant id
}

func newXeroPlugin() *xeroPlugin {
	return &xeroPlugin{
		client:      &http.Client{Timeout: 30 * time.Second},
		tenantCache: map[string]string{},
	}
}

func (p *xeroPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "xero",
		Desc: "Xero: organisation, invoices, contacts, accounts, payments, bank transactions and items over the Xero Accounting API, plus a raw `api` escape hatch. Authenticates via conductor's managed OAuth2 — run `conductor connector auth xero` after configuring an `auth:` block; this plugin never talks to identity.xero.com itself.",
		Connection: plugin.Schema{
			"tenant_id": {Type: "string", Desc: "Xero organisation (tenant) id, sent as the Xero-tenant-id header. If omitted, the first tenant from GET /connections is used and cached for the process.", Scope: "tenant"},
			"api_base":  {Type: "string", Desc: "override https://api.xero.com (tests only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "connections", Desc: "list the tenants (organisations) this token is authorized for",
				Usage:   "GET https://api.xero.com/connections",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "organisation", Desc: "get the connected organisation's details",
				Usage:   "GET /api.xro/2.0/Organisation",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "invoices", Desc: "list invoices",
				Usage: "GET /api.xro/2.0/Invoices",
				Options: plugin.Schema{
					"where":    {Type: "string", Desc: "Xero where-clause filter expression"},
					"order":    {Type: "string", Desc: "sort expression, e.g. InvoiceNumber DESC"},
					"page":     {Type: "integer", Desc: "page number (1-based)"},
					"statuses": {Type: "list", Desc: "filter by Status, e.g. [\"AUTHORISED\", \"PAID\"]"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "invoice_get", Desc: "get one invoice",
				Usage: "GET /api.xro/2.0/Invoices/{id}",
				Options: plugin.Schema{
					"invoice_id": {Type: "string", Required: true, Scope: "invoice"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "contacts", Desc: "list contacts",
				Usage:   "GET /api.xro/2.0/Contacts",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "contact_get", Desc: "get one contact",
				Usage: "GET /api.xro/2.0/Contacts/{id}",
				Options: plugin.Schema{
					"contact_id": {Type: "string", Required: true, Scope: "contact"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "accounts", Desc: "list chart-of-accounts accounts",
				Usage:   "GET /api.xro/2.0/Accounts",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "payments", Desc: "list payments",
				Usage:   "GET /api.xro/2.0/Payments",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "bank_transactions", Desc: "list bank transactions",
				Usage:   "GET /api.xro/2.0/BankTransactions",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "items", Desc: "list inventory items",
				Usage:   "GET /api.xro/2.0/Items",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "api", Desc: "raw escape hatch: any Xero Accounting API endpoint (enables writes)",
				Usage: "method + path under /api.xro/2.0, for anything without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Desc: "HTTP method (default GET)"},
					"path":   {Type: "string", Required: true, Desc: "path under /api.xro/2.0, e.g. /Invoices"},
					"query":  {Type: "map", Desc: "query string parameters"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
		},
		// Conductor performs the OAuth2 token exchange with identity.xero.com on
		// this plugin's behalf; the plugin itself only ever calls api.xero.com.
		Capabilities: plugin.Capabilities{Egress: []string{"api.xero.com:443"}},
		Auth: &plugin.AuthSpec{
			Grants:   []string{"authorization_code", "refresh_token", "client_credentials"},
			TokenURL: "https://identity.xero.com/connect/token",
			AuthURL:  "https://login.xero.com/identity/connect/authorize",
			Scopes:   []string{"accounting.transactions", "accounting.contacts", "accounting.settings", "offline_access"},
		},
	}
}

func (p *xeroPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, token, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "connections":
		return p.connections(conn, token)
	case "organisation":
		return p.organisation(conn, token)
	case "invoices":
		return p.invoices(conn, token, o)
	case "invoice_get":
		return p.invoiceGet(conn, token, o)
	case "contacts":
		return p.contacts(conn, token)
	case "contact_get":
		return p.contactGet(conn, token, o)
	case "accounts":
		return p.accounts(conn, token)
	case "payments":
		return p.payments(conn, token)
	case "bank_transactions":
		return p.bankTransactions(conn, token)
	case "items":
		return p.items(conn, token)
	case "api":
		return p.api(conn, token, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type xeroConn struct {
	apiBase  string
	tenantID string // explicit override; empty means "resolve via /connections"
}

// parseConn reads the connection map and the managed-OAuth2 bearer token
// injected by the daemon. A missing token means the operator has not
// configured an `auth:` block and logged in yet.
func parseConn(m map[string]any) (xeroConn, string, error) {
	token := plugin.AccessToken(m)
	if token == "" {
		return xeroConn{}, "", fmt.Errorf("no access token — add an auth: block and run `conductor connector auth xero`")
	}
	base := strOr(str(m["api_base"]), defaultAPIBase)
	base = strings.TrimRight(base, "/")
	return xeroConn{apiBase: base, tenantID: str(m["tenant_id"])}, token, nil
}

// resolveTenant returns the Xero tenant id to send as Xero-tenant-id: the
// connection's explicit tenant_id if set, otherwise the first tenant from
// GET /connections, cached per (api_base, token) for the life of the process.
func (p *xeroPlugin) resolveTenant(conn xeroConn, token string) (string, error) {
	if conn.tenantID != "" {
		return conn.tenantID, nil
	}

	key := conn.apiBase + "|" + token
	p.mu.Lock()
	if t, ok := p.tenantCache[key]; ok {
		p.mu.Unlock()
		return t, nil
	}
	p.mu.Unlock()

	_, body, err := p.do(token, http.MethodGet, conn.apiBase+"/connections", nil, nil, "")
	if err != nil {
		return "", err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return "", plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	list, _ := decoded.([]any)
	if len(list) == 0 {
		return "", plugin.Errorf(plugin.CodeInvalidParams, "no Xero tenant found; set tenant_id or connect an organisation via `conductor connector auth xero`")
	}
	first, _ := list[0].(map[string]any)
	tenantID := str(first["tenantId"])
	if tenantID == "" {
		return "", plugin.Errorf(plugin.CodeInternalError, "GET /connections response missing tenantId")
	}

	p.mu.Lock()
	p.tenantCache[key] = tenantID
	p.mu.Unlock()
	return tenantID, nil
}

// --- verb implementations ---

func (p *xeroPlugin) connections(conn xeroConn, token string) (plugin.InvokeResult, error) {
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+"/connections", nil, nil, "")
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items, _ := decoded.([]any)
	if items == nil {
		items = []any{}
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *xeroPlugin) organisation(conn xeroConn, token string) (plugin.InvokeResult, error) {
	return p.accountingSingle(conn, token, "/Organisation", "Organisations")
}

func (p *xeroPlugin) invoices(conn xeroConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["where"]); v != "" {
		q.Set("where", v)
	}
	if v := str(o["order"]); v != "" {
		q.Set("order", v)
	}
	if v := intStr(o["page"]); v != "" {
		q.Set("page", v)
	}
	if statuses := strList(o["statuses"]); len(statuses) > 0 {
		q.Set("Statuses", strings.Join(statuses, ","))
	}
	return p.accountingCollection(conn, token, "/Invoices", "Invoices", q)
}

func (p *xeroPlugin) invoiceGet(conn xeroConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["invoice_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "invoice_id is required")
	}
	return p.accountingSingle(conn, token, "/Invoices/"+url.PathEscape(id), "Invoices")
}

func (p *xeroPlugin) contacts(conn xeroConn, token string) (plugin.InvokeResult, error) {
	return p.accountingCollection(conn, token, "/Contacts", "Contacts", nil)
}

func (p *xeroPlugin) contactGet(conn xeroConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["contact_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "contact_id is required")
	}
	return p.accountingSingle(conn, token, "/Contacts/"+url.PathEscape(id), "Contacts")
}

func (p *xeroPlugin) accounts(conn xeroConn, token string) (plugin.InvokeResult, error) {
	return p.accountingCollection(conn, token, "/Accounts", "Accounts", nil)
}

func (p *xeroPlugin) payments(conn xeroConn, token string) (plugin.InvokeResult, error) {
	return p.accountingCollection(conn, token, "/Payments", "Payments", nil)
}

func (p *xeroPlugin) bankTransactions(conn xeroConn, token string) (plugin.InvokeResult, error) {
	return p.accountingCollection(conn, token, "/BankTransactions", "BankTransactions", nil)
}

func (p *xeroPlugin) items(conn xeroConn, token string) (plugin.InvokeResult, error) {
	return p.accountingCollection(conn, token, "/Items", "Items", nil)
}

func (p *xeroPlugin) api(conn xeroConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	path := str(o["path"])
	if path == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "path is required")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	method := strOr(str(o["method"]), http.MethodGet)
	tenant, err := p.resolveTenant(conn, token)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	q := url.Values{}
	if m, ok := o["query"].(map[string]any); ok {
		for k, v := range m {
			q.Set(k, fmt.Sprintf("%v", v))
		}
	}
	status, body, err := p.do(token, method, conn.apiBase+accountingPath+path, q, o["body"], tenant)
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

// --- shared Accounting-API helpers ---

// accountingCollection GETs an Accounting API path, resolving the tenant
// first, and hoists the PascalCase envelope's list into `items`.
func (p *xeroPlugin) accountingCollection(conn xeroConn, token, path, envelopeKey string, query url.Values) (plugin.InvokeResult, error) {
	tenant, err := p.resolveTenant(conn, token)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+accountingPath+path, query, nil, tenant)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, envelopeKey)
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

// accountingSingle GETs an Accounting API path, resolving the tenant first,
// and lifts the first element of the PascalCase envelope's list into
// `result` (Xero wraps even single-resource responses in a one-element list).
func (p *xeroPlugin) accountingSingle(conn xeroConn, token, path, envelopeKey string) (plugin.InvokeResult, error) {
	tenant, err := p.resolveTenant(conn, token)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	status, body, err := p.do(token, http.MethodGet, conn.apiBase+accountingPath+path, nil, nil, tenant)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	items := hoist(decoded, envelopeKey)
	var result any
	if len(items) > 0 {
		result = items[0]
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": result, "status_code": status}}, nil
}

// --- HTTP plumbing ---

// do performs one HTTP request against the Xero API, attaching the Bearer
// token and (when non-empty) the Xero-tenant-id header, and returns the
// status code and raw response body. A non-2xx status is translated into a
// CodeInternalError carrying the status and body — callers never need to
// check status codes themselves.
func (p *xeroPlugin) do(token, method, endpoint string, query url.Values, body any, tenant string) (int, []byte, error) {
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
	if tenant != "" {
		req.Header.Set("Xero-tenant-id", tenant)
	}
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

// hoist pulls the list out of a decoded Xero Accounting API envelope: a map
// with the given PascalCase key holding a list (e.g. {"Invoices": [...]}).
// A bare list is returned as-is. Anything else yields an empty (never nil)
// list, so callers get a consistent [] rather than null on the wire.
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
	if err := plugin.Serve(newXeroPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-xero:", err)
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
