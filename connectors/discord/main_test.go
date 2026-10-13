package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- Describe / semantics ---

func TestDescribe(t *testing.T) {
	d := discordPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "discord" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("discord spawns nothing: %#v", d.Capabilities)
	}
	wantVerbs := []string{"post", "ask"}
	gotVerbs := map[string]plugin.Verb{}
	for _, v := range d.Verbs {
		gotVerbs[v.Name] = v
	}
	for _, w := range wantVerbs {
		if _, ok := gotVerbs[w]; !ok {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Verbs) != len(wantVerbs) {
		t.Errorf("verb count: got %d want %d", len(d.Verbs), len(wantVerbs))
	}
	post := gotVerbs["post"]
	if post.Semantics == nil || !post.Semantics.ConversationPost {
		t.Errorf("post should declare conversation_post: %#v", post.Semantics)
	}
	ask := gotVerbs["ask"]
	if !ask.Ask {
		t.Errorf("ask verb should have Ask: true")
	}
	if ask.Semantics == nil || ask.Semantics.OpensConversation == nil || ask.Semantics.OpensConversation.ID != "ref" {
		t.Errorf("ask should declare opens_conversation with id: ref, got %#v", ask.Semantics)
	}
	if ask.Semantics.OpensConversation.Approvers != "approvers" {
		t.Errorf("ask should declare opens_conversation.approvers: approvers, got %#v", ask.Semantics.OpensConversation)
	}
	if len(d.Events) != 1 || d.Events[0].Name != "reply" {
		t.Fatalf("expected exactly one event named reply: %#v", d.Events)
	}
	ev := d.Events[0]
	if ev.Semantics == nil || ev.Semantics.ConversationReply == nil {
		t.Fatalf("reply event should declare conversation_reply: %#v", ev.Semantics)
	}
	// The id must be a TEMPLATE over the event's facts ("{{.channel}}", not
	// the bare word "channel" — which would render as that literal string
	// for every reply, colliding every channel onto one conversation slot).
	// It also folds in replied_to (Discord's native reply-to feature) so two
	// concurrent asks in the same channel resolve independently; see
	// TestConversationReplyIDMatchesAsk.
	if ev.Semantics.ConversationReply.ID != "{{.channel}}:{{.replied_to}}" || ev.Semantics.ConversationReply.Author != "author" || ev.Semantics.ConversationReply.Text != "text" {
		t.Errorf("conversation_reply fact names: %#v", ev.Semantics.ConversationReply)
	}
	if ev.Semantics.Author == nil || ev.Semantics.Author.Login != "author" || ev.Semantics.Author.Automated != "author_bot" {
		t.Errorf("reply event should declare author semantics: %#v", ev.Semantics.Author)
	}
}

// TestSemanticsPassContractChecks asserts this plugin's declarations are
// must-understand-clean (CheckSemantics) and internally consistent
// (ValidateSemantics), the way the host itself gates a plugin at load.
func TestSemanticsPassContractChecks(t *testing.T) {
	d := discordPlugin{}.Describe()
	d.ProtocolVersion = plugin.ProtocolVersion
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal decl: %v", err)
	}
	if problems := plugin.CheckSemantics(raw); len(problems) != 0 {
		t.Fatalf("CheckSemantics: %v", problems)
	}
	if problems := plugin.ValidateSemantics(d); len(problems) != 0 {
		t.Fatalf("ValidateSemantics: %v", problems)
	}
}

// --- post verb ---

func TestInvokePostChannel(t *testing.T) {
	var gotPath, gotBody, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"id":"999"}`))
	}))
	defer srv.Close()

	res, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "post",
		Connection: map[string]any{"bot_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"channel": "C1", "text": "hi"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["id"] != "999" || res.Outputs["channel"] != "C1" {
		t.Fatalf("out = %v", res.Outputs)
	}
	if gotPath != "/channels/C1/messages" {
		t.Fatalf("path: got %q", gotPath)
	}
	if gotAuth != "Bot tok" {
		t.Fatalf("auth: got %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"content":"hi"`) {
		t.Fatalf("body: got %q", gotBody)
	}
}

func TestInvokePostDM(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/users/@me/channels" {
			w.Write([]byte(`{"id":"D1"}`))
			return
		}
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()

	res, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "post",
		Connection: map[string]any{"bot_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"user": "U1", "text": "hi"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["channel"] != "D1" {
		t.Fatalf("out = %v", res.Outputs)
	}
	if len(paths) != 2 || paths[0] != "/users/@me/channels" || paths[1] != "/channels/D1/messages" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestInvokePostRequiresText(t *testing.T) {
	_, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "post",
		Connection: map[string]any{"bot_token": "tok"},
		Options:    map[string]any{"channel": "C1"},
	})
	assertInvalidParams(t, err, "options.text is required")
}

func TestInvokePostRequiresChannelOrUser(t *testing.T) {
	_, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "post",
		Connection: map[string]any{"bot_token": "tok"},
		Options:    map[string]any{"text": "hi"},
	})
	assertInvalidParams(t, err, "set options.channel or options.user")
}

func TestInvokePostWebhookOnly(t *testing.T) {
	var gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	res, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "post",
		Connection: map[string]any{"webhook_url": srv.URL},
		Options:    map[string]any{"text": "hello"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["id"] != "" || res.Outputs["channel"] != "" {
		t.Fatalf("webhook-only post should return empty id/channel: %v", res.Outputs)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type: %q", gotContentType)
	}
	if !strings.Contains(gotBody, `"content":"hello"`) {
		t.Fatalf("body: %q", gotBody)
	}
}

func TestInvokePostNoCredentialsErrors(t *testing.T) {
	_, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:    "post",
		Options: map[string]any{"channel": "C1", "text": "hi"},
	})
	assertInvalidParams(t, err, "set bot_token or webhook_url")
}

// --- ask verb ---

func TestInvokeAskThread(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()

	res, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "ask",
		Connection: map[string]any{"bot_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"to": "thread", "channel": "C1", "prompt": "ok?"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["ref"] != "C1:1" {
		t.Fatalf("out = %v", res.Outputs)
	}
	if gotPath != "/channels/C1/messages" {
		t.Fatalf("path: got %q", gotPath)
	}
	if !strings.Contains(gotBody, "ok?") {
		t.Fatalf("body should carry the prompt: %q", gotBody)
	}
}

// TestTwoConcurrentAsksInOneChannelGetDistinctRefs is the regression test for
// the adversarial-pass finding: two pending asks posted to the SAME channel
// used to get the IDENTICAL conversation id (ref was just the channel), so
// the second ask's registration would clobber the first's in the engine's
// (instance, id)-keyed inbox — the first ask would never resolve. ref now
// includes the posted question's own message id, so each ask gets a unique
// conversation id even in the same channel.
func TestTwoConcurrentAsksInOneChannelGetDistinctRefs(t *testing.T) {
	var nextID = 100
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextID++
		fmt.Fprintf(w, `{"id":"%d"}`, nextID)
	}))
	defer srv.Close()
	conn := map[string]any{"bot_token": "tok", "api_base": srv.URL}

	res1, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "ask", Connection: conn,
		Options: map[string]any{"to": "thread", "channel": "C1", "prompt": "deploy staging?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "ask", Connection: conn,
		Options: map[string]any{"to": "thread", "channel": "C1", "prompt": "deploy prod?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref1, ref2 := res1.Outputs["ref"], res2.Outputs["ref"]
	if ref1 == ref2 {
		t.Fatalf("two concurrent asks in the same channel must not share a conversation id, both got %v", ref1)
	}
	if !strings.HasPrefix(ref1.(string), "C1:") || !strings.HasPrefix(ref2.(string), "C1:") {
		t.Fatalf("both refs should still be scoped to channel C1: %v, %v", ref1, ref2)
	}
}

// TestConversationReplyIDMatchesAsk proves the id a "reply" event's
// conversation_reply template produces, for a message that replies (Discord's
// native reply-to) to the ask's own posted message, equals exactly what
// invokeAsk returned as ref — the two sides of the match the engine performs.
// A plain message with no reply-to must NOT match (replied_to is empty, and
// ref always carries a real message id).
func TestConversationReplyIDMatchesAsk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"999"}`))
	}))
	defer srv.Close()
	askRes, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "ask",
		Connection: map[string]any{"bot_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"to": "thread", "channel": "C1", "prompt": "ok?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := askRes.Outputs["ref"].(string)

	gs := &gatewayState{}
	var got map[string]any
	emit := func(payload any) error {
		b, _ := json.Marshal(payload)
		return json.Unmarshal(b, &got)
	}
	// A reply using Discord's own reply-to feature, referencing the ask's
	// own posted message (id "999").
	raw := []byte(`{"op":0,"t":"MESSAGE_CREATE","d":{"id":"r1","channel_id":"C1","content":"approve","author":{"id":"U1","bot":false},"message_reference":{"message_id":"999"}}}`)
	handleFrame(gs, raw, emit, noopLog)
	ctxm := got["context"].(map[string]any)
	gotID := ctxm["channel"].(string) + ":" + ctxm["replied_to"].(string)
	if gotID != ref {
		t.Fatalf("conversation_reply id %q does not match the ask's ref %q", gotID, ref)
	}

	// A plain message, no reply-to: must not resolve to the same ask.
	got = nil
	raw = []byte(`{"op":0,"t":"MESSAGE_CREATE","d":{"id":"r2","channel_id":"C1","content":"unrelated chatter","author":{"id":"U1","bot":false}}}`)
	handleFrame(gs, raw, emit, noopLog)
	ctxm = got["context"].(map[string]any)
	gotID = ctxm["channel"].(string) + ":" + ctxm["replied_to"].(string)
	if gotID == ref {
		t.Fatalf("a plain message with no reply-to must not match the ask's ref %q", ref)
	}
}

func TestInvokeAskDM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/@me/channels" {
			w.Write([]byte(`{"id":"D1"}`))
			return
		}
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()

	res, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "ask",
		Connection: map[string]any{"bot_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"to": "dm", "user": "U1", "prompt": "ok?"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["ref"] != "D1:1" {
		t.Fatalf("out = %v", res.Outputs)
	}
}

func TestInvokeAskMissingToErrors(t *testing.T) {
	_, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "ask",
		Connection: map[string]any{"bot_token": "tok"},
		Options:    map[string]any{"prompt": "ok?"},
	})
	assertInvalidParams(t, err, "must be dm|thread")
}

func TestInvokeAskMissingPromptErrors(t *testing.T) {
	_, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "ask",
		Connection: map[string]any{"bot_token": "tok"},
		Options:    map[string]any{"to": "thread", "channel": "C1"},
	})
	assertInvalidParams(t, err, "options.prompt is required")
}

func TestInvokeAskNoBotTokenErrors(t *testing.T) {
	_, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "ask",
		Connection: map[string]any{"webhook_url": "https://example.test/hook"},
		Options:    map[string]any{"to": "thread", "channel": "C1", "prompt": "ok?"},
	})
	assertInvalidParams(t, err, "needs a bot_token")
}

func assertInvalidParams(t *testing.T, err error, want string) {
	t.Helper()
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
	if !strings.Contains(pe.Message, want) {
		t.Fatalf("expected error to contain %q, got %q", want, pe.Message)
	}
}

// --- Validate ---

func TestValidate(t *testing.T) {
	res, err := discordPlugin{}.Validate(context.Background(), plugin.ValidateRequest{Config: map[string]any{}})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(res.Problems) == 0 {
		t.Fatal("expected a problem for a connection with neither bot_token nor webhook_url")
	}
	res, err = discordPlugin{}.Validate(context.Background(), plugin.ValidateRequest{Config: map[string]any{"bot_token": "tok"}})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(res.Problems) != 0 {
		t.Fatalf("expected no problems with bot_token set: %v", res.Problems)
	}
}

// --- gateway: pure frame handling ---

func noopLog(string, ...any) {}

func TestHandleFrameHello(t *testing.T) {
	gs := &gatewayState{}
	action, hbMS := handleFrame(gs, []byte(`{"op":10,"d":{"heartbeat_interval":41250}}`), func(any) error { return nil }, noopLog)
	if action != actionIdentify {
		t.Fatalf("HELLO should trigger actionIdentify, got %v", action)
	}
	if hbMS != 41250 {
		t.Fatalf("expected heartbeat_interval 41250, got %d", hbMS)
	}
}

func TestHandleFrameReady(t *testing.T) {
	gs := &gatewayState{}
	action, _ := handleFrame(gs, []byte(`{"op":0,"t":"READY","s":1,"d":{"user":{"id":"SELF1"}}}`), func(any) error { return nil }, noopLog)
	if action != actionNone {
		t.Fatalf("READY should not request an action, got %v", action)
	}
	if gs.selfID != "SELF1" {
		t.Fatalf("READY should capture the bot's own user id, got %q", gs.selfID)
	}
	if gs.seq == nil || *gs.seq != 1 {
		t.Fatalf("expected sequence to be tracked as 1, got %v", gs.seq)
	}
}

func TestHandleFrameMessageCreateEmits(t *testing.T) {
	gs := &gatewayState{selfID: "SELF1"}
	var got []map[string]any
	emit := func(payload any) error {
		b, _ := json.Marshal(payload)
		var m map[string]any
		json.Unmarshal(b, &m)
		got = append(got, m)
		return nil
	}
	raw := []byte(`{"op":0,"t":"MESSAGE_CREATE","s":2,"d":{"id":"m1","channel_id":"C123","content":"approve","author":{"id":"U999","bot":false}}}`)
	action, _ := handleFrame(gs, raw, emit, noopLog)
	if action != actionNone {
		t.Fatalf("MESSAGE_CREATE should not request a connection action, got %v", action)
	}
	if len(got) != 1 {
		t.Fatalf("expected one emitted event, got %d", len(got))
	}
	ev := got[0]
	if ev["event"] != "reply" || ev["dedup"] != "m1" {
		t.Fatalf("event = %#v", ev)
	}
	if tg, _ := ev["target"].(map[string]any); tg["assigned"] != true || tg["key"] != "discord:C123:m1" {
		t.Fatalf("the reply's target is not marked assigned on the wire: %#v", ev["target"])
	}
	ctxm, _ := ev["context"].(map[string]any)
	if ctxm["channel"] != "C123" || ctxm["author"] != "U999" || ctxm["text"] != "approve" || ctxm["author_bot"] != false {
		t.Fatalf("context = %#v", ctxm)
	}
}

func TestHandleFrameIgnoresOwnMessageByID(t *testing.T) {
	gs := &gatewayState{selfID: "SELF1"}
	emitted := false
	emit := func(any) error { emitted = true; return nil }
	raw := []byte(`{"op":0,"t":"MESSAGE_CREATE","d":{"id":"m1","channel_id":"C123","content":"the posted draft","author":{"id":"SELF1","bot":false}}}`)
	handleFrame(gs, raw, emit, noopLog)
	if emitted {
		t.Fatal("the gateway's own message (matching selfID) must not be emitted")
	}
}

func TestHandleFrameIgnoresBotMessages(t *testing.T) {
	gs := &gatewayState{selfID: "SELF1"}
	emitted := false
	emit := func(any) error { emitted = true; return nil }
	// A different bot account (author.bot true, different id) must also be
	// ignored — not just the gateway's own id.
	raw := []byte(`{"op":0,"t":"MESSAGE_CREATE","d":{"id":"m2","channel_id":"C123","content":"beep boop","author":{"id":"OTHERBOT","bot":true}}}`)
	handleFrame(gs, raw, emit, noopLog)
	if emitted {
		t.Fatal("a bot message must not be emitted")
	}
}

func TestHandleFrameReconnectOps(t *testing.T) {
	for _, op := range []int{7, 9} {
		gs := &gatewayState{}
		action, _ := handleFrame(gs, []byte(`{"op":`+strconv.Itoa(op)+`}`), func(any) error { return nil }, noopLog)
		if action != actionReconnect {
			t.Fatalf("op %d should trigger actionReconnect, got %v", op, action)
		}
	}
}

// TestHandleFrameResumesWhenSessionCarriesOver is the regression test for
// the gateway reconnect/resume gap: a HELLO arriving with a session already
// established (sessionID set from a prior READY, surviving the reconnect in
// the SAME gatewayState — see StartSource) must trigger actionResume, not a
// fresh actionIdentify. A full re-IDENTIFY starts a brand-new session, so
// Discord has nothing to replay and any event during the reconnect gap is
// lost — for this plugin, a lost MESSAGE_CREATE is a reply nobody ever sees.
func TestHandleFrameResumesWhenSessionCarriesOver(t *testing.T) {
	gs := &gatewayState{}
	action, _ := handleFrame(gs, []byte(`{"op":10,"d":{"heartbeat_interval":41250}}`), func(any) error { return nil }, noopLog)
	if action != actionIdentify {
		t.Fatalf("first HELLO (no session yet) should identify, got %v", action)
	}
	handleFrame(gs, []byte(`{"op":0,"t":"READY","d":{"user":{"id":"SELF1"},"session_id":"sess-abc"}}`), func(any) error { return nil }, noopLog)
	if gs.sessionID != "sess-abc" {
		t.Fatalf("READY should capture session_id, got %q", gs.sessionID)
	}

	// The connection drops and StartSource redials, reusing the SAME gs.
	action, _ = handleFrame(gs, []byte(`{"op":10,"d":{"heartbeat_interval":41250}}`), func(any) error { return nil }, noopLog)
	if action != actionResume {
		t.Fatalf("HELLO after a reconnect with a carried-over session should resume, got %v", action)
	}
}

// TestHandleFrameInvalidSessionResumability proves d:true keeps the session
// (a later HELLO still resumes) while d:false clears it (a later HELLO must
// re-identify) — conflating the two would either waste a resume attempt
// Discord will reject, or throw away a perfectly resumable session.
func TestHandleFrameInvalidSessionResumability(t *testing.T) {
	t.Run("resumable (d:true) keeps the session", func(t *testing.T) {
		gs := &gatewayState{sessionID: "sess-abc"}
		gs.setSeq(5)
		action, _ := handleFrame(gs, []byte(`{"op":9,"d":true}`), func(any) error { return nil }, noopLog)
		if action != actionReconnect {
			t.Fatalf("INVALID_SESSION should trigger actionReconnect, got %v", action)
		}
		if gs.sessionID != "sess-abc" {
			t.Fatalf("a resumable INVALID_SESSION must not clear the session, got %q", gs.sessionID)
		}
		next, _ := handleFrame(gs, []byte(`{"op":10,"d":{"heartbeat_interval":1000}}`), func(any) error { return nil }, noopLog)
		if next != actionResume {
			t.Fatalf("the next HELLO should resume, got %v", next)
		}
	})
	t.Run("not resumable (d:false) clears the session", func(t *testing.T) {
		gs := &gatewayState{sessionID: "sess-abc"}
		gs.setSeq(5)
		handleFrame(gs, []byte(`{"op":9,"d":false}`), func(any) error { return nil }, noopLog)
		if gs.sessionID != "" {
			t.Fatalf("a non-resumable INVALID_SESSION must clear the session, got %q", gs.sessionID)
		}
		if seq, ok := gs.seqValue(); ok {
			t.Fatalf("a non-resumable INVALID_SESSION must also clear the stale sequence, got %d", seq)
		}
		next, _ := handleFrame(gs, []byte(`{"op":10,"d":{"heartbeat_interval":1000}}`), func(any) error { return nil }, noopLog)
		if next != actionIdentify {
			t.Fatalf("the next HELLO should re-identify, got %v", next)
		}
	})
}

func TestHandleFrameMalformedIsIgnored(t *testing.T) {
	gs := &gatewayState{}
	action, _ := handleFrame(gs, []byte(`not json`), func(any) error { return nil }, noopLog)
	if action != actionNone {
		t.Fatalf("malformed frame should be a no-op, got %v", action)
	}
}

func TestHandleFrameTracksSequence(t *testing.T) {
	gs := &gatewayState{}
	handleFrame(gs, []byte(`{"op":11,"s":5}`), func(any) error { return nil }, noopLog) // heartbeat ACK
	if gs.seq == nil || *gs.seq != 5 {
		t.Fatalf("expected sequence 5, got %v", gs.seq)
	}
	handleFrame(gs, []byte(`{"op":11,"s":6}`), func(any) error { return nil }, noopLog)
	if gs.seq == nil || *gs.seq != 6 {
		t.Fatalf("expected sequence to advance to 6, got %v", gs.seq)
	}
}

// TestRunGatewayOnceResumesWithCarriedOverSession proves runGatewayOnce
// sends RESUME (op 6), not IDENTIFY, when handed a gatewayState that already
// carries a session — the shape StartSource reuses across a reconnect.
func TestRunGatewayOnceResumesWithCarriedOverSession(t *testing.T) {
	type received struct {
		op      int
		payload map[string]any
	}
	got := make(chan received, 1)
	srv := newFakeGateway(t, func(c *websocket.Conn) {
		if err := c.WriteJSON(gatewayFrame{Op: gwOpHello, D: json.RawMessage(`{"heartbeat_interval":30000}`)}); err != nil {
			return
		}
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		var f gatewayFrame
		var payload map[string]any
		if json.Unmarshal(data, &f) == nil {
			_ = json.Unmarshal(f.D, &payload)
		}
		got <- received{op: f.Op, payload: payload}
		time.Sleep(100 * time.Millisecond)
	})
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn := discordConn{BotToken: "tok-secret", GatewayURL: wsURL}
	gs := &gatewayState{sessionID: "sess-xyz"}
	gs.setSeq(42)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = runGatewayOnce(ctx, conn, gs, func(any) error { return nil }); close(done) }()

	var r received
	select {
	case r = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the fake gateway never received a frame after HELLO")
	}
	if r.op != gwOpResume {
		t.Fatalf("expected op %d (resume), got %d", gwOpResume, r.op)
	}
	if r.payload["token"] != "tok-secret" || r.payload["session_id"] != "sess-xyz" {
		t.Fatalf("resume payload: %#v", r.payload)
	}
	if seq, ok := r.payload["seq"].(float64); !ok || int(seq) != 42 {
		t.Fatalf("resume payload seq: %#v", r.payload["seq"])
	}
	<-done
}

// --- gateway: URL resolution ---

func TestGatewayDialURLOverrideSkipsBootstrap(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"url":"wss://should-not-be-used"}`))
	}))
	defer srv.Close()

	conn := discordConn{BotToken: "tok", APIBase: srv.URL, GatewayURL: "wss://fake.test/gw"}
	url, err := gatewayDialURL(conn)
	if err != nil {
		t.Fatal(err)
	}
	if url != "wss://fake.test/gw" {
		t.Fatalf("expected the override verbatim, got %q", url)
	}
	if called {
		t.Fatal("gateway_url override should skip the /gateway/bot bootstrap call")
	}
}

func TestGatewayDialURLBootstrap(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Write([]byte(`{"url":"wss://gateway.example.test"}`))
	}))
	defer srv.Close()

	conn := discordConn{BotToken: "bot-secret", APIBase: srv.URL}
	url, err := gatewayDialURL(conn)
	if err != nil {
		t.Fatal(err)
	}
	if url != "wss://gateway.example.test?v=10&encoding=json" {
		t.Fatalf("unexpected gateway url %q", url)
	}
	if gotPath != "/gateway/bot" {
		t.Fatalf("unexpected path %q", gotPath)
	}
	if gotAuth != "Bot bot-secret" {
		t.Fatalf("unexpected Authorization header %q", gotAuth)
	}
}

func TestGatewayDialURLBootstrapError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	conn := discordConn{BotToken: "bad-token", APIBase: srv.URL}
	if _, err := gatewayDialURL(conn); err == nil {
		t.Fatal("expected an error on a non-2xx gateway/bot response")
	}
}

// --- gateway: end-to-end over a fake websocket server ---

// newFakeGateway starts an httptest.Server that upgrades every connection to
// a websocket and runs script against it, in its own goroutine.
func newFakeGateway(t *testing.T, script func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		script(c)
	}))
	return srv
}

// TestRunGatewayOnceEndToEnd drives runGatewayOnce against a fake gateway
// server that speaks HELLO -> (expects IDENTIFY) -> READY -> MESSAGE_CREATE,
// proving the dial, handshake and reply-capture wiring end-to-end.
func TestRunGatewayOnceEndToEnd(t *testing.T) {
	var identifyReceived atomic.Bool
	srv := newFakeGateway(t, func(c *websocket.Conn) {
		if err := c.WriteJSON(gatewayFrame{Op: gwOpHello, D: json.RawMessage(`{"heartbeat_interval":30000}`)}); err != nil {
			return
		}
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		var f gatewayFrame
		if json.Unmarshal(data, &f) == nil && f.Op == gwOpIdentify {
			identifyReceived.Store(true)
		}
		if err := c.WriteJSON(gatewayFrame{Op: gwOpDispatch, T: "READY", D: json.RawMessage(`{"user":{"id":"BOT1"}}`)}); err != nil {
			return
		}
		_ = c.WriteJSON(gatewayFrame{
			Op: gwOpDispatch, T: "MESSAGE_CREATE",
			D: json.RawMessage(`{"id":"m1","channel_id":"C1","content":"approve","author":{"id":"U1","bot":false}}`),
		})
		time.Sleep(100 * time.Millisecond)
	})
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn := discordConn{BotToken: "tok", GatewayURL: wsURL}

	var mu sync.Mutex
	var events []map[string]any
	emit := func(payload any) error {
		mu.Lock()
		defer mu.Unlock()
		b, _ := json.Marshal(payload)
		var m map[string]any
		json.Unmarshal(b, &m)
		events = append(events, m)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = runGatewayOnce(ctx, conn, &gatewayState{}, emit) // ends when the fake server closes the conn

	if !identifyReceived.Load() {
		t.Fatal("expected IDENTIFY to be sent after HELLO")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("expected exactly one emitted event, got %d: %#v", len(events), events)
	}
	ctxm, _ := events[0]["context"].(map[string]any)
	if ctxm["channel"] != "C1" || ctxm["text"] != "approve" || ctxm["author"] != "U1" {
		t.Fatalf("event context = %#v", ctxm)
	}
}

// TestStartSourceIdlesWithoutBotToken proves a webhook-only (or unconfigured)
// instance's StartSource returns cleanly on ctx cancellation without trying
// to dial anything.
func TestStartSourceIdlesWithoutBotToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := discordPlugin{}.StartSource(ctx, plugin.StartSourceRequest{
		Instance: "d",
		Config:   map[string]any{"webhook_url": "https://example.test/hook"},
	}, func(any) error { return nil })
	if err != nil {
		t.Fatalf("StartSource: %v", err)
	}
}

// A reply arrives over the bot's authenticated gateway: its target is one
// Discord assigned, declared and on the wire — conductor only lets such a
// delivery answer an ask.
func TestReplyTargetIsAssigned(t *testing.T) {
	d := discordPlugin{}.Describe()
	var reply *plugin.Event
	for i := range d.Events {
		if d.Events[i].Name == "reply" {
			reply = &d.Events[i]
		}
	}
	if reply == nil || reply.Semantics == nil || reply.Semantics.Target == nil ||
		string(reply.Semantics.Target.Assigned) != "true" || len(reply.Semantics.Target.Scope) != 2 {
		t.Fatalf("reply target not declared assigned: %+v", reply)
	}
}
