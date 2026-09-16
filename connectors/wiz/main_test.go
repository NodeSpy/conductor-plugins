package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

func nopCtx() context.Context { return context.Background() }

// --- test fixtures: fake OAuth2 + GraphQL servers ---

// fakeWiz is an httptest-backed stand-in for Wiz's auth + GraphQL endpoints.
// It counts token fetches so tests can assert caching behavior, and lets a
// test swap in an arbitrary GraphQL responder.
type fakeWiz struct {
	authServer   *httptest.Server
	apiServer    *httptest.Server
	tokenFetches int32
	lastAuthForm url.Values
	graphqlFn    func(w http.ResponseWriter, req graphqlRequest)
	lastAuth     map[string]any
	accessToken  string
	expiresIn    int64
}

func newFakeWiz(t *testing.T) *fakeWiz {
	t.Helper()
	f := &fakeWiz{accessToken: "tok-123", expiresIn: 3600}
	f.authServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("auth server: parse form: %v", err)
		}
		atomic.AddInt32(&f.tokenFetches, 1)
		f.lastAuthForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": f.accessToken,
			"expires_in":   f.expiresIn,
		})
	}))
	f.apiServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+f.accessToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("api server: decode request: %v", err)
		}
		if f.graphqlFn != nil {
			f.graphqlFn(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}))
	t.Cleanup(func() {
		f.authServer.Close()
		f.apiServer.Close()
	})
	return f
}

func (f *fakeWiz) conn() wizConn {
	return wizConn{
		clientID: "client-id", clientSecret: "client-secret",
		apiURL: f.apiServer.URL, authURL: f.authServer.URL, audience: defaultAudience,
	}
}

func (f *fakeWiz) invokeConn() map[string]any {
	return map[string]any{
		"client_id": "client-id", "client_secret": "client-secret",
		"api_url": f.apiServer.URL, "auth_url": f.authServer.URL,
	}
}

// --- OAuth2 token: fetch + form body + caching ---

func TestTokenFetchAndCache(t *testing.T) {
	f := newFakeWiz(t)
	p := newWizPlugin()

	tok, err := p.token(nopCtx(), f.conn())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if tok != "tok-123" {
		t.Fatalf("token: got %q", tok)
	}
	if got := atomic.LoadInt32(&f.tokenFetches); got != 1 {
		t.Fatalf("expected 1 token fetch, got %d", got)
	}

	// Assert the form body carries the client-credentials grant.
	form := f.lastAuthForm
	if form.Get("grant_type") != "client_credentials" {
		t.Errorf("grant_type: got %q", form.Get("grant_type"))
	}
	if form.Get("audience") != defaultAudience {
		t.Errorf("audience: got %q", form.Get("audience"))
	}
	if form.Get("client_id") != "client-id" {
		t.Errorf("client_id: got %q", form.Get("client_id"))
	}
	if form.Get("client_secret") != "client-secret" {
		t.Errorf("client_secret: got %q", form.Get("client_secret"))
	}

	// A second call within expiry must NOT re-fetch.
	tok2, err := p.token(nopCtx(), f.conn())
	if err != nil {
		t.Fatalf("token (cached): %v", err)
	}
	if tok2 != tok {
		t.Fatalf("cached token mismatch: %q vs %q", tok2, tok)
	}
	if got := atomic.LoadInt32(&f.tokenFetches); got != 1 {
		t.Fatalf("expected still 1 token fetch after cache hit, got %d", got)
	}
}

func TestTokenCacheKeyedPerClient(t *testing.T) {
	f := newFakeWiz(t)
	p := newWizPlugin()
	c1 := f.conn()
	c2 := f.conn()
	c2.clientID = "other-client"

	if _, err := p.token(nopCtx(), c1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.token(nopCtx(), c2); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&f.tokenFetches); got != 2 {
		t.Fatalf("expected 2 fetches for 2 distinct client_ids, got %d", got)
	}
}

// --- GraphQL transport: auth header + query/variables + errors ---

func TestDoGraphQLSendsBearerAndBody(t *testing.T) {
	f := newFakeWiz(t)
	var gotQuery string
	var gotVars map[string]any
	f.graphqlFn = func(w http.ResponseWriter, req graphqlRequest) {
		gotQuery = req.Query
		gotVars = req.Variables
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	}
	p := newWizPlugin()
	data, err := p.doGraphQL(nopCtx(), f.conn(), "query Q { ok }", map[string]any{"a": 1})
	if err != nil {
		t.Fatalf("doGraphQL: %v", err)
	}
	if gotQuery != "query Q { ok }" {
		t.Errorf("query: got %q", gotQuery)
	}
	if gotVars["a"] != float64(1) {
		t.Errorf("variables: got %#v", gotVars)
	}
	var out struct {
		Ok bool `json:"ok"`
	}
	_ = json.Unmarshal(data, &out)
	if !out.Ok {
		t.Errorf("expected ok:true in response data, got %s", string(data))
	}
}

func TestDoGraphQLErrorsArray(t *testing.T) {
	f := newFakeWiz(t)
	f.graphqlFn = func(w http.ResponseWriter, req graphqlRequest) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{{"message": "field not found"}},
		})
	}
	p := newWizPlugin()
	_, err := p.doGraphQL(nopCtx(), f.conn(), "query Bad { nope }", nil)
	if err == nil {
		t.Fatal("expected error for graphql errors array")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "field not found") {
		t.Errorf("error message missing graphql error text: %q", pe.Message)
	}
}

func TestDoGraphQLNon2xx(t *testing.T) {
	f := newFakeWiz(t)
	f.graphqlFn = func(w http.ResponseWriter, req graphqlRequest) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server exploded"))
	}
	p := newWizPlugin()
	_, err := p.doGraphQL(nopCtx(), f.conn(), "query Q { ok }", nil)
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "server exploded") {
		t.Errorf("error message missing response body: %q", pe.Message)
	}
}

// --- verbs ---

func TestInvokeIssues(t *testing.T) {
	f := newFakeWiz(t)
	f.graphqlFn = func(w http.ResponseWriter, req graphqlRequest) {
		if !strings.Contains(req.Query, "issues(") {
			t.Errorf("expected issues query, got %q", req.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{map[string]any{"id": "i1", "status": "OPEN"}},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": "c1"},
				},
			},
		})
	}
	p := newWizPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "issues",
		Connection: f.invokeConn(),
		Options:    map[string]any{"filter": map[string]any{"status": []any{"OPEN"}}, "first": 10},
	})
	if err != nil {
		t.Fatalf("Invoke issues: %v", err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if res.Outputs["page_info"] == nil {
		t.Fatalf("page_info missing: %#v", res.Outputs)
	}
}

func TestInvokeUpdateIssue(t *testing.T) {
	f := newFakeWiz(t)
	var gotVars map[string]any
	f.graphqlFn = func(w http.ResponseWriter, req graphqlRequest) {
		if !strings.Contains(req.Query, "mutation UpdateIssue") {
			t.Errorf("expected update mutation, got %q", req.Query)
		}
		gotVars = req.Variables
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"updateIssue": map[string]any{"issue": map[string]any{"id": "i1", "status": "RESOLVED"}}},
		})
	}
	p := newWizPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "update_issue",
		Connection: f.invokeConn(),
		Options:    map[string]any{"id": "i1", "status": "RESOLVED", "note": "fixed", "resolution_reason": "Remediated"},
	})
	if err != nil {
		t.Fatalf("Invoke update_issue: %v", err)
	}
	input, ok := gotVars["input"].(map[string]any)
	if !ok || input["id"] != "i1" {
		t.Fatalf("input.id: %#v", gotVars)
	}
	patch, ok := input["patch"].(map[string]any)
	if !ok || patch["status"] != "RESOLVED" || patch["note"] != "fixed" || patch["resolutionReason"] != "Remediated" {
		t.Fatalf("patch: %#v", patch)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "RESOLVED" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeUpdateIssueRequiresID(t *testing.T) {
	f := newFakeWiz(t)
	p := newWizPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "update_issue", Connection: f.invokeConn(), Options: map[string]any{"status": "RESOLVED"}})
	if err == nil {
		t.Fatal("expected error when id is missing")
	}
}

func TestInvokeFindings(t *testing.T) {
	f := newFakeWiz(t)
	f.graphqlFn = func(w http.ResponseWriter, req graphqlRequest) {
		if !strings.Contains(req.Query, "vulnerabilityFindings(") {
			t.Errorf("expected findings query, got %q", req.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"vulnerabilityFindings": map[string]any{
					"nodes":    []any{map[string]any{"id": "f1"}},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		})
	}
	p := newWizPlugin()
	for _, verb := range []string{"findings", "vulnerabilities"} {
		res, err := p.Invoke(plugin.InvokeRequest{Verb: verb, Connection: f.invokeConn()})
		if err != nil {
			t.Fatalf("Invoke %s: %v", verb, err)
		}
		items, ok := res.Outputs["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("%s items: %#v", verb, res.Outputs["items"])
		}
	}
}

func TestInvokeProjects(t *testing.T) {
	f := newFakeWiz(t)
	f.graphqlFn = func(w http.ResponseWriter, req graphqlRequest) {
		if !strings.Contains(req.Query, "query Projects") {
			t.Errorf("expected projects query, got %q", req.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"projects": map[string]any{"nodes": []any{map[string]any{"id": "p1", "name": "prod"}}}},
		})
	}
	p := newWizPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "projects", Connection: f.invokeConn()})
	if err != nil {
		t.Fatalf("Invoke projects: %v", err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeGraphQLEscapeHatch(t *testing.T) {
	f := newFakeWiz(t)
	f.graphqlFn = func(w http.ResponseWriter, req graphqlRequest) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"whoami": "tester"}})
	}
	p := newWizPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "graphql",
		Connection: f.invokeConn(),
		Options:    map[string]any{"query": "query { whoami }"},
	})
	if err != nil {
		t.Fatalf("Invoke graphql: %v", err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["whoami"] != "tester" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeMissingConnection(t *testing.T) {
	p := newWizPlugin()
	cases := []map[string]any{
		{},
		{"client_id": "x"},
		{"client_id": "x", "client_secret": "y"},
	}
	for _, conn := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "projects", Connection: conn})
		if err == nil {
			t.Errorf("expected error for connection %#v", conn)
		}
	}
}

// --- Describe ---

func TestDescribe(t *testing.T) {
	d := newWizPlugin().Describe()
	if d.Kind != plugin.KindConnector || d.Type != "wiz" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("wiz spawns nothing: %#v", d.Capabilities)
	}
	wantEgress := []string{"auth.app.wiz.io:443", "api.*.app.wiz.io:443"}
	if !equalStrings(d.Capabilities.Egress, wantEgress) {
		t.Fatalf("egress: got %#v want %#v", d.Capabilities.Egress, wantEgress)
	}
	wantVerbs := []string{"issues", "issue_get", "update_issue", "findings", "vulnerabilities", "projects", "graphql"}
	got := map[string]bool{}
	for _, v := range d.Verbs {
		got[v.Name] = true
	}
	for _, w := range wantVerbs {
		if !got[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Events) != 1 || d.Events[0].Name != "issue" {
		t.Fatalf("events: %#v", d.Events)
	}
}

// --- webhook source ---

func TestParseIssuePayload(t *testing.T) {
	body := []byte(`{"issue_id":"iss-1","title":"Exposed bucket","severity":"CRITICAL","status":"OPEN","entity":"my-bucket","project":"prod","url":"https://app.wiz.io/issues/iss-1"}`)
	p := parseIssuePayload(body)
	if p.IssueID != "iss-1" || p.Title != "Exposed bucket" || p.Severity != "CRITICAL" || p.Status != "OPEN" || p.Entity != "my-bucket" || p.Project != "prod" || p.URL != "https://app.wiz.io/issues/iss-1" {
		t.Fatalf("parsed payload: %#v", p)
	}
}

func TestRequireWebhookSecret(t *testing.T) {
	if err := requireWebhookSecret("wiz", "", false, "webhook.secret"); err == nil {
		t.Fatal("expected fail-closed error with no secret and no allow_unsigned")
	}
	if err := requireWebhookSecret("wiz", "", true, "webhook.secret"); err != nil {
		t.Fatalf("allow_unsigned should permit an empty secret: %v", err)
	}
	if err := requireWebhookSecret("wiz", "shh", false, "webhook.secret"); err != nil {
		t.Fatalf("a configured secret should never error: %v", err)
	}
}

func TestCheckTokenHeaderAndQuery(t *testing.T) {
	wc := wizWebhookConfig{secret: "s3cr3t", header: defaultTokenHeader}

	mk := func(header, query string) *sourcekit.Request {
		r := &sourcekit.Request{Header: http.Header{}, Query: url.Values{}}
		if header != "" {
			r.Header.Set(defaultTokenHeader, header)
		}
		if query != "" {
			r.Query.Set("token", query)
		}
		return r
	}

	if !checkToken(wc, mk("s3cr3t", "")) {
		t.Error("expected header token to be accepted")
	}

	if !checkToken(wc, mk("", "s3cr3t")) {
		t.Error("expected query token to be accepted")
	}

	if checkToken(wc, mk("", "nope")) {
		t.Error("expected wrong token to be rejected")
	}

	if checkToken(wc, mk("", "")) {
		t.Error("expected missing token to be rejected")
	}
}

func TestStartSourceRequiresListenAddr(t *testing.T) {
	p := newWizPlugin()
	err := p.StartSource(context.Background(), plugin.StartSourceRequest{
		Instance: "test",
		Config:   map[string]any{"webhook": map[string]any{"secret": "topsecret"}},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected error when webhook.listen is not configured")
	}
}

func TestStartSourceFailsClosedWithoutSecret(t *testing.T) {
	p := newWizPlugin()
	err := p.StartSource(context.Background(), plugin.StartSourceRequest{
		Instance: "test",
		Config:   map[string]any{"webhook": map[string]any{"listen": "127.0.0.1:0"}},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected fail-closed error when webhook.secret is absent and allow_unsigned is not set")
	}
}

// --- dedup semantics (issue_id + status) ---

func TestDedupOnIssueIDAndStatus(t *testing.T) {
	d := sourcekit.NewDedup(16)
	iss1 := parseIssuePayload([]byte(`{"issue_id":"i1","status":"OPEN"}`))
	dk1 := iss1.IssueID + "\x00" + iss1.Status
	if !d.Add(dk1) {
		t.Fatal("first delivery should not be a duplicate")
	}
	if d.Add(dk1) {
		t.Fatal("identical redelivery should be deduped")
	}
	iss2 := parseIssuePayload([]byte(`{"issue_id":"i1","status":"RESOLVED"}`))
	dk2 := iss2.IssueID + "\x00" + iss2.Status
	if !d.Add(dk2) {
		t.Fatal("same issue_id with a different status should NOT be deduped")
	}
}

// --- misc helpers for tests ---

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
