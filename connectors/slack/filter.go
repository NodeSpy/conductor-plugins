package main

import (
	"encoding/json"
	"fmt"

	"github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// compiledTrigger is one configured trigger (plugin.SourceTrigger), lowered
// once at start_source: its event, enabled switch, structural filter, and
// (for app_mention/message_shortcut) its form and the `users:` it requires
// on every match path.
//
// This is the plugin evaluating its OWN match keys, the replacement for the
// deleted TypeDecl.Filter hook (plugin-contract.md G12): the host hands over
// the trigger's filter already parsed (pkg/sourcekit.Filter) and the plugin
// is who knows what "channel", "users", "reaction", "command" and
// "callback_id" mean.
type compiledTrigger struct {
	id      string
	name    string
	event   string
	enabled bool
	filter  *sourcekit.Filter
	form    *Form
	anyUser bool
	// users is the set of user ids EVERY match path through filter requires
	// (see requiredMatchValues) — what #161 calls the form/shortcut
	// safety rule.
	users []string
}

// compileTriggers lowers the triggers a start_source/validate/translate
// request carries.
func compileTriggers(triggers []plugin.SourceTrigger) ([]compiledTrigger, error) {
	out := make([]compiledTrigger, 0, len(triggers))
	for _, t := range triggers {
		enabled := t.Enabled == nil || *t.Enabled
		var f *sourcekit.Filter
		if len(t.Filter) > 0 && string(t.Filter) != "null" {
			f = new(sourcekit.Filter)
			if err := json.Unmarshal(t.Filter, f); err != nil {
				return nil, fmt.Errorf("trigger %s: filter: %w", t.ID, err)
			}
		}
		form, err := ParseForm(t.Options["form"])
		if err != nil {
			return nil, fmt.Errorf("trigger %s: %w", t.ID, err)
		}
		if form != nil && t.Event != "app_mention" && t.Event != "message_shortcut" {
			return nil, fmt.Errorf("trigger %s: options.form applies to app_mention and message_shortcut only", t.ID)
		}
		out = append(out, compiledTrigger{
			id: t.ID, name: t.Name, event: t.Event, enabled: enabled, filter: f, form: form,
			anyUser: truthy(t.Options["any_user"]),
			users:   requiredMatchValues(f, "users"),
		})
	}
	return out, nil
}

// needsUsers reports whether this trigger must name a non-empty `users:`
// filter (a form, or a bare message_shortcut), per #161's safety rule.
func (ct compiledTrigger) needsUsers() bool {
	return ct.form != nil || ct.event == "message_shortcut"
}

// formOrShortcutOK reports whether the users:/any_user: rule is satisfied.
func (ct compiledTrigger) formOrShortcutOK() bool {
	return !ct.needsUsers() || ct.anyUser || len(ct.users) > 0
}

// match evaluates an ordinary trigger's filter (app_mention without a form,
// reaction_added, slash_command, reply) against the event's facts.
func (ct compiledTrigger) match(facts map[string]any) bool {
	if ct.filter == nil {
		return true
	}
	ok, err := ct.filter.Eval(facts, slackMatch)
	return err == nil && ok
}

// preMatch decides, at shortcut/mention time, whether a form or shortcut
// trigger applies: the users: gate (the trigger's own filter, or any_user),
// then its filter.
func (ct compiledTrigger) preMatch(facts map[string]any) bool {
	user, _ := facts["user"].(string)
	if len(ct.users) > 0 {
		if !containsFold(ct.users, user) {
			return false
		}
	} else if !ct.anyUser && ct.needsUsers() {
		return false
	}
	return ct.match(facts)
}

// slackMatch is the single implementation of every slack filter key: channel,
// users, reaction, command, callback_id. facts carries the flat event
// values (channel, user, reaction, command, callback_id) — not the
// nested `.slack.*` publish shape, which is a presentation detail for
// templates, not for matching.
func slackMatch(key string, val any, facts map[string]any) (bool, error) {
	switch key {
	case "channel":
		want, _ := val.(string)
		return want == "" || want == str(facts["channel"]), nil
	case "users":
		users := sourcekit.FilterValueStrings(val)
		if len(users) == 0 {
			return true, nil
		}
		return containsFold(users, str(facts["user"])), nil
	case "reaction":
		want, _ := val.(string)
		return want == "" || want == str(facts["reaction"]), nil
	case "command":
		want, _ := val.(string)
		return want == "" || want == str(facts["command"]), nil
	case "callback_id":
		want, _ := val.(string)
		return want == "" || want == str(facts["callback_id"]), nil
	}
	return false, fmt.Errorf("slack: unknown filter key %q", key)
}

// requiredMatchValues returns the string values of match key `key` that
// EVERY path through filter f requires: a non-negated match under ANDs, or
// one on each branch of an OR (their union). nil when some path to a match
// does not require the key, or f is nil.
func requiredMatchValues(f *sourcekit.Filter, key string) []string {
	if f == nil {
		return nil
	}
	switch f.Op {
	case sourcekit.FilterOpMatch:
		if f.Key == key {
			return sourcekit.FilterValueStrings(f.Val)
		}
	case sourcekit.FilterOpAnd:
		for _, k := range f.Kids {
			if v := requiredMatchValues(k, key); len(v) > 0 {
				return v
			}
		}
	case sourcekit.FilterOpOr:
		var out []string
		for _, k := range f.Kids {
			v := requiredMatchValues(k, key)
			if len(v) == 0 {
				return nil
			}
			out = append(out, v...)
		}
		return out
	}
	return nil
}
