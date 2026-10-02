package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// eventCallback is the Events API payload delivered inside an events_api
// envelope.
type eventCallback struct {
	Event struct {
		Type        string      `json:"type"`
		Text        string      `json:"text"`
		User        string      `json:"user"`
		Channel     string      `json:"channel"`
		ChannelType string      `json:"channel_type"` // "im" for a DM; only present on message events
		TS          string      `json:"ts"`
		ThreadTS    string      `json:"thread_ts"`
		Reaction    string      `json:"reaction"`
		BotID       string      `json:"bot_id"` // set on a message/reaction a bot posted
		Files       []slackFile `json:"files"`
		Item        struct {
			Channel string `json:"channel"`
			TS      string `json:"ts"`
		} `json:"item"`
	} `json:"event"`
}

// slashPayload is the slash_commands envelope payload.
type slashPayload struct {
	Command   string `json:"command"`
	Text      string `json:"text"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
}

// evt is the normalized Slack event the plugin routes and templates on.
type evt struct {
	text, user, channel, ts, threadTS, reaction, command string
	via, callbackID                                      string
	isBot                                                bool
	files                                                []slackFile
	form                                                 map[string]any
}

// facts is what filter matching (slackMatch) reads: the plain values, not
// the nested `.slack.*` publish shape.
func (ev evt) facts() map[string]any {
	return map[string]any{
		"channel": ev.channel, "user": ev.user, "reaction": ev.reaction,
		"command": ev.command, "callback_id": ev.callbackID,
	}
}

// context is the event's published `.slack` context (plus the sibling
// slack_bot_token fact — see decl.go). form is always a map (empty
// without one) so {{.slack.form.x}} renders empty rather than failing on a
// nil.
func (ev evt) context(botToken string) map[string]any {
	form := ev.form
	if form == nil {
		form = map[string]any{}
	}
	files := make([]any, 0, len(ev.files))
	for _, f := range ev.files {
		files = append(files, map[string]any{"id": f.ID, "name": f.Name, "mimetype": f.Mimetype})
	}
	return map[string]any{
		"slack": map[string]any{
			"channel": ev.channel, "user": ev.user, "text": ev.text, "ts": ev.ts,
			"thread_ts": ev.threadTS, "reaction": ev.reaction, "command": ev.command,
			"is_bot": ev.isBot, "via": ev.via, "callback_id": ev.callbackID,
			"files": files, "form": form,
		},
		"slack_bot_token": botToken,
	}
}

func (s *instanceSource) handleEvent(ctx context.Context, emit emitFunc, raw json.RawMessage) {
	var cb eventCallback
	if json.Unmarshal(raw, &cb) != nil {
		return
	}
	e := cb.Event
	switch e.Type {
	case "app_mention":
		ev := evt{
			text: e.Text, user: e.User, channel: e.Channel, ts: e.TS, threadTS: firstNonEmpty(e.ThreadTS, e.TS),
			via: "mention", files: e.Files, isBot: e.BotID != "",
		}
		if !s.dedup.Add("app_mention:" + ev.channel + ":" + ev.ts) {
			return
		}
		s.fire(ctx, emit, "app_mention", ev, false)
		s.offerForms(ctx, ev)
	case "reaction_added":
		ev := evt{reaction: e.Reaction, user: e.User, channel: e.Item.Channel, ts: e.Item.TS, threadTS: e.Item.TS}
		if !s.dedup.Add("reaction_added:" + ev.channel + ":" + ev.ts + ":" + ev.reaction + ":" + ev.user) {
			return
		}
		s.fire(ctx, emit, "reaction_added", ev, false)
	case "message":
		// A human reply inside a thread — or any message in a DM —
		// may be answering a pending ask/hand-off, or be ordinary chatter a
		// trigger on `slack.reply` wants. Either way it is published as a
		// "reply" event with conversation_reply semantics; the ENGINE (not
		// this plugin) decides whether a pending conversation consumes it.
		// Skip the bot's own posts (bot_id set) — a hand-off must never
		// see itself as a reply.
		if e.BotID != "" || e.User == "" || (e.ThreadTS == "" && e.ChannelType != "im") {
			return
		}
		ev := evt{text: e.Text, user: e.User, channel: e.Channel, ts: e.TS, threadTS: e.ThreadTS}
		if !s.dedup.Add("reply:" + ev.channel + ":" + ev.ts) {
			return
		}
		s.fire(ctx, emit, "reply", ev, false)
	}
}

func (s *instanceSource) handleSlash(ctx context.Context, emit emitFunc, raw json.RawMessage) {
	var p slashPayload
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	ev := evt{text: p.Text, user: p.UserID, channel: p.ChannelID, command: p.Command}
	if !s.dedup.Add("slash_command:" + ev.channel + ":" + ev.user + ":" + ev.command + ":" + ev.text) {
		return
	}
	s.fire(ctx, emit, "slash_command", ev, false)
}

// fire emits one routed plugin.SourceEvent per enabled trigger ON `on` whose
// match (preMatch when usersGated, else the plain filter) holds. A
// message_shortcut/form trigger that fires HERE (not through offerForms/
// onMessageShortcut) is never form-gated: fire is also used by the
// interactive path once a form has already been submitted (see
// onViewSubmission), where usersGated is left false because preMatch already
// ran once, at shortcut/button time.
func (s *instanceSource) fire(ctx context.Context, emit emitFunc, on string, ev evt, usersGated bool) {
	facts := ev.facts()
	matched := 0
	for _, ct := range s.triggers {
		if ct.event != on || !ct.enabled || ct.form != nil {
			continue
		}
		ok := ct.match(facts)
		if usersGated {
			ok = ct.preMatch(facts)
		}
		if !ok {
			continue
		}
		s.emitEvent(ctx, emit, on, ct.id, ev)
		matched++
	}
	if matched == 0 && on == "reply" {
		// No trigger claimed it: still publish it, unrouted, so the host's
		// conversation routing gets a chance, and a generic `on: slack.reply`
		// evaluated by the host itself (not this plugin) can still match.
		s.emitEvent(ctx, emit, on, "", ev)
	}
}

func (s *instanceSource) emitEvent(_ context.Context, emit emitFunc, on, trigger string, ev evt) {
	title := firstLine(firstNonEmpty(ev.text, ev.command+" reaction:"+ev.reaction, "slack "+on))
	se := plugin.SourceEvent{
		Event: on, Title: title,
		Target:  plugin.Target{Key: "slack:" + ev.channel + ":" + ev.ts, Assigned: true},
		Context: ev.context(s.botToken),
		Dedup:   on + ":" + ev.channel + ":" + firstNonEmpty(ev.ts, ev.reaction+ev.text),
		Trigger: trigger,
	}
	if err := emit(se); err != nil {
		log.Printf("slack[%s]: emit %s: %v", s.instance, on, err)
	}
}

// emitFunc is the Serve-provided notification sink.
type emitFunc func(plugin.SourceEvent) error
