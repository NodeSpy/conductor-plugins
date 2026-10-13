package ghsource

import (
	"testing"

	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// This is the ACCEPTANCE test for the unified `filter:`
// (docs/design/unified-filter.md §Legacy lowering): every representative
// legacy `filters:` block, evaluated against the same event facts, must yield
// the same keep/skip decision through the lowered Filter IR as it did through
// the inline matchers.
//
// The "want" side is never a hand-written expectation — it CALLS the
// pre-change predicate (Exclude.Matches, draftGate, mergeGatePasses,
// commentAuthorAllowed, authorBotMatch, anyFold/allFold/containsFold), which
// is why those functions are kept even though the keep-conditions no longer
// call them. Drift in the lowering fails here; so does "fixing" the oracle to
// match a broken lowering, because the oracle is the shipped legacy code.

func testIntegration(selves ...string) *Source {
	self := map[string]bool{}
	for _, s := range selves {
		self[s] = true
	}
	return &Source{name: "parity", self: self}
}

// prCase is one PR the legacy and lowered paths are both asked about.
type prCase struct {
	name                      string
	head, base, title, author string
	labels                    []string
	draft                     bool
}

var prCases = []prCase{
	{name: "plain feature PR", head: "feature/x", base: "main", title: "add a thing", author: "alice"},
	{name: "draft feature PR", head: "feature/x", base: "main", title: "add a thing", author: "alice", draft: true},
	{name: "release PR on staging", head: "staging", base: "main", title: "Release 2.4.0", author: "bot", labels: []string{"release"}},
	{name: "release PR on release branch", head: "release/2.4", base: "main", title: "Release 2.4.0", author: "bot"},
	// RosterStream#5590: a changelog PR whose TITLE contains "release" as an
	// ordinary word, on a branch that is not a release branch.
	{name: "#5590 changelog PR", head: "codex-changelog", base: "main",
		title: "changelog: publish each release entry as its own file", author: "alice"},
	{name: "labelled urgent", head: "hotfix/a", base: "main", title: "fix prod", author: "carol", labels: []string{"urgent", "Bug"}},
	{name: "empty everything", head: "", base: "", title: "", author: ""},
	{name: "case-varied label", head: "x", base: "main", title: "T", author: "d", labels: []string{"URGENT"}},
}

// legacyActions are the representative legacy `filters:` blocks, already
// lowered to Action the way internal/connector.lowerTrigger does.
func legacyActions() map[string]Action {
	yes, no := true, false
	return map[string]Action{
		"no filters at all": {},
		"exclude branches only": {
			Exclude: Exclude{Branches: []string{"staging", "prod", "release/*"}},
		},
		"exclude title only (the #5590 substring footgun)": {
			Exclude: Exclude{Title: []string{"Release "}},
		},
		"exclude labels only": {
			Exclude: Exclude{Labels: []string{"urgent"}},
		},
		"exclude all three arms (an OR denylist)": {
			Exclude: Exclude{
				Branches: []string{"staging", "prod"},
				Labels:   []string{"release"},
				Title:    []string{"Release "},
			},
		},
		"exclude with an empty-string title entry": {
			Exclude: Exclude{Title: []string{""}},
		},
		"exclude with a catch-all branch glob": {
			Exclude: Exclude{Branches: []string{"*"}},
		},
		"not_draft gate on": {
			Gates: map[string]any{"not_draft": true},
		},
		"not_draft gate explicitly off": {
			Gates: map[string]any{"not_draft": false},
		},
		"not_draft gate as the string \"true\"": {
			Gates: map[string]any{"not_draft": "true"},
		},
		"not_draft gate as the string \"no\"": {
			Gates: map[string]any{"not_draft": "no"},
		},
		"an unrelated gate key only": {
			Gates: map[string]any{"merge_state": true},
		},
		"the #5590 config: exclude branches+title AND not_draft": {
			Exclude: Exclude{Branches: []string{"staging", "prod"}, Title: []string{"Release "}},
			Gates:   map[string]any{"not_draft": true},
		},
		"labels_any": {LabelsAny: []string{"urgent", "security"}},
		"labels_all": {LabelsAll: []string{"urgent", "bug"}},
		"labels_any + labels_all": {
			LabelsAny: []string{"urgent"}, LabelsAll: []string{"urgent", "bug"},
		},
		"authors allowlist": {Authors: []string{"alice", "Bot"}},
		"sole_assignee":     {SoleAssignee: true},
		"require_label":     {RequireLabel: "ship-it"},
		"from_users":        {FromUsers: []string{"reviewer1"}},
		"ignore_users":      {IgnoreUsers: []string{"github-actions[bot]"}},
		"from_users + ignore_users overlapping (ignore wins)": {
			FromUsers: []string{"github-actions[bot]", "reviewer1"}, IgnoreUsers: []string{"github-actions[bot]"},
		},
		"author_bot true":                        {AuthorBot: &yes},
		"author_bot false":                       {AuthorBot: &no},
		"merge gates all default (all enforced)": {},
		"merge gate threads_resolved relaxed": {
			Gates: map[string]any{"threads_resolved": false},
		},
		"merge gates several relaxed": {
			Gates: map[string]any{"review_decision": false, "non_author_approval": false, "merge_state": "no"},
		},
		"require_label + a relaxed merge gate": {
			RequireLabel: "ship-it", Gates: map[string]any{"threads_resolved": false},
		},
		"the kitchen sink": {
			Exclude:   Exclude{Branches: []string{"staging"}, Labels: []string{"wip"}, Title: []string{"WIP"}},
			Gates:     map[string]any{"not_draft": true},
			LabelsAny: []string{"urgent", "bug"},
			LabelsAll: []string{"urgent"},
			Authors:   []string{"alice"},
		},
	}
}

// TestLegacyParityReviewRequested covers sweepReviewRequested and the
// review_requested webhook path: `!draftGate && !Exclude.Matches`.
func TestLegacyParityReviewRequested(t *testing.T) {
	g := testIntegration()
	for name, act := range legacyActions() {
		for _, pr := range prCases {
			want := !draftGate(act, pr.draft) && !act.Exclude.Matches(pr.head, pr.title, pr.labels)
			facts := prFilterFacts(pr.head, pr.base, pr.title, pr.author, pr.labels, pr.draft)
			got := g.filterPasses(act, "test", "o/r", facts, lowerReviewRequested(act))
			if got != want {
				t.Errorf("review_requested [%s] on %q: lowered=%v legacy=%v\n  filter: %s\n  facts:  %+v",
					name, pr.name, got, want, lowerReviewRequested(act), facts)
			}
		}
	}
}

// TestLegacyParityReadyReview covers readyReviewTriggers, which checks the
// exclude list but deliberately NOT the not_draft gate.
func TestLegacyParityReadyReview(t *testing.T) {
	g := testIntegration()
	for name, act := range legacyActions() {
		for _, pr := range prCases {
			want := !act.Exclude.Matches(pr.head, pr.title, pr.labels)
			facts := prFilterFacts(pr.head, pr.base, pr.title, pr.author, pr.labels, pr.draft)
			got := g.filterPasses(act, "test", "o/r", facts, lowerReadyReview(act))
			if got != want {
				t.Errorf("ready_for_review [%s] on %q: lowered=%v legacy=%v\n  filter: %s",
					name, pr.name, got, want, lowerReadyReview(act))
			}
		}
	}
}

// issueCases are the issue states cheapMatch is asked about.
var issueCases = []struct {
	name string
	st   issueMatchState
}{
	{"assigned to you alone", issueMatchState{title: "billing is broken", author: "alice",
		labels: []string{"urgent", "bug"}, assignees: []string{"me"}}},
	{"assigned to you and someone else", issueMatchState{title: "billing is broken", author: "alice",
		labels: []string{"urgent", "bug"}, assignees: []string{"me", "bob"}}},
	{"assigned to someone else", issueMatchState{title: "billing", author: "bob",
		labels: []string{"chore"}, assignees: []string{"bob"}}},
	{"unassigned", issueMatchState{title: "Release notes", author: "bot", labels: nil}},
	{"one matching label only", issueMatchState{title: "x", author: "alice",
		labels: []string{"urgent"}, assignees: []string{"me"}}},
	{"empty issue", issueMatchState{}},
}

// TestLegacyParityIssueMatch covers cheapMatch's sole_assignee / labels_any /
// labels_all / exclude / authors conjuncts. The `assignee` check stays outside
// the filter (it resolves against the `me` identity, not a fact), so it is
// excluded from both sides here.
func TestLegacyParityIssueMatch(t *testing.T) {
	g := testIntegration("me")
	for name, act := range legacyActions() {
		for _, ic := range issueCases {
			st := ic.st
			// The pre-change body of cheapMatch, minus the assignee check.
			want := true
			switch {
			case act.SoleAssignee && !g.soleSelf(st.assignees):
				want = false
			case len(act.LabelsAny) > 0 && !anyFold(st.labels, act.LabelsAny):
				want = false
			case len(act.LabelsAll) > 0 && !allFold(st.labels, act.LabelsAll):
				want = false
			case act.Exclude.Matches("", st.title, st.labels):
				want = false
			case len(act.Authors) > 0 && !containsFold(act.Authors, st.author):
				want = false
			}
			facts := issueFilterFacts(st, g.soleSelf(st.assignees))
			got := g.filterPasses(act, "test", "o/r", facts, lowerIssueMatch(act))
			if got != want {
				t.Errorf("issue_matched [%s] on %q: lowered=%v legacy=%v\n  filter: %s\n  facts:  %+v",
					name, ic.name, got, want, lowerIssueMatch(act), facts)
			}
		}
	}
}

var gateCases = []struct {
	name string
	gate mergeGate
}{
	{"all green", mergeGate{MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
		ThreadsResolved: true, NonAuthorApprove: true, Author: "me", Labels: []string{"ship-it"}}},
	{"green but no ship-it label", mergeGate{MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
		ThreadsResolved: true, NonAuthorApprove: true, Author: "me"}},
	{"draft", mergeGate{IsDraft: true, MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
		ThreadsResolved: true, NonAuthorApprove: true, Labels: []string{"ship-it"}}},
	{"blocked merge state", mergeGate{MergeStateStatus: "BLOCKED", ReviewDecision: "APPROVED",
		ThreadsResolved: true, NonAuthorApprove: true, Labels: []string{"ship-it"}}},
	{"changes requested", mergeGate{MergeStateStatus: "CLEAN", ReviewDecision: "CHANGES_REQUESTED",
		ThreadsResolved: true, NonAuthorApprove: true, Labels: []string{"ship-it"}}},
	{"threads unresolved", mergeGate{MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
		NonAuthorApprove: true, Labels: []string{"ship-it"}}},
	{"self-approved only", mergeGate{MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
		ThreadsResolved: true, Labels: []string{"ship-it"}}},
	{"label cased differently", mergeGate{MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
		ThreadsResolved: true, NonAuthorApprove: true, Labels: []string{"SHIP-IT"}}},
	{"zero gate", mergeGate{}},
}

// TestLegacyParityMergeReady covers mergeReadyTriggers: require_label plus the
// opt-OUT merge gates.
func TestLegacyParityMergeReady(t *testing.T) {
	g := testIntegration()
	for name, act := range legacyActions() {
		for _, gc := range gateCases {
			gate := gc.gate
			want := !(act.RequireLabel != "" && !containsFold(gate.Labels, act.RequireLabel)) &&
				mergeGatePasses(&gate, act.Gates)
			got := g.filterPasses(act, "test", "o/r", mergeReadyFilterFacts(&gate), lowerMergeReady(act))
			if got != want {
				t.Errorf("merge_ready [%s] on %q: lowered=%v legacy=%v\n  filter: %s",
					name, gc.name, got, want, lowerMergeReady(act))
			}
		}
	}
}

var commentCases = []struct {
	name, author, body string
	isBot              bool
}{
	{name: "a human reviewer", author: "reviewer1", body: "please fix this"},
	{name: "a different human", author: "reviewer2", body: "lgtm"},
	{name: "a CI bot", author: "github-actions[bot]", body: "build failed", isBot: true},
	{name: "a bot not on the ignore list", author: "dependabot[bot]", body: "bump", isBot: true},
	{name: "the allowed user, cased differently", author: "REVIEWER1", body: "x"},
	{name: "no author", author: "", body: ""},
}

// TestLegacyParityNewComment covers both comment sites: the webhook path
// (from_users + ignore_users + author_bot) and the sweep's missed-comment
// recovery (from_users + ignore_users only — its listing carries no account
// type, and lowerComment must preserve that difference rather than tidy it).
func TestLegacyParityNewComment(t *testing.T) {
	g := testIntegration()
	for name, act := range legacyActions() {
		for _, cc := range commentCases {
			facts := commentFilterFacts(cc.author, cc.body, cc.isBot)

			wantWebhook := commentAuthorAllowed(act, cc.author) && authorBotMatch(act.AuthorBot, cc.isBot)
			if got := g.filterPasses(act, "test", "o/r", facts, lowerComment(act, true)); got != wantWebhook {
				t.Errorf("new_comment webhook [%s] on %q: lowered=%v legacy=%v\n  filter: %s",
					name, cc.name, got, wantWebhook, lowerComment(act, true))
			}

			wantSweep := commentAuthorAllowed(act, cc.author)
			if got := g.filterPasses(act, "test", "o/r", facts, lowerComment(act, false)); got != wantSweep {
				t.Errorf("new_comment sweep [%s] on %q: lowered=%v legacy=%v\n  filter: %s",
					name, cc.name, got, wantSweep, lowerComment(act, false))
			}
		}
	}
}

// TestLegacyParityChangesRequested covers reviewTriggers' author_bot gate.
func TestLegacyParityChangesRequested(t *testing.T) {
	g := testIntegration()
	for name, act := range legacyActions() {
		for _, isBot := range []bool{true, false} {
			want := authorBotMatch(act.AuthorBot, isBot)
			facts := prFilterFacts("h", "main", "t", "alice", nil, false)
			facts["author_is_bot"] = isBot
			facts["reviewer"] = "someone"
			got := g.filterPasses(act, "test", "o/r", facts, lowerChangesRequested(act))
			if got != want {
				t.Errorf("changes_requested [%s] bot=%v: lowered=%v legacy=%v", name, isBot, got, want)
			}
		}
	}
}

// --- the #5590 case ---------------------------------------------------------

// TestIssue5590 is the bug the unified filter exists to fix. The legacy config
// skipped the PR because `exclude` ORs its arms and `title` is a
// case-insensitive SUBSTRING — so "…publish each release entry…" hit the
// "Release " denylist entry. The unified filter says what was meant (skip only
// a release-titled PR ON a release branch) and the PR reviews.
func TestIssue5590(t *testing.T) {
	const (
		head  = "codex-changelog"
		title = "changelog: publish each release entry as its own file"
	)
	g := testIntegration()
	facts := prFilterFacts(head, "main", title, "alice", nil, false)

	// The legacy config that mis-fired, lowered as internal/connector does.
	legacy := Action{
		Exclude: Exclude{Branches: []string{"staging", "prod"}, Title: []string{"Release "}},
		Gates:   map[string]any{"not_draft": true},
	}
	if g.filterPasses(legacy, "test", "o/r", facts, lowerReviewRequested(legacy)) {
		t.Fatal("the legacy config is expected to WRONGLY skip #5590 — if it now fires, " +
			"the lowering changed behaviour rather than preserving it")
	}

	// The unified spelling from docs/design/unified-filter.md §Worked example.
	const unified = "!is_draft && !( (head_branch == 'staging' || head_branch == 'prod') && contains(title, 'Release ') )"
	act, err := filterAction(`"` + unified + `"`)
	if err != nil {
		t.Fatalf("decode the unified filter: %v", err)
	}
	if act.Filter == nil {
		t.Fatal("filter: did not decode onto the action")
	}
	if !g.filterPasses(act, "test", "o/r", facts, nil) {
		t.Errorf("#5590 must NOT be excluded by the unified filter (head=%q title=%q)", head, title)
	}

	// …and a REAL release PR on a release branch still is skipped.
	real := prFilterFacts("staging", "main", "Release 2.4.0", "bot", nil, false)
	if g.filterPasses(act, "test", "o/r", real, nil) {
		t.Error("a real `Release 2.4.0` PR on staging must still be skipped")
	}
	// A release-titled PR on an ordinary branch is NOT skipped (the AND holds).
	ordinary := prFilterFacts("feature/x", "main", "Release 2.4.0", "bot", nil, false)
	if !g.filterPasses(act, "test", "o/r", ordinary, nil) {
		t.Error("a release-titled PR on a non-release branch should fire (the inner AND is false)")
	}
	// A draft is still skipped by the leading !is_draft.
	draft := prFilterFacts(head, "main", title, "alice", nil, true)
	if g.filterPasses(act, "test", "o/r", draft, nil) {
		t.Error("a draft must still be skipped by !is_draft")
	}
}

// TestUnifiedFilterReplacesLegacy: when an action carries a `filter:`, the
// legacy lowering is not consulted at all — the two never silently AND.
func TestUnifiedFilterReplacesLegacy(t *testing.T) {
	g := testIntegration()
	act := Action{
		// A legacy exclude that WOULD skip this PR…
		Exclude: Exclude{Branches: []string{"feature/*"}},
		// …and a unified filter that says fire.
		Filter: sourcekit.FilterExpr("!is_draft"),
	}
	facts := prFilterFacts("feature/x", "main", "t", "alice", nil, false)
	if !g.filterPasses(act, "test", "o/r", facts, lowerReviewRequested(act)) {
		t.Error("the unified filter should REPLACE the legacy lowering, not AND with it")
	}
}

// TestFilterFailsClosed: a filter that cannot be evaluated must not fire.
func TestFilterFailsClosed(t *testing.T) {
	g := testIntegration()
	act := Action{Filter: sourcekit.FilterMatch("no_such_key", true)}
	if g.filterPasses(act, "test", "o/r", prFilterFacts("h", "b", "t", "a", nil, false), nil) {
		t.Error("an unevaluable filter must fail closed (not fire)")
	}
	act = Action{Filter: sourcekit.FilterMatch("label_any", 42)} // wrong type
	if g.filterPasses(act, "test", "o/r", prFilterFacts("h", "b", "t", "a", nil, false), nil) {
		t.Error("a mistyped match value must fail closed")
	}
}
