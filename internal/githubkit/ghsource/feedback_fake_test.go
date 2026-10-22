package ghsource

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghfake"
)

// fakeRig is a source wired to the stateful, schema-validating fake: App
// auth against the fake's App, and the fake's own webhook deliveries fed to
// the source in whatever order a test chooses.
type fakeRig struct {
	f  *ghfake.Fake
	g  *Source
	n  int
	mu sync.Mutex
	at time.Time
}

func newFakeRig(t *testing.T) *fakeRig {
	t.Helper()
	r := &fakeRig{at: time.Now().UTC().Truncate(time.Second)}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	r.f = ghfake.New(ghfake.Options{Logf: t.Logf, Now: r.tick})
	r.f.Start()
	t.Cleanup(r.f.Close)
	t.Cleanup(func() { // runs before Close: every call the source made is one GitHub has
		if u := r.f.Unhandled(); len(u) > 0 {
			t.Errorf("calls the fake does not serve: %v", u)
		}
	})
	r.f.SetApp(77, "conductor-test", &key.PublicKey)
	r.f.AddUser("acme", "Organization")
	r.f.AddRepo("acme/w")
	r.f.Install("acme")
	r.f.UserToken("me", "tok-me")
	for _, u := range []string{"rev", "rev2", "rev3"} {
		r.f.AddUser(u, "User")
		r.f.AddCollaborator("acme/w", u)
	}
	r.f.AddUser("cursor[bot]", "Bot")
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(202) }))
	t.Cleanup(sink.Close)
	r.f.AddHook(sink.URL, "s3cret")
	r.n = r.f.OpenPR("acme/w", "me", ghfake.PROpts{Title: "feature", Head: "feat"})
	r.f.Flush()

	r.g = newTestIntegration(t, Config{
		App:     AppConfig{AppID: 77, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s3cret"},
		Sweep:   SweepConfig{Enabled: boolp(true), Repos: []string{"acme/w"}},
		Rules: []Rule{{
			Match:   Match{Repos: []string{"acme/w"}},
			Me:      Actors{Logins: []string{"me"}},
			Actions: as1(map[string]Action{"changes_requested": {}}),
		}},
	})
	r.g.app = &appAuth{appID: 77, key: key, httpc: http.DefaultClient, apiBase: r.f.URL(), now: time.Now, cache: map[int64]cachedToken{}}
	r.g.rest = newRESTClient(r.g.app)
	return r
}

// tick is the fake's clock: every reading is a new second.
func (r *fakeRig) tick() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.at = r.at.Add(time.Second)
	return r.at
}

// deliveriesSince returns the deliveries made after the first `from`.
func (r *fakeRig) deliveriesSince(from int) []ghfake.Delivery {
	r.f.Flush()
	ds := r.f.Deliveries()
	return ds[from:]
}

// feed hands deliveries to the source and returns the changes_requested
// events it emitted.
func (r *fakeRig) feed(ds []ghfake.Delivery) []Trigger {
	var out []Trigger
	for _, d := range ds {
		for _, tr := range r.g.Translate(context.Background(), d.Event, d.Body) {
			if tr.Kind == "changes_requested" {
				out = append(out, tr)
			}
		}
	}
	return out
}

func commentIDs(tr Trigger) map[int64]bool {
	out := map[int64]bool{}
	cs, _ := tr.Context["review_comments"].([]any)
	for _, c := range cs {
		if m, ok := c.(map[string]any); ok {
			if id, ok := m["id"].(int64); ok {
				out[id] = true
			}
		}
	}
	return out
}

func commentBodies(tr Trigger) map[string]bool {
	out := map[string]bool{}
	cs, _ := tr.Context["review_comments"].([]any)
	for _, c := range cs {
		if m, ok := c.(map[string]any); ok {
			if b, ok := m["body"].(string); ok {
				out[b] = true
			}
		}
	}
	return out
}

func cursorOf(t *testing.T, tr Trigger) int64 {
	t.Helper()
	c, ok := tr.Context["feedback_cursor"].(int64)
	if !ok || c <= 0 {
		t.Fatalf("event carries no feedback_cursor: %v", tr.Context["feedback_cursor"])
	}
	return c
}

// Two reviews from one reviewer seconds apart — X a new inline comment, Y a
// reply on an older thread — were two events; only Y's ran, and X's comment
// was never addressed. Every changes_requested event now carries all the
// feedback still waiting on you, so either one covers both, in either order,
// and the other's cursor is at or below it (the engine drops it as covered).
func TestTwoReviewsSecondsApartAreOneRun(t *testing.T) {
	r := newFakeRig(t)
	// An older thread you already answered.
	_, old := r.f.SubmitReview("acme/w", r.n, "rev", "COMMENTED", "", ghfake.InlineComment{Body: "why this?"})
	r.f.Reply("acme/w", r.n, old[0], "me", "because")
	r.f.Flush()
	mark := len(r.f.Deliveries())

	xID, xc := r.f.SubmitReview("acme/w", r.n, "rev", "COMMENTED", "", ghfake.InlineComment{Body: "handle the nil case"})
	r.f.Flush()
	mid := len(r.f.Deliveries())
	yc := r.f.Reply("acme/w", r.n, old[0], "rev", "that does not answer it")
	all := r.deliveriesSince(mark)
	xs, ys := all[:mid-mark], all[mid-mark:]

	// Y handled first (the order that lost X).
	gotY := r.feed(ys)
	if len(gotY) != 1 {
		t.Fatalf("Y emitted %d changes_requested, want 1", len(gotY))
	}
	if bs := commentBodies(gotY[0]); !bs["handle the nil case"] || !bs["that does not answer it"] {
		t.Fatalf("Y's run must carry X's comment and Y's reply, got %v", bs)
	}
	ids := commentIDs(gotY[0])
	if !ids[yc] || !ids[xc[0]] {
		t.Fatalf("Y's run must name X's comment %d and Y's reply %d, got %v", xc[0], yc, ids)
	}
	if ids[old[0]] != true {
		t.Fatal("the reply's thread opener rides along for context")
	}
	gotX := r.feed(xs)
	if len(gotX) != 1 {
		t.Fatalf("X emitted %d changes_requested, want 1", len(gotX))
	}
	if cx, cy := cursorOf(t, gotX[0]), cursorOf(t, gotY[0]); cx > cy {
		t.Fatalf("X's cursor %d is above Y's %d: it would run again for feedback Y's run already carried", cx, cy)
	}
	// Both reviews are reacted on.
	subs, _ := gotY[0].Context["reaction_subjects"].([]any)
	found := map[int64]bool{}
	for _, s := range subs {
		m := s.(map[string]any)
		found[m["id"].(int64)] = true
	}
	if !found[xc[0]] && !found[xID] {
		t.Fatalf("X is not among Y's reaction subjects %v", subs)
	}
	yID, _ := gotY[0].Context["review_id"].(int64)
	if yID == 0 || !found[yID] {
		t.Fatalf("Y (the review that fired) is not among its reaction subjects %v", subs)
	}
	if found[yc] {
		t.Fatalf("Y is reacted on as a whole; its own comment %d must not be a second subject: %v", yc, subs)
	}
}

// GitHub numbers a review's comments when they are DRAFTED. A review drafted
// before another but submitted after it has the lower ids; under a mark over
// comment ids it was dropped as already handled. The cursor is the submission.
func TestReviewDraftedEarlierSubmittedLaterIsNew(t *testing.T) {
	r := newFakeRig(t)
	draft, dc := r.f.SubmitReview("acme/w", r.n, "rev", "PENDING", "", ghfake.InlineComment{Body: "drafted first"})
	r.f.Flush()
	mark := len(r.f.Deliveries())
	_, qc := r.f.SubmitReview("acme/w", r.n, "rev2", "COMMENTED", "", ghfake.InlineComment{Body: "submitted first"})
	first := r.feed(r.deliveriesSince(mark))
	if len(first) != 1 {
		t.Fatalf("want 1 event, got %d", len(first))
	}
	if commentIDs(first[0])[dc[0]] {
		t.Fatal("a draft is visible to nobody: it cannot ride the first run")
	}
	mark = len(r.f.Deliveries())
	r.f.SubmitPending("acme/w", r.n, draft, "COMMENTED")
	second := r.feed(r.deliveriesSince(mark))
	if len(second) != 1 {
		t.Fatalf("want 1 event for the late submission, got %d", len(second))
	}
	if dc[0] >= qc[0] {
		t.Fatalf("rig: the draft's comment id %d must be below the other's %d", dc[0], qc[0])
	}
	c1, c2 := cursorOf(t, first[0]), cursorOf(t, second[0])
	if c2 <= c1 {
		t.Fatalf("the later submission's cursor %d must be above the earlier run's %d", c2, c1)
	}
	// A sweep recovering the same feedback (a restart lost what was held)
	// keys it on the same submissions: at the latest one, so above the
	// earlier run's mark.
	var swept []Trigger
	if err := r.g.sweep(context.Background(), func(_ context.Context, tr Trigger) {
		if tr.Kind == "changes_requested" {
			swept = append(swept, tr)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(swept) != 1 {
		t.Fatalf("sweep emitted %d changes_requested, want 1", len(swept))
	}
	if c := cursorOf(t, swept[0]); c != c2 {
		t.Fatalf("the sweep's cursor %d must be the latest submission's %d", c, c2)
	}
}

// When the PR's feedback can't be read, a review still dispatches on its
// own comments, keyed on its own submission.
func TestReviewEventWithoutTheFeedbackRead(t *testing.T) {
	r := newFakeRig(t)
	mark := len(r.f.Deliveries())
	r.f.SubmitReview("acme/w", r.n, "rev", "CHANGES_REQUESTED", "rework the cache")
	ds := r.deliveriesSince(mark)
	r.g.rest = nil // every REST/GraphQL read now fails
	got := r.feed(ds)
	if len(got) != 1 {
		t.Fatalf("want 1 changes_requested, got %d", len(got))
	}
	cursorOf(t, got[0])
}

// What is NOT waiting on you: a thread you answered last, a resolved thread,
// one opened by a reviewer who has since approved, and a changes-request made
// on an older commit than the head (you pushed since). A changes-request on
// the head is, with its summary.
func TestFoldLeavesOutWhatIsNotWaitingOnYou(t *testing.T) {
	r := newFakeRig(t)
	_, answered := r.f.SubmitReview("acme/w", r.n, "rev", "COMMENTED", "", ghfake.InlineComment{Body: "answered"})
	r.f.Reply("acme/w", r.n, answered[0], "me", "done")
	_, resolved := r.f.SubmitReview("acme/w", r.n, "rev", "COMMENTED", "", ghfake.InlineComment{Body: "resolved"})
	r.f.ResolveThread("acme/w", r.n, resolved[0], "me", true)
	_, signed := r.f.SubmitReview("acme/w", r.n, "rev2", "CHANGES_REQUESTED", "old ask", ghfake.InlineComment{Body: "approver's"})
	r.f.SubmitReview("acme/w", r.n, "rev2", "APPROVED", "fine")
	// rev3's latest review asks for changes, on a commit you have pushed past.
	r.f.SubmitReview("acme/w", r.n, "rev3", "CHANGES_REQUESTED", "stale ask")
	r.f.Push("acme/w", "feat", "me", "fix")
	cr, _ := r.f.SubmitReview("acme/w", r.n, "rev", "CHANGES_REQUESTED", "current ask")
	_, bot := r.f.SubmitReview("acme/w", r.n, "cursor[bot]", "COMMENTED", "", ghfake.InlineComment{Body: "bot finding"})
	r.f.Flush()

	var got []Trigger
	if err := r.g.sweep(context.Background(), func(_ context.Context, tr Trigger) {
		if tr.Kind == "changes_requested" {
			got = append(got, tr)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 changes_requested, got %d", len(got))
	}
	ids := commentIDs(got[0])
	for name, id := range map[string]int64{"answered": answered[0], "resolved": resolved[0], "approver's": signed[0]} {
		if ids[id] {
			t.Errorf("the %s thread is not waiting on you, but rides the run", name)
		}
	}
	if !ids[bot[0]] {
		t.Error("the bot's open finding is waiting on you")
	}
	rs, _ := got[0].Context["outstanding_reviews"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["review_id"] != cr {
		t.Fatalf("outstanding_reviews = %v, want only the changes-request on the head", rs)
	}
	if a := got[0].Context["author"]; a != "rev" {
		t.Fatalf("author = %v, want the human with changes outstanding", a)
	}
}
