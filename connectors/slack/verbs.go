package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// wrapUpstream turns a slackAPI call's error into the verb's answer: a
// contract code slackAPIError already classified (rate_limited, upstream) is
// passed through as-is — the engine's retry/stop behavior is keyed on it —
// anything else falls back to the generic upstream error the call sites used
// before classification existed.
func wrapUpstream(prefix string, err error) error {
	var pe *plugin.Error
	if errors.As(err, &pe) {
		return pe
	}
	return plugin.Errorf(plugin.CodeUpstream, prefix+err.Error())
}

// remapTargetGone promotes a channel_not_found/message_not_found upstream
// answer (slackAPIError tags it with Data["slack_error"]) into target_gone
// keyed on key — the SAME string semantics.go's target.key template renders
// for the message/thread this call addresses ("slack:{{.channel}}:{{.ts}}").
//
// Call it ONLY where the call addresses an event's OWN message/thread:
// react's triggering message, or a post into the thread that message
// started (thread_ts set). Never for a destination channel/user a post or
// ask merely posts NEW content to — a channel no event ever targeted going
// missing is a configuration problem, not a sign the run's target closed;
// those stay the upstream{retryable:false} slackAPIError already built.
func remapTargetGone(err error, key string) error {
	var pe *plugin.Error
	if !errors.As(err, &pe) || pe.Code != plugin.CodeUpstream {
		return err
	}
	se, _ := pe.Data["slack_error"].(string)
	if se != "channel_not_found" && se != "message_not_found" {
		return err
	}
	return plugin.Fail(plugin.CodeTargetGone, pe.Message, map[string]any{"target": key})
}

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
			return plugin.InvokeResult{}, wrapUpstream("slack.post: ", err)
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
			return plugin.InvokeResult{}, wrapUpstream("slack.post: ", err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"ts": "", "channel": channel}}, nil
	}
	if channel == "" {
		dm, err := api.openDM(ctx, user)
		if err != nil {
			return plugin.InvokeResult{}, wrapUpstream("slack.post: open dm: ", err)
		}
		channel = dm
	}
	threadTS, _ := opts["thread_ts"].(string)
	ts, err := api.postMessage(ctx, channel, threadTS, text)
	if err != nil {
		wrapped := wrapUpstream("slack.post: ", err)
		if threadTS != "" {
			// This post addresses the thread's root message (the event's own
			// target when it's a reply in that thread) — a gone channel or
			// thread is target_gone; a bare post to a channel (threadTS=="")
			// is a destination and stays upstream.
			wrapped = remapTargetGone(wrapped, fmt.Sprintf("slack:%s:%s", channel, threadTS))
		}
		return plugin.InvokeResult{}, wrapped
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
		// react always addresses a specific message — the event's own
		// triggering message, in every configured use of this verb — so a
		// gone channel/message here is always target_gone.
		return plugin.InvokeResult{}, remapTargetGone(wrapUpstream("slack.react: ", err), fmt.Sprintf("slack:%s:%s", channel, ts))
	}
	return plugin.InvokeResult{Outputs: map[string]any{"ok": true}}, nil
}

// feedbackVerb is the generic ack/on_done/on_fail block, restored byte-for-
// byte from the old builtin's Feedback shape (react, say, ephemeral,
// in_thread) — only now invoked by the engine's generic option_hooks
// semantic instead of a vendor-specific completion hook. channel/ts/user/
// thread_ts come from the triggering event's own facts (feedbackOptionHooks'
// Args); react/say/ephemeral/in_thread come from the operator's own
// ack/on_done/on_fail block verbatim. Best-effort by construction — a hook
// invocation never fails the run — so a missing react target is skipped
// rather than erroring; only "neither react nor say is set" (a config
// mistake, not a runtime gap) is reported as an error.
func (p *Plugin) feedbackVerb(ctx context.Context, api *slackAPI, botToken string, opts map[string]any) (plugin.InvokeResult, error) {
	channel, _ := opts["channel"].(string)
	ts, _ := opts["ts"].(string)
	user, _ := opts["user"].(string)
	threadTS, _ := opts["thread_ts"].(string)
	react, _ := opts["react"].(string)
	say, _ := opts["say"].(string)
	if react == "" && say == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams,
			"slack.feedback: neither react nor say is set; omit the block for silence instead of an empty one")
	}
	if botToken == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.feedback needs a bot_token (the webhook_url connection is post-only)")
	}
	if react != "" {
		if channel == "" || ts == "" {
			// No triggering message to react to (a synthetic/degraded
			// event) — skip the reaction, still try the say below.
		} else if err := api.react(ctx, channel, ts, strings.Trim(react, ":")); err != nil {
			return plugin.InvokeResult{}, remapTargetGone(wrapUpstream("slack.feedback: ", err), fmt.Sprintf("slack:%s:%s", channel, ts))
		}
	}
	if say == "" {
		return plugin.InvokeResult{Outputs: map[string]any{"ok": true}}, nil
	}
	inThread := true
	if v, ok := opts["in_thread"]; ok {
		inThread, _ = v.(bool)
	}
	if truthy(opts["ephemeral"]) && channel != "" && user != "" {
		if err := api.postEphemeral(ctx, channel, user, say); err != nil {
			return plugin.InvokeResult{}, wrapUpstream("slack.feedback: ", err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"ok": true}}, nil
	}
	// ephemeral requested but no user to send it to: fall back to a normal
	// post, same as the old builtin did, rather than silently dropping it.
	if channel == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.feedback: say has no channel to post to")
	}
	tts := ""
	if inThread {
		tts = threadTS
	}
	if _, err := api.postMessage(ctx, channel, tts, say); err != nil {
		wrapped := wrapUpstream("slack.feedback: ", err)
		if tts != "" {
			wrapped = remapTargetGone(wrapped, fmt.Sprintf("slack:%s:%s", channel, tts))
		}
		return plugin.InvokeResult{}, wrapped
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
			return plugin.InvokeResult{}, wrapUpstream("slack.ask: open dm: ", err)
		}
		ts, err := api.postMessage(ctx, channel, "", text)
		if err != nil {
			return plugin.InvokeResult{}, wrapUpstream("slack.ask: post: ", err)
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
			return plugin.InvokeResult{}, wrapUpstream("slack.ask: post: ", err)
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
