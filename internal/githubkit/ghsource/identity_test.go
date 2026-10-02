package ghsource

import (
	"testing"
)

func identityConfig(me, reviewer []string) Config {
	return Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Rules: []Rule{{
			Match:    Match{Repos: []string{"acme/*"}},
			Me:       Actors{Logins: me},
			Reviewer: Actors{Logins: reviewer},
			Actions: as1(map[string]Action{
				"new_comment": {},
				"self_review": {},
			}),
		}},
	}
}

// comment models a comment by `login` on a PR you authored (issue.user = "me"), so
// the me-authored gate on new_comment passes and only the commenter identity varies.
func comment(login string) string {
	return `{"action":"created","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"me"}},"comment":{"id":1,"user":{"login":"` + login + `"}}}`
}

func prOpenedBy(login string) string {
	return `{"action":"opened","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"},"user":{"login":"` + login + `"}}}`
}

func TestIdentityUsesMeNotReviewer(t *testing.T) {
	// me = "me"; reviewer = "teammate" (a different person).
	g := newTestIntegration(t, identityConfig([]string{"me"}, []string{"teammate"}))

	// A comment by the reviewer is NOT self → new_comment fires.
	if k := do(t, g, "issue_comment", comment("teammate")); len(k) != 1 || k[0] != "new_comment" {
		t.Fatalf("reviewer is not 'you' — comment should trigger, got %v", k)
	}
	// A comment by you is filtered.
	if k := do(t, g, "issue_comment", comment("me")); len(k) != 0 {
		t.Fatalf("your own comment should be ignored, got %v", k)
	}
	// self_review only on your own PR.
	if k := do(t, g, "pull_request", prOpenedBy("me")); len(k) == 0 || !has(k, "self_review") {
		t.Fatalf("self_review should fire on your PR, got %v", k)
	}
	if k := do(t, g, "pull_request", prOpenedBy("teammate")); has(k, "self_review") {
		t.Fatalf("self_review should NOT fire on a reviewer's PR, got %v", k)
	}
}

func TestIdentityFallsBackToReviewer(t *testing.T) {
	// No `me:` set → fall back to reviewer/assignee logins.
	g := newTestIntegration(t, identityConfig(nil, []string{"me"}))
	if k := do(t, g, "issue_comment", comment("me")); len(k) != 0 {
		t.Fatalf("fallback: reviewer login should count as you, got %v", k)
	}
	if k := do(t, g, "pull_request", prOpenedBy("me")); !has(k, "self_review") {
		t.Fatalf("fallback: self_review should fire on your PR, got %v", k)
	}
}

func TestActionLevelActors(t *testing.T) {
	// reviewer lives on review_requested; assignee on issue_matched. No rule-level actors.
	cfg := Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Rules: []Rule{{
			Match: Match{Repos: []string{"acme/*"}},
			Actions: as1(map[string]Action{
				"review_requested": {
					Reviewer: Actors{Logins: []string{"me"}}},
				"issue_matched": {
					Assignee: Actors{Logins: []string{"me"}}},
			}),
		}},
	}
	g := newTestIntegration(t, cfg)

	rr := func(login string) string {
		return `{"action":"review_requested","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
			"pull_request":{"number":6,"head":{"sha":"h"}},"requested_reviewer":{"login":"` + login + `"}}`
	}
	if k := do(t, g, "pull_request", rr("me")); !has(k, "review_requested") {
		t.Fatalf("action-level reviewer should match, got %v", k)
	}
	if k := do(t, g, "pull_request", rr("other")); has(k, "review_requested") {
		t.Fatalf("non-matching reviewer should not fire, got %v", k)
	}

	ia := func(login string) string {
		return `{"action":"assigned","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
			"issue":{"number":9,"assignees":[{"login":"` + login + `"}]}}`
	}
	if k := do(t, g, "issues", ia("me")); !has(k, "issue_matched") {
		t.Fatalf("action-level assignee should match, got %v", k)
	}
	if k := do(t, g, "issues", ia("other")); has(k, "issue_matched") {
		t.Fatalf("non-matching assignee should not fire, got %v", k)
	}
}

func TestActorsDefaultToMe(t *testing.T) {
	// Only `me` is set — reviewer/assignee are NOT specified on the actions.
	cfg := Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Rules: []Rule{{
			Match: Match{Repos: []string{"acme/*"}},
			Me:    Actors{Logins: []string{"me"}},
			Actions: as1(map[string]Action{
				"review_requested": {},
				"issue_matched":    {},
			}),
		}},
	}
	g := newTestIntegration(t, cfg)

	rr := func(login string) string {
		return `{"action":"review_requested","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
			"pull_request":{"number":6,"head":{"sha":"h"}},"requested_reviewer":{"login":"` + login + `"}}`
	}
	if k := do(t, g, "pull_request", rr("me")); !has(k, "review_requested") {
		t.Fatalf("review_requested should default to matching you, got %v", k)
	}
	if k := do(t, g, "pull_request", rr("someone")); has(k, "review_requested") {
		t.Fatalf("review_requested should not fire for someone else, got %v", k)
	}

	ia := func(login string) string {
		return `{"action":"assigned","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
			"issue":{"number":9,"assignees":[{"login":"` + login + `"}]}}`
	}
	if k := do(t, g, "issues", ia("me")); !has(k, "issue_matched") {
		t.Fatalf("issue_matched should default to matching you, got %v", k)
	}
	if k := do(t, g, "issues", ia("someone")); has(k, "issue_matched") {
		t.Fatalf("issue_matched should not fire for someone else, got %v", k)
	}
}

// TestAutopilotOnlyOwnPRs pins the fix for the live bug where new_comment (and the
// other fix-and-push kinds) fired on PRs authored by someone else — the handlers
// ignored our own *comments* but never checked we authored the *PR*. Autopilot pushes
// to the PR branch, so it must only act on PRs the `me` identity authored, regardless
// of who left the comment / requested changes.
func TestAutopilotOnlyOwnPRs(t *testing.T) {
	g := newTestIntegration(t, richConfig()) // self = "me"

	// issue_comment on someone else's PR (issue.user = "teammate") → no new_comment,
	// even though the commenter ("bot") isn't us.
	otherIssue := `{"action":"created","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"teammate"}},"comment":{"id":1,"user":{"login":"bot"},"body":"fix"}}`
	if k := do(t, g, "issue_comment", otherIssue); len(k) != 0 {
		t.Fatalf("new_comment must not fire on a teammate's PR, got %v", k)
	}
	// Same comment on our own PR → fires.
	ownIssue := `{"action":"created","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"me"}},"comment":{"id":1,"user":{"login":"bot"},"body":"fix"}}`
	if k := do(t, g, "issue_comment", ownIssue); len(k) != 1 || k[0] != "new_comment" {
		t.Fatalf("new_comment should fire on our own PR, got %v", k)
	}

	// review-comment on someone else's PR → no new_comment.
	otherReview := `{"action":"created","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"},"user":{"login":"teammate"}},"comment":{"id":2,"user":{"login":"bot"},"body":"nit"}}`
	if k := do(t, g, "pull_request_review_comment", otherReview); len(k) != 0 {
		t.Fatalf("new_comment must not fire on a teammate's PR review comment, got %v", k)
	}

	// changes_requested on someone else's PR → no trigger.
	otherCR := `{"action":"submitted","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":6,"head":{"sha":"h"},"user":{"login":"teammate"}},"review":{"state":"changes_requested","id":9,"user":{"login":"bot"}}}`
	if k := do(t, g, "pull_request_review", otherCR); has(k, "changes_requested") {
		t.Fatalf("changes_requested must not fire on a teammate's PR, got %v", k)
	}
}

func has(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
