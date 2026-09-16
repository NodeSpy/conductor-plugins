package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- Describe -------------------------------------------------------------

func TestDescribe(t *testing.T) {
	d := matrixPlugin{}.Describe()
	if d.Kind != plugin.KindConnector {
		t.Fatalf("Kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "matrix" {
		t.Fatalf("Type: got %q want matrix", d.Type)
	}
	for _, key := range []string{"homeserver", "access_token"} {
		f, ok := d.Connection[key]
		if !ok || !f.Required {
			t.Errorf("connection field %q: got %#v, want present+required", key, f)
		}
	}
	wantEvents := []string{"message", "invite"}
	gotEvents := map[string]bool{}
	for _, ev := range d.Events {
		gotEvents[ev.Name] = true
	}
	for _, w := range wantEvents {
		if !gotEvents[w] {
			t.Errorf("Describe missing event %q", w)
		}
	}
	wantVerbs := []string{
		"send_message", "send_notice", "send_event", "join_room", "leave_room",
		"invite", "redact", "set_name", "set_topic", "get_messages", "whoami", "api",
	}
	gotVerbs := map[string]bool{}
	for _, v := range d.Verbs {
		gotVerbs[v.Name] = true
	}
	for _, w := range wantVerbs {
		if !gotVerbs[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Errorf("Capabilities.Egress: got %v, want empty (homeserver is operator-configured, documented in Connection)", d.Capabilities.Egress)
	}
}

// --- /sync parsing (pure, hermetic) ---------------------------------------

const capturedSync = `{
  "next_batch": "s72595_4483_1934",
  "rooms": {
    "join": {
      "!room1:example.org": {
        "timeline": {
          "events": [
            {
              "type": "m.room.message",
              "sender": "@alice:example.org",
              "event_id": "$msg1:example.org",
              "content": {
                "msgtype": "m.text",
                "body": "hello there",
                "formatted_body": "<b>hello</b> there"
              }
            },
            {
              "type": "m.room.member",
              "sender": "@alice:example.org",
              "event_id": "$invite1:example.org",
              "state_key": "@bob:example.org",
              "content": { "membership": "invite" }
            },
            {
              "type": "m.room.member",
              "sender": "@alice:example.org",
              "event_id": "$join1:example.org",
              "state_key": "@alice:example.org",
              "content": { "membership": "join" }
            },
            {
              "type": "m.reaction",
              "sender": "@alice:example.org",
              "event_id": "$react1:example.org",
              "content": {}
            }
          ]
        }
      }
    }
  }
}`

func TestProcessSync_MessageAndInvite(t *testing.T) {
	seen := map[string]bool{}
	events, next, err := processSync([]byte(capturedSync), false, seen)
	if err != nil {
		t.Fatalf("processSync: %v", err)
	}
	if next != "s72595_4483_1934" {
		t.Fatalf("next: got %q", next)
	}
	if len(events) != 2 {
		t.Fatalf("events: got %d, want 2 (message + invite; membership=join and m.reaction excluded): %#v", len(events), events)
	}

	var msg, invite *wireEvent
	for i := range events {
		switch events[i].Event {
		case "message":
			msg = &events[i]
		case "invite":
			invite = &events[i]
		}
	}
	if msg == nil {
		t.Fatal("no message event emitted")
	}
	if msg.Context["room_id"] != "!room1:example.org" || msg.Context["sender"] != "@alice:example.org" ||
		msg.Context["body"] != "hello there" || msg.Context["msgtype"] != "m.text" ||
		msg.Context["event_id"] != "$msg1:example.org" || msg.Context["formatted_body"] != "<b>hello</b> there" {
		t.Errorf("message context: %#v", msg.Context)
	}
	// Plural filter-alias fields carry the same value as their scalar
	// counterpart (sentry-style), so the daemon's list-contains filter
	// evaluator can match against them.
	if msg.Context["room_ids"] != msg.Context["room_id"] || msg.Context["senders"] != msg.Context["sender"] || msg.Context["msgtypes"] != msg.Context["msgtype"] {
		t.Errorf("message context plural aliases mismatch: %#v", msg.Context)
	}
	if msg.Dedup != "$msg1:example.org" {
		t.Errorf("message dedup: got %q", msg.Dedup)
	}

	if invite == nil {
		t.Fatal("no invite event emitted")
	}
	if invite.Context["room_id"] != "!room1:example.org" || invite.Context["sender"] != "@alice:example.org" ||
		invite.Context["user_id"] != "@bob:example.org" || invite.Context["event_id"] != "$invite1:example.org" {
		t.Errorf("invite context: %#v", invite.Context)
	}
}

func TestProcessSync_FirstSyncSetsSinceWithoutEmitting(t *testing.T) {
	seen := map[string]bool{}
	events, next, err := processSync([]byte(capturedSync), true, seen)
	if err != nil {
		t.Fatalf("processSync: %v", err)
	}
	if next != "s72595_4483_1934" {
		t.Fatalf("next: got %q, want the sync's next_batch even on the first sync", next)
	}
	if len(events) != 0 {
		t.Fatalf("first sync must not emit backlog events, got %d: %#v", len(events), events)
	}
	// seen must stay untouched on a first sync: nothing was "processed".
	if len(seen) != 0 {
		t.Errorf("seen should be empty after a first sync, got %v", seen)
	}
}

func TestProcessSync_Dedup(t *testing.T) {
	seen := map[string]bool{}
	first, _, err := processSync([]byte(capturedSync), false, seen)
	if err != nil {
		t.Fatalf("processSync (1st): %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first call: got %d events, want 2", len(first))
	}
	// Same body again (as if the same batch were re-delivered/re-processed):
	// every event_id has already been seen, so nothing new is emitted.
	second, _, err := processSync([]byte(capturedSync), false, seen)
	if err != nil {
		t.Fatalf("processSync (2nd): %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second call: got %d events, want 0 (deduped): %#v", len(second), second)
	}
}

func TestProcessSync_MalformedBody(t *testing.T) {
	seen := map[string]bool{}
	if _, _, err := processSync([]byte("not json"), false, seen); err == nil {
		t.Fatal("expected an error for malformed /sync body")
	}
}

// --- verbs via httptest ----------------------------------------------------

func newTestServer(t *testing.T, handler http.HandlerFunc) (conn map[string]any, srv *httptest.Server) {
	t.Helper()
	srv = httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	conn = map[string]any{
		"homeserver":   "http://unused.invalid",
		"access_token": "tok123",
		"api_base":     srv.URL,
	}
	return conn, srv
}

func TestInvoke_SendMessage(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var gotBody map[string]any
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		decodeJSONBody(t, r, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"event_id":"$abc:example.org"}`)
	})

	res, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "send_message",
		Connection: conn,
		Options:    map[string]any{"room_id": "!room1:example.org", "body": "hi"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method: got %q want PUT", gotMethod)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("Authorization: got %q", gotAuth)
	}
	if !strings.HasPrefix(gotPath, "/rooms/!room1:example.org/send/m.room.message/") {
		t.Errorf("path: got %q", gotPath)
	}
	if gotBody["msgtype"] != "m.text" || gotBody["body"] != "hi" {
		t.Errorf("body: %#v", gotBody)
	}
	if res.Outputs["event_id"] != "$abc:example.org" {
		t.Errorf("event_id: got %#v", res.Outputs["event_id"])
	}
}

func TestInvoke_SendMessage_CustomMsgtypeAndFormatted(t *testing.T) {
	var gotBody map[string]any
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		decodeJSONBody(t, r, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"event_id":"$x:example.org"}`)
	})
	_, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "send_message",
		Connection: conn,
		Options: map[string]any{
			"room_id": "!r:example.org", "body": "hi", "msgtype": "m.emote",
			"formatted_body": "<i>hi</i>",
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotBody["msgtype"] != "m.emote" || gotBody["formatted_body"] != "<i>hi</i>" || gotBody["format"] != "org.matrix.custom.html" {
		t.Errorf("body: %#v", gotBody)
	}
}

func TestInvoke_SendNotice(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		decodeJSONBody(t, r, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"event_id":"$n:example.org"}`)
	})
	res, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "send_notice", Connection: conn,
		Options: map[string]any{"room_id": "!r:example.org", "body": "notice text"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotBody["msgtype"] != "m.notice" || gotBody["body"] != "notice text" {
		t.Errorf("body: %#v", gotBody)
	}
	if !strings.Contains(gotPath, "/send/m.room.message/") {
		t.Errorf("path: %q", gotPath)
	}
	if res.Outputs["event_id"] != "$n:example.org" {
		t.Errorf("event_id: %#v", res.Outputs["event_id"])
	}
}

func TestInvoke_SendEvent(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		decodeJSONBody(t, r, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"event_id":"$e:example.org"}`)
	})
	res, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "send_event", Connection: conn,
		Options: map[string]any{
			"room_id": "!r:example.org", "event_type": "m.reaction",
			"content": map[string]any{"m.relates_to": map[string]any{"key": "x"}},
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(gotPath, "/send/m.reaction/") {
		t.Errorf("path: %q", gotPath)
	}
	if gotBody["m.relates_to"] == nil {
		t.Errorf("body: %#v", gotBody)
	}
	if res.Outputs["event_id"] != "$e:example.org" {
		t.Errorf("event_id: %#v", res.Outputs["event_id"])
	}
}

func TestInvoke_JoinRoom(t *testing.T) {
	var gotPath, gotMethod string
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"room_id":"!r:example.org"}`)
	})
	res, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "join_room", Connection: conn,
		Options: map[string]any{"room_id_or_alias": "#alias:example.org"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: %q", gotMethod)
	}
	if !strings.HasPrefix(gotPath, "/join/") {
		t.Errorf("path: %q", gotPath)
	}
	out, _ := res.Outputs["result"].(map[string]any)
	if out["room_id"] != "!r:example.org" {
		t.Errorf("result: %#v", res.Outputs["result"])
	}
}

func TestInvoke_LeaveRoom(t *testing.T) {
	var gotPath, gotMethod string
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	_, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "leave_room", Connection: conn,
		Options: map[string]any{"room_id": "!r:example.org"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodPost || !strings.HasSuffix(gotPath, "/leave") {
		t.Errorf("method/path: %q %q", gotMethod, gotPath)
	}
}

func TestInvoke_Invite(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		decodeJSONBody(t, r, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	_, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "invite", Connection: conn,
		Options: map[string]any{"room_id": "!r:example.org", "user_id": "@bob:example.org"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/invite") {
		t.Errorf("path: %q", gotPath)
	}
	if gotBody["user_id"] != "@bob:example.org" {
		t.Errorf("body: %#v", gotBody)
	}
}

func TestInvoke_Redact(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		decodeJSONBody(t, r, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"event_id":"$red:example.org"}`)
	})
	res, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "redact", Connection: conn,
		Options: map[string]any{"room_id": "!r:example.org", "event_id": "$msg:example.org", "reason": "oops"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(gotPath, "/redact/$msg:example.org/") {
		t.Errorf("path: %q", gotPath)
	}
	if gotBody["reason"] != "oops" {
		t.Errorf("body: %#v", gotBody)
	}
	if res.Outputs["event_id"] != "$red:example.org" {
		t.Errorf("event_id: %#v", res.Outputs["event_id"])
	}
}

func TestInvoke_SetNameAndTopic(t *testing.T) {
	var gotPaths []string
	var gotBodies []map[string]any
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		var b map[string]any
		decodeJSONBody(t, r, &b)
		gotBodies = append(gotBodies, b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	if _, err := (matrixPlugin{}).Invoke(plugin.InvokeRequest{
		Verb: "set_name", Connection: conn,
		Options: map[string]any{"room_id": "!r:example.org", "value": "Team Room"},
	}); err != nil {
		t.Fatalf("set_name: %v", err)
	}
	if _, err := (matrixPlugin{}).Invoke(plugin.InvokeRequest{
		Verb: "set_topic", Connection: conn,
		Options: map[string]any{"room_id": "!r:example.org", "value": "topic here"},
	}); err != nil {
		t.Fatalf("set_topic: %v", err)
	}
	if !strings.HasSuffix(gotPaths[0], "/state/m.room.name") || gotBodies[0]["name"] != "Team Room" {
		t.Errorf("set_name: path=%q body=%#v", gotPaths[0], gotBodies[0])
	}
	if !strings.HasSuffix(gotPaths[1], "/state/m.room.topic") || gotBodies[1]["topic"] != "topic here" {
		t.Errorf("set_topic: path=%q body=%#v", gotPaths[1], gotBodies[1])
	}
}

func TestInvoke_GetMessages(t *testing.T) {
	var gotQuery string
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"chunk":[{"event_id":"$1"}],"end":"tok"}`)
	})
	res, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "get_messages", Connection: conn,
		Options: map[string]any{"room_id": "!r:example.org", "limit": 5, "from": "start-tok"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(gotQuery, "limit=5") || !strings.Contains(gotQuery, "from=start-tok") || !strings.Contains(gotQuery, "dir=b") {
		t.Errorf("query: %q", gotQuery)
	}
	out, _ := res.Outputs["result"].(map[string]any)
	if out["end"] != "tok" {
		t.Errorf("result: %#v", res.Outputs["result"])
	}
}

func TestInvoke_Whoami(t *testing.T) {
	var gotPath string
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"user_id":"@bot:example.org"}`)
	})
	res, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{Verb: "whoami", Connection: conn})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotPath != "/account/whoami" {
		t.Errorf("path: %q", gotPath)
	}
	out, _ := res.Outputs["result"].(map[string]any)
	if out["user_id"] != "@bot:example.org" {
		t.Errorf("result: %#v", res.Outputs["result"])
	}
}

func TestInvoke_Api(t *testing.T) {
	var gotPath, gotMethod, gotQuery string
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotQuery = r.URL.Path, r.Method, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	res, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: conn,
		Options: map[string]any{
			"method": "get", "path": "/rooms/!r:example.org/state",
			"query": map[string]any{"foo": "bar"},
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method: %q", gotMethod)
	}
	if gotPath != "/rooms/!r:example.org/state" {
		t.Errorf("path: %q", gotPath)
	}
	if !strings.Contains(gotQuery, "foo=bar") {
		t.Errorf("query: %q", gotQuery)
	}
	out, _ := res.Outputs["result"].(map[string]any)
	if out["ok"] != true {
		t.Errorf("result: %#v", res.Outputs["result"])
	}
}

func TestInvoke_NonSuccessStatus(t *testing.T) {
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"errcode":"M_FORBIDDEN","error":"nope"}`)
	})
	_, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "whoami", Connection: conn,
	})
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	pe, ok := err.(*plugin.Error)
	if !ok {
		t.Fatalf("expected *plugin.Error, got %T: %v", err, err)
	}
	if pe.Code != plugin.CodeInternalError {
		t.Errorf("code: got %d want %d", pe.Code, plugin.CodeInternalError)
	}
	if !strings.Contains(pe.Message, "403") || !strings.Contains(pe.Message, "M_FORBIDDEN") {
		t.Errorf("message should carry status+body: %q", pe.Message)
	}
}

func TestInvoke_MissingConnection(t *testing.T) {
	_, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "whoami",
		Connection: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected an error for missing homeserver/access_token")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestInvoke_UnknownVerb(t *testing.T) {
	conn, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	_, err := matrixPlugin{}.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: conn})
	if err == nil {
		t.Fatal("expected an error for an unknown verb")
	}
}

// --- test helpers -----------------------------------------------------------

func decodeJSONBody(t *testing.T, r *http.Request, v *map[string]any) {
	t.Helper()
	if r.Body == nil {
		*v = map[string]any{}
		return
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		// A body-less request (e.g. GET) legitimately has nothing to decode.
		*v = map[string]any{}
	}
}
