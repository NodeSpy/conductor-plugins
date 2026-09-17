package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{"base_url": srv.URL, "api_key": "secret-key"}
}

func checkBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
		t.Errorf("Authorization header: got %q want %q", got, "Bearer secret-key")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/health" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"database": "ok", "version": "11.0.0"})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "health", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["database"] != "ok" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/search" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("query") != "cpu" || q.Get("type") != "dash-db" {
			t.Errorf("query: got %v", q)
		}
		if !reflect.DeepEqual(q["tag"], []string{"prod", "infra"}) {
			t.Errorf("tag query: got %v", q["tag"])
		}
		writeJSON(w, 200, []any{map[string]any{"uid": "d1", "title": "CPU usage"}})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "search", Connection: testConn(srv),
		Options: map[string]any{"query": "cpu", "type": "dash-db", "tag": []any{"prod", "infra"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["uid"] != "d1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestDashboardGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/dashboards/uid/abc123" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"dashboard": map[string]any{"uid": "abc123"}})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "dashboard_get", Connection: testConn(srv),
		Options: map[string]any{"uid": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["dashboard"]; !ok {
		t.Fatalf("result missing dashboard: %#v", result)
	}
}

func TestDashboardCreate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api/dashboards/db" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeJSON(w, 200, map[string]any{"uid": "new1", "status": "success"})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "dashboard_create", Connection: testConn(srv),
		Options: map[string]any{
			"dashboard":  map[string]any{"title": "New dash"},
			"folder_uid": "fold1",
			"overwrite":  true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"dashboard": map[string]any{"title": "New dash"},
		"folderUid": "fold1",
		"overwrite": true,
	}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["uid"] != "new1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestDashboardDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, 200, map[string]any{"title": "deleted-dash"})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "dashboard_delete", Connection: testConn(srv),
		Options: map[string]any{"uid": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/dashboards/uid/abc123" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestDatasourcesAndFolders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch r.URL.Path {
		case "/api/datasources":
			writeJSON(w, 200, []any{map[string]any{"uid": "ds1", "name": "Prometheus"}})
		case "/api/datasources/uid/ds1":
			writeJSON(w, 200, map[string]any{"uid": "ds1", "name": "Prometheus"})
		case "/api/folders":
			writeJSON(w, 200, []any{map[string]any{"uid": "f1", "title": "Ops"}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newGrafanaPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "datasources", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["uid"] != "ds1" {
		t.Fatalf("datasources items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "datasource_get", Connection: testConn(srv), Options: map[string]any{"uid": "ds1"}})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["name"] != "Prometheus" {
		t.Fatalf("datasource_get result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "folders", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok = res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["uid"] != "f1" {
		t.Fatalf("folders items: %#v", res.Outputs["items"])
	}
}

func TestFolderCreate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api/folders" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, 200, map[string]any{"uid": "f2", "title": "New folder"})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "folder_create", Connection: testConn(srv),
		Options: map[string]any{"title": "New folder"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotBody, map[string]any{"title": "New folder"}) {
		t.Fatalf("request body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["uid"] != "f2" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAlertRules(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/provisioning/alert-rules" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, []any{map[string]any{"uid": "rule1", "title": "High CPU"}})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "alert_rules", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["uid"] != "rule1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAnnotations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/annotations" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("from") != "1000" || q.Get("to") != "2000" {
			t.Errorf("query: got %v", q)
		}
		if !reflect.DeepEqual(q["tags"], []string{"deploy"}) {
			t.Errorf("tags query: got %v", q["tags"])
		}
		writeJSON(w, 200, []any{map[string]any{"id": 1, "text": "deployed v2"}})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "annotations", Connection: testConn(srv),
		Options: map[string]any{"from": 1000, "to": 2000, "tags": []any{"deploy"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAnnotationCreate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api/annotations" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, 200, map[string]any{"id": 5, "message": "Annotation added"})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "annotation_create", Connection: testConn(srv),
		Options: map[string]any{
			"dashboard_uid": "abc123", "panel_id": 4, "time": 1000, "time_end": 2000,
			"tags": []any{"deploy"}, "text": "deployed v2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"dashboardUID": "abc123", "panelId": float64(4), "time": float64(1000), "timeEnd": float64(2000),
		"tags": []any{"deploy"}, "text": "deployed v2",
	}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestOrg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/org" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": 1, "name": "Main Org."})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "org", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["name"] != "Main Org." {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/ping":
			writeJSON(w, 200, map[string]any{"ok": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/datasources" && r.URL.Query().Get("accessControl") == "1":
			writeJSON(w, 200, []any{map[string]any{"uid": "ds1"}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newGrafanaPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/api/ping"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["ok"] != true {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/api/datasources", "query": map[string]any{"accessControl": "1"}},
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
		_, _ = w.Write([]byte(`{"message":"invalid API key"}`))
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "org", Connection: testConn(srv)})
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
	if !containsAll(pe.Message, "401", "invalid API key") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newGrafanaPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "api_key": "k"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"dashboard_get", map[string]any{}},
		{"dashboard_create", map[string]any{}},
		{"dashboard_delete", map[string]any{}},
		{"datasource_get", map[string]any{}},
		{"folder_create", map[string]any{}},
		{"annotation_create", map[string]any{}},
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
	p := newGrafanaPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "org", Connection: map[string]any{"api_key": "k"}}); err == nil {
		t.Error("expected error for missing base_url")
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "org", Connection: map[string]any{"base_url": "http://x"}}); err == nil {
		t.Error("expected error for missing api_key")
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newGrafanaPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: map[string]any{"base_url": "http://x", "api_key": "k"}})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

// TestInsecureSkipVerify proves the connection flag actually controls TLS
// verification: without it, a self-signed test server's certificate is
// rejected; with it, the same server is reachable.
func TestInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		writeJSON(w, 200, map[string]any{"database": "ok"})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()

	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "health",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "secret-key"},
	})
	if err == nil {
		t.Fatal("expected TLS verification error without insecure_skip_verify")
	}

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "health",
		Connection: map[string]any{
			"base_url": srv.URL, "api_key": "secret-key",
			"insecure_skip_verify": true,
		},
	})
	if err != nil {
		t.Fatalf("expected success with insecure_skip_verify=true, got %v", err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestDescribe(t *testing.T) {
	p := newGrafanaPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "grafana" {
		t.Errorf("type: got %q", d.Type)
	}
	if d.Connection["base_url"].Type != "string" || !d.Connection["base_url"].Required {
		t.Errorf("connection.base_url: %#v", d.Connection["base_url"])
	}
	if d.Connection["api_key"].Type != "string" || !d.Connection["api_key"].Required {
		t.Errorf("connection.api_key: %#v", d.Connection["api_key"])
	}
	if f := d.Connection["insecure_skip_verify"]; f.Type != "boolean" || f.Required {
		t.Errorf("connection.insecure_skip_verify: %#v", f)
	}
	if f := d.Connection["poll_interval"]; f.Type != "duration" {
		t.Errorf("connection.poll_interval: %#v", f)
	}
	want := []string{
		"health", "search", "dashboard_get", "dashboard_create", "dashboard_delete",
		"datasources", "datasource_get", "folders", "folder_create", "alert_rules",
		"annotations", "annotation_create", "org", "api",
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
	if len(d.Events) != 1 || d.Events[0].Name != "alert" {
		t.Fatalf("events: %#v", d.Events)
	}
	for _, f := range []string{"severities", "alertnames"} {
		if _, ok := d.Events[0].Filters[f]; !ok {
			t.Errorf("alert event missing filter %q", f)
		}
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Errorf("expected empty egress (self-hosted), got %v", d.Capabilities.Egress)
	}
}

// --- source: pure emit-decision tests ---

func mustAlert(t *testing.T, s string) grafanaAlert {
	t.Helper()
	var a grafanaAlert
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		t.Fatalf("canned alert JSON: %v", err)
	}
	return a
}

const (
	activeAlertJSON = `{
	  "labels": {"alertname": "HighCPU", "severity": "critical"},
	  "annotations": {"summary": "CPU usage above 90%"},
	  "startsAt": "2026-09-01T00:00:00Z",
	  "fingerprint": "abc123",
	  "status": {"state": "active", "silencedBy": [], "inhibitedBy": []}
	}`
	resolvedAlertJSON = `{
	  "labels": {"alertname": "HighCPU", "severity": "critical"},
	  "annotations": {"summary": "CPU usage above 90%"},
	  "startsAt": "2026-09-01T00:00:00Z",
	  "endsAt": "2026-09-01T00:05:00Z",
	  "fingerprint": "abc123",
	  "status": {"state": "suppressed", "silencedBy": ["sil1"], "inhibitedBy": []}
	}`
)

func TestAlertEvent(t *testing.T) {
	t.Run("active alert emits", func(t *testing.T) {
		ev, ok := alertEvent(mustAlert(t, activeAlertJSON))
		if !ok {
			t.Fatal("expected an event")
		}
		if ev["event"] != "alert" || ev["kind"] != "alert" {
			t.Errorf("event/kind: %#v", ev)
		}
		if ev["dedup"] != "abc123" {
			t.Errorf("dedup: %#v", ev["dedup"])
		}
		ctx, _ := ev["context"].(map[string]any)
		if ctx["fingerprint"] != "abc123" || ctx["alertname"] != "HighCPU" || ctx["severity"] != "critical" {
			t.Errorf("context: %#v", ctx)
		}
		if ctx["summary"] != "CPU usage above 90%" || ctx["startsAt"] != "2026-09-01T00:00:00Z" {
			t.Errorf("context: %#v", ctx)
		}
		status, ok := ctx["status"].(map[string]any)
		if !ok || status["state"] != "active" {
			t.Errorf("context.status: %#v", ctx["status"])
		}
		// plural aliases for the documented filters
		if ctx["severities"] != "critical" || ctx["alertnames"] != "HighCPU" {
			t.Errorf("filter aliases: %#v", ctx)
		}
	})

	t.Run("non-active state does not emit", func(t *testing.T) {
		if _, ok := alertEvent(mustAlert(t, resolvedAlertJSON)); ok {
			t.Fatal("expected no event for a non-active alert")
		}
	})
}

// TestAlertDedup proves the source deduplicates on fingerprint: a
// persistently-firing alert would be re-fetched every poll cycle, but the
// dedup set suppresses every emit after the first.
func TestAlertDedup(t *testing.T) {
	seen := map[string]bool{}
	emitted := 0
	add := func(key string) bool {
		if seen[key] {
			return false
		}
		seen[key] = true
		return true
	}
	for i := 0; i < 3; i++ {
		ev, ok := alertEvent(mustAlert(t, activeAlertJSON))
		if ok && add(ev["dedup"].(string)) {
			emitted++
		}
	}
	if emitted != 1 {
		t.Fatalf("emitted %d times, want 1", emitted)
	}
}

// TestPollAlerts drives the HTTP+decode half of the source against a fake
// Alertmanager v2 alerts endpoint.
func TestPollAlerts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != alertsPath {
			t.Errorf("path: got %q want %q", r.URL.Path, alertsPath)
		}
		writeJSON(w, 200, []any{
			json.RawMessage(activeAlertJSON),
		})
	}))
	defer srv.Close()

	p := newGrafanaPlugin()
	conn, err := parseConn(map[string]any{"base_url": srv.URL, "api_key": "secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := p.pollAlerts(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Fingerprint != "abc123" {
		t.Fatalf("alerts: %#v", alerts)
	}
}

func TestParseConnPollInterval(t *testing.T) {
	conn, err := parseConn(map[string]any{"base_url": "http://x", "api_key": "k"})
	if err != nil {
		t.Fatal(err)
	}
	if conn.pollInterval != time.Minute {
		t.Errorf("default poll_interval: got %v want 1m", conn.pollInterval)
	}

	conn, err = parseConn(map[string]any{"base_url": "http://x", "api_key": "k", "poll_interval": "5m"})
	if err != nil {
		t.Fatal(err)
	}
	if conn.pollInterval != 5*time.Minute {
		t.Errorf("poll_interval: got %v want 5m", conn.pollInterval)
	}

	if _, err := parseConn(map[string]any{"base_url": "http://x", "api_key": "k", "poll_interval": "nope"}); err == nil {
		t.Error("expected error for an unparseable poll_interval")
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
