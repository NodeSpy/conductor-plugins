package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

func newInvokeFake(t *testing.T) (*Plugin, *fakeSlackAPI, string) {
	t.Helper()
	rec := &fakeSlackAPI{}
	srv := newFakeSlackServer(t, rec)
	return New(), rec, srv.URL
}

func TestPostVerbChannel(t *testing.T) {
	p, rec, base := newInvokeFake(t)
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "post",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options:    map[string]any{"channel": "C1", "text": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["channel"] != "C1" {
		t.Fatalf("outputs: %+v", res.Outputs)
	}
	calls := rec.all()
	if len(calls) != 1 || calls[0].path != "/chat.postMessage" || calls[0].body["text"] != "hello" {
		t.Fatalf("calls: %+v", calls)
	}
}

func TestPostVerbRequiresText(t *testing.T) {
	p, _, base := newInvokeFake(t)
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "post",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options:    map[string]any{"channel": "C1"},
	})
	if err == nil {
		t.Fatal("expected an error with no text")
	}
}

func TestPostVerbWebhookOnly(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = jsonDecode(r, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := New()
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "post",
		Connection: map[string]any{"webhook_url": srv.URL},
		Options:    map[string]any{"text": "notify"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["ts"] != "" {
		t.Fatalf("webhook post should return an empty ts: %+v", res.Outputs)
	}
	if got["text"] != "notify" {
		t.Fatalf("webhook payload: %+v", got)
	}
}

func TestReactVerb(t *testing.T) {
	p, rec, base := newInvokeFake(t)
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "react",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options:    map[string]any{"channel": "C1", "ts": "1.1", "emoji": ":eyes:"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["ok"] != true {
		t.Fatalf("outputs: %+v", res.Outputs)
	}
	calls := rec.all()
	if len(calls) != 1 || calls[0].body["name"] != "eyes" {
		t.Fatalf("emoji colons should be trimmed: %+v", calls)
	}
}

func TestReactVerbNeedsBotToken(t *testing.T) {
	p := New()
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "react",
		Connection: map[string]any{"webhook_url": "http://example.com"},
		Options:    map[string]any{"channel": "C1", "ts": "1.1", "emoji": "eyes"},
	})
	if err == nil {
		t.Fatal("expected an error: react needs a bot_token")
	}
}

// TestAskThreadReturnsMatchingConversationID proves ask's conversation_id
// output is exactly what a reply event's conversation_reply.id template
// produces for the same thread (events_test.go's
// TestConversationReplyIDMatchesAsk checks the reverse direction).
func TestAskThreadReturnsMatchingConversationID(t *testing.T) {
	rec := &fakeSlackAPI{}
	srv := newFakeSlackServerWithTS(t, rec, "1700000000.000500")
	p := New()
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "ask",
		Connection: map[string]any{"bot_token": "xoxb-1", "app_token": "xapp-1", "api_base": srv.URL},
		Options:    map[string]any{"to": "thread", "channel": "C1", "prompt": "deploy?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "C1:1700000000.000500"
	if res.Outputs["conversation_id"] != want {
		t.Fatalf("conversation_id = %v, want %v", res.Outputs["conversation_id"], want)
	}
	calls := rec.all()
	if len(calls) != 1 || calls[0].path != "/chat.postMessage" {
		t.Fatalf("calls: %+v", calls)
	}
	text := calls[0].body["text"].(string)
	if !strings.Contains(text, "approve") {
		t.Fatalf("ask draft must include the reply instructions: %q", text)
	}
}

func TestAskDMReturnsEmptyThreadTS(t *testing.T) {
	rec := &fakeSlackAPI{}
	srv := newFakeSlackServerWithDM(t, rec, "D1", "1700000000.000700")
	p := New()
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "ask",
		Connection: map[string]any{"bot_token": "xoxb-1", "app_token": "xapp-1", "api_base": srv.URL},
		Options:    map[string]any{"to": "dm", "user": "U1", "prompt": "deploy?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["conversation_id"] != "D1:" {
		t.Fatalf("conversation_id = %v, want \"D1:\"", res.Outputs["conversation_id"])
	}
}

func TestAskRequiresToOption(t *testing.T) {
	p := New()
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "ask",
		Connection: map[string]any{"bot_token": "xoxb-1", "app_token": "xapp-1"},
		Options:    map[string]any{"prompt": "x"},
	})
	if err == nil {
		t.Fatal("expected an error with no options.to")
	}
}
