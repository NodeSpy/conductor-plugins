package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn builds a connection map with the managed-OAuth2 token already
// injected (as the daemon would do), pointed at srv for both the
// Accounting API and /connections.
func testConn(srv *httptest.Server, tenantID string) map[string]any {
	m := map[string]any{
		plugin.AccessTokenKey: "test-token",
		"api_base":            srv.URL,
	}
	if tenantID != "" {
		m["tenant_id"] = tenantID
	}
	return m
}

func checkBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization header: got %q want %q", got, "Bearer test-token")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestConnectionsVerb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/connections" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if got := r.Header.Get("Xero-tenant-id"); got != "" {
			t.Errorf("connections must not send Xero-tenant-id, got %q", got)
		}
		writeJSON(w, 200, []any{
			map[string]any{"id": "conn-1", "tenantId": "org-1", "tenantName": "Acme"},
		})
	}))
	defer srv.Close()

	p := newXeroPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "connections", Connection: testConn(srv, "")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	got := items[0].(map[string]any)
	if got["tenantId"] != "org-1" {
		t.Errorf("items[0]: %#v", got)
	}
}

func TestOrganisationSendsBearerAndTenantHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if got := r.Header.Get("Xero-tenant-id"); got != "org-1" {
			t.Errorf("Xero-tenant-id: got %q want %q", got, "org-1")
		}
		if r.URL.Path != "/api.xro/2.0/Organisation" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{
			"Organisations": []any{map[string]any{"Name": "Acme Ltd"}},
		})
	}))
	defer srv.Close()

	p := newXeroPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "organisation", Connection: testConn(srv, "org-1")})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["Name"] != "Acme Ltd" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvoicesHoistsPascalCaseEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if got := r.Header.Get("Xero-tenant-id"); got != "org-1" {
			t.Errorf("Xero-tenant-id: got %q want %q", got, "org-1")
		}
		if r.URL.Path != "/api.xro/2.0/Invoices" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("where") != `Status=="AUTHORISED"` || q.Get("order") != "InvoiceNumber DESC" || q.Get("page") != "2" || q.Get("Statuses") != "AUTHORISED,PAID" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{
			"Id":       "abc",
			"Invoices": []any{map[string]any{"InvoiceID": "inv-1"}},
		})
	}))
	defer srv.Close()

	p := newXeroPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "invoices", Connection: testConn(srv, "org-1"),
		Options: map[string]any{
			"where": `Status=="AUTHORISED"`, "order": "InvoiceNumber DESC", "page": 2,
			"statuses": []any{"AUTHORISED", "PAID"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["InvoiceID"] != "inv-1" {
		t.Fatalf("items (hoisted from Invoices): %#v", res.Outputs["items"])
	}
}

func TestInvoiceGetLiftsFirstElementIntoResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api.xro/2.0/Invoices/inv-1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"Invoices": []any{map[string]any{"InvoiceID": "inv-1", "Total": 42.5}}})
	}))
	defer srv.Close()

	p := newXeroPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "invoice_get", Connection: testConn(srv, "org-1"),
		Options: map[string]any{"invoice_id": "inv-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["InvoiceID"] != "inv-1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestContactsAndContactGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch r.URL.Path {
		case "/api.xro/2.0/Contacts":
			writeJSON(w, 200, map[string]any{"Contacts": []any{map[string]any{"ContactID": "c1"}}})
		case "/api.xro/2.0/Contacts/c1":
			writeJSON(w, 200, map[string]any{"Contacts": []any{map[string]any{"ContactID": "c1", "Name": "Bob"}}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newXeroPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "contacts", Connection: testConn(srv, "org-1")})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["ContactID"] != "c1" {
		t.Fatalf("contacts items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "contact_get", Connection: testConn(srv, "org-1"),
		Options: map[string]any{"contact_id": "c1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["Name"] != "Bob" {
		t.Fatalf("contact_get result: %#v", res.Outputs["result"])
	}
}

func TestAccountsPaymentsBankTransactionsItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch r.URL.Path {
		case "/api.xro/2.0/Accounts":
			writeJSON(w, 200, map[string]any{"Accounts": []any{map[string]any{"AccountID": "a1"}}})
		case "/api.xro/2.0/Payments":
			writeJSON(w, 200, map[string]any{"Payments": []any{map[string]any{"PaymentID": "p1"}}})
		case "/api.xro/2.0/BankTransactions":
			writeJSON(w, 200, map[string]any{"BankTransactions": []any{map[string]any{"BankTransactionID": "b1"}}})
		case "/api.xro/2.0/Items":
			writeJSON(w, 200, map[string]any{"Items": []any{map[string]any{"ItemID": "i1"}}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newXeroPlugin()
	cases := []struct {
		verb string
		key  string
		id   string
	}{
		{"accounts", "AccountID", "a1"},
		{"payments", "PaymentID", "p1"},
		{"bank_transactions", "BankTransactionID", "b1"},
		{"items", "ItemID", "i1"},
	}
	for _, tc := range cases {
		res, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: testConn(srv, "org-1")})
		if err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		items, ok := res.Outputs["items"].([]any)
		if !ok || len(items) != 1 || items[0].(map[string]any)[tc.key] != tc.id {
			t.Fatalf("%s items: %#v", tc.verb, res.Outputs["items"])
		}
	}
}

// TestTenantAutoResolution covers the no-tenant_id path: the connector calls
// GET /connections (no tenant header) to discover the tenant, then uses it
// on the subsequent Accounting API call. Both requests land on the same
// api_base-derived httptest server.
func TestTenantAutoResolution(t *testing.T) {
	var sawTenantHeaderOnAccounts string
	var connectionsCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch r.URL.Path {
		case "/connections":
			connectionsCalls++
			if got := r.Header.Get("Xero-tenant-id"); got != "" {
				t.Errorf("connections must not send Xero-tenant-id, got %q", got)
			}
			writeJSON(w, 200, []any{map[string]any{"tenantId": "auto-org", "tenantName": "Auto Inc"}})
		case "/api.xro/2.0/Accounts":
			sawTenantHeaderOnAccounts = r.Header.Get("Xero-tenant-id")
			writeJSON(w, 200, map[string]any{"Accounts": []any{map[string]any{"AccountID": "a1"}}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newXeroPlugin()
	// No tenant_id set: must auto-resolve via /connections.
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "accounts", Connection: testConn(srv, "")})
	if err != nil {
		t.Fatal(err)
	}
	if sawTenantHeaderOnAccounts != "auto-org" {
		t.Fatalf("Accounts call Xero-tenant-id: got %q want %q", sawTenantHeaderOnAccounts, "auto-org")
	}
	if _, ok := res.Outputs["items"].([]any); !ok {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}

	// A second call for the same connection must use the cached tenant,
	// not call /connections again.
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "accounts", Connection: testConn(srv, "")}); err != nil {
		t.Fatal(err)
	}
	if connectionsCalls != 1 {
		t.Errorf("expected /connections to be called once (cached thereafter), got %d", connectionsCalls)
	}
}

func TestNoTenantFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		writeJSON(w, 200, []any{})
	}))
	defer srv.Close()

	p := newXeroPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "accounts", Connection: testConn(srv, "")})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams when no tenant found, got %v", err)
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api.xro/2.0/Invoices" && r.URL.Query().Get("page") == "3":
			writeJSON(w, 200, map[string]any{"Invoices": []any{map[string]any{"InvoiceID": "inv-9"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api.xro/2.0/Contacts":
			writeJSON(w, 200, map[string]any{"Contacts": []any{map[string]any{"ContactID": "new-c"}}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newXeroPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv, "org-1"),
		Options: map[string]any{"path": "/Invoices", "query": map[string]any{"page": "3"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["Invoices"]; !ok {
		t.Fatalf("result missing Invoices: %#v", result)
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv, "org-1"),
		Options: map[string]any{"method": "POST", "path": "Contacts", "body": map[string]any{"Name": "New Co"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatchArrayResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api.xro/2.0/Currencies" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{map[string]any{"Code": "USD"}})
	}))
	defer srv.Close()

	p := newXeroPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv, "org-1"),
		Options: map[string]any{"path": "/Currencies"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"Detail":"TokenExpired"}`))
	}))
	defer srv.Close()

	p := newXeroPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "organisation", Connection: testConn(srv, "org-1")})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok {
		t.Fatalf("expected *plugin.Error, got %T: %v", err, err)
	}
	if pe.Code != plugin.CodeInternalError {
		t.Errorf("code: got %d want %d", pe.Code, plugin.CodeInternalError)
	}
	if !containsAll(pe.Message, "401", "TokenExpired") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingAccessTokenIsInvalidParams(t *testing.T) {
	p := newXeroPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "organisation",
		Connection: map[string]any{"tenant_id": "org-1"}, // no access_token injected
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing access token, got %v", err)
	}
	if !containsAll(pe.Message, "access token", "conductor connector auth xero") {
		t.Errorf("message should point at the auth: block / login flow, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newXeroPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t", "tenant_id": "org-1"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"invoice_get", map[string]any{}},
		{"contact_get", map[string]any{}},
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s: expected error for missing required options", tc.verb)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", tc.verb, err)
		}
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newXeroPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{plugin.AccessTokenKey: "t"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for unknown verb, got %v", err)
	}
}

func TestDescribe(t *testing.T) {
	p := newXeroPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "xero" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["tenant_id"].Type != "string" || d.Connection["tenant_id"].Required {
		t.Errorf("connection.tenant_id: %#v", d.Connection["tenant_id"])
	}
	if d.Connection["api_base"].Type != "string" || d.Connection["api_base"].Required {
		t.Errorf("connection.api_base: %#v", d.Connection["api_base"])
	}

	want := []string{
		"connections", "organisation", "invoices", "invoice_get", "contacts",
		"contact_get", "accounts", "payments", "bank_transactions", "items", "api",
	}
	got := map[string]bool{}
	for _, v := range d.Verbs {
		got[v.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Verbs) != len(want) {
		t.Errorf("verb count: got %d want %d", len(d.Verbs), len(want))
	}

	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "api.xero.com:443" {
		t.Errorf("egress: got %v want [api.xero.com:443]", d.Capabilities.Egress)
	}

	if d.Auth == nil {
		t.Fatal("expected Describe().Auth to be non-nil (managed OAuth2)")
	}
	if d.Auth.TokenURL != "https://identity.xero.com/connect/token" {
		t.Errorf("Auth.TokenURL: got %q", d.Auth.TokenURL)
	}
	if d.Auth.AuthURL != "https://login.xero.com/identity/connect/authorize" {
		t.Errorf("Auth.AuthURL: got %q", d.Auth.AuthURL)
	}
	foundOfflineAccess := false
	for _, s := range d.Auth.Scopes {
		if s == "offline_access" {
			foundOfflineAccess = true
		}
	}
	if !foundOfflineAccess {
		t.Errorf("Auth.Scopes missing offline_access: %v", d.Auth.Scopes)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (sub == "" || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
