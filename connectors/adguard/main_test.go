package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn wires a fresh connection map pointed at srv.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{"base_url": srv.URL, "username": "admin", "password": "hunter2"}
}

func checkBasicAuth(t *testing.T, r *http.Request) {
	t.Helper()
	user, pass, ok := r.BasicAuth()
	if !ok || user != "admin" || pass != "hunter2" {
		t.Errorf("BasicAuth: got user=%q pass=%q ok=%v, want user=%q pass=%q ok=true", user, pass, ok, "admin", "hunter2")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.URL.Path != "/control/status" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"version": "v0.107.0", "protection_enabled": true, "running": true})
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "status", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["version"] != "v0.107.0" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.URL.Path != "/control/stats" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"num_dns_queries": float64(42)})
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "stats", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["num_dns_queries"] != float64(42) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestQuerylog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.URL.Path != "/control/querylog" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("older_than") != "2024-01-01T00:00:00Z" || q.Get("limit") != "10" || q.Get("search") != "example.com" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{
			"oldest": "2023-12-31T00:00:00Z",
			"data":   []any{map[string]any{"question": map[string]any{"name": "example.com."}}},
		})
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "querylog", Connection: testConn(srv),
		Options: map[string]any{"older_than": "2024-01-01T00:00:00Z", "limit": 10, "search": "example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items (hoisted from result.data): %#v", res.Outputs["items"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["oldest"] != "2023-12-31T00:00:00Z" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestProtection(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/control/protection" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type: got %q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "protection", Connection: testConn(srv),
		Options: map[string]any{"enabled": false, "duration": 60000},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"enabled": false, "duration": float64(60000)}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestProtectionMissingEnabled(t *testing.T) {
	p := newAdguardPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "protection", Connection: map[string]any{"base_url": "http://x", "username": "u", "password": "p"},
		Options: map[string]any{},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestFilteringStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.URL.Path != "/control/filtering/status" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"enabled": true, "filters": []any{}})
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "filtering_status", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["enabled"] != true {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestFilteringAddURL(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/control/filtering/add_url" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "filtering_add_url", Connection: testConn(srv),
		Options: map[string]any{"name": "OISD", "url": "https://example.com/oisd.txt", "whitelist": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"name": "OISD", "url": "https://example.com/oisd.txt", "whitelist": false}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestFilteringRemoveURL(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/control/filtering/remove_url" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "filtering_remove_url", Connection: testConn(srv),
		Options: map[string]any{"url": "https://example.com/oisd.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"url": "https://example.com/oisd.txt", "whitelist": false}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
}

func TestFilteringSetRules(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/control/filtering/set_rules" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "filtering_set_rules", Connection: testConn(srv),
		Options: map[string]any{"rules": []any{"||example.com^", "@@||allow.com^"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"rules": []any{"||example.com^", "@@||allow.com^"}}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
}

func TestRewrites(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.URL.Path != "/control/rewrite/list" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{map[string]any{"domain": "example.com", "answer": "1.2.3.4"}})
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "rewrites", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["domain"] != "example.com" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestRewriteAddAndDelete(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newAdguardPlugin()

	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "rewrite_add", Connection: testConn(srv),
		Options: map[string]any{"domain": "example.com", "answer": "1.2.3.4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/control/rewrite/add" {
		t.Errorf("path: got %q", gotPath)
	}
	want := map[string]any{"domain": "example.com", "answer": "1.2.3.4"}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}

	_, err = p.Invoke(plugin.InvokeRequest{
		Verb: "rewrite_delete", Connection: testConn(srv),
		Options: map[string]any{"domain": "example.com", "answer": "1.2.3.4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/control/rewrite/delete" {
		t.Errorf("path: got %q", gotPath)
	}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
}

func TestClients(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.URL.Path != "/control/clients" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{
			"clients":      []any{map[string]any{"name": "laptop"}},
			"auto_clients": []any{},
		})
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "clients", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["name"] != "laptop" {
		t.Fatalf("items (hoisted from result.clients): %#v", res.Outputs["items"])
	}
}

func TestDNSInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.URL.Path != "/control/dns_info" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"upstream_dns": []any{"1.1.1.1"}})
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "dns_info", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestDNSConfig(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/control/dns_config" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "dns_config", Connection: testConn(srv),
		Options: map[string]any{"config": map[string]any{"cache_enabled": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"cache_enabled": true}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
}

func TestSafebrowsingToggle(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newAdguardPlugin()

	_, err := p.Invoke(plugin.InvokeRequest{Verb: "safebrowsing_toggle", Connection: testConn(srv), Options: map[string]any{"enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/control/safebrowsing/enable" {
		t.Errorf("path: got %q", gotPath)
	}

	_, err = p.Invoke(plugin.InvokeRequest{Verb: "safebrowsing_toggle", Connection: testConn(srv), Options: map[string]any{"enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/control/safebrowsing/disable" {
		t.Errorf("path: got %q", gotPath)
	}
}

func TestParentalToggle(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newAdguardPlugin()

	_, err := p.Invoke(plugin.InvokeRequest{Verb: "parental_toggle", Connection: testConn(srv), Options: map[string]any{"enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/control/parental/enable" {
		t.Errorf("path: got %q", gotPath)
	}

	_, err = p.Invoke(plugin.InvokeRequest{Verb: "parental_toggle", Connection: testConn(srv), Options: map[string]any{"enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/control/parental/disable" {
		t.Errorf("path: got %q", gotPath)
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/control/i18n/current_language":
			writeJSON(w, 200, "en")
		case r.Method == http.MethodGet && r.URL.Path == "/control/rewrite/list" && r.URL.Query().Get("foo") == "bar":
			writeJSON(w, 200, []any{map[string]any{"domain": "x"}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newAdguardPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/i18n/current_language"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["result"] != "en" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/rewrite/list", "query": map[string]any{"foo": "bar"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIMissingPath(t *testing.T) {
	p := newAdguardPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: map[string]any{"base_url": "http://x", "username": "u", "password": "p"},
		Options: map[string]any{},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid credentials"}`))
	}))
	defer srv.Close()

	p := newAdguardPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "status", Connection: testConn(srv)})
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
	if !containsAll(pe.Message, "401", "invalid credentials") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newAdguardPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "username": "u", "password": "p"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"protection", map[string]any{}},
		{"filtering_add_url", map[string]any{}},
		{"filtering_add_url", map[string]any{"name": "n"}}, // missing url
		{"filtering_remove_url", map[string]any{}},
		{"filtering_set_rules", map[string]any{}},
		{"rewrite_add", map[string]any{}},
		{"rewrite_add", map[string]any{"domain": "d"}}, // missing answer
		{"rewrite_delete", map[string]any{}},
		{"dns_config", map[string]any{}},
		{"safebrowsing_toggle", map[string]any{}},
		{"parental_toggle", map[string]any{}},
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
	p := newAdguardPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "status", Connection: map[string]any{"username": "u", "password": "p"}}); err == nil {
		t.Error("expected error for missing base_url")
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "status", Connection: map[string]any{"base_url": "http://x", "password": "p"}}); err == nil {
		t.Error("expected error for missing username")
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "status", Connection: map[string]any{"base_url": "http://x", "username": "u"}}); err == nil {
		t.Error("expected error for missing password")
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newAdguardPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: map[string]any{"base_url": "http://x", "username": "u", "password": "p"}})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDescribe(t *testing.T) {
	p := newAdguardPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "adguard" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["base_url"].Type != "string" || !d.Connection["base_url"].Required {
		t.Errorf("connection.base_url: %#v", d.Connection["base_url"])
	}
	if d.Connection["username"].Type != "string" || !d.Connection["username"].Required {
		t.Errorf("connection.username: %#v", d.Connection["username"])
	}
	if d.Connection["password"].Type != "string" || !d.Connection["password"].Required {
		t.Errorf("connection.password: %#v", d.Connection["password"])
	}
	if d.Connection["insecure_skip_verify"].Type != "boolean" || d.Connection["insecure_skip_verify"].Required {
		t.Errorf("connection.insecure_skip_verify: %#v", d.Connection["insecure_skip_verify"])
	}
	want := []string{
		"status", "stats", "querylog", "protection", "filtering_status",
		"filtering_add_url", "filtering_remove_url", "filtering_set_rules",
		"rewrites", "rewrite_add", "rewrite_delete", "clients", "dns_info",
		"dns_config", "safebrowsing_toggle", "parental_toggle", "api",
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
