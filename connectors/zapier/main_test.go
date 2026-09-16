package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// TestDescribe asserts the declared surface: kind, type, egress capability,
// the single "send" verb, and the single "event" source event.
func TestDescribe(t *testing.T) {
	d := zapierPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "zapier" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Egress, "hooks.zapier.com:443") {
		t.Fatalf("egress: %#v", d.Capabilities.Egress)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns || len(d.Capabilities.FS) != 0 {
		t.Fatalf("expected no commands/spawns/fs, got %#v", d.Capabilities)
	}
	if len(d.Verbs) != 1 || d.Verbs[0].Name != "send" {
		t.Fatalf("verbs: %#v", d.Verbs)
	}
	if _, ok := d.Verbs[0].Options["body"]; !ok {
		t.Error("send verb missing body option")
	}
	if len(d.Events) != 1 || d.Events[0].Name != "event" {
		t.Fatalf("events: %#v", d.Events)
	}
	if !d.Events[0].Dynamic {
		t.Error("event should be declared Dynamic (payload shape is operator-defined)")
	}
	for _, k := range []string{"events", "event"} {
		if _, ok := d.Events[0].Filters[k]; !ok {
			t.Errorf("missing filter key %q", k)
		}
	}
}

// TestValidateHookURL proves send refuses any host other than
// hooks.zapier.com, and accepts a well-formed Catch Hook URL.
func TestValidateHookURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"valid catch hook", "https://hooks.zapier.com/hooks/catch/123/abc/", false},
		{"wrong host", "https://evil.example.com/hooks/catch/123/abc/", true},
		{"subdomain spoof", "https://hooks.zapier.com.evil.example.com/x", true},
		{"malformed url", "://not a url", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHookURL(tc.raw)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
		})
	}
}

// TestInvokeSendRejectsNonZapierHost proves Invoke enforces the host check
// before ever attempting a network call.
func TestInvokeSendRejectsNonZapierHost(t *testing.T) {
	_, err := zapierPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "send",
		Connection: map[string]any{"hook_url": "https://attacker.example.com/steal"},
		Options:    map[string]any{"body": map[string]any{"a": 1}},
	})
	if err == nil {
		t.Fatal("expected error for non-zapier host")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams, got %v", err)
	}
}

// TestInvokeSendRequiresHookURL proves Invoke refuses when neither
// options.hook_url nor connection.hook_url is set.
func TestInvokeSendRequiresHookURL(t *testing.T) {
	_, err := zapierPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:    "send",
		Options: map[string]any{"body": map[string]any{"a": 1}},
	})
	if err == nil {
		t.Fatal("expected error for missing hook_url")
	}
}

// TestInvokeUnknownVerb proves Invoke refuses any verb other than send.
func TestInvokeUnknownVerb(t *testing.T) {
	_, err := zapierPlugin{}.Invoke(plugin.InvokeRequest{Verb: "nope"})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams, got %v", err)
	}
}

// TestPostToHook drives the actual HTTP POST against an httptest.Server,
// asserting the method, content type, and JSON body sent, and that a
// successful JSON response is parsed into "result".
func TestPostToHook(t *testing.T) {
	var gotMethod, gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	res, err := postToHook(srv.URL, map[string]any{"name": "hi", "n": float64(3)})
	if err != nil {
		t.Fatalf("postToHook: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type: got %q", gotContentType)
	}
	if gotBody["name"] != "hi" || gotBody["n"] != float64(3) {
		t.Errorf("posted body: %#v", gotBody)
	}
	if res.Outputs["status_code"] != http.StatusOK {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "success" {
		t.Errorf("result: %#v", res.Outputs["result"])
	}
}

// TestPostToHookNon2xx proves a non-2xx response becomes a CodeInternalError
// carrying the status and body.
func TestPostToHookNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := postToHook(srv.URL, map[string]any{"a": 1})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
}

// TestParseEventJSON proves a JSON object's top-level keys are flattened
// into context alongside a full copy under payload, and that an id yields a
// dedup key.
func TestParseEventJSON(t *testing.T) {
	body := []byte(`{"id": "evt_1", "title": "Order placed", "events": ["order.created"], "event": "order.created", "amount": 42}`)
	ev, ok := parseEvent(body)
	if !ok {
		t.Fatal("expected parseEvent to succeed")
	}
	if ev["event"] != "event" || ev["kind"] != "event" {
		t.Fatalf("event/kind: %#v %#v", ev["event"], ev["kind"])
	}
	if ev["title"] != "Order placed" {
		t.Errorf("title: got %#v", ev["title"])
	}
	if ev["dedup"] != "evt_1" {
		t.Errorf("dedup: got %#v", ev["dedup"])
	}
	ctx, ok := ev["context"].(map[string]any)
	if !ok {
		t.Fatalf("context not a map: %#v", ev["context"])
	}
	if ctx["amount"] != float64(42) {
		t.Errorf("context.amount: got %#v", ctx["amount"])
	}
	events, ok := ctx["events"].([]any)
	if !ok || len(events) != 1 || events[0] != "order.created" {
		t.Errorf("context.events: got %#v", ctx["events"])
	}
	if ctx["event"] != "order.created" {
		t.Errorf("context.event: got %#v", ctx["event"])
	}
	payload, ok := ctx["payload"].(map[string]any)
	if !ok || payload["id"] != "evt_1" {
		t.Errorf("context.payload: got %#v", ctx["payload"])
	}
}

// TestParseEventNonJSON proves a non-JSON-object body falls back to a bare
// context.body string instead of erroring.
func TestParseEventNonJSON(t *testing.T) {
	ev, ok := parseEvent([]byte("plain text body"))
	if !ok {
		t.Fatal("expected parseEvent to succeed for a non-JSON body")
	}
	ctx, ok := ev["context"].(map[string]any)
	if !ok || ctx["body"] != "plain text body" {
		t.Errorf("context.body: got %#v", ev["context"])
	}
}

// TestParseEventEmpty proves an empty delivery (blank body, or "{}") is
// reported as nothing worth emitting.
func TestParseEventEmpty(t *testing.T) {
	if _, ok := parseEvent([]byte("")); ok {
		t.Error("empty body should not parse")
	}
	if _, ok := parseEvent([]byte("{}")); ok {
		t.Error("empty JSON object should not parse")
	}
}

// TestVerifyToken drives the token check directly: header wins when present,
// falls back to the ?token= query parameter, and rejects a wrong or absent
// token.
func TestVerifyToken(t *testing.T) {
	mkReq := func(header, query string) *sourcekit.Request {
		r := &sourcekit.Request{Header: http.Header{}, Query: url.Values{}}
		if query != "" {
			r.Query.Set("token", query)
		}
		if header != "" {
			r.Header.Set("X-Conductor-Token", header)
		}
		return r
	}
	cases := []struct {
		name          string
		header, query string
		want          bool
	}{
		{"header match", "s3cret", "", true},
		{"header mismatch", "wrong", "", false},
		{"query fallback match", "", "s3cret", true},
		{"query fallback mismatch", "", "wrong", false},
		{"header wins over query", "s3cret", "wrong", true},
		{"neither present", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyToken("s3cret", mkReq(tc.header, tc.query)); got != tc.want {
				t.Errorf("verifyToken(header=%q, query=%q): got %v want %v", tc.header, tc.query, got, tc.want)
			}
		})
	}
}

// TestRequireWebhookSecret asserts the fail-closed policy: empty secret and
// no allow_unsigned refuses to start; either a secret or allow_unsigned lets
// it through.
func TestRequireWebhookSecret(t *testing.T) {
	if err := requireWebhookSecret("zapier", "", false, "webhook.secret"); err == nil {
		t.Fatal("empty secret with no allow_unsigned should fail closed")
	}
	if err := requireWebhookSecret("zapier", "", true, "webhook.secret"); err != nil {
		t.Fatalf("allow_unsigned should permit an empty secret: %v", err)
	}
	if err := requireWebhookSecret("zapier", "s3cret", false, "webhook.secret"); err != nil {
		t.Fatalf("a configured secret should never fail: %v", err)
	}
}

// TestStartSourceRequiresListenAddress proves StartSource refuses to start
// with no webhook.listen configured, without touching the network.
func TestStartSourceRequiresListenAddress(t *testing.T) {
	err := zapierPlugin{}.StartSource(nil, plugin.StartSourceRequest{
		Instance: "test",
		Config:   map[string]any{},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected error for missing webhook.listen")
	}
}

// TestStartSourceRequiresSecret proves StartSource fails closed when
// webhook.secret is absent and allow_unsigned is not set, without binding a
// listener.
func TestStartSourceRequiresSecret(t *testing.T) {
	err := zapierPlugin{}.StartSource(nil, plugin.StartSourceRequest{
		Instance: "test",
		Config: map[string]any{
			"webhook": map[string]any{"listen": ":0"},
		},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected fail-closed error for missing secret")
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

// asPluginError unwraps a *plugin.Error without importing errors just for the
// test (the SDK returns the concrete type directly).
func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}
