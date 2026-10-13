package ghsource

import (
	"strings"
	"testing"

	"github.com/NodeSpy/conductor/pkg/sourcekit"
	"gopkg.in/yaml.v3"
)

// Phase 2 of the unified filter (docs/design/unified-filter-phase2.md): the
// retired `filters:` block's every predicate key has a spelling in the one
// `filter:`, and that spelling decides identically.
//
// filter_parity_test.go proves the LOWERING is bit-identical to the pre-filter
// matchers. This file proves the MIGRATION is: for each retired key, the
// legacy config (as Action fields, exactly what the old `filters:`
// decoded into) and the `filter:` an operator writes instead must agree on
// every PR in prCases. Both sides run through filterPasses, so a divergence is
// a real behavior change and not a difference of test harness.

// mustFilterAction decodes a `filter:` onto an Action the way the connectors
// lowering hands one to the source: the operator's YAML, through the surface
// grammar (sourcekit.ParseFilter — pinned to conductor's own config decoder).
func mustFilterAction(t *testing.T, body string) Action {
	t.Helper()
	act, err := filterAction(body)
	if err != nil {
		t.Fatalf("decode filter %s: %v", body, err)
	}
	if act.Filter == nil {
		t.Fatalf("filter %s did not decode onto the action", body)
	}
	return act
}

// filterAction parses YAML filter text onto an Action.
func filterAction(body string) (Action, error) {
	var v any
	if err := yaml.Unmarshal([]byte(body), &v); err != nil {
		return Action{}, err
	}
	f, err := sourcekit.ParseFilter(v)
	return Action{Filter: f}, err
}

// TestPhase2MigrationParityPRPredicates: the retired PR-shaped keys.
func TestPhase2MigrationParityPRPredicates(t *testing.T) {
	g := testIntegration()
	yes := true
	cases := []struct {
		name   string
		legacy Action // what `filters:` used to decode into
		filter string // what an operator writes now
		lower  func(Action) *sourcekit.Filter
	}{
		{
			name:   "exclude.branches → not_branch",
			legacy: Action{Exclude: Exclude{Branches: []string{"staging", "release/*"}}},
			filter: `{ not_branch: [staging, "release/*"] }`,
			lower:  lowerReviewRequested,
		},
		{
			name:   "exclude.labels → not_label_any",
			legacy: Action{Exclude: Exclude{Labels: []string{"hold", "wip"}}},
			filter: `{ not_label_any: [hold, wip] }`,
			lower:  lowerReviewRequested,
		},
		{
			name:   "exclude.title → not_title (substring, footgun preserved)",
			legacy: Action{Exclude: Exclude{Title: []string{"Release "}}},
			filter: `{ not_title: ["Release "] }`,
			lower:  lowerReviewRequested,
		},
		{
			// The old block ORed its arms and negated the whole thing, which is
			// exactly what three `not_` keys AND-ed together come to. The new
			// spelling can also say "and" instead — see TestPhase2NotKeysCanAND.
			name: "the whole exclude block → three not_ keys AND-ed",
			legacy: Action{Exclude: Exclude{
				Branches: []string{"staging", "prod"},
				Labels:   []string{"hold"},
				Title:    []string{"Release "},
			}},
			filter: `{ not_branch: [staging, prod], not_label_any: [hold], not_title: ["Release "] }`,
			lower:  lowerReviewRequested,
		},
		{
			name:   "gates.not_draft → not_draft",
			legacy: Action{Gates: map[string]any{"not_draft": true}},
			filter: `{ not_draft: true }`,
			lower:  lowerReviewRequested,
		},
		{
			name: "the #5590 config, key for key",
			legacy: Action{
				Exclude: Exclude{Branches: []string{"staging", "prod"}, Title: []string{"Release "}},
				Gates:   map[string]any{"not_draft": true},
			},
			filter: `{ not_branch: [staging, prod], not_title: ["Release "], not_draft: true }`,
			lower:  lowerReviewRequested,
		},
		{
			name:   "labels_any → label_any",
			legacy: Action{LabelsAny: []string{"urgent", "security"}},
			filter: `{ label_any: [urgent, security] }`,
			lower:  lowerIssueMatch,
		},
		{
			name:   "labels_all → label_all",
			legacy: Action{LabelsAll: []string{"urgent", "Bug"}},
			filter: `{ label_all: [urgent, Bug] }`,
			lower:  lowerIssueMatch,
		},
		{
			name:   "authors → author",
			legacy: Action{Authors: []string{"alice", "Bot"}},
			filter: `{ author: [alice, Bot] }`,
			lower:  lowerIssueMatch,
		},
		{
			name:   "labels_any + labels_all + authors together",
			legacy: Action{LabelsAny: []string{"urgent"}, LabelsAll: []string{"Bug"}, Authors: []string{"carol"}},
			filter: `{ label_any: [urgent], label_all: [Bug], author: [carol] }`,
			lower:  lowerIssueMatch,
		},
		{
			name:   "sole_assignee stays",
			legacy: Action{SoleAssignee: true},
			filter: `{ sole_assignee: true }`,
			lower:  lowerIssueMatch,
		},
		{
			name:   "author_bot stays",
			legacy: Action{AuthorBot: &yes},
			filter: `{ author_bot: true }`,
			lower:  lowerChangesRequested,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unified := mustFilterAction(t, c.filter)
			for _, pr := range prCases {
				legacyFacts := phase2Facts(g, pr)
				unifiedFacts := phase2Facts(g, pr)
				want := g.filterPasses(c.legacy, "legacy", "o/r", legacyFacts, c.lower(c.legacy))
				got := g.filterPasses(unified, "unified", "o/r", unifiedFacts, nil)
				if got != want {
					t.Errorf("%s: legacy=%v unified=%v\n  legacy lowers to: %s\n  unified is:       %s",
						pr.name, want, got, c.lower(c.legacy), unified.Filter)
				}
			}
		})
	}
}

// phase2Facts builds one PR's facts for both the PR-shaped events and
// issue_matched's keys (sole_assignee, author_bot), so a single table can
// cover keys that live on different events.
func phase2Facts(g *Source, pr prCase) map[string]any {
	f := prFilterFacts(pr.head, pr.base, pr.title, pr.author, pr.labels, pr.draft)
	f["sole_assignee"] = pr.author == "alice"
	f["author_is_bot"] = pr.author == "bot"
	return f
}

// TestPhase2MigrationParityComment: `from_users:` became `comment_author:` and
// `ignore_users:` became its NEGATION rather than a second key — the point
// being that the relationship between the two is now visible in their names.
func TestPhase2MigrationParityComment(t *testing.T) {
	g := testIntegration()
	authors := []string{"alice", "ci-bot", "github-actions[bot]", "Alice", ""}
	cases := []struct {
		name   string
		legacy Action
		filter string
	}{
		{"from_users → comment_author", Action{FromUsers: []string{"alice"}}, `{ comment_author: [alice] }`},
		{"ignore_users → not_comment_author", Action{IgnoreUsers: []string{"ci-bot"}}, `{ not_comment_author: [ci-bot] }`},
		{
			"both, overlapping — the deny still wins, because an AND of a key " +
				"and its negation is false for the overlap",
			Action{FromUsers: []string{"alice", "ci-bot"}, IgnoreUsers: []string{"ci-bot"}},
			`{ comment_author: [alice, ci-bot], not_comment_author: [ci-bot] }`,
		},
		{
			"a [bot]-suffixed login matches by convention, either way round",
			Action{IgnoreUsers: []string{"github-actions"}},
			`{ not_comment_author: [github-actions] }`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unified := mustFilterAction(t, c.filter)
			for _, author := range authors {
				want := g.filterPasses(c.legacy, "legacy", "o/r",
					commentFilterFacts(author, "body", false), lowerComment(c.legacy, false))
				got := g.filterPasses(unified, "unified", "o/r",
					commentFilterFacts(author, "body", false), nil)
				if got != want {
					t.Errorf("commenter %q: legacy=%v unified=%v", author, want, got)
				}
			}
		})
	}
}

// TestPhase2NotDraftRequiresNotDraft: `not_draft: true` must fire ONLY when
// the PR is not a draft. There is no `not_draft` matcher — the key decodes to
// Not(Match(draft,true)) — so this is the check that the generic negation
// composes with a plain equality to mean what the old baked-polarity key meant.
func TestPhase2NotDraftRequiresNotDraft(t *testing.T) {
	g := testIntegration()
	act := mustFilterAction(t, `{ not_draft: true }`)
	if got := act.Filter.String(); got != `and(not(match(draft,true)))` {
		t.Fatalf("not_draft should decode to a Not around the base key, got %s", got)
	}
	for _, draft := range []bool{false, true} {
		facts := prFilterFacts("h", "main", "t", "alice", nil, draft)
		got := g.filterPasses(act, "not_draft", "o/r", facts, nil)
		if want := !draft; got != want {
			t.Errorf("not_draft: true on draft=%v fired %v, want %v", draft, got, want)
		}
	}
	// The base key is still a plain equality in both directions, and stacking
	// the prefix twice is not a thing the grammar invents a meaning for.
	for _, c := range []struct {
		body  string
		draft bool
		want  bool
	}{
		{`{ draft: true }`, true, true},
		{`{ draft: true }`, false, false},
		{`{ draft: false }`, false, true},
		{`{ draft: false }`, true, false},
		{`{ not_draft: false }`, true, true}, // "not (not a draft)" → require draft
		{`{ not_draft: false }`, false, false},
	} {
		facts := prFilterFacts("h", "main", "t", "alice", nil, c.draft)
		got := g.filterPasses(mustFilterAction(t, c.body), "draft", "o/r", facts, nil)
		if got != c.want {
			t.Errorf("filter %s on draft=%v = %v, want %v", c.body, c.draft, got, c.want)
		}
	}
}

// TestPhase2NotKeysCanAND is what the whole unification was for. The old
// `exclude:` block could only OR its arms: "skip if the branch matches OR the
// title matches". #5590 needed the AND — skip only a release-titled PR ON a
// release branch — and there was no way to write it.
func TestPhase2NotKeysCanAND(t *testing.T) {
	g := testIntegration()
	// The old shape, as three independent denials (an AND of nots = a not of
	// the OR): the changelog PR is skipped, wrongly.
	deny := mustFilterAction(t, `{ not_branch: [staging, prod], not_title: ["Release "] }`)
	// The intent, with the negation over the CONJUNCTION instead.
	both := mustFilterAction(t, `{ not_expr: "(head_branch == 'staging' || head_branch == 'prod') && contains(title, 'Release ')" }`)

	changelog := func() map[string]any {
		return prFilterFacts("codex-changelog", "main",
			"changelog: publish each release entry as its own file", "alice", nil, false)
	}
	realRelease := func() map[string]any {
		return prFilterFacts("staging", "main", "Release 2.4.0", "bot", nil, false)
	}
	if g.filterPasses(deny, "deny", "o/r", changelog(), nil) {
		t.Error("the OR-of-denials spelling is expected to skip #5590 — that is the bug being reproduced")
	}
	if !g.filterPasses(both, "both", "o/r", changelog(), nil) {
		t.Error("negating the CONJUNCTION must let #5590 through")
	}
	if g.filterPasses(both, "both", "o/r", realRelease(), nil) {
		t.Error("a real release PR on a release branch must still be skipped")
	}
}

// TestPhase2RepoRoutingKey: `repo` is evaluable as a match key too, not only
// hoistable. A `repo` nested under an OR arm cannot be hoisted soundly (the
// hoist is a superset), so the filter has to decide it precisely here.
func TestPhase2RepoRoutingKey(t *testing.T) {
	g := testIntegration()
	act := mustFilterAction(t, `[ { repo: [org/web], not_draft: true }, { repo: ["org/api-*"], label_any: [urgent] } ]`)
	cases := []struct {
		repo   string
		draft  bool
		labels []string
		want   bool
	}{
		{"org/web", false, nil, true},                   // first arm
		{"org/web", true, nil, false},                   // first arm's predicate fails
		{"org/api-v2", false, []string{"urgent"}, true}, // second arm, glob
		{"org/api-v2", false, nil, false},               // second arm's predicate fails
		{"org/other", false, []string{"urgent"}, false}, // neither repo matches
	}
	for _, c := range cases {
		facts := prFilterFacts("h", "main", "t", "alice", c.labels, c.draft)
		got := g.filterPasses(act, "repo", c.repo, facts, nil)
		if got != c.want {
			t.Errorf("repo=%s draft=%v labels=%v → %v, want %v", c.repo, c.draft, c.labels, got, c.want)
		}
	}
	// A bare `repo` is a glob match against the event's repo, same matcher the
	// structural gate uses.
	scope := mustFilterAction(t, `{ repo: ["org/*"], expr: "!is_draft" }`)
	for repo, want := range map[string]bool{"org/web": true, "other/web": false} {
		facts := prFilterFacts("h", "main", "t", "alice", nil, false)
		if got := g.filterPasses(scope, "repo", repo, facts, nil); got != want {
			t.Errorf("repo glob on %s = %v, want %v", repo, got, want)
		}
	}
}

// TestPhase2MergeReadyDefaultsSurvive: merge_ready is the one event whose
// intrinsic default is not "fire" — its five gates are opt-OUT, so an absent
// `gates:` map means all five enforced. With the `filters:` surface gone, that
// default has to come from the lowering, and it must still be all five.
func TestPhase2MergeReadyDefaultsSurvive(t *testing.T) {
	g := testIntegration()
	var act Action // no config at all — the connectors-model state
	lowered := lowerMergeReady(act)
	for _, want := range []string{
		"not(match(draft,true))",
		"match(merge_state,true)",
		"match(review_decision,true)",
		"match(non_author_approval,true)",
		"match(threads_resolved,true)",
	} {
		if !strings.Contains(lowered.String(), want) {
			t.Errorf("the merge_ready default lost %s: %s", want, lowered)
		}
	}
	green := mergeGate{
		MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
		NonAuthorApprove: true, ThreadsResolved: true,
	}
	if !g.filterPasses(act, "merge_ready", "o/r", mergeReadyFilterFacts(&green), lowered) {
		t.Error("an all-green PR must still pass the default gates")
	}
	// Each gate, failed one at a time, must suppress it.
	for name, break_ := range map[string]func(g *mergeGate){
		"draft":               func(g *mergeGate) { g.IsDraft = true },
		"merge_state":         func(g *mergeGate) { g.MergeStateStatus = "DIRTY" },
		"review_decision":     func(g *mergeGate) { g.ReviewDecision = "REVIEW_REQUIRED" },
		"non_author_approval": func(g *mergeGate) { g.NonAuthorApprove = false },
		"threads_resolved":    func(g *mergeGate) { g.ThreadsResolved = false },
	} {
		gate := green
		break_(&gate)
		if g.filterPasses(act, "merge_ready", "o/r", mergeReadyFilterFacts(&gate), lowerMergeReady(act)) {
			t.Errorf("the default %s gate must suppress merge_ready", name)
		}
	}
	// The oracle agrees: this is mergeGatePasses with an empty gates map.
	for _, gate := range []mergeGate{green, {}, {MergeStateStatus: "CLEAN"}} {
		want := mergeGatePasses(&gate, act.Gates)
		got := g.filterPasses(act, "merge_ready", "o/r", mergeReadyFilterFacts(&gate), lowerMergeReady(act))
		if got != want {
			t.Errorf("gate %+v: lowered=%v oracle=%v", gate, got, want)
		}
	}
}

// TestPhase2ReadyForReviewSkipsTheDraftGate: the ready_for_review transition
// fires BECAUSE the PR just left draft, so re-applying a draft gate there is
// what the transition exists to undo. That asymmetry with review_requested is
// a property of the lowering, and it has to survive the surface going away.
func TestPhase2ReadyForReviewSkipsTheDraftGate(t *testing.T) {
	g := testIntegration()
	act := Action{Gates: map[string]any{"not_draft": true}}
	if lowerReadyReview(act) != nil {
		t.Errorf("ready_for_review must not lower a draft gate, got %s", lowerReadyReview(act))
	}
	// Even asked about a draft PR, and even with the gate configured on.
	facts := prFilterFacts("h", "main", "t", "alice", nil, true)
	if !g.filterPasses(act, "ready_for_review", "o/r", facts, lowerReadyReview(act)) {
		t.Error("ready_for_review must fire for a PR that is still flagged draft in the payload")
	}
	// review_requested, the same action, does apply it.
	if g.filterPasses(act, "review_requested", "o/r",
		prFilterFacts("h", "main", "t", "alice", nil, true), lowerReviewRequested(act)) {
		t.Error("review_requested must apply its opt-in draft gate")
	}
}

// TestPhase2MatchKeysAreDeclaredForEveryEvent: the match-key surface and the
// matcher must not drift. Every key FilterMatchKeys advertises has to be
// evaluable, and every base key the matcher implements has to be advertised
// somewhere — otherwise a filter validates and then fails closed at runtime,
// or works but cannot be written.
func TestPhase2MatchKeysAreDeclaredForEveryEvent(t *testing.T) {
	declared := map[string]bool{}
	for _, ev := range append(FilterEvents(), "failing_checks", "merge_conflict", "pr_behind", "stuck_checks") {
		keys := FilterMatchKeys(ev)
		if _, ok := keys[FilterRepoKey]; !ok {
			t.Errorf("%s must accept the routing key %q", ev, FilterRepoKey)
		}
		for k, kind := range keys {
			declared[k] = true
			// A declared key must be evaluable. Feed it a value of its own
			// type; the answer does not matter, only that it is not "no such
			// match key".
			var val any = []string{"x"}
			switch kind {
			case FilterBool:
				val = true
			case FilterString:
				val = "x"
			}
			if _, err := matchFilterKey(k, val, map[string]any{}); err != nil &&
				strings.Contains(err.Error(), "no match key") {
				t.Errorf("%s declares %q but the matcher does not implement it", ev, k)
			}
		}
	}
	for k := range matchKeyFacts {
		if !declared[k] {
			t.Errorf("match key %q is implemented but no event declares it", k)
		}
	}
	// And a retired key is gone from both sides, rather than quietly aliased.
	for _, gone := range []string{"branches", "base_branches", "labels_any", "labels_all",
		"authors", "from_users", "ignore_users", "not_draft"} {
		if declared[gone] {
			t.Errorf("retired key %q is still declared", gone)
		}
		if _, err := matchFilterKey(gone, []string{"x"}, map[string]any{}); err == nil {
			t.Errorf("retired key %q is still evaluable", gone)
		}
	}
}

// TestPhase2FilterFailsClosedOnARetiredKey: a config that somehow reaches the
// matcher with an old key must not fire. Load-time validation is the first
// line, but the runtime is the one that decides whether an agent is dispatched.
func TestPhase2FilterFailsClosedOnARetiredKey(t *testing.T) {
	g := testIntegration()
	for _, key := range []string{"labels_any", "from_users", "not_draft"} {
		act := Action{Filter: sourcekit.FilterMatch(key, []string{"x"})}
		facts := prFilterFacts("h", "main", "t", "alice", []string{"x"}, false)
		if g.filterPasses(act, "retired", "o/r", facts, nil) {
			t.Errorf("a filter using the retired key %q must fail closed", key)
		}
	}
}
