package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn wires a connection pointed at srv.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{"base_url": srv.URL, "api_token": "secret-token"}
}

func checkBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
		t.Errorf("Authorization header: got %q want %q", got, "Bearer secret-token")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestUsersHoistsResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/v3/core/users/" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("search") != "dan" || q.Get("is_active") != "true" || q.Get("ordering") != "username" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{
			"pagination": map[string]any{"count": 1, "next": 0, "previous": 0},
			"results":    []any{map[string]any{"pk": 1, "username": "dan"}},
		})
	}))
	defer srv.Close()

	p := newAuthentikPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "users", Connection: testConn(srv),
		Options: map[string]any{"search": "dan", "is_active": true, "ordering": "username"},
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
	got := items[0].(map[string]any)
	if got["username"] != "dan" {
		t.Errorf("items[0]: %#v", got)
	}
	// result carries the full body (pagination metadata survives).
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["pagination"]; !ok {
		t.Errorf("result missing pagination: %#v", result)
	}
}

func TestUserGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/v3/core/users/42/" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"pk": 42, "username": "dan"})
	}))
	defer srv.Close()

	p := newAuthentikPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "user_get", Connection: testConn(srv),
		Options: map[string]any{"user_id": "42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["username"] != "dan" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestUserCreate(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 201, map[string]any{"pk": 7, "username": "newuser"})
	}))
	defer srv.Close()

	p := newAuthentikPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "user_create", Connection: testConn(srv),
		Options: map[string]any{
			"username": "newuser", "name": "New User", "is_active": true,
			"groups": []any{"grp1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v3/core/users/" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	want := map[string]any{
		"username": "newuser", "name": "New User", "is_active": true,
		"groups": []any{"grp1"},
	}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
	if res.Outputs["status_code"] != 201 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["username"] != "newuser" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestUserUpdate(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"pk": 42, "email": "dan@example.com"})
	}))
	defer srv.Close()

	p := newAuthentikPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "user_update", Connection: testConn(srv),
		Options: map[string]any{"user_id": "42", "email": "dan@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/api/v3/core/users/42/" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	want := map[string]any{"email": "dan@example.com"}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["email"] != "dan@example.com" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestUserDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newAuthentikPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "user_delete", Connection: testConn(srv),
		Options: map[string]any{"user_id": "42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v3/core/users/42/" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["status_code"] != 204 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
	if _, ok := res.Outputs["result"]; ok {
		t.Errorf("expected no result for empty body, got %#v", res.Outputs["result"])
	}
}

func TestGroupsApplicationsProvidersFlowsTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch r.URL.Path {
		case "/api/v3/core/groups/":
			writeJSON(w, 200, map[string]any{"results": []any{map[string]any{"pk": "g1"}}})
		case "/api/v3/core/applications/":
			writeJSON(w, 200, map[string]any{"results": []any{map[string]any{"slug": "app1"}}})
		case "/api/v3/providers/all/":
			writeJSON(w, 200, map[string]any{"results": []any{map[string]any{"pk": 1, "name": "prov1"}}})
		case "/api/v3/flows/instances/":
			writeJSON(w, 200, map[string]any{"results": []any{map[string]any{"slug": "flow1"}}})
		case "/api/v3/core/tokens/":
			writeJSON(w, 200, map[string]any{"results": []any{map[string]any{"identifier": "tok1"}}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newAuthentikPlugin()
	cases := []struct {
		verb string
		key  string
		want string
	}{
		{"groups", "pk", "g1"},
		{"applications", "slug", "app1"},
		{"flows", "slug", "flow1"},
		{"tokens", "identifier", "tok1"},
	}
	for _, tc := range cases {
		res, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: testConn(srv)})
		if err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		items, ok := res.Outputs["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("%s items: %#v", tc.verb, res.Outputs["items"])
		}
		got := items[0].(map[string]any)
		if got[tc.key] != tc.want {
			t.Errorf("%s items[0]: %#v", tc.verb, got)
		}
	}

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "providers", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["name"] != "prov1" {
		t.Fatalf("providers items: %#v", res.Outputs["items"])
	}
}

func TestEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/v3/events/events/" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("action") != "login" || q.Get("username") != "dan" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{"results": []any{map[string]any{"action": "login"}}})
	}))
	defer srv.Close()

	p := newAuthentikPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "events", Connection: testConn(srv),
		Options: map[string]any{"action": "login", "username": "dan"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/core/users/me/":
			writeJSON(w, 200, map[string]any{"user": map[string]any{"username": "dan"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/core/groups/" && r.URL.Query().Get("ordering") == "name":
			writeJSON(w, 200, map[string]any{"results": []any{map[string]any{"pk": "g1"}}, "pagination": map[string]any{"count": 1}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newAuthentikPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/core/users/me/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["user"]; !ok {
		t.Fatalf("result missing user: %#v", result)
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/core/groups/", "query": map[string]any{"ordering": "name"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items (hoisted from result.results): %#v", res.Outputs["items"])
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid token"}`))
	}))
	defer srv.Close()

	p := newAuthentikPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "users", Connection: testConn(srv)})
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
	if !containsAll(pe.Message, "401", "invalid token") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newAuthentikPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "api_token": "t"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"user_get", map[string]any{}},
		{"user_create", map[string]any{}},
		{"user_update", map[string]any{}},
		{"user_delete", map[string]any{}},
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

func TestMissingConnection(t *testing.T) {
	p := newAuthentikPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "users", Connection: map[string]any{"api_token": "t"}}); err == nil {
		t.Error("expected error for missing base_url")
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "users", Connection: map[string]any{"base_url": "http://x"}}); err == nil {
		t.Error("expected error for missing api_token")
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newAuthentikPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: map[string]any{"base_url": "http://x", "api_token": "t"}})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		writeJSON(w, 200, map[string]any{"results": []any{}})
	}))
	defer srv.Close()

	p := newAuthentikPlugin()
	conn := map[string]any{"base_url": srv.URL, "api_token": "secret-token", "insecure_skip_verify": true}
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "users", Connection: conn})
	if err != nil {
		t.Fatalf("expected insecure_skip_verify to allow the self-signed cert, got %v", err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}

	// Without the flag, the self-signed cert should be rejected.
	p2 := newAuthentikPlugin()
	_, err = p2.Invoke(plugin.InvokeRequest{Verb: "users", Connection: map[string]any{"base_url": srv.URL, "api_token": "secret-token"}})
	if err == nil {
		t.Fatal("expected TLS verification error without insecure_skip_verify")
	}
}

func TestDescribe(t *testing.T) {
	p := newAuthentikPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "authentik" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["base_url"].Type != "string" || !d.Connection["base_url"].Required {
		t.Errorf("connection.base_url: %#v", d.Connection["base_url"])
	}
	if d.Connection["api_token"].Type != "string" || !d.Connection["api_token"].Required {
		t.Errorf("connection.api_token: %#v", d.Connection["api_token"])
	}
	if d.Connection["insecure_skip_verify"].Type != "boolean" {
		t.Errorf("connection.insecure_skip_verify: %#v", d.Connection["insecure_skip_verify"])
	}
	want := []string{
		"users", "user_get", "user_create", "user_update", "user_delete",
		"groups", "applications", "providers", "flows", "events", "tokens", "api",
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
