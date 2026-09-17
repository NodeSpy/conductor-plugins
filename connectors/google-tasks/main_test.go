package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn builds a connection map with the managed-OAuth2 token already
// injected (as the daemon would do), pointed at srv.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{
		plugin.AccessTokenKey: "test-token",
		"api_base":            srv.URL,
	}
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

func TestTasklistsVerb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/users/@me/lists" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"items": []any{
			map[string]any{"id": "@default", "title": "My Tasks"},
		}})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "tasklists", Connection: testConn(srv)})
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
	if items[0].(map[string]any)["id"] != "@default" {
		t.Errorf("items[0]: %#v", items[0])
	}
}

func TestTasklistGetDefaultsToAtDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/users/@me/lists/@default" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "@default", "title": "My Tasks"})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "tasklist_get", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["title"] != "My Tasks" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestTasklistGetExplicitTasklist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/users/@me/lists/abc123" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "abc123"})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "tasklist_get", Connection: testConn(srv),
		Options: map[string]any{"tasklist": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTasklistCreate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/users/@me/lists" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"id": "new-list", "title": gotBody["title"]})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "tasklist_create", Connection: testConn(srv),
		Options: map[string]any{"title": "Groceries"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["title"] != "Groceries" {
		t.Fatalf("body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "new-list" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestTasksHoistsItemsAndSendsQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/lists/@default/tasks" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("showCompleted") != "false" || q.Get("showHidden") != "true" ||
			q.Get("maxResults") != "10" || q.Get("dueMin") != "2026-01-01T00:00:00Z" ||
			q.Get("dueMax") != "2026-02-01T00:00:00Z" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"id": "t1"}}})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "tasks", Connection: testConn(srv),
		Options: map[string]any{
			"showCompleted": false, "showHidden": true, "maxResults": 10,
			"dueMin": "2026-01-01T00:00:00Z", "dueMax": "2026-02-01T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "t1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestTasksTasklistOptionOverridesDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/lists/work-list/tasks" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "tasks", Connection: testConn(srv),
		Options: map[string]any{"tasklist": "work-list"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTaskGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/lists/@default/tasks/t1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "t1", "title": "Buy milk"})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "task_get", Connection: testConn(srv),
		Options: map[string]any{"task_id": "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["title"] != "Buy milk" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestTaskCreate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/lists/@default/tasks" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"id": "t-new", "title": gotBody["title"]})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "task_create", Connection: testConn(srv),
		Options: map[string]any{
			"title":  "Buy milk",
			"notes":  "2%",
			"due":    "2026-03-01T00:00:00Z",
			"status": "needsAction",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["title"] != "Buy milk" || gotBody["notes"] != "2%" ||
		gotBody["due"] != "2026-03-01T00:00:00Z" || gotBody["status"] != "needsAction" {
		t.Fatalf("body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "t-new" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestTaskUpdateConvenienceFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPatch || r.URL.Path != "/lists/@default/tasks/t1" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"id": "t1", "title": gotBody["title"]})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "task_update", Connection: testConn(srv),
		Options: map[string]any{"task_id": "t1", "title": "Updated title"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["title"] != "Updated title" {
		t.Fatalf("body: %#v", gotBody)
	}
	if _, ok := gotBody["notes"]; ok {
		t.Fatalf("body should not include unset convenience fields: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["title"] != "Updated title" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestTaskUpdateTaskMapOverridesConvenience(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"id": "t1"})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "task_update", Connection: testConn(srv),
		Options: map[string]any{
			"task_id": "t1",
			"title":   "should be ignored",
			"task":    map[string]any{"title": "Raw title", "notes": "raw notes"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["title"] != "Raw title" || gotBody["notes"] != "raw notes" {
		t.Fatalf("body should come from task map, got %#v", gotBody)
	}
}

func TestTaskUpdateRequiresSomeField(t *testing.T) {
	p := newGoogleTasksPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "task_update", Connection: conn,
		Options: map[string]any{"task_id": "t1"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestTaskDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "task_delete", Connection: testConn(srv),
		Options: map[string]any{"task_id": "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/lists/@default/tasks/t1" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["status_code"] != 204 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestTaskComplete(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPatch || r.URL.Path != "/lists/@default/tasks/t1" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"id": "t1", "status": gotBody["status"]})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "task_complete", Connection: testConn(srv),
		Options: map[string]any{"task_id": "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["status"] != "completed" {
		t.Fatalf("body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "completed" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestTaskMove(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/lists/@default/tasks/t1/move" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("parent") != "t-parent" || q.Get("previous") != "t-prev" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{"id": "t1", "parent": "t-parent"})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "task_move", Connection: testConn(srv),
		Options: map[string]any{"task_id": "t1", "parent": "t-parent", "previous": "t-prev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["parent"] != "t-parent" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/@me/lists" && r.URL.Query().Get("maxResults") == "5":
			writeJSON(w, 200, []any{map[string]any{"id": "@default"}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/users/@me/lists", "query": map[string]any{"maxResults": "5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "@default" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIEscapeHatchObjectResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/lists/@default/tasks" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "t-api"})
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "POST", "path": "/lists/@default/tasks", "body": map[string]any{"title": "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "t-api" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer srv.Close()

	p := newGoogleTasksPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "tasklists", Connection: testConn(srv)})
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
	if !containsAll(pe.Message, "401", "invalid_token") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingAccessTokenIsInvalidParams(t *testing.T) {
	p := newGoogleTasksPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "tasklists",
		Connection: map[string]any{}, // no access_token injected
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing access token, got %v", err)
	}
	if !containsAll(pe.Message, "access token", "conductor connector auth google-tasks") {
		t.Errorf("message should point at the auth: block / login flow, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newGoogleTasksPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"tasklist_create", map[string]any{}},
		{"task_get", map[string]any{}},
		{"task_create", map[string]any{}},
		{"task_update", map[string]any{}},
		{"task_update", map[string]any{"task_id": "t1"}}, // no task map, no convenience field
		{"task_delete", map[string]any{}},
		{"task_complete", map[string]any{}},
		{"task_move", map[string]any{}},
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
			t.Errorf("%s %v: expected CodeInvalidParams, got %v", tc.verb, tc.opts, err)
		}
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newGoogleTasksPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{plugin.AccessTokenKey: "t"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for unknown verb, got %v", err)
	}
}

func TestDescribe(t *testing.T) {
	p := newGoogleTasksPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "google-tasks" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["api_base"].Type != "string" || d.Connection["api_base"].Required {
		t.Errorf("connection.api_base: %#v", d.Connection["api_base"])
	}

	want := []string{
		"tasklists", "tasklist_get", "tasklist_create",
		"tasks", "task_get", "task_create", "task_update", "task_delete",
		"task_complete", "task_move", "api",
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

	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "tasks.googleapis.com:443" {
		t.Errorf("egress: got %v want [tasks.googleapis.com:443]", d.Capabilities.Egress)
	}

	if d.Auth == nil {
		t.Fatal("expected Describe().Auth to be non-nil (managed OAuth2)")
	}
	if d.Auth.TokenURL != "https://oauth2.googleapis.com/token" {
		t.Errorf("Auth.TokenURL: got %q", d.Auth.TokenURL)
	}
	if d.Auth.AuthURL != "https://accounts.google.com/o/oauth2/v2/auth" {
		t.Errorf("Auth.AuthURL: got %q", d.Auth.AuthURL)
	}
	foundTasksScope := false
	for _, s := range d.Auth.Scopes {
		if s == "https://www.googleapis.com/auth/tasks" {
			foundTasksScope = true
		}
	}
	if !foundTasksScope {
		t.Errorf("Auth.Scopes missing tasks scope: %v", d.Auth.Scopes)
	}
	if d.Auth.AuthParams["access_type"] != "offline" {
		t.Errorf("Auth.AuthParams[access_type]: got %q want %q", d.Auth.AuthParams["access_type"], "offline")
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
