package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn wires a connection pointed at srv.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{"base_url": srv.URL, "api_key": "secret-key"}
}

func checkAPIKey(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("X-API-Key"); got != "secret-key" {
		t.Errorf("X-API-Key header: got %q want %q", got, "secret-key")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.Path != "/api/status" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"Version": "2.19.4"})
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "status", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["Version"] != "2.19.4" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.Path != "/api/endpoints" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{map[string]any{"Id": float64(1), "Name": "local"}})
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "endpoints", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["Name"] != "local" {
		t.Fatalf("items (bare-array hoist): %#v", res.Outputs["items"])
	}
}

func TestEndpointGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.Path != "/api/endpoints/1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"Id": float64(1), "Name": "local"})
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "endpoint_get", Connection: testConn(srv),
		Options: map[string]any{"endpoint_id": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["Name"] != "local" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestStacks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.Path != "/api/stacks" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{map[string]any{"Id": float64(5), "Name": "mystack"}})
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "stacks", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["Name"] != "mystack" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestStackGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.Path != "/api/stacks/5" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"Id": float64(5), "Name": "mystack"})
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "stack_get", Connection: testConn(srv),
		Options: map[string]any{"stack_id": "5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["Name"] != "mystack" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestStackStartStop(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, 200, map[string]any{"Id": float64(5), "Status": 1})
	}))
	defer srv.Close()

	p := newPortainerPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "stack_start", Connection: testConn(srv),
		Options: map[string]any{"stack_id": "5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/stacks/5/start" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "stack_stop", Connection: testConn(srv),
		Options: map[string]any{"stack_id": "5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/stacks/5/stop" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestStackDelete(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "stack_delete", Connection: testConn(srv),
		Options: map[string]any{"stack_id": "5", "endpoint_id": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/stacks/5" || gotQuery != "endpointId=1" {
		t.Fatalf("request: %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if res.Outputs["status_code"] != 204 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestContainers(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, 200, []any{map[string]any{"Id": "c1", "Names": []any{"/web"}}})
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "containers", Connection: testConn(srv),
		Options: map[string]any{"endpoint_id": "1", "all": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/endpoints/1/docker/containers/json" || gotQuery != "all=1" {
		t.Fatalf("request: %s?%s", gotPath, gotQuery)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["Id"] != "c1" {
		t.Fatalf("items (bare-array hoist): %#v", res.Outputs["items"])
	}
}

func TestContainerAction(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "container_action", Connection: testConn(srv),
		Options: map[string]any{"endpoint_id": "1", "container_id": "c1", "action": "restart"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/endpoints/1/docker/containers/c1/restart" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["status_code"] != 204 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestContainerActionInvalidAction(t *testing.T) {
	p := newPortainerPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "container_action",
		Connection: map[string]any{"base_url": "http://example.invalid", "api_key": "k"},
		Options:    map[string]any{"endpoint_id": "1", "container_id": "c1", "action": "nuke"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for invalid action, got %v", err)
	}
}

func TestContainerLogs(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("line one\nline two\n"))
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "container_logs", Connection: testConn(srv),
		Options: map[string]any{"endpoint_id": "1", "container_id": "c1", "tail": "100"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/endpoints/1/docker/containers/c1/logs" {
		t.Fatalf("path: got %q", gotPath)
	}
	if gotQuery != "stdout=1&tail=100" {
		t.Fatalf("query: got %q", gotQuery)
	}
	if res.Outputs["logs"] != "line one\nline two\n" {
		t.Fatalf("logs: %#v", res.Outputs["logs"])
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestContainerLogsStdoutFalse(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotQuery = r.URL.RawQuery
		w.WriteHeader(200)
		_, _ = w.Write([]byte("err line\n"))
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "container_logs", Connection: testConn(srv),
		Options: map[string]any{"endpoint_id": "1", "container_id": "c1", "stdout": false, "stderr": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "stderr=1" {
		t.Fatalf("query: got %q (expected stdout omitted, stderr=1)", gotQuery)
	}
}

func TestImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.Path != "/api/endpoints/1/docker/images/json" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{map[string]any{"Id": "sha256:abc"}})
	}))
	defer srv.Close()

	p := newPortainerPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "images", Connection: testConn(srv),
		Options: map[string]any{"endpoint_id": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["Id"] != "sha256:abc" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/status":
			writeJSON(w, 200, map[string]any{"Version": "2.19.4"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints" && r.URL.Query().Get("limit") == "1":
			writeJSON(w, 200, []any{map[string]any{"Id": float64(1)}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newPortainerPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/status"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["Version"] != "2.19.4" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/endpoints", "query": map[string]any{"limit": "1"}},
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
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer srv.Close()

	p := newPortainerPlugin()
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
	if !containsAll(pe.Message, "401", "invalid api key") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newPortainerPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "api_key": "k"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"endpoint_get", map[string]any{}},
		{"stack_get", map[string]any{}},
		{"stack_start", map[string]any{}},
		{"stack_stop", map[string]any{}},
		{"stack_delete", map[string]any{}},
		{"containers", map[string]any{}},
		{"container_action", map[string]any{}},
		{"container_action", map[string]any{"endpoint_id": "1"}},
		{"container_action", map[string]any{"endpoint_id": "1", "container_id": "c1"}},
		{"container_logs", map[string]any{}},
		{"container_logs", map[string]any{"endpoint_id": "1"}},
		{"images", map[string]any{}},
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s %#v: expected error for missing required options", tc.verb, tc.opts)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s %#v: expected CodeInvalidParams, got %v", tc.verb, tc.opts, err)
		}
	}
}

func TestMissingConnection(t *testing.T) {
	p := newPortainerPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "status", Connection: map[string]any{"api_key": "k"}}); err == nil {
		t.Error("expected error for missing base_url")
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "status", Connection: map[string]any{"base_url": "http://x"}}); err == nil {
		t.Error("expected error for missing api_key")
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newPortainerPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: map[string]any{"base_url": "http://x", "api_key": "k"}})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDescribe(t *testing.T) {
	p := newPortainerPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "portainer" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["base_url"].Type != "string" || !d.Connection["base_url"].Required {
		t.Errorf("connection.base_url: %#v", d.Connection["base_url"])
	}
	if d.Connection["api_key"].Type != "string" || !d.Connection["api_key"].Required {
		t.Errorf("connection.api_key: %#v", d.Connection["api_key"])
	}
	if d.Connection["insecure_skip_verify"].Type != "boolean" {
		t.Errorf("connection.insecure_skip_verify: %#v", d.Connection["insecure_skip_verify"])
	}
	want := []string{
		"status", "endpoints", "endpoint_get", "stacks", "stack_get", "stack_start",
		"stack_stop", "stack_delete", "containers", "container_action", "container_logs",
		"images", "api",
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
