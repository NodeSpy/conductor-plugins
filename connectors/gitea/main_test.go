package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- Describe ---

func TestDescribe(t *testing.T) {
	d := giteaPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "gitea" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if _, ok := d.Connection["url"]; !ok || !d.Connection["url"].Required {
		t.Errorf("Describe: connection.url must be present and required")
	}
	wantEvents := []string{"push", "pull_request", "issues", "issue_comment"}
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
		"comment_issue", "create_issue", "update_issue", "get_issue", "list_issues",
		"create_pr", "merge_pr", "get_pr", "add_labels", "create_release",
		"create_branch", "put_file", "api",
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
	if d.Capabilities.Egress == nil || len(d.Capabilities.Egress) != 0 {
		t.Errorf("Describe: expected an empty (non-nil-in-spirit) Egress manifest, got %#v", d.Capabilities.Egress)
	}
}

// --- buildRequest: pure request-shape construction ---

func TestBuildRequest(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		wantMethod string
		wantPath   string
		wantQuery  string
		wantBody   any
		wantList   bool
	}{
		{
			name:       "comment_issue",
			verb:       "comment_issue",
			opts:       map[string]any{"repo": "acme/app", "index": 5, "body": "lgtm"},
			wantMethod: "POST",
			wantPath:   "/repos/acme/app/issues/5/comments",
			wantBody:   map[string]any{"body": "lgtm"},
		},
		{
			name:       "create_issue",
			verb:       "create_issue",
			opts:       map[string]any{"repo": "acme/app", "title": "bug", "labels": []any{1, 2}, "assignees": []any{"bob"}},
			wantMethod: "POST",
			wantPath:   "/repos/acme/app/issues",
			wantBody:   map[string]any{"title": "bug", "labels": []int64{1, 2}, "assignees": []string{"bob"}},
		},
		{
			name:       "update_issue",
			verb:       "update_issue",
			opts:       map[string]any{"repo": "acme/app", "index": 3, "state": "closed"},
			wantMethod: "PATCH",
			wantPath:   "/repos/acme/app/issues/3",
			wantBody:   map[string]any{"state": "closed"},
		},
		{
			name:       "get_issue",
			verb:       "get_issue",
			opts:       map[string]any{"repo": "acme/app", "index": 3},
			wantMethod: "GET",
			wantPath:   "/repos/acme/app/issues/3",
		},
		{
			name:       "list_issues",
			verb:       "list_issues",
			opts:       map[string]any{"repo": "acme/app", "state": "open"},
			wantMethod: "GET",
			wantPath:   "/repos/acme/app/issues",
			wantQuery:  "state=open",
			wantList:   true,
		},
		{
			name:       "create_pr",
			verb:       "create_pr",
			opts:       map[string]any{"repo": "acme/app", "head": "feat", "base": "main", "title": "Add feat"},
			wantMethod: "POST",
			wantPath:   "/repos/acme/app/pulls",
			wantBody:   map[string]any{"head": "feat", "base": "main", "title": "Add feat"},
		},
		{
			name:       "merge_pr default",
			verb:       "merge_pr",
			opts:       map[string]any{"repo": "acme/app", "index": 7},
			wantMethod: "POST",
			wantPath:   "/repos/acme/app/pulls/7/merge",
			wantBody:   map[string]any{"do": "merge"},
		},
		{
			name:       "merge_pr explicit",
			verb:       "merge_pr",
			opts:       map[string]any{"repo": "acme/app", "index": 7, "do": "squash"},
			wantMethod: "POST",
			wantPath:   "/repos/acme/app/pulls/7/merge",
			wantBody:   map[string]any{"do": "squash"},
		},
		{
			name:       "get_pr",
			verb:       "get_pr",
			opts:       map[string]any{"repo": "acme/app", "index": 7},
			wantMethod: "GET",
			wantPath:   "/repos/acme/app/pulls/7",
		},
		{
			name:       "add_labels",
			verb:       "add_labels",
			opts:       map[string]any{"repo": "acme/app", "index": 3, "labels": []any{1, 2}},
			wantMethod: "POST",
			wantPath:   "/repos/acme/app/issues/3/labels",
			wantBody:   map[string]any{"labels": []int64{1, 2}},
		},
		{
			name:       "create_release",
			verb:       "create_release",
			opts:       map[string]any{"repo": "acme/app", "tag_name": "v1.0.0", "draft": true},
			wantMethod: "POST",
			wantPath:   "/repos/acme/app/releases",
			wantBody:   map[string]any{"tag_name": "v1.0.0", "draft": true},
		},
		{
			name:       "create_branch",
			verb:       "create_branch",
			opts:       map[string]any{"repo": "acme/app", "new_branch_name": "feat", "old_branch_name": "main"},
			wantMethod: "POST",
			wantPath:   "/repos/acme/app/branches",
			wantBody:   map[string]any{"new_branch_name": "feat", "old_branch_name": "main"},
		},
		{
			name:       "put_file",
			verb:       "put_file",
			opts:       map[string]any{"repo": "acme/app", "path": "a/b.txt", "content": "hi", "message": "add file"},
			wantMethod: "PUT",
			wantPath:   "/repos/acme/app/contents/a/b.txt",
			wantBody:   map[string]any{"content": "aGk=", "message": "add file"},
		},
		{
			name:       "api escape hatch",
			verb:       "api",
			opts:       map[string]any{"method": "delete", "path": "/repos/acme/app/topics/foo"},
			wantMethod: "DELETE",
			wantPath:   "/repos/acme/app/topics/foo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rb, err := buildRequest(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("buildRequest(%s): unexpected error: %v", tc.verb, err)
			}
			if rb.method != tc.wantMethod {
				t.Errorf("method: got %q want %q", rb.method, tc.wantMethod)
			}
			if rb.path != tc.wantPath {
				t.Errorf("path: got %q want %q", rb.path, tc.wantPath)
			}
			gotQuery := ""
			if rb.query != nil {
				gotQuery = rb.query.Encode()
			}
			if gotQuery != tc.wantQuery {
				t.Errorf("query: got %q want %q", gotQuery, tc.wantQuery)
			}
			if tc.wantBody != nil {
				assertBodyEqual(t, rb.body, tc.wantBody)
			}
			if rb.list != tc.wantList {
				t.Errorf("list: got %v want %v", rb.list, tc.wantList)
			}
		})
	}
}

// assertBodyEqual compares the interesting keys of a request body map,
// tolerant of the numeric-slice representations produced along different
// paths (intList vs. a literal []int64 in the test table).
func assertBodyEqual(t *testing.T, got any, want any) {
	t.Helper()
	gm, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("body: got %#v, want a map", got)
	}
	wm, ok := want.(map[string]any)
	if !ok {
		t.Fatalf("test table body must be a map, got %#v", want)
	}
	for k, wv := range wm {
		gv, present := gm[k]
		if !present {
			t.Errorf("body[%q]: missing (want %#v)", k, wv)
			continue
		}
		switch w := wv.(type) {
		case []int64:
			g, ok := gv.([]int64)
			if !ok || len(g) != len(w) {
				t.Errorf("body[%q]: got %#v want %#v", k, gv, wv)
				continue
			}
			for i := range w {
				if g[i] != w[i] {
					t.Errorf("body[%q][%d]: got %v want %v", k, i, g[i], w[i])
				}
			}
		case []string:
			g, ok := gv.([]string)
			if !ok || len(g) != len(w) {
				t.Errorf("body[%q]: got %#v want %#v", k, gv, wv)
				continue
			}
			for i := range w {
				if g[i] != w[i] {
					t.Errorf("body[%q][%d]: got %v want %v", k, i, g[i], w[i])
				}
			}
		default:
			if gv != wv {
				t.Errorf("body[%q]: got %#v want %#v", k, gv, wv)
			}
		}
	}
}

func TestBuildRequestErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"comment_issue", map[string]any{"repo": "a/b"}},                          // no index
		{"comment_issue", map[string]any{"repo": "a/b", "index": 1}},              // no body
		{"create_issue", map[string]any{"repo": "a/b"}},                           // no title
		{"update_issue", map[string]any{"repo": "a/b"}},                           // no index
		{"get_issue", map[string]any{"repo": "a/b"}},                              // no index
		{"list_issues", map[string]any{}},                                         // no repo
		{"create_pr", map[string]any{"repo": "a/b"}},                              // no head/base/title
		{"merge_pr", map[string]any{"repo": "a/b"}},                               // no index
		{"get_pr", map[string]any{"repo": "a/b"}},                                 // no index
		{"add_labels", map[string]any{"repo": "a/b", "index": 1}},                 // no labels
		{"create_release", map[string]any{"repo": "a/b"}},                         // no tag_name
		{"create_branch", map[string]any{"repo": "a/b"}},                          // no new_branch_name
		{"put_file", map[string]any{"repo": "a/b", "path": "x"}},                  // no content/message
		{"api", map[string]any{}},                                                 // no method/path
		{"nope", map[string]any{"repo": "a/b"}},                                   // unknown verb
		{"comment_issue", map[string]any{"index": 1, "body": "x"}},                // no repo
		{"comment_issue", map[string]any{"repo": "bad", "index": 1, "body": "x"}}, // repo not owner/name
	}
	for _, tc := range cases {
		if _, err := buildRequest(tc.verb, tc.opts); err == nil {
			t.Errorf("buildRequest(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

func TestApiBase(t *testing.T) {
	got, err := apiBase("https://gitea.example.com/")
	if err != nil || got != "https://gitea.example.com/api/v1" {
		t.Errorf("apiBase: got %q, %v", got, err)
	}
	if _, err := apiBase(""); err == nil {
		t.Error("apiBase(\"\") should error — url is required")
	}
}

func TestToBase64Content(t *testing.T) {
	if got := toBase64Content("hi"); got != "aGk=" {
		t.Errorf("toBase64Content(raw): got %q", got)
	}
	// Already-base64 content should pass through unchanged.
	if got := toBase64Content("aGk="); got != "aGk=" {
		t.Errorf("toBase64Content(already-encoded): got %q", got)
	}
}

// --- Invoke against a fake Gitea API server ---

func TestInvokeCommentIssue(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 42, "body": "lgtm"}`))
	}))
	defer srv.Close()

	p := giteaPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "comment_issue",
		Connection: map[string]any{"url": srv.URL, "token": "tok-abc"},
		Options:    map[string]any{"repo": "acme/app", "index": 5, "body": "lgtm"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method: got %q", gotMethod)
	}
	wantPath := "/api/v1/repos/acme/app/issues/5/comments"
	if gotPath != wantPath {
		t.Errorf("path: got %q want %q", gotPath, wantPath)
	}
	if gotAuth != "token tok-abc" {
		t.Errorf("Authorization: got %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"body":"lgtm"`) {
		t.Errorf("request body: got %q", gotBody)
	}
	if res.Outputs["status_code"] != 201 {
		t.Errorf("status_code: got %v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != float64(42) {
		t.Errorf("result: got %#v", res.Outputs["result"])
	}
}

func TestInvokeListIssuesItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/issues" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if r.URL.RawQuery != "state=open" {
			t.Errorf("query: got %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"number": 1}, {"number": 2}]`))
	}))
	defer srv.Close()

	p := giteaPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "list_issues",
		Connection: map[string]any{"url": srv.URL, "token": "t"},
		Options:    map[string]any{"repo": "acme/app", "state": "open"},
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
		_, _ = w.Write([]byte(`{"message": "repo not found"}`))
	}))
	defer srv.Close()

	p := giteaPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get_pr",
		Connection: map[string]any{"url": srv.URL, "token": "t"},
		Options:    map[string]any{"repo": "acme/app", "index": 1},
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
	if !strings.Contains(pe.Message, "404") || !strings.Contains(pe.Message, "repo not found") {
		t.Errorf("message should carry status+body, got %q", pe.Message)
	}
}

func TestInvokeRequiresURL(t *testing.T) {
	p := giteaPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get_pr",
		Connection: map[string]any{"token": "t"},
		Options:    map[string]any{"repo": "acme/app", "index": 1},
	})
	if err == nil {
		t.Fatal("expected error when connection.url is missing")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %#v", err)
	}
}

// --- webhook event parsing ---

func TestParseWebhookPush(t *testing.T) {
	body := []byte(`{
		"ref": "refs/heads/main",
		"before": "aaa",
		"after": "bbb",
		"compare_url": "https://gitea.example.com/acme/app/compare/aaa...bbb",
		"repository": {"full_name": "acme/app", "html_url": "https://gitea.example.com/acme/app"},
		"sender": {"login": "alice"}
	}`)
	ev := parseWebhook("push", body)
	if ev == nil {
		t.Fatal("expected an event")
	}
	if ev["event"] != "push" || ev["kind"] != "push" {
		t.Fatalf("event/kind: %#v", ev)
	}
	ctx, ok := ev["context"].(map[string]any)
	if !ok {
		t.Fatalf("context: %#v", ev["context"])
	}
	want := map[string]string{
		"repo": "acme/app", "ref": "refs/heads/main", "branch": "main", "sender": "alice",
		"url": "https://gitea.example.com/acme/app/compare/aaa...bbb",
	}
	for k, w := range want {
		if got, _ := ctx[k].(string); got != w {
			t.Errorf("context[%q]: got %q want %q", k, got, w)
		}
	}
	if ev["dedup"] == "" {
		t.Error("expected non-empty dedup key")
	}
}

func TestParseWebhookPullRequest(t *testing.T) {
	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/app"},
		"sender": {"login": "bob"},
		"pull_request": {
			"number": 7, "title": "Add feature", "html_url": "https://gitea.example.com/acme/app/pulls/7",
			"state": "open", "merged": false,
			"base": {"ref": "main"}, "head": {"ref": "feature"}
		}
	}`)
	ev := parseWebhook("pull_request", body)
	if ev == nil {
		t.Fatal("expected an event")
	}
	if ev["event"] != "pull_request" {
		t.Fatalf("event: %#v", ev)
	}
	ctx := ev["context"].(map[string]any)
	wantStr := map[string]string{
		"repo": "acme/app", "action": "opened", "sender": "bob",
		"title": "Add feature", "state": "open", "base": "main", "head": "feature",
	}
	for k, w := range wantStr {
		if got, _ := ctx[k].(string); got != w {
			t.Errorf("context[%q]: got %q want %q", k, got, w)
		}
	}
	if ctx["pr_number"] != int64(7) {
		t.Errorf("context[pr_number]: got %#v", ctx["pr_number"])
	}
	if ctx["merged"] != false {
		t.Errorf("context[merged]: got %#v", ctx["merged"])
	}
	if ev["dedup"] == "" {
		t.Error("expected non-empty dedup key")
	}
}

func TestParseWebhookIssues(t *testing.T) {
	body := []byte(`{
		"action": "closed",
		"repository": {"full_name": "acme/app"},
		"sender": {"login": "carol"},
		"issue": {"number": 12, "title": "Bug", "state": "closed", "html_url": "https://gitea.example.com/acme/app/issues/12"}
	}`)
	ev := parseWebhook("issues", body)
	if ev == nil {
		t.Fatal("expected an event")
	}
	if ev["event"] != "issues" {
		t.Fatalf("event: %#v", ev)
	}
	ctx := ev["context"].(map[string]any)
	if ctx["issue_number"] != int64(12) || ctx["title"] != "Bug" || ctx["state"] != "closed" {
		t.Errorf("context: %#v", ctx)
	}
}

func TestParseWebhookIssueComment(t *testing.T) {
	body := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/app"},
		"sender": {"login": "dave"},
		"issue": {"number": 9, "title": "Q", "html_url": "https://gitea.example.com/acme/app/issues/9"},
		"comment": {"id": 555, "body": "thanks!", "html_url": "https://gitea.example.com/acme/app/issues/9#comment-555"}
	}`)
	ev := parseWebhook("issue_comment", body)
	if ev == nil {
		t.Fatal("expected an event")
	}
	if ev["event"] != "issue_comment" {
		t.Fatalf("event: %#v", ev)
	}
	ctx := ev["context"].(map[string]any)
	if ctx["issue_number"] != int64(9) || ctx["comment_body"] != "thanks!" {
		t.Errorf("context: %#v", ctx)
	}
	if ev["dedup"] == "" {
		t.Error("expected non-empty dedup key")
	}
}

func TestParseWebhookUnknownKind(t *testing.T) {
	if ev := parseWebhook("release", []byte(`{}`)); ev != nil {
		t.Errorf("expected nil for an unrecognized event kind, got %#v", ev)
	}
}

func TestParseWebhookMalformedJSON(t *testing.T) {
	if ev := parseWebhook("push", []byte(`not json`)); ev != nil {
		t.Errorf("expected nil for malformed JSON, got %#v", ev)
	}
}

// --- HMAC verification, exercised end-to-end through StartSource ---

func giteaSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestStartSourceHMACAcceptReject(t *testing.T) {
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

	p := giteaPlugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.StartSource(ctx, plugin.StartSourceRequest{
			Instance: "test",
			Config: map[string]any{
				"webhook": map[string]any{"listen": addr, "path": "/gitea", "secret": "s3cr3t"},
			},
		}, emit)
	}()
	waitListening(t, addr)

	push := []byte(`{"ref":"refs/heads/main","after":"x","repository":{"full_name":"acme/app"},"sender":{"login":"bob"}}`)

	// Wrong signature: rejected, no event emitted.
	postSignedWebhook(t, addr, "/gitea", "X-Gitea-Signature", giteaSign("wrong-secret", push), push)
	// Correct signature: accepted.
	postSignedWebhook(t, addr, "/gitea", "X-Gitea-Signature", giteaSign("s3cr3t", push), push)

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
	if received[0]["event"] != "push" {
		t.Errorf("event: %#v", received[0])
	}
}

func TestStartSourceFailsClosedWithoutSecret(t *testing.T) {
	p := giteaPlugin{}
	err := p.StartSource(context.Background(), plugin.StartSourceRequest{
		Instance: "test",
		Config: map[string]any{
			"webhook": map[string]any{"listen": "127.0.0.1:0", "path": "/gitea"},
		},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected error when no secret and webhook.allow_unsigned is unset")
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

	p := giteaPlugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.StartSource(ctx, plugin.StartSourceRequest{
			Instance: "test",
			Config: map[string]any{
				"webhook": map[string]any{"listen": addr, "path": "/gitea", "allow_unsigned": true},
			},
		}, emit)
	}()
	waitListening(t, addr)

	push := []byte(`{"ref":"refs/heads/main","after":"y","repository":{"full_name":"acme/app"},"sender":{"login":"bob"}}`)
	postSignedWebhook(t, addr, "/gitea", "X-Gitea-Signature", "", push)

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

func TestStartSourceNoListenAddr(t *testing.T) {
	p := giteaPlugin{}
	err := p.StartSource(context.Background(), plugin.StartSourceRequest{
		Instance: "test",
		Config:   map[string]any{"webhook": map[string]any{"secret": "s"}},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected error when webhook.listen is empty")
	}
}

// --- test helpers ---

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

func postSignedWebhook(t *testing.T, addr, path, sigHeader, sig string, body []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", "http://"+addr+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if sig != "" {
		req.Header.Set(sigHeader, sig)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Event", "push")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
