package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestDescribe asserts the declared surface: kind, type, events, and verbs.
func TestDescribe(t *testing.T) {
	d := gitlabPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "gitlab" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	wantEvents := []string{"push", "merge_request", "pipeline", "issue", "note"}
	gotEvents := map[string]bool{}
	for _, e := range d.Events {
		gotEvents[e.Name] = true
	}
	for _, w := range wantEvents {
		if !gotEvents[w] {
			t.Errorf("Describe missing event %q", w)
		}
	}
	wantVerbs := []string{
		"comment_mr", "comment_issue", "create_issue", "update_issue",
		"create_mr", "update_mr", "merge_mr", "get_mr", "get_issue",
		"list_mrs", "add_labels", "create_branch", "trigger_pipeline",
		"retry_pipeline", "cancel_pipeline", "api",
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
	if len(d.Capabilities.Egress) == 0 {
		t.Error("Describe: expected non-empty Egress capability")
	}
}

// --- buildRequest: pure argv/path construction ---

func TestBuildRequest(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		wantMethod string
		wantPath   string
		wantQuery  string // encoded query string, "" if none
		wantBody   map[string]any
	}{
		{
			name:       "comment_mr",
			verb:       "comment_mr",
			opts:       map[string]any{"project": "group/sub/proj", "mr": 5, "body": "lgtm"},
			wantMethod: "POST",
			wantPath:   "/projects/group%2Fsub%2Fproj/merge_requests/5/notes",
			wantBody:   map[string]any{"body": "lgtm"},
		},
		{
			name:       "comment_issue",
			verb:       "comment_issue",
			opts:       map[string]any{"project": "acme/app", "issue": 7, "body": "hi"},
			wantMethod: "POST",
			wantPath:   "/projects/acme%2Fapp/issues/7/notes",
			wantBody:   map[string]any{"body": "hi"},
		},
		{
			name: "create_issue with labels",
			verb: "create_issue",
			opts: map[string]any{
				"project": "acme/app", "title": "bug", "description": "desc",
				"labels": []any{"bug", "p1"},
			},
			wantMethod: "POST",
			wantPath:   "/projects/acme%2Fapp/issues",
			wantBody:   map[string]any{"title": "bug", "description": "desc", "labels": "bug,p1"},
		},
		{
			name:       "update_issue",
			verb:       "update_issue",
			opts:       map[string]any{"project": "acme/app", "issue": 3, "state_event": "close"},
			wantMethod: "PUT",
			wantPath:   "/projects/acme%2Fapp/issues/3",
			wantBody:   map[string]any{"state_event": "close"},
		},
		{
			name: "create_mr",
			verb: "create_mr",
			opts: map[string]any{
				"project": "acme/app", "source_branch": "feature", "target_branch": "main", "title": "add feature",
			},
			wantMethod: "POST",
			wantPath:   "/projects/acme%2Fapp/merge_requests",
			wantBody:   map[string]any{"source_branch": "feature", "target_branch": "main", "title": "add feature"},
		},
		{
			name:       "update_mr",
			verb:       "update_mr",
			opts:       map[string]any{"project": "acme/app", "mr": 9, "target_branch": "develop"},
			wantMethod: "PUT",
			wantPath:   "/projects/acme%2Fapp/merge_requests/9",
			wantBody:   map[string]any{"target_branch": "develop"},
		},
		{
			name:       "merge_mr",
			verb:       "merge_mr",
			opts:       map[string]any{"project": "acme/app", "mr": 9, "squash": true, "merge_when_pipeline_succeeds": false},
			wantMethod: "PUT",
			wantPath:   "/projects/acme%2Fapp/merge_requests/9/merge",
			wantBody:   map[string]any{"squash": true, "merge_when_pipeline_succeeds": false},
		},
		{
			name:       "get_mr",
			verb:       "get_mr",
			opts:       map[string]any{"project": "acme/app", "mr": 9},
			wantMethod: "GET",
			wantPath:   "/projects/acme%2Fapp/merge_requests/9",
		},
		{
			name:       "get_issue",
			verb:       "get_issue",
			opts:       map[string]any{"project": "acme/app", "issue": 3},
			wantMethod: "GET",
			wantPath:   "/projects/acme%2Fapp/issues/3",
		},
		{
			name:       "list_mrs",
			verb:       "list_mrs",
			opts:       map[string]any{"project": "acme/app", "state": "opened", "target_branch": "main"},
			wantMethod: "GET",
			wantPath:   "/projects/acme%2Fapp/merge_requests",
			wantQuery:  "state=opened&target_branch=main",
		},
		{
			name:       "add_labels",
			verb:       "add_labels",
			opts:       map[string]any{"project": "acme/app", "issue": 3, "labels": []any{"a", "b"}},
			wantMethod: "PUT",
			wantPath:   "/projects/acme%2Fapp/issues/3",
			wantBody:   map[string]any{"add_labels": "a,b"},
		},
		{
			name:       "create_branch",
			verb:       "create_branch",
			opts:       map[string]any{"project": "acme/app", "branch": "new-feat", "ref": "main"},
			wantMethod: "POST",
			wantPath:   "/projects/acme%2Fapp/repository/branches",
			wantQuery:  "branch=new-feat&ref=main",
		},
		{
			name:       "trigger_pipeline",
			verb:       "trigger_pipeline",
			opts:       map[string]any{"project": "acme/app", "ref": "main"},
			wantMethod: "POST",
			wantPath:   "/projects/acme%2Fapp/pipeline",
			wantQuery:  "ref=main",
		},
		{
			name:       "retry_pipeline",
			verb:       "retry_pipeline",
			opts:       map[string]any{"project": "acme/app", "pipeline": 42},
			wantMethod: "POST",
			wantPath:   "/projects/acme%2Fapp/pipelines/42/retry",
		},
		{
			name:       "cancel_pipeline",
			verb:       "cancel_pipeline",
			opts:       map[string]any{"project": "acme/app", "pipeline": 42},
			wantMethod: "POST",
			wantPath:   "/projects/acme%2Fapp/pipelines/42/cancel",
		},
		{
			name:       "api escape hatch",
			verb:       "api",
			opts:       map[string]any{"method": "DELETE", "path": "projects/1/issues/2"},
			wantMethod: "DELETE",
			wantPath:   "/projects/1/issues/2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method, path, query, body, err := buildRequest(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("buildRequest(%s): unexpected error: %v", tc.verb, err)
			}
			if method != tc.wantMethod {
				t.Errorf("method: got %q want %q", method, tc.wantMethod)
			}
			if path != tc.wantPath {
				t.Errorf("path: got %q want %q", path, tc.wantPath)
			}
			gotQuery := ""
			if query != nil {
				gotQuery = query.Encode()
			}
			if gotQuery != tc.wantQuery {
				t.Errorf("query: got %q want %q", gotQuery, tc.wantQuery)
			}
			if tc.wantBody != nil && !reflect.DeepEqual(body, tc.wantBody) {
				t.Errorf("body: got %#v want %#v", body, tc.wantBody)
			}
		})
	}
}

func TestBuildRequestErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"comment_mr", map[string]any{"project": "a/b"}},             // no mr
		{"comment_mr", map[string]any{"project": "a/b", "mr": 1}},    // no body
		{"comment_issue", map[string]any{"project": "a/b"}},          // no issue
		{"create_issue", map[string]any{"project": "a/b"}},           // no title
		{"update_issue", map[string]any{"project": "a/b"}},           // no issue
		{"create_mr", map[string]any{"project": "a/b"}},              // missing branches/title
		{"update_mr", map[string]any{"project": "a/b"}},              // no mr
		{"merge_mr", map[string]any{"project": "a/b"}},               // no mr
		{"get_mr", map[string]any{"project": "a/b"}},                 // no mr
		{"get_issue", map[string]any{"project": "a/b"}},              // no issue
		{"add_labels", map[string]any{"project": "a/b", "issue": 1}}, // no labels
		{"create_branch", map[string]any{"project": "a/b"}},          // no branch/ref
		{"trigger_pipeline", map[string]any{"project": "a/b"}},       // no ref
		{"retry_pipeline", map[string]any{"project": "a/b"}},         // no pipeline
		{"cancel_pipeline", map[string]any{"project": "a/b"}},        // no pipeline
		{"api", map[string]any{}},                                    // no path
		{"nope", map[string]any{"project": "a/b"}},                   // unknown verb
		{"comment_mr", map[string]any{"mr": 1, "body": "x"}},         // no project
	}
	for _, tc := range cases {
		if _, _, _, _, err := buildRequest(tc.verb, tc.opts); err == nil {
			t.Errorf("buildRequest(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// --- Invoke against a fake GitLab REST server ---

func TestInvokeCommentMR(t *testing.T) {
	var gotMethod, gotPath, gotToken, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 123, "body": "lgtm"}`))
	}))
	defer srv.Close()

	p := gitlabPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "comment_mr",
		Connection: map[string]any{"url": srv.URL, "token": "tok-123"},
		Options:    map[string]any{"project": "group/sub/proj", "mr": 5, "body": "lgtm"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method: got %q", gotMethod)
	}
	wantPath := "/api/v4/projects/group%2Fsub%2Fproj/merge_requests/5/notes"
	if gotPath != wantPath {
		t.Errorf("path: got %q want %q", gotPath, wantPath)
	}
	if gotToken != "tok-123" {
		t.Errorf("PRIVATE-TOKEN: got %q", gotToken)
	}
	if !strings.Contains(gotBody, `"body":"lgtm"`) {
		t.Errorf("request body: got %q", gotBody)
	}
	if res.Outputs["status_code"] != 201 {
		t.Errorf("status_code: got %v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != float64(123) {
		t.Errorf("result: got %#v", res.Outputs["result"])
	}
}

func TestInvokeListMRsItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "state=opened" {
			t.Errorf("query: got %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"iid": 1}, {"iid": 2}]`))
	}))
	defer srv.Close()

	p := gitlabPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "list_mrs",
		Connection: map[string]any{"url": srv.URL, "token": "t"},
		Options:    map[string]any{"project": "acme/app", "state": "opened"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: got %#v", res.Outputs["items"])
	}
}

func TestInvokeNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "404 Project Not Found"}`))
	}))
	defer srv.Close()

	p := gitlabPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get_mr",
		Connection: map[string]any{"url": srv.URL, "token": "t"},
		Options:    map[string]any{"project": "acme/app", "mr": 1},
	})
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	pe, ok := err.(*plugin.Error)
	if !ok {
		t.Fatalf("expected *plugin.Error, got %T: %v", err, err)
	}
	if pe.Code != plugin.CodeInternalError {
		t.Errorf("code: got %d want %d", pe.Code, plugin.CodeInternalError)
	}
	if !strings.Contains(pe.Message, "404") || !strings.Contains(pe.Message, "Project Not Found") {
		t.Errorf("message should carry status+body, got %q", pe.Message)
	}
}

func TestApiBaseDefault(t *testing.T) {
	if got := apiBase(map[string]any{}); got != "https://gitlab.com/api/v4" {
		t.Errorf("default apiBase: got %q", got)
	}
	if got := apiBase(map[string]any{"url": "https://gitlab.example.com/"}); got != "https://gitlab.example.com/api/v4" {
		t.Errorf("trailing slash apiBase: got %q", got)
	}
}

// --- webhook event parsing ---

func TestParseWebhookMergeRequest(t *testing.T) {
	body := []byte(`{
		"object_kind": "merge_request",
		"user": {"username": "alice"},
		"project": {"path_with_namespace": "acme/app", "web_url": "https://gitlab.com/acme/app"},
		"object_attributes": {
			"iid": 7, "title": "Add feature", "state": "opened", "action": "open",
			"source_branch": "feature", "target_branch": "main",
			"url": "https://gitlab.com/acme/app/-/merge_requests/7"
		}
	}`)
	evs := parseWebhook(body)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.event != "merge_request" || ev.kind != "merge_request" {
		t.Fatalf("event/kind: %q/%q", ev.event, ev.kind)
	}
	ctx := ev.context
	wantStrings := map[string]string{
		"project": "acme/app", "user": "alice", "action": "open",
		"source_branch": "feature", "target_branch": "main", "title": "Add feature", "state": "opened",
	}
	for k, want := range wantStrings {
		if got, _ := ctx[k].(string); got != want {
			t.Errorf("context[%q]: got %q want %q", k, got, want)
		}
	}
	if ctx["mr_iid"] != 7 {
		t.Errorf("context[mr_iid]: got %v", ctx["mr_iid"])
	}
	if ev.dedup == "" {
		t.Error("expected non-empty dedup key")
	}
}

func TestParseWebhookPush(t *testing.T) {
	body := []byte(`{
		"object_kind": "push",
		"ref": "refs/heads/main",
		"after": "abc123",
		"user_username": "bob",
		"project": {"path_with_namespace": "acme/app", "web_url": "https://gitlab.com/acme/app"}
	}`)
	evs := parseWebhook(body)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ctx := evs[0].context
	if ctx["branch"] != "main" {
		t.Errorf("branch: got %v", ctx["branch"])
	}
	if ctx["user"] != "bob" {
		t.Errorf("user: got %v", ctx["user"])
	}
	if ctx["project"] != "acme/app" {
		t.Errorf("project: got %v", ctx["project"])
	}
}

func TestParseWebhookPipeline(t *testing.T) {
	body := []byte(`{
		"object_kind": "pipeline",
		"user": {"username": "carol"},
		"project": {"path_with_namespace": "acme/app", "web_url": "https://gitlab.com/acme/app"},
		"object_attributes": {"id": 99, "ref": "main", "status": "failed"}
	}`)
	evs := parseWebhook(body)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ctx := evs[0].context
	if ctx["status"] != "failed" || ctx["pipeline_id"] != 99 {
		t.Errorf("pipeline context: %#v", ctx)
	}
}

func TestParseWebhookUnknownKind(t *testing.T) {
	if evs := parseWebhook([]byte(`{"object_kind": "wiki_page"}`)); evs != nil {
		t.Errorf("expected nil for unrecognized object_kind, got %#v", evs)
	}
}

// --- X-Gitlab-Token verification via StartSource ---

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestStartSourceTokenAcceptReject(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var received []map[string]any
	emit := func(v any) error {
		mu.Lock()
		defer mu.Unlock()
		m, _ := v.(map[string]any)
		received = append(received, m)
		return nil
	}

	p := gitlabPlugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.StartSource(ctx, plugin.StartSourceRequest{
			Instance: "test",
			Config: map[string]any{
				"webhook": map[string]any{"listen": addr, "path": "/gitlab", "secret": "s3cr3t"},
			},
		}, emit)
	}()

	waitListening(t, addr)

	push := []byte(`{"object_kind":"push","ref":"refs/heads/main","after":"x","user_username":"bob","project":{"path_with_namespace":"acme/app","web_url":"https://gitlab.com/acme/app"}}`)

	// Wrong token: rejected, no event emitted.
	postWebhook(t, addr, "/gitlab", "wrong", push)
	// Correct token: accepted.
	postWebhook(t, addr, "/gitlab", "s3cr3t", push)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartSource did not return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected exactly 1 emitted event, got %d: %#v", len(received), received)
	}
}

func TestStartSourceFailsClosedWithoutSecret(t *testing.T) {
	p := gitlabPlugin{}
	err := p.StartSource(context.Background(), plugin.StartSourceRequest{
		Instance: "test",
		Config: map[string]any{
			"webhook": map[string]any{"listen": "127.0.0.1:0", "path": "/gitlab"},
		},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected error when no secret and allow_unsigned is unset")
	}
}

func TestStartSourceAllowUnsigned(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var received int
	emit := func(v any) error {
		mu.Lock()
		defer mu.Unlock()
		received++
		return nil
	}

	p := gitlabPlugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.StartSource(ctx, plugin.StartSourceRequest{
			Instance: "test",
			Config: map[string]any{
				"webhook": map[string]any{"listen": addr, "path": "/gitlab", "allow_unsigned": true},
			},
		}, emit)
	}()

	waitListening(t, addr)
	push := []byte(`{"object_kind":"push","ref":"refs/heads/main","after":"y","user_username":"bob","project":{"path_with_namespace":"acme/app","web_url":"https://gitlab.com/acme/app"}}`)
	postWebhook(t, addr, "/gitlab", "", push)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartSource did not return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if received != 1 {
		t.Fatalf("expected 1 emitted event with allow_unsigned, got %d", received)
	}
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}

func postWebhook(t *testing.T, addr, path, token string, body []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", "http://"+addr+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Gitlab-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("s3cr3t", "s3cr3t") {
		t.Error("expected equal secrets to match")
	}
	if constantTimeEqual("wrong", "s3cr3t") {
		t.Error("expected mismatched secrets to fail")
	}
	if constantTimeEqual("", "s3cr3t") {
		t.Error("expected empty header to fail against a configured secret")
	}
}
