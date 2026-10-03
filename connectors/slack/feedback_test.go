package main

import (
	"testing"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// TestFeedbackVerbReactsAndSays proves the restored ack/on_done/on_fail
// shape in one call: both a reaction and a threaded message, against the
// event's own message coordinates — exactly how the host's option_hooks
// semantic invokes it (channel/ts/user/thread_ts filled from the triggering
// event's own facts, react/say/ephemeral/in_thread from the operator's own
// block verbatim).
func TestFeedbackVerbReactsAndSays(t *testing.T) {
	p, rec, base := newInvokeFake(t)
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "feedback",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options: map[string]any{
			"channel": "C1", "ts": "1.1", "thread_ts": "1.1",
			"react": "eyes", "say": "on it",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["ok"] != true {
		t.Fatalf("outputs: %+v", res.Outputs)
	}
	calls := rec.all()
	if len(calls) != 2 {
		t.Fatalf("want a react call and a post call, got %+v", calls)
	}
	if calls[0].path != "/reactions.add" || calls[0].body["name"] != "eyes" {
		t.Fatalf("react call: %+v", calls[0])
	}
	if calls[1].path != "/chat.postMessage" || calls[1].body["text"] != "on it" || calls[1].body["thread_ts"] != "1.1" {
		t.Fatalf("post call: %+v", calls[1])
	}
}

// TestFeedbackVerbReactOnly proves say is optional: react alone posts no
// message.
func TestFeedbackVerbReactOnly(t *testing.T) {
	p, rec, base := newInvokeFake(t)
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "feedback",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options:    map[string]any{"channel": "C1", "ts": "1.1", "react": "white_check_mark"},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := rec.all()
	if len(calls) != 1 || calls[0].path != "/reactions.add" {
		t.Fatalf("want only a react call, got %+v", calls)
	}
}

// TestFeedbackVerbSayOnlyDefaultsToThread proves say alone (no react) posts
// into the thread by default (in_thread unset == true).
func TestFeedbackVerbSayOnlyDefaultsToThread(t *testing.T) {
	p, rec, base := newInvokeFake(t)
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "feedback",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options:    map[string]any{"channel": "C1", "thread_ts": "1.1", "say": "done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := rec.all()
	if len(calls) != 1 || calls[0].path != "/chat.postMessage" || calls[0].body["thread_ts"] != "1.1" {
		t.Fatalf("want a threaded post, got %+v", calls)
	}
}

// TestFeedbackVerbInThreadFalsePostsToChannel proves in_thread: false posts
// to the channel instead of the thread — the old builtin's default-true
// shape restored exactly.
func TestFeedbackVerbInThreadFalsePostsToChannel(t *testing.T) {
	p, rec, base := newInvokeFake(t)
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "feedback",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options:    map[string]any{"channel": "C1", "thread_ts": "1.1", "say": "done", "in_thread": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := rec.all()
	if len(calls) != 1 {
		t.Fatalf("calls: %+v", calls)
	}
	if _, ok := calls[0].body["thread_ts"]; ok {
		t.Fatalf("in_thread: false must not thread the post: %+v", calls[0])
	}
}

// TestFeedbackVerbEphemeral proves ephemeral posts via chat.postEphemeral to
// the triggering user instead of the channel/thread.
func TestFeedbackVerbEphemeral(t *testing.T) {
	p, rec, base := newInvokeFake(t)
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "feedback",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options:    map[string]any{"channel": "C1", "user": "U1", "say": "private note", "ephemeral": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := rec.all()
	if len(calls) != 1 || calls[0].path != "/chat.postEphemeral" || calls[0].body["user"] != "U1" {
		t.Fatalf("want an ephemeral post to U1, got %+v", calls)
	}
}

// TestFeedbackVerbEphemeralWithoutUserFallsBackToNormalPost mirrors the old
// builtin: ephemeral with no user to send it to is not dropped — it posts
// normally instead.
func TestFeedbackVerbEphemeralWithoutUserFallsBackToNormalPost(t *testing.T) {
	p, rec, base := newInvokeFake(t)
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "feedback",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options:    map[string]any{"channel": "C1", "say": "note", "ephemeral": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := rec.all()
	if len(calls) != 1 || calls[0].path != "/chat.postMessage" {
		t.Fatalf("want a normal post fallback, got %+v", calls)
	}
}

// TestFeedbackVerbRequiresReactOrSay mirrors the old builtin's Validate: an
// empty block is a configuration mistake, not silence.
func TestFeedbackVerbRequiresReactOrSay(t *testing.T) {
	p, _, base := newInvokeFake(t)
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "feedback",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options:    map[string]any{"channel": "C1", "ts": "1.1"},
	})
	if err == nil {
		t.Fatal("expected an error: neither react nor say is set")
	}
}

// TestFeedbackVerbMissingReactTargetSkipsReactButStillSays proves a
// best-effort degrade: react has nothing to react to (no channel/ts), but
// say still goes out — a hook invocation never fails the run over a
// reaction alone.
func TestFeedbackVerbMissingReactTargetSkipsReactButStillSays(t *testing.T) {
	p, rec, base := newInvokeFake(t)
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "feedback",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": base},
		Options:    map[string]any{"channel": "C1", "say": "done", "react": "eyes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["ok"] != true {
		t.Fatalf("outputs: %+v", res.Outputs)
	}
	calls := rec.all()
	if len(calls) != 1 || calls[0].path != "/chat.postMessage" {
		t.Fatalf("want only the post (react skipped, no ts), got %+v", calls)
	}
}

func TestFeedbackVerbNeedsBotToken(t *testing.T) {
	p := New()
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "feedback",
		Connection: map[string]any{"webhook_url": "http://example.com"},
		Options:    map[string]any{"channel": "C1", "ts": "1.1", "react": "eyes"},
	})
	if err == nil {
		t.Fatal("expected an error: feedback needs a bot_token")
	}
}
