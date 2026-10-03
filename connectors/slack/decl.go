// Package main is the Slack connector as a standalone conductor plugin
// (docs/design/plugin-contract.md §4.2): Socket Mode events in (mentions,
// reactions, slash commands, message shortcuts, forms, thread replies/DMs),
// messages/reactions/asks/thread-reads/file-downloads out.
//
// It is a drop-in for conductor's former bundled slack connector: the same
// connection keys, event names, verbs and trigger options keep working
// unchanged. Where the builtin leaned on conductor-specific machinery (the
// hand-off inbox, ForceNoCheckout, a completion hook), this plugin instead
// DECLARES the generic semantic that covers it (plugin-contract.md §2) and
// lets the engine do the rest — see decl.go's doc comments on each one.
//
// Built ONLY against the public SDK (pkg/plugin) plus pkg/sourcekit's HMAC
// helper is NOT used (Socket Mode has no webhook to verify) — Slack
// Socket Mode is a plain outbound websocket, authenticated by the app token
// alone. The one third-party dependency is github.com/coder/websocket, the
// same client conductor's bundled connector used.
package main

import (
	"fmt"
	"os"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// Type is this connector's type name.
const Type = "slack"

// slackCtxSchema is the context every slack event publishes (nested under
// .slack), unchanged from the builtin plus #161's interactive additions.
func slackCtxSchema() plugin.Schema {
	return plugin.Schema{
		"slack.channel":   {Type: "string"},
		"slack.user":      {Type: "string"},
		"slack.text":      {Type: "string"},
		"slack.ts":        {Type: "string"},
		"slack.thread_ts": {Type: "string"},
		"slack.reaction":  {Type: "string"},
		"slack.command":   {Type: "string"},
		"slack.is_bot":    {Type: "boolean", Desc: "the acting user is a bot (bot_id was set)"},
		// #161 interactive additions; always present so a template never
		// trips on a missing key.
		"slack.form":        {Type: "map", Desc: "submitted form values by field name"},
		"slack.via":         {Type: "string", Desc: "shortcut | mention (how a form trigger fired)"},
		"slack.callback_id": {Type: "string", Desc: "message_shortcut: the shortcut's callback id"},
		"slack.files":       {Type: "list", Desc: "files on the triggering message: {id, name, mimetype}"},
		"slack.trigger_id":  {Type: "string", Desc: "slash_command: the invocation's unique trigger id (Slack mints no message ts for a command, so this is its target discriminator instead)"},
		// slack_bot_token sits OUTSIDE the .slack map (a sibling root fact),
		// exactly as the builtin published it, so an action can still reply
		// directly via the Web API with {{.slack_bot_token}}. secret:
		// (semantics.go) keeps it out of persisted state and agent prompts.
		"slack_bot_token": {Type: "string", Desc: "the connection's bot token"},
	}
}

// slackFormOptions are the per-trigger options of the form-capable events
// (app_mention, message_shortcut).
func slackFormOptions() plugin.Schema {
	return plugin.Schema{
		"form":     {Type: "map", Desc: "a modal to collect before firing: {title, submit, fields: [{name, label, type: select|text|textarea, options, default, optional}]}"},
		"any_user": {Type: "boolean", Desc: "allow a form/shortcut trigger with no users: filter (anyone in the workspace)"},
	}
}

// Decl is the full slack connector declaration.
func Decl() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: Type,
		Desc: "Slack: mentions/reactions/slash commands/shortcuts/forms in (Socket Mode); messages, reactions, asks, thread reads and file downloads out.",
		Connection: plugin.Schema{
			"app_token":   {Type: "string", Secret: true, Desc: "Socket Mode app token (xapp-…) — needed for events and ask replies"},
			"bot_token":   {Type: "string", Secret: true, Desc: "bot token (xoxb-…) for the Web API verbs"},
			"webhook_url": {Type: "string", Secret: true, Desc: "an incoming-webhook URL — post-only alternative to a bot token"},
			"api_base":    {Type: "string", Desc: "override the Slack Web API base URL (tests); default https://slack.com/api"},
		},
		Events: []plugin.Event{
			{
				Name: "app_mention", Desc: "the bot was @-mentioned (with options.form: the mentioning user gets a private button that opens the form; the trigger fires on submission)",
				Filters: plugin.Schema{
					"channel": {Type: "string", Desc: "only this channel id"},
					"users":   {Type: "list", Desc: "only these user ids"},
				},
				Options:   slackFormOptions(),
				Context:   slackCtxSchema(),
				Semantics: msgSemantics(),
			},
			{
				Name: "message_shortcut", Desc: "a message shortcut was used on a message (with options.form: opens the form first; the trigger fires on submission)",
				Filters: plugin.Schema{
					"callback_id": {Type: "string", Desc: "only this shortcut callback id"},
					"channel":     {Type: "string", Desc: "only this channel id"},
					"users":       {Type: "list", Desc: "only these user ids (required unless options.any_user)"},
				},
				Options:   slackFormOptions(),
				Context:   slackCtxSchema(),
				Semantics: msgSemantics(),
			},
			{
				Name: "reaction_added", Desc: "a reaction was added",
				Filters: plugin.Schema{
					"reaction": {Type: "string", Desc: "only this emoji name (no colons)"},
					"channel":  {Type: "string"},
					"users":    {Type: "list"},
				},
				Context:   slackCtxSchema(),
				Semantics: msgSemantics(),
			},
			{
				Name: "slash_command", Desc: "a slash command was invoked",
				Filters: plugin.Schema{
					"command": {Type: "string", Desc: "only this command (e.g. /fix)"},
					"channel": {Type: "string"},
					"users":   {Type: "list"},
				},
				Context:   slackCtxSchema(),
				Semantics: slashCommandSemantics(),
			},
			{
				Name: "reply", Desc: "a thread reply or a DM, as Socket Mode delivers them — consumed by a pending ask/hand-off when one is waiting on it, otherwise an ordinary event a trigger may match with `on: slack.reply`",
				Context:   slackCtxSchema(),
				Semantics: replySemantics(),
			},
		},
		Verbs: []plugin.Verb{
			{
				Name: "post", Desc: "post a message", Semantics: &plugin.VerbSemantics{ConversationPost: true},
				Options: plugin.Schema{
					"channel":   {Type: "string", Scope: "channel", Desc: "channel id (or set user: for a DM)"},
					"user":      {Type: "string", Scope: "user", Desc: "user id to DM"},
					"text":      {Type: "string", Required: true},
					"thread_ts": {Type: "string", Desc: "post into this thread"},
					"ephemeral": {Type: "boolean", Desc: "visible only to user: (requires channel: and user:)"},
				},
				Outputs: plugin.Schema{"ts": {Type: "string"}, "channel": {Type: "string"}},
			},
			{
				Name: "react", Desc: "add a reaction to a message",
				Options: plugin.Schema{
					"channel": {Type: "string", Required: true, Scope: "channel"},
					"ts":      {Type: "string", Required: true, Desc: "message timestamp to react to"},
					"emoji":   {Type: "string", Required: true, Desc: "emoji name, no colons"},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "ask", Desc: "present a question/draft and wait for the reply", Ask: true,
				Semantics: &plugin.VerbSemantics{OpensConversation: &plugin.OpensConversation{ID: "conversation_id", Approvers: "approvers"}},
				Options: mergeSchema(askOptionBase(), plugin.Schema{
					"to":        {Type: "string", Enum: []string{"dm", "thread"}, Required: true},
					"user":      {Type: "string", Scope: "user", Desc: "user id (to: dm)"},
					"channel":   {Type: "string", Scope: "channel", Desc: "channel id (to: thread)"},
					"approvers": {Type: "list", Desc: "to: thread — only these user ids may resolve the ask (default: anyone in the channel)"},
				}),
				Outputs: plugin.Schema{"conversation_id": {Type: "string"}, "ref": {Type: "string"}, "ts": {Type: "string"}, "channel": {Type: "string"}},
			},
			{
				Name: "thread", Desc: "read a message's thread: ordered messages with author names, file metadata, and a permalink",
				Options: plugin.Schema{
					"channel": {Type: "string", Required: true, Scope: "channel"},
					"ts":      {Type: "string", Required: true, Desc: "the thread's root ts (or any message in it)"},
					"limit":   {Type: "integer", Desc: fmt.Sprintf("max messages (default %d, at most %d)", defaultThreadLimit, maxThreadLimit)},
				},
				Outputs: plugin.Schema{
					"messages":  {Type: "list", Desc: "[{user, user_name, text, ts, files: [{id, name, mimetype, size}]}], oldest first"},
					"text":      {Type: "string", Desc: "the thread as plain text, one message per block"},
					"permalink": {Type: "string"},
					"thread_ts": {Type: "string"},
					"count":     {Type: "integer"},
					"truncated": {Type: "boolean", Desc: "the thread had more messages than limit"},
				},
			},
			{
				Name: "download", Desc: "download a message's (or its whole thread's) files into the plugin's staging directory",
				Options: plugin.Schema{
					"channel":         {Type: "string", Required: true, Scope: "channel"},
					"ts":              {Type: "string", Required: true, Desc: "the thread's root ts (or the message's own ts with thread: false)"},
					"thread":          {Type: "boolean", Desc: "every file in the thread (default true); false: only the message at ts"},
					"max_files":       {Type: "integer", Desc: fmt.Sprintf("default %d, at most %d", defaultMaxFiles, capMaxFiles)},
					"max_file_bytes":  {Type: "integer", Desc: fmt.Sprintf("default %d, at most %d", defaultMaxFileBytes, capMaxFileBytes)},
					"max_total_bytes": {Type: "integer", Desc: fmt.Sprintf("default %d, at most %d", defaultMaxTotalBytes, capMaxTotalBytes)},
				},
				Outputs: plugin.Schema{
					"dir":     {Type: "string", Desc: "the staging directory"},
					"files":   {Type: "list", Desc: "[{name, path, mimetype, size}]"},
					"paths":   {Type: "list"},
					"images":  {Type: "list", Desc: "paths of the image/* files (for an agent step's images:)"},
					"skipped": {Type: "list", Desc: "[{name, reason}] files not downloaded"},
					"count":   {Type: "integer"},
				},
			},
		},
		// Socket Mode dials out to Slack only; it listens on nothing and
		// spawns nothing. The Web API lives on slack.com itself, but the
		// Socket Mode WSS URL apps.connections.open returns, and Slack's
		// file download/upload URLs, are on subdomains (wss-primary.slack.com
		// and files.slack.com, among others) — declaring only the bare host
		// would have conductor's egress sandbox (internal/sandbox.go,
		// EgressAllowed) block them.
		Capabilities: plugin.Capabilities{Egress: []string{"slack.com:443", "*.slack.com:443"}},
	}
}

// mergeSchema overlays b onto a copy of a.
func mergeSchema(a, b plugin.Schema) plugin.Schema {
	out := plugin.Schema{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// askOutputs/askOptionBase mirror the shared `ask` shape every interactive
// connector in conductor's own tree declares (internal/connector/ask.go);
// reproduced here since a plugin may not import conductor's internal
// packages.
func askOptionBase() plugin.Schema {
	return plugin.Schema{
		"prompt":  {Type: "string", Required: true, Desc: "the question to present"},
		"draft":   {Type: "string", Desc: "editable draft text presented with the question"},
		"title":   {Type: "string", Desc: "presentation title (default: the prompt's first line)"},
		"timeout": {Type: "string", Desc: "how long to wait for an answer (default 1h) — enforced by the host"},
	}
}

func main() {
	if err := plugin.Serve(New()); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-slack: %v\n", err)
		os.Exit(1)
	}
}
