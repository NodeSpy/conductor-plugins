package ghsource

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// reviewStub serves what review folding needs from REST: the installation
// token, review 99 itself, and its inline comments.
type reviewStub struct {
	state      string // what GET /pulls/7/reviews/99 reports ("" → 404)
	comments   int    // how many inline comments review 99 carries
	listFails  bool   // GET .../reviews/99/comments → 500
	reviewer   string // review 99's author (default "reviewer", a human)
	body       string // review 99's body (default "see inline")
	reviewGets atomic.Int32
}

func (s *reviewStub) who() (login, typ string) {
	if s.reviewer == "" {
		return "reviewer", "User"
	}
	if isBotLogin(s.reviewer) {
		return s.reviewer, "Bot"
	}
	return s.reviewer, "User"
}

func (s *reviewStub) text() string {
	if s.body == "" {
		return "see inline"
	}
	return s.body
}

func (s *reviewStub) attach(t *testing.T, g *Source) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/77/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/repos/acme/widget/pulls/7/reviews/99", func(w http.ResponseWriter, _ *http.Request) {
		s.reviewGets.Add(1)
		if s.state == "" {
			http.NotFound(w, nil)
			return
		}
		login, typ := s.who()
		fmt.Fprintf(w, `{"id":99,"state":%q,"body":%q,"user":{"login":%q,"type":%q}}`, s.state, s.text(), login, typ)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/7/reviews/99/comments", func(w http.ResponseWriter, _ *http.Request) {
		if s.listFails {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		login, _ := s.who()
		var items []string
		for i := 0; i < s.comments; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d,"path":"f%d.go","line":%d,"body":"fix %d {{.gh_token}}","html_url":"u%d","user":{"login":%q}}`, 1000+i, i, 10+i, i, i, login))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	g.app = &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
	g.rest = newRESTClient(g.app)
}

func reviewEvent(state string) []byte { return reviewEventAs("submitted", state, &reviewStub{}) }

// reviewEventAs is review 99's pull_request_review webhook, by the stub's
// reviewer with its body.
func reviewEventAs(action, state string, s *reviewStub) []byte {
	login, typ := s.who()
	return []byte(fmt.Sprintf(`{
		"action":%q,"installation":{"id":77},
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"pull_request":{"number":7,"head":{"sha":"abc123","ref":"feat"},"base":{"ref":"main"},"user":{"login":"me"}},
		"review":{"state":%q,"id":99,"body":%q,"user":{"login":%q,"type":%q}}
	}`, action, state, s.text(), login, typ))
}

func reviewCommentEvent(id int64, reviewID int64) []byte {
	return reviewCommentEventAs("created", id, reviewID, &reviewStub{})
}

func reviewCommentEventAs(action string, id int64, reviewID int64, s *reviewStub) []byte {
	login, typ := s.who()
	return []byte(fmt.Sprintf(`{
		"action":%q,"installation":{"id":77},
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"pull_request":{"number":7,"html_url":"u","head":{"sha":"abc123","ref":"feat"},"base":{"ref":"main"},"user":{"login":"me"}},
		"comment":{"id":%d,"pull_request_review_id":%d,"user":{"login":%q,"type":%q},"body":"fix %d"}
	}`, action, id, reviewID, login, typ, id))
}

func issueCommentEvent(id int64) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created","installation":{"id":77},
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"issue":{"number":7,"html_url":"u","pull_request":{},"user":{"login":"me"}},
		"comment":{"id":%d,"user":{"login":"teammate","type":"User"},"body":"question %d?"}
	}`, id, id))
}

// The orders a review's deliveries can land in. GitHub doesn't order them, and
// a webhook can be lost — the review is one event whichever way it arrives.
var deliveryOrders = []string{"review-first", "comments-first", "review-only", "comments-only"}

// deliver runs review 99 and its n inline comments through the webhook path
// in the given order and returns every trigger they produced.
func deliver(g *Source, state string, n int, order string) []Trigger {
	return deliverAs(g, &reviewStub{}, state, n, order)
}

// deliverAs is deliver for the stub's reviewer and body.
func deliverAs(g *Source, s *reviewStub, state string, n int, order string) []Trigger {
	ctx := context.Background()
	var out []Trigger
	review := func() {
		out = append(out, g.triggersFor(ctx, "pull_request_review", reviewEventAs("submitted", state, s))...)
	}
	comments := func() {
		for i := 0; i < n; i++ {
			out = append(out, g.triggersFor(ctx, "pull_request_review_comment", reviewCommentEventAs("created", int64(1000+i), 99, s))...)
		}
	}
	switch order {
	case "review-first":
		review()
		comments()
	case "comments-first":
		comments()
		review()
	case "review-only":
		review()
	case "comments-only":
		comments()
	}
	return out
}

func kindsOf(trs []Trigger) map[string]int {
	out := map[string]int{}
	for _, tr := range trs {
		out[tr.Kind]++
	}
	return out
}

// One review submission is ONE event, whatever its state, however its
// deliveries arrive. With a changes_requested trigger that takes the review, a
// changes-request — or a review that leaves inline comments without approving
// — is that one run (the incident: not that plus one fixer per comment); an
// approval with inline comments is a single new_comment. Either way the event
// carries the review body and every inline comment, and the review's highest
// inline comment id (which the engine's high-water mark dispatches once).
func TestReviewIsExactlyOneEvent(t *testing.T) {
	const n = 4
	for _, tc := range []struct{ restState, hookState, wantKind string }{
		{"CHANGES_REQUESTED", "changes_requested", "changes_requested"},
		{"COMMENTED", "commented", "changes_requested"},
		{"APPROVED", "approved", "new_comment"},
	} {
		for _, order := range deliveryOrders {
			t.Run(tc.hookState+"/"+order, func(t *testing.T) {
				g := newTestIntegration(t, baseConfig())
				stub := &reviewStub{state: tc.restState, comments: n}
				stub.attach(t, g)

				trs := deliver(g, tc.hookState, n, order)
				if k := kindsOf(trs); len(trs) != 1 || k[tc.wantKind] != 1 {
					t.Fatalf("a %s review with %d inline comments produced %v, want exactly one %s", tc.hookState, n, k, tc.wantKind)
				}
				ev := trs[0]
				list, _ := ev.Context["review_comments"].([]any)
				if len(list) != n {
					t.Fatalf("%s carries %d review_comments, want all %d", ev.Kind, len(list), n)
				}
				first, _ := list[0].(map[string]any)
				if first["path"] != "f0.go" || first["line"] != 10 || first["author"] != "reviewer" || !strings.HasPrefix(first["body"].(string), "fix 0") {
					t.Fatalf("review comment not carried faithfully: %v", first)
				}
				if ev.Context["review_id"] != int64(99) || ev.Context["review_body"] != "see inline" || ev.Context["author"] != "reviewer" {
					t.Fatalf("review identity not carried: %v", ev.Context)
				}
				// Run progress reacts on the review itself, not its comments.
				if got, want := ev.Context["reaction_subjects"], reactionSubjects("review", 99); !reflect.DeepEqual(got, want) {
					t.Fatalf("reaction_subjects = %v, want %v (the review)", got, want)
				}
				// The review's facts cost at most one REST read across all of
				// its deliveries (none when the review event came first).
				if got := stub.reviewGets.Load(); got > 1 {
					t.Fatalf("review read %d times for one review, want at most 1", got)
				}
				if ev.Context["comment_id"] != int64(1000+n-1) || ev.Context["comment_kind"] != CommentKindReview {
					t.Fatalf("comment_id/kind = %v/%v, want the review's highest id %d / review", ev.Context["comment_id"], ev.Context["comment_kind"], 1000+n-1)
				}
				if ev.Context["review_state"] != tc.hookState {
					t.Fatalf("review_state %v, want %s", ev.Context["review_state"], tc.hookState)
				}
				if tc.wantKind != "new_comment" {
					return
				}
				// A single-comment trigger's fields still read sensibly: the
				// reviewer is the commenter, comment_body is the whole review,
				// comment_id is the review's highest (the engine's high-water
				// mark then drops any later recovery of it).
				body, _ := ev.Context["comment_body"].(string)
				if !strings.HasPrefix(body, "see inline") || !strings.Contains(body, "f3.go:13: fix 3") {
					t.Fatalf("comment_body should be the review body plus every inline comment, got %q", body)
				}
				if ev.Dedup != "review:99" {
					t.Fatalf("dedup %q, want review:99", ev.Dedup)
				}
			})
		}
	}
}

// A review no changes_requested trigger takes is still one review — so ONE
// new_comment, never one per inline comment.
func TestUntakenChangesRequestIsOneNewComment(t *testing.T) {
	for _, st := range []struct{ rest, hook string }{{"CHANGES_REQUESTED", "changes_requested"}, {"COMMENTED", "commented"}} {
		t.Run("no changes_requested trigger/"+st.hook, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Rules[0].Actions = as1(map[string]Action{"new_comment": {}})
			g := newTestIntegration(t, cfg)
			(&reviewStub{state: st.rest, comments: 3}).attach(t, g)
			if k := kindsOf(deliver(g, st.hook, 3, "comments-first")); k["new_comment"] != 1 || len(k) != 1 {
				t.Fatalf("got %v, want one new_comment", k)
			}
		})
	}
	t.Run("changes_requested filter rejects the reviewer", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Rules[0].Actions["changes_requested"] = ActionSet{{
			Filter: sourcekit.FilterExpr("reviewer != 'reviewer'")}}
		g := newTestIntegration(t, cfg)
		(&reviewStub{state: "CHANGES_REQUESTED", comments: 3}).attach(t, g)
		if k := kindsOf(deliver(g, "changes_requested", 3, "review-first")); k["new_comment"] != 1 || len(k) != 1 {
			t.Fatalf("got %v, want one new_comment and no changes_requested", k)
		}
	})
}

// Folding must never DROP feedback: when a review can't be read, its comments
// stand alone — one new_comment each, as before.
func TestUnreadableReviewCommentsStandAlone(t *testing.T) {
	for name, stub := range map[string]*reviewStub{
		"review unreadable":   {state: ""},
		"comments unreadable": {state: "APPROVED", listFails: true},
	} {
		t.Run(name, func(t *testing.T) {
			g := newTestIntegration(t, baseConfig())
			stub.attach(t, g)
			if k := kindsOf(deliver(g, "approved", 3, "comments-only")); k["new_comment"] != 3 {
				t.Fatalf("got %v, want the 3 comments as a new_comment each", k)
			}
		})
	}
}

// A: a review bot (Cursor Bugbot) reviews COMMENTED with inline findings.
// When a changes_requested trigger takes it, that review is ONE
// changes_requested run carrying every finding — and zero new_comment runs —
// whichever way its deliveries arrive. The run's re-request names the bot,
// which only_outstanding (and the bot filter) then decline to ping.
func TestBotCommentedReviewIsOneChangesRequested(t *testing.T) {
	const n = 3
	for _, order := range deliveryOrders {
		t.Run(order, func(t *testing.T) {
			g := newTestIntegration(t, baseConfig())
			stub := &reviewStub{state: "COMMENTED", comments: n, reviewer: "cursor[bot]", body: "Bugbot found 3 potential issues"}
			stub.attach(t, g)
			trs := deliverAs(g, stub, "commented", n, order)
			if k := kindsOf(trs); len(trs) != 1 || k["changes_requested"] != 1 {
				t.Fatalf("a COMMENTED bot review with %d findings produced %v, want exactly one changes_requested and no new_comment", n, k)
			}
			ev := trs[0]
			if ev.Context["author"] != "cursor[bot]" || ev.Context["author_is_bot"] != true {
				t.Fatalf("reviewer not carried: author=%v author_is_bot=%v", ev.Context["author"], ev.Context["author_is_bot"])
			}
			if list, _ := ev.Context["review_comments"].([]any); len(list) != n {
				t.Fatalf("changes_requested carries %d findings, want all %d", len(list), n)
			}
		})
	}
	// A changes_requested trigger that does NOT take bot reviews leaves the
	// review a single new_comment.
	cfg := baseConfig()
	cfg.Rules[0].Actions["changes_requested"] = ActionSet{{
		Filter: sourcekit.FilterExpr("!author_is_bot")}}
	g := newTestIntegration(t, cfg)
	stub := &reviewStub{state: "COMMENTED", comments: n, reviewer: "cursor[bot]"}
	stub.attach(t, g)
	if k := kindsOf(deliverAs(g, stub, "commented", n, "comments-first")); k["new_comment"] != 1 || len(k) != 1 {
		t.Fatalf("bot review no changes_requested trigger takes: %v, want one new_comment", k)
	}
}

// B: an approval with inline suggestions ("optional; nothing blocks") is ONE
// run for the review — a new_comment, never a changes_requested (so no
// re-request of the approver), even with a changes_requested trigger armed.
func TestApprovedReviewWithSuggestionsIsOneNewComment(t *testing.T) {
	const n = 4
	for _, order := range deliveryOrders {
		t.Run(order, func(t *testing.T) {
			g := newTestIntegration(t, baseConfig())
			stub := &reviewStub{state: "APPROVED", comments: n, reviewer: "lead", body: "4 optional suggestions; nothing blocks"}
			stub.attach(t, g)
			trs := deliverAs(g, stub, "approved", n, order)
			if k := kindsOf(trs); len(trs) != 1 || k["new_comment"] != 1 {
				t.Fatalf("an approval with %d inline suggestions produced %v, want exactly one new_comment", n, k)
			}
			ev := trs[0]
			if ev.Context["review_state"] != "approved" || ev.Context["author"] != "lead" {
				t.Fatalf("review not carried: %v", ev.Context)
			}
			if list, _ := ev.Context["review_comments"].([]any); len(list) != n {
				t.Fatalf("new_comment carries %d suggestions, want all %d", len(list), n)
			}
			if body, _ := ev.Context["comment_body"].(string); !strings.HasPrefix(body, "4 optional suggestions; nothing blocks") {
				t.Fatalf("comment_body = %q, want the review body first", body)
			}
		})
	}
}

// C: only a SUBMISSION is an event. Bugbot later rewrites an old review's
// body ("Stale Bugbot comment from a previous run.") and may edit its
// comments: those "edited" deliveries dispatch nothing — not after the
// review's own event, and not on their own.
func TestEditedReviewIsNotAnEvent(t *testing.T) {
	stale := &reviewStub{state: "COMMENTED", comments: 3, reviewer: "cursor[bot]", body: "Stale Bugbot comment from a previous run."}
	edits := func(g *Source) []Trigger {
		ctx := context.Background()
		out := g.triggersFor(ctx, "pull_request_review", reviewEventAs("edited", "commented", stale))
		for i := 0; i < 3; i++ {
			out = append(out, g.triggersFor(ctx, "pull_request_review_comment", reviewCommentEventAs("edited", int64(1000+i), 99, stale))...)
		}
		return out
	}
	t.Run("after the submission", func(t *testing.T) {
		g := newTestIntegration(t, baseConfig())
		stale.attach(t, g)
		sub := &reviewStub{state: "COMMENTED", comments: 3, reviewer: "cursor[bot]", body: "Bugbot found 3 potential issues"}
		if k := kindsOf(deliverAs(g, sub, "commented", 3, "review-first")); k["changes_requested"] != 1 {
			t.Fatalf("submission: %v, want one changes_requested", k)
		}
		if trs := edits(g); len(trs) != 0 {
			t.Fatalf("editing the review re-dispatched it: %v", kindsOf(trs))
		}
	})
	t.Run("on their own", func(t *testing.T) {
		g := newTestIntegration(t, baseConfig())
		stale.attach(t, g)
		if trs := edits(g); len(trs) != 0 {
			t.Fatalf("an edited review/comment is not a submission, got %v", kindsOf(trs))
		}
	})
}

// A standalone comment — a conversation comment, or a review comment that
// names no review — is its own event: N of them are N events, never merged.
func TestStandaloneCommentsAreOneEventEach(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	(&reviewStub{state: "COMMENTED", comments: 3}).attach(t, g)
	ctx := context.Background()
	var trs []Trigger
	for i := 0; i < 3; i++ {
		trs = append(trs, g.triggersFor(ctx, "issue_comment", issueCommentEvent(int64(2000+i)))...)
		trs = append(trs, g.triggersFor(ctx, "pull_request_review_comment", reviewCommentEvent(int64(3000+i), 0))...)
	}
	if k := kindsOf(trs); len(trs) != 6 || k["new_comment"] != 6 {
		t.Fatalf("6 standalone comments produced %v, want 6 new_comment", k)
	}
	seen := map[string]bool{}
	for _, tr := range trs {
		if !strings.HasPrefix(tr.Dedup, "comment:") || seen[tr.Dedup] {
			t.Fatalf("standalone comment dedup %q: want a distinct comment:<id> each", tr.Dedup)
		}
		seen[tr.Dedup] = true
	}
}

// sweepStubFor serves an `acme/widget` sweep over PR 9 whose recent comments
// are: one conversation comment, three inline findings of review bot
// cursor[bot]'s COMMENTED review 41 (its threads still unresolved), and two
// inline suggestions of carol's APPROVED review 42 (her thread is unresolved
// too, but she approved — not outstanding change-requested feedback).
func sweepStubFor(t *testing.T, reviewGets *atomic.Int32) *appAuth {
	return sweepStubCounting(t, reviewGets, new(atomic.Int32))
}

// sweepStubCounting is sweepStubFor that also counts reads of review 42's
// inline comments.
func sweepStubCounting(t *testing.T, reviewGets, listGets *atomic.Int32) *appAuth {
	t.Helper()
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
		fmt.Fprint(w, `{"mergeable_state":"clean","head":{"sha":"h9","ref":"feat"},"base":{"ref":"main"},"html_url":"u"}`)
	})
	fresh := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	mux.HandleFunc("/repos/acme/widget/issues/9/comments", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"id":5515854542,"user":{"login":"teammate"},"body":"question?","created_at":%q}]`, fresh)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/9/comments", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[
			{"id":3918412105,"pull_request_review_id":42,"path":"y.go","line":2,"user":{"login":"carol"},"body":"nit 2","created_at":%[1]q},
			{"id":3918412104,"pull_request_review_id":42,"path":"x.go","line":1,"user":{"login":"carol"},"body":"nit 1","created_at":%[1]q},
			{"id":3918412102,"pull_request_review_id":41,"user":{"login":"cursor[bot]"},"body":"c","created_at":%[1]q},
			{"id":3918412101,"pull_request_review_id":41,"user":{"login":"cursor[bot]"},"body":"b","created_at":%[1]q},
			{"id":3918412100,"pull_request_review_id":41,"user":{"login":"cursor[bot]"},"body":"a","created_at":%[1]q}]`, fresh)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/9/reviews/41", func(w http.ResponseWriter, _ *http.Request) {
		reviewGets.Add(1)
		fmt.Fprint(w, `{"id":41,"state":"COMMENTED","body":"Stale Bugbot comment from a previous run.","user":{"login":"cursor[bot]","type":"Bot"}}`)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/9/reviews/42", func(w http.ResponseWriter, _ *http.Request) {
		reviewGets.Add(1)
		fmt.Fprint(w, `{"id":42,"state":"APPROVED","body":"two nits","user":{"login":"carol","type":"User"}}`)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/9/reviews/41/comments", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[
			{"id":3918412100,"path":"a.go","line":3,"body":"a","user":{"login":"cursor[bot]"}},
			{"id":3918412101,"path":"b.go","line":8,"body":"b","user":{"login":"cursor[bot]"}},
			{"id":3918412102,"path":"c.go","line":5,"body":"c","user":{"login":"cursor[bot]"}}]`)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/9/reviews/42/comments", func(w http.ResponseWriter, _ *http.Request) {
		listGets.Add(1)
		fmt.Fprint(w, `[
			{"id":3918412104,"path":"x.go","line":1,"body":"nit 1","user":{"login":"carol"}},
			{"id":3918412105,"path":"y.go","line":2,"body":"nit 2","user":{"login":"carol"}}]`)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{
			"latestOpinionatedReviews":{"nodes":[{"state":"APPROVED","author":{"login":"carol"}}]},
			"reviewThreads":{"nodes":[
			{"id":"t1","isResolved":false,"comments":{"nodes":[{"databaseId":3918412100,"author":{"login":"cursor","__typename":"Bot"},"path":"a.go","line":3,"body":"a","url":"ua"}]}},
			{"id":"t2","isResolved":false,"comments":{"nodes":[{"databaseId":3918412101,"author":{"login":"cursor","__typename":"Bot"},"path":"b.go","originalLine":8,"body":"b","url":"ub"}]}},
			{"id":"t3","isResolved":false,"comments":{"nodes":[{"databaseId":3918412102,"author":{"login":"cursor","__typename":"Bot"},"path":"c.go","line":5,"body":"c","url":"uc"}]}},
			{"id":"t4","isResolved":false,"comments":{"nodes":[{"databaseId":3918412104,"author":{"login":"carol","__typename":"User"},"path":"x.go","line":1,"body":"nit 1","url":"ux"}]}}]}}}}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	return &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
}

func sweepConfig() Config {
	return Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Sweep:   SweepConfig{Enabled: boolp(true), Repos: []string{"acme/widget"}},
		Rules: []Rule{{
			Match: Match{Repos: []string{"acme/widget"}},
			Me:    Actors{Logins: []string{"me"}},
			Actions: as1(map[string]Action{
				"changes_requested": {},
				"new_comment":       {},
			}),
		}},
	}
}

// The sweep's missed-comment recovery follows the same rule: a review is
// recovered as its one event — the approval → one new_comment; the bot's
// COMMENTED review → the unresolved-threads changes_requested (its findings,
// not the approver's thread), no new_comment — a standalone comment as its
// own, and a review already emitted is not recovered again.
func TestSweepRecoversAReviewAsOneEvent(t *testing.T) {
	var reviewGets atomic.Int32
	g := newTestIntegration(t, sweepConfig())
	g.app = sweepStubFor(t, &reviewGets)
	g.rest = newRESTClient(g.app)

	sweepOnce := func() []Trigger {
		var got []Trigger
		if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
			t.Fatal(err)
		}
		return got
	}
	var standalone, reviews, cr []Trigger
	for _, tr := range sweepOnce() {
		switch {
		case tr.Kind == "changes_requested":
			cr = append(cr, tr)
		case tr.Kind == "new_comment" && strings.HasPrefix(tr.Dedup, "review:"):
			reviews = append(reviews, tr)
		case tr.Kind == "new_comment":
			standalone = append(standalone, tr)
		}
	}
	if len(standalone) != 1 || standalone[0].Context["comment_id"] != int64(5515854542) {
		t.Fatalf("standalone recovered: %v, want just the conversation comment", standalone)
	}
	if len(reviews) != 1 || reviews[0].Dedup != "review:42" {
		t.Fatalf("reviews recovered as new_comment: %v, want ONE for approved review 42 (and none for the bot's review 41)", reviews)
	}
	rv := reviews[0]
	if list, _ := rv.Context["review_comments"].([]any); len(list) != 2 || rv.Context["review_body"] != "two nits" ||
		rv.Context["comment_id"] != int64(3918412105) || rv.Context["author"] != "carol" {
		t.Fatalf("recovered review lost its content: %v", rv.Context)
	}
	if len(cr) != 1 {
		t.Fatalf("sweep emitted %d changes_requested, want 1 for the unresolved threads", len(cr))
	}
	list, _ := cr[0].Context["review_comments"].([]any)
	if len(list) != 3 {
		t.Fatalf("sweep changes_requested carries %d review_comments, want the bot's 3 unresolved threads (not the approver's)", len(list))
	}
	if cr[0].Context["author"] != "cursor[bot]" || cr[0].Context["comment_id"] != int64(3918412102) || cr[0].Context["comment_kind"] != CommentKindReview {
		t.Fatalf("sweep changes_requested: author=%v comment_id=%v kind=%v, want cursor[bot] / the threads' highest id 3918412102 / review",
			cr[0].Context["author"], cr[0].Context["comment_id"], cr[0].Context["comment_kind"])
	}
	if second, _ := list[1].(map[string]any); second["path"] != "b.go" || second["line"] != 8 || second["body"] != "b" {
		t.Fatalf("outdated thread comment not carried (want b.go:8 from originalLine): %v", second)
	}

	// A second sweep must not recover review 42 again, and costs no further
	// review reads (one per review, cached).
	for _, tr := range sweepOnce() {
		if tr.Dedup == "review:42" {
			t.Fatal("second sweep re-emitted review 42")
		}
	}
	if n := reviewGets.Load(); n != 2 {
		t.Fatalf("review read %d times over two sweeps, want 2 (once per review)", n)
	}
}

// A review the webhook already turned into its event is not recovered by the
// sweep as another one — nor read again.
func TestSweepSkipsAReviewTheWebhookEmitted(t *testing.T) {
	var reviewGets, listGets atomic.Int32
	g := newTestIntegration(t, sweepConfig())
	g.app = sweepStubCounting(t, &reviewGets, &listGets)
	g.rest = newRESTClient(g.app)
	hook := []byte(`{
		"action":"submitted","installation":{"id":77},
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"pull_request":{"number":9,"head":{"sha":"h9","ref":"feat"},"base":{"ref":"main"},"user":{"login":"me"}},
		"review":{"state":"approved","id":42,"body":"two nits","user":{"login":"carol","type":"User"}}
	}`)
	if k := kindsOf(g.triggersFor(context.Background(), "pull_request_review", hook)); k["new_comment"] != 1 {
		t.Fatalf("webhook review: %v, want one new_comment", k)
	}
	var got []Trigger
	if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
		t.Fatal(err)
	}
	for _, tr := range got {
		if tr.Dedup == "review:42" {
			t.Fatal("sweep re-emitted a review the webhook already emitted")
		}
	}
	if n := listGets.Load(); n != 1 {
		t.Fatalf("review 42's comments read %d times, want 1 (the webhook's; the sweep skips a claimed review)", n)
	}
}

// C, sweep half: a bot review the webhook dispatched as changes_requested is
// re-derived by every later sweep while its threads stay unresolved (and
// after the bot rewrites the review body as stale) — at or below the
// comment_id the webhook's run carried, so the engine's high-water mark
// (TestChangesRequestedDispatchesAReviewOnce) never dispatches it again.
func TestSweepRederivesADispatchedReviewAtItsMark(t *testing.T) {
	var reviewGets atomic.Int32
	g := newTestIntegration(t, sweepConfig())
	g.app = sweepStubFor(t, &reviewGets)
	g.rest = newRESTClient(g.app)
	hook := []byte(`{
		"action":"submitted","installation":{"id":77},
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"pull_request":{"number":9,"head":{"sha":"h9","ref":"feat"},"base":{"ref":"main"},"user":{"login":"me"}},
		"review":{"state":"commented","id":41,"body":"Bugbot found 3 potential issues","user":{"login":"cursor[bot]","type":"Bot"}}
	}`)
	trs := g.triggersFor(context.Background(), "pull_request_review", hook)
	if k := kindsOf(trs); k["changes_requested"] != 1 || len(trs) != 1 {
		t.Fatalf("webhook: bot review produced %v, want one changes_requested", k)
	}
	mark, _ := trs[0].Context["comment_id"].(int64)
	if mark == 0 {
		t.Fatal("webhook changes_requested carries no comment_id — nothing would mark the review dispatched")
	}
	var got []Trigger
	if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
		t.Fatal(err)
	}
	for _, tr := range got {
		if tr.Kind == "changes_requested" {
			if id, _ := tr.Context["comment_id"].(int64); id == 0 || id > mark {
				t.Fatalf("sweep re-derived the dispatched review with comment_id %d, want 0 < id <= the webhook's %d", id, mark)
			}
		}
		if tr.Dedup == "review:41" {
			t.Fatal("sweep re-emitted the bot review as a new_comment")
		}
	}
}

// review_comments stays inside a prompt a runtime will accept: each body is
// capped (on a rune boundary) and the list stops at a byte budget, counting
// what it left out so the agent knows to read the rest on the PR.
func TestReviewCommentsAreCapped(t *testing.T) {
	long := strings.Repeat("é", maxReviewCommentBody) // 2 bytes per rune
	ctx := map[string]any{}
	addReviewComments(ctx, []reviewComment{{Author: "a", Path: "p", Body: long}})
	body := ctx["review_comments"].([]any)[0].(map[string]any)["body"].(string)
	if len(body) > maxReviewCommentBody+len("…") || !strings.HasSuffix(body, "…") {
		t.Fatalf("body not capped: %d bytes", len(body))
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(body, "…")) {
		t.Fatal("cap split a rune")
	}
	if _, ok := ctx["review_comments_omitted"]; ok {
		t.Fatal("nothing was omitted, but review_comments_omitted is set")
	}

	many := make([]reviewComment, 200)
	for i := range many {
		many[i] = reviewComment{Author: "a", Path: "p", Body: strings.Repeat("x", maxReviewCommentBody)}
	}
	ctx = map[string]any{}
	addReviewComments(ctx, many)
	kept := len(ctx["review_comments"].([]any))
	total := 0
	for _, c := range ctx["review_comments"].([]any) {
		total += len(c.(map[string]any)["body"].(string))
	}
	if total > maxReviewCommentsBytes || kept == 0 {
		t.Fatalf("kept %d comments / %d body bytes, want within the %d-byte budget", kept, total, maxReviewCommentsBytes)
	}
	if got := ctx["review_comments_omitted"]; got != len(many)-kept {
		t.Fatalf("review_comments_omitted = %v, want %d", got, len(many)-kept)
	}
	if s := reviewSummary("", many); len(s) > maxReviewSummaryBytes+len("…") {
		t.Fatalf("folded comment_body not capped: %d bytes", len(s))
	}
}
