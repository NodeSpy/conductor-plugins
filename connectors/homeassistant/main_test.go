package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// recordedRequest captures everything a verb test needs to assert about the
// HTTP call the connector made, without hitting a real Home Assistant.
type recordedRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   map[string]any
}

func newRecordingServer(t *testing.T, status int, respBody string, contentType string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			_ = json.Unmarshal(b, &rec.body)
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{
		"base_url": "http://ignored.example",
		"api_base": srv.URL, // test-only override, see baseURL()
		"token":    "secret-token",
	}
}

func TestCallService(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `[{"entity_id":"light.kitchen","state":"on"}]`, "application/json")
	p := haPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "call_service",
		Connection: testConn(srv),
		Options: map[string]any{
			"domain": "light", "service": "turn_on",
			"entity_id": "light.kitchen",
			"data":      map[string]any{"brightness": float64(200)},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.method != http.MethodPost || rec.path != "/services/light/turn_on" {
		t.Fatalf("method/path: got %s %s", rec.method, rec.path)
	}
	if rec.auth != "Bearer secret-token" {
		t.Fatalf("auth header: got %q", rec.auth)
	}
	if rec.body["entity_id"] != "light.kitchen" {
		t.Fatalf("entity_id not merged into body: %#v", rec.body)
	}
	if rec.body["brightness"] != float64(200) {
		t.Fatalf("data not merged into body: %#v", rec.body)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestCallServiceEntityIDList(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `[]`, "application/json")
	p := haPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "call_service",
		Connection: testConn(srv),
		Options: map[string]any{
			"domain": "light", "service": "turn_off",
			"entity_id": []any{"light.a", "light.b"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := rec.body["entity_id"].([]any)
	if !ok || !reflect.DeepEqual(got, []any{"light.a", "light.b"}) {
		t.Fatalf("entity_id list not preserved: %#v", rec.body["entity_id"])
	}
}

func TestCallServiceMissingRequired(t *testing.T) {
	p := haPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "call_service", Options: map[string]any{"domain": "light"}})
	if err == nil {
		t.Fatal("expected error for missing service")
	}
	assertCode(t, err, plugin.CodeInvalidParams)
}

func TestGetState(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"entity_id":"sensor.temp","state":"21.5"}`, "application/json")
	p := haPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "get_state", Connection: testConn(srv),
		Options: map[string]any{"entity_id": "sensor.temp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodGet || rec.path != "/states/sensor.temp" {
		t.Fatalf("method/path: %s %s", rec.method, rec.path)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["state"] != "21.5" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestListStates(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `[{"entity_id":"a"},{"entity_id":"b"}]`, "application/json")
	p := haPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "list_states", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodGet || rec.path != "/states" {
		t.Fatalf("method/path: %s %s", rec.method, rec.path)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestSetState(t *testing.T) {
	srv, rec := newRecordingServer(t, 201, `{"entity_id":"sensor.custom","state":"42"}`, "application/json")
	p := haPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "set_state", Connection: testConn(srv),
		Options: map[string]any{
			"entity_id": "sensor.custom", "state": "42",
			"attributes": map[string]any{"unit": "widgets"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/states/sensor.custom" {
		t.Fatalf("method/path: %s %s", rec.method, rec.path)
	}
	if rec.body["state"] != "42" {
		t.Fatalf("body state: %#v", rec.body)
	}
	attrs, ok := rec.body["attributes"].(map[string]any)
	if !ok || attrs["unit"] != "widgets" {
		t.Fatalf("body attributes: %#v", rec.body)
	}
}

func TestFireEvent(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"message":"Event some_event fired."}`, "application/json")
	p := haPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "fire_event", Connection: testConn(srv),
		Options: map[string]any{"event_type": "some_event", "data": map[string]any{"k": "v"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/events/some_event" {
		t.Fatalf("method/path: %s %s", rec.method, rec.path)
	}
	if rec.body["k"] != "v" {
		t.Fatalf("body: %#v", rec.body)
	}
}

func TestRenderTemplate(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `it is 72 degrees`, "text/plain")
	p := haPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "render_template", Connection: testConn(srv),
		Options: map[string]any{"template": "it is {{ states('sensor.temp') }} degrees"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/template" {
		t.Fatalf("method/path: %s %s", rec.method, rec.path)
	}
	if rec.body["template"] != "it is {{ states('sensor.temp') }} degrees" {
		t.Fatalf("body: %#v", rec.body)
	}
	if res.Outputs["text"] != "it is 72 degrees" {
		t.Fatalf("text: %#v", res.Outputs["text"])
	}
}

func TestGetServices(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `[{"domain":"light","services":{"turn_on":{}}}]`, "application/json")
	p := haPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "get_services", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if rec.path != "/services" {
		t.Fatalf("path: %s", rec.path)
	}
	if items, ok := res.Outputs["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestGetConfig(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"location_name":"Home"}`, "application/json")
	p := haPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "get_config", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if rec.path != "/config" {
		t.Fatalf("path: %s", rec.path)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["location_name"] != "Home" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestHistory(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `[[{"state":"on"}]]`, "application/json")
	p := haPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "history", Connection: testConn(srv),
		Options: map[string]any{"entity_id": "light.kitchen", "start": "2024-01-01T00:00:00Z", "end": "2024-01-02T00:00:00Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.path != "/history/period/2024-01-01T00:00:00Z" {
		t.Fatalf("path: %s", rec.path)
	}
	q := rec.query
	if !contains(q, "filter_entity_id=light.kitchen") || !contains(q, "end_time=2024-01-02T00%3A00%3A00Z") {
		t.Fatalf("query: %s", q)
	}
	if items, ok := res.Outputs["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestLogbook(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `[{"name":"Kitchen light","message":"turned on"}]`, "application/json")
	p := haPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "logbook", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if rec.path != "/logbook" {
		t.Fatalf("path: %s", rec.path)
	}
	if items, ok := res.Outputs["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"ok":true}`, "application/json")
	p := haPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{
			"method": "GET", "path": "states/sensor.foo",
			"query": map[string]any{"a": "b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodGet || rec.path != "/states/sensor.foo" {
		t.Fatalf("method/path: %s %s", rec.method, rec.path)
	}
	if rec.query != "a=b" {
		t.Fatalf("query: %s", rec.query)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["ok"] != true {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestNonOKStatusIsInternalError(t *testing.T) {
	srv, _ := newRecordingServer(t, 401, `{"message":"invalid auth"}`, "application/json")
	p := haPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "get_state", Connection: testConn(srv),
		Options: map[string]any{"entity_id": "sensor.temp"},
	})
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	pe := assertCode(t, err, plugin.CodeInternalError)
	if !contains(pe.Message, "401") || !contains(pe.Message, "invalid auth") {
		t.Fatalf("error message should include status+body: %q", pe.Message)
	}
}

func TestUnknownVerb(t *testing.T) {
	p := haPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope"})
	if err == nil {
		t.Fatal("expected error")
	}
	assertCode(t, err, plugin.CodeInvalidParams)
}

func TestDescribe(t *testing.T) {
	d := haPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "homeassistant" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if d.Connection["base_url"].Required != true || d.Connection["token"].Required != true {
		t.Fatalf("base_url/token should be required: %#v", d.Connection)
	}
	want := []string{"call_service", "get_state", "list_states", "set_state", "fire_event",
		"render_template", "get_services", "get_config", "history", "logbook", "api"}
	got := map[string]bool{}
	for _, v := range d.Verbs {
		got[v.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Verbs) != len(want) {
		t.Errorf("verb count: got %d want %d", len(d.Verbs), len(want))
	}
	if len(d.Events) != 1 || d.Events[0].Name != "event" {
		t.Fatalf("events: %#v", d.Events)
	}
	if d.Capabilities.Egress == nil || len(d.Capabilities.Egress) != 0 {
		t.Fatalf("capabilities.egress should be an empty (non-nil) list: %#v", d.Capabilities.Egress)
	}
}

// --- webhook source tests ---

func TestBuildEvent(t *testing.T) {
	ev := buildEvent(map[string]any{
		"id": "abc123", "event_type": "doorbell", "friendly_name": "Front door",
	})
	if ev.Event != "event" {
		t.Fatalf("event: %q", ev.Event)
	}
	if ev.Kind != "doorbell" {
		t.Fatalf("kind: %q", ev.Kind)
	}
	if ev.Dedup != "abc123" {
		t.Fatalf("dedup: %q", ev.Dedup)
	}
	if ev.Context["friendly_name"] != "Front door" {
		t.Fatalf("context missing top-level key: %#v", ev.Context)
	}
	if ev.Context["event_types"] != "doorbell" {
		t.Fatalf("context missing event_types alias: %#v", ev.Context)
	}
	payload, ok := ev.Context["payload"].(map[string]any)
	if !ok || payload["id"] != "abc123" {
		t.Fatalf("context payload copy: %#v", ev.Context["payload"])
	}
}

func TestBuildEventNoIDNoEventType(t *testing.T) {
	ev := buildEvent(map[string]any{"foo": "bar"})
	if ev.Kind != "" || ev.Dedup != "" {
		t.Fatalf("expected empty kind/dedup, got %q/%q", ev.Kind, ev.Dedup)
	}
	if ev.Context["foo"] != "bar" {
		t.Fatalf("context: %#v", ev.Context)
	}
}

func TestVerifyToken(t *testing.T) {
	req := func(header, queryToken string) *sourcekit.Request {
		rq := &sourcekit.Request{Header: http.Header{}, Query: url.Values{}}
		if queryToken != "" {
			rq.Query.Set("token", queryToken)
		}
		if header != "" {
			rq.Header.Set("X-Conductor-Token", header)
		}
		return rq
	}
	if !verifyToken("", req("", "")) {
		t.Fatal("empty secret should always verify (only reached when allow_unsigned)")
	}
	if !verifyToken("s3cret", req("s3cret", "")) {
		t.Fatal("matching header should verify")
	}
	if !verifyToken("s3cret", req("", "s3cret")) {
		t.Fatal("matching query token should verify")
	}
	if verifyToken("s3cret", req("wrong", "")) {
		t.Fatal("mismatched header should NOT verify")
	}
	if verifyToken("s3cret", req("", "")) {
		t.Fatal("missing token should NOT verify when a secret is configured")
	}
	if verifyToken("s3cret", req("", "wrong")) {
		t.Fatal("mismatched query token should NOT verify")
	}
}

func TestRequireWebhookSecret(t *testing.T) {
	if err := requireWebhookSecret("", false); err == nil {
		t.Fatal("expected fail-closed error for empty secret without allow_unsigned")
	}
	if err := requireWebhookSecret("", true); err != nil {
		t.Fatalf("allow_unsigned should permit an empty secret: %v", err)
	}
	if err := requireWebhookSecret("s3cret", false); err != nil {
		t.Fatalf("a configured secret should never error: %v", err)
	}
}

// TestStartSourceWebhookEndToEnd drives the real HTTP listener StartSource
// spins up: an accepted, signed delivery produces exactly one emitted event;
// an unsigned delivery is silently dropped (the shared sourcekit.Listener
// always responds 202 once it has read the body — verifyToken's rejection
// just skips the emit) and produces none.
func TestStartSourceWebhookEndToEnd(t *testing.T) {
	events := make(chan map[string]any, 4)
	emit := func(payload any) error {
		b, _ := json.Marshal(payload)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		events <- m
		return nil
	}

	addr := "127.0.0.1:18099"
	cfg := map[string]any{"webhook": map[string]any{
		"listen": addr, "path": "/homeassistant", "secret": "s3cret",
	}}
	ctxDone := make(chan struct{})
	go func() {
		p := haPlugin{}
		_ = p.StartSource(testContext(ctxDone), plugin.StartSourceRequest{Instance: "ha", Config: cfg}, emit)
	}()
	defer close(ctxDone)

	url := "http://" + addr + "/homeassistant"
	waitForListener(t, url)

	// No token at all: the listener still accepts the delivery (202), but
	// verifyToken drops it before emit — no event follows.
	resp, err := http.Post(url, "application/json", jsonBody(`{"event_type":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("unsigned request: want 202, got %d", resp.StatusCode)
	}
	select {
	case ev := <-events:
		t.Fatalf("unsigned request should not emit, got %#v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	// Accepted: header token.
	req, _ := http.NewRequest(http.MethodPost, url, jsonBody(`{"id":"1","event_type":"doorbell"}`))
	req.Header.Set("X-Conductor-Token", "s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("signed request: want 202, got %d", resp.StatusCode)
	}

	select {
	case ev := <-events:
		if ev["event"] != "event" || ev["kind"] != "doorbell" {
			t.Fatalf("emitted event: %#v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for emitted event")
	}

	select {
	case ev := <-events:
		t.Fatalf("unexpected second event: %#v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestStartSourceNoListenAddr(t *testing.T) {
	p := haPlugin{}
	err := p.StartSource(testContext(nil), plugin.StartSourceRequest{Instance: "ha"}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected error when webhook.listen is not configured")
	}
}

func TestStartSourceFailsClosedWithoutSecret(t *testing.T) {
	p := haPlugin{}
	err := p.StartSource(testContext(nil), plugin.StartSourceRequest{
		Instance: "ha",
		Config:   map[string]any{"webhook": map[string]any{"listen": "127.0.0.1:0"}},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected fail-closed error when no secret and no allow_unsigned")
	}
}

// --- test helpers ---

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (sub == "" || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func assertCode(t *testing.T, err error, code int) *plugin.Error {
	t.Helper()
	pe, ok := err.(*plugin.Error)
	if !ok {
		t.Fatalf("expected *plugin.Error, got %T (%v)", err, err)
	}
	if pe.Code != code {
		t.Fatalf("code: got %d want %d (%s)", pe.Code, code, pe.Message)
	}
	return pe
}

func jsonBody(s string) io.Reader { return strings.NewReader(s) }

func testContext(done chan struct{}) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if done != nil {
			<-done
		}
		cancel()
	}()
	return ctx
}

func waitForListener(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Post(url, "application/json", jsonBody(`{}`)); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("listener never came up")
}
