package ghsource

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sweepStub serves the org installation lookup, installation repo list, open-PR
// list, and per-PR mergeable state needed for an `owner/*` sweep.
func sweepStub(t *testing.T) *appAuth {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/77/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/orgs/acme/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77}`)
	})
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":2,"repositories":[
			{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			{"full_name":"acme/gadget","name":"gadget","owner":{"login":"acme"}}]}`)
	})
	// widget has one authored, conflicting PR; gadget has none.
	mux.HandleFunc("/repos/acme/widget/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"number":3,"user":{"login":"me"}}]`)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/3", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"mergeable_state":"dirty","head":{"sha":"h3"},"base":{"ref":"main"}}`)
	})
	mux.HandleFunc("/repos/acme/gadget/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	return &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
}

func TestSweepReviewRequested(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/77/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/repos/acme/widget/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77}`)
	})
	// Two open PRs by a teammate: #7 requests your review, #8 requests someone else.
	// Neither is authored by you, so no conflict/behind fetch happens.
	mux.HandleFunc("/repos/acme/widget/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[
			{"number":7,"user":{"login":"teammate"},"head":{"sha":"h7","ref":"feat"},"base":{"ref":"main"},
			 "html_url":"u7","requested_reviewers":[{"login":"me"}]},
			{"number":8,"user":{"login":"teammate"},"head":{"sha":"h8"},"base":{"ref":"main"},
			 "requested_reviewers":[{"login":"someoneelse"}]}]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)

	cfg := Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Sweep:   SweepConfig{Enabled: boolp(true), Repos: []string{"acme/widget"}},
		Rules: []Rule{{
			Match:    Match{Repos: []string{"acme/widget"}},
			Reviewer: Actors{Logins: []string{"me"}},
			Actions:  as1(map[string]Action{"review_requested": {}}),
		}},
	}
	g := newTestIntegration(t, cfg)
	g.app = &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
	g.rest = newRESTClient(g.app)

	var got []Trigger
	if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "review_requested" || got[0].Target.Number != 7 {
		t.Fatalf("want one review_requested on #7, got %+v", got)
	}
	// Dedup must match the webhook path so a request already handled live isn't re-fired.
	if got[0].Dedup != "reviewreq@h7" {
		t.Fatalf("dedup mismatch, got %q", got[0].Dedup)
	}
}

func TestSweepUnresolvedComments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/77/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/repos/acme/widget/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77}`)
	})
	mux.HandleFunc("/repos/acme/widget/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"number":9,"user":{"login":"me"},"head":{"sha":"h9","ref":"feat"},"base":{"ref":"main"},"html_url":"u"}]`)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/9", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"mergeable_state":"clean","head":{"sha":"h9"},"base":{"ref":"main"},"html_url":"u"}`)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
			{"id":"t1","isResolved":false,"comments":{"nodes":[{"author":{"login":"me","__typename":"User"}}]}},
			{"id":"t2","isResolved":true,"comments":{"nodes":[{"author":{"login":"carol","__typename":"User"}}]}},
			{"id":"t3","isResolved":false,"comments":{"nodes":[{"author":{"login":"dana","__typename":"User"}}]}}]}}}}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)

	cfg := Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Sweep:   SweepConfig{Enabled: boolp(true), Repos: []string{"acme/widget"}},
		Rules: []Rule{{
			Match:   Match{Repos: []string{"acme/widget"}},
			Me:      Actors{Logins: []string{"me"}},
			Actions: as1(map[string]Action{"changes_requested": {}}),
		}},
	}
	g := newTestIntegration(t, cfg)
	g.app = &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
	g.rest = newRESTClient(g.app)

	var got []Trigger
	if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "changes_requested" || got[0].Target.Number != 9 {
		t.Fatalf("want one changes_requested on #9 for unresolved threads, got %+v", got)
	}
	// Signature carries the head + 2 unresolved threads, so it re-fires on change
	// and stops once resolved.
	if !strings.HasPrefix(got[0].Dedup, "threads:h9:2:") {
		t.Fatalf("unexpected dedup signature: %q", got[0].Dedup)
	}
	// The reviewer to ping back is the opener of the first unresolved thread
	// that isn't you (t1 is self-authored; t2 is resolved) — so a flow's
	// "{{.author}}" resolves on a sweep-recovered event, as on the webhook one.
	if a := got[0].Context["author"]; a != "dana" {
		t.Fatalf("author = %v, want dana (first unresolved non-self thread)", a)
	}
	if b := got[0].Context["author_is_bot"]; b != false {
		t.Fatalf("author_is_bot = %v, want false", b)
	}
	// The PR's head branch rides along, so a worktree dispatch lands on the PR
	// branch rather than a local pr-<n> branch the agent would push as new.
	if hr := got[0].Context["head_ref"]; hr != "feat" {
		t.Fatalf("head_ref = %v, want feat", hr)
	}
}

// Threads left open by a reviewer who has since approved are not outstanding
// feedback: once every unresolved thread's opener approved, the sweep emits no
// changes_requested (it used to, on every head move, re-running the fixer on
// an approved PR). A thread from someone who hasn't approved still counts.
func TestSweepUnresolvedCommentsSkipsApprovers(t *testing.T) {
	for _, tc := range []struct {
		name, reviews string
		want          int
	}{
		{"opener approved", `[{"state":"APPROVED","author":{"login":"dana"}}]`, 0},
		{"opener still requesting changes", `[{"state":"CHANGES_REQUESTED","author":{"login":"dana"}}]`, 1},
		{"someone else approved", `[{"state":"APPROVED","author":{"login":"erin"}}]`, 1},
		{"approval matched case-insensitively", `[{"state":"APPROVED","author":{"login":"Dana"}}]`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/app/installations/77/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
			})
			mux.HandleFunc("/repos/acme/widget/installation", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"id":77}`)
			})
			mux.HandleFunc("/repos/acme/widget/pulls", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `[{"number":9,"user":{"login":"me"},"head":{"sha":"h9","ref":"feat"},"base":{"ref":"main"},"html_url":"u"}]`)
			})
			mux.HandleFunc("/repos/acme/widget/pulls/9", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"mergeable_state":"clean","head":{"sha":"h9"},"base":{"ref":"main"},"html_url":"u"}`)
			})
			mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{
					"latestOpinionatedReviews":{"nodes":%s},
					"reviewThreads":{"nodes":[
					{"id":"t3","isResolved":false,"comments":{"nodes":[{"author":{"login":"dana","__typename":"User"}}]}}]}}}}}`, tc.reviews)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			key, _ := rsa.GenerateKey(rand.Reader, 1024)
			cfg := Config{
				App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
				Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
				Sweep:   SweepConfig{Enabled: boolp(true), Repos: []string{"acme/widget"}},
				Rules: []Rule{{
					Match:   Match{Repos: []string{"acme/widget"}},
					Me:      Actors{Logins: []string{"me"}},
					Actions: as1(map[string]Action{"changes_requested": {}}),
				}},
			}
			g := newTestIntegration(t, cfg)
			g.app = &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
			g.rest = newRESTClient(g.app)
			var got []Trigger
			if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("want %d changes_requested, got %+v", tc.want, got)
			}
		})
	}
}

// The sweep's missed-comment recovery tags each comment with the endpoint it came
// from (issue vs review — separate id sequences, separate high-water marks), skips
// your own comments, and ignores comments older than commentRecoveryWindow.
func TestSweepMissedCommentsKindsAndWindow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/77/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/repos/acme/widget/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77}`)
	})
	mux.HandleFunc("/repos/acme/widget/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"number":9,"user":{"login":"me"},"head":{"sha":"h9","ref":"feat"},"base":{"ref":"main"},"html_url":"u"}]`)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/9", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"mergeable_state":"clean","head":{"sha":"h9"},"base":{"ref":"main"},"html_url":"u"}`)
	})
	fresh := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	stale := time.Now().Add(-commentRecoveryWindow - time.Hour).UTC().Format(time.RFC3339)
	// Conversation comments: a fresh bot report (high id), a fresh self comment, a
	// stale teammate comment.
	mux.HandleFunc("/repos/acme/widget/issues/9/comments", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[
			{"id":5515854542,"user":{"login":"github-actions[bot]"},"body":"report","created_at":%q},
			{"id":5515854000,"user":{"login":"me"},"body":"mine","created_at":%q},
			{"id":5515000000,"user":{"login":"teammate"},"body":"old","created_at":%q}]`, fresh, fresh, stale)
	})
	// Inline review comments: two fresh from a reviewer (low ids).
	mux.HandleFunc("/repos/acme/widget/pulls/9/comments", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[
			{"id":3918412099,"user":{"login":"reviewer"},"body":"nit 2","created_at":%q},
			{"id":3918412084,"user":{"login":"reviewer"},"body":"nit 1","created_at":%q}]`, fresh, fresh)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)

	cfg := Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Sweep:   SweepConfig{Enabled: boolp(true), Repos: []string{"acme/widget"}},
		Rules: []Rule{{
			Match:   Match{Repos: []string{"acme/widget"}},
			Me:      Actors{Logins: []string{"me"}},
			Actions: as1(map[string]Action{"new_comment": {}}),
		}},
	}
	g := newTestIntegration(t, cfg)
	g.app = &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
	g.rest = newRESTClient(g.app)

	var got []Trigger
	if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
		t.Fatal(err)
	}
	kinds := map[int64]string{}
	for _, tr := range got {
		if tr.Kind != "new_comment" {
			t.Fatalf("unexpected trigger kind %q: %+v", tr.Kind, tr)
		}
		id, _ := tr.Context["comment_id"].(int64)
		kinds[id], _ = tr.Context["comment_kind"].(string)
	}
	want := map[int64]string{
		5515854542: CommentKindIssue,
		3918412099: CommentKindReview,
		3918412084: CommentKindReview,
	}
	if len(kinds) != len(want) {
		t.Fatalf("want %d new_comment triggers (self + stale skipped), got %d: %v", len(want), len(kinds), kinds)
	}
	for id, k := range want {
		if kinds[id] != k {
			t.Fatalf("comment %d: want comment_kind=%q, got %q", id, k, kinds[id])
		}
	}
}

func TestSweepOrgGlob(t *testing.T) {
	cfg := Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Sweep:   SweepConfig{Enabled: boolp(true), Repos: []string{"acme/*"}},
		Rules: []Rule{{
			Match:    Match{Repos: []string{"acme/*"}},
			Reviewer: Actors{Logins: []string{"me"}}, // makes "me" a self login
			Actions:  as1(map[string]Action{"merge_conflict": {}}),
		}},
	}
	g := newTestIntegration(t, cfg)
	g.app = sweepStub(t)
	g.rest = newRESTClient(g.app)

	var got []Trigger
	if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "merge_conflict" || got[0].Target.Repo != "acme/widget" {
		t.Fatalf("want one merge_conflict on acme/widget, got %+v", got)
	}
}

// boolp returns a pointer to b — for the *bool sweep.enabled field.
func boolp(b bool) *bool { return &b }

func TestSweepNow(t *testing.T) {
	// Enabled: signals renew, non-blocking and coalescing.
	g := &Source{cfg: Config{Sweep: SweepConfig{Enabled: boolp(true)}}, renew: make(chan struct{}, 1)}
	if !g.SweepNow() {
		t.Fatal("SweepNow should return true when the sweep is enabled")
	}
	select {
	case <-g.renew:
	default:
		t.Fatal("SweepNow should have signaled renew")
	}
	// Coalescing: repeated calls without a drain don't block.
	g.SweepNow()
	g.SweepNow()

	// Disabled: no-op, returns false.
	g2 := &Source{cfg: Config{Sweep: SweepConfig{Enabled: boolp(false)}}, renew: make(chan struct{}, 1)}
	if g2.SweepNow() {
		t.Fatal("SweepNow should return false when the sweep is disabled")
	}
}

func TestStuckReposAndInterval(t *testing.T) {
	// stuck_checks in defaults → applies to all rules → union of their repos; interval
	// read from the action (poll_interval).
	cfg := Config{
		Defaults: Rule{Actions: as1(map[string]Action{
			"stuck_checks": {PollInterval: time.Duration(7 * time.Minute)},
		})},
		Rules: []Rule{
			{Match: Match{Repos: []string{"org/a", "org/b"}}},
			{Match: Match{Repos: []string{"org/b", "org/c"}}}, // b deduped
		},
	}
	g := &Source{cfg: cfg}
	repos := g.stuckRepos()
	if len(repos) != 3 {
		t.Fatalf("want 3 deduped repos, got %v", repos)
	}
	if g.stuckPollInterval() != 7*time.Minute {
		t.Fatalf("poll interval should come from the action, got %s", g.stuckPollInterval())
	}
	if !g.anyStuckChecks() {
		t.Fatal("anyStuckChecks should be true")
	}

	// No stuck_checks anywhere → no repos, default interval, gate off.
	g2 := &Source{cfg: Config{Rules: []Rule{{Match: Match{Repos: []string{"org/a"}}}}}}
	if len(g2.stuckRepos()) != 0 || g2.anyStuckChecks() {
		t.Fatal("no stuck_checks → empty repos + gate off")
	}
	if g2.stuckPollInterval() != 15*time.Minute {
		t.Fatalf("default interval should be 15m, got %s", g2.stuckPollInterval())
	}
}

// TestStuckReposUsesActionScopeUnderConnectorsLowering pins the fix for the
// "stuck */*: users/*/installation 404" bug: the connectors lowering makes every
// rule a "*/*" catch-all and carries each trigger's real repo scope on its
// stuck_checks action. stuckRepos must return the action's repos (org/a, org/b),
// never the rule's "*/*" — otherwise eachRepo tries to resolve an installation
// for a wildcard owner. Reverting stuckRepos to read r.Match.Repos returns
// ["*/*"] and fails this test.
func TestStuckReposUsesActionScopeUnderConnectorsLowering(t *testing.T) {
	cfg := Config{
		// Defaults carry no stuck_checks (connectors puts nothing there).
		Rules: []Rule{{
			Match: Match{Repos: []string{"*/*"}}, // the lowering's catch-all
			Actions: as1(map[string]Action{
				"stuck_checks": {Repos: []string{"org/a", "org/b"}},
			}),
		}},
	}
	g := &Source{cfg: cfg}
	got := g.stuckRepos()
	for _, r := range got {
		if r == "*/*" {
			t.Fatalf("stuckRepos leaked the catch-all wildcard: %v", got)
		}
	}
	if len(got) != 2 || got[0] != "org/a" || got[1] != "org/b" {
		t.Fatalf("want the action's concrete repos [org/a org/b], got %v", got)
	}
}
