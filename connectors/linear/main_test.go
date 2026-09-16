package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- TestDescribe ---

func TestDescribe(t *testing.T) {
	d := newLinearPlugin().Describe()
	if d.Kind != plugin.KindConnector {
		t.Fatalf("Kind = %v, want KindConnector", d.Kind)
	}
	if d.Type != "linear" {
		t.Fatalf("Type = %q, want linear", d.Type)
	}
	if !contains(d.Capabilities.Egress, "api.linear.app:443") {
		t.Fatalf("Capabilities.Egress = %v, want api.linear.app:443", d.Capabilities.Egress)
	}
	if d.Capabilities.Spawns || len(d.Capabilities.Commands) != 0 {
		t.Fatalf("Capabilities should declare no commands/spawns: %#v", d.Capabilities)
	}

	wantEvents := map[string]bool{"issue": true, "comment": true, "project": true}
	gotEvents := map[string]bool{}
	for _, e := range d.Events {
		gotEvents[e.Name] = true
	}
	for name := range wantEvents {
		if !gotEvents[name] {
			t.Errorf("Describe missing event %q", name)
		}
	}

	wantVerbs := []string{"create_issue", "update_issue", "comment", "get_issue", "search_issues", "archive_issue", "graphql"}
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

// --- webhook event parsing ---

func TestParseWebhookIssueCreate(t *testing.T) {
	body := []byte(`{
		"action": "create",
		"type": "Issue",
		"data": {
			"id": "issue-1",
			"identifier": "ENG-123",
			"title": "Fix the thing",
			"priority": 2,
			"url": "https://linear.app/acme/issue/ENG-123",
			"state": {"name": "In Progress"},
			"assignee": {"name": "Ada"},
			"team": {"name": "Engineering", "key": "ENG"}
		}
	}`)
	ev := parseWebhook(body)
	if ev == nil {
		t.Fatal("parseWebhook returned nil")
	}
	if ev.Event != "issue" {
		t.Fatalf("Event = %q, want issue", ev.Event)
	}
	if ev.Dedup != "issue-1:create" {
		t.Fatalf("Dedup = %q, want issue-1:create", ev.Dedup)
	}
	want := map[string]any{
		"id": "issue-1", "identifier": "ENG-123", "title": "Fix the thing",
		"state": "In Progress", "priority": 2, "assignee": "Ada", "team": "ENG",
		"url": "https://linear.app/acme/issue/ENG-123", "action": "create",
	}
	for k, v := range want {
		if ev.Context[k] != v {
			t.Errorf("context[%q] = %v, want %v", k, ev.Context[k], v)
		}
	}
	// Filter-alias keys must be present for the daemon's generic filter evaluator.
	for _, k := range []string{"types", "actions", "states", "teams", "priorities"} {
		if _, ok := ev.Context[k]; !ok {
			t.Errorf("context missing filter alias key %q", k)
		}
	}
}

func TestParseWebhookCommentCreate(t *testing.T) {
	body := []byte(`{
		"action": "create",
		"type": "Comment",
		"data": {
			"id": "comment-1",
			"body": "looks good",
			"issueId": "issue-1",
			"user": {"name": "Grace"}
		}
	}`)
	ev := parseWebhook(body)
	if ev == nil {
		t.Fatal("parseWebhook returned nil")
	}
	if ev.Event != "comment" {
		t.Fatalf("Event = %q, want comment", ev.Event)
	}
	if ev.Dedup != "comment-1:create" {
		t.Fatalf("Dedup = %q, want comment-1:create", ev.Dedup)
	}
	want := map[string]any{
		"id": "comment-1", "body": "looks good", "issue_id": "issue-1",
		"user": "Grace", "action": "create",
	}
	for k, v := range want {
		if ev.Context[k] != v {
			t.Errorf("context[%q] = %v, want %v", k, ev.Context[k], v)
		}
	}
}

func TestParseWebhookProject(t *testing.T) {
	body := []byte(`{
		"action": "update",
		"type": "Project",
		"data": {"id": "proj-1", "name": "Q3 Launch", "state": "started", "url": "https://linear.app/acme/project/q3"}
	}`)
	ev := parseWebhook(body)
	if ev == nil {
		t.Fatal("parseWebhook returned nil")
	}
	if ev.Event != "project" {
		t.Fatalf("Event = %q, want project", ev.Event)
	}
	if ev.Dedup != "proj-1:update" {
		t.Fatalf("Dedup = %q, want proj-1:update", ev.Dedup)
	}
}

func TestParseWebhookUnknownTypeIgnored(t *testing.T) {
	body := []byte(`{"action": "create", "type": "Reaction", "data": {"id": "x"}}`)
	if ev := parseWebhook(body); ev != nil {
		t.Fatalf("expected nil for unknown type, got %+v", ev)
	}
}

func TestParseWebhookMalformedIgnored(t *testing.T) {
	if ev := parseWebhook([]byte(`not json`)); ev != nil {
		t.Fatalf("expected nil for malformed body, got %+v", ev)
	}
	if ev := parseWebhook([]byte(`{"action":"create","type":"Issue"}`)); ev != nil {
		t.Fatalf("expected nil for missing data, got %+v", ev)
	}
}

// --- dedup on data.id + action ---

func TestDedupOnIDAndAction(t *testing.T) {
	body1 := []byte(`{"action":"create","type":"Issue","data":{"id":"issue-1","identifier":"ENG-1","title":"x"}}`)
	ev1 := parseWebhook(body1)
	ev1b := parseWebhook(body1)
	if ev1.Dedup != ev1b.Dedup {
		t.Fatalf("same delivery must produce the same dedup key: %q vs %q", ev1.Dedup, ev1b.Dedup)
	}
	body2 := []byte(`{"action":"update","type":"Issue","data":{"id":"issue-1","identifier":"ENG-1","title":"x"}}`)
	ev2 := parseWebhook(body2)
	if ev1.Dedup == ev2.Dedup {
		t.Fatalf("different action on the same id must produce a different dedup key, got %q for both", ev1.Dedup)
	}
}

// --- HMAC signature verification ---

func TestVerifySignatureAcceptsValid(t *testing.T) {
	secret := "shh"
	body := []byte(`{"action":"create"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	if !verifySignature(secret, body, sig) {
		t.Fatal("expected valid signature to be accepted")
	}
}

func TestVerifySignatureRejectsInvalid(t *testing.T) {
	secret := "shh"
	body := []byte(`{"action":"create"}`)
	if verifySignature(secret, body, "deadbeef") {
		t.Fatal("expected bad signature to be rejected")
	}
	if verifySignature(secret, body, "") {
		t.Fatal("expected empty signature to be rejected")
	}
	// A signature computed under a different secret must not verify.
	mac := hmac.New(sha256.New, []byte("wrong-secret"))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	if verifySignature(secret, body, sig) {
		t.Fatal("expected signature under a different secret to be rejected")
	}
	// Tampered body must not verify against the original signature.
	mac2 := hmac.New(sha256.New, []byte(secret))
	mac2.Write(body)
	sigForOriginal := hex.EncodeToString(mac2.Sum(nil))
	if verifySignature(secret, []byte(`{"action":"tampered"}`), sigForOriginal) {
		t.Fatal("expected signature to fail against a tampered body")
	}
}

// --- verb tests: GraphQL over httptest ---

// gqlServer records the last request (method, path, headers, body) and
// replies with the given JSON response body.
func gqlServer(t *testing.T, respBody string, statusCode int) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.authorization = r.Header.Get("Authorization")
		rec.body = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(respBody))
	}))
	return srv, rec
}

type recordedRequest struct {
	method, path, authorization, body string
}

func TestCreateIssueSuccess(t *testing.T) {
	srv, rec := gqlServer(t, `{"data":{"issueCreate":{"success":true,"issue":{"id":"issue-9","identifier":"ENG-9","url":"https://linear.app/x/ENG-9"}}}}`, 200)
	defer srv.Close()

	p := newLinearPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "create_issue",
		Connection: map[string]any{"api_key": "raw-key-123", "api_base": srv.URL},
		Options:    map[string]any{"title": "New bug", "team_id": "team-1", "priority": 2},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["issue_id"] != "issue-9" || res.Outputs["identifier"] != "ENG-9" {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
	if rec.authorization != "raw-key-123" {
		t.Fatalf("Authorization header = %q, want raw key with no Bearer prefix", rec.authorization)
	}
	if !strings.Contains(rec.body, "issueCreate") {
		t.Fatalf("request body missing mutation name issueCreate: %s", rec.body)
	}
	if !strings.Contains(rec.body, "New bug") || !strings.Contains(rec.body, "team-1") {
		t.Fatalf("request body missing variables: %s", rec.body)
	}
}

func TestCreateIssueMissingRequiredFields(t *testing.T) {
	p := newLinearPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "create_issue",
		Connection: map[string]any{"api_key": "k"},
		Options:    map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error for missing title/team_id")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %#v", err)
	}
}

func TestUpdateIssueSuccess(t *testing.T) {
	srv, rec := gqlServer(t, `{"data":{"issueUpdate":{"success":true,"issue":{"id":"issue-1","identifier":"ENG-1","url":"https://linear.app/x/ENG-1"}}}}`, 200)
	defer srv.Close()
	p := newLinearPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "update_issue",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"id": "issue-1", "state_id": "state-2"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["identifier"] != "ENG-1" {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
	if !strings.Contains(rec.body, "issueUpdate") {
		t.Fatalf("request body missing mutation name issueUpdate: %s", rec.body)
	}
}

func TestCommentSuccess(t *testing.T) {
	srv, rec := gqlServer(t, `{"data":{"commentCreate":{"success":true,"comment":{"id":"comment-5","url":"https://linear.app/x/c5"}}}}`, 200)
	defer srv.Close()
	p := newLinearPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "comment",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"issue_id": "issue-1", "body": "hi there"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["id"] != "comment-5" {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
	if !strings.Contains(rec.body, "commentCreate") {
		t.Fatalf("request body missing mutation name commentCreate: %s", rec.body)
	}
}

func TestGetIssueSuccess(t *testing.T) {
	srv, _ := gqlServer(t, `{"data":{"issue":{"id":"issue-1","identifier":"ENG-1","title":"x","priority":1}}}`, 200)
	defer srv.Close()
	p := newLinearPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get_issue",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"id": "issue-1"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["identifier"] != "ENG-1" {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
}

func TestSearchIssuesByQuery(t *testing.T) {
	srv, rec := gqlServer(t, `{"data":{"issueSearch":{"nodes":[{"id":"issue-1","identifier":"ENG-1"}]}}}`, 200)
	defer srv.Close()
	p := newLinearPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "search_issues",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"query": "fix the thing"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
	if !strings.Contains(rec.body, "issueSearch") {
		t.Fatalf("request body missing issueSearch: %s", rec.body)
	}
}

func TestSearchIssuesMissingQueryAndFilter(t *testing.T) {
	p := newLinearPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "search_issues",
		Connection: map[string]any{"api_key": "k"},
		Options:    map[string]any{},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %#v", err)
	}
}

func TestArchiveIssueSuccess(t *testing.T) {
	srv, rec := gqlServer(t, `{"data":{"issueArchive":{"success":true}}}`, 200)
	defer srv.Close()
	p := newLinearPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "archive_issue",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"id": "issue-1"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["ok"] != true {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
	if !strings.Contains(rec.body, "issueArchive") {
		t.Fatalf("request body missing issueArchive: %s", rec.body)
	}
}

func TestRawGraphQLSuccess(t *testing.T) {
	srv, rec := gqlServer(t, `{"data":{"viewer":{"id":"me-1","name":"Ada"}}}`, 200)
	defer srv.Close()
	p := newLinearPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "graphql",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"query": "query { viewer { id name } }"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
	viewer, _ := result["viewer"].(map[string]any)
	if viewer["name"] != "Ada" {
		t.Fatalf("viewer = %#v", viewer)
	}
	_ = rec
}

// --- non-2xx and GraphQL "errors" handling ---

func TestNon2xxIsCodeInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream exploded"))
	}))
	defer srv.Close()
	p := newLinearPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "archive_issue",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"id": "issue-1"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %#v", err)
	}
	if !strings.Contains(pe.Message, "upstream exploded") {
		t.Fatalf("error message should include response body, got %q", pe.Message)
	}
}

func TestGraphQLErrorsFieldIsCodeInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Entity not found: Issue"}]}`))
	}))
	defer srv.Close()
	p := newLinearPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get_issue",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"id": "nope"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %#v", err)
	}
	if !strings.Contains(pe.Message, "Entity not found") {
		t.Fatalf("error message should include the graphql error, got %q", pe.Message)
	}
}

func TestMissingAPIKey(t *testing.T) {
	p := newLinearPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "archive_issue",
		Connection: map[string]any{},
		Options:    map[string]any{"id": "x"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError for missing api_key, got %#v", err)
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

// sanity: ensure our recorded body is valid JSON (guards against a broken test
// helper masking a real failure as a body-content mismatch).
func TestGQLServerHelperRecordsValidJSON(t *testing.T) {
	srv, rec := gqlServer(t, `{"data":{}}`, 200)
	defer srv.Close()
	p := newLinearPlugin()
	_, _ = p.Invoke(plugin.InvokeRequest{
		Verb:       "graphql",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"query": "query { x }"},
	})
	var v map[string]any
	if err := json.Unmarshal([]byte(rec.body), &v); err != nil {
		t.Fatalf("recorded request body is not valid JSON: %v (%s)", err, rec.body)
	}
}
