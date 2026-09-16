package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- TestDescribe ---

func TestDescribe(t *testing.T) {
	d := jiraPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "jira" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "*.atlassian.net:443" {
		t.Fatalf("capabilities.egress: %#v", d.Capabilities.Egress)
	}
	if _, ok := d.Connection["base_url"]; !ok || !d.Connection["base_url"].Required {
		t.Fatalf("connection.base_url must be present and required: %#v", d.Connection["base_url"])
	}
	wantVerbs := []string{
		"create_issue", "add_comment", "transition", "list_transitions",
		"get_issue", "update_issue", "assign", "add_labels", "search", "api",
	}
	got := map[string]bool{}
	for _, v := range d.Verbs {
		got[v.Name] = true
	}
	for _, w := range wantVerbs {
		if !got[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Verbs) != len(wantVerbs) {
		t.Errorf("verb count: got %d want %d", len(d.Verbs), len(wantVerbs))
	}
	wantEvents := []string{"issue_created", "issue_updated", "comment_created"}
	gotEv := map[string]bool{}
	for _, e := range d.Events {
		gotEv[e.Name] = true
	}
	for _, w := range wantEvents {
		if !gotEv[w] {
			t.Errorf("Describe missing event %q", w)
		}
	}
}

// --- adf() ---

func TestADF(t *testing.T) {
	got := adf("hello world")
	want := map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "hello world"},
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("adf() = %#v\nwant %#v", got, want)
	}
}

// --- webhook event parsing ---

func TestParseWebhookIssueCreated(t *testing.T) {
	body := []byte(`{
		"timestamp": 1700000000000,
		"webhookEvent": "jira:issue_created",
		"issue": {
			"key": "PROJ-1",
			"fields": {
				"summary": "Something broke",
				"status": {"name": "To Do"},
				"issuetype": {"name": "Bug"},
				"project": {"key": "PROJ"},
				"assignee": {"displayName": "Alice"},
				"reporter": {"displayName": "Bob"},
				"priority": {"name": "High"}
			}
		}
	}`)
	evs := parseWebhook("https://acme.atlassian.net", body)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Event != "issue_created" || ev.Kind != "issue_created" {
		t.Fatalf("event/kind: %q %q", ev.Event, ev.Kind)
	}
	wantDedup := "PROJ-1:jira:issue_created:1700000000000"
	if ev.Dedup != wantDedup {
		t.Fatalf("dedup: got %q want %q", ev.Dedup, wantDedup)
	}
	want := map[string]any{
		"key": "PROJ-1", "summary": "Something broke", "status": "To Do", "issuetype": "Bug",
		"project": "PROJ", "assignee": "Alice", "reporter": "Bob", "priority": "High",
		"url": "https://acme.atlassian.net/browse/PROJ-1", "action": "issue_created",
		"projects": "PROJ", "statuses": "To Do", "issuetypes": "Bug", "priorities": "High",
	}
	if !reflect.DeepEqual(ev.Context, want) {
		t.Fatalf("context = %#v\nwant %#v", ev.Context, want)
	}
}

func TestParseWebhookIssueUpdated(t *testing.T) {
	body := []byte(`{
		"timestamp": 1700000001000,
		"webhookEvent": "jira:issue_updated",
		"issue": {"key": "PROJ-2", "fields": {"summary": "s", "status": {"name": "Done"}, "issuetype": {"name": "Task"}, "project": {"key": "PROJ"}}}
	}`)
	evs := parseWebhook("https://acme.atlassian.net", body)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0].Context["action"] != "issue_updated" {
		t.Fatalf("action: %v", evs[0].Context["action"])
	}
	if evs[0].Context["assignee"] != "" || evs[0].Context["reporter"] != "" || evs[0].Context["priority"] != "" {
		t.Fatalf("expected empty optional fields, got %#v", evs[0].Context)
	}
}

func TestParseWebhookCommentCreated(t *testing.T) {
	body := []byte(`{
		"timestamp": 1700000002000,
		"webhookEvent": "comment_created",
		"issue": {"key": "PROJ-3", "fields": {"summary": "s", "status": {"name": "In Progress"}, "issuetype": {"name": "Story"}, "project": {"key": "PROJ"}}},
		"comment": {"body": "looks good", "author": {"displayName": "Carol"}}
	}`)
	evs := parseWebhook("https://acme.atlassian.net", body)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Event != "comment_created" {
		t.Fatalf("event: %q", ev.Event)
	}
	wantDedup := "PROJ-3:comment_created:1700000002000"
	if ev.Dedup != wantDedup {
		t.Fatalf("dedup: got %q want %q", ev.Dedup, wantDedup)
	}
	want := map[string]any{
		"key": "PROJ-3", "comment_body": "looks good", "author": "Carol",
		"project": "PROJ", "status": "In Progress", "issuetype": "Story",
		"url": "https://acme.atlassian.net/browse/PROJ-3", "action": "comment_created",
		"projects": "PROJ", "statuses": "In Progress", "issuetypes": "Story",
	}
	if !reflect.DeepEqual(ev.Context, want) {
		t.Fatalf("context = %#v\nwant %#v", ev.Context, want)
	}
}

func TestParseWebhookUnknownEventIgnored(t *testing.T) {
	body := []byte(`{"webhookEvent": "jira:worklog_updated", "issue": {"key": "PROJ-1"}}`)
	if evs := parseWebhook("https://acme.atlassian.net", body); evs != nil {
		t.Fatalf("expected nil for unrecognized event, got %#v", evs)
	}
}

func TestParseWebhookMalformedJSON(t *testing.T) {
	if evs := parseWebhook("https://acme.atlassian.net", []byte("not json")); evs != nil {
		t.Fatalf("expected nil for malformed body, got %#v", evs)
	}
}

// --- secret verification ---

func TestVerifyJiraSecret(t *testing.T) {
	mustReq := func(t *testing.T, url string, headerToken string) *http.Request {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, url, nil)
		if headerToken != "" {
			r.Header.Set("X-Conductor-Token", headerToken)
		}
		return r
	}
	cases := []struct {
		name        string
		secret      string
		url         string
		headerToken string
		want        bool
	}{
		{"empty secret always passes", "", "/jira", "", true},
		{"query match", "s3cr3t", "/jira?secret=s3cr3t", "", true},
		{"query mismatch", "s3cr3t", "/jira?secret=wrong", "", false},
		{"header match", "s3cr3t", "/jira", "s3cr3t", true},
		{"header mismatch", "s3cr3t", "/jira", "wrong", false},
		{"neither provided", "s3cr3t", "/jira", "", false},
		{"query takes precedence over header", "s3cr3t", "/jira?secret=s3cr3t", "wrong", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustReq(t, tc.url, tc.headerToken)
			if got := verifyJiraSecret(tc.secret, r); got != tc.want {
				t.Errorf("verifyJiraSecret(%q, %s) = %v, want %v", tc.secret, tc.url, got, tc.want)
			}
		})
	}
}

func TestRequireWebhookSecret(t *testing.T) {
	if err := requireWebhookSecret("jira", "", false, "webhook.secret"); err == nil {
		t.Fatal("expected error when secret is empty and allow_unsigned is false")
	}
	if err := requireWebhookSecret("jira", "", true, "webhook.secret"); err != nil {
		t.Fatalf("expected no error when allow_unsigned is true, got %v", err)
	}
	if err := requireWebhookSecret("jira", "s3cr3t", false, "webhook.secret"); err != nil {
		t.Fatalf("expected no error when secret is set, got %v", err)
	}
}

// --- verbs against an httptest.Server ---

func TestCreateIssue(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"key": "PROJ-9", "id": "10009"})
	}))
	defer srv.Close()

	p := jiraPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "create_issue",
		Connection: map[string]any{"base_url": srv.URL, "email": "bot@acme.com", "api_token": "tok"},
		Options: map[string]any{
			"project": "PROJ", "summary": "New bug", "issuetype": "Bug",
			"description": "steps to reproduce", "labels": []any{"urgent"},
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/rest/api/3/issue" {
		t.Fatalf("method/path: %s %s", gotMethod, gotPath)
	}
	if wantAuth := basicAuthHeader("bot@acme.com", "tok"); gotAuth != wantAuth {
		t.Fatalf("Authorization: got %q want %q", gotAuth, wantAuth)
	}
	fields, _ := gotBody["fields"].(map[string]any)
	if fields == nil {
		t.Fatalf("request body missing fields: %#v", gotBody)
	}
	wantDesc := adf("steps to reproduce")
	gotDesc, _ := fields["description"].(map[string]any)
	if !reflect.DeepEqual(normalize(gotDesc), normalize(wantDesc)) {
		t.Fatalf("description ADF: got %#v want %#v", gotDesc, wantDesc)
	}
	if res.Outputs["key"] != "PROJ-9" || res.Outputs["id"] != "10009" {
		t.Fatalf("outputs: %#v", res.Outputs)
	}
	if res.Outputs["url"] != srv.URL+"/browse/PROJ-9" {
		t.Fatalf("url: %v", res.Outputs["url"])
	}
	if res.Outputs["status_code"] != http.StatusCreated {
		t.Fatalf("status_code: %v", res.Outputs["status_code"])
	}
}

func TestAddComment(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "1000"})
	}))
	defer srv.Close()

	p := jiraPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "add_comment",
		Connection: map[string]any{"base_url": srv.URL},
		Options:    map[string]any{"key": "PROJ-1", "body": "on it"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/rest/api/3/issue/PROJ-1/comment" {
		t.Fatalf("method/path: %s %s", gotMethod, gotPath)
	}
	gotADF, _ := gotBody["body"].(map[string]any)
	if !reflect.DeepEqual(normalize(gotADF), normalize(adf("on it"))) {
		t.Fatalf("body ADF: got %#v", gotADF)
	}
	if res.Outputs["id"] != "1000" {
		t.Fatalf("outputs: %#v", res.Outputs)
	}
}

func TestTransitionByName(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/issue/PROJ-1/transitions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"transitions": []any{
					map[string]any{"id": "11", "name": "To Do"},
					map[string]any{"id": "21", "name": "In Progress"},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue/PROJ-1/transitions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			transition, _ := body["transition"].(map[string]any)
			if transition["id"] != "21" {
				t.Errorf("expected transition id 21, got %#v", transition)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := jiraPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "transition",
		Connection: map[string]any{"base_url": srv.URL},
		Options:    map[string]any{"key": "PROJ-1", "transition_name": "in progress"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 requests (lookup + transition), got %d", calls)
	}
}

func TestTransitionNameNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"transitions": []any{map[string]any{"id": "11", "name": "To Do"}}})
	}))
	defer srv.Close()

	p := jiraPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "transition",
		Connection: map[string]any{"base_url": srv.URL},
		Options:    map[string]any{"key": "PROJ-1", "transition_name": "nonexistent"},
	})
	if err == nil {
		t.Fatal("expected error for unknown transition name")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestGetIssue(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"key": "PROJ-1", "fields": map[string]any{"summary": "s"}})
	}))
	defer srv.Close()

	p := jiraPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get_issue",
		Connection: map[string]any{"base_url": srv.URL},
		Options:    map[string]any{"key": "PROJ-1", "fields": []any{"summary", "status"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotQuery != "fields=summary%2Cstatus" {
		t.Fatalf("query: %s", gotQuery)
	}
	m, _ := res.Outputs["result"].(map[string]any)
	if m["key"] != "PROJ-1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestSearch(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": []any{map[string]any{"key": "PROJ-1"}}})
	}))
	defer srv.Close()

	p := jiraPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "search",
		Connection: map[string]any{"base_url": srv.URL},
		Options:    map[string]any{"jql": "project = PROJ", "max_results": 10},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotBody["jql"] != "project = PROJ" {
		t.Fatalf("body: %#v", gotBody)
	}
	issues, ok := res.Outputs["issues"].([]any)
	if !ok || len(issues) != 1 {
		t.Fatalf("issues: %#v", res.Outputs["issues"])
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorMessages":["bad request"]}`))
	}))
	defer srv.Close()

	p := jiraPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get_issue",
		Connection: map[string]any{"base_url": srv.URL},
		Options:    map[string]any{"key": "PROJ-1"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
	if !contains(pe.Message, "400") || !contains(pe.Message, "bad request") {
		t.Fatalf("error message should carry status and body: %q", pe.Message)
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	p := jiraPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "api",
		Connection: map[string]any{"base_url": srv.URL},
		Options:    map[string]any{"method": "get", "path": "/myself", "query": map[string]any{"expand": "groups"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/rest/api/3/myself" || gotQuery != "expand=groups" {
		t.Fatalf("method/path/query: %s %s %s", gotMethod, gotPath, gotQuery)
	}
	m, _ := res.Outputs["result"].(map[string]any)
	if m["ok"] != true {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := jiraPlugin{}
	conn := map[string]any{"base_url": "https://example.atlassian.net"}
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"create_issue", map[string]any{}},
		{"add_comment", map[string]any{"key": "PROJ-1"}},
		{"transition", map[string]any{"key": "PROJ-1"}},
		{"list_transitions", map[string]any{}},
		{"get_issue", map[string]any{}},
		{"update_issue", map[string]any{"key": "PROJ-1"}},
		{"assign", map[string]any{"key": "PROJ-1"}},
		{"add_labels", map[string]any{"key": "PROJ-1"}},
		{"search", map[string]any{}},
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("verb %q: expected error for missing options %#v", tc.verb, tc.opts)
			continue
		}
		var pe *plugin.Error
		if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("verb %q: expected CodeInvalidParams, got %v", tc.verb, err)
		}
	}
}

func TestMissingBaseURL(t *testing.T) {
	p := jiraPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "get_issue", Connection: map[string]any{}, Options: map[string]any{"key": "PROJ-1"}})
	if err == nil {
		t.Fatal("expected error for missing base_url")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

// --- helpers ---

func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}

func basicAuthHeader(email, token string) string {
	r, _ := http.NewRequest(http.MethodGet, "http://x", nil)
	r.SetBasicAuth(email, token)
	return r.Header.Get("Authorization")
}

// normalize round-trips a value through JSON so map[string]any built by Go
// code compares equal to one decoded off the wire (json.Decode uses float64
// for numbers; adf()'s "version": 1 is an untyped int literal).
func normalize(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
