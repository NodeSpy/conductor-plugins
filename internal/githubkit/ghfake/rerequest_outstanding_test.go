package ghfake

import (
	"context"
	"reflect"
	"testing"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit"
)

// The pack re-requests "{{.author}}" — the event's reviewer. When that review
// was only COMMENTED, the verb skipped them ("not requested changes") while a
// DIFFERENT reviewer's changes-request on an older commit stayed outstanding
// and kept blocking the merge. all_outstanding pings every reviewer whose
// latest review still requests changes, through the same checks: never a bot,
// the PR's author, an approver, or someone already requested.
func TestRerequestAllOutstanding(t *testing.T) {
	f, _, n := world(t, Options{})
	for _, u := range []string{"blocker", "commenter", "approver", "waiting"} {
		f.AddUser(u, "User")
		f.AddCollaborator("acme/w", u)
	}
	f.SubmitReview("acme/w", n, "blocker", "CHANGES_REQUESTED", "fix the leak")
	f.SubmitReview("acme/w", n, "approver", "CHANGES_REQUESTED", "nope")
	f.SubmitReview("acme/w", n, "approver", "APPROVED", "ok now")
	f.SubmitReview("acme/w", n, "waiting", "CHANGES_REQUESTED", "hmm")
	f.SubmitReview("acme/w", n, "cursor[bot]", "CHANGES_REQUESTED", "bot finding")
	f.Push("acme/w", "feat", "me", "fix")
	f.RequestReview("acme/w", n, "me", "waiting")
	f.SubmitReview("acme/w", n, "commenter", "COMMENTED", "one more thing", InlineComment{Body: "rename this"})
	f.Flush()

	kit, err := githubkit.NewClient(githubkit.Config{WriteToken: "tok-me", APIBase: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	out, err := kit.Invoke(context.Background(), "rerequest_review", map[string]any{
		"repo": "acme/w", "pr": n, "reviewers": []any{"commenter"}, "only_outstanding": true, "all_outstanding": true})
	if err != nil {
		t.Fatal(err)
	}
	if out["skipped"] != nil {
		t.Fatalf("skipped %v: the blocker's changes-request is outstanding", out["skipped"])
	}
	if got, want := f.RequestedReviewers("acme/w", n), []string{"blocker", "waiting"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requested reviewers = %v, want %v (the blocker added; already-requested kept; no approver, commenter, bot or author)", got, want)
	}
	if c := f.ReviewRequestsOf("acme/w", n, "blocker"); c != 1 {
		t.Fatalf("the blocker was requested %d times, want 1", c)
	}
	if c := f.ReviewRequestsOf("acme/w", n, "waiting"); c != 1 {
		t.Fatalf("an already-requested reviewer was requested again (%d deliveries)", c)
	}

	// Without all_outstanding the old behavior holds: only the named reviewer
	// is considered, and a commenter has nothing outstanding.
	out, err = kit.Invoke(context.Background(), "rerequest_review", map[string]any{
		"repo": "acme/w", "pr": n, "reviewers": []any{"commenter"}})
	if err != nil {
		t.Fatal(err)
	}
	if out["skipped"] != "commenter not requested changes" {
		t.Fatalf("skipped = %v", out["skipped"])
	}

	// Nobody outstanding at all: skip, never fail the flow.
	f.SubmitReview("acme/w", n, "blocker", "APPROVED", "")
	out, err = kit.Invoke(context.Background(), "rerequest_review", map[string]any{
		"repo": "acme/w", "pr": n, "all_outstanding": true})
	if err != nil {
		t.Fatal(err)
	}
	if out["skipped"] == nil {
		t.Fatalf("want a skip with nothing new to request, got %v", out)
	}
}

// A reviewer's latest verdict can be past the first page of reviews (every
// thread reply is a review of its own). Reading one page took an approval on
// page two for the changes-request on page one, and re-requested an approver.
func TestRerequestReadsEveryPageOfReviews(t *testing.T) {
	f, _, n := world(t, Options{})
	f.AddUser("blocker", "User")
	f.AddCollaborator("acme/w", "blocker")
	f.SubmitReview("acme/w", n, "blocker", "CHANGES_REQUESTED", "fix the leak")
	for i := 0; i < 105; i++ {
		f.SubmitReview("acme/w", n, "rev", "COMMENTED", "noise")
	}
	f.Push("acme/w", "feat", "me", "fix")
	f.SubmitReview("acme/w", n, "blocker", "APPROVED", "ok")
	f.Flush()
	kit, err := githubkit.NewClient(githubkit.Config{WriteToken: "tok-me", APIBase: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []map[string]any{
		{"repo": "acme/w", "pr": n, "all_outstanding": true},
		{"repo": "acme/w", "pr": n, "reviewers": []any{"blocker"}},
	} {
		if _, err := kit.Invoke(context.Background(), "rerequest_review", opts); err != nil {
			t.Fatal(err)
		}
	}
	if c := f.ReviewRequestsOf("acme/w", n, "blocker"); c != 0 {
		t.Fatalf("the blocker approved (on the second page of reviews) and was re-requested %d times", c)
	}
}
