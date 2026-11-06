package ghsource

import (
	"context"
	"testing"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit"
	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghfake"
)

// A review bot that puts every finding in its review body: submitted
// COMMENTED, a long summary, no inline comments.
const (
	reviewBot     = "review-bot[bot]"
	botReviewBody = "- [fixable] `bin/foo:41`: the loop never exits on EOF\n- [fixable] `bin/foo:57`: quote the path"
)

func newBodyRig(t *testing.T, actions map[string]Action) *fakeRig {
	t.Helper()
	r := newFakeRigWith(t, actions)
	r.f.AddUser(reviewBot, "Bot")
	return r
}

// feedbackEvents hands deliveries to the source and returns the
// new_comment and changes_requested events it emitted.
func (r *fakeRig) feedbackEvents(g *Source, from int) []Trigger {
	var out []Trigger
	for _, d := range r.deliveriesSince(from) {
		for _, tr := range g.Translate(context.Background(), d.Event, d.Body) {
			if feedbackKind(tr.Kind) {
				out = append(out, tr)
			}
		}
	}
	return out
}

// swept runs one sweep and returns the new_comment and changes_requested
// events it emitted.
func (r *fakeRig) swept(t *testing.T, g *Source) []Trigger {
	t.Helper()
	var out []Trigger
	if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) {
		if feedbackKind(tr.Kind) {
			out = append(out, tr)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func kinds(trs []Trigger) map[string]int {
	out := map[string]int{}
	for _, tr := range trs {
		out[tr.Kind]++
	}
	return out
}

// outstandingReview returns the outstanding_reviews entry for id, if any.
func outstandingReview(tr Trigger, id int64) (map[string]any, bool) {
	rs, _ := tr.Context["outstanding_reviews"].([]any)
	for _, r := range rs {
		if m, ok := r.(map[string]any); ok && m["review_id"] == id {
			return m, true
		}
	}
	return nil, false
}

// A body-only review no changes_requested trigger takes is ONE new_comment:
// the body is comment_body, the review_* fields are set, the bot is its
// author, and its comment_id is its submission key on its own stream.
func TestBodyOnlyReviewIsOneNewComment(t *testing.T) {
	r := newBodyRig(t, map[string]Action{"new_comment": {}})
	mark := len(r.f.Deliveries())
	id, _ := r.f.SubmitReview("acme/w", r.n, reviewBot, "COMMENTED", botReviewBody)
	got := r.feedbackEvents(r.g, mark)
	if len(got) != 1 || got[0].Kind != "new_comment" {
		t.Fatalf("want one new_comment, got %v", kinds(got))
	}
	c := got[0].Context
	if c["comment_body"] != botReviewBody || c["review_body"] != botReviewBody {
		t.Fatalf("comment_body = %q, review_body = %q, want the review body", c["comment_body"], c["review_body"])
	}
	if c["review_id"] != id || c["review_state"] != "commented" {
		t.Fatalf("review_id = %v, review_state = %v", c["review_id"], c["review_state"])
	}
	if c["author"] != reviewBot || c["author_is_bot"] != true {
		t.Fatalf("author = %v, author_is_bot = %v", c["author"], c["author_is_bot"])
	}
	if c["comment_kind"] != CommentKindReviewSubmission {
		t.Fatalf("comment_kind = %v, want %q", c["comment_kind"], CommentKindReviewSubmission)
	}
	var want int64
	for _, v := range r.f.PR("acme/w", r.n).Reviews {
		if v.ID == id {
			want = githubkit.SubmissionKey(v.SubmittedAt, id)
		}
	}
	if k, _ := c["comment_id"].(int64); k <= 0 || k != want {
		t.Fatalf("comment_id = %v, want the review's submission key %d", c["comment_id"], want)
	}
	subs, _ := c["reaction_subjects"].([]any)
	if len(subs) != 1 || subs[0].(map[string]any)["id"] != id {
		t.Fatalf("reaction_subjects = %v, want the review", subs)
	}
}

// With a changes_requested trigger that takes it, a body-only review is ONE
// changes_requested whose outstanding_reviews carries its body; until a push
// addresses it, every sweep re-derives it at the same cursor (the engine
// dispatches it once), and after a push it is not outstanding.
func TestBodyOnlyReviewIsOneChangesRequested(t *testing.T) {
	r := newBodyRig(t, map[string]Action{"changes_requested": {}, "new_comment": {}})
	mark := len(r.f.Deliveries())
	id, _ := r.f.SubmitReview("acme/w", r.n, reviewBot, "COMMENTED", botReviewBody)
	got := r.feedbackEvents(r.g, mark)
	if len(got) != 1 || got[0].Kind != "changes_requested" {
		t.Fatalf("want one changes_requested, got %v", kinds(got))
	}
	m, ok := outstandingReview(got[0], id)
	if !ok || m["body"] != botReviewBody || m["author"] != reviewBot {
		t.Fatalf("outstanding_reviews = %v, want review %d with its body", got[0].Context["outstanding_reviews"], id)
	}
	if got[0].Context["review_state"] != "commented" || got[0].Context["review_body"] != botReviewBody {
		t.Fatalf("review_state = %v, review_body = %v", got[0].Context["review_state"], got[0].Context["review_body"])
	}
	cursor := cursorOf(t, got[0])

	sw := r.swept(t, r.g)
	if len(sw) != 1 || sw[0].Kind != "changes_requested" {
		t.Fatalf("sweep: want one changes_requested, got %v", kinds(sw))
	}
	if _, ok := outstandingReview(sw[0], id); !ok {
		t.Fatalf("sweep: the body-only review is outstanding until addressed: %v", sw[0].Context["outstanding_reviews"])
	}
	if c := cursorOf(t, sw[0]); c != cursor {
		t.Fatalf("sweep cursor %d != the webhook run's %d: the engine would run it again", c, cursor)
	}
	if a := sw[0].Context["author"]; a != reviewBot {
		t.Fatalf("sweep: author = %v, want the reviewer", a)
	}

	r.f.Push("acme/w", "feat", "me", "fix the findings")
	if sw := r.swept(t, r.g); len(sw) != 0 {
		t.Fatalf("after a push the review is addressed; sweep emitted %v", kinds(sw))
	}
}

// What makes a body-only review outstanding, as for a changes-request: the
// reviewer's latest review with a body, on the head, not self, reviewer not
// approved. A reply in a thread (a bodiless COMMENTED review) doesn't
// supersede it; a later summary does.
func TestBodyOnlyReviewOutstandingRules(t *testing.T) {
	r := newBodyRig(t, map[string]Action{"changes_requested": {}})
	sweepReviews := func() map[int64]bool {
		t.Helper()
		sw := r.swept(t, r.g)
		out := map[int64]bool{}
		for _, tr := range sw {
			rs, _ := tr.Context["outstanding_reviews"].([]any)
			for _, x := range rs {
				out[x.(map[string]any)["review_id"].(int64)] = true
			}
		}
		return out
	}
	first, _ := r.f.SubmitReview("acme/w", r.n, "rev", "COMMENTED", "please also rename Foo")
	_, inline := r.f.SubmitReview("acme/w", r.n, "rev2", "COMMENTED", "", ghfake.InlineComment{Body: "a finding"})
	r.f.Reply("acme/w", r.n, inline[0], "rev", "agreed with rev2")
	approver, _ := r.f.SubmitReview("acme/w", r.n, "rev3", "COMMENTED", "consider caching this")
	r.f.SubmitReview("acme/w", r.n, "rev3", "APPROVED", "")
	r.f.SubmitReview("acme/w", r.n, "rev3", "APPROVED", "")
	r.f.Flush()
	got := sweepReviews()
	if !got[first] {
		t.Fatalf("rev's body-only review is outstanding (a thread reply doesn't supersede it): %v", got)
	}
	if got[approver] {
		t.Fatalf("rev3 approved: their body-only review is not outstanding: %v", got)
	}
	second, _ := r.f.SubmitReview("acme/w", r.n, "rev", "COMMENTED", "and the docs")
	r.f.Flush()
	got = sweepReviews()
	if got[first] || !got[second] {
		t.Fatalf("rev's later summary supersedes the first: %v", got)
	}
}

// The bot filters apply to the reviewer: a new_comment trigger for humans
// only drops the bot's body-only review and keeps a human's; a
// changes_requested trigger for humans only doesn't take the bot's review,
// which is then its new_comment.
func TestBodyOnlyReviewBotFilters(t *testing.T) {
	no := false
	r := newBodyRig(t, map[string]Action{"new_comment": {AuthorBot: &no}})
	mark := len(r.f.Deliveries())
	r.f.SubmitReview("acme/w", r.n, reviewBot, "COMMENTED", botReviewBody)
	if got := r.feedbackEvents(r.g, mark); len(got) != 0 {
		t.Fatalf("a humans-only new_comment fired for a bot's review: %v", kinds(got))
	}
	mark = len(r.f.Deliveries())
	r.f.SubmitReview("acme/w", r.n, "rev", "COMMENTED", "please split this function")
	if got := r.feedbackEvents(r.g, mark); len(got) != 1 || got[0].Context["author"] != "rev" {
		t.Fatalf("want one new_comment for the human's review, got %v", kinds(got))
	}

	r2 := newBodyRig(t, map[string]Action{"changes_requested": {AuthorBot: &no}, "new_comment": {}})
	mark = len(r2.f.Deliveries())
	r2.f.SubmitReview("acme/w", r2.n, reviewBot, "COMMENTED", botReviewBody)
	got := r2.feedbackEvents(r2.g, mark)
	if len(got) != 1 || got[0].Kind != "new_comment" {
		t.Fatalf("a humans-only changes_requested doesn't take the bot's review: want one new_comment, got %v", kinds(got))
	}
}

// The sweep recovers a body-only review whose webhook was missed, as the
// same event; whichever of webhook and sweep comes first, the other emits
// nothing; and past a restart the sweep re-derives it at the same
// comment_id, which the engine's mark then drops.
func TestSweepRecoversABodyOnlyReview(t *testing.T) {
	r := newBodyRig(t, map[string]Action{"new_comment": {}})
	mark := len(r.f.Deliveries())
	id, _ := r.f.SubmitReview("acme/w", r.n, reviewBot, "COMMENTED", botReviewBody)
	r.f.Flush()

	// Missed webhook: the sweep recovers it.
	sw := r.swept(t, r.g)
	if len(sw) != 1 || sw[0].Kind != "new_comment" {
		t.Fatalf("sweep: want one new_comment, got %v", kinds(sw))
	}
	c := sw[0].Context
	if c["review_id"] != id || c["comment_body"] != botReviewBody || c["author_is_bot"] != true ||
		c["comment_kind"] != CommentKindReviewSubmission {
		t.Fatalf("sweep: wrong event context %v", c)
	}
	recovered, _ := c["comment_id"].(int64)
	// The webhook arriving late, and the next sweep, emit nothing.
	if got := r.feedbackEvents(r.g, mark); len(got) != 0 {
		t.Fatalf("webhook after the sweep recovered the review: %v", kinds(got))
	}
	if sw := r.swept(t, r.g); len(sw) != 0 {
		t.Fatalf("second sweep: %v", kinds(sw))
	}

	// The other order: webhook first, then the sweep.
	mark = len(r.f.Deliveries())
	id2, _ := r.f.SubmitReview("acme/w", r.n, "rev", "COMMENTED", "rename Foo")
	if got := r.feedbackEvents(r.g, mark); len(got) != 1 {
		t.Fatalf("webhook: want one new_comment, got %v", kinds(got))
	}
	if sw := r.swept(t, r.g); len(sw) != 0 {
		t.Fatalf("sweep after the webhook emitted the review: %v", kinds(sw))
	}

	// A restart forgets the claims; the sweep re-derives both at the
	// comment_id each was dispatched on.
	re := r.swept(t, r.source(t, map[string]Action{"new_comment": {}}))
	if len(re) != 2 {
		t.Fatalf("after a restart want both re-derived, got %v", kinds(re))
	}
	for _, tr := range re {
		if tr.Context["review_id"] == id && tr.Context["comment_id"] != recovered {
			t.Fatalf("re-derived comment_id %v != dispatched %d", tr.Context["comment_id"], recovered)
		}
		if tr.Context["review_id"] != id && tr.Context["review_id"] != id2 {
			t.Fatalf("unexpected review %v", tr.Context["review_id"])
		}
	}
}

// An approval with only a body is not an event — not by webhook, not by
// sweep, with or without a changes_requested trigger.
func TestBodyOnlyApprovalIsNotAnEvent(t *testing.T) {
	for _, actions := range []map[string]Action{
		{"new_comment": {}},
		{"changes_requested": {}, "new_comment": {}},
	} {
		r := newBodyRig(t, actions)
		mark := len(r.f.Deliveries())
		r.f.SubmitReview("acme/w", r.n, "rev", "APPROVED", "looks good, ship it")
		if got := r.feedbackEvents(r.g, mark); len(got) != 0 {
			t.Fatalf("%v: webhook: an approval's body is not an event: %v", actions, kinds(got))
		}
		if sw := r.swept(t, r.g); len(sw) != 0 {
			t.Fatalf("%v: sweep: an approval's body is not an event: %v", actions, kinds(sw))
		}
	}
}
