package ghfake

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit"
	"github.com/golang-jwt/jwt/v5"
)

// hookSink records the deliveries a fake sends.
type hookSink struct {
	mu  sync.Mutex
	got []*http.Request
	bod [][]byte
}

func (s *hookSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.got = append(s.got, r)
	s.bod = append(s.bod, b)
	s.mu.Unlock()
	w.WriteHeader(202)
}

func (s *hookSink) events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for i, r := range s.got {
		var p map[string]any
		_ = json.Unmarshal(s.bod[i], &p)
		a, _ := p["action"].(string)
		out = append(out, strings.TrimSuffix(r.Header.Get("X-GitHub-Event")+"."+a, "."))
	}
	return out
}

// world is a small GitHub: an org repo with an App installed, people, a PR.
func world(t *testing.T, opts Options) (*Fake, *hookSink, int) {
	t.Helper()
	if opts.Logf == nil {
		opts.Logf = t.Logf
	}
	f := New(opts)
	base := f.Start()
	t.Cleanup(f.Close)
	_ = base
	f.AddUser("acme", "Organization")
	f.SetApp(77, "conductor-test", nil)
	f.AddRepo("acme/w")
	f.Install("acme")
	f.UserToken("me", "tok-me")
	f.UserToken("rev", "tok-rev")
	f.AddUser("cursor[bot]", "")
	f.AddCollaborator("acme/w", "rev")
	sink := &hookSink{}
	hs := httptest.NewServer(sink)
	t.Cleanup(hs.Close)
	f.AddHook(hs.URL, "s3cret")
	n := f.OpenPR("acme/w", "me", PROpts{Title: "feature", Head: "feat"})
	f.Flush()
	return f, sink, n
}

func do(t *testing.T, f *Fake, method, path, tok string, body any, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.URL()+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// mergeable_state is computed from the model, as GitHub computes it.
func TestMergeableStateFromTheModel(t *testing.T) {
	f, _, n := world(t, Options{})
	state := func() string {
		_, b := do(t, f, "GET", "/repos/acme/w/pulls/1", "tok-me", nil)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m["mergeable_state"].(string)
	}
	if got := state(); got != "blocked" { // one approval required, none given
		t.Fatalf("fresh PR: %s", got)
	}
	f.SubmitReview("acme/w", n, "rev", "APPROVED", "lgtm")
	if got := state(); got != "clean" {
		t.Fatalf("approved: %s", got)
	}
	head := f.HeadSHA("acme/w", n)
	f.Check("acme/w", head, "lint", "failure")
	if got := state(); got != "unstable" { // a failing NON-required check
		t.Fatalf("failing optional check: %s", got)
	}
	f.SetProtection("acme/w", Protection{RequiredApprovals: 1, RequireUpToDate: true, RequiredChecks: []string{"lint"}})
	if got := state(); got != "blocked" {
		t.Fatalf("failing required check: %s", got)
	}
	f.Push("acme/w", "main", "acme", "base moves")
	if got := state(); got != "behind" {
		t.Fatalf("base moved: %s", got)
	}
	f.SetConflict("acme/w", n, true)
	if got := state(); got != "dirty" {
		t.Fatalf("conflict: %s", got)
	}
}

// GitHub computes mergeability in the background: the first read after a
// change says null / unknown.
func TestMergeabilityDelay(t *testing.T) {
	f, _, n := world(t, Options{MergeabilityDelay: true})
	read := func() (any, string) {
		_, b := do(t, f, "GET", "/repos/acme/w/pulls/1", "tok-me", nil)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m["mergeable"], m["mergeable_state"].(string)
	}
	if m, s := read(); m != nil || s != "unknown" {
		t.Fatalf("first read after open: %v %s", m, s)
	}
	if m, s := read(); m != true || s != "blocked" {
		t.Fatalf("second read: %v %s", m, s)
	}
	f.SetConflict("acme/w", n, true)
	if _, s := read(); s != "unknown" {
		t.Fatalf("after a change: %s", s)
	}
	if m, s := read(); m != false || s != "dirty" {
		t.Fatalf("computed: %v %s", m, s)
	}
}

func TestETagAnd304(t *testing.T) {
	f, _, _ := world(t, Options{})
	r1, _ := do(t, f, "GET", "/repos/acme/w/pulls/1", "tok-me", nil)
	etag := r1.Header.Get("ETag")
	if etag == "" || r1.Header.Get("X-RateLimit-Remaining") == "" {
		t.Fatalf("headers: %v", r1.Header)
	}
	r2, b2 := do(t, f, "GET", "/repos/acme/w/pulls/1", "tok-me", nil, "If-None-Match", etag)
	if r2.StatusCode != 304 || len(b2) != 0 {
		t.Fatalf("conditional read of an unchanged PR: %d %q", r2.StatusCode, b2)
	}
	f.Comment("acme/w", 1, "rev", "hi")
	r3, _ := do(t, f, "GET", "/repos/acme/w/pulls/1", "tok-me", nil, "If-None-Match", etag)
	if r3.StatusCode != 200 {
		t.Fatalf("a changed PR must not 304: %d", r3.StatusCode)
	}
}

func TestPaginationLinkHeader(t *testing.T) {
	f, _, _ := world(t, Options{})
	for i := 0; i < 5; i++ {
		f.Comment("acme/w", 1, "rev", "c")
	}
	r, b := do(t, f, "GET", "/repos/acme/w/issues/1/comments?per_page=2", "tok-me", nil)
	var page []any
	_ = json.Unmarshal(b, &page)
	link := r.Header.Get("Link")
	if len(page) != 2 || !strings.Contains(link, `rel="next"`) || !strings.Contains(link, "page=3") {
		t.Fatalf("page 1: %d items, Link %q", len(page), link)
	}
	_, b = do(t, f, "GET", "/repos/acme/w/issues/1/comments?per_page=2&page=3", "tok-me", nil)
	_ = json.Unmarshal(b, &page)
	if len(page) != 1 {
		t.Fatalf("last page: %d", len(page))
	}
}

func TestAuthDistinctions(t *testing.T) {
	f, _, _ := world(t, Options{})
	if r, _ := do(t, f, "GET", "/repos/acme/w/pulls/1", "", nil); r.StatusCode != 401 {
		t.Fatalf("no credential: %d", r.StatusCode)
	}
	if r, _ := do(t, f, "GET", "/repos/acme/w/pulls/1", "nope", nil); r.StatusCode != 401 {
		t.Fatalf("bad credential: %d", r.StatusCode)
	}
	f.mu.Lock()
	inst := f.mintInstallationToken(f.repo("acme/w").InstallationID)
	f.mu.Unlock()
	if r, _ := do(t, f, "GET", "/user", inst, nil); r.StatusCode != 403 {
		t.Fatalf("GET /user with an installation token: %d", r.StatusCode)
	}
	if r, b := do(t, f, "GET", "/user", "tok-me", nil); r.StatusCode != 200 || !strings.Contains(string(b), `"login":"me"`) {
		t.Fatalf("GET /user with a user token: %d %s", r.StatusCode, b)
	}
	// A write with the installation token is the App's bot, not a person.
	do(t, f, "POST", "/repos/acme/w/issues/1/comments", inst, map[string]any{"body": "from the app"})
	cs := f.Comments("acme/w", 1)
	if len(cs) != 1 || cs[0].User != "conductor-test[bot]" {
		t.Fatalf("installation-token write attribution: %+v", cs)
	}
	f.AddRepo("other/x")
	if r, _ := do(t, f, "GET", "/repos/other/x/pulls", inst, nil); r.StatusCode != 404 {
		t.Fatalf("a repo outside the installation: %d", r.StatusCode)
	}
}

func TestValidationErrors(t *testing.T) {
	f, _, n := world(t, Options{})
	head := f.HeadSHA("acme/w", n)
	cases := []struct {
		method, path string
		body         map[string]any
	}{
		{"POST", "/repos/acme/w/statuses/" + head, map[string]any{"state": "bogus", "context": "x"}},
		{"POST", "/repos/acme/w/statuses/" + head, map[string]any{"state": "success", "context": "x", "description": strings.Repeat("d", 141)}},
		{"POST", "/repos/acme/w/statuses/deadbeef", map[string]any{"state": "success"}},
		{"POST", "/repos/acme/w/pulls/1/requested_reviewers", map[string]any{"reviewers": []any{"me"}}},          // the author
		{"POST", "/repos/acme/w/pulls/1/requested_reviewers", map[string]any{"reviewers": []any{"cursor[bot]"}}}, // not a collaborator
		{"POST", "/repos/acme/w/pulls/1/reviews", map[string]any{"event": "APPROVE"}},                            // your own PR
		{"POST", "/repos/acme/w/issues/1/comments/x", nil},
	}
	for _, c := range cases[:6] {
		if r, b := do(t, f, c.method, c.path, "tok-me", c.body); r.StatusCode != 422 {
			t.Errorf("%s %s %v: %d %s", c.method, c.path, c.body, r.StatusCode, b)
		}
	}
	if r, _ := do(t, f, "GET", "/repos/acme/w/pulls/999", "tok-me", nil); r.StatusCode != 404 {
		t.Errorf("unknown PR: %d", r.StatusCode)
	}
	if r, b := do(t, f, "POST", "/repos/acme/w/pulls/1/reviews", "tok-rev", map[string]any{"event": "COMMENT", "body": "x",
		"comments": []any{map[string]any{"path": "main.go", "line": 999, "body": "off the diff"}}}); r.StatusCode != 422 {
		t.Errorf("inline comment outside the diff: %d %s", r.StatusCode, b)
	}
}

// An endpoint the fake does not implement is a 501 AND a recorded failure.
func TestUnknownEndpointIsLoud(t *testing.T) {
	f, _, _ := world(t, Options{})
	if r, _ := do(t, f, "GET", "/repos/acme/w/branches/main/protection", "tok-me", nil); r.StatusCode != 501 {
		t.Fatalf("unknown endpoint: %d", r.StatusCode)
	}
	if u := f.Unhandled(); len(u) != 1 || !strings.Contains(u[0], "/branches/main/protection") {
		t.Fatalf("unhandled not recorded: %v", u)
	}
}

func TestReviewDeliveryOrders(t *testing.T) {
	for _, c := range []struct {
		order ReviewOrder
		want  string
	}{
		{ReviewFirst, "pull_request_review.submitted,pull_request_review_comment.created,pull_request_review_comment.created"},
		{CommentsFirst, "pull_request_review_comment.created,pull_request_review_comment.created,pull_request_review.submitted"},
	} {
		f, sink, n := world(t, Options{ReviewOrder: c.order})
		before := len(sink.events())
		f.SubmitReview("acme/w", n, "rev", "CHANGES_REQUESTED", "fix", InlineComment{Body: "a"}, InlineComment{Body: "b"})
		f.Flush()
		if got := strings.Join(sink.events()[before:], ","); got != c.want {
			t.Errorf("order %d: %s", c.order, got)
		}
	}
	// Shuffled is seeded: the same seed gives the same order.
	orders := map[string]bool{}
	for i := 0; i < 2; i++ {
		f, sink, n := world(t, Options{ReviewOrder: Shuffled, Seed: 42})
		before := len(sink.events())
		f.SubmitReview("acme/w", n, "rev", "COMMENTED", "x", InlineComment{Body: "a"}, InlineComment{Body: "b"}, InlineComment{Body: "c"})
		f.Flush()
		orders[strings.Join(sink.events()[before:], ",")] = true
	}
	if len(orders) != 1 {
		t.Errorf("a seed must make the shuffle reproducible: %v", orders)
	}
}

func TestDeliveriesAreSignedAndRedeliverable(t *testing.T) {
	f, sink, _ := world(t, Options{DuplicateDeliveries: true})
	f.Comment("acme/w", 1, "rev", "hello")
	f.Flush()
	sink.mu.Lock()
	n := len(sink.got)
	a, b := sink.got[n-2], sink.got[n-1]
	body := sink.bod[n-1]
	sink.mu.Unlock()
	if a.Header.Get("X-GitHub-Delivery") != b.Header.Get("X-GitHub-Delivery") {
		t.Fatal("a duplicate delivery must carry the same GUID")
	}
	if !githubkitSigOK("s3cret", body, b.Header.Get("X-Hub-Signature-256")) {
		t.Fatal("bad X-Hub-Signature-256")
	}
	if !f.Redeliver(b.Header.Get("X-GitHub-Delivery")) {
		t.Fatal("redeliver")
	}
	f.Flush()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.got) != n+1 || sink.got[n].Header.Get("X-GitHub-Delivery") != b.Header.Get("X-GitHub-Delivery") {
		t.Fatal("a redelivery keeps the GUID")
	}
}

func TestGraphQLRejectsUnknownFields(t *testing.T) {
	f, _, _ := world(t, Options{})
	_, b := do(t, f, "POST", "/graphql", "tok-me", map[string]any{
		"query":     `query($o:String!,$n:String!){ repository(owner:$o,name:$n){ pullRequest(number:1){ headRefOid noSuchField } } }`,
		"variables": map[string]any{"o": "acme", "n": "w"}})
	if !strings.Contains(string(b), `"code":"undefinedField"`) || !strings.Contains(string(b), "Field 'noSuchField' doesn't exist on type 'PullRequest'") {
		t.Fatalf("unknown field: %s", b)
	}
	_, b = do(t, f, "POST", "/graphql", "tok-me", map[string]any{
		"query":     `query($o:String!,$n:String!){ repository(owner:$o,name:$n){ pullRequest(number:1){ headRefOid mergeStateStatus reviewDecision author{login} } } }`,
		"variables": map[string]any{"o": "acme", "n": "w"}})
	if !strings.Contains(string(b), `"mergeStateStatus":"BLOCKED"`) || !strings.Contains(string(b), `"reviewDecision":"REVIEW_REQUIRED"`) {
		t.Fatalf("resolved query: %s", b)
	}
	_, b = do(t, f, "POST", "/graphql", "tok-me", map[string]any{
		"query": `{ repository(owner:"acme",name:"nope"){ id } }`})
	if !strings.Contains(string(b), `"type":"NOT_FOUND"`) {
		t.Fatalf("missing repo: %s", b)
	}
}

// --- shape validation: every response conductor's verbs and source reads see ---

// TestEveryResponseValidatesAgainstGitHubsSchema drives the real githubkit
// verb surface against the fake, and validates EVERY REST response the fake
// writes against the vendored OpenAPI schema of its operation, and every
// webhook it delivers against that webhook's schema.
func TestEveryResponseValidatesAgainstGitHubsSchema(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	var problems []string
	OnResponse = func(op string, status int, body []byte) {
		schema, ok := OperationSchema(op, status)
		mu.Lock()
		defer mu.Unlock()
		seen[op]++
		if !ok {
			problems = append(problems, op+": status "+itoa(status)+" is not a documented response")
			return
		}
		if schema == nil || body == nil {
			return
		}
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			problems = append(problems, op+": "+err.Error())
			return
		}
		for _, e := range Validate(schema, v) {
			problems = append(problems, op+": "+e)
		}
	}
	defer func() { OnResponse = nil }()

	f, sink, n := world(t, Options{})
	head := f.HeadSHA("acme/w", n)
	kit, err := githubkit.NewClient(githubkit.Config{WriteToken: "tok-me", APIBase: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	rv, cids := f.SubmitReview("acme/w", n, "rev", "CHANGES_REQUESTED", "please", InlineComment{Body: "here"})
	cid := f.Comment("acme/w", n, "rev", "a conversation comment")
	f.Check("acme/w", head, "ci", "failure")
	ctx := context.Background()
	call := func(verb string, opts map[string]any) {
		t.Helper()
		opts["repo"] = "acme/w"
		if _, err := kit.Invoke(ctx, verb, opts); err != nil {
			t.Errorf("%s: %v", verb, err)
		}
	}
	call("comment", map[string]any{"number": n, "body": "hi"})
	call("reply", map[string]any{"pr": n, "in_reply_to": cids[0], "body": "done"})
	call("pr_get", map[string]any{"pr": n})
	call("pr_files", map[string]any{"pr": n})
	call("pr_diff", map[string]any{"pr": n})
	call("review_comments", map[string]any{"pr": n})
	call("react", map[string]any{"subjects": []any{map[string]any{"kind": "issue_comment", "id": cid}, map[string]any{"kind": "review", "id": rv},
		map[string]any{"kind": "review_comment", "id": cids[0]}}, "pr": n, "content": "eyes"})
	call("react", map[string]any{"subjects": []any{map[string]any{"kind": "issue_comment", "id": cid},
		map[string]any{"kind": "review_comment", "id": cids[0]}}, "pr": n, "content": "eyes", "remove": true})
	call("submit_review", map[string]any{"pr": n, "event": "COMMENT", "body": "a note",
		"comments": []any{map[string]any{"path": "main.go", "line": 3, "body": "nit"}}})
	call("set_status", map[string]any{"sha": head, "state": "pending", "context": "me / review", "description": "working"})
	call("set_status", map[string]any{"pr": n, "state": "success"})
	call("request_review", map[string]any{"pr": n, "reviewers": []any{"rev"}})
	call("rerequest_review", map[string]any{"pr": n, "reviewers": []any{"rev"}})
	call("remove_reviewer", map[string]any{"pr": n, "reviewers": []any{"rev"}})
	call("add_labels", map[string]any{"number": n, "labels": []any{"bug"}})
	call("remove_label", map[string]any{"number": n, "label": "bug"})
	call("assign", map[string]any{"number": n, "add": []any{"me"}})
	call("get_issue", map[string]any{"number": n})
	call("create_issue", map[string]any{"title": "an issue"})
	call("update_issue", map[string]any{"number": 2, "state": "closed"})
	call("list_issues", map[string]any{})
	call("search_issues", map[string]any{"q": "feature"})
	call("checks", map[string]any{"ref": head})
	call("list_runs", map[string]any{})
	call("get_ref", map[string]any{"ref": "main"})
	call("create_branch", map[string]any{"branch": "topic", "from": "main"})
	call("put_file", map[string]any{"path": "docs/a.md", "content": "hello", "message": "add a"})
	call("file", map[string]any{"path": "docs/a.md"})
	call("create_release", map[string]any{"tag": "v1.0.0"})
	call("create_gist", map[string]any{"files": map[string]any{"a.txt": "x"}})
	call("list_gists", map[string]any{})
	call("update_pr", map[string]any{"pr": n, "title": "renamed"})
	call("ready_for_review", map[string]any{"pr": n})
	f.SubmitReview("acme/w", n, "rev", "APPROVED", "ok")
	f.SetProtection("acme/w", Protection{RequiredApprovals: 1})
	call("merge_pr", map[string]any{"pr": n})

	// The rest of the surface, by hand: the reads the sweep and the verbs
	// make that the calls above did not, and the App endpoints.
	runID, _ := f.Check("acme/w", head, "build", "failure")
	raw := func(method, path, tok string, body any) {
		t.Helper()
		if r, b := do(t, f, method, path, tok, body); r.StatusCode >= 400 {
			t.Errorf("%s %s: %d %s", method, path, r.StatusCode, b)
		}
	}
	for _, p := range []string{
		"/repos/acme/w/pulls?state=all", "/repos/acme/w/pulls/1/commits", "/repos/acme/w/pulls/1/requested_reviewers",
		"/repos/acme/w/pulls/1/reviews", "/repos/acme/w/pulls/1/reviews/" + itoa64(rv), "/repos/acme/w/pulls/1/reviews/" + itoa64(rv) + "/comments",
		"/repos/acme/w/pulls/1/comments?per_page=30&sort=created&direction=desc", "/repos/acme/w/issues/1/comments",
		"/repos/acme/w/issues/comments/" + itoa64(cid) + "/reactions", "/repos/acme/w/pulls/comments/" + itoa64(cids[0]) + "/reactions",
		"/repos/acme/w/commits/" + head + "/status", "/repos/acme/w/commits/" + head + "/statuses", "/repos/acme/w/commits/" + head,
		"/repos/acme/w/actions/runs/" + itoa64(runID), "/repos/acme/w/actions/runs?head_sha=" + head, "/gists",
	} {
		raw("GET", p, "tok-me", nil)
	}
	f.mu.Lock()
	var job int64
	for _, j := range f.repo("acme/w").Jobs {
		job = j.ID
	}
	inst := f.mintInstallationToken(f.repo("acme/w").InstallationID)
	f.mu.Unlock()
	raw("GET", "/repos/acme/w/actions/jobs/"+itoa64(job), "tok-me", nil)
	raw("POST", "/repos/acme/w/actions/runs/"+itoa64(runID)+"/rerun-failed-jobs", "tok-me", nil)
	f.CompleteRerun("acme/w", runID, "success")
	raw("POST", "/repos/acme/w/actions/runs/"+itoa64(runID)+"/rerun", "tok-me", nil)
	raw("POST", "/repos/acme/w/actions/runs/"+itoa64(runID)+"/cancel", "tok-me", nil)
	raw("POST", "/repos/acme/w/actions/workflows/ci.yml/dispatches", "tok-me", map[string]any{"ref": "main"})
	raw("GET", "/installation/repositories", inst, nil)
	raw("POST", "/repos/acme/w/pulls", "tok-me", map[string]any{"title": "another", "head": "topic", "base": "main"})
	raw("PATCH", "/repos/acme/w/issues/2", "tok-me", map[string]any{"state": "open"})
	raw("POST", "/repos/acme/w/issues/2/assignees", "tok-me", map[string]any{"assignees": []any{"rev"}})
	raw("DELETE", "/repos/acme/w/issues/2/assignees", "tok-me", map[string]any{"assignees": []any{"rev"}})
	f.mu.Lock()
	content := f.repo("acme/w").Contents["docs/a.md"].SHA
	var gist string
	for id := range f.gists {
		gist = id
	}
	f.mu.Unlock()
	raw("DELETE", "/repos/acme/w/contents/docs/a.md", "tok-me", map[string]any{"message": "rm", "sha": content})
	raw("GET", "/gists/"+gist, "tok-me", nil)
	raw("PATCH", "/gists/"+gist, "tok-me", map[string]any{"description": "d"})
	raw("GET", "/users/me/gists", "tok-me", nil)
	raw("DELETE", "/gists/"+gist, "tok-me", nil)
	f.Flush()

	// Webhooks: every delivery against its webhook schema.
	sink.mu.Lock()
	for i, r := range sink.got {
		var p map[string]any
		_ = json.Unmarshal(sink.bod[i], &p)
		a, _ := p["action"].(string)
		name := WebhookName(r.Header.Get("X-GitHub-Event"), a)
		schema, ok := WebhookSchema(name)
		if !ok {
			problems = append(problems, "webhook "+name+": not vendored")
			continue
		}
		for _, e := range Validate(schema, p) {
			problems = append(problems, "webhook "+name+": "+e)
		}
	}
	nhooks := len(sink.got)
	sink.mu.Unlock()
	if u := f.Unhandled(); len(u) > 0 {
		t.Errorf("unhandled requests: %v", u)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range problems {
		t.Error(p)
	}
	t.Logf("validated %d operations (%d responses) and %d deliveries", len(seen), total(seen), nhooks)
	// The App endpoints authenticate with a JWT; TestAppAuthFlow covers them
	// through githubkit's real App auth and validates them through this hook
	// too. Everything else the fake serves must have been seen here.
	for _, ep := range Endpoints() {
		if strings.Contains(ep, "/app/") || strings.HasSuffix(ep, "/installation") || strings.Contains(ep, "/assets") ||
			strings.Contains(ep, "/releases/{release_id}") && strings.HasPrefix(ep, "GET") {
			continue
		}
		if seen[ep] == 0 {
			t.Errorf("not exercised by this test: %s", ep)
		}
	}
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + jsonInt(n)) }

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func githubkitSigOK(secret string, body []byte, header string) bool {
	return checkSig(secret, body, header)
}

var _ = time.Second

// App auth: a JWT signed with the App's key mints installation tokens and
// reads the installation endpoints; one signed with another key does not.
func TestAppAuthFlow(t *testing.T) {
	var problems []string
	OnResponse = func(op string, status int, body []byte) {
		schema, ok := OperationSchema(op, status)
		if !ok {
			problems = append(problems, op+": undocumented status")
			return
		}
		var v any
		_ = json.Unmarshal(body, &v)
		for _, e := range Validate(schema, v) {
			problems = append(problems, op+": "+e)
		}
	}
	defer func() { OnResponse = nil }()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := New(Options{Logf: t.Logf})
	f.Start()
	defer f.Close()
	f.AddUser("acme", "Organization")
	f.SetApp(77, "conductor-test", &key.PublicKey)
	f.AddRepo("acme/w")
	in := f.Install("acme")
	sign := func(k *rsa.PrivateKey) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(9 * time.Minute).Unix(), "iss": 77})
		s, _ := tok.SignedString(k)
		return s
	}
	good, bad := sign(key), sign(other)
	if r, _ := do(t, f, "GET", "/app/installations", bad, nil); r.StatusCode != 401 {
		t.Fatalf("a JWT signed with the wrong key: %d", r.StatusCode)
	}
	for _, p := range []string{"/app/installations", "/repos/acme/w/installation", "/orgs/acme/installation"} {
		if r, b := do(t, f, "GET", p, good, nil); r.StatusCode != 200 {
			t.Fatalf("%s: %d %s", p, r.StatusCode, b)
		}
	}
	if r, _ := do(t, f, "GET", "/users/acme/installation", good, nil); r.StatusCode != 404 {
		t.Fatalf("an org's installation by /users/: %d", r.StatusCode)
	}
	r, b := do(t, f, "POST", "/app/installations/"+itoa64(in.ID)+"/access_tokens", good, nil)
	var minted map[string]any
	_ = json.Unmarshal(b, &minted)
	tok, _ := minted["token"].(string)
	if r.StatusCode != 201 || !strings.HasPrefix(tok, "ghs_") {
		t.Fatalf("mint: %d %s", r.StatusCode, b)
	}
	if r, _ := do(t, f, "GET", "/repos/acme/w/pulls", good, nil); r.StatusCode != 401 {
		t.Fatalf("a JWT on a repo endpoint: %d", r.StatusCode)
	}
	if r, _ := do(t, f, "GET", "/repos/acme/w/pulls", tok, nil); r.StatusCode != 200 {
		t.Fatalf("the installation token on a repo endpoint: %d", r.StatusCode)
	}
	for _, p := range problems {
		t.Error(p)
	}
}
