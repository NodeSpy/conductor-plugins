package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// fakeKeycloak is an httptest-backed stand-in for a Keycloak server: it
// serves the realm's OAuth2 client_credentials token endpoint and, behind an
// overridable admin handler, the admin REST API — both on the same host, as
// real Keycloak does.
type fakeKeycloak struct {
	srv *httptest.Server

	tokenFetches  int32
	adminRequests int32  // every request that reaches the admin mux entry, 401s included
	tokenRealm    string // expected {auth_realm} path segment
	current       string // the access_token value currently considered valid
	next          int    // counter used to mint distinct token values
	revoked       map[string]bool

	admin func(w http.ResponseWriter, r *http.Request) // handles everything under /admin/realms/...
}

func newFakeKeycloak(t *testing.T, tokenRealm string) *fakeKeycloak {
	t.Helper()
	f := &fakeKeycloak{tokenRealm: tokenRealm, revoked: map[string]bool{}}
	mux := http.NewServeMux()
	tokenPath := "/realms/" + tokenRealm + "/protocol/openid-connect/token"
	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("token endpoint: unexpected method %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("token endpoint: Content-Type = %q", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("token endpoint: parse form: %v", err)
		}
		if r.PostForm.Get("grant_type") != "client_credentials" {
			t.Errorf("token endpoint: grant_type = %q", r.PostForm.Get("grant_type"))
		}
		if r.PostForm.Get("client_id") != "svc" {
			t.Errorf("token endpoint: client_id = %q", r.PostForm.Get("client_id"))
		}
		if r.PostForm.Get("client_secret") != "shh" {
			t.Errorf("token endpoint: client_secret = %q", r.PostForm.Get("client_secret"))
		}
		atomic.AddInt32(&f.tokenFetches, 1)
		f.next++
		f.current = fmt.Sprintf("tok-%d", f.next)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": f.current,
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("/admin/realms/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.adminRequests, 1)
		auth := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(auth, "Bearer ")
		if tok == "" || tok != f.current || f.revoked[tok] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.admin == nil {
			t.Fatalf("unexpected admin request with no handler set: %s %s", r.Method, r.URL.Path)
			return
		}
		f.admin(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// revokeCurrent marks the currently-issued token as no-longer-valid, so the
// NEXT admin request bearing it gets a 401 — simulating server-side
// revocation ahead of the token's stated expiry.
func (f *fakeKeycloak) revokeCurrent() { f.revoked[f.current] = true }

func (f *fakeKeycloak) conn(overrides map[string]any) map[string]any {
	m := map[string]any{
		"base_url":      f.srv.URL,
		"client_id":     "svc",
		"client_secret": "shh",
	}
	for k, v := range overrides {
		m[k] = v
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- token fetch + cache ---

func TestTokenFetchedOnceAndReused(t *testing.T) {
	f := newFakeKeycloak(t, "master")
	var calls int32
	f.admin = func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeJSON(w, 200, []any{})
	}
	p := newKeycloakPlugin()

	for i := 0; i < 3; i++ {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "users", Connection: f.conn(nil)})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&f.tokenFetches); got != 1 {
		t.Fatalf("expected 1 token fetch across repeated calls, got %d", got)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 admin calls, got %d", got)
	}
}

func TestAuthRealmDefaultsToRealm(t *testing.T) {
	f := newFakeKeycloak(t, "corp") // auth_realm defaults to realm, so token must be fetched from /realms/corp/...
	f.admin = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, []any{}) }
	p := newKeycloakPlugin()

	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "users", Connection: f.conn(map[string]any{"realm": "corp"}),
	})
	if err != nil {
		t.Fatalf("token realm should default to realm: %v", err)
	}
}

func TestAuthRealmOverride(t *testing.T) {
	f := newFakeKeycloak(t, "auth-only") // client authenticates against a different realm than it operates on
	f.admin = func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/admin/realms/corp/") {
			t.Errorf("admin path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{})
	}
	p := newKeycloakPlugin()

	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "users", Connection: f.conn(map[string]any{"realm": "corp", "auth_realm": "auth-only"}),
	})
	if err != nil {
		t.Fatalf("auth_realm override: %v", err)
	}
}

// --- 401 -> refresh + retry ---

func TestAdminRefreshesAndRetriesOn401(t *testing.T) {
	f := newFakeKeycloak(t, "master")
	var calls int32
	f.admin = func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeJSON(w, 200, []any{})
	}
	p := newKeycloakPlugin()
	conn := f.conn(nil)

	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "users", Connection: conn}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := atomic.LoadInt32(&f.tokenFetches); got != 1 {
		t.Fatalf("expected 1 token fetch, got %d", got)
	}

	// Simulate the cached token being revoked server-side before its stated
	// expiry: the next request must transparently refresh and retry once,
	// rather than surfacing the 401.
	f.revokeCurrent()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "users", Connection: conn}); err != nil {
		t.Fatalf("second call (after revocation): %v", err)
	}
	if got := atomic.LoadInt32(&f.tokenFetches); got != 2 {
		t.Fatalf("expected a refresh fetch after 401, got %d total fetches", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		// call 1 (success) + call 2's retry (success) reach f.admin; call 2's
		// FIRST attempt 401s before f.admin is invoked (see adminRequests).
		t.Fatalf("expected 2 successful admin calls, got %d", got)
	}
	if got := atomic.LoadInt32(&f.adminRequests); got != 3 {
		// call 1 (success) + call 2 (401) + call 2 retry (success) = 3 requests
		// reaching the admin mux entry.
		t.Fatalf("expected 3 admin requests total (including the 401), got %d", got)
	}
}

// --- users: list hoisted to items ---

func TestUsersListHoistsItems(t *testing.T) {
	f := newFakeKeycloak(t, "master")
	f.admin = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/realms/master/users" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("search") != "ann" || q.Get("max") != "20" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, []any{
			map[string]any{"id": "u1", "username": "ann"},
		})
	}
	p := newKeycloakPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "users", Connection: f.conn(nil),
		Options: map[string]any{"search": "ann", "max": 20},
	})
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
	if items[0].(map[string]any)["username"] != "ann" {
		t.Errorf("items[0]: %#v", items[0])
	}
}

func TestRealmsListHoistsItemsEvenWhenEmpty(t *testing.T) {
	f := newFakeKeycloak(t, "master")
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/master/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		f.next++
		f.current = fmt.Sprintf("tok-%d", f.next)
		writeJSON(w, 200, map[string]any{"access_token": f.current, "expires_in": 3600})
	})
	mux.HandleFunc("/admin/realms", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/realms" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{map[string]any{"realm": "master"}, map[string]any{"realm": "corp"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newKeycloakPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "realms",
		Connection: map[string]any{"base_url": srv.URL, "client_id": "svc", "client_secret": "shh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

// --- user_create: 201 + Location -> id ---

func TestUserCreateParsesLocationID(t *testing.T) {
	f := newFakeKeycloak(t, "master")
	var gotBody map[string]any
	f.admin = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/realms/master/users" || r.Method != http.MethodPost {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type: got %q", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Location", "http://"+r.Host+"/admin/realms/master/users/abc-123")
		w.WriteHeader(http.StatusCreated) // Keycloak's create response has NO body
	}
	p := newKeycloakPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "user_create", Connection: f.conn(nil),
		Options: map[string]any{"body": map[string]any{"username": "ann", "enabled": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != http.StatusCreated {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	if res.Outputs["id"] != "abc-123" {
		t.Fatalf("id (parsed from Location): %#v", res.Outputs["id"])
	}
	if gotBody["username"] != "ann" || gotBody["enabled"] != true {
		t.Errorf("request body: %#v", gotBody)
	}
}

func TestUserCreateRequiresBody(t *testing.T) {
	p := newKeycloakPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "user_create",
		Connection: map[string]any{"base_url": "http://x", "client_id": "c", "client_secret": "s"},
	})
	if err == nil {
		t.Fatal("expected error for missing body")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

// --- user_reset_password / user_delete / user_logout: status-only ---

func TestUserResetPassword(t *testing.T) {
	f := newFakeKeycloak(t, "master")
	var gotBody map[string]any
	f.admin = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/realms/master/users/u1/reset-password" || r.Method != http.MethodPut {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}
	p := newKeycloakPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "user_reset_password", Connection: f.conn(nil),
		Options: map[string]any{"id": "u1", "value": "s3cret!", "temporary": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != http.StatusNoContent {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	if gotBody["type"] != "password" || gotBody["value"] != "s3cret!" || gotBody["temporary"] != true {
		t.Errorf("request body: %#v", gotBody)
	}
}

func TestUserDeleteAndLogout(t *testing.T) {
	f := newFakeKeycloak(t, "master")
	var methods []string
	f.admin = func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
	p := newKeycloakPlugin()
	conn := f.conn(nil)

	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "user_delete", Connection: conn, Options: map[string]any{"id": "u1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "user_logout", Connection: conn, Options: map[string]any{"id": "u1"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /admin/realms/master/users/u1", "POST /admin/realms/master/users/u1/logout"}
	if len(methods) != 2 || methods[0] != want[0] || methods[1] != want[1] {
		t.Errorf("requests: got %v want %v", methods, want)
	}
}

// --- api escape hatch ---

func TestAPIEscapeHatch(t *testing.T) {
	f := newFakeKeycloak(t, "master")
	f.admin = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/realms/master/groups" {
			writeJSON(w, 200, []any{map[string]any{"id": "g1"}})
			return
		}
		t.Errorf("unexpected path: %s", r.URL.Path)
	}
	p := newKeycloakPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: f.conn(nil),
		Options: map[string]any{"path": "/admin/realms/master/groups"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIEscapeHatchMissingPath(t *testing.T) {
	p := newKeycloakPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "api",
		Connection: map[string]any{"base_url": "http://x", "client_id": "c", "client_secret": "s"},
	})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

// --- non-2xx -> CodeInternalError ---

func TestNon2xxIsInternalError(t *testing.T) {
	f := newFakeKeycloak(t, "master")
	f.admin = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}
	p := newKeycloakPlugin()

	_, err := p.Invoke(plugin.InvokeRequest{Verb: "users", Connection: f.conn(nil)})
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
	if !strings.Contains(pe.Message, "500") || !strings.Contains(pe.Message, "boom") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

// A 401 that persists even after refresh (a genuinely bad credential, not
// just a stale cache) must surface as CodeInternalError, not loop forever.
func TestPersistent401IsInternalErrorAfterOneRetry(t *testing.T) {
	f := newFakeKeycloak(t, "master")
	var calls int32
	f.admin = func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}
	p := newKeycloakPlugin()

	_, err := p.Invoke(plugin.InvokeRequest{Verb: "users", Connection: f.conn(nil)})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
	// The 401 handler in the fake server always answers 401 regardless of
	// bearer token, so this only checks the retry happens exactly once (not
	// zero, not an infinite loop): one token fetch, one admin call, one
	// refresh, one retried admin call.
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 admin attempts (initial + one retry), got %d", got)
	}
	if got := atomic.LoadInt32(&f.tokenFetches); got != 2 {
		t.Fatalf("expected exactly 2 token fetches (initial + refresh), got %d", got)
	}
}

// --- missing connection fields ---

func TestMissingConnection(t *testing.T) {
	p := newKeycloakPlugin()
	cases := []map[string]any{
		{"client_id": "c", "client_secret": "s"},       // missing base_url
		{"base_url": "http://x", "client_secret": "s"}, // missing client_id
		{"base_url": "http://x", "client_id": "c"},     // missing client_secret
	}
	for i, conn := range cases {
		if _, err := p.Invoke(plugin.InvokeRequest{Verb: "users", Connection: conn}); err == nil {
			t.Errorf("case %d: expected error for %#v", i, conn)
		} else if pe, ok := err.(*plugin.Error); !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("case %d: expected CodeInvalidParams, got %v", i, err)
		}
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newKeycloakPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "client_id": "c", "client_secret": "s"}
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"user_get", map[string]any{}},
		{"user_create", map[string]any{}},
		{"user_update", map[string]any{"id": "u1"}},                              // missing body
		{"user_update", map[string]any{"body": map[string]any{"enabled": true}}}, // missing id
		{"user_delete", map[string]any{}},
		{"user_reset_password", map[string]any{"id": "u1"}},   // missing value
		{"user_reset_password", map[string]any{"value": "x"}}, // missing id
		{"user_logout", map[string]any{}},
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s %v: expected error for missing required options", tc.verb, tc.opts)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", tc.verb, err)
		}
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newKeycloakPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{"base_url": "http://x", "client_id": "c", "client_secret": "s"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

// --- locationID helper ---

func TestLocationID(t *testing.T) {
	cases := map[string]string{
		"http://host/admin/realms/master/users/abc-123":  "abc-123",
		"http://host/admin/realms/master/users/abc-123/": "abc-123",
		"/admin/realms/master/users/xyz":                 "xyz",
		"":                                               "",
	}
	for in, want := range cases {
		if got := locationID(in); got != want {
			t.Errorf("locationID(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- Describe ---

func TestDescribe(t *testing.T) {
	p := newKeycloakPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "keycloak" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	for _, key := range []string{"base_url", "client_id", "client_secret"} {
		f := d.Connection[key]
		if f.Type != "string" || !f.Required {
			t.Errorf("connection.%s should be required string, got %#v", key, f)
		}
	}
	if d.Connection["insecure_skip_verify"].Type != "boolean" {
		t.Errorf("connection.insecure_skip_verify: %#v", d.Connection["insecure_skip_verify"])
	}
	want := []string{
		"realms", "realm_get", "users", "user_get", "user_create", "user_update",
		"user_delete", "user_reset_password", "user_logout", "groups", "clients",
		"roles", "sessions", "events", "api",
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
	if len(d.Capabilities.Egress) != 0 {
		t.Errorf("expected empty egress (self-hosted), got %v", d.Capabilities.Egress)
	}
}
