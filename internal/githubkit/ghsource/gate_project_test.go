package ghsource

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMergeGatePasses(t *testing.T) {
	green := &mergeGate{MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
		IsDraft: false, ThreadsResolved: true, NonAuthorApprove: true}
	if !mergeGatePasses(green, nil) {
		t.Fatal("fully green gate should pass")
	}
	// Each failing condition blocks by default.
	for _, mut := range []func(*mergeGate){
		func(g *mergeGate) { g.MergeStateStatus = "BLOCKED" },
		func(g *mergeGate) { g.ReviewDecision = "REVIEW_REQUIRED" },
		func(g *mergeGate) { g.IsDraft = true },
		func(g *mergeGate) { g.ThreadsResolved = false },
		func(g *mergeGate) { g.NonAuthorApprove = false },
	} {
		g := *green
		mut(&g)
		if mergeGatePasses(&g, nil) {
			t.Fatalf("gate should have failed: %+v", g)
		}
	}
	// Relaxing a gate via `false` lets it pass.
	g := *green
	g.ThreadsResolved = false
	if !mergeGatePasses(&g, map[string]any{"threads_resolved": false}) {
		t.Fatal("threads_resolved:false should relax the thread check")
	}
}

// graphqlStub serves the App token endpoint plus a canned /graphql response.
func graphqlStub(t *testing.T, graphqlResp string) *appAuth {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, graphqlResp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	return &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
}

func mergeReadyConfig() Config {
	return Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Rules: []Rule{{
			Match: Match{Repos: []string{"acme/*"}},
			Me:    Actors{Logins: []string{"me"}}, // auto-merge only acts on your authored PRs
			Actions: as1(map[string]Action{
				"merge_ready": {RequireLabel: "automerge"},
			}),
		}},
	}
}

func TestMergeReadyGreen(t *testing.T) {
	resp := `{"data":{"repository":{"pullRequest":{
		"headRefOid":"h1","mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","isDraft":false,
		"author":{"login":"me"},
		"labels":{"nodes":[{"name":"automerge"}]},
		"reviewThreads":{"nodes":[{"isResolved":true}]},
		"approvals":{"nodes":[{"author":{"login":"reviewer"}}]}
	}}}}`
	g := newTestIntegration(t, mergeReadyConfig())
	g.app = graphqlStub(t, resp)
	g.rest = newRESTClient(g.app)

	body := `{"action":"synchronize","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h1"},"base":{"ref":"main"}}}`
	trs := g.triggersFor(context.Background(), "pull_request", []byte(body))
	found := false
	for _, tr := range trs {
		if tr.Kind == "merge_ready" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected merge_ready, got %+v", trs)
	}
}

func TestMergeReadyBlockedByLabel(t *testing.T) {
	// Green gate but the required label is missing → no merge_ready.
	resp := `{"data":{"repository":{"pullRequest":{
		"headRefOid":"h1","mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","isDraft":false,
		"author":{"login":"me"},"labels":{"nodes":[]},
		"reviewThreads":{"nodes":[]},"approvals":{"nodes":[{"author":{"login":"r"}}]}
	}}}}`
	g := newTestIntegration(t, mergeReadyConfig())
	g.app = graphqlStub(t, resp)
	g.rest = newRESTClient(g.app)
	body := `{"action":"synchronize","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h1"}}}`
	for _, tr := range g.triggersFor(context.Background(), "pull_request", []byte(body)) {
		if tr.Kind == "merge_ready" {
			t.Fatal("missing required label should block merge_ready")
		}
	}
}

func projectConfig() Config {
	return Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Rules: []Rule{{
			Match: Match{Repos: []string{"acme/*"}},
			Me:    Actors{Logins: []string{"me"}}, // only start work on issues assigned to you
			Actions: as1(map[string]Action{
				"issue_matched": {
					Gates: map[string]any{"project": map[string]any{"Status": "Ready"}}},
			}),
		}},
	}
}

// projectResp builds a combined GraphQL response that satisfies both the
// projectItem lookup (data.node.content → repo+number) and the issueEnrich
// fetch (data.repository.issue → facts + project field values) that a
// projects_v2_item → issue_matched flow issues back-to-back.
func projectResp(assignee, status string) string {
	return `{"data":{
		"node":{"content":{"number":42,"repository":{"nameWithOwner":"acme/w"}}},
		"repository":{"issue":{
			"title":"Do it","author":{"login":"boss"},
			"labels":{"nodes":[]},
			"assignees":{"nodes":[{"login":"` + assignee + `"}]},
			"linkedBranches":{"totalCount":0},
			"closedByPullRequestsReferences":{"totalCount":0},
			"projectItems":{"nodes":[{"fieldValues":{"nodes":[
				{"name":"` + status + `","field":{"name":"Status"}}
			]}}]}
		}}
	}}`
}

func TestIssueMatchedOnProjectMove(t *testing.T) {
	g := newTestIntegration(t, projectConfig())
	g.app = graphqlStub(t, projectResp("me", "Ready"))
	g.rest = newRESTClient(g.app)

	body := `{"action":"edited","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"projects_v2_item":{"node_id":"PVTI_x","content_type":"Issue"}}`
	trs := g.triggersFor(context.Background(), "projects_v2_item", []byte(body))
	if len(trs) != 1 || trs[0].Kind != "issue_matched" || trs[0].Target.Issue != 42 {
		t.Fatalf("expected issue_matched for #42, got %+v", trs)
	}
}

func TestIssueMatchedProjectNotMine(t *testing.T) {
	// Moved to Ready but assigned to someone else → no trigger (me-assignee gate).
	g := newTestIntegration(t, projectConfig())
	g.app = graphqlStub(t, projectResp("teammate", "Ready"))
	g.rest = newRESTClient(g.app)
	body := `{"action":"edited","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"projects_v2_item":{"node_id":"PVTI_x","content_type":"Issue"}}`
	if trs := g.triggersFor(context.Background(), "projects_v2_item", []byte(body)); len(trs) != 0 {
		t.Fatalf("Ready on a teammate's issue should not trigger, got %+v", trs)
	}
}

func TestIssueMatchedProjectWrongStatus(t *testing.T) {
	g := newTestIntegration(t, projectConfig())
	g.app = graphqlStub(t, projectResp("me", "Backlog"))
	g.rest = newRESTClient(g.app)
	body := `{"action":"edited","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"projects_v2_item":{"node_id":"PVTI_x","content_type":"Issue"}}`
	if trs := g.triggersFor(context.Background(), "projects_v2_item", []byte(body)); len(trs) != 0 {
		t.Fatalf("Backlog should not trigger, got %+v", trs)
	}
}

func TestSelfReviewOwnPR(t *testing.T) {
	g := newTestIntegration(t, richConfig()) // reviewer/assignee logins include "me"
	body := `{"action":"opened","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"},"user":{"login":"me"}}}`
	trs := g.triggersFor(context.Background(), "pull_request", []byte(body))
	found := false
	for _, tr := range trs {
		if tr.Kind == "self_review" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected self_review on own PR, got %+v", trs)
	}
	// Someone else's PR → no self_review.
	other := `{"action":"opened","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":7,"head":{"sha":"h"},"user":{"login":"stranger"}}}`
	for _, tr := range g.triggersFor(context.Background(), "pull_request", []byte(other)) {
		if tr.Kind == "self_review" {
			t.Fatal("self_review should only fire on your own PRs")
		}
	}
}
