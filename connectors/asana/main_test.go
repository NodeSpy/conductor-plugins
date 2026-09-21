package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// --- Describe ---

func TestDescribe(t *testing.T) {
	d := newAsanaPlugin().Describe()
	if d.Kind != plugin.KindConnector {
		t.Fatalf("Kind = %v, want KindConnector", d.Kind)
	}
	if d.Type != "asana" {
		t.Fatalf("Type = %q, want asana", d.Type)
	}
	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "app.asana.com:443" {
		t.Fatalf("Capabilities.Egress = %v, want exactly [app.asana.com:443]", d.Capabilities.Egress)
	}
	if d.Capabilities.Spawns || len(d.Capabilities.Commands) != 0 || len(d.Capabilities.FS) != 0 {
		t.Fatalf("Capabilities should declare no commands/spawns/fs: %#v", d.Capabilities)
	}
	if d.Auth == nil || d.Auth.TokenURL != "https://app.asana.com/-/oauth_token" || d.Auth.AuthURL != "https://app.asana.com/-/oauth_authorize" {
		t.Fatalf("Auth = %#v, want Asana's OAuth2 endpoints", d.Auth)
	}
	if !contains(d.Auth.Grants, "authorization_code") || !contains(d.Auth.Grants, "refresh_token") {
		t.Fatalf("Auth.Grants = %v", d.Auth.Grants)
	}

	wantEvents := []string{"task", "story", "project", "event"}
	gotEvents := map[string]plugin.Event{}
	for _, e := range d.Events {
		gotEvents[e.Name] = e
	}
	for _, name := range wantEvents {
		e, ok := gotEvents[name]
		if !ok {
			t.Errorf("Describe missing event %q", name)
			continue
		}
		// Every scalar filter key must also be a declared context key on the
		// (fully enriched) task event, or a filter on it could never match —
		// the daemon fails a filter whose context key is absent. The plural
		// list-typed forms are runtime aliases carrying the same scalar.
		if name == "task" {
			for k, f := range e.Filters {
				if f.Type == "list" {
					continue
				}
				if _, ok := e.Context[k]; !ok {
					t.Errorf("event %q filter key %q has no context key", name, k)
				}
			}
		}
	}
	if len(d.Events) != len(wantEvents) {
		t.Errorf("event count = %d, want %d", len(d.Events), len(wantEvents))
	}

	wantVerbs := []string{
		"me", "workspaces", "users", "projects", "get_project", "sections",
		"tasks", "get_task", "create_task", "update_task", "complete_task", "delete_task",
		"add_comment", "stories", "subtasks",
		"add_to_project", "remove_from_project", "move_to_section", "add_tag", "remove_tag", "tags",
		"search_tasks", "typeahead",
		"webhooks", "create_webhook", "delete_webhook",
		"api",
	}
	gotVerbs := map[string]bool{}
	for _, v := range d.Verbs {
		gotVerbs[v.Name] = true
	}
	for _, w := range wantVerbs {
		if !gotVerbs[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Verbs) != len(wantVerbs) {
		t.Errorf("verb count = %d, want %d", len(d.Verbs), len(wantVerbs))
	}
}

// --- connection ---

func TestParseConnPrefersManagedTokenThenPAT(t *testing.T) {
	_, tok, err := parseConn(map[string]any{"token": "pat", plugin.AccessTokenKey: "managed"})
	if err != nil || tok != "managed" {
		t.Fatalf("managed token should win: tok=%q err=%v", tok, err)
	}
	conn, tok, err := parseConn(map[string]any{"token": "pat", "api_base": "http://x/api/1.0/"})
	if err != nil || tok != "pat" || conn.apiBase != "http://x/api/1.0" {
		t.Fatalf("PAT fallback: conn=%+v tok=%q err=%v", conn, tok, err)
	}
	if _, _, err := parseConn(map[string]any{}); err == nil {
		t.Fatal("expected an error with no credentials")
	}
}

func TestMissingCredentialsIsInvalidParams(t *testing.T) {
	_, err := newAsanaPlugin().Invoke(plugin.InvokeRequest{Verb: "me", Connection: map[string]any{}})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %#v", err)
	}
}

// --- verb tests over httptest ---

type recordedRequest struct {
	method, path, rawQuery, authorization, contentType, body string
}

// apiServer answers every request with respond(r) and records each request.
func apiServer(t *testing.T, respond func(r *http.Request) (int, string)) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	recs := &[]recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		*recs = append(*recs, recordedRequest{
			method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), body: string(buf),
		})
		mu.Unlock()
		status, body := respond(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, recs
}

func fixed(status int, body string) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) { return status, body }
}

func invoke(t *testing.T, srv *httptest.Server, verb string, o map[string]any) (plugin.InvokeResult, error) {
	t.Helper()
	return newAsanaPlugin().Invoke(plugin.InvokeRequest{
		Verb:       verb,
		Connection: map[string]any{"token": "pat-123", "api_base": srv.URL},
		Options:    o,
	})
}

func last(recs *[]recordedRequest) recordedRequest { return (*recs)[len(*recs)-1] }

func decodeBody(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("request body is not JSON: %v (%s)", err, s)
	}
	return m
}

func TestMeSendsBearerAndUnwrapsData(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":{"gid":"u1","name":"Ada","workspaces":[{"gid":"w1"}]}}`))
	res, err := invoke(t, srv, "me", nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := last(recs)
	if r.method != "GET" || r.path != "/users/me" {
		t.Fatalf("request = %s %s", r.method, r.path)
	}
	if r.authorization != "Bearer pat-123" {
		t.Fatalf("Authorization = %q, want Bearer pat-123", r.authorization)
	}
	result, _ := res.Outputs["result"].(map[string]any)
	if result["name"] != "Ada" || res.Outputs["status_code"] != 200 {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
}

func TestCreateTaskWrapsDataAndHoistsOutputs(t *testing.T) {
	srv, recs := apiServer(t, fixed(201, `{"data":{"gid":"t9","name":"Ship it","permalink_url":"https://app.asana.com/0/p1/t9"}}`))
	res, err := invoke(t, srv, "create_task", map[string]any{
		"name": "Ship it", "projects": []any{"p1", "p2"}, "section": "s1",
		"assignee": "me", "due_on": "2026-10-01", "completed": false,
		"custom_fields": map[string]any{"cf1": "x"},
		"fields":        map[string]any{"notes": "raw wins"},
		"notes":         "shortcut loses",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := last(recs)
	if r.method != "POST" || r.path != "/tasks" || r.contentType != "application/json" {
		t.Fatalf("request = %s %s (%s)", r.method, r.path, r.contentType)
	}
	body := decodeBody(t, r.body)
	data, _ := body["data"].(map[string]any)
	if data == nil {
		t.Fatalf("body not wrapped in data: %s", r.body)
	}
	if data["name"] != "Ship it" || data["assignee"] != "me" || data["due_on"] != "2026-10-01" || data["completed"] != false {
		t.Fatalf("data = %#v", data)
	}
	// section → membership on the FIRST project, which then leaves `projects`.
	memberships, _ := data["memberships"].([]any)
	if len(memberships) != 1 {
		t.Fatalf("memberships = %#v", data["memberships"])
	}
	m0, _ := memberships[0].(map[string]any)
	if m0["project"] != "p1" || m0["section"] != "s1" {
		t.Fatalf("membership = %#v", m0)
	}
	projects, _ := data["projects"].([]any)
	if len(projects) != 1 || projects[0] != "p2" {
		t.Fatalf("projects = %#v, want [p2]", data["projects"])
	}
	if data["notes"] != "raw wins" {
		t.Fatalf("fields should merge last: notes = %v", data["notes"])
	}
	cf, _ := data["custom_fields"].(map[string]any)
	if cf["cf1"] != "x" {
		t.Fatalf("custom_fields = %#v", data["custom_fields"])
	}
	if res.Outputs["gid"] != "t9" || res.Outputs["url"] != "https://app.asana.com/0/p1/t9" || res.Outputs["name"] != "Ship it" || res.Outputs["status_code"] != 201 {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
}

func TestCreateTaskValidation(t *testing.T) {
	srv, recs := apiServer(t, fixed(201, `{"data":{}}`))
	for name, o := range map[string]map[string]any{
		"missing name":       {"projects": []any{"p1"}},
		"missing container":  {"name": "x"},
		"section w/o projec": {"name": "x", "section": "s1", "workspace": "w1"},
	} {
		_, err := invoke(t, srv, "create_task", o)
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %#v", name, err)
		}
	}
	if len(*recs) != 0 {
		t.Fatalf("invalid options must not reach the API: %d requests", len(*recs))
	}
	// A parent alone is a valid container (subtask).
	if _, err := invoke(t, srv, "create_task", map[string]any{"name": "sub", "parent": "t1"}); err != nil {
		t.Fatalf("parent-only create_task: %v", err)
	}
}

func TestUpdateTaskRequiresAField(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":{"gid":"t1"}}`))
	_, err := invoke(t, srv, "update_task", map[string]any{"task": "t1"})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %#v", err)
	}
	if _, err := invoke(t, srv, "update_task", map[string]any{"task": "t1", "name": "renamed"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := last(recs)
	if r.method != "PUT" || r.path != "/tasks/t1" || !strings.Contains(r.body, `"renamed"`) {
		t.Fatalf("request = %s %s %s", r.method, r.path, r.body)
	}
}

func TestCompleteTaskDefaultsTrue(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":{"gid":"t1","completed":true}}`))
	if _, err := invoke(t, srv, "complete_task", map[string]any{"task": "t1"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	data, _ := decodeBody(t, last(recs).body)["data"].(map[string]any)
	if data["completed"] != true {
		t.Fatalf("data = %#v", data)
	}
	if _, err := invoke(t, srv, "complete_task", map[string]any{"task": "t1", "completed": false}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	data, _ = decodeBody(t, last(recs).body)["data"].(map[string]any)
	if data["completed"] != false {
		t.Fatalf("data = %#v", data)
	}
}

func TestTasksRoutesBySelector(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":[]}`))
	cases := []struct {
		o        map[string]any
		wantPath string
		wantQ    string
	}{
		{map[string]any{"tag": "g1"}, "/tags/g1/tasks", ""},
		{map[string]any{"user_task_list": "u1", "completed_since": "now"}, "/user_task_lists/u1/tasks", "completed_since=now"},
		{map[string]any{"project": "p1"}, "/tasks", "project=p1"},
		{map[string]any{"section": "s1"}, "/tasks", "section=s1"},
		{map[string]any{"assignee": "me", "workspace": "w1"}, "/tasks", "assignee=me&workspace=w1"},
	}
	for _, tc := range cases {
		if _, err := invoke(t, srv, "tasks", tc.o); err != nil {
			t.Fatalf("tasks %v: %v", tc.o, err)
		}
		r := last(recs)
		if r.path != tc.wantPath || r.rawQuery != tc.wantQ {
			t.Errorf("tasks %v → %s?%s, want %s?%s", tc.o, r.path, r.rawQuery, tc.wantPath, tc.wantQ)
		}
	}
	_, err := invoke(t, srv, "tasks", map[string]any{"assignee": "me"})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("assignee without workspace: expected CodeInvalidParams, got %#v", err)
	}
}

func TestListAllFollowsNextPage(t *testing.T) {
	srv, recs := apiServer(t, func(r *http.Request) (int, string) {
		if r.URL.Query().Get("offset") == "" {
			return 200, `{"data":[{"gid":"1"},{"gid":"2"}],"next_page":{"offset":"abc","path":"/x","uri":"https://x"}}`
		}
		return 200, `{"data":[{"gid":"3"}],"next_page":null}`
	})
	res, err := invoke(t, srv, "projects", map[string]any{"workspace": "w1", "all": true, "opt_fields": []any{"name", "archived"}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	items, _ := res.Outputs["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %#v, want 3 across two pages", items)
	}
	if res.Outputs["next_offset"] != "" {
		t.Fatalf("next_offset = %v, want empty after exhausting", res.Outputs["next_offset"])
	}
	if len(*recs) != 2 {
		t.Fatalf("requests = %d, want 2", len(*recs))
	}
	first, second := (*recs)[0], (*recs)[1]
	if !strings.Contains(first.rawQuery, "limit=100") || !strings.Contains(first.rawQuery, "opt_fields=name%2Carchived") || !strings.Contains(first.rawQuery, "workspace=w1") {
		t.Fatalf("first page query = %q", first.rawQuery)
	}
	if !strings.Contains(second.rawQuery, "offset=abc") {
		t.Fatalf("second page query = %q, want offset=abc", second.rawQuery)
	}
}

func TestListSinglePageReturnsNextOffset(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":[{"gid":"1"}],"next_page":{"offset":"tok"}}`))
	res, err := invoke(t, srv, "sections", map[string]any{"project": "p1", "limit": 1})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["next_offset"] != "tok" || len(*recs) != 1 {
		t.Fatalf("outputs = %#v, requests = %d", res.Outputs, len(*recs))
	}
	if last(recs).path != "/projects/p1/sections" || last(recs).rawQuery != "limit=1" {
		t.Fatalf("request = %s?%s", last(recs).path, last(recs).rawQuery)
	}
}

func TestSearchTasksBuildsAsanaParams(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":[{"gid":"t1"}]}`))
	res, err := invoke(t, srv, "search_tasks", map[string]any{
		"workspace": "w1", "text": "login", "completed": false,
		"assignee": []any{"me", "u2"}, "projects": "p1", "sort_by": "modified_at", "sort_ascending": true,
		"query": map[string]any{"due_on.before": "2026-10-01"}, "limit": 50,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := last(recs)
	if r.path != "/workspaces/w1/tasks/search" {
		t.Fatalf("path = %s", r.path)
	}
	for _, want := range []string{"text=login", "completed=false", "assignee.any=me%2Cu2", "projects.any=p1", "sort_by=modified_at", "sort_ascending=true", "due_on.before=2026-10-01", "limit=50"} {
		if !strings.Contains(r.rawQuery, want) {
			t.Errorf("query %q missing %q", r.rawQuery, want)
		}
	}
	items, _ := res.Outputs["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %#v", res.Outputs["items"])
	}
}

func TestTypeaheadRequiresResourceType(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":[{"gid":"p1","name":"Roadmap"}]}`))
	_, err := invoke(t, srv, "typeahead", map[string]any{"workspace": "w1", "query": "road"})
	if pe, ok := err.(*plugin.Error); !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %#v", err)
	}
	if _, err := invoke(t, srv, "typeahead", map[string]any{"workspace": "w1", "resource_type": "project", "query": "road", "count": 5}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := last(recs)
	if r.path != "/workspaces/w1/typeahead" || !strings.Contains(r.rawQuery, "resource_type=project") || !strings.Contains(r.rawQuery, "count=5") {
		t.Fatalf("request = %s?%s", r.path, r.rawQuery)
	}
}

func TestAddCommentRequiresText(t *testing.T) {
	srv, recs := apiServer(t, fixed(201, `{"data":{"gid":"st1","text":"hi"}}`))
	_, err := invoke(t, srv, "add_comment", map[string]any{"task": "t1"})
	if pe, ok := err.(*plugin.Error); !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %#v", err)
	}
	res, err := invoke(t, srv, "add_comment", map[string]any{"task": "t1", "text": "hi", "is_pinned": true})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := last(recs)
	if r.method != "POST" || r.path != "/tasks/t1/stories" {
		t.Fatalf("request = %s %s", r.method, r.path)
	}
	data, _ := decodeBody(t, r.body)["data"].(map[string]any)
	if data["text"] != "hi" || data["is_pinned"] != true {
		t.Fatalf("data = %#v", data)
	}
	if res.Outputs["gid"] != "st1" {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
}

func TestMembershipAndTagActions(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":{}}`))
	cases := []struct {
		verb     string
		o        map[string]any
		wantPath string
		wantKey  string
	}{
		{"add_to_project", map[string]any{"task": "t1", "project": "p1", "section": "s1"}, "/tasks/t1/addProject", "section"},
		{"remove_from_project", map[string]any{"task": "t1", "project": "p1"}, "/tasks/t1/removeProject", "project"},
		{"move_to_section", map[string]any{"section": "s1", "task": "t1", "insert_after": "t0"}, "/sections/s1/addTask", "insert_after"},
		{"add_tag", map[string]any{"task": "t1", "tag": "g1"}, "/tasks/t1/addTag", "tag"},
		{"remove_tag", map[string]any{"task": "t1", "tag": "g1"}, "/tasks/t1/removeTag", "tag"},
	}
	for _, tc := range cases {
		res, err := invoke(t, srv, tc.verb, tc.o)
		if err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		r := last(recs)
		if r.method != "POST" || r.path != tc.wantPath {
			t.Errorf("%s → %s %s, want POST %s", tc.verb, r.method, r.path, tc.wantPath)
		}
		data, _ := decodeBody(t, r.body)["data"].(map[string]any)
		if _, ok := data[tc.wantKey]; !ok {
			t.Errorf("%s: data %#v missing %q", tc.verb, data, tc.wantKey)
		}
		if res.Outputs["ok"] != true {
			t.Errorf("%s: outputs = %#v", tc.verb, res.Outputs)
		}
	}
	// Missing required options never reach the API.
	before := len(*recs)
	for _, verb := range []string{"add_to_project", "remove_from_project", "move_to_section", "add_tag", "remove_tag"} {
		if _, err := invoke(t, srv, verb, map[string]any{"task": "t1"}); err == nil {
			t.Errorf("%s with only task: expected error", verb)
		}
	}
	if len(*recs) != before {
		t.Fatalf("invalid options reached the API")
	}
}

func TestDeleteTaskAndWebhook(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":{}}`))
	res, err := invoke(t, srv, "delete_task", map[string]any{"task": "t1"})
	if err != nil || res.Outputs["ok"] != true {
		t.Fatalf("delete_task: res=%#v err=%v", res.Outputs, err)
	}
	if r := last(recs); r.method != "DELETE" || r.path != "/tasks/t1" || r.body != "" {
		t.Fatalf("request = %s %s body=%q", r.method, r.path, r.body)
	}
	if _, err := invoke(t, srv, "delete_webhook", map[string]any{"webhook": "wh1"}); err != nil {
		t.Fatalf("delete_webhook: %v", err)
	}
	if r := last(recs); r.method != "DELETE" || r.path != "/webhooks/wh1" {
		t.Fatalf("request = %s %s", r.method, r.path)
	}
}

func TestCreateAndListWebhooks(t *testing.T) {
	srv, recs := apiServer(t, fixed(201, `{"data":{"gid":"wh1","active":true,"target":"https://h/asana"}}`))
	res, err := invoke(t, srv, "create_webhook", map[string]any{
		"resource": "p1", "target": "https://h/asana",
		"filters": []any{map[string]any{"resource_type": "task", "action": "changed", "fields": []any{"completed"}}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	data, _ := decodeBody(t, last(recs).body)["data"].(map[string]any)
	if data["resource"] != "p1" || data["target"] != "https://h/asana" {
		t.Fatalf("data = %#v", data)
	}
	if filters, _ := data["filters"].([]any); len(filters) != 1 {
		t.Fatalf("filters = %#v", data["filters"])
	}
	if res.Outputs["gid"] != "wh1" {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
	if _, err := invoke(t, srv, "webhooks", map[string]any{"workspace": "w1", "resource": "p1"}); err != nil {
		t.Fatalf("webhooks: %v", err)
	}
	if r := last(recs); r.path != "/webhooks" || !strings.Contains(r.rawQuery, "workspace=w1") || !strings.Contains(r.rawQuery, "resource=p1") {
		t.Fatalf("request = %s?%s", r.path, r.rawQuery)
	}
	if _, err := invoke(t, srv, "webhooks", map[string]any{}); err == nil {
		t.Fatal("webhooks without workspace: expected error")
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":[{"gid":"a1"}],"next_page":{"offset":"n"}}`))
	res, err := invoke(t, srv, "api", map[string]any{
		"method": "get", "path": "/tasks/t1/attachments", "query": map[string]any{"opt_fields": []any{"name", "host"}, "limit": 10},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := last(recs)
	if r.method != "GET" || r.path != "/tasks/t1/attachments" || !strings.Contains(r.rawQuery, "opt_fields=name%2Chost") || !strings.Contains(r.rawQuery, "limit=10") {
		t.Fatalf("request = %s %s?%s", r.method, r.path, r.rawQuery)
	}
	items, _ := res.Outputs["items"].([]any)
	if len(items) != 1 || res.Outputs["next_offset"] != "n" {
		t.Fatalf("outputs = %#v", res.Outputs)
	}

	// A bare body is wrapped in {data: ...}; an already-enveloped one is sent as-is.
	if _, err := invoke(t, srv, "api", map[string]any{"method": "POST", "path": "tasks", "body": map[string]any{"name": "x"}}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if b := decodeBody(t, last(recs).body); b["data"] == nil {
		t.Fatalf("bare body not wrapped: %s", last(recs).body)
	}
	if _, err := invoke(t, srv, "api", map[string]any{"method": "POST", "path": "tasks", "body": map[string]any{"data": map[string]any{"name": "y"}}}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if b := decodeBody(t, last(recs).body); b["data"].(map[string]any)["name"] != "y" {
		t.Fatalf("enveloped body double-wrapped: %s", last(recs).body)
	}

	for _, bad := range []map[string]any{{"method": "PATCH", "path": "x"}, {"method": "GET"}} {
		if _, err := invoke(t, srv, "api", bad); err == nil {
			t.Errorf("api %v: expected error", bad)
		}
	}
}

func TestNon2xxIsInternalErrorWithBody(t *testing.T) {
	srv, _ := apiServer(t, fixed(404, `{"errors":[{"message":"task: Unknown object: nope","help":"For more information..."}]}`))
	_, err := invoke(t, srv, "get_task", map[string]any{"task": "nope"})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %#v", err)
	}
	if !strings.Contains(pe.Message, "404") || !strings.Contains(pe.Message, "Unknown object") {
		t.Fatalf("error should carry status and Asana's message: %q", pe.Message)
	}
}

func TestGetTaskHoistsFields(t *testing.T) {
	srv, recs := apiServer(t, fixed(200, `{"data":{"gid":"t1","name":"Fix login","permalink_url":"https://app.asana.com/0/1/t1"}}`))
	res, err := invoke(t, srv, "get_task", map[string]any{"task": "t1", "opt_fields": "name,permalink_url"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if last(recs).rawQuery != "opt_fields=name%2Cpermalink_url" {
		t.Fatalf("query = %q", last(recs).rawQuery)
	}
	if res.Outputs["gid"] != "t1" || res.Outputs["name"] != "Fix login" || res.Outputs["url"] != "https://app.asana.com/0/1/t1" {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
}

func TestUnknownVerb(t *testing.T) {
	srv, _ := apiServer(t, fixed(200, `{}`))
	_, err := invoke(t, srv, "nope", nil)
	if pe, ok := err.(*plugin.Error); !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %#v", err)
	}
}

// --- source: handshake, signatures, deliveries ---

func hmacHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// newTestSource builds a source whose enrichment GETs go to api (nil disables
// enrichment) and whose emitted events land on the returned channel.
func newTestSource(t *testing.T, api *httptest.Server, secrets ...string) (*source, chan *wireEvent) {
	t.Helper()
	events := make(chan *wireEvent, 16)
	s := &source{
		instance:      "test",
		plugin:        newAsanaPlugin(),
		taskOptFields: defaultTaskOptFields,
		handshake:     len(secrets) == 0,
		handshakeOnce: len(secrets) == 0,
		dedup:         sourcekit.NewDedup(64),
		emit: func(v any) error {
			events <- v.(*wireEvent)
			return nil
		},
	}
	if api != nil {
		s.conn, s.token, s.enrich = asanaConn{apiBase: api.URL}, "pat", true
	}
	for _, sec := range secrets {
		s.addSecret(sec)
	}
	return s, events
}

func post(h http.Handler, headers map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/asana", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func waitEvent(t *testing.T, ch chan *wireEvent) *wireEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an emitted event")
		return nil
	}
}

func assertNoEvent(t *testing.T, ch chan *wireEvent) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event emitted: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}

const taskDelivery = `{"events":[{"action":"changed","created_at":"2026-09-21T10:00:00.000Z","user":{"gid":"u1","resource_type":"user"},"resource":{"gid":"t1","resource_type":"task","resource_subtype":"default_task"},"parent":{"gid":"p1","resource_type":"project"},"change":{"field":"completed","action":"changed"}}]}`

func TestHandshakeBootstrapEchoesSecretAndInstallsIt(t *testing.T) {
	s, events := newTestSource(t, nil)
	h := http.HandlerFunc(s.handle)

	rec := post(h, map[string]string{"X-Hook-Secret": "hs-secret"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("handshake status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-Hook-Secret") != "hs-secret" {
		t.Fatalf("handshake must echo X-Hook-Secret, got %q", rec.Header().Get("X-Hook-Secret"))
	}
	assertNoEvent(t, events)

	// The handshaken secret now verifies deliveries.
	rec = post(h, map[string]string{"X-Hook-Signature": hmacHex("hs-secret", []byte(taskDelivery))}, taskDelivery)
	if rec.Code != http.StatusOK {
		t.Fatalf("signed delivery status = %d, want 200", rec.Code)
	}
	ev := waitEvent(t, events)
	if ev.Event != "task" || ev.Context["gid"] != "t1" || ev.Context["action"] != "changed" || ev.Context["field"] != "completed" {
		t.Fatalf("event = %+v", ev)
	}

	// Bootstrap accepts exactly ONE handshake: the window is now closed, so a
	// second one cannot install another key.
	rec = post(h, map[string]string{"X-Hook-Secret": "intruder"}, "")
	if rec.Code != http.StatusForbidden || rec.Header().Get("X-Hook-Secret") != "" {
		t.Fatalf("second bootstrap handshake: status=%d echo=%q, want 403 and no echo", rec.Code, rec.Header().Get("X-Hook-Secret"))
	}
	if got := s.knownSecrets(); len(got) != 1 || got[0] != "hs-secret" {
		t.Fatalf("known secrets = %v, want only the first handshake's", got)
	}
}

func TestHandshakeRefusedWhenSecretPinned(t *testing.T) {
	s, _ := newTestSource(t, nil, "pinned")
	rec := post(http.HandlerFunc(s.handle), map[string]string{"X-Hook-Secret": "attacker"}, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("handshake with a pinned secret: status = %d, want 403", rec.Code)
	}
	if rec.Header().Get("X-Hook-Secret") != "" {
		t.Fatal("a refused handshake must not echo the secret")
	}
	if got := s.knownSecrets(); len(got) != 1 || got[0] != "pinned" {
		t.Fatalf("known secrets = %v, attacker's must not be installed", got)
	}

	// Explicit opt-in re-enables it for as many webhooks as needed.
	s.handshake = true
	for _, sec := range []string{"second", "third"} {
		rec = post(http.HandlerFunc(s.handle), map[string]string{"X-Hook-Secret": sec}, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("opted-in handshake %q: status=%d", sec, rec.Code)
		}
	}
	if len(s.knownSecrets()) != 3 {
		t.Fatalf("secrets = %v, want pinned + two handshaken", s.knownSecrets())
	}
}

func TestDeliveryRejectedWithNoSecretKnown(t *testing.T) {
	s, events := newTestSource(t, nil)
	rec := post(http.HandlerFunc(s.handle), map[string]string{"X-Hook-Signature": hmacHex("whatever", []byte(taskDelivery))}, taskDelivery)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (fail closed before any handshake)", rec.Code)
	}
	assertNoEvent(t, events)
}

func TestDeliveryBadSignatureRejected(t *testing.T) {
	s, events := newTestSource(t, nil, "real")
	h := http.HandlerFunc(s.handle)
	for name, sig := range map[string]string{
		"wrong secret": hmacHex("other", []byte(taskDelivery)),
		"garbage":      "deadbeef",
		"missing":      "",
	} {
		rec := post(h, map[string]string{"X-Hook-Signature": sig}, taskDelivery)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}
	// Tampered body against a valid signature for the original.
	rec := post(h, map[string]string{"X-Hook-Signature": hmacHex("real", []byte(taskDelivery))}, strings.Replace(taskDelivery, "t1", "t2", 1))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body: status = %d, want 401", rec.Code)
	}
	assertNoEvent(t, events)
}

func TestVerifyAcceptsAnyKnownSecret(t *testing.T) {
	s, _ := newTestSource(t, nil, "a", "b")
	body := []byte(taskDelivery)
	if !s.verify(body, hmacHex("a", body)) || !s.verify(body, hmacHex("b", body)) {
		t.Fatal("both pinned secrets must verify")
	}
	if s.verify(body, hmacHex("c", body)) {
		t.Fatal("an unknown secret must not verify")
	}
	s.addSecret("a") // duplicates collapse
	s.addSecret("  ")
	if got := s.knownSecrets(); len(got) != 2 {
		t.Fatalf("secrets = %v", got)
	}
}

func TestAllowUnsignedAcceptsAnything(t *testing.T) {
	s, events := newTestSource(t, nil)
	s.allowUnsigned = true
	rec := post(http.HandlerFunc(s.handle), nil, taskDelivery)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ev := waitEvent(t, events); ev.Event != "task" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestHeartbeatAndMalformedBodies(t *testing.T) {
	s, events := newTestSource(t, nil, "sec")
	h := http.HandlerFunc(s.handle)
	hb := `{"events":[]}`
	if rec := post(h, map[string]string{"X-Hook-Signature": hmacHex("sec", []byte(hb))}, hb); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want 200", rec.Code)
	}
	assertNoEvent(t, events)

	bad := `not json`
	if rec := post(h, map[string]string{"X-Hook-Signature": hmacHex("sec", []byte(bad))}, bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/asana", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func TestTaskEventIsEnriched(t *testing.T) {
	api, recs := apiServer(t, func(r *http.Request) (int, string) {
		if r.URL.Path != "/tasks/t1" {
			return 404, `{"errors":[{"message":"nope"}]}`
		}
		return 200, `{"data":{"gid":"t1","name":"Fix login","completed":true,"resource_subtype":"default_task",
			"assignee":{"gid":"u1","name":"Ada"},"due_on":"2026-10-01","due_at":null,
			"permalink_url":"https://app.asana.com/0/p1/t1","workspace":{"gid":"w1","name":"Acme"},
			"projects":[{"gid":"p1","name":"Roadmap"},{"gid":"p2","name":"Bugs"}],
			"memberships":[{"project":{"gid":"p1","name":"Roadmap"},"section":{"gid":"s1","name":"Done"}},{"project":{"gid":"p2","name":"Bugs"},"section":{"gid":"s2","name":"Triage"}}],
			"tags":[{"gid":"g1","name":"urgent"}]}}`
	})
	s, events := newTestSource(t, api, "sec")
	rec := post(http.HandlerFunc(s.handle), map[string]string{"X-Hook-Signature": hmacHex("sec", []byte(taskDelivery))}, taskDelivery)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	ev := waitEvent(t, events)
	if ev.Event != "task" || ev.Kind != "task" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Title != "asana task Fix login changed (completed)" {
		t.Fatalf("title = %q", ev.Title)
	}
	if ev.Dedup != "t1:changed:completed:2026-09-21T10:00:00.000Z:p1" {
		t.Fatalf("dedup = %q", ev.Dedup)
	}
	want := map[string]any{
		"gid": "t1", "action": "changed", "actions": "changed", "field": "completed", "fields": "completed",
		"resource_type": "task", "subtype": "default_task", "parent_gid": "p1", "parent_type": "project",
		"user": "u1", "users": "u1", "name": "Fix login", "completed": true,
		"assignee": "Ada", "assignees": "Ada", "assignee_gid": "u1", "due_on": "2026-10-01",
		"project": "Roadmap", "projects": "Roadmap", "project_gid": "p1", "section": "Done",
		"workspace": "Acme", "url": "https://app.asana.com/0/p1/t1",
	}
	for k, v := range want {
		if ev.Context[k] != v {
			t.Errorf("context[%q] = %#v, want %#v", k, ev.Context[k], v)
		}
	}
	if names, _ := ev.Context["project_names"].([]any); len(names) != 2 || names[1] != "Bugs" {
		t.Errorf("project_names = %#v", ev.Context["project_names"])
	}
	if tags, _ := ev.Context["tags"].([]any); len(tags) != 1 || tags[0] != "urgent" {
		t.Errorf("tags = %#v", ev.Context["tags"])
	}
	r := last(recs)
	if r.authorization != "Bearer pat" || !strings.Contains(r.rawQuery, "opt_fields=") {
		t.Fatalf("enrichment request = %+v", r)
	}
}

func TestEnrichmentFailureStillEmitsCompactEvent(t *testing.T) {
	api, _ := apiServer(t, fixed(500, `boom`))
	s, events := newTestSource(t, api, "sec")
	post(http.HandlerFunc(s.handle), map[string]string{"X-Hook-Signature": hmacHex("sec", []byte(taskDelivery))}, taskDelivery)
	ev := waitEvent(t, events)
	if ev.Event != "task" || ev.Context["gid"] != "t1" || ev.Title != "asana task t1 changed (completed)" {
		t.Fatalf("event = %+v", ev)
	}
	if _, enriched := ev.Context["name"]; enriched {
		t.Fatalf("failed enrichment must not fabricate fields: %+v", ev.Context)
	}
}

func TestDeletedTaskSkipsEnrichment(t *testing.T) {
	api, recs := apiServer(t, fixed(200, `{"data":{}}`))
	s, _ := newTestSource(t, api, "sec")
	ev := s.toWire(asanaEvent{Action: "deleted", Resource: asanaRef{GID: "t1", ResourceType: "task"}})
	if ev == nil || ev.Event != "task" || ev.Context["action"] != "deleted" {
		t.Fatalf("event = %+v", ev)
	}
	if len(*recs) != 0 {
		t.Fatalf("a deleted task must not be fetched: %d requests", len(*recs))
	}
}

func TestStoryEventIsEnriched(t *testing.T) {
	api, _ := apiServer(t, func(r *http.Request) (int, string) {
		if r.URL.Path != "/stories/st1" {
			return 404, `{}`
		}
		return 200, `{"data":{"gid":"st1","type":"comment","resource_subtype":"comment_added","text":"LGTM","created_by":{"gid":"u2","name":"Grace"},"target":{"gid":"t1","name":"Fix login"}}}`
	})
	s, _ := newTestSource(t, api, "sec")
	ev := s.toWire(asanaEvent{
		Action: "added", CreatedAt: "2026-09-21T10:00:00.000Z",
		Resource: asanaRef{GID: "st1", ResourceType: "story", ResourceSubtype: "comment_added"},
		Parent:   &asanaRef{GID: "t1", ResourceType: "task"},
	})
	if ev == nil || ev.Event != "story" {
		t.Fatalf("event = %+v", ev)
	}
	want := map[string]any{"text": "LGTM", "type": "comment", "author": "Grace", "task_gid": "t1", "task": "Fix login", "subtype": "comment_added", "subtypes": "comment_added", "user": "u2"}
	for k, v := range want {
		if ev.Context[k] != v {
			t.Errorf("context[%q] = %#v, want %#v", k, ev.Context[k], v)
		}
	}
	if ev.Title != "asana comment_added added on task Fix login" {
		t.Fatalf("title = %q", ev.Title)
	}
}

func TestProjectEventIsEnriched(t *testing.T) {
	api, _ := apiServer(t, fixed(200, `{"data":{"gid":"p1","name":"Roadmap","archived":false,"permalink_url":"https://app.asana.com/0/p1","owner":{"name":"Ada"},"team":{"name":"Eng"},"workspace":{"name":"Acme"}}}`))
	s, _ := newTestSource(t, api, "sec")
	ev := s.toWire(asanaEvent{Action: "changed", Resource: asanaRef{GID: "p1", ResourceType: "project"}, Change: &struct {
		Field    string `json:"field"`
		Action   string `json:"action"`
		NewValue any    `json:"new_value"`
	}{Field: "name", Action: "changed", NewValue: "Roadmap"}})
	if ev == nil || ev.Event != "project" || ev.Title != "asana project Roadmap changed (name)" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Context["name"] != "Roadmap" || ev.Context["owner"] != "Ada" || ev.Context["team"] != "Eng" || ev.Context["archived"] != false || ev.Context["new_value"] != "Roadmap" {
		t.Fatalf("context = %+v", ev.Context)
	}
}

func TestUnknownResourceTypeIsCatchAllEvent(t *testing.T) {
	s, _ := newTestSource(t, nil, "sec")
	ev := s.toWire(asanaEvent{Action: "added", Resource: asanaRef{GID: "sec1", ResourceType: "section"}, Parent: &asanaRef{GID: "p1", ResourceType: "project"}})
	if ev == nil || ev.Event != "event" || ev.Kind != "section" || ev.Context["resource_type"] != "section" || ev.Context["resource_types"] != "section" {
		t.Fatalf("event = %+v", ev)
	}
	if s.toWire(asanaEvent{Action: "added"}) != nil || s.toWire(asanaEvent{Resource: asanaRef{GID: "x"}}) != nil {
		t.Fatal("an event with no gid or no action must be dropped")
	}
}

func TestDedupSuppressesRedelivery(t *testing.T) {
	s, events := newTestSource(t, nil, "sec")
	var d asanaDelivery
	if err := json.Unmarshal([]byte(taskDelivery), &d); err != nil {
		t.Fatal(err)
	}
	s.process(d.Events)
	s.process(d.Events) // Asana retried the same batch
	waitEvent(t, events)
	assertNoEvent(t, events)

	// A different action on the same resource at the same instant is distinct.
	d.Events[0].Action = "undeleted"
	s.process(d.Events)
	if ev := waitEvent(t, events); ev.Context["action"] != "undeleted" {
		t.Fatalf("event = %+v", ev)
	}
}

// --- StartSource wiring ---

func startSource(t *testing.T, cfg map[string]any) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	webhook, _ := cfg["webhook"].(map[string]any)
	webhook["listen"] = addr
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() {
		errc <- newAsanaPlugin().StartSource(ctx, plugin.StartSourceRequest{Instance: "s1", Config: cfg}, func(any) error { return nil })
	}()
	base := "http://" + addr + "/asana"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errc:
			t.Fatalf("StartSource exited early: %v", err)
		default:
		}
		resp, err := http.Get(base)
		if err == nil {
			resp.Body.Close()
			return base
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("listener never came up")
	return ""
}

func TestStartSourceRequiresListen(t *testing.T) {
	err := newAsanaPlugin().StartSource(context.Background(), plugin.StartSourceRequest{Instance: "s1", Config: map[string]any{"token": "pat"}}, func(any) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "webhook.listen") {
		t.Fatalf("expected a webhook.listen error, got %v", err)
	}
}

func TestStartSourceHandshakeDefaultFollowsPinning(t *testing.T) {
	// Bootstrap (no pinned secret): the handshake is accepted and echoed.
	base := startSource(t, map[string]any{"webhook": map[string]any{"path": "/asana", "enrich": false}})
	req, _ := http.NewRequest(http.MethodPost, base, nil)
	req.Header.Set("X-Hook-Secret", "boot")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Hook-Secret") != "boot" {
		t.Fatalf("bootstrap handshake: status=%d echo=%q", resp.StatusCode, resp.Header.Get("X-Hook-Secret"))
	}
	// ...exactly once.
	req, _ = http.NewRequest(http.MethodPost, base, nil)
	req.Header.Set("X-Hook-Secret", "again")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("second bootstrap handshake: status=%d, want 403", resp.StatusCode)
	}

	// Pinned: the handshake is refused by default...
	base = startSource(t, map[string]any{"webhook": map[string]any{"path": "/asana", "secret": []any{"pinned"}, "enrich": false}})
	req, _ = http.NewRequest(http.MethodPost, base, nil)
	req.Header.Set("X-Hook-Secret", "intruder")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("pinned handshake: status=%d, want 403", resp.StatusCode)
	}
	// ...while a pinned secret verifies deliveries immediately.
	req, _ = http.NewRequest(http.MethodPost, base, strings.NewReader(taskDelivery))
	req.Header.Set("X-Hook-Signature", hmacHex("pinned", []byte(taskDelivery)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pinned delivery: status=%d, want 200", resp.StatusCode)
	}

	// ...unless webhook.handshake is set explicitly.
	base = startSource(t, map[string]any{"webhook": map[string]any{"path": "/asana", "secret": "pinned", "handshake": true, "enrich": false}})
	req, _ = http.NewRequest(http.MethodPost, base, nil)
	req.Header.Set("X-Hook-Secret", "second")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("opted-in handshake: status=%d, want 200", resp.StatusCode)
	}
}

// --- helpers ---

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestDecodeEnvelope(t *testing.T) {
	data, next, err := decodeEnvelope([]byte(`{"data":[1,2],"next_page":{"offset":"o"}}`))
	if err != nil || next != "o" {
		t.Fatalf("data=%v next=%q err=%v", data, next, err)
	}
	if list, _ := data.([]any); len(list) != 2 {
		t.Fatalf("data = %#v", data)
	}
	if data, next, err := decodeEnvelope([]byte("  ")); err != nil || data != nil || next != "" {
		t.Fatalf("empty body: data=%v next=%q err=%v", data, next, err)
	}
	if _, _, err := decodeEnvelope([]byte("<html>")); err == nil {
		t.Fatal("non-JSON must error")
	}
}
