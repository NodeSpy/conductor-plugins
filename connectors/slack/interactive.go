package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"time"
	"unicode/utf8"
)

// Interactive (Socket Mode `interactive` envelope) handling: message
// shortcuts, the ephemeral "open form" button a mention-with-form posts, and
// modal submissions (#161 parity). Every one of these must be ACKed within 3
// seconds, and a trigger_id is only good for views.open for 3 seconds —
// so the decisions here (does a trigger match, which form opens) are made
// synchronously in the socket loop, and the follow-up work (views.open,
// emit) runs right after the ACK is written (see socket.go's runOnce).

// Callback/action ids conductor owns on the app's surfaces.
const (
	formViewCallbackID = "conductor_form"
	formOpenActionID   = "conductor_form_open"
)

// pendingTTL bounds how long a mention's "open form" button stays usable.
const pendingTTL = 30 * time.Minute

// maxPendingForms bounds the button-token table.
const maxPendingForms = 1024

// interactivePayload is the subset of Slack's interaction payloads read here
// (message_action, block_actions, view_submission).
type interactivePayload struct {
	Type       string `json:"type"`
	CallbackID string `json:"callback_id"`
	TriggerID  string `json:"trigger_id"`
	User       struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		TS       string      `json:"ts"`
		ThreadTS string      `json:"thread_ts"`
		Text     string      `json:"text"`
		Files    []slackFile `json:"files"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
	View struct {
		ID              string `json:"id"`
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		State           struct {
			Values map[string]map[string]stateValue `json:"values"`
		} `json:"state"`
	} `json:"view"`
}

// stateValue is one input element's submitted state.
type stateValue struct {
	Type           string `json:"type"`
	Value          string `json:"value"`
	SelectedOption *struct {
		Value string `json:"value"`
	} `json:"selected_option"`
}

// slackFile is the file metadata Slack attaches to a message.
type slackFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mimetype string `json:"mimetype"`
}

// formMeta is a modal's private_metadata: which trigger the form belongs to
// and the message/thread it was opened on. Slack stores it server-side with
// the view and returns it unchanged on submission. Key is the trigger's
// SourceTrigger.ID — already a stable, host-assigned identity, so (unlike
// the builtin) no separate "rule key" scheme is needed.
type formMeta struct {
	Key      string      `json:"k"`
	Channel  string      `json:"c"`
	TS       string      `json:"ts"`
	ThreadTS string      `json:"tt,omitempty"`
	User     string      `json:"u"`
	Via      string      `json:"v"`
	Callback string      `json:"cb,omitempty"`
	Text     string      `json:"x,omitempty"`
	Files    []slackFile `json:"f,omitempty"`
}

// maxPrivateMetadata is Slack's private_metadata limit.
const maxPrivateMetadata = 3000

func (m formMeta) encode() string {
	b, _ := json.Marshal(m)
	if len(b) <= maxPrivateMetadata {
		return string(b)
	}
	m.Files = nil
	b, _ = json.Marshal(m)
	for len(b) > maxPrivateMetadata && m.Text != "" {
		m.Text = clipRunes(m.Text, utf8.RuneCountInString(m.Text)/2)
		b, _ = json.Marshal(m)
	}
	return string(b)
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// formState holds the mention-path "open form" buttons awaiting a click.
type formState struct {
	mu      sync.Mutex
	pending map[string]pendingForm
	order   []string
}

type pendingForm struct {
	meta    formMeta
	expires time.Time
}

func (s *formState) add(m formMeta) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = map[string]pendingForm{}
	}
	s.pending[tok] = pendingForm{meta: m, expires: time.Now().Add(pendingTTL)}
	s.order = append(s.order, tok)
	for len(s.order) > maxPendingForms {
		delete(s.pending, s.order[0])
		s.order = s.order[1:]
	}
	return tok
}

func (s *formState) get(tok string) (formMeta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[tok]
	if !ok || time.Now().After(p.expires) {
		return formMeta{}, false
	}
	return p.meta, true
}

// triggerByID finds a form trigger by its SourceTrigger id.
func (s *instanceSource) triggerByID(id string) (compiledTrigger, bool) {
	for _, ct := range s.triggers {
		if ct.id == id && ct.form != nil {
			return ct, true
		}
	}
	return compiledTrigger{}, false
}

// handleInteractive processes one `interactive` envelope payload. It returns
// the ACK payload (nil for a bare ACK) and work to run once the ACK is sent.
func (s *instanceSource) handleInteractive(ctx context.Context, emit emitFunc, raw json.RawMessage) (ack any, after func()) {
	var p interactivePayload
	if json.Unmarshal(raw, &p) != nil {
		return nil, nil
	}
	switch p.Type {
	case "message_action":
		return nil, s.onMessageShortcut(ctx, emit, p)
	case "block_actions":
		return nil, s.onBlockActions(ctx, p)
	case "view_submission":
		if p.View.CallbackID != formViewCallbackID {
			return nil, nil
		}
		return s.onViewSubmission(ctx, emit, p)
	}
	return nil, nil
}

// onMessageShortcut: a message shortcut was used on a message. A matching
// trigger with a form opens it (the first one — one trigger_id opens one
// modal); matching triggers without one fire immediately.
func (s *instanceSource) onMessageShortcut(ctx context.Context, emit emitFunc, p interactivePayload) func() {
	ev := evt{
		text: p.Message.Text, user: p.User.ID, channel: p.Channel.ID,
		ts: p.Message.TS, threadTS: firstNonEmpty(p.Message.ThreadTS, p.Message.TS),
		via: "shortcut", callbackID: p.CallbackID, files: p.Message.Files,
	}
	facts := ev.facts()
	var direct []compiledTrigger
	var form *compiledTrigger
	for _, ct := range s.triggers {
		if ct.event != "message_shortcut" || !ct.enabled || !ct.preMatch(facts) {
			continue
		}
		if ct.form == nil {
			direct = append(direct, ct)
		} else if form == nil {
			ct := ct
			form = &ct
		}
	}
	if form == nil && len(direct) == 0 {
		log.Printf("slack[%s]: message shortcut %q by %s matched no trigger", s.instance, p.CallbackID, p.User.ID)
		return nil
	}
	return func() {
		if form != nil {
			s.openForm(ctx, p.TriggerID, form.form, formMeta{
				Key: form.id, Channel: ev.channel, TS: ev.ts, ThreadTS: ev.threadTS,
				User: ev.user, Via: ev.via, Callback: ev.callbackID, Text: clipRunes(ev.text, 1500), Files: ev.files,
			})
		}
		for _, ct := range direct {
			s.emitEvent(ctx, emit, "message_shortcut", ct.id, ev)
		}
	}
}

// offerForms is the mention-with-form path: for each form trigger that
// matches the mention, post an ephemeral message (visible only to the
// mentioning user, in the thread) with a button that opens the form.
func (s *instanceSource) offerForms(ctx context.Context, ev evt) {
	facts := ev.facts()
	for _, ct := range s.triggers {
		if ct.event != "app_mention" || ct.form == nil || !ct.enabled || !ct.preMatch(facts) {
			continue
		}
		if !s.dedup.Add("app_mention-form:" + ct.id + ":" + ev.channel + ":" + ev.ts) {
			continue
		}
		tok := s.forms.add(formMeta{
			Key: ct.id, Channel: ev.channel, TS: ev.ts, ThreadTS: ev.threadTS,
			User: ev.user, Via: "mention", Text: clipRunes(ev.text, 1500), Files: ev.files,
		})
		text := "Open the form to continue."
		blocks := []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
			map[string]any{"type": "actions", "elements": []any{map[string]any{
				"type": "button", "action_id": formOpenActionID, "value": tok,
				"text": plain(ct.form.title()), "style": "primary",
			}}},
		}
		if err := s.api.postEphemeralBlocks(ctx, ev.channel, ev.user, text, blocks); err != nil {
			log.Printf("slack[%s]: post form button: %v", s.instance, err)
		}
	}
}

// onBlockActions: the ephemeral "open form" button was clicked. Only the user
// it was offered to may open it.
func (s *instanceSource) onBlockActions(ctx context.Context, p interactivePayload) func() {
	for _, a := range p.Actions {
		if a.ActionID != formOpenActionID {
			continue
		}
		meta, ok := s.forms.get(a.Value)
		if !ok {
			log.Printf("slack[%s]: form button clicked after it expired", s.instance)
			return nil
		}
		if meta.User != p.User.ID {
			log.Printf("slack[%s]: form button offered to %s clicked by %s — ignored", s.instance, meta.User, p.User.ID)
			return nil
		}
		ct, ok := s.triggerByID(meta.Key)
		if !ok {
			return nil
		}
		return func() { s.openForm(ctx, p.TriggerID, ct.form, meta) }
	}
	return nil
}

// onViewSubmission validates a submitted form and, if it holds, fires the
// form's trigger. Validation errors are returned in the ACK (Slack shows
// them inline and keeps the modal open); nothing fires.
func (s *instanceSource) onViewSubmission(ctx context.Context, emit emitFunc, p interactivePayload) (any, func()) {
	var meta formMeta
	if json.Unmarshal([]byte(p.View.PrivateMetadata), &meta) != nil {
		return nil, nil
	}
	ct, ok := s.triggerByID(meta.Key)
	if !ok {
		log.Printf("slack[%s]: form submission for unknown trigger %q (config changed?)", s.instance, meta.Key)
		return nil, nil
	}
	on := "app_mention"
	if meta.Via == "shortcut" {
		on = "message_shortcut"
	}
	if ct.event != on || meta.User != p.User.ID {
		log.Printf("slack[%s]: form submission does not match its trigger (on=%s via=%s user=%s/%s) — ignored", s.instance, ct.event, meta.Via, meta.User, p.User.ID)
		return nil, nil
	}
	ev := evt{
		text: meta.Text, user: p.User.ID, channel: meta.Channel, ts: meta.TS, threadTS: meta.ThreadTS,
		via: meta.Via, callbackID: meta.Callback, files: meta.Files,
	}
	// Re-check the trigger against the submitter before anything else: the
	// config may have changed while the modal was open.
	if !ct.preMatch(ev.facts()) {
		log.Printf("slack[%s]: form submission by %s no longer matches its trigger — ignored", s.instance, p.User.ID)
		return nil, nil
	}
	vals, errs := ct.form.formValues(p.View.State.Values)
	if len(errs) > 0 {
		return map[string]any{"response_action": "errors", "errors": errs}, nil
	}
	ev.form = vals
	return nil, func() { s.emitEvent(ctx, emit, on, ct.id, ev) }
}

// openForm opens a trigger's form as a modal on trigger_id.
func (s *instanceSource) openForm(ctx context.Context, triggerID string, f *Form, meta formMeta) {
	if triggerID == "" || f == nil {
		return
	}
	view := map[string]any{
		"type": "modal", "callback_id": formViewCallbackID,
		"title": plain(f.title()), "submit": plain(f.submit()), "close": plain("Cancel"),
		"private_metadata": meta.encode(),
		"blocks":           f.blocks(),
	}
	if err := s.api.viewsOpen(ctx, triggerID, view); err != nil {
		log.Printf("slack[%s]: views.open: %v", s.instance, err)
	}
}
