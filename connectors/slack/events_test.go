package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// testTrigger builds a plugin.SourceTrigger with a filter from the surface
// grammar (map/list/string), mirroring how the daemon lowers a trigger's
// `filter:` before start_source.
func testTrigger(t *testing.T, id, event string, filter any, options map[string]any) plugin.SourceTrigger {
	t.Helper()
	st := plugin.SourceTrigger{ID: id, Event: event, Options: options}
	if filter != nil {
		f, err := sourcekit.ParseFilter(filter)
		if err != nil {
			t.Fatalf("filter: %v", err)
		}
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		st.Filter = b
	}
	return st
}

func newSource(t *testing.T, triggers []plugin.SourceTrigger) *instanceSource {
	t.Helper()
	ct, err := compileTriggers(triggers)
	if err != nil {
		t.Fatal(err)
	}
	return &instanceSource{instance: "test", botToken: "xoxb-1", triggers: ct, dedup: sourcekit.NewDedup(64)}
}

func collect() (emitFunc, *[]plugin.SourceEvent) {
	var got []plugin.SourceEvent
	return func(se plugin.SourceEvent) error { got = append(got, se); return nil }, &got
}

// fireSync runs, synchronously, the work handleEvent/handleSlash/
// handleInteractive's helpers defer until after their dedup check and (for
// interactive envelopes) the ACK — production runs it on a goroutine (see
// socket.go's runOnce); tests that assert on emitted events right after the
// call want it to have already happened.
func fireSync(work func()) {
	if work != nil {
		work()
	}
}

func TestAppMentionEmits(t *testing.T) {
	s := newSource(t, []plugin.SourceTrigger{testTrigger(t, "t1", "app_mention", nil, nil)})
	emit, got := collect()
	raw := json.RawMessage(`{"event":{"type":"app_mention","text":"hey fix Widget","user":"U1","channel":"C1","ts":"1.1"}}`)
	fireSync(s.handleEvent(context.Background(), emit, raw))
	if len(*got) != 1 {
		t.Fatalf("want 1 event, got %d", len(*got))
	}
	ev := (*got)[0]
	if ev.Event != "app_mention" || ev.Title != "hey fix Widget" {
		t.Fatalf("unexpected event/title: %q / %q", ev.Event, ev.Title)
	}
	if ev.Trigger != "t1" {
		t.Fatalf("expected routing to t1, got %q", ev.Trigger)
	}
	if ev.Target.Key != "slack:C1:1.1" || !ev.Target.Assigned {
		t.Fatalf("unexpected target: %+v", ev.Target)
	}
	sctx := ev.Context["slack"].(map[string]any)
	if sctx["channel"] != "C1" || sctx["user"] != "U1" {
		t.Fatalf("slack context wrong: %+v", sctx)
	}
	if ev.Context["slack_bot_token"] != "xoxb-1" {
		t.Fatal("bot token should be exposed to actions via context")
	}
}

func TestReactionFilter(t *testing.T) {
	s := newSource(t, []plugin.SourceTrigger{testTrigger(t, "t1", "reaction_added", map[string]any{"reaction": "eyes"}, nil)})
	emit, got := collect()
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"reaction_added","reaction":"tada","user":"U1","item":{"channel":"C1","ts":"2.2"}}}`)))
	if len(*got) != 0 {
		t.Fatalf("non-matching reaction should not fire, got %d", len(*got))
	}
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"reaction_added","reaction":"eyes","user":"U1","item":{"channel":"C1","ts":"3.3"}}}`)))
	if len(*got) != 1 || (*got)[0].Event != "reaction_added" {
		t.Fatalf("matching reaction should fire once, got %+v", *got)
	}
}

func TestSlashCommandFilter(t *testing.T) {
	s := newSource(t, []plugin.SourceTrigger{testTrigger(t, "t1", "slash_command", map[string]any{"command": "/conductor"}, nil)})
	emit, got := collect()
	fireSync(s.handleSlash(context.Background(), emit, json.RawMessage(
		`{"command":"/other","text":"x","channel_id":"C1","user_id":"U1","trigger_id":"T1"}`)))
	if len(*got) != 0 {
		t.Fatalf("non-matching command should not fire, got %d", len(*got))
	}
	fireSync(s.handleSlash(context.Background(), emit, json.RawMessage(
		`{"command":"/conductor","text":"deploy please","channel_id":"C1","user_id":"U1","trigger_id":"T2"}`)))
	if len(*got) != 1 || (*got)[0].Event != "slash_command" {
		t.Fatalf("matching command should fire, got %+v", *got)
	}
}

// TestSlashCommandTargetsAreDistinctPerInvocation proves two slash commands
// fired in the SAME channel get DISTINCT targets, keyed on Slack's
// per-invocation trigger_id rather than colliding on "slack:<channel>:"
// (slash commands carry no message ts, unlike every other slack event).
func TestSlashCommandTargetsAreDistinctPerInvocation(t *testing.T) {
	s := newSource(t, []plugin.SourceTrigger{testTrigger(t, "t1", "slash_command", nil, nil)})
	emit, got := collect()
	fireSync(s.handleSlash(context.Background(), emit, json.RawMessage(
		`{"command":"/deploy","text":"staging","channel_id":"C1","user_id":"U1","trigger_id":"111.111"}`)))
	fireSync(s.handleSlash(context.Background(), emit, json.RawMessage(
		`{"command":"/deploy","text":"production","channel_id":"C1","user_id":"U1","trigger_id":"222.222"}`)))
	if len(*got) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(*got), *got)
	}
	k1, k2 := (*got)[0].Target.Key, (*got)[1].Target.Key
	if k1 == k2 {
		t.Fatalf("two distinct slash command invocations in one channel must not share a target, both got %q", k1)
	}
	if k1 != "slack:C1:111.111" || k2 != "slack:C1:222.222" {
		t.Fatalf("target keys = %q, %q, want slack:C1:111.111 and slack:C1:222.222", k1, k2)
	}
	sctx1 := (*got)[0].Context["slack"].(map[string]any)
	if sctx1["trigger_id"] != "111.111" {
		t.Fatalf(".slack.trigger_id not published: %+v", sctx1)
	}
}

func TestUsersFilter(t *testing.T) {
	s := newSource(t, []plugin.SourceTrigger{testTrigger(t, "t1", "app_mention", map[string]any{"users": []any{"U1"}}, nil)})
	emit, got := collect()
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"app_mention","text":"hi","user":"U2","channel":"C1","ts":"1.1"}}`)))
	if len(*got) != 0 {
		t.Fatalf("non-listed user should not fire, got %d", len(*got))
	}
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"app_mention","text":"hi","user":"U1","channel":"C1","ts":"1.2"}}`)))
	if len(*got) != 1 {
		t.Fatalf("listed user should fire, got %d", len(*got))
	}
}

func TestDedupRedelivery(t *testing.T) {
	s := newSource(t, []plugin.SourceTrigger{testTrigger(t, "t1", "app_mention", nil, nil)})
	emit, got := collect()
	raw := json.RawMessage(`{"event":{"type":"app_mention","text":"hi","user":"U1","channel":"C1","ts":"9.9"}}`)
	fireSync(s.handleEvent(context.Background(), emit, raw))
	fireSync(s.handleEvent(context.Background(), emit, raw)) // redelivery of the same ts
	if len(*got) != 1 {
		t.Fatalf("redelivered event should emit once, got %d", len(*got))
	}
}

// TestSlashCommandDedupKeyIncludesTriggerID is the regression test for
// finding #10: the SAME command+text typed twice by a user (two distinct
// invocations, each with its own Slack-minted trigger_id) must both fire —
// the old dedup key (channel+user+command+text, with no trigger_id) silently
// dropped the second one as if it were a retry. A genuine Socket Mode retry
// of the SAME delivery, which reuses the SAME trigger_id, must still
// collapse to one.
func TestSlashCommandDedupKeyIncludesTriggerID(t *testing.T) {
	s := newSource(t, []plugin.SourceTrigger{testTrigger(t, "t1", "slash_command", nil, nil)})
	emit, got := collect()

	fireSync(s.handleSlash(context.Background(), emit, json.RawMessage(
		`{"command":"/deploy","text":"staging","channel_id":"C1","user_id":"U1","trigger_id":"T1"}`)))
	fireSync(s.handleSlash(context.Background(), emit, json.RawMessage(
		`{"command":"/deploy","text":"staging","channel_id":"C1","user_id":"U1","trigger_id":"T2"}`)))
	if len(*got) != 2 {
		t.Fatalf("the same command+text with two different trigger_ids are two invocations, want 2 events, got %d: %+v", len(*got), *got)
	}

	*got = nil
	retry := json.RawMessage(`{"command":"/deploy","text":"staging","channel_id":"C1","user_id":"U1","trigger_id":"T3"}`)
	fireSync(s.handleSlash(context.Background(), emit, retry))
	fireSync(s.handleSlash(context.Background(), emit, retry)) // a Socket Mode retry: same trigger_id
	if len(*got) != 1 {
		t.Fatalf("a retry (same trigger_id) must still collapse to one event, got %d", len(*got))
	}
}

// TestMultipleTriggersRoutedIndependently proves one physical event can fan
// out to several routed plugin.SourceEvents, one per matching trigger —
// the contract's routing model (plugin-contract.md §1.5), replacing the
// builtin's per-rule action-variant fan-out.
func TestMultipleTriggersRoutedIndependently(t *testing.T) {
	s := newSource(t, []plugin.SourceTrigger{
		testTrigger(t, "a", "app_mention", nil, nil),
		testTrigger(t, "b", "app_mention", nil, nil),
	})
	emit, got := collect()
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"app_mention","text":"hi","user":"U1","channel":"C1","ts":"1.1"}}`)))
	if len(*got) != 2 {
		t.Fatalf("want 2 routed events, got %d", len(*got))
	}
	ids := map[string]bool{(*got)[0].Trigger: true, (*got)[1].Trigger: true}
	if !ids["a"] || !ids["b"] {
		t.Fatalf("expected routing to both a and b, got %+v", *got)
	}
}

// TestReplyEvent proves a thread reply publishes a "reply" event carrying
// the facts conversation_reply needs, and that the bot's own posts and
// non-threaded channel chatter never do.
func TestReplyEvent(t *testing.T) {
	s := newSource(t, nil)
	emit, got := collect()

	// A human thread reply.
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"message","channel":"C1","user":"U1","text":"approve","thread_ts":"T1"}}`)))
	if len(*got) != 1 || (*got)[0].Event != "reply" {
		t.Fatalf("thread reply should emit a reply event, got %+v", *got)
	}
	sctx := (*got)[0].Context["slack"].(map[string]any)
	if sctx["thread_ts"] != "T1" || sctx["text"] != "approve" || sctx["user"] != "U1" {
		t.Fatalf("reply context wrong: %+v", sctx)
	}
	if (*got)[0].Trigger != "" {
		t.Fatalf("an unrouted reply must carry no trigger id, got %q", (*got)[0].Trigger)
	}

	// The bot's own post is skipped.
	*got = nil
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"message","channel":"C1","user":"U1","text":"x","thread_ts":"T1","bot_id":"B1"}}`)))
	if len(*got) != 0 {
		t.Fatalf("bot posts must not emit a reply event, got %+v", *got)
	}

	// A top-level (non-threaded) channel message is skipped too.
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"message","channel":"C1","user":"U1","text":"hi"}}`)))
	if len(*got) != 0 {
		t.Fatalf("non-threaded channel messages must not emit a reply event, got %+v", *got)
	}

	// A DM message (channel_type im, no thread_ts) IS a reply.
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"message","channel":"D1","user":"U1","text":"approve","channel_type":"im"}}`)))
	if len(*got) != 1 || (*got)[0].Event != "reply" {
		t.Fatalf("a DM message should emit a reply event, got %+v", *got)
	}
}

func TestConversationReplyIDMatchesAsk(t *testing.T) {
	// The id a "reply" event's conversation_reply template would produce
	// ("{{.slack.channel}}:{{.slack.thread_ts}}") must equal what askVerb
	// returns as conversation_id for the same thread/DM, or the engine would
	// never match a reply back to its ask.
	s := newSource(t, nil)
	emit, got := collect()
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"message","channel":"C1","user":"U1","text":"approve","thread_ts":"1700000000.000100"}}`)))
	sctx := (*got)[0].Context["slack"].(map[string]any)
	gotID := sctx["channel"].(string) + ":" + sctx["thread_ts"].(string)
	if gotID != "C1:1700000000.000100" {
		t.Fatalf("id = %q", gotID)
	}

	*got = nil
	fireSync(s.handleEvent(context.Background(), emit, json.RawMessage(
		`{"event":{"type":"message","channel":"D1","user":"U1","text":"approve","channel_type":"im"}}`)))
	sctx = (*got)[0].Context["slack"].(map[string]any)
	gotDMID := sctx["channel"].(string) + ":" + sctx["thread_ts"].(string)
	if gotDMID != "D1:" {
		t.Fatalf("dm id = %q", gotDMID)
	}
}
