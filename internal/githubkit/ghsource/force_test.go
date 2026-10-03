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

func TestForceUnconfiguredKind(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	// A kind with no configured action errors before any network call.
	n, err := g.Force(context.Background(), "nonexistent_kind", "acme/widget", 1,
		func(context.Context, Trigger) {})
	if err == nil || n != 0 {
		t.Fatalf("force should error for an unconfigured kind, got n=%d err=%v", n, err)
	}
}

// TestForceBypassesDraftAndExclude (#57 T3): `conductor force` runs an action on
// demand, skipping the applicability filters (draft gate, exclude) AND the
// engine's dedup/liveness gates (Trigger.Force). It fetches the PR to fill the
// target, resolves the App installation, and injects the installation id + a
// freshly minted app token into each trigger's Context — so the forced run has
// the same credentials a webhook-driven one would.
func TestForceBypassesDraftAndExclude(t *testing.T) {
	// review_requested gated by not_draft and excluding the "wip" label.
	cfg := richConfig()
	cfg.Rules[0].Actions = as1(map[string]Action{
		"review_requested": {

			Gates:   map[string]any{"not_draft": true},
			Exclude: Exclude{Labels: []string{"wip"}},
		},
	})

	// Stub the App + REST endpoints: installation lookup, the PR (a draft with
	// the excluded label, authored by "me"), and the installation token.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/w/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":42}`)
	})
	mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"inst-tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/repos/acme/w/pulls/6", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"draft":true,"head":{"sha":"h6","ref":"feature/x"},"base":{"ref":"main"},"html_url":"http://x/6","user":{"login":"me"},"labels":[{"name":"wip"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	app := &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}

	g := newTestIntegration(t, cfg)
	g.app = app
	g.rest = newRESTClient(app)

	// Baseline: the normal webhook path filters this PR out — the requested
	// reviewer matches, but the not_draft gate + label exclude suppress it.
	body := `{"action":"review_requested","installation":{"id":42},
		"requested_reviewer":{"login":"me"},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"draft":true,"head":{"sha":"h6","ref":"feature/x"},"base":{"ref":"main"},"user":{"login":"me"},"labels":[{"name":"wip"}]}}`
	if trs := g.triggersFor(context.Background(), "pull_request", []byte(body)); len(trs) != 0 {
		t.Fatalf("draft + excluded label must filter the normal path, got %+v", trs)
	}

	// Force fires it regardless, marks the trigger Force, and injects the
	// installation id + minted app token.
	var got []Trigger
	n, err := g.Force(context.Background(), "review_requested", "acme/w", 6,
		func(_ context.Context, tr Trigger) { got = append(got, tr) })
	if err != nil || n != 1 || len(got) != 1 {
		t.Fatalf("force should fire once past the filters, got n=%d err=%v trs=%d", n, err, len(got))
	}
	tr := got[0]
	if tr.Kind != "review_requested" || !tr.Force {
		t.Fatalf("forced trigger must carry the kind and Force flag: %+v", tr)
	}
	if tr.Target.PR != 6 || tr.Target.HeadSHA != "h6" {
		t.Fatalf("forced trigger target should be filled from the fetched PR: %+v", tr.Target)
	}
	if tr.Context["installation_id"] != int64(42) {
		t.Fatalf("installation id must be injected into context, got %v", tr.Context["installation_id"])
	}
	if tr.Context["app_token"] != "inst-tok" {
		t.Fatalf("app token must be injected into context, got %v", tr.Context["app_token"])
	}
}

// A forced changes_requested must not hand the flow the PR author as
// `author`: that's the login "{{.author}}" re-requests, and GitHub 422s a
// review request to the PR author. It resolves the reviewer from the
// unresolved threads (as the sweep does), or carries none.
func TestForceChangesRequestedAuthorIsReviewer(t *testing.T) {
	for _, tc := range []struct {
		name, threads string
		want          any
	}{
		{"thread reviewer", `[
			{"id":"t1","isResolved":false,"comments":{"nodes":[{"author":{"login":"me","__typename":"User"}}]}},
			{"id":"t2","isResolved":false,"comments":{"nodes":[{"author":{"login":"dana","__typename":"User"}}]}}]`, "dana"},
		// A review bot can't be re-requested (GitHub 422s it as "not a
		// collaborator"): a human reviewer wins even behind an older bot thread.
		{"human over bot", `[
			{"id":"t1","isResolved":false,"comments":{"nodes":[{"author":{"login":"cursor","__typename":"Bot"}}]}},
			{"id":"t2","isResolved":false,"comments":{"nodes":[{"author":{"login":"dana","__typename":"User"}}]}}]`, "dana"},
		// Bot-only: still named (author_is_bot tells the flow), in the REST /
		// webhook "[bot]" form rather than GraphQL's bare slug.
		{"bot only", `[
			{"id":"t1","isResolved":false,"comments":{"nodes":[{"author":{"login":"cursor","__typename":"Bot"}}]}}]`, "cursor[bot]"},
		{"no reviewer", `[]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/acme/w/installation", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"id":42}`)
			})
			mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"token":"inst-tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
			})
			mux.HandleFunc("/repos/acme/w/pulls/6", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"head":{"sha":"h6","ref":"feature/x"},"base":{"ref":"main"},"html_url":"http://x/6","user":{"login":"me"}}`)
			})
			mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":%s}}}}}`, tc.threads)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			key, err := rsa.GenerateKey(rand.Reader, 1024)
			if err != nil {
				t.Fatal(err)
			}
			g := newTestIntegration(t, baseConfig())
			g.app = &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
			g.rest = newRESTClient(g.app)

			var got []Trigger
			if _, err := g.Force(context.Background(), "changes_requested", "acme/w", 6,
				func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil || len(got) != 1 {
				t.Fatalf("force: err=%v trs=%d", err, len(got))
			}
			if a := got[0].Context["author"]; a != tc.want {
				t.Fatalf("author = %v, want %v (never the PR author)", a, tc.want)
			}
			if isBot, _ := got[0].Context["author_is_bot"].(bool); isBot != (tc.name == "bot only") {
				t.Fatalf("author_is_bot = %v for %s", isBot, tc.name)
			}
		})
	}
}
