package ghsource

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// `reaction_subjects` is what a run handling the event reacts on (github.react's
// `subjects`). These pin it per event path.

func subjects(kind string, ids ...int64) []any { return reactionSubjects(kind, ids...) }

// A standalone conversation comment reacts on itself (issues/comments).
func TestReactionSubjectIssueComment(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	trs := g.triggersFor(context.Background(), "issue_comment", []byte(`{
		"action":"created",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
		"comment":{"id":12,"user":{"login":"bob"},"body":"please fix"}}`))
	if len(trs) != 1 {
		t.Fatalf("want 1 trigger, got %d", len(trs))
	}
	if got, want := trs[0].Context["reaction_subjects"], subjects("issue_comment", 12); !reflect.DeepEqual(got, want) {
		t.Fatalf("reaction_subjects = %v, want %v", got, want)
	}
}

// An inline comment that stands alone reacts on the review-comment endpoint.
func TestReactionSubjectReviewComment(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	trs := g.triggersFor(context.Background(), "pull_request_review_comment", []byte(`{
		"action":"created",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"pull_request":{"number":3,"html_url":"u","head":{"sha":"h","ref":"feat"},"base":{"ref":"main"},"user":{"login":"me"}},
		"comment":{"id":3918412084,"user":{"login":"reviewer"},"body":"nit"}}`))
	if len(trs) != 1 {
		t.Fatalf("want 1 trigger, got %d", len(trs))
	}
	if got, want := trs[0].Context["reaction_subjects"], subjects("review_comment", 3918412084); !reflect.DeepEqual(got, want) {
		t.Fatalf("reaction_subjects = %v, want %v", got, want)
	}
}

// A failing check has no comment or review: no subject, so no reaction.
func TestReactionSubjectNoneForChecks(t *testing.T) {
	g := richWithREST(t)
	trs := g.triggersFor(context.Background(), "check_run", []byte(`{"action":"completed","installation":{"id":42},
		"repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"check_run":{"conclusion":"failure","name":"build","head_sha":"h9","id":321,
		"details_url":"https://github.com/acme/w/actions/runs/777/job/321","pull_requests":[{"number":4}]}}`))
	for _, tr := range trs {
		if _, has := tr.Context["reaction_subjects"]; has {
			t.Fatalf("%s carries reaction_subjects %v; a check has nothing to react on", tr.Kind, tr.Context["reaction_subjects"])
		}
	}
}

// A sweep-recovered unresolved-threads run reacts on each thread's opening
// comment — capped, so a PR with a dozen open threads doesn't fan out a
// dozen reactions each way.
func TestReactionSubjectsSweepThreadsCapped(t *testing.T) {
	got, _ := sweepThreadsRig(t, 12, map[string]Action{"changes_requested": {}})
	if len(got) != 1 || got[0].Kind != "changes_requested" {
		t.Fatalf("want one changes_requested, got %+v", got)
	}
	ss, _ := got[0].Context["reaction_subjects"].([]any)
	if len(ss) != maxThreadReactions {
		t.Fatalf("reaction_subjects has %d entries, want the cap %d: %v", len(ss), maxThreadReactions, ss)
	}
	seen := map[int64]bool{}
	for _, x := range ss {
		m := x.(map[string]any)
		id, _ := m["id"].(int64)
		if m["kind"] != "review_comment" || id < 501 || id > 512 || seen[id] {
			t.Fatalf("subject %v is not a distinct thread opener", m)
		}
		seen[id] = true
	}
}

// sweepThreadsRig sweeps one PR (#9, yours, head h9, with conductor's own
// `failure` status on it) carrying n unresolved threads opened by dana, and
// returns what it emitted plus how often a commit-status endpoint was read.
func sweepThreadsRig(t *testing.T, n int, actions map[string]Action) ([]Trigger, *int) {
	t.Helper()
	var nodes []string
	for i := 1; i <= n; i++ {
		nodes = append(nodes, fmt.Sprintf(`{"id":"t%d","isResolved":false,"comments":{"nodes":[{"databaseId":%d,"author":{"login":"dana","__typename":"User"},"path":"f.go","line":%d,"body":"b"}]}}`, i, 500+i, i))
	}
	statusReads := new(int)
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/77/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/repos/acme/widget/installation", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"id":77}`) })
	mux.HandleFunc("/repos/acme/widget/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"number":9,"user":{"login":"me"},"head":{"sha":"h9","ref":"feat"},"base":{"ref":"main"},"html_url":"u"}]`)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/9", func(w http.ResponseWriter, _ *http.Request) {
		// unstable: what GitHub reports while a non-required status fails.
		fmt.Fprint(w, `{"mergeable_state":"unstable","head":{"sha":"h9"},"base":{"ref":"main"},"html_url":"u"}`)
	})
	mux.HandleFunc("/repos/acme/widget/commits/", func(w http.ResponseWriter, _ *http.Request) {
		*statusReads++
		fmt.Fprint(w, `{"state":"failure","statuses":[{"context":"me","state":"failure","description":"gave up: timed out"}]}`)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[`+strings.Join(nodes, ",")+`]}}}}}`)
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
			Actions: as1(actions),
		}},
	}
	g := newTestIntegration(t, cfg)
	g.app = &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
	g.rest = newRESTClient(g.app)
	var got []Trigger
	if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
		t.Fatal(err)
	}
	return got, statusReads
}

// statusDelivery is a `status` webhook for context c in state st.
func statusDelivery(c, st string) []byte {
	return []byte(fmt.Sprintf(`{"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"installation":{"id":42},"sha":"abc1234def","context":%q,"state":%q,
		"branches":[{"name":"feat"}]}`, c, st))
}

// THE self-trigger guard. Hooks post commit statuses as you (github.set_status);
// a `failure` one read back as a failing check would dispatch the next fixer,
// whose verdict dispatches the next. Every status under one of your logins
// (set_status's default context) or under ANY context a set_status call posted
// (noted at call time — custom ones like "me / ci" included) is recognised as
// conductor's own and dropped at the router, before any handler, so nothing a
// status ever grows can see it.
func TestOwnStatusNeverTriggers(t *testing.T) {
	cfg := baseConfig()
	cfg.Rules[0].Actions = as1(map[string]Action{
		"failing_checks": {},
		"merge_ready":    {},
	})
	g := newTestIntegration(t, cfg)
	g.NoteOwnStatusContext("me / ci")
	g.NoteOwnStatusContext("Release Gate")

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	for _, c := range []string{"me", "ME", "me / ci", "release gate"} {
		for _, st := range []string{"failure", "error", "pending", "success"} {
			buf.Reset()
			if trs := g.triggersFor(context.Background(), "status", statusDelivery(c, st)); len(trs) != 0 {
				t.Fatalf("own status %q/%s produced %d trigger(s): %+v", c, st, len(trs), trs)
			}
			if !strings.Contains(buf.String(), "is conductor's own progress — ignored") {
				t.Fatalf("own status %q/%s was not recognised by the guard (log: %q)", c, st, buf.String())
			}
		}
	}
	// A foreign status is not conductor's own (and, today, not a CI signal).
	buf.Reset()
	if trs := g.triggersFor(context.Background(), "status", statusDelivery("ci/jenkins", "failure")); len(trs) != 0 {
		t.Fatalf("foreign status produced triggers: %+v", trs)
	}
	if strings.Contains(buf.String(), "own progress") {
		t.Fatalf("a foreign status was taken for conductor's own: %q", buf.String())
	}
	if g.OwnStatus("") || g.OwnStatus("ci/jenkins") {
		t.Fatal("OwnStatus matched a context that isn't conductor's")
	}
}

// The sweep side of the same guard: the sweep derives no CI facts from
// commit statuses — it never reads one — so a PR whose head carries
// conductor's own `failure` status sweeps to nothing CI-shaped even with
// failing_checks armed.
func TestSweepIgnoresOwnFailureStatus(t *testing.T) {
	got, statusReads := sweepThreadsRig(t, 1, map[string]Action{
		"changes_requested": {},
		"failing_checks":    {},
	})
	for _, tr := range got {
		if tr.Kind == "failing_checks" {
			t.Fatalf("sweep emitted failing_checks off a commit status: %+v", tr)
		}
	}
	if len(got) == 0 {
		t.Fatal("the sweep emitted nothing at all — the rig isn't exercising it")
	}
	if *statusReads != 0 {
		t.Fatalf("the sweep read commit statuses %d time(s)", *statusReads)
	}
}

// Every event carries `me: {login}` — you, as your writes act — so a hook can
// name a status context after you ("{{.me.login}} / review") without
// hardcoding a username: the login discovered from the write identity, else
// the first login you are identified by. Absent while unknown.
func TestMeFactOnEvents(t *testing.T) {
	comment := []byte(`{"action":"created",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
		"comment":{"id":12,"user":{"login":"bob"},"body":"please fix"}}`)
	g := newTestIntegration(t, baseConfig()) // me: { logins: [me] }
	trs := g.triggersFor(context.Background(), "issue_comment", comment)
	if len(trs) != 1 || !reflect.DeepEqual(trs[0].Context["me"], map[string]any{"login": "me"}) {
		t.Fatalf("me = %v, want the configured login", trs[0].Context["me"])
	}
	// Discovery from the write identity names the acting login.
	d := newTestIntegration(t, Config{
		App:      AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Identity: Identity{WriteToken: "literal-write-tok"},
		Rules:    baseConfig().Rules,
	})
	d.self = map[string]bool{}
	d.acting = ""
	d.app = userStub(t, "Octocat")
	d.discoverSelf(context.Background())
	if got := d.meFact(); !reflect.DeepEqual(got, map[string]any{"login": "Octocat"}) {
		t.Fatalf("me after discovery = %v, want Octocat (as GitHub spells it)", got)
	}
	// Unknown: no fact at all.
	u := newTestIntegration(t, Config{App: AppConfig{AppID: 1, PrivateKeyPath: "x"}})
	if u.meFact() != nil {
		t.Fatalf("me with no known login = %v, want nil", u.meFact())
	}
}
