package ghsource

import (
	"context"
	"path"
	"strings"
	"time"

	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// The kit's own vocabulary. Each type is the daemon-agnostic half of a
// conductor type the bundled integration used to read directly — Trigger is
// core.Trigger, Action is the slice of config.Action this source evaluates —
// so a host (conductor's builtin adapter, or an external plugin) converts at
// its edge and the logic in between is the one implementation both run.

// KindClosed is the reserved kind emitted when a PR reaches a terminal state,
// so the host engine drops its dedup state and settles its outcome. It never
// dispatches an action. It is the event the github declaration marks
// closes_target (ghplugin.ClosedEvent).
const KindClosed = "_closed"

// The two GitHub comment id sequences, published as `comment_kind` so the host
// engine keeps a separate high-water mark per sequence: conversation comments
// (issues/{n}/comments) and inline review comments (pulls/{n}/comments). Same
// values as conductor's store.CommentKindIssue / store.CommentKindReview.
const (
	CommentKindIssue  = "issue"
	CommentKindReview = "review"
)

// FilterNotPrefix negates any `filter:` object key (sourcekit.FilterNotPrefix).
const FilterNotPrefix = sourcekit.FilterNotPrefix

// BranchFixKind reports whether a trigger kind's fixer works on, and pushes
// to, its PR's own head branch — work that stops mattering once the PR closes,
// and whose triggers must carry head_ref. Same set as conductor's
// the declaration's bound_to_target (ghplugin.EventSemantics).
func BranchFixKind(kind string) bool {
	switch kind {
	case "new_comment", "changes_requested", "failing_checks", "merge_conflict", "pr_behind":
		return true
	}
	return false
}

// Target identifies the GitHub object a Trigger concerns. Field for field the
// same as conductor's core.Target, so the host converts with a type
// conversion rather than a copy that could drift.
type Target struct {
	Repo    string // "owner/name"
	Owner   string
	Name    string
	PR      int
	Issue   int
	Number  int // the PR or issue number, whichever applies
	HeadSHA string
	BaseRef string
	HTMLURL string
	// Project is the paseo project/workspace to check out when it differs
	// from Repo (project_map / project_rewrite). Checkout only.
	Project string
}

// CheckoutRepo returns the project to check out: Project when set, else Repo.
// Forge operations use Repo directly.
func (t Target) CheckoutRepo() string {
	if t.Project != "" {
		return t.Project
	}
	return t.Repo
}

// Trigger is one event the source emits: a kind, the variant (trigger) it is
// for, its target and context, and the matched Action.
type Trigger struct {
	Source   string // "github"
	Instance string // the source's instance name
	Kind     string // e.g. "merge_conflict", "review_requested"
	Variant  string // the matched action's Name; "" for an unnamed sole action
	Target   Target
	Title    string
	Context  map[string]any
	Dedup    string
	Labels   map[string]string
	// Action is the matched variant (an Action value; nil only on KindClosed,
	// which matches no variant). Its Ext is the host's own action payload.
	Action any
	// TargetTrusted: GitHub assigned the target (a signature-verified
	// delivery, or a read made with the source's own credentials), not the
	// event's sender.
	TargetTrusted bool
	// CatchUp marks a trigger the sweep re-derived rather than a fresh
	// delivery.
	CatchUp bool
	// Force marks a manually-injected trigger (Source.Force): the host engine
	// bypasses its dedup / liveness / backoff gates.
	Force bool
}

// EmitFunc delivers one Trigger to the host.
type EmitFunc func(ctx context.Context, t Trigger)

// Actors is a set of GitHub logins/teams (reviewer/assignee matching).
type Actors struct {
	Logins []string `yaml:"logins" json:"logins,omitempty"`
	Teams  []string `yaml:"teams" json:"teams,omitempty"`
}

// HasLogin reports whether login is in the set (case-insensitive).
func (a Actors) HasLogin(login string) bool {
	for _, l := range a.Logins {
		if strings.EqualFold(l, login) {
			return true
		}
	}
	return false
}

// Exclude is the legacy PR denylist an action's intrinsic default lowers
// (see lowerExclude). No connectors-model surface sets it.
type Exclude struct {
	Branches []string // head-branch globs, e.g. "release/*"
	Labels   []string // PR labels (case-insensitive)
	Title    []string // case-insensitive substrings of the PR title
}

// Empty reports whether no exclusion is configured.
func (e Exclude) Empty() bool {
	return len(e.Branches) == 0 && len(e.Labels) == 0 && len(e.Title) == 0
}

// Matches reports whether a PR (head branch, title, labels) hits any exclusion.
func (e Exclude) Matches(branch, title string, labels []string) bool {
	for _, p := range e.Branches {
		if ok, _ := path.Match(p, branch); ok {
			return true
		}
	}
	for _, want := range e.Labels {
		for _, l := range labels {
			if strings.EqualFold(want, l) {
				return true
			}
		}
	}
	lt := strings.ToLower(title)
	for _, s := range e.Title {
		if s != "" && strings.Contains(lt, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

// Action is one configured variant of an event kind — one trigger — as this
// source evaluates it: its name, whether it is on, where it applies, its
// identity gates and per-kind options, and its `filter:`.
//
// It carries ONLY what the source reads. What a host does with a matched
// trigger (an agent, a workflow, a prompt) is the host's business and rides
// opaquely in Ext: conductor's builtin adapter puts its config.Action there,
// a plugin leaves it nil.
type Action struct {
	Name    string
	Enabled *bool // nil = enabled
	// Repos / ExcludeRepos are per-variant repo gates (globs). A trigger's
	// `filter: {repo: …}` / `not_repo` lowers here (see LowerTrigger).
	Repos        []string
	ExcludeRepos []string
	// Filter is the trigger's whole predicate. When set it REPLACES the
	// event's intrinsic default keep-condition at every site that evaluates
	// one; when nil each site lowers its own default into the same IR.
	Filter *sourcekit.Filter

	// Identity gates (resolved against the source's `me` when empty).
	Reviewer Actors // review_requested: whose requested review triggers it
	Assignee Actors // issue_matched: whose assignment triggers it

	// Per-kind options.
	IgnoreChecks       []string      // failing_checks: check names that never trigger
	StuckAfter         time.Duration // stuck_checks: a run longer than this is stuck (default 30m)
	PollInterval       time.Duration // stuck_checks: poller cadence (default 15m)
	IncludePrereleases bool          // release: also fire on prereleases

	// Legacy predicate fields: no connectors-model surface sets them, but a
	// legacy rules config does, and the intrinsic defaults lower them into
	// the filter IR (filter.go).
	Exclude      Exclude
	FromUsers    []string
	IgnoreUsers  []string
	AuthorBot    *bool
	LabelsAny    []string
	LabelsAll    []string
	Authors      []string
	SoleAssignee bool
	RequireLabel string
	Gates        map[string]any

	// Ext is the host's own action, merged by Config.MergeExt and handed back
	// on every Trigger this variant produces.
	Ext any
}

// IsEnabled reports whether the action is enabled (default true).
func (a Action) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// StuckAfterDur returns the stuck-check threshold, defaulting to 30m.
func (a Action) StuckAfterDur() time.Duration {
	if a.StuckAfter > 0 {
		return a.StuckAfter
	}
	return 30 * time.Minute
}

// PollIntervalDur returns the stuck_checks poll cadence, defaulting to 15m.
func (a Action) PollIntervalDur() time.Duration {
	if a.PollInterval > 0 {
		return a.PollInterval
	}
	return 15 * time.Minute
}

// ActionSet is one or more variants of a kind, in configured order.
type ActionSet []Action
