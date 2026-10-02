package ghsource

import (
	"fmt"
	"time"

	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// The connectors model, lowered. A `connectors:` github entry plus the
// `triggers:` that name it become one source Config here, and nowhere else:
// conductor's bundled connector and the conductor-github plugin both call
// LowerTrigger per trigger and Connection.SourceConfig once, so the same
// config builds the same source in either.

// TriggerSpec is one configured trigger as a host hands it over.
type TriggerSpec struct {
	// Name is the trigger's name — the variant its triggers carry.
	Name string
	// Event is the event it is on ("new_comment" for `on: gh.new_comment`).
	Event string
	// Enabled is its enabled switch (nil = enabled).
	Enabled *bool
	// Options are its event `options:`.
	Options map[string]any
	// Filter is its `filter:` (nil = none).
	Filter *sourcekit.Filter
	// Ext is the host's own payload for the trigger; it rides on the lowered
	// Action and comes back on every Trigger the trigger fires.
	Ext any
}

// LowerTrigger lowers one trigger to the Action the source evaluates.
//
// The trigger's whole predicate is its `filter:`, which rides through as the
// IR untouched. Two things are read OUT of it, because they are answered
// structurally rather than per event:
//
//   - `repo` scopes the trigger (Action.Repos). emit() gates on it before any
//     keep-condition, and the stuck_checks poller and the sweep derive their
//     repo scope from it — neither of which has an event to evaluate against.
//     The union across the WHOLE filter is taken, so an OR of repo sets is a
//     superset rather than a miss; the filter itself still decides precisely.
//     No repo anywhere falls back to defaultRepos (the connector's `repos:`).
//   - top-level `not_repo` excludes (Action.ExcludeRepos).
//
// A filter that is nothing but a flat conjunction of those two routing keys
// leaves Action.Filter nil, so the event's intrinsic default keep-condition
// still applies: stating where a trigger applies is not stating what it wants
// of the event (merge_ready keeps its all-green gates). The shape has to be
// FLAT for that — an Or of repo sets is more than the pre-gate can carry, so
// it stays in the filter and is evaluated.
//
// The event options the source reads are lowered too (reviewer, assignee,
// ignore_checks, include_prereleases, stuck_after, poll_interval). Options the
// HOST's engine reads (max_attempts_per_head, flaky_rerun) are not the
// source's business and are left to the host.
func LowerTrigger(t TriggerSpec, defaultRepos []string) (Action, error) {
	act := Action{Name: t.Name, Enabled: t.Enabled, Ext: t.Ext}
	f := t.Filter
	if !f.FlatConjunctionOf(FilterRepoKey) {
		act.Filter = f
	}
	act.Repos = f.MatchUnion(FilterRepoKey)
	if len(act.Repos) == 0 {
		act.Repos = defaultRepos
	}
	act.ExcludeRepos = f.TopLevelNegated(FilterRepoKey)
	o := t.Options
	act.Reviewer = optActors(o["reviewer"])
	act.Assignee = optActors(o["assignee"])
	act.IgnoreChecks = optStrings(o["ignore_checks"])
	act.IncludePrereleases, _ = o["include_prereleases"].(bool)
	d, err := optDuration(o["stuck_after"])
	if err != nil {
		return act, fmt.Errorf("options.stuck_after: %w", err)
	}
	act.StuckAfter = d
	if d, err = optDuration(o["poll_interval"]); err != nil {
		return act, fmt.Errorf("options.poll_interval: %w", err)
	}
	act.PollInterval = d
	return act, nil
}

// Connection is a github connector's connection block — the fields of its
// `connectors:` entry the source needs.
type Connection struct {
	App            AppConfig
	Token          string
	Webhook        WebhookConfig
	Sweep          SweepConfig
	Me             Actors
	Repos          []string
	Identity       Identity
	ProjectMap     map[string]string
	ProjectRewrite ProjectRewrite
	APIBase        string
}

// SourceConfig builds the source Config for this connection carrying the
// lowered triggers, grouped by event kind in configured order.
//
// Every trigger becomes a variant of its kind on ONE catch-all rule: rule
// resolution only matches explicit rules (defaults never fire on their own),
// and each variant's own repo gates scope it — so triggers stay independent
// (every matching trigger fires) while the source evaluates each one's filter
// per event. An enabled sweep with no repos of its own sweeps the connector's
// `repos:`.
func (c Connection) SourceConfig(actions map[string]ActionSet) Config {
	sweep := c.Sweep
	if sweep.IsEnabled() && len(sweep.Repos) == 0 {
		sweep.Repos = c.Repos
	}
	return Config{
		App:            c.App,
		Token:          c.Token,
		Webhook:        c.Webhook,
		Sweep:          sweep,
		Identity:       c.Identity,
		ProjectMap:     c.ProjectMap,
		ProjectRewrite: c.ProjectRewrite,
		APIBase:        c.APIBase,
		Defaults:       Rule{Me: c.Me},
		Rules: []Rule{{
			Match:   Match{Repos: []string{"*/*"}},
			Actions: actions,
		}},
	}
}

// --- option coercion: the shapes YAML and JSON decode options into ---------

func optStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			} else {
				out = append(out, fmt.Sprintf("%v", e))
			}
		}
		return out
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	}
	return nil
}

// optActors coerces { logins: [...], teams: [...] } (or a bare list = logins).
func optActors(v any) Actors {
	switch x := v.(type) {
	case map[string]any:
		return Actors{Logins: optStrings(x["logins"]), Teams: optStrings(x["teams"])}
	case []any, []string:
		return Actors{Logins: optStrings(x)}
	}
	return Actors{}
}

// optDuration coerces a duration string or integer seconds ("" -> 0).
func optDuration(v any) (time.Duration, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case string:
		if x == "" {
			return 0, nil
		}
		return time.ParseDuration(x)
	case int:
		return time.Duration(x) * time.Second, nil
	case int64:
		return time.Duration(x) * time.Second, nil
	case float64:
		return time.Duration(x) * time.Second, nil
	}
	return 0, fmt.Errorf("want a duration, got %T", v)
}
