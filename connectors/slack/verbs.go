package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

func (p *Plugin) postVerb(ctx context.Context, api *slackAPI, botToken, webhookURL string, opts map[string]any) (plugin.InvokeResult, error) {
	channel, _ := opts["channel"].(string)
	user, _ := opts["user"].(string)
	text, _ := opts["text"].(string)
	if text == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.post: options.text is required")
	}
	// Webhook-only connection: the incoming webhook IS the channel — post
	// {"text": …} there (byte-identical to the legacy notify sink).
	if botToken == "" {
		if webhookURL == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.post: connection needs bot_token or webhook_url")
		}
		if err := postIncomingWebhook(ctx, api.httpc, webhookURL, map[string]string{"text": text}); err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeUpstream, "slack.post: "+err.Error())
		}
		return plugin.InvokeResult{Outputs: map[string]any{"ts": "", "channel": ""}}, nil
	}
	if channel == "" && user == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.post: set options.channel or options.user")
	}
	if truthy(opts["ephemeral"]) {
		if channel == "" || user == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.post: ephemeral needs both channel and user")
		}
		if err := api.postEphemeral(ctx, channel, user, text); err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeUpstream, err.Error())
		}
		return plugin.InvokeResult{Outputs: map[string]any{"ts": "", "channel": channel}}, nil
	}
	if channel == "" {
		dm, err := api.openDM(ctx, user)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeUpstream, "slack.post: open dm: "+err.Error())
		}
		channel = dm
	}
	threadTS, _ := opts["thread_ts"].(string)
	ts, err := api.postMessage(ctx, channel, threadTS, text)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeUpstream, err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"ts": ts, "channel": channel}}, nil
}

func (p *Plugin) reactVerb(ctx context.Context, api *slackAPI, botToken string, opts map[string]any) (plugin.InvokeResult, error) {
	if botToken == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.react needs a bot_token (the webhook_url connection is post-only)")
	}
	channel, _ := opts["channel"].(string)
	ts, _ := opts["ts"].(string)
	emoji, _ := opts["emoji"].(string)
	if channel == "" || ts == "" || emoji == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.react: options.channel, ts, and emoji are required")
	}
	if err := api.react(ctx, channel, ts, strings.Trim(emoji, ":")); err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeUpstream, err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"ok": true}}, nil
}

// askVerb posts a draft (thread or DM) and returns the conversation id the
// host registers the pending hand-off under (opens_conversation: the engine,
// not this plugin, waits for the reply and enforces approvers — see
// semantics.go's replySemantics and decl.go's ask verb declaration).
//
// The conversation id scheme MUST agree with what a "reply" event's
// conversation_reply.id template produces for the same thread/DM
// ("{{.slack.channel}}:{{.slack.thread_ts}}"), so askVerb returns exactly
// channel+":"+rootTS — rootTS being the posted message's ts (thread mode:
// any reply's thread_ts will equal it) or "" (dm mode: an unthreaded DM
// message's thread_ts is empty).
func (p *Plugin) askVerb(ctx context.Context, api *slackAPI, botToken string, opts map[string]any) (plugin.InvokeResult, error) {
	if botToken == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.ask needs a bot_token and app_token (the webhook_url connection is post-only)")
	}
	to, _ := opts["to"].(string)
	text := renderAskDraft(opts)
	switch to {
	case "dm":
		user, _ := opts["user"].(string)
		if user == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.ask: to: dm needs options.user (a Slack user id)")
		}
		channel, err := api.openDM(ctx, user)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeUpstream, "slack.ask: open dm: "+err.Error())
		}
		ts, err := api.postMessage(ctx, channel, "", text)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeUpstream, "slack.ask: post: "+err.Error())
		}
		return plugin.InvokeResult{Outputs: map[string]any{
			"conversation_id": channel + ":", "ref": "slack:" + channel + ":dm", "ts": ts, "channel": channel,
		}}, nil
	case "thread":
		channel, _ := opts["channel"].(string)
		if channel == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.ask: to: thread needs options.channel")
		}
		ts, err := api.postMessage(ctx, channel, "", text)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeUpstream, "slack.ask: post: "+err.Error())
		}
		return plugin.InvokeResult{Outputs: map[string]any{
			"conversation_id": channel + ":" + ts, "ref": "slack:" + channel + ":" + ts, "ts": ts, "channel": channel,
		}}, nil
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, fmt.Sprintf("slack.ask: options.to must be dm|thread, got %q", to))
}

// renderAskDraft builds the message text for an ask: the title (bold) plus
// the draft (falling back to the prompt), with the reply instructions the
// builtin's hand-off channel appended — approve/discard/revise now
// parsed by the host's generic conversation inbox, not this plugin.
func renderAskDraft(opts map[string]any) string {
	prompt, _ := opts["prompt"].(string)
	title, _ := opts["title"].(string)
	if title == "" {
		title = firstLine(prompt)
	}
	body, _ := opts["draft"].(string)
	if body == "" {
		body = prompt
	}
	var b strings.Builder
	if title != "" {
		b.WriteString("*")
		b.WriteString(title)
		b.WriteString("*\n")
	}
	b.WriteString(body)
	b.WriteString("\n\n_Reply in this thread:_ `approve`, `discard`, or the revised text to send back to the agent.")
	return b.String()
}

// postIncomingWebhook POSTs a JSON payload to a Slack incoming webhook —
// the post-only transport the builtin's webhook_url connection used, kept
// byte-identical (internal/connector/notifysinks.go's helper, ported since a
// plugin cannot import conductor's internal packages).
func postIncomingWebhook(ctx context.Context, hc *http.Client, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook post: HTTP %d", resp.StatusCode)
	}
	return nil
}
