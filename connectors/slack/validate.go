package main

import (
	"context"
	"fmt"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// Validate is plugin.validate: the plugin's own config and trigger checks,
// run at load and by `conductor validate`. This is what closes the gap the
// contract calls out (docs/design/plugin-contract.md §1.5): the builtin's
// form/shortcut `users:` rule used to be enforced ONLY by a bundled-only
// `TypeDecl.ValidateTrigger` hook a plugin could never reach; here it is the
// plugin's own optional method, run the same way for every plugin.
func (p *Plugin) Validate(_ context.Context, req plugin.ValidateRequest) (plugin.ValidateResult, error) {
	var problems []plugin.Problem
	appToken := str(req.Config["app_token"])
	botToken := str(req.Config["bot_token"])
	webhookURL := str(req.Config["webhook_url"])
	if botToken == "" && webhookURL == "" {
		problems = append(problems, plugin.Problem{Path: "bot_token", Message: "set bot_token (Web API) or webhook_url (post-only)"})
	}
	if len(req.Triggers) > 0 && appToken == "" {
		problems = append(problems, plugin.Problem{Path: "app_token", Message: "app_token is required to receive slack events (xapp- Socket Mode token)"})
	}
	for i, t := range req.Triggers {
		where := fmt.Sprintf("triggers[%d]", i)
		for _, name := range []string{"ack", "on_done", "on_fail"} {
			problems = append(problems, validateFeedbackOption(fmt.Sprintf("%s.options.%s", where, name), t.Options[name])...)
		}
		form, err := ParseForm(t.Options["form"])
		if err != nil {
			problems = append(problems, plugin.Problem{Path: where + ".options.form", Message: err.Error()})
			continue
		}
		if form != nil && t.Event != "app_mention" && t.Event != "message_shortcut" {
			problems = append(problems, plugin.Problem{Path: where + ".options.form", Message: "options.form applies to app_mention and message_shortcut only"})
			continue
		}
		if form == nil && t.Event != "message_shortcut" {
			continue
		}
		if truthy(t.Options["any_user"]) {
			continue
		}
		ct, cerr := compileTriggers([]plugin.SourceTrigger{t})
		if cerr != nil {
			problems = append(problems, plugin.Problem{Path: where, Message: cerr.Error()})
			continue
		}
		if len(ct) == 1 && len(ct[0].users) == 0 {
			kind := "message_shortcut"
			if form != nil {
				kind = "form"
			}
			problems = append(problems, plugin.Problem{Path: where + ".filter.users", Message: fmt.Sprintf(
				"a %s trigger needs a non-empty `users:` filter (Slack user ids allowed to use it), or options.any_user: true to allow anyone in the workspace", kind)})
		}
	}
	return plugin.ValidateResult{Problems: problems}, nil
}

// validateFeedbackOption checks one ack/on_done/on_fail block: absent (nil)
// is fine (silence), but a present block must be a map naming at least one
// of react/say, and ephemeral only makes sense alongside say — the same
// rules the old builtin's Feedback.empty()/Validate enforced, now run here
// since the engine's generic option_hooks semantic only checks the block is
// shaped like a map (plugin-contract.md §1.5's plugin.validate is exactly
// the seam for a plugin's own, non-generic trigger checks).
func validateFeedbackOption(path string, v any) []plugin.Problem {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return []plugin.Problem{{Path: path, Message: "must be a map: {react, say, ephemeral, in_thread}"}}
	}
	react := str(m["react"])
	say := str(m["say"])
	if react == "" && say == "" {
		return []plugin.Problem{{Path: path, Message: "neither react nor say is set; omit it for silence instead of an empty block"}}
	}
	if truthy(m["ephemeral"]) && say == "" {
		return []plugin.Problem{{Path: path, Message: "ephemeral only applies to say"}}
	}
	return nil
}
