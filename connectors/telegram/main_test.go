package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- Describe ---

func TestDescribe(t *testing.T) {
	d := telegram{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "telegram" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Egress, "api.telegram.org:443") {
		t.Fatalf("capabilities.egress: %#v", d.Capabilities.Egress)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("telegram spawns nothing: %#v", d.Capabilities)
	}
	wantVerbs := []string{
		"send_message", "send_photo", "send_document", "edit_message_text",
		"delete_message", "answer_callback_query", "get_updates",
		"set_webhook", "delete_webhook", "get_me", "api",
	}
	got := map[string]bool{}
	for _, v := range d.Verbs {
		got[v.Name] = true
	}
	for _, w := range wantVerbs {
		if !got[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Verbs) != len(wantVerbs) {
		t.Errorf("verb count: got %d want %d", len(d.Verbs), len(wantVerbs))
	}
	wantEvents := []string{"message", "callback_query"}
	gotEv := map[string]bool{}
	for _, e := range d.Events {
		gotEv[e.Name] = true
	}
	for _, w := range wantEvents {
		if !gotEv[w] {
			t.Errorf("Describe missing event %q", w)
		}
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// --- update parsing: message (incl. command extraction) + callback_query ---

func TestParseUpdateMessage(t *testing.T) {
	body := []byte(`{
		"update_id": 100,
		"message": {
			"message_id": 55,
			"from": {"id": 42, "username": "alice"},
			"chat": {"id": -7, "type": "supergroup"},
			"text": "hello there"
		}
	}`)
	evs := parseUpdate(body)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev["event"] != "message" || ev["kind"] != "message" {
		t.Fatalf("event/kind: %#v", ev)
	}
	if ev["dedup"] != "100" {
		t.Fatalf("dedup: %#v", ev["dedup"])
	}
	ctx, ok := ev["context"].(map[string]any)
	if !ok {
		t.Fatalf("context: %#v", ev["context"])
	}
	want := map[string]any{
		"chat_id": int64(-7), "chat_type": "supergroup", "text": "hello there",
		"from": "alice", "from_id": int64(42), "message_id": int64(55),
		"command":  "",
		"chat_ids": int64(-7), "chat_types": "supergroup", "commands": "",
	}
	for k, v := range want {
		if ctx[k] != v {
			t.Errorf("context[%q] = %#v, want %#v", k, ctx[k], v)
		}
	}
}

func TestParseUpdateMessageCommand(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"/start", "start"},
		{"/help@my_bot", "help"},
		{"/deploy prod now", "deploy"},
		{"not a command", ""},
		{"", ""},
	}
	for _, tc := range cases {
		body, _ := json.Marshal(map[string]any{
			"update_id": 1,
			"message": map[string]any{
				"message_id": 1,
				"chat":       map[string]any{"id": 1, "type": "private"},
				"text":       tc.text,
			},
		})
		evs := parseUpdate(body)
		if len(evs) != 1 {
			t.Fatalf("text %q: expected 1 event, got %d", tc.text, len(evs))
		}
		ctx := evs[0]["context"].(map[string]any)
		if ctx["command"] != tc.want {
			t.Errorf("text %q: command = %#v, want %q", tc.text, ctx["command"], tc.want)
		}
		if ctx["commands"] != tc.want {
			t.Errorf("text %q: commands (filter alias) = %#v, want %q", tc.text, ctx["commands"], tc.want)
		}
	}
}

func TestParseUpdateCallbackQuery(t *testing.T) {
	body := []byte(`{
		"update_id": 200,
		"callback_query": {
			"id": "cbid1",
			"data": "approve:42",
			"from": {"id": 9, "username": "bob"},
			"message": {"message_id": 77, "chat": {"id": 5, "type": "private"}}
		}
	}`)
	evs := parseUpdate(body)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev["event"] != "callback_query" || ev["kind"] != "callback_query" {
		t.Fatalf("event/kind: %#v", ev)
	}
	if ev["dedup"] != "200" {
		t.Fatalf("dedup: %#v", ev["dedup"])
	}
	ctx := ev["context"].(map[string]any)
	want := map[string]any{
		"callback_data": "approve:42", "from": "bob", "from_id": int64(9),
		"message_id": int64(77), "chat_id": int64(5), "chat_ids": int64(5),
	}
	for k, v := range want {
		if ctx[k] != v {
			t.Errorf("context[%q] = %#v, want %#v", k, ctx[k], v)
		}
	}
}

func TestParseUpdateNeitherShape(t *testing.T) {
	evs := parseUpdate([]byte(`{"update_id": 1}`))
	if evs != nil {
		t.Fatalf("expected no events, got %#v", evs)
	}
}

func TestParseUpdateMalformedJSON(t *testing.T) {
	evs := parseUpdate([]byte(`not json`))
	if evs != nil {
		t.Fatalf("expected no events for malformed body, got %#v", evs)
	}
}

// --- dedup: two updates with the same update_id must fire once ---

func TestDedupSameUpdateID(t *testing.T) {
	dedup := newDedupForTest()
	body := []byte(`{"update_id": 5, "message": {"message_id": 1, "chat": {"id": 1, "type": "private"}, "text": "hi"}}`)
	evs1 := parseUpdate(body)
	dk1, _ := evs1[0]["dedup"].(string)
	if !dedup.add(dk1) {
		t.Fatal("first delivery should be new")
	}
	evs2 := parseUpdate(body)
	dk2, _ := evs2[0]["dedup"].(string)
	if dedup.add(dk2) {
		t.Fatal("redelivery with same update_id should be a duplicate")
	}
}

// tinyDedup mirrors sourcekit.Dedup's Add semantics without importing it,
// keeping this dedup assertion self-contained and hermetic.
type tinyDedup struct{ seen map[string]bool }

func newDedupForTest() *tinyDedup { return &tinyDedup{seen: map[string]bool{}} }

func (d *tinyDedup) add(key string) bool {
	if d.seen[key] {
		return false
	}
	d.seen[key] = true
	return true
}

// --- secret-token accept/reject ---

func TestValidSecretToken(t *testing.T) {
	if !validSecretToken("shh", "shh", false) {
		t.Error("matching secret should be accepted")
	}
	if validSecretToken("shh", "nope", false) {
		t.Error("mismatched secret should be rejected")
	}
	if validSecretToken("shh", "", false) {
		t.Error("empty header against a configured secret should be rejected")
	}
	if validSecretToken("", "anything", false) {
		t.Error("no configured secret and allow_unsigned false should be rejected")
	}
	if !validSecretToken("", "anything", true) {
		t.Error("no configured secret with allow_unsigned true should be accepted")
	}
}

func TestRequireWebhookSecret(t *testing.T) {
	if err := requireWebhookSecret("telegram", "", false); err == nil {
		t.Fatal("expected error: no secret, no allow_unsigned")
	}
	if err := requireWebhookSecret("telegram", "", true); err != nil {
		t.Fatalf("allow_unsigned true should pass: %v", err)
	}
	if err := requireWebhookSecret("telegram", "shh", false); err != nil {
		t.Fatalf("secret configured should pass: %v", err)
	}
}

// --- verb call building: pure, hermetic ---

func TestVerbCall(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		wantMethod string
		wantParams map[string]any
	}{
		{
			name:       "send_message",
			verb:       "send_message",
			opts:       map[string]any{"chat_id": float64(1), "text": "hi", "parse_mode": "Markdown"},
			wantMethod: "sendMessage",
			wantParams: map[string]any{"chat_id": float64(1), "text": "hi", "parse_mode": "Markdown"},
		},
		{
			name:       "send_photo",
			verb:       "send_photo",
			opts:       map[string]any{"chat_id": "@chan", "photo": "https://x/1.png"},
			wantMethod: "sendPhoto",
			wantParams: map[string]any{"chat_id": "@chan", "photo": "https://x/1.png"},
		},
		{
			name:       "send_document",
			verb:       "send_document",
			opts:       map[string]any{"chat_id": float64(1), "document": "file_id_1", "caption": "doc"},
			wantMethod: "sendDocument",
			wantParams: map[string]any{"chat_id": float64(1), "document": "file_id_1", "caption": "doc"},
		},
		{
			name:       "edit_message_text",
			verb:       "edit_message_text",
			opts:       map[string]any{"chat_id": float64(1), "message_id": float64(9), "text": "new"},
			wantMethod: "editMessageText",
			wantParams: map[string]any{"chat_id": float64(1), "message_id": float64(9), "text": "new"},
		},
		{
			name:       "delete_message",
			verb:       "delete_message",
			opts:       map[string]any{"chat_id": float64(1), "message_id": float64(9)},
			wantMethod: "deleteMessage",
			wantParams: map[string]any{"chat_id": float64(1), "message_id": float64(9)},
		},
		{
			name:       "answer_callback_query",
			verb:       "answer_callback_query",
			opts:       map[string]any{"callback_query_id": "cbid1", "text": "ok", "show_alert": true},
			wantMethod: "answerCallbackQuery",
			wantParams: map[string]any{"callback_query_id": "cbid1", "text": "ok", "show_alert": true},
		},
		{
			name:       "get_updates",
			verb:       "get_updates",
			opts:       map[string]any{"offset": float64(5), "limit": float64(10)},
			wantMethod: "getUpdates",
			wantParams: map[string]any{"offset": float64(5), "limit": float64(10)},
		},
		{
			name:       "set_webhook",
			verb:       "set_webhook",
			opts:       map[string]any{"url": "https://x/hook", "secret_token": "shh"},
			wantMethod: "setWebhook",
			wantParams: map[string]any{"url": "https://x/hook", "secret_token": "shh"},
		},
		{
			name:       "delete_webhook",
			verb:       "delete_webhook",
			opts:       map[string]any{},
			wantMethod: "deleteWebhook",
			wantParams: map[string]any{},
		},
		{
			name:       "get_me",
			verb:       "get_me",
			opts:       map[string]any{},
			wantMethod: "getMe",
			wantParams: map[string]any{},
		},
		{
			name:       "api escape hatch",
			verb:       "api",
			opts:       map[string]any{"method": "pinChatMessage", "params": map[string]any{"chat_id": float64(1), "message_id": float64(2)}},
			wantMethod: "pinChatMessage",
			wantParams: map[string]any{"chat_id": float64(1), "message_id": float64(2)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method, params, err := verbCall(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("verbCall(%s): unexpected error: %v", tc.verb, err)
			}
			if method != tc.wantMethod {
				t.Fatalf("verbCall(%s) method = %q, want %q", tc.verb, method, tc.wantMethod)
			}
			if !reflect.DeepEqual(params, tc.wantParams) {
				t.Fatalf("verbCall(%s) params = %#v, want %#v", tc.verb, params, tc.wantParams)
			}
		})
	}
}

func TestVerbCallErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"send_message", map[string]any{"text": "hi"}},               // no chat_id
		{"send_message", map[string]any{"chat_id": float64(1)}},      // no text
		{"send_photo", map[string]any{"chat_id": float64(1)}},        // no photo
		{"send_document", map[string]any{"chat_id": float64(1)}},     // no document
		{"edit_message_text", map[string]any{"chat_id": float64(1)}}, // no message_id/text
		{"delete_message", map[string]any{"chat_id": float64(1)}},    // no message_id
		{"answer_callback_query", map[string]any{}},                  // no callback_query_id
		{"set_webhook", map[string]any{}},                            // no url
		{"api", map[string]any{}},                                    // no method
		{"nope", map[string]any{}},                                   // unknown verb
	}
	for _, tc := range cases {
		if _, _, err := verbCall(tc.verb, tc.opts); err == nil {
			t.Errorf("verbCall(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// --- verb execution against a fake Telegram Bot API ---

func newTestServer(t *testing.T, wantMethodPath string, resp map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
		}
		wantSuffix := "/bottest-token/" + wantMethodPath
		if r.URL.Path != wantSuffix {
			t.Errorf("path: got %q want %q", r.URL.Path, wantSuffix)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type: got %q", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestInvokeSendMessage(t *testing.T) {
	srv := newTestServer(t, "sendMessage", map[string]any{
		"ok":     true,
		"result": map[string]any{"message_id": 123, "text": "hi"},
	})
	defer srv.Close()

	res, err := telegram{}.Invoke(plugin.InvokeRequest{
		Verb:       "send_message",
		Connection: map[string]any{"token": "test-token", "api_base": srv.URL},
		Options:    map[string]any{"chat_id": float64(1), "text": "hi"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["ok"] != true {
		t.Fatalf("ok: %#v", res.Outputs["ok"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["message_id"] != float64(123) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeOKFalseIsInternalError(t *testing.T) {
	srv := newTestServer(t, "sendMessage", map[string]any{
		"ok": false, "description": "Bad Request: chat not found",
	})
	defer srv.Close()

	_, err := telegram{}.Invoke(plugin.InvokeRequest{
		Verb:       "send_message",
		Connection: map[string]any{"token": "test-token", "api_base": srv.URL},
		Options:    map[string]any{"chat_id": float64(1), "text": "hi"},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
	if pe.Message != "Bad Request: chat not found" {
		t.Fatalf("error should surface Telegram's description: %v", pe.Message)
	}
}

func TestInvokeMissingToken(t *testing.T) {
	_, err := telegram{}.Invoke(plugin.InvokeRequest{
		Verb:       "get_me",
		Connection: map[string]any{},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing token, got %v", err)
	}
}

func TestInvokeUnknownVerb(t *testing.T) {
	_, err := telegram{}.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{"token": "test-token"},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for unknown verb, got %v", err)
	}
}

func TestInvokeGetMe(t *testing.T) {
	srv := newTestServer(t, "getMe", map[string]any{
		"ok": true, "result": map[string]any{"id": float64(1), "username": "mybot"},
	})
	defer srv.Close()

	res, err := telegram{}.Invoke(plugin.InvokeRequest{
		Verb:       "get_me",
		Connection: map[string]any{"token": "test-token", "api_base": srv.URL},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result := res.Outputs["result"].(map[string]any)
	if result["username"] != "mybot" {
		t.Fatalf("result: %#v", result)
	}
}

func TestInvokeAPIEscapeHatch(t *testing.T) {
	srv := newTestServer(t, "pinChatMessage", map[string]any{"ok": true, "result": true})
	defer srv.Close()

	res, err := telegram{}.Invoke(plugin.InvokeRequest{
		Verb:       "api",
		Connection: map[string]any{"token": "test-token", "api_base": srv.URL},
		Options:    map[string]any{"method": "pinChatMessage", "params": map[string]any{"chat_id": float64(1), "message_id": float64(2)}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outputs["result"] != true {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

// asPluginError unwraps a *plugin.Error without needing errors.As just for
// the test (the SDK returns the concrete type directly).
func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}
