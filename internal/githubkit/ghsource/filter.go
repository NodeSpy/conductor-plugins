package ghsource

import (
	"fmt"
	"log"
	"sort"

	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// This file is the github half of the unified `filter:`
// (docs/design/unified-filter.md, docs/design/unified-filter-phase2.md): the
// FACTS each filterable event publishes, the MATCH predicate for each
// structured key, and the lowering that expresses each event's INTRINSIC
// DEFAULT keep-condition in the same IR.
//
// Every keep-condition site evaluates exactly ONE filter — the trigger's own
// `filter:` when it set a predicate, otherwise the event's default lowered
// here. The default path must stay bit-identical to the pre-filter engine, so
// each Match predicate's BODY is the matcher that site already called
// (Exclude.Matches, anyFold/allFold, containsFold, loginMatch,
// authorBotMatch, the gate readers) rather than a reimplementation, and each
// site lowers only the conjuncts IT evaluated.
//
// The match keys carry NO baked polarity: negation is the grammar's universal
// `not_` prefix (FilterNotPrefix), which decodes to a Not around the
// BASE key. So there is one `draft` matcher and `not_draft:` comes free — and
// no key can mean the opposite of what its name says.

// Fact and match-key value kinds, as internal/connector turns them into its
// own schema types. Plain strings because internal/connector imports this
// package, not the other way round.
const (
	FilterString = "string"
	FilterBool   = "bool"
	FilterList   = "list"
)

// filterFacts declares, per event, the facts a `filter:` may reference by name
// — the surface load-time validation checks expr strings against.
//
// It is deliberately a SUBSET of what the runtime facts map carries. The
// issue_matched map, for instance, also carries an empty `head_branch` so a
// legacy `exclude.branches` lowers to a bit-identical `path.Match(glob, "")`;
// declaring that as a referenceable fact would only invite `filter:` authors
// to read a value that is always empty.
var filterFacts = map[string]map[string]string{
	"review_requested": {
		"head_branch": FilterString, "base_branch": FilterString,
		"title": FilterString, "labels": FilterList,
		"is_draft": FilterBool, "author": FilterString,
	},
	"changes_requested": {
		"head_branch": FilterString, "base_branch": FilterString,
		"title": FilterString, "labels": FilterList,
		"author": FilterString, "reviewer": FilterString,
		"author_is_bot": FilterBool,
	},
	"new_comment": {
		"comment_author": FilterString, "comment_body": FilterString,
		"author_is_bot": FilterBool,
	},
	"issue_matched": {
		"title": FilterString, "labels": FilterList,
		"author": FilterString, "sole_assignee": FilterBool,
	},
	"merge_ready": {
		"labels": FilterList, "author": FilterString, "is_draft": FilterBool,
		"merge_state": FilterString, "review_decision": FilterString,
		"non_author_approval": FilterBool, "threads_resolved": FilterBool,
	},
}

// FilterRepoKey routes a trigger to repositories: the one-key replacement for
// the retired `filters: {repos, exclude_repos}`. It is STRUCTURAL, not a
// predicate — `internal/connector` hoists a trigger's top-level `repo` /
// `not_repo` into Action.Repos / Action.ExcludeRepos, which gate emit() before
// any keep-condition runs and give the sweep its repo scope.
//
// Because the gate is structural it needs no per-event fact declaration, so
// `filter: {repo: […]}` works on every github event — including the four
// (failing_checks, merge_conflict, stuck_checks, pr_behind) that publish no
// predicate facts at all and whose keep-conditions evaluate nothing.
const FilterRepoKey = "repo"

// matchKeyFacts maps each structured match key to the fact it reads and the
// type of value it takes. A key is legal for an event exactly when that event
// publishes the fact it reads, so the two surfaces cannot drift apart.
//
// FilterRepoKey is absent deliberately: it reads no declared fact (see above).
var matchKeyFacts = map[string]struct {
	fact string
	typ  string
}{
	"branch":              {"head_branch", FilterList},
	"base_branch":         {"base_branch", FilterList},
	"title":               {"title", FilterList},
	"label_any":           {"labels", FilterList},
	"label_all":           {"labels", FilterList},
	"require_label":       {"labels", FilterString},
	"author":              {"author", FilterList},
	"comment_author":      {"comment_author", FilterList},
	"author_bot":          {"author_is_bot", FilterBool},
	"sole_assignee":       {"sole_assignee", FilterBool},
	"draft":               {"is_draft", FilterBool},
	"merge_state":         {"merge_state", FilterBool},
	"review_decision":     {"review_decision", FilterBool},
	"non_author_approval": {"non_author_approval", FilterBool},
	"threads_resolved":    {"threads_resolved", FilterBool},
}

// FilterFacts returns the facts the named event publishes to a `filter:`
// (fact name → value kind), or nil when the event publishes none. An event
// with no facts still takes a `filter:` — a routing-only one (FilterRepoKey).
func FilterFacts(event string) map[string]string {
	out := make(map[string]string, len(filterFacts[event]))
	for k, v := range filterFacts[event] {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FilterMatchKeys returns the structured match keys legal inside a `filter:`
// object for the named event (key → value kind): every key whose underlying
// fact the event publishes, plus the routing key every event accepts.
//
// Only BASE keys are listed. The `not_` twin of each is produced by the
// grammar (FilterNotPrefix) as a Not around the base key, so it never
// reaches a match-key check and never needs declaring.
func FilterMatchKeys(event string) map[string]string {
	out := map[string]string{FilterRepoKey: FilterList}
	facts := filterFacts[event]
	for key, m := range matchKeyFacts {
		if _, ok := facts[m.fact]; ok {
			out[key] = m.typ
		}
	}
	return out
}

// FilterEvents lists the events that publish predicate facts, sorted.
func FilterEvents() []string {
	out := make([]string, 0, len(filterFacts))
	for e := range filterFacts {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// --- evaluation ------------------------------------------------------------

// filterPasses evaluates the one filter gating a keep-condition: the action's
// `filter:` when it stated a predicate, else the event's intrinsic default,
// lowered into the same IR by the caller.
//
// repo is injected as a fact rather than declared, so a nested `repo` (one
// inside an OR arm, which the structural hoist in internal/connector can only
// treat as a superset) still evaluates exactly here. It is not readable from
// an expr string: the routing key is the supported way to ask.
//
// An evaluation error fails CLOSED — a filter that cannot be evaluated is not
// a filter that passed — and is logged with where it came from, since a
// silently non-firing trigger is otherwise invisible. A lowered default is
// built from typed config fields and cannot error, so this only ever bites a
// hand-written `filter:`.
func (g *Source) filterPasses(act Action, kind, repo string, facts map[string]any, deflt *sourcekit.Filter) bool {
	f := act.Filter
	if f == nil {
		f = deflt
	}
	if facts != nil {
		facts[FilterRepoKey] = repo
	}
	ok, err := f.Eval(facts, matchFilterKey)
	if err != nil {
		log.Printf("github[%s]: %s %s: filter not evaluated (%v) — not firing", g.name, kind, repo, err)
		return false
	}
	return ok
}

// matchFilterKey evaluates one structured match key against the event's facts.
// Each case delegates to the matcher the pre-filter call site used, so a
// lowered default and a hand-written `filter:` share one implementation.
//
// Only BASE keys appear here. A `not_<key>` is a Not node wrapping the base
// key's Match (FilterNotPrefix), so negation never reaches this switch.
func matchFilterKey(key string, val any, facts map[string]any) (bool, error) {
	switch key {
	// --- routing ---
	case FilterRepoKey:
		globs, err := filterStrings(key, val)
		if err != nil {
			return false, err
		}
		return matchRepo(globs, factString(facts, FilterRepoKey)), nil

	// --- glob / substring keys: the Exclude matchers, one arm each ---
	case "branch":
		globs, err := filterStrings(key, val)
		if err != nil {
			return false, err
		}
		return Exclude{Branches: globs}.Matches(factString(facts, "head_branch"), "", nil), nil
	case "base_branch":
		globs, err := filterStrings(key, val)
		if err != nil {
			return false, err
		}
		return Exclude{Branches: globs}.Matches(factString(facts, "base_branch"), "", nil), nil
	case "title":
		// Case-insensitive SUBSTRING match — the legacy semantics, footgun and
		// all ("Release " matches "…each release entry…"). New configs should
		// reach for expr + startswith()/contains() instead.
		subs, err := filterStrings(key, val)
		if err != nil {
			return false, err
		}
		return Exclude{Title: subs}.Matches("", factString(facts, "title"), nil), nil

	// --- label / login sets ---
	case "label_any":
		want, err := filterStrings(key, val)
		if err != nil {
			return false, err
		}
		return anyFold(factStrings(facts, "labels"), want), nil
	case "label_all":
		want, err := filterStrings(key, val)
		if err != nil {
			return false, err
		}
		return allFold(factStrings(facts, "labels"), want), nil
	case "require_label":
		label, err := filterString(key, val)
		if err != nil {
			return false, err
		}
		return containsFold(factStrings(facts, "labels"), label), nil
	case "author":
		want, err := filterStrings(key, val)
		if err != nil {
			return false, err
		}
		return containsFold(want, factString(facts, "author")), nil
	case "comment_author":
		// The COMMENTER, not the PR author. `comment_author:` is the old
		// `from_users:`; `not_comment_author:` is the old `ignore_users:`,
		// which is now visibly the same key negated rather than a second one.
		want, err := filterStrings(key, val)
		if err != nil {
			return false, err
		}
		return loginMatch(want, factString(facts, "comment_author")), nil
	case "author_bot":
		want, err := filterBool(key, val)
		if err != nil {
			return false, err
		}
		return authorBotMatch(&want, factBool(facts, "author_is_bot")), nil

	// --- draft: a plain equality, so `not_draft: true` reads as "require NOT
	// draft" through the generic negation rather than through a second key ---
	case "draft":
		want, err := filterBool(key, val)
		if err != nil {
			return false, err
		}
		return factBool(facts, "is_draft") == want, nil

	// --- opt-out toggles: false relaxes the check, true (or absent) enforces ---
	case "sole_assignee":
		return gatedBy(key, val, func() bool { return factBool(facts, "sole_assignee") })
	case "merge_state":
		return gatedBy(key, val, func() bool { return factString(facts, "merge_state") == "CLEAN" })
	case "review_decision":
		return gatedBy(key, val, func() bool { return factString(facts, "review_decision") == "APPROVED" })
	case "non_author_approval":
		return gatedBy(key, val, func() bool { return factBool(facts, "non_author_approval") })
	case "threads_resolved":
		return gatedBy(key, val, func() bool { return factBool(facts, "threads_resolved") })
	}
	return false, fmt.Errorf("filter: github has no match key %q", key)
}

// gatedBy evaluates a boolean toggle key: `true` enforces the check, `false`
// waives it (the key is then vacuously satisfied).
func gatedBy(key string, val any, check func() bool) (bool, error) {
	on, err := filterBool(key, val)
	if err != nil {
		return false, err
	}
	return !on || check(), nil
}

// --- value coercion --------------------------------------------------------

// filterStrings coerces a match key's value to a string list, accepting the
// single-string shorthand (`branches: main`) YAML users expect.
func filterStrings(key string, val any) ([]string, error) {
	switch x := val.(type) {
	case nil:
		return nil, nil
	case []string:
		return x, nil
	case string:
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("filter: %s: want a list of strings, got a %T entry", key, e)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("filter: %s: want a list of strings, got %T", key, val)
}

func filterString(key string, val any) (string, error) {
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("filter: %s: want a string, got %T", key, val)
	}
	return s, nil
}

func filterBool(key string, val any) (bool, error) {
	b, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("filter: %s: want true or false, got %T", key, val)
	}
	return b, nil
}

// --- fact accessors --------------------------------------------------------

func factString(facts map[string]any, key string) string {
	s, _ := facts[key].(string)
	return s
}

func factBool(facts map[string]any, key string) bool {
	b, _ := facts[key].(bool)
	return b
}

func factStrings(facts map[string]any, key string) []string {
	v, _ := facts[key].([]string)
	return v
}

// --- intrinsic-default lowering --------------------------------------------
//
// These lower each event's DEFAULT keep-condition — what it evaluates when the
// trigger states no predicate. They read Action's legacy predicate fields,
// which no config surface populates any more (`filters:` is gone), so in the
// connectors model every one of them is zero and the lowering collapses to the
// event's built-in behavior: merge_ready's five opt-out gates stay enforced,
// ready_for_review still skips the draft gate, everything else is unfiltered.
//
// The fields are still read rather than assumed zero because a legacy
// integration config (internal/integrations/github's own `rules:`/`actions:`
// YAML, which core.Build decodes straight into Action) can still set
// them — and because "no behavior change when a trigger sets no filter" is
// only demonstrable if the same code path produces both.

// matchIf emits a Match node only for a non-empty list. Emptiness is why the
// lowering cannot be a blanket "one Match per key": `label_any: []` means "no
// constraint" as a config field but "has any of nothing" (false) as a
// predicate, so an unset field must produce NO node rather than an empty one.
// FilterAnd/FilterOr drop the nils.
func matchIf(key string, vals []string) *sourcekit.Filter {
	if len(vals) == 0 {
		return nil
	}
	return sourcekit.FilterMatch(key, vals)
}

// lowerExclude lowers an `exclude:` denylist. It is an OR across its arms, and
// "not excluded" is the negation of that OR — so an empty exclude lowers to
// nothing (Or of no arms is false, Not of that is true), matching
// Exclude.Matches returning false for an empty Exclude.
//
// This is the shape an author now writes directly, one key per arm:
// `not_branch:`, `not_label_any:`, `not_title:` — which also lets them be
// AND-ed instead, the distinction the old single `exclude:` block could not
// express (docs/design/unified-filter.md §the #5590 fix).
func lowerExclude(e Exclude) *sourcekit.Filter {
	if e.Empty() {
		return nil
	}
	return sourcekit.FilterNot(sourcekit.FilterOr(
		matchIf("branch", e.Branches),
		matchIf("label_any", e.Labels),
		matchIf("title", e.Title),
	))
}

// notDraftIf emits the "require not draft" conjunct when the gate is on, and
// nothing when it is off — nothing rather than a vacuously-true node, because
// `draft` is now a plain equality with no waiving value to pass it.
func notDraftIf(on bool) *sourcekit.Filter {
	if !on {
		return nil
	}
	return sourcekit.FilterNot(sourcekit.FilterMatch("draft", true))
}

// lowerReviewRequested lowers what a review_requested keep-condition
// evaluated inline: the opt-IN `gates.not_draft` toggle AND "not excluded".
// Both are zero in the connectors model, so the default is "always keep".
func lowerReviewRequested(act Action) *sourcekit.Filter {
	return sourcekit.FilterAnd(
		notDraftIf(gateEnabled(act.Gates, "not_draft")),
		lowerExclude(act.Exclude),
	)
}

// lowerReadyReview lowers the ready_for_review keep-condition, which checks
// the exclude list but deliberately NOT the not_draft gate — the PR just left
// draft, and re-applying the gate here is what the transition exists to undo.
func lowerReadyReview(act Action) *sourcekit.Filter {
	return lowerExclude(act.Exclude)
}

// lowerIssueMatch lowers issue_matched's payload filters. The `assignee` key
// stays outside the IR: it resolves against the connector's `me` identity
// rather than a published fact, so it has no match key.
func lowerIssueMatch(act Action) *sourcekit.Filter {
	var sole *sourcekit.Filter
	if act.SoleAssignee {
		sole = sourcekit.FilterMatch("sole_assignee", true)
	}
	return sourcekit.FilterAnd(
		sole,
		matchIf("label_any", act.LabelsAny),
		matchIf("label_all", act.LabelsAll),
		lowerExclude(act.Exclude),
		matchIf("author", act.Authors),
	)
}

// lowerMergeReady lowers merge_ready's `require_label` plus its `gates:` map.
// These gates are opt-OUT (absent means enforced — mergeGateOn), the opposite
// of the opt-IN toggle draftGate reads, which is why the draft conjunct is
// lowered from a different reader here.
//
// This is the one lowering that is NOT vacuous in the connectors model: with a
// zero `gates:` map mergeGateOn says true for all five, so merge_ready keeps
// its full all-green requirement by default. A trigger that states its own
// predicate takes that over — which is what `merge_state: false` (waive one
// gate) has always meant, and is why the toggles keep their opt-out reading.
func lowerMergeReady(act Action) *sourcekit.Filter {
	var require *sourcekit.Filter
	if act.RequireLabel != "" {
		require = sourcekit.FilterMatch("require_label", act.RequireLabel)
	}
	gates := make([]*sourcekit.Filter, 0, len(mergeGateKeys))
	for _, k := range mergeGateKeys {
		on := mergeGateOn(act.Gates, k)
		if k == "not_draft" {
			gates = append(gates, notDraftIf(on))
			continue
		}
		gates = append(gates, sourcekit.FilterMatch(k, on))
	}
	return sourcekit.FilterAnd(require, sourcekit.FilterAnd(gates...))
}

// lowerComment lowers a new_comment keep-condition. withAuthorBot reflects a
// real difference between the two sites: the webhook path also gates on
// author_bot, while the sweep's missed-comment recovery does not (its comment
// listing carries no account type). A hand-written `filter:` replaces both and
// applies uniformly.
func lowerComment(act Action, withAuthorBot bool) *sourcekit.Filter {
	var bot *sourcekit.Filter
	if withAuthorBot && act.AuthorBot != nil {
		bot = sourcekit.FilterMatch("author_bot", *act.AuthorBot)
	}
	var ignore *sourcekit.Filter
	if m := matchIf("comment_author", act.IgnoreUsers); m != nil {
		ignore = sourcekit.FilterNot(m)
	}
	return sourcekit.FilterAnd(
		matchIf("comment_author", act.FromUsers),
		ignore,
		bot,
	)
}

// lowerChangesRequested lowers the changes_requested keep-condition: the
// reviewer's bot-ness, when the trigger constrains it.
func lowerChangesRequested(act Action) *sourcekit.Filter {
	if act.AuthorBot == nil {
		return nil
	}
	return sourcekit.FilterMatch("author_bot", *act.AuthorBot)
}

// --- facts builders --------------------------------------------------------

// prFilterFacts builds the facts a PR-shaped event filters on.
func prFilterFacts(headBranch, baseBranch, title, author string, labels []string, isDraft bool) map[string]any {
	return map[string]any{
		"head_branch": headBranch,
		"base_branch": baseBranch,
		"title":       title,
		"author":      author,
		"labels":      labels,
		"is_draft":    isDraft,
	}
}

// issueFilterFacts builds issue_matched's facts. head_branch/base_branch are
// present but empty: a legacy `exclude.branches` on an issue ran its globs
// against "" (Exclude.Matches with no branch), and the lowered Match
// has to do the same to stay bit-identical. Neither is a DECLARED fact, so no
// `filter:` can read them.
func issueFilterFacts(st issueMatchState, soleAssignee bool) map[string]any {
	return map[string]any{
		"head_branch":   "",
		"base_branch":   "",
		"title":         st.title,
		"author":        st.author,
		"labels":        st.labels,
		"is_draft":      false,
		"sole_assignee": soleAssignee,
	}
}

// mergeReadyFilterFacts builds merge_ready's facts from the composite gate.
// The gate query carries no title or head ref, so neither is declared.
func mergeReadyFilterFacts(gate *mergeGate) map[string]any {
	return map[string]any{
		"labels":              gate.Labels,
		"author":              gate.Author,
		"is_draft":            gate.IsDraft,
		"merge_state":         gate.MergeStateStatus,
		"review_decision":     gate.ReviewDecision,
		"non_author_approval": gate.NonAuthorApprove,
		"threads_resolved":    gate.ThreadsResolved,
	}
}

// commentFilterFacts builds new_comment's facts. The author here is the
// COMMENTER, not the PR author — `comment_author` says so in the name, which
// is the whole reason the unified grammar does not reuse `author`.
func commentFilterFacts(commentAuthor, body string, isBot bool) map[string]any {
	return map[string]any{
		"comment_author": commentAuthor,
		"comment_body":   body,
		"author_is_bot":  isBot,
	}
}
