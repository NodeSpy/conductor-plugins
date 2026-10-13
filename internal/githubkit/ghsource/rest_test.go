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

// stubAPI serves the installation-token and pull endpoints against a settable
// mergeable_state, so merge-state parsing can be tested without the network.
func stubAPI(t *testing.T, mergeableState string) (*httptest.Server, *appAuth) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"inst-tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	// Any PR number resolves; author is "me" (the self identity in test configs) so the
	// me-authored gate on autopilot kinds passes.
	mux.HandleFunc("/repos/acme/w/pulls/{num}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"mergeable_state":%q,"head":{"sha":"h6","ref":"feature/x"},"base":{"ref":"main"},"html_url":"http://x/6","user":{"login":"me"},"labels":[{"name":"conductor:off"}]}`, mergeableState)
	})
	// Pending requested reviewers (for ready_for_review REST fallback): reports "me".
	mux.HandleFunc("/repos/acme/w/pulls/{num}/requested_reviewers", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"users":[{"login":"me"}],"teams":[]}`)
	})
	// Actions job 321 belongs to run 555; any other job id is not an Actions job.
	mux.HandleFunc("/repos/acme/w/actions/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "321" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"id":321,"run_id":555}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	app := &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
	return srv, app
}

func TestMergeStateConflict(t *testing.T) {
	_, app := stubAPI(t, "dirty")
	g := newTestIntegration(t, richConfig())
	g.app = app
	g.rest = newRESTClient(app)

	body := `{"action":"opened","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h6"},"base":{"ref":"main"}}}`
	trs := g.triggersFor(context.Background(), "pull_request", []byte(body))
	if len(trs) != 1 || trs[0].Kind != "merge_conflict" {
		t.Fatalf("want merge_conflict, got %+v", trs)
	}
	// App token was injected via the stubbed token endpoint.
	if trs[0].Context["app_token"] != "inst-tok" {
		t.Fatalf("app token not injected: %+v", trs[0].Context)
	}
}

func TestMergeStateBehind(t *testing.T) {
	_, app := stubAPI(t, "behind")
	g := newTestIntegration(t, richConfig())
	g.app = app
	g.rest = newRESTClient(app)

	body := `{"action":"synchronize","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h6"},"base":{"ref":"main"}}}`
	trs := g.triggersFor(context.Background(), "pull_request", []byte(body))
	if len(trs) != 1 || trs[0].Kind != "pr_behind" {
		t.Fatalf("want pr_behind, got %+v", trs)
	}
}

// TestMergeStateNotOwnPR: a dirty PR authored by someone else must not fire
// merge_conflict — autopilot pushes fixes, so it only acts on your authored PRs.
func TestMergeStateNotOwnPR(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"inst-tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/repos/acme/w/pulls/{num}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"mergeable_state":"dirty","head":{"sha":"h6"},"base":{"ref":"main"},"html_url":"http://x/6","user":{"login":"teammate"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	app := &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}

	g := newTestIntegration(t, richConfig())
	g.app = app
	g.rest = newRESTClient(app)

	body := `{"action":"opened","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h6"},"base":{"ref":"main"}}}`
	if trs := g.triggersFor(context.Background(), "pull_request", []byte(body)); len(trs) != 0 {
		t.Fatalf("merge_conflict must not fire on a teammate's PR, got %+v", trs)
	}
}

func TestMergeStateCleanNoTrigger(t *testing.T) {
	_, app := stubAPI(t, "clean")
	g := newTestIntegration(t, richConfig())
	g.app = app
	g.rest = newRESTClient(app)

	body := `{"action":"opened","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h6"},"base":{"ref":"main"}}}`
	if trs := g.triggersFor(context.Background(), "pull_request", []byte(body)); len(trs) != 0 {
		t.Fatalf("clean PR should not trigger, got %+v", trs)
	}
}

func TestStuckRuns(t *testing.T) {
	srv, app := stubAPI(t, "clean")
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	old := now.Add(-45 * time.Minute).Format(time.RFC3339)  // stuck
	fresh := now.Add(-2 * time.Minute).Format(time.RFC3339) // still running normally
	mux := srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/repos/acme/w/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"workflow_runs":[
			{"id":1,"name":"tests","status":"in_progress","created_at":%q},
			{"id":2,"name":"lint","status":"in_progress","created_at":%q},
			{"id":3,"name":"done","status":"completed","created_at":%q}
		]}`, old, fresh, old)
	})
	c := newRESTClient(app)
	runs, err := c.stuckRuns(context.Background(), 42, "acme", "w", "h6", 30*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != 1 {
		t.Fatalf("only the old in_progress run should be stuck, got %+v", runs)
	}
}
