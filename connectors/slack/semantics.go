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
