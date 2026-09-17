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
	return map[string]any{"base_url": srv.URL, "password": "hunter2"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// authHandler answers POST /api/auth with a fixed SID/CSRF pair and counts
// how many times it was called, so tests can assert the session is cached
// (one login) rather than re-established on every verb call.
func authHandler(t *testing.T, calls *int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/auth" {
			t.Errorf("auth request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("auth body decode: %v", err)
		}
		if body["password"] != "hunter2" {
			t.Errorf("auth password: got %#v", body["password"])
		}
		atomic.AddInt32(calls, 1)
		writeJSON(w, 200, map[string]any{"session": map[string]any{
			"valid": true, "sid": "sid-123", "csrf": "csrf-456", "validity": 300,
		}})
	}
}

func checkSID(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("X-FTL-SID"); got != "sid-123" {
		t.Errorf("X-FTL-SID header: got %q want %q", got, "sid-123")
	}
}

func TestSummary(t *testing.T) {
	var authCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	mux.HandleFunc("/api/stats/summary", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		writeJSON(w, 200, map[string]any{"queries": map[string]any{"total": 100}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "summary", Instance: "pi1", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	queries, ok := result["queries"].(map[string]any)
	if !ok || queries["total"] != float64(100) {
		t.Fatalf("result.queries: %#v", result["queries"])
	}

	// A second verb call against the same instance must reuse the cached
	// session — no second /api/auth call.
	_, err = p.Invoke(plugin.InvokeRequest{Verb: "summary", Instance: "pi1", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&authCalls); got != 1 {
		t.Errorf("auth calls: got %d want 1 (session should be cached)", got)
	}
}

func TestHistory(t *testing.T) {
	var authCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	mux.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		if r.URL.Query().Get("from") != "100" || r.URL.Query().Get("until") != "200" {
			t.Errorf("query: got %v", r.URL.Query())
		}
		writeJSON(w, 200, map[string]any{"history": []any{map[string]any{"timestamp": 100}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "history", Instance: "pi1", Connection: testConn(srv),
		Options: map[string]any{"from": 100, "until": 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items (hoisted from history): %#v", res.Outputs["items"])
	}
}

func TestQueries(t *testing.T) {
	mux := http.NewServeMux()
	var authCalls int32
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	mux.HandleFunc("/api/queries", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		q := r.URL.Query()
		if q.Get("domain") != "example.com" || q.Get("blocked") != "true" || q.Get("length") != "50" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{"queries": []any{map[string]any{"id": 1}}, "cursor": "next"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "queries", Instance: "pi1", Connection: testConn(srv),
		Options: map[string]any{"domain": "example.com", "blocked": true, "length": 50},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items (hoisted from queries): %#v", res.Outputs["items"])
	}
}

func TestTopDomainsAndTopClients(t *testing.T) {
	mux := http.NewServeMux()
	var authCalls int32
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	mux.HandleFunc("/api/stats/top_domains", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		if r.URL.Query().Get("count") != "5" || r.URL.Query().Get("blocked") != "true" {
			t.Errorf("query: got %v", r.URL.Query())
		}
		writeJSON(w, 200, map[string]any{"domains": []any{map[string]any{"domain": "ads.example"}}})
	})
	mux.HandleFunc("/api/stats/top_clients", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		writeJSON(w, 200, map[string]any{"clients": []any{map[string]any{"ip": "10.0.0.5"}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "top_domains", Instance: "pi1", Connection: testConn(srv),
		Options: map[string]any{"count": 5, "blocked": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["domain"] != "ads.example" {
		t.Fatalf("top_domains items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "top_clients", Instance: "pi1", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok = res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["ip"] != "10.0.0.5" {
		t.Fatalf("top_clients items: %#v", res.Outputs["items"])
	}
}

func TestUpstreams(t *testing.T) {
	mux := http.NewServeMux()
	var authCalls int32
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	mux.HandleFunc("/api/stats/upstreams", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		writeJSON(w, 200, map[string]any{"upstreams": []any{map[string]any{"ip": "1.1.1.1"}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "upstreams", Instance: "pi1", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestBlockingGet(t *testing.T) {
	mux := http.NewServeMux()
	var authCalls int32
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	mux.HandleFunc("/api/dns/blocking", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		if r.Method != http.MethodGet {
			t.Errorf("method: got %s", r.Method)
		}
		writeJSON(w, 200, map[string]any{"blocking": "enabled", "timer": nil})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "blocking", Instance: "pi1", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["blocking"] != "enabled" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestSetBlocking(t *testing.T) {
	mux := http.NewServeMux()
	var authCalls int32
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	var gotBody map[string]any
	var gotCSRF string
	mux.HandleFunc("/api/dns/blocking", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s want POST", r.Method)
		}
		gotCSRF = r.Header.Get("X-FTL-CSRF")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"blocking": "disabled", "timer": float64(60)})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "set_blocking", Instance: "pi1", Connection: testConn(srv),
		Options: map[string]any{"blocking": false, "timer": 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotCSRF != "csrf-456" {
		t.Errorf("X-FTL-CSRF header on POST: got %q want %q", gotCSRF, "csrf-456")
	}
	want := map[string]any{"blocking": false, "timer": float64(60)}
	if gotBody["blocking"] != want["blocking"] || gotBody["timer"] != want["timer"] {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["blocking"] != "disabled" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestDomainsListAddRemove(t *testing.T) {
	mux := http.NewServeMux()
	var authCalls int32
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	var addBody map[string]any
	var addCSRF string
	var removePath, removeMethod, removeCSRF string
	mux.HandleFunc("/api/domains", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		if r.URL.Query().Get("type") != "allow" {
			t.Errorf("query: got %v", r.URL.Query())
		}
		writeJSON(w, 200, map[string]any{"domains": []any{map[string]any{"domain": "good.example"}}})
	})
	mux.HandleFunc("/api/domains/deny/exact", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		addCSRF = r.Header.Get("X-FTL-CSRF")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &addBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 201, map[string]any{"domains": []any{map[string]any{"domain": "ads.example"}}})
	})
	mux.HandleFunc("/api/domains/deny/exact/ads.example", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		removeMethod, removePath, removeCSRF = r.Method, r.URL.Path, r.Header.Get("X-FTL-CSRF")
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "domains", Instance: "pi1", Connection: testConn(srv),
		Options: map[string]any{"type": "allow"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("domains items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "domain_add", Instance: "pi1", Connection: testConn(srv),
		Options: map[string]any{"type": "deny", "kind": "exact", "domain": "ads.example", "comment": "blocklist", "enabled": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if addCSRF != "csrf-456" {
		t.Errorf("X-FTL-CSRF on domain_add: got %q", addCSRF)
	}
	want := map[string]any{"domain": "ads.example", "comment": "blocklist", "enabled": true}
	for k, v := range want {
		if addBody[k] != v {
			t.Errorf("add body[%s]: got %#v want %#v", k, addBody[k], v)
		}
	}
	if res.Outputs["status_code"] != 201 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "domain_remove", Instance: "pi1", Connection: testConn(srv),
		Options: map[string]any{"type": "deny", "kind": "exact", "domain": "ads.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if removeMethod != http.MethodDelete || removePath != "/api/domains/deny/exact/ads.example" {
		t.Fatalf("remove request: %s %s", removeMethod, removePath)
	}
	if removeCSRF != "csrf-456" {
		t.Errorf("X-FTL-CSRF on domain_remove: got %q", removeCSRF)
	}
	if res.Outputs["status_code"] != 204 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestDomainAddInvalidTypeKind(t *testing.T) {
	p := newPiholePlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "password": "p"}

	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "domain_add", Instance: "pi1", Connection: conn,
		Options: map[string]any{"type": "nope", "kind": "exact", "domain": "x.example"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for bad type, got %v", err)
	}

	_, err = p.Invoke(plugin.InvokeRequest{
		Verb: "domain_add", Instance: "pi1", Connection: conn,
		Options: map[string]any{"type": "deny", "kind": "nope", "domain": "x.example"},
	})
	pe, ok = err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for bad kind, got %v", err)
	}
}

func TestListsGroupsClients(t *testing.T) {
	mux := http.NewServeMux()
	var authCalls int32
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	mux.HandleFunc("/api/lists", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		if r.URL.Query().Get("type") != "block" {
			t.Errorf("query: got %v", r.URL.Query())
		}
		writeJSON(w, 200, map[string]any{"lists": []any{map[string]any{"id": 1}}})
	})
	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		writeJSON(w, 200, map[string]any{"groups": []any{map[string]any{"id": 1}}})
	})
	mux.HandleFunc("/api/clients", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		writeJSON(w, 200, map[string]any{"clients": []any{map[string]any{"id": 1}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "lists", Instance: "pi1", Connection: testConn(srv),
		Options: map[string]any{"type": "block"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if items, ok := res.Outputs["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("lists items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "groups", Instance: "pi1", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if items, ok := res.Outputs["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("groups items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "clients", Instance: "pi1", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if items, ok := res.Outputs["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("clients items: %#v", res.Outputs["items"])
	}
}

func TestGravityUpdate(t *testing.T) {
	mux := http.NewServeMux()
	var authCalls int32
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	var gotMethod, gotCSRF string
	mux.HandleFunc("/api/action/gravity", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		gotMethod = r.Method
		gotCSRF = r.Header.Get("X-FTL-CSRF")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Pi-hole blocking lists updated"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "gravity_update", Instance: "pi1", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %s want POST", gotMethod)
	}
	if gotCSRF != "csrf-456" {
		t.Errorf("X-FTL-CSRF: got %q", gotCSRF)
	}
	if res.Outputs["result"] != "Pi-hole blocking lists updated" {
		t.Errorf("result (non-JSON body as string): %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	mux := http.NewServeMux()
	var authCalls int32
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		writeJSON(w, 200, map[string]any{"version": "v6.0"})
	})
	mux.HandleFunc("/api/stats/database/summary", func(w http.ResponseWriter, r *http.Request) {
		checkSID(t, r)
		if r.URL.Query().Get("from") != "10" {
			t.Errorf("query: got %v", r.URL.Query())
		}
		writeJSON(w, 200, []any{map[string]any{"id": 1}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Instance: "pi1", Connection: testConn(srv),
		Options: map[string]any{"path": "/version"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["version"] != "v6.0" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Instance: "pi1", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/stats/database/summary", "query": map[string]any{"from": "10"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestReauthOn401(t *testing.T) {
	var authCalls int32
	var summaryCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	mux.HandleFunc("/api/stats/summary", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&summaryCalls, 1)
		if n == 1 {
			// First call: simulate an expired/invalid session.
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"key":"unauthorized","message":"Unauthorized"}}`))
			return
		}
		checkSID(t, r)
		writeJSON(w, 200, map[string]any{"queries": map[string]any{"total": 1}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "summary", Instance: "pi1", Connection: testConn(srv)})
	if err != nil {
		t.Fatalf("expected transparent re-auth to succeed, got %v", err)
	}
	if got := atomic.LoadInt32(&authCalls); got != 2 {
		t.Errorf("auth calls: got %d want 2 (initial login + re-auth after 401)", got)
	}
	if got := atomic.LoadInt32(&summaryCalls); got != 2 {
		t.Errorf("summary calls: got %d want 2 (original 401 + retry)", got)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestLoginFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth" {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"key": "unauthorized", "message": "Unauthorized"}})
			return
		}
		t.Errorf("unexpected request past login: %s", r.URL.Path)
	}))
	defer srv.Close()

	p := newPiholePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "summary", Instance: "pi1", Connection: testConn(srv)})
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
	var authCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth", authHandler(t, &authCalls))
	mux.HandleFunc("/api/stats/summary", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPiholePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "summary", Instance: "pi1", Connection: testConn(srv)})
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
	p := newPiholePlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "password": "p"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"set_blocking", map[string]any{}},
		{"domain_add", map[string]any{}},
		{"domain_add", map[string]any{"type": "allow", "kind": "exact"}}, // missing domain
		{"domain_remove", map[string]any{}},
		{"domain_remove", map[string]any{"type": "allow", "kind": "exact"}}, // missing domain
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Instance: "pi1", Connection: conn, Options: tc.opts})
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
	p := newPiholePlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "summary", Instance: "pi1", Connection: map[string]any{"password": "p"}}); err == nil {
		t.Error("expected error for missing base_url")
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "summary", Instance: "pi1", Connection: map[string]any{"base_url": "http://x"}}); err == nil {
		t.Error("expected error for missing password")
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newPiholePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Instance: "pi1", Connection: map[string]any{"base_url": "http://x", "password": "p"}})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestDescribe(t *testing.T) {
	p := newPiholePlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "pihole" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["base_url"].Type != "string" || !d.Connection["base_url"].Required {
		t.Errorf("connection.base_url: %#v", d.Connection["base_url"])
	}
	if d.Connection["password"].Type != "string" || !d.Connection["password"].Required {
		t.Errorf("connection.password: %#v", d.Connection["password"])
	}
	if d.Connection["insecure_skip_verify"].Type != "boolean" {
		t.Errorf("connection.insecure_skip_verify: %#v", d.Connection["insecure_skip_verify"])
	}
	want := []string{
		"summary", "history", "queries", "top_domains", "top_clients", "upstreams",
		"blocking", "set_blocking", "domains", "domain_add", "domain_remove",
		"lists", "groups", "clients", "gravity_update", "api",
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
	p := newPiholePlugin()
	c1, err := p.clientFor("pi1", map[string]any{"base_url": "http://x", "password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := p.clientFor("pi1", map[string]any{"base_url": "http://x", "password": "p", "insecure_skip_verify": true})
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c2 {
		t.Error("expected a new client when insecure_skip_verify changes for the same instance")
	}
}
