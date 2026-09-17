package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{"base_url": srv.URL, "email": "admin@example.com", "password": "hunter2"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// tokenHandler answers POST /api/tokens with a fixed JWT and counts how many
// times it was called, so tests can assert the session is cached (one login)
// rather than re-established on every verb call.
func tokenHandler(t *testing.T, calls *int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tokens" {
			t.Errorf("token request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("token body decode: %v", err)
		}
		if body["identity"] != "admin@example.com" {
			t.Errorf("token identity: got %#v", body["identity"])
		}
		if body["secret"] != "hunter2" {
			t.Errorf("token secret: got %#v", body["secret"])
		}
		atomic.AddInt32(calls, 1)
		writeJSON(w, 200, map[string]any{"token": "jwt-123", "expires": "2099-01-01T00:00:00.000Z"})
	}
}

func checkAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer jwt-123" {
		t.Errorf("Authorization header: got %q want %q", got, "Bearer jwt-123")
	}
}

func TestProxyHostsList(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tokens", tokenHandler(t, &tokenCalls))
	mux.HandleFunc("/api/nginx/proxy-hosts", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.Method != http.MethodGet {
			t.Errorf("method: got %s want GET", r.Method)
		}
		writeJSON(w, 200, []any{map[string]any{"id": float64(1), "domain_names": []any{"a.example"}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newNPMPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "proxy_hosts", Instance: "npm1", Connection: testConn(srv)})
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

	// A second verb call against the same instance must reuse the cached
	// token — no second /api/tokens call.
	_, err = p.Invoke(plugin.InvokeRequest{Verb: "proxy_hosts", Instance: "npm1", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Errorf("token calls: got %d want 1 (session should be cached)", got)
	}
}

func TestProxyHostGet(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tokens", tokenHandler(t, &tokenCalls))
	mux.HandleFunc("/api/nginx/proxy-hosts/42", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.Method != http.MethodGet {
			t.Errorf("method: got %s want GET", r.Method)
		}
		writeJSON(w, 200, map[string]any{"id": float64(42), "forward_host": "10.0.0.5"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newNPMPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "proxy_host_get", Instance: "npm1", Connection: testConn(srv),
		Options: map[string]any{"id": float64(42)},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["forward_host"] != "10.0.0.5" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestProxyHostCreate(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tokens", tokenHandler(t, &tokenCalls))
	var gotBody map[string]any
	mux.HandleFunc("/api/nginx/proxy-hosts", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 201, map[string]any{"id": float64(7), "domain_names": []any{"new.example"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newNPMPlugin()
	body := map[string]any{
		"domain_names":   []any{"new.example"},
		"forward_scheme": "http",
		"forward_host":   "10.0.0.9",
		"forward_port":   float64(8080),
	}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "proxy_host_create", Instance: "npm1", Connection: testConn(srv),
		Options: map[string]any{"body": body},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["forward_host"] != "10.0.0.9" {
		t.Errorf("request body forward_host: got %#v", gotBody["forward_host"])
	}
	if res.Outputs["status_code"] != 201 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != float64(7) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestProxyHostUpdate(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tokens", tokenHandler(t, &tokenCalls))
	var gotMethod string
	var gotBody map[string]any
	mux.HandleFunc("/api/nginx/proxy-hosts/9", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"id": float64(9), "forward_port": float64(9090)})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newNPMPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "proxy_host_update", Instance: "npm1", Connection: testConn(srv),
		Options: map[string]any{"id": 9, "body": map[string]any{"forward_port": float64(9090)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method: got %s want PUT", gotMethod)
	}
	if gotBody["forward_port"] != float64(9090) {
		t.Errorf("request body: got %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["forward_port"] != float64(9090) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestProxyHostDelete(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tokens", tokenHandler(t, &tokenCalls))
	var gotMethod, gotPath string
	mux.HandleFunc("/api/nginx/proxy-hosts/3", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, 200, true)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newNPMPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "proxy_host_delete", Instance: "npm1", Connection: testConn(srv),
		Options: map[string]any{"id": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/nginx/proxy-hosts/3" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["result"] != true {
		t.Errorf("result (bare boolean body): %#v", res.Outputs["result"])
	}
}

func TestProxyHostEnableDisable(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tokens", tokenHandler(t, &tokenCalls))
	var enablePath, disablePath string
	mux.HandleFunc("/api/nginx/proxy-hosts/5/enable", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		enablePath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s want POST", r.Method)
		}
		writeJSON(w, 200, true)
	})
	mux.HandleFunc("/api/nginx/proxy-hosts/5/disable", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		disablePath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s want POST", r.Method)
		}
		writeJSON(w, 200, true)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newNPMPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "proxy_host_enable", Instance: "npm1", Connection: testConn(srv),
		Options: map[string]any{"id": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if enablePath != "/api/nginx/proxy-hosts/5/enable" {
		t.Errorf("enable path: got %q", enablePath)
	}
	if res.Outputs["result"] != true {
		t.Errorf("enable result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "proxy_host_disable", Instance: "npm1", Connection: testConn(srv),
		Options: map[string]any{"id": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if disablePath != "/api/nginx/proxy-hosts/5/disable" {
		t.Errorf("disable path: got %q", disablePath)
	}
	if res.Outputs["result"] != true {
		t.Errorf("disable result: %#v", res.Outputs["result"])
	}
}

func TestListVerbs(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tokens", tokenHandler(t, &tokenCalls))
	mux.HandleFunc("/api/nginx/redirection-hosts", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		writeJSON(w, 200, []any{map[string]any{"id": float64(1)}})
	})
	mux.HandleFunc("/api/nginx/streams", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		writeJSON(w, 200, []any{map[string]any{"id": float64(2)}})
	})
	mux.HandleFunc("/api/nginx/dead-hosts", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		writeJSON(w, 200, []any{map[string]any{"id": float64(3)}})
	})
	mux.HandleFunc("/api/nginx/access-lists", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		writeJSON(w, 200, []any{map[string]any{"id": float64(4)}})
	})
	mux.HandleFunc("/api/nginx/certificates", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		writeJSON(w, 200, []any{map[string]any{"id": float64(5)}})
	})
	mux.HandleFunc("/api/reports/hosts", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		writeJSON(w, 200, map[string]any{"proxy": float64(10)})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newNPMPlugin()

	for _, verb := range []string{"redirection_hosts", "streams", "dead_hosts", "access_lists", "certificates"} {
		res, err := p.Invoke(plugin.InvokeRequest{Verb: verb, Instance: "npm1", Connection: testConn(srv)})
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		items, ok := res.Outputs["items"].([]any)
		if !ok || len(items) != 1 {
			t.Errorf("%s items: %#v", verb, res.Outputs["items"])
		}
	}

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "reports", Instance: "npm1", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["proxy"] != float64(10) {
		t.Fatalf("reports result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tokens", tokenHandler(t, &tokenCalls))
	mux.HandleFunc("/api/nginx/proxy-hosts", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		writeJSON(w, 200, []any{map[string]any{"id": float64(1)}})
	})
	mux.HandleFunc("/api/schema", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		writeJSON(w, 200, map[string]any{"openapi": "3.0.0"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newNPMPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Instance: "npm1", Connection: testConn(srv),
		Options: map[string]any{"path": "/nginx/proxy-hosts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items (bare array): %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Instance: "npm1", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "schema"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["openapi"] != "3.0.0" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestReauthOn401(t *testing.T) {
	var tokenCalls int32
	var listCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tokens", tokenHandler(t, &tokenCalls))
	mux.HandleFunc("/api/nginx/proxy-hosts", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&listCalls, 1)
		if n == 1 {
			// First call: simulate an expired/invalid token.
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Unauthorized"}}`))
			return
		}
		checkAuth(t, r)
		writeJSON(w, 200, []any{map[string]any{"id": float64(1)}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newNPMPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "proxy_hosts", Instance: "npm1", Connection: testConn(srv)})
	if err != nil {
		t.Fatalf("expected transparent re-auth to succeed, got %v", err)
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 2 {
		t.Errorf("token calls: got %d want 2 (initial login + re-auth after 401)", got)
	}
	if got := atomic.LoadInt32(&listCalls); got != 2 {
		t.Errorf("list calls: got %d want 2 (original 401 + retry)", got)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestLoginFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tokens" {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "Invalid credentials"}})
			return
		}
		t.Errorf("unexpected request past login: %s", r.URL.Path)
	}))
	defer srv.Close()

	p := newNPMPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "proxy_hosts", Instance: "npm1", Connection: testConn(srv)})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "login failed") {
		t.Errorf("message should mention login failure: %q", pe.Message)
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tokens", tokenHandler(t, &tokenCalls))
	mux.HandleFunc("/api/nginx/proxy-hosts", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newNPMPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "proxy_hosts", Instance: "npm1", Connection: testConn(srv)})
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

func TestMissingRequiredOptions(t *testing.T) {
	p := newNPMPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "email": "a@b.com", "password": "p"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"proxy_host_get", map[string]any{}},
		{"proxy_host_create", map[string]any{}},
		{"proxy_host_update", map[string]any{}},
		{"proxy_host_update", map[string]any{"id": 1}}, // missing body
		{"proxy_host_delete", map[string]any{}},
		{"proxy_host_enable", map[string]any{}},
		{"proxy_host_disable", map[string]any{}},
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Instance: "npm1", Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s %v: expected error for missing required options", tc.verb, tc.opts)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s %v: expected CodeInvalidParams, got %v", tc.verb, tc.opts, err)
		}
	}
}

func TestMissingConnection(t *testing.T) {
	p := newNPMPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "proxy_hosts", Instance: "npm1", Connection: map[string]any{"email": "a@b.com", "password": "p"}}); err == nil {
		t.Error("expected error for missing base_url")
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "proxy_hosts", Instance: "npm1", Connection: map[string]any{"base_url": "http://x", "password": "p"}}); err == nil {
		t.Error("expected error for missing email")
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "proxy_hosts", Instance: "npm1", Connection: map[string]any{"base_url": "http://x", "email": "a@b.com"}}); err == nil {
		t.Error("expected error for missing password")
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newNPMPlugin()
	conn := map[string]any{"base_url": "http://x", "email": "a@b.com", "password": "p"}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Instance: "npm1", Connection: conn})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestDescribe(t *testing.T) {
	p := newNPMPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "nginx-proxy-manager" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["base_url"].Type != "string" || !d.Connection["base_url"].Required {
		t.Errorf("connection.base_url: %#v", d.Connection["base_url"])
	}
	if d.Connection["email"].Type != "string" || !d.Connection["email"].Required {
		t.Errorf("connection.email: %#v", d.Connection["email"])
	}
	if d.Connection["password"].Type != "string" || !d.Connection["password"].Required {
		t.Errorf("connection.password: %#v", d.Connection["password"])
	}
	if d.Connection["insecure_skip_verify"].Type != "boolean" {
		t.Errorf("connection.insecure_skip_verify: %#v", d.Connection["insecure_skip_verify"])
	}
	want := []string{
		"proxy_hosts", "proxy_host_get", "proxy_host_create", "proxy_host_update",
		"proxy_host_delete", "proxy_host_enable", "proxy_host_disable",
		"redirection_hosts", "streams", "dead_hosts", "access_lists",
		"certificates", "reports", "api",
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

func TestInsecureSkipVerifyUsesSeparateClient(t *testing.T) {
	p := newNPMPlugin()
	c1, err := p.clientFor("npm1", map[string]any{"base_url": "http://x", "email": "a@b.com", "password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := p.clientFor("npm1", map[string]any{"base_url": "http://x", "email": "a@b.com", "password": "p", "insecure_skip_verify": true})
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c2 {
		t.Error("expected a new client when insecure_skip_verify changes for the same instance")
	}
}
