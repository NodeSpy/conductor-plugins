package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestDescribe asserts the declared surface: kind, type, capabilities, the
// publish verb, and the message event.
func TestDescribe(t *testing.T) {
	d := ntfyPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "ntfy" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("ntfy spawns nothing: %#v", d.Capabilities)
	}
	if !contains(d.Capabilities.Egress, "ntfy.sh:443") {
		t.Fatalf("capabilities.egress: %#v", d.Capabilities.Egress)
	}
	if len(d.Verbs) != 1 || d.Verbs[0].Name != "publish" {
		t.Fatalf("verbs: %#v", d.Verbs)
	}
	if !d.Verbs[0].Options["topic"].Required {
		t.Fatalf("publish.topic must be required: %#v", d.Verbs[0].Options["topic"])
	}
	if len(d.Events) != 1 || d.Events[0].Name != "message" {
		t.Fatalf("events: %#v", d.Events)
	}
	for _, k := range []string{"topics", "priorities", "tags", "topic", "priority"} {
		if _, ok := d.Events[0].Filters[k]; !ok {
			t.Errorf("message event missing filter %q", k)
		}
	}
}

// TestPublish drives Invoke against an httptest.Server standing in for the
// ntfy server, asserting the JSON body it POSTs and the auth header it
// applies, and that the response is parsed back into outputs.
func TestPublish(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"abc123","time":1700000000,"event":"message","topic":"alerts","message":"hi"}`))
	}))
	defer srv.Close()

	p := ntfyPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "publish",
		Connection: map[string]any{"server": srv.URL, "token": "tok_123"},
		Options: map[string]any{
			"topic":    "alerts",
			"message":  "disk is full",
			"title":    "warning",
			"priority": "high",
			"tags":     []any{"warning", "computer"},
			"click":    "https://example.com",
			"markdown": true,
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if gotPath != "/" {
		t.Errorf("path: got %q want /", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type: got %q", gotContentType)
	}
	if gotAuth != "Bearer tok_123" {
		t.Errorf("auth header: got %q want Bearer tok_123", gotAuth)
	}
	if gotBody["topic"] != "alerts" || gotBody["message"] != "disk is full" {
		t.Fatalf("posted body missing topic/message: %#v", gotBody)
	}
	if p, ok := gotBody["priority"].(float64); !ok || int(p) != 4 {
		t.Errorf("posted priority: got %#v want 4 (high)", gotBody["priority"])
	}
	if gotBody["markdown"] != true {
		t.Errorf("posted markdown: got %#v want true", gotBody["markdown"])
	}
	tags, _ := gotBody["tags"].([]any)
	if len(tags) != 2 || tags[0] != "warning" {
		t.Errorf("posted tags: got %#v", gotBody["tags"])
	}

	if res.Outputs["status_code"] != http.StatusOK {
		t.Errorf("status_code: got %v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["topic"] != "alerts" {
		t.Fatalf("result: got %#v", res.Outputs["result"])
	}
}

// TestPublishBasicAuth checks the Basic-auth path (username+password, no
// token) applies when no token is configured.
func TestPublishBasicAuth(t *testing.T) {
	var user, pass string
	var haveAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, haveAuth = r.BasicAuth()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := ntfyPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "publish",
		Connection: map[string]any{"server": srv.URL, "username": "alice", "password": "s3cret"},
		Options:    map[string]any{"topic": "alerts"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !haveAuth || user != "alice" || pass != "s3cret" {
		t.Fatalf("basic auth: got user=%q pass=%q haveAuth=%v", user, pass, haveAuth)
	}
}

// TestPublishRequiresTopic covers the one required option.
func TestPublishRequiresTopic(t *testing.T) {
	p := ntfyPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "publish", Options: map[string]any{}})
	if err == nil {
		t.Fatal("expected an error when topic is missing")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams, got %v", err)
	}
}

// TestUnknownVerb covers the connector's one-verb surface.
func TestUnknownVerb(t *testing.T) {
	p := ntfyPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope"})
	if err == nil {
		t.Fatal("expected an error for an unknown verb")
	}
}

// TestHandleLine feeds the stream parser canned newline-delimited JSON —
// open, keepalive, two messages, and a re-delivered duplicate of the first —
// and asserts only the two distinct message events emit, with the right
// context/filter keys, and that the duplicate is suppressed by dedup on id.
func TestHandleLine(t *testing.T) {
	lines := []string{
		`{"id":"open1","time":1700000000,"event":"open","topic":"alerts"}`,
		`{"id":"keep1","time":1700000001,"event":"keepalive","topic":"alerts"}`,
		`{"id":"msg1","time":1700000002,"event":"message","topic":"alerts","message":"disk full","title":"warn","priority":4,"tags":["warning"],"click":"https://x"}`,
		`{"id":"msg2","time":1700000003,"event":"message","topic":"ci","message":"build ok","priority":3}`,
		// re-delivery of msg1 (same id) must be suppressed
		`{"id":"msg1","time":1700000004,"event":"message","topic":"alerts","message":"disk full","title":"warn","priority":4}`,
	}

	dedup := newDedup(16)
	var emitted []map[string]any
	emit := func(payload any) error {
		m, ok := payload.(map[string]any)
		if !ok {
			t.Fatalf("emit payload not a map: %#v", payload)
		}
		emitted = append(emitted, m)
		return nil
	}

	for _, l := range lines {
		if err := handleLine([]byte(l), dedup, emit); err != nil {
			t.Fatalf("handleLine: %v", err)
		}
	}

	if len(emitted) != 2 {
		t.Fatalf("expected 2 emitted events (open/keepalive/duplicate suppressed), got %d: %#v", len(emitted), emitted)
	}

	first := emitted[0]
	if first["event"] != "message" || first["dedup"] != "msg1" {
		t.Fatalf("first event: %#v", first)
	}
	ctx1, ok := first["context"].(map[string]any)
	if !ok {
		t.Fatalf("first context not a map: %#v", first["context"])
	}
	wantCtx := map[string]any{
		"topic": "alerts", "message": "disk full", "title": "warn",
		"priority": 4, "click": "https://x", "id": "msg1", "time": int64(1700000002),
		"topics": "alerts", "priorities": 4,
	}
	for k, want := range wantCtx {
		if ctx1[k] != want {
			t.Errorf("context[%q] = %#v, want %#v", k, ctx1[k], want)
		}
	}
	tags, ok := ctx1["tags"].([]string)
	if !ok || len(tags) != 1 || tags[0] != "warning" {
		t.Errorf("context[tags] = %#v", ctx1["tags"])
	}

	second := emitted[1]
	if second["dedup"] != "msg2" {
		t.Fatalf("second event dedup: %#v", second["dedup"])
	}
}

// TestHandleLineMalformed proves a garbage line is skipped rather than
// failing the whole stream.
func TestHandleLineMalformed(t *testing.T) {
	dedup := newDedup(16)
	emitCount := 0
	emit := func(any) error { emitCount++; return nil }

	if err := handleLine([]byte(`not json`), dedup, emit); err != nil {
		t.Fatalf("handleLine on malformed input should not error: %v", err)
	}
	if emitCount != 0 {
		t.Fatalf("malformed line must not emit: %d", emitCount)
	}
}

// TestPriorityInt covers the accepted forms: named levels, numeric strings,
// and numbers.
func TestPriorityInt(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{"min", 1}, {"low", 2}, {"default", 3}, {"high", 4}, {"max", 5},
		{"HIGH", 4}, {"3", 3}, {float64(5), 5}, {2, 2},
	}
	for _, tc := range cases {
		got, err := priorityInt(tc.in)
		if err != nil {
			t.Fatalf("priorityInt(%#v): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("priorityInt(%#v) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if _, err := priorityInt("bogus"); err == nil {
		t.Error("expected an error for an invalid priority string")
	}
}

// TestDedupSetEviction proves the bounded set evicts the oldest key once full.
func TestDedupSetEviction(t *testing.T) {
	d := newDedup(2)
	if !d.Add("a") || !d.Add("b") {
		t.Fatal("first insertions must be new")
	}
	if d.Add("a") {
		t.Fatal("a is a duplicate, must report false")
	}
	d.Add("c") // evicts "a"
	if !d.Add("a") {
		t.Fatal("a was evicted, re-adding it must report new")
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
