package ghsource

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// richConfig enables most kinds for parsing tests.
func richConfig() Config {
	act := func() Action { return Action{} }
	return Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Rules: []Rule{{
			Match:    Match{Repos: []string{"acme/*"}},
			Reviewer: Actors{Logins: []string{"me"}},
			Assignee: Actors{Logins: []string{"me"}},
			Actions: as1(map[string]Action{
				"changes_requested": act(),
				"new_comment":       act(),
				"failing_checks":    act(),
				"merge_conflict":    act(),
				"pr_behind":         {},
				"issue_matched":     {LabelsAny: []string{"Ready"}},
				"review_requested":  {},
				"self_review":       {},
			}),
		}},
	}
}

func do(t *testing.T, g *Source, event, body string) []string {
	t.Helper()
	trs := g.triggersFor(context.Background(), event, []byte(body))
	kinds := make([]string, len(trs))
	for i, tr := range trs {
		kinds[i] = tr.Kind
	}
	return kinds
}

// richWithREST is richConfig plus a stubbed REST/app so kinds that resolve the PR
// author via a fetch (failing_checks) can pass the me-authored gate. The stub reports
// author "me" for every PR.
func richWithREST(t *testing.T) *Source {
	t.Helper()
	_, app := stubAPI(t, "clean")
	g := newTestIntegration(t, richConfig())
	g.app = app
	g.rest = newRESTClient(app)
	return g
}

func TestNewCommentHeadRefEnriched(t *testing.T) {
	g := richWithREST(t) // stub PR author "me", head ref "feature/x"
	// An issue_comment on your own PR by someone else → new_comment, and dispatch
	// needs the PR head branch to adopt an open workspace. issue_comment carries no
	// head ref, so it's looked up via REST and stamped into Context.
	body := `{"action":"created","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":6,"pull_request":{},"user":{"login":"me"}},
		"comment":{"id":12,"user":{"login":"bot"},"body":"please fix"}}`
	trs := g.triggersFor(context.Background(), "issue_comment", []byte(body))
	if len(trs) != 1 || trs[0].Kind != "new_comment" {
		t.Fatalf("want new_comment, got %+v", trs)
	}
	if trs[0].Context["head_ref"] != "feature/x" {
		t.Fatalf("head_ref should be enriched via REST, got %v", trs[0].Context["head_ref"])
	}
}

func TestNewCommentLabelsEnriched(t *testing.T) {
	g := richWithREST(t) // stub PR returns label "conductor:off"
	// A comment event carries no PR labels, so control.pause_label can't catch it
	// unless we enrich. The REST enrichment must stamp the PR's labels into Context
	// so the engine's pause-label check works on comment triggers too.
	body := `{"action":"created","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":6,"pull_request":{},"user":{"login":"me"}},
		"comment":{"id":13,"user":{"login":"bot"},"body":"please fix"}}`
	trs := g.triggersFor(context.Background(), "issue_comment", []byte(body))
	if len(trs) != 1 {
		t.Fatalf("want 1 new_comment, got %d", len(trs))
	}
	lbls, _ := trs[0].Context["labels"].([]string)
	found := false
	for _, l := range lbls {
		if l == "conductor:off" {
			found = true
		}
	}
	if !found {
		t.Fatalf("PR labels should be enriched into Context for pause_label, got %v", trs[0].Context["labels"])
	}
}

func TestCheckRunFailing(t *testing.T) {
	g := richWithREST(t)
	// A check_run's id is the *job* id; run_id must be the workflow run (from
	// details_url) — `gh run rerun` 404s on a job id.
	body := `{"action":"completed","installation":{"id":42},"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"check_run":{"conclusion":"failure","name":"build","head_sha":"h9","id":321,
		"details_url":"https://github.com/acme/w/actions/runs/777/job/321","pull_requests":[{"number":4}]}}`
	trs := g.triggersFor(context.Background(), "check_run", []byte(body))
	if len(trs) != 1 || trs[0].Kind != "failing_checks" {
		t.Fatalf("want failing_checks, got %+v", trs)
	}
	if trs[0].Context["run_id"] != int64(777) || trs[0].Dedup != "fail@h9" {
		t.Fatalf("run_id/dedup wrong: %+v", trs[0].Context)
	}
	// A successful check produces nothing.
	ok := `{"action":"completed","installation":{"id":42},"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"check_run":{"conclusion":"success","head_sha":"h","pull_requests":[{"number":4}]}}`
	if k := do(t, g, "check_run", ok); len(k) != 0 {
		t.Fatalf("success check should not trigger, got %v", k)
	}
}

func TestCheckRunRunIDFallbacks(t *testing.T) {
	g := richWithREST(t)
	runID := func(body string) any {
		trs := g.triggersFor(context.Background(), "check_run", []byte(body))
		if len(trs) != 1 || trs[0].Kind != "failing_checks" {
			t.Fatalf("want failing_checks, got %+v", trs)
		}
		return trs[0].Context["run_id"]
	}
	// No details_url → the jobs API resolves the run (job 321 → run 555).
	noURL := `{"action":"completed","installation":{"id":42},"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"check_run":{"conclusion":"failure","name":"build","head_sha":"h9","id":321,"pull_requests":[{"number":4}]}}`
	if got := runID(noURL); got != int64(555) {
		t.Fatalf("run_id via jobs API = %v, want 555", got)
	}
	// A non-Actions check (external details_url, unknown job) has no run to rerun.
	external := `{"action":"completed","installation":{"id":42},"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"check_run":{"conclusion":"failure","name":"Bugbot","head_sha":"h9","id":999,
		"details_url":"https://cursor.com/docs/bugbot","pull_requests":[{"number":4}]}}`
	if got := runID(external); got != int64(0) {
		t.Fatalf("run_id for non-Actions check = %v, want 0", got)
	}
}

func TestCheckSuiteFailing(t *testing.T) {
	srv, app := stubAPI(t, "clean")
	g := newTestIntegration(t, richConfig())
	g.app = app
	g.rest = newRESTClient(app)
	// A check_suite id is the suite id; the run is looked up by check_suite_id (77 → 888).
	mux := srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/repos/acme/w/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("check_suite_id") != "77" {
			t.Errorf("runs lookup should filter by check_suite_id=77, got %q", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"workflow_runs":[{"id":888}]}`)
	})
	body := `{"action":"completed","installation":{"id":42},"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"check_suite":{"conclusion":"failure","head_sha":"h9","id":77,"pull_requests":[{"number":4}]}}`
	trs := g.triggersFor(context.Background(), "check_suite", []byte(body))
	if len(trs) != 1 || trs[0].Kind != "failing_checks" {
		t.Fatalf("want failing_checks, got %+v", trs)
	}
	if trs[0].Context["run_id"] != int64(888) {
		t.Fatalf("run_id via runs API = %v, want 888", trs[0].Context["run_id"])
	}
}

func TestRunIDFromDetailsURL(t *testing.T) {
	cases := map[string]int64{
		"https://github.com/acme/w/actions/runs/34249290897/job/102139431976": 34249290897,
		"https://ghe.example.com/o/r/actions/runs/12?pr=5":                    12,
		"https://cursor.com/docs/bugbot":                                      0,
		"":                                                                    0,
	}
	for u, want := range cases {
		if got := runIDFromDetailsURL(u); got != want {
			t.Errorf("runIDFromDetailsURL(%q) = %d, want %d", u, got, want)
		}
	}
}

func TestWorkflowRunFailing(t *testing.T) {
	g := richWithREST(t)
	body := `{"action":"completed","installation":{"id":42},"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"workflow_run":{"conclusion":"timed_out","head_sha":"hh","id":9,"pull_requests":[{"number":2}]}}`
	if k := do(t, g, "workflow_run", body); len(k) != 1 || k[0] != "failing_checks" {
		t.Fatalf("want failing_checks, got %v", k)
	}
}

func TestReviewCommentTriggers(t *testing.T) {
	g := newTestIntegration(t, richConfig())
	body := `{"action":"created","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"},"base":{"ref":"main"},"user":{"login":"me"}},
		"comment":{"id":7,"user":{"login":"bot"},"body":"nit"}}`
	if k := do(t, g, "pull_request_review_comment", body); len(k) != 1 || k[0] != "new_comment" {
		t.Fatalf("want new_comment, got %v", k)
	}
}

func TestReviewRequestedMatch(t *testing.T) {
	g := newTestIntegration(t, richConfig())
	yes := `{"action":"review_requested","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"}},"requested_reviewer":{"login":"me"}}`
	if k := do(t, g, "pull_request", yes); len(k) != 1 || k[0] != "review_requested" {
		t.Fatalf("want review_requested, got %v", k)
	}
	no := `{"action":"review_requested","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"}},"requested_reviewer":{"login":"someone-else"}}`
	if k := do(t, g, "pull_request", no); len(k) != 0 {
		t.Fatalf("reviewer mismatch should not trigger, got %v", k)
	}
}

func gatedReviewConfig() Config {
	c := richConfig()
	rr := c.Rules[0].Actions["review_requested"][0]
	rr.Gates = map[string]any{"not_draft": true}
	c.Rules[0].Actions["review_requested"] = ActionSet{rr}
	return c
}

func excludeReviewConfig() Config {
	c := richConfig()
	rr := c.Rules[0].Actions["review_requested"][0]
	rr.Exclude = Exclude{Branches: []string{"release/*"}, Labels: []string{"release"}, Title: []string{"[skip review]"}}
	c.Rules[0].Actions["review_requested"] = ActionSet{rr}
	return c
}

func TestReviewRequestedExcludesReleasePRs(t *testing.T) {
	g := newTestIntegration(t, excludeReviewConfig())
	base := `{"action":"review_requested","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"requested_reviewer":{"login":"me"},"pull_request":%s}`

	// A normal PR still fires.
	normal := `{"number":6,"head":{"sha":"h","ref":"feat/x"},"title":"add feature"}`
	if k := do(t, g, "pull_request", fmt.Sprintf(base, normal)); len(k) != 1 || k[0] != "review_requested" {
		t.Fatalf("normal PR should fire, got %v", k)
	}
	// Release branch is excluded.
	rel := `{"number":7,"head":{"sha":"h","ref":"release/1.2.0"},"title":"Release 1.2.0"}`
	if k := do(t, g, "pull_request", fmt.Sprintf(base, rel)); len(k) != 0 {
		t.Fatalf("release-branch PR must be excluded, got %v", k)
	}
	// Release label is excluded.
	lbl := `{"number":8,"head":{"sha":"h","ref":"main"},"title":"cut","labels":[{"name":"release"}]}`
	if k := do(t, g, "pull_request", fmt.Sprintf(base, lbl)); len(k) != 0 {
		t.Fatalf("release-labeled PR must be excluded, got %v", k)
	}
	// Title marker is excluded.
	ttl := `{"number":9,"head":{"sha":"h","ref":"chore"},"title":"chore: bump [skip review]"}`
	if k := do(t, g, "pull_request", fmt.Sprintf(base, ttl)); len(k) != 0 {
		t.Fatalf("title-marked PR must be excluded, got %v", k)
	}
}

func TestExcludeMatches(t *testing.T) {
	e := Exclude{Branches: []string{"release/*"}, Labels: []string{"Release"}, Title: []string{"skip"}}
	if !e.Matches("release/1.0", "x", nil) {
		t.Error("branch glob should match")
	}
	if !e.Matches("feat", "x", []string{"release"}) {
		t.Error("label should match case-insensitively")
	}
	if !e.Matches("feat", "please SKIP this", nil) {
		t.Error("title substring should match case-insensitively")
	}
	if e.Matches("feat/x", "normal", []string{"bug"}) {
		t.Error("non-matching PR should not be excluded")
	}
	if (Exclude{}).Matches("release/1.0", "release", []string{"release"}) {
		t.Error("empty exclude must never match")
	}
}

func TestReviewRequestedDraftGate(t *testing.T) {
	draft := `{"action":"review_requested","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"},"draft":true},"requested_reviewer":{"login":"me"}}`
	ready := `{"action":"review_requested","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"},"draft":false},"requested_reviewer":{"login":"me"}}`

	// Opt-in default: no gate → drafts still fire.
	if k := do(t, newTestIntegration(t, richConfig()), "pull_request", draft); len(k) != 1 || k[0] != "review_requested" {
		t.Fatalf("without gate, draft should fire, got %v", k)
	}
	// not_draft gate on → drafts skipped, ready still fires.
	g := newTestIntegration(t, gatedReviewConfig())
	if k := do(t, g, "pull_request", draft); len(k) != 0 {
		t.Fatalf("not_draft gate should skip draft, got %v", k)
	}
	if k := do(t, g, "pull_request", ready); len(k) != 1 || k[0] != "review_requested" {
		t.Fatalf("non-draft should fire even with gate, got %v", k)
	}
}

func TestReadyForReviewFiresPendingReview(t *testing.T) {
	// A review requested while draft (skipped by the gate) fires when marked ready.
	g := newTestIntegration(t, gatedReviewConfig())
	body := `{"action":"ready_for_review","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"},"draft":false,"user":{"login":"teammate"},
		"requested_reviewers":[{"login":"me"}]}}`
	found := false
	for _, k := range do(t, g, "pull_request", body) {
		if k == "review_requested" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ready_for_review should fire review_requested for a pending reviewer")
	}
}

func TestGateEnabledOptIn(t *testing.T) {
	if gateEnabled(nil, "not_draft") {
		t.Fatal("absent gate must be off (opt-in)")
	}
	if !gateEnabled(map[string]any{"not_draft": true}, "not_draft") {
		t.Fatal("true gate must be on")
	}
	if gateEnabled(map[string]any{"not_draft": false}, "not_draft") {
		t.Fatal("false gate must be off")
	}
	if !gateEnabled(map[string]any{"not_draft": "yes"}, "not_draft") {
		t.Fatal("string yes must be on")
	}
	// draftGate only blocks when the gate is set AND the PR is a draft.
	gated := gatedReviewConfig().Rules[0].Actions["review_requested"][0]
	if !draftGate(gated, true) {
		t.Fatal("gated + draft should block")
	}
	if draftGate(gated, false) {
		t.Fatal("non-draft should not block")
	}
	ungated := richConfig().Rules[0].Actions["review_requested"][0]
	if draftGate(ungated, true) {
		t.Fatal("no gate should not block even a draft")
	}
}

func TestPullRequestClosedEmitsKindClosed(t *testing.T) {
	g := newTestIntegration(t, richConfig())
	body := `{"action":"closed","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"}}}`
	trs := g.triggersFor(context.Background(), "pull_request", []byte(body))
	if len(trs) != 1 || trs[0].Kind == "" || trs[0].Kind != "_closed" {
		t.Fatalf("want _closed, got %+v", trs)
	}
}

func TestIssueMatchedStateBased(t *testing.T) {
	g := newTestIntegration(t, richConfig()) // issue_matched: labels_any [Ready]
	// Current state has the Ready label + assigned to me → matches.
	ready := `{"action":"labeled","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":10,"assignees":[{"login":"me"}],"labels":[{"name":"Ready"}]},"label":{"name":"Ready"}}`
	if k := do(t, g, "issues", ready); len(k) != 1 || k[0] != "issue_matched" {
		t.Fatalf("want issue_matched, got %v", k)
	}
	// No matching label in the current set → no trigger.
	other := `{"action":"labeled","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":10,"assignees":[{"login":"me"}],"labels":[{"name":"wontfix"}]},"label":{"name":"wontfix"}}`
	if k := do(t, g, "issues", other); len(k) != 0 {
		t.Fatalf("no Ready label should not trigger, got %v", k)
	}
	// Assigned to someone else → no trigger (me-assignee gate).
	notMine := `{"action":"labeled","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":10,"assignees":[{"login":"teammate"}],"labels":[{"name":"Ready"}]},"label":{"name":"Ready"}}`
	if k := do(t, g, "issues", notMine); len(k) != 0 {
		t.Fatalf("Ready label on a teammate's issue should not trigger, got %v", k)
	}
	// State-based: the Ready label was already there, and an ASSIGNED event (not a
	// label event) flips it into matching — the old label-event trigger missed this.
	nowAssigned := `{"action":"assigned","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":10,"assignees":[{"login":"me"}],"labels":[{"name":"Ready"}]},"assignee":{"login":"me"}}`
	if k := do(t, g, "issues", nowAssigned); !has(k, "issue_matched") {
		t.Fatalf("assigning a Ready issue to me should now match, got %v", k)
	}
}

func TestNewCommentFromUsersFilter(t *testing.T) {
	cfg := richConfig()
	cfg.Rules[0].Actions["new_comment"] = ActionSet{{FromUsers: []string{"coderabbitai[bot]"}}}
	g := newTestIntegration(t, cfg)

	match := `{"action":"created","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"me"}},"comment":{"id":1,"user":{"login":"coderabbitai[bot]"},"body":"x"}}`
	if k := do(t, g, "issue_comment", match); len(k) != 1 {
		t.Fatalf("allowed bot should trigger, got %v", k)
	}
	skip := `{"action":"created","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"me"}},"comment":{"id":2,"user":{"login":"randomuser"},"body":"x"}}`
	if k := do(t, g, "issue_comment", skip); len(k) != 0 {
		t.Fatalf("non-listed user should be filtered, got %v", k)
	}
}

func TestDisabledActionNoTrigger(t *testing.T) {
	cfg := richConfig()
	dis := false
	a := cfg.Rules[0].Actions["changes_requested"][0]
	a.Enabled = &dis
	cfg.Rules[0].Actions["changes_requested"] = ActionSet{a}
	g := newTestIntegration(t, cfg)
	body := `{"action":"submitted","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"}},"review":{"state":"changes_requested","id":1,"user":{"login":"r"}}}`
	if k := do(t, g, "pull_request_review", body); len(k) != 0 {
		t.Fatalf("disabled action should not trigger, got %v", k)
	}
}

func TestReadyForReviewRESTFallback(t *testing.T) {
	g := richWithREST(t) // stub /pulls/{n}/requested_reviewers returns "me"
	// A draft→ready transition whose payload omits requested_reviewers must still
	// emit review_requested — conductor REST-fetches the pending reviewers.
	ready := `{"action":"ready_for_review","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"draft":false,"head":{"sha":"h6","ref":"feature/x"},
		"base":{"ref":"main"},"user":{"login":"them"},"requested_reviewers":[],"html_url":"http://x/6"}}`
	kinds := do(t, g, "pull_request", ready)
	found := false
	for _, k := range kinds {
		if k == "review_requested" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ready_for_review should emit review_requested via the REST fallback, got %v", kinds)
	}
}

// A failing check whose name is in the action's ignore_checks must NOT spin up a
// fixer (e.g. a PR-title convention gate the user doesn't follow); other failing
// checks still do.
func TestFailingCheckIgnoredByName(t *testing.T) {
	cfg := richConfig()
	cfg.Rules[0].Actions = as1(map[string]Action{
		"failing_checks": {IgnoreChecks: []string{"title-validation"}},
	})
	_, app := stubAPI(t, "clean")
	g := newTestIntegration(t, cfg)
	g.app, g.rest = app, newRESTClient(app)

	ignored := `{"action":"completed","installation":{"id":42},"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"check_run":{"conclusion":"failure","name":"title-validation","head_sha":"h1","id":1,"pull_requests":[{"number":4}]}}`
	if k := do(t, g, "check_run", ignored); len(k) != 0 {
		t.Fatalf("ignored check must not trigger failing_checks, got %v", k)
	}
	real := `{"action":"completed","installation":{"id":42},"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"check_run":{"conclusion":"failure","name":"tests","head_sha":"h2","id":2,"pull_requests":[{"number":4}]}}`
	if k := do(t, g, "check_run", real); len(k) != 1 || k[0] != "failing_checks" {
		t.Fatalf("a non-ignored failing check should still trigger failing_checks, got %v", k)
	}
}

// A failing-check fixer pushes to the PR branch, but a check payload carries no
// head branch: it must be enriched from the PR like comment/review triggers,
// or the worktree lands on pr-<n> and the fix is pushed as a stray new branch.
func TestFailingChecksHeadRefEnriched(t *testing.T) {
	g := richWithREST(t) // stub PR head ref "feature/x"
	body := `{"action":"completed","installation":{"id":42},"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"check_run":{"conclusion":"failure","name":"build","head_sha":"h9","id":321,
		"details_url":"https://github.com/acme/w/actions/runs/777/job/321","pull_requests":[{"number":4}]}}`
	trs := g.triggersFor(context.Background(), "check_run", []byte(body))
	if len(trs) != 1 || trs[0].Kind != "failing_checks" {
		t.Fatalf("want failing_checks, got %+v", trs)
	}
	if trs[0].Context["head_ref"] != "feature/x" {
		t.Fatalf("head_ref should be enriched via REST, got %v", trs[0].Context["head_ref"])
	}
}

// A check finishing after the PR merged has no branch left to fix.
func TestCheckRunOnClosedPRDropped(t *testing.T) {
	_, app := stubAPI(t, "clean")
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"inst-tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/repos/acme/w/pulls/{num}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"state":"closed","mergeable_state":"clean","head":{"sha":"h6","ref":"feature/x"},"base":{"ref":"main"},"user":{"login":"me"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	app.apiBase = srv.URL
	g := newTestIntegration(t, richConfig())
	g.app = app
	g.rest = newRESTClient(app)
	body := `{"action":"completed","installation":{"id":42},"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"check_run":{"conclusion":"failure","name":"build","head_sha":"h9","id":321,
		"details_url":"https://github.com/acme/w/actions/runs/777/job/321","pull_requests":[{"number":4}]}}`
	for _, k := range do(t, g, "check_run", body) {
		if k == "failing_checks" {
			t.Fatalf("failing check on a closed PR must not trigger a fixer")
		}
	}
}

// Merge-state detection re-reads the PR; if it has closed by then (the event
// raced the merge), no conflict/behind fixer is emitted for the dead branch.
func TestMergeStateOnClosedPRDropped(t *testing.T) {
	_, app := stubAPI(t, "dirty")
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"inst-tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/repos/acme/w/pulls/{num}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"state":"closed","mergeable_state":"dirty","head":{"sha":"h6","ref":"feature/x"},"base":{"ref":"main"},"user":{"login":"me"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	app.apiBase = srv.URL
	g := newTestIntegration(t, richConfig())
	g.app = app
	g.rest = newRESTClient(app)
	body := `{"action":"synchronize","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"state":"open","head":{"sha":"h6"},"base":{"ref":"main"}}}`
	for _, k := range do(t, g, "pull_request", body) {
		if k == "merge_conflict" || k == "pr_behind" {
			t.Fatalf("merge-state fixer emitted for a closed PR: %s", k)
		}
	}
}
