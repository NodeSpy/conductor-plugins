package ghfake

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit"
)

// steppingClock advances one second per reading, so every submission lands
// in its own second.
func steppingClock() func() time.Time {
	var mu sync.Mutex
	t := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(time.Second)
		return t
	}
}

func reviewKey(t *testing.T, f *Fake, repo string, n int, reviewID int64) int64 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	_, p := f.mustPull(repo, n)
	for _, rv := range p.Reviews {
		if rv.ID == reviewID {
			return githubkit.SubmissionKey(rv.SubmittedAt, rv.ID)
		}
	}
	t.Fatalf("no review %d", reviewID)
	return 0
}

// resolve_threads resolves exactly the threads a run says it addressed:
// named by any comment in them, on this PR, with no feedback from someone
// else since the run's cursor. A thread the run did not name (it only argued
// there) stays open, as does one the reviewer has commented on since.
func TestResolveThreadsResolvesOnlyWhatTheRunAddressed(t *testing.T) {
	f, _, n := world(t, Options{Now: steppingClock()})
	rv, cs := f.SubmitReview("acme/w", n, "rev", "CHANGES_REQUESTED", "a few things",
		InlineComment{Body: "fix A"}, InlineComment{Body: "fix B"}, InlineComment{Body: "fix C"}, InlineComment{Body: "fix D"})
	cursor := reviewKey(t, f, "acme/w", n, rv)
	// While the run worked: it replied on A; the reviewer added to B.
	reA := f.Reply("acme/w", n, cs[0], "me", "done")
	f.Reply("acme/w", n, cs[1], "rev", "and also rename it")
	// D is resolved by hand already.
	f.ResolveThread("acme/w", n, cs[3], "rev", true)
	// Another PR's thread, named by mistake.
	n2 := f.OpenPR("acme/w", "me", PROpts{Title: "other", Head: "other"})
	_, other := f.SubmitReview("acme/w", n2, "rev", "COMMENTED", "", InlineComment{Body: "elsewhere"})
	f.Flush()

	kit, err := githubkit.NewClient(githubkit.Config{WriteToken: "tok-me", APIBase: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	out, err := kit.Invoke(context.Background(), "resolve_threads", map[string]any{
		"repo": "acme/w", "pr": n, "seen_through": cursor,
		// A by its reply's id, B by its opener, D already resolved, the other
		// PR's comment. C is not named: the run only argued there.
		"comments": []any{reA, cs[1], float64(cs[3]), other[0]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.ThreadResolved("acme/w", n, cs[0]) {
		t.Fatal("A was addressed and named: it must be resolved")
	}
	if f.ThreadResolved("acme/w", n, cs[1]) {
		t.Fatal("B has new feedback the run never saw: it must stay open")
	}
	if f.ThreadResolved("acme/w", n, cs[2]) {
		t.Fatal("C was not named: it must stay open")
	}
	if !f.ThreadResolved("acme/w", n, cs[3]) {
		t.Fatal("D was already resolved")
	}
	if f.ThreadResolved("acme/w", n2, other[0]) {
		t.Fatal("a thread on another PR must never be resolved")
	}
	if got := len(out["resolved"].([]any)); got != 2 {
		t.Fatalf("resolved = %v, want A and D", out["resolved"])
	}
	if out["skipped"] == nil {
		t.Fatal("the skips (new feedback on B, a comment not on this PR) must be reported")
	}

	// Without seen_through, B's newer comment is not a reason to keep it.
	if _, err := kit.Invoke(context.Background(), "resolve_threads", map[string]any{
		"repo": "acme/w", "pr": n, "comments": "[" + itoa64(cs[1]) + "]"}); err != nil {
		t.Fatal(err)
	}
	if !f.ThreadResolved("acme/w", n, cs[1]) {
		t.Fatal("a rendered list string must name B too")
	}

	// Nothing named: a no-op, never a failure.
	out, err = kit.Invoke(context.Background(), "resolve_threads", map[string]any{"repo": "acme/w", "pr": n})
	if err != nil || out["skipped"] != "no threads named" {
		t.Fatalf("got %v, %v", out, err)
	}
}
