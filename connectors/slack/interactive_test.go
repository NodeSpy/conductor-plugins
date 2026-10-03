package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// fakeSlackAPI records every Web API call and answers ok:true, so interactive
// handling can be driven without a real Slack. views.open's trigger_id and
// chat.postEphemeral's blocks are both captured for assertions.
type fakeSlackAPI struct {
	mu    sync.Mutex
	calls []apiCall
}

type apiCall struct {
	path string
	body map[string]any
}

func (r *fakeSlackAPI) all() []apiCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]apiCall(nil), r.calls...)
}

func newFakeSlackServer(t *testing.T, rec *fakeSlackAPI) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		rec.calls = append(rec.calls, apiCall{path: r.URL.Path, body: body})
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sourceWithFake(t *testing.T, triggers []plugin.SourceTrigger) (*instanceSource, *fakeSlackAPI) {
	t.Helper()
	rec := &fakeSlackAPI{}
	srv := newFakeSlackServer(t, rec)
	s := newSource(t, triggers)
	s.api = newSlackAPI("xoxb-1", "xapp-1", srv.URL)
	return s, rec
}

func mentionForm() map[string]any {
	return map[string]any{
		"form": map[string]any{
			"title": "Deploy", "fields": []any{
				map[string]any{"name": "env", "type": "select", "options": []any{"staging", "prod"}},
			},
		},
	}
}

// TestMentionFormOffersButton proves a form-carrying app_mention trigger
// offers an ephemeral button instead of firing directly, visible only to the
// mentioning user.
func TestMentionFormOffersButton(t *testing.T) {
	s, rec := sourceWithFake(t, []plugin.SourceTrigger{
		testTrigger(t, "t1", "app_mention", map[string]any{"users": []any{"U1"}}, mentionForm()),
	})
	emit, got := collect()
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"app_mention","text":"deploy it","user":"U1","channel":"C1","ts":"1.1"}}`)))
	// The bare mention still fires (app_mention has no "direct" rule here
	// since its only trigger carries a form, so nothing not-form matches);
	// only the button should post.
	if len(*got) != 0 {
		t.Fatalf("a form trigger must not fire on the bare mention, got %+v", *got)
	}
	calls := rec.all()
	if len(calls) != 1 || calls[0].path != "/chat.postEphemeral" {
		t.Fatalf("want one chat.postEphemeral call, got %+v", calls)
	}
	if calls[0].body["user"] != "U1" || calls[0].body["channel"] != "C1" {
		t.Fatalf("button not addressed to the mentioning user: %+v", calls[0].body)
	}
}

// TestMentionFormRequiresUsers proves a form trigger with no users:/any_user
// never offers its button (the #161 safety rule, enforced again here in case
// a config somehow reaches the integration without it).
func TestMentionFormRequiresUsers(t *testing.T) {
	s, rec := sourceWithFake(t, []plugin.SourceTrigger{
		testTrigger(t, "t1", "app_mention", nil, mentionForm()),
	})
	emit, _ := collect()
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"app_mention","text":"deploy it","user":"U1","channel":"C1","ts":"1.1"}}`)))
	if len(rec.all()) != 0 {
		t.Fatalf("a form trigger with no users:/any_user must not fire, got %+v", rec.all())
	}
}

// TestMentionFormAnyUserOptsOut proves any_user: true lets any mentioning
// user open the form.
func TestMentionFormAnyUserOptsOut(t *testing.T) {
	opts := mentionForm()
	opts["any_user"] = true
	s, rec := sourceWithFake(t, []plugin.SourceTrigger{testTrigger(t, "t1", "app_mention", nil, opts)})
	emit, _ := collect()
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"app_mention","text":"deploy it","user":"U9","channel":"C1","ts":"1.1"}}`)))
	if len(rec.all()) != 1 {
		t.Fatalf("any_user should let anyone open the form, got %+v", rec.all())
	}
}

// TestButtonClickOpensForm drives the full mention -> button click -> modal
// flow and checks only the offered user may click it.
func TestButtonClickOpensForm(t *testing.T) {
	s, rec := sourceWithFake(t, []plugin.SourceTrigger{
		testTrigger(t, "t1", "app_mention", map[string]any{"users": []any{"U1"}}, mentionForm()),
	})
	emit, _ := collect()
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"app_mention","text":"deploy it","user":"U1","channel":"C1","ts":"1.1"}}`)))
	calls := rec.all()
	blocks := calls[0].body["blocks"].([]any)
	actions := blocks[1].(map[string]any)["elements"].([]any)[0].(map[string]any)
	tok := actions["value"].(string)

	// Wrong user clicking: refused, no views.open.
	raw, _ := json.Marshal(map[string]any{
		"type": "block_actions", "trigger_id": "trig-1",
		"user":    map[string]any{"id": "U2"},
		"actions": []any{map[string]any{"action_id": formOpenActionID, "value": tok}},
	})
	ack, after := s.handleInteractive(context.Background(), emit, raw)
	if ack != nil {
		t.Fatalf("block_actions never ACKs with a payload, got %v", ack)
	}
	if after != nil {
		after()
	}
	if len(rec.all()) != 1 {
		t.Fatalf("wrong user's click must not open the form: %+v", rec.all())
	}

	// Right user: views.open fires with the trigger_id.
	raw, _ = json.Marshal(map[string]any{
		"type": "block_actions", "trigger_id": "trig-1",
		"user":    map[string]any{"id": "U1"},
		"actions": []any{map[string]any{"action_id": formOpenActionID, "value": tok}},
	})
	_, after = s.handleInteractive(context.Background(), emit, raw)
	if after != nil {
		after()
	}
	calls = rec.all()
	if len(calls) != 2 || calls[1].path != "/views.open" {
		t.Fatalf("want views.open, got %+v", calls)
	}
	if calls[1].body["trigger_id"] != "trig-1" {
		t.Fatalf("views.open missing trigger_id: %+v", calls[1].body)
	}
}

// TestFormSubmissionFiresWithValues drives a full shortcut -> submission
// round trip and checks the submitted value publishes at .slack.form.<name>.
func TestFormSubmissionFiresWithValues(t *testing.T) {
	form := map[string]any{"form": map[string]any{
		"fields": []any{map[string]any{"name": "env", "type": "select", "options": []any{"staging", "prod"}}},
	}}
	s, _ := sourceWithFake(t, []plugin.SourceTrigger{
		testTrigger(t, "shortcut1", "message_shortcut", map[string]any{"users": []any{"U1"}}, form),
	})
	emit, got := collect()

	shortcutRaw, _ := json.Marshal(map[string]any{
		"type": "message_action", "callback_id": "whatever", "trigger_id": "trig-1",
		"user": map[string]any{"id": "U1"}, "channel": map[string]any{"id": "C1"},
		"message": map[string]any{"ts": "5.5", "text": "deploy this"},
	})
	_, after := s.handleInteractive(context.Background(), emit, shortcutRaw)
	if after != nil {
		after()
	}
	if len(*got) != 0 {
		t.Fatalf("a form shortcut must not fire before submission, got %+v", *got)
	}

	// Build the private_metadata the way openForm would have (key = trigger
	// id "shortcut1", via shortcut).
	meta := formMeta{Key: "shortcut1", Channel: "C1", TS: "5.5", Via: "shortcut", Text: "deploy this", User: "U1"}
	submitRaw, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U1"},
		"view": map[string]any{
			"callback_id":      formViewCallbackID,
			"private_metadata": meta.encode(),
			"state": map[string]any{"values": map[string]any{
				"env": map[string]any{"value": map[string]any{"type": "static_select", "selected_option": map[string]any{"value": "prod"}}},
			}},
		},
	})
	ack, after := s.handleInteractive(context.Background(), emit, submitRaw)
	if ack != nil {
		t.Fatalf("a valid submission must ACK bare, got %v", ack)
	}
	if after != nil {
		after()
	}
	if len(*got) != 1 {
		t.Fatalf("want 1 event after submission, got %d", len(*got))
	}
	sctx := (*got)[0].Context["slack"].(map[string]any)
	form2 := sctx["form"].(map[string]any)
	if form2["env"] != "prod" {
		t.Fatalf("submitted value not published: %+v", form2)
	}
}

// TestFormSubmissionValidationErrors proves a missing required field or an
// off-list select value comes back as a response_action: errors ACK and
// fires nothing.
func TestFormSubmissionValidationErrors(t *testing.T) {
	form := map[string]any{"form": map[string]any{
		"fields": []any{map[string]any{"name": "env", "type": "select", "options": []any{"staging", "prod"}}},
	}}
	s, _ := sourceWithFake(t, []plugin.SourceTrigger{
		testTrigger(t, "shortcut1", "message_shortcut", map[string]any{"users": []any{"U1"}}, form),
	})
	emit, got := collect()
	meta := formMeta{Key: "shortcut1", Channel: "C1", TS: "5.5", Via: "shortcut", User: "U1"}

	for _, val := range []string{"", "bogus"} {
		values := map[string]any{}
		if val != "" {
			values["env"] = map[string]any{"value": map[string]any{"type": "static_select", "selected_option": map[string]any{"value": val}}}
		} else {
			values["env"] = map[string]any{"value": map[string]any{"type": "static_select"}}
		}
		submitRaw, _ := json.Marshal(map[string]any{
			"type": "view_submission",
			"user": map[string]any{"id": "U1"},
			"view": map[string]any{"callback_id": formViewCallbackID, "private_metadata": meta.encode(), "state": map[string]any{"values": values}},
		})
		ack, after := s.handleInteractive(context.Background(), emit, submitRaw)
		if after != nil {
			t.Fatal("an invalid submission must not run follow-up work")
		}
		m, ok := ack.(map[string]any)
		if !ok || m["response_action"] != "errors" {
			t.Fatalf("want response_action errors for %q, got %v", val, ack)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("no event should fire on invalid submissions, got %+v", *got)
	}
}

// TestShortcutRequiresUsers proves a bare message_shortcut (no form) still
// needs users:/any_user.
func TestShortcutRequiresUsers(t *testing.T) {
	s, _ := sourceWithFake(t, []plugin.SourceTrigger{testTrigger(t, "t1", "message_shortcut", nil, nil)})
	emit, got := collect()
	raw, _ := json.Marshal(map[string]any{
		"type": "message_action", "trigger_id": "trig-1",
		"user": map[string]any{"id": "U1"}, "channel": map[string]any{"id": "C1"},
		"message": map[string]any{"ts": "5.5", "text": "x"},
	})
	_, after := s.handleInteractive(context.Background(), emit, raw)
	if after != nil {
		after()
	}
	if len(*got) != 0 {
		t.Fatalf("shortcut with no users:/any_user must not fire, got %+v", *got)
	}
}

// TestShortcutCallbackIDFilter proves a message_shortcut trigger can be
// scoped to one callback_id.
func TestShortcutCallbackIDFilter(t *testing.T) {
	s, _ := sourceWithFake(t, []plugin.SourceTrigger{
		testTrigger(t, "t1", "message_shortcut", map[string]any{"callback_id": "deploy_shortcut", "users": []any{"U1"}}, nil),
	})
	emit, got := collect()
	raw, _ := json.Marshal(map[string]any{
		"type": "message_action", "callback_id": "other_shortcut", "trigger_id": "trig-1",
		"user": map[string]any{"id": "U1"}, "channel": map[string]any{"id": "C1"},
		"message": map[string]any{"ts": "5.5", "text": "x"},
	})
	_, after := s.handleInteractive(context.Background(), emit, raw)
	if after != nil {
		after()
	}
	if len(*got) != 0 {
		t.Fatalf("non-matching callback_id must not fire, got %+v", *got)
	}

	raw, _ = json.Marshal(map[string]any{
		"type": "message_action", "callback_id": "deploy_shortcut", "trigger_id": "trig-1",
		"user": map[string]any{"id": "U1"}, "channel": map[string]any{"id": "C1"},
		"message": map[string]any{"ts": "5.5", "text": "x"},
	})
	_, after = s.handleInteractive(context.Background(), emit, raw)
	if after != nil {
		after()
	}
	if len(*got) != 1 {
		t.Fatalf("matching callback_id should fire, got %+v", *got)
	}
}

func TestFormValidateCaughtAtLoad(t *testing.T) {
	bad := map[string]any{"form": map[string]any{"fields": []any{}}}
	if _, err := compileTriggers([]plugin.SourceTrigger{testTrigger(t, "t1", "app_mention", nil, bad)}); err == nil {
		t.Fatal("a form with no fields should fail to compile")
	}
}
