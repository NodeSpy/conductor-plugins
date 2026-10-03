package main

import (
	"encoding/json"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// What the ENGINE does with each slack event and verb, declared in the
// contract's generic terms (docs/design/plugin-contract.md §4.2). Conductor
// implements each semantic once for every plugin; nothing in it knows these
// names.
//
// This REPLACES the builtin's bespoke wiring:
//   - the per-channel "trusted" synthetic target (inbound.SyntheticTarget)
//     becomes a per-MESSAGE target (target.key templated on channel+ts),
//     assigned because Slack itself chose the channel/ts, not the sender;
//   - the channel/user resource-scoping hook (slackImpl.ContextScope) becomes
//     target.scope's channel/user dimensions;
//   - the hand-off Inbox + SetReplyHook fan-in becomes the generic
//     conversation_reply routing: a "reply" event with no pending ask on it
//     is simply an ordinary event.

// msgSemantics is the semantics every slack message-shaped event shares: a
// per-message target (channel assigned it, not the sender), the acting
// user as author, and the bot token marked secret (never persisted or
// rendered into an agent prompt).
func msgSemantics() *plugin.EventSemantics {
	return &plugin.EventSemantics{
		Target: &plugin.TargetSemantics{
			Key: "slack:{{.slack.channel}}:{{.slack.ts}}", Label: "message",
			Assigned: json.RawMessage(`true`),
			Scope: []plugin.ScopeFact{
				{Dimension: "channel", Fact: "slack.channel"},
				{Dimension: "user", Fact: "slack.user"},
			},
		},
		Author: &plugin.AuthorSemantics{Login: "slack.user", Automated: "slack.is_bot"},
		Secret: []string{"slack_bot_token"},
	}
}

// feedbackOptionHooks restores the builtin's old per-trigger `ack`/`on_done`/
// `on_fail` feedback (an emoji reaction and/or a message, fired at dispatch
// and when the dispatched work finishes) as the generic `option_hooks`
// semantic (plugin-contract.md §2.2): whichever of these the trigger's own
// `options:` sets fires the `feedback` verb at the matching run phase. Args
// carries the event's own message coordinates, so the verb always acts on
// the message that triggered the run, not whatever the operator's own
// react/say block happens to mention.
func feedbackOptionHooks() []plugin.OptionHook {
	args := map[string]string{
		"channel": "{{.slack.channel}}", "ts": "{{.slack.ts}}",
		"user": "{{.slack.user}}", "thread_ts": "{{.slack.thread_ts}}",
	}
	return []plugin.OptionHook{
		{Option: "ack", At: "start", Verb: "feedback", Args: args},
		{Option: "on_done", At: "done", Verb: "feedback", Args: args},
		{Option: "on_fail", At: "fail", Verb: "feedback", Args: args},
	}
}

// feedbackOptions is the trigger-options schema for ack/on_done/on_fail,
// documented on every event that carries feedbackOptionHooks so `conductor
// validate` checks a trigger's blocks are at least shaped like a map (full
// cross-field checks — e.g. "ephemeral only applies to say" — are this
// plugin's own Validate, as they always were).
func feedbackOptions() plugin.Schema {
	desc := "{react, say, ephemeral, in_thread}: an optional reaction and/or message on the triggering message"
	return plugin.Schema{
		"ack":     {Type: "map", Desc: "fired when the trigger dispatches — " + desc},
		"on_done": {Type: "map", Desc: "fired when the dispatched work finishes successfully — " + desc},
		"on_fail": {Type: "map", Desc: "fired when the dispatched work fails — " + desc},
	}
}

// withFeedback attaches feedbackOptionHooks to s and returns it, for the
// events that carry ack/on_done/on_fail.
func withFeedback(s *plugin.EventSemantics) *plugin.EventSemantics {
	s.OptionHooks = feedbackOptionHooks()
	return s
}

// replySemantics is msgSemantics plus conversation_reply: a thread reply or
// DM message resolves a pending ask/hand-off on the same (channel,
// thread_ts) pair; unconsumed, it is an ordinary "reply" event a trigger may
// still match on.
func replySemantics() *plugin.EventSemantics {
	s := msgSemantics()
	s.ConversationReply = &plugin.ConversationReply{
		ID: "{{.slack.channel}}:{{.slack.thread_ts}}", Author: "slack.user", Text: "slack.text",
	}
	return s
}

// slashCommandSemantics is msgSemantics with ONE difference: the target key.
// A slash command carries no message ts (Slack never attaches one to the
// invocation), so keying it on {{.slack.ts}} like every other event would
// collapse every command fired in a channel onto one target — the per-
// invocation `trigger_id` Slack does always mint is the discriminator
// instead (events.go's discriminator keeps the wire event's Target.Key
// in agreement with this template).
func slashCommandSemantics() *plugin.EventSemantics {
	s := msgSemantics()
	s.Target.Key = "slack:{{.slack.channel}}:{{.slack.trigger_id}}"
	return s
}
