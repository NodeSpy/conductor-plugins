package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// TestDescribe asserts the declared surface: kind, type, capabilities, verbs,
// and the source event.
func TestDescribe(t *testing.T) {
	d := iftttPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "ifttt" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "maker.ifttt.com:443" {
		t.Fatalf("capabilities: %#v", d.Capabilities)
	}
	verbs := map[string]bool{}
	for _, v := range d.Verbs {
		verbs[v.Name] = true
	}
	for _, want := range []string{"trigger", "trigger_json"} {
		if !verbs[want] {
			t.Errorf("Describe missing verb %q", want)
		}
	}
	if len(d.Events) != 1 || d.Events[0].Name != "event" {
		t.Fatalf("events: %#v", d.Events)
	}
}

// TestInvokeTrigger drives the classic value1/value2/value3 shape against an
// httptest.Server, proving the path, method, and omit-empty body.
func TestInvokeTrigger(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("congratulations"))
	}))
	defer srv.Close()

	p := iftttPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "trigger",
		Connection: map[string]any{"key": "abc123", "base_url": srv.URL},
		Options: map[string]any{
			"event": "my_event", "value1": "one", "value2": "",
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	wantPath := "/trigger/my_event/with/key/abc123"
	if gotPath != wantPath {
		t.Errorf("path: got %q want %q", gotPath, wantPath)
	}
	if gotBody["value1"] != "one" {
		t.Errorf("body value1: %#v", gotBody)
	}
	if _, present := gotBody["value2"]; present {
		t.Errorf("body value2 should be omitted (empty): %#v", gotBody)
	}
	if _, present := gotBody["value3"]; present {
		t.Errorf("body value3 should be omitted (absent): %#v", gotBody)
	}
	if res.Outputs["result"] != "congratulations" {
		t.Errorf("result: %#v", res.Outputs["result"])
	}
	if res.Outputs["status_code"] != http.StatusOK {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

// TestInvokeTriggerJSON drives trigger_json, proving the /json/ path and that
// the data map goes straight through as the request body.
func TestInvokeTriggerJSON(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("congratulations"))
	}))
	defer srv.Close()

	p := iftttPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "trigger_json",
		Connection: map[string]any{"key": "abc123", "base_url": srv.URL},
		Options: map[string]any{
			"event": "my_event",
			"data":  map[string]any{"foo": "bar", "n": float64(3)},
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	wantPath := "/trigger/my_event/json/with/key/abc123"
	if gotPath != wantPath {
		t.Errorf("path: got %q want %q", gotPath, wantPath)
	}
	if gotBody["foo"] != "bar" || gotBody["n"] != float64(3) {
		t.Errorf("body: %#v", gotBody)
	}
	if res.Outputs["status_code"] != http.StatusOK {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

// TestInvokeTriggerJSONRequiresData proves data is required and non-empty.
func TestInvokeTriggerJSONRequiresData(t *testing.T) {
	p := iftttPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "trigger_json",
		Connection: map[string]any{"key": "abc123"},
		Options:    map[string]any{"event": "my_event"},
	})
	if err == nil {
		t.Fatal("expected error for missing data")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams, got %v", err)
	}
}

// TestInvokeRequiresKey proves connection.key is required.
func TestInvokeRequiresKey(t *testing.T) {
	p := iftttPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:    "trigger",
		Options: map[string]any{"event": "my_event"},
	})
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

// TestInvokeNon2xx proves a non-2xx response becomes a CodeInternalError
// carrying the status and body.
func TestInvokeNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"message":"You must give a value for at least one ingredient"}]}`))
	}))
	defer srv.Close()

	p := iftttPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "trigger",
		Connection: map[string]any{"key": "abc123", "base_url": srv.URL},
		Options:    map[string]any{"event": "my_event"},
	})
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "400") || !strings.Contains(pe.Message, "at least one ingredient") {
		t.Fatalf("error message missing status/body: %q", pe.Message)
	}
}

// TestParseInboundJSON proves the JSON path: top-level keys spread into
// context, a payload key holding the whole object, and id-based dedup.
func TestParseInboundJSON(t *testing.T) {
	ev := parseInbound([]byte(`{"event":"door_opened","id":"42","room":"kitchen"}`))
	if ev.Event != "event" {
		t.Fatalf("event: %q", ev.Event)
	}
	if ev.Dedup != "42" {
		t.Fatalf("dedup: %q", ev.Dedup)
	}
	if ev.Context["room"] != "kitchen" {
		t.Fatalf("context room: %#v", ev.Context["room"])
	}
	if ev.Context["events"] != "door_opened" {
		t.Fatalf("context events alias: %#v", ev.Context["events"])
	}
	payload, ok := ev.Context["payload"].(map[string]any)
	if !ok || payload["room"] != "kitchen" {
		t.Fatalf("context payload: %#v", ev.Context["payload"])
	}
	if !strings.Contains(ev.Title, "door_opened") {
		t.Fatalf("title: %q", ev.Title)
	}
}

// TestParseInboundNumericID proves a numeric `id` is rendered as a dedup key.
func TestParseInboundNumericID(t *testing.T) {
	ev := parseInbound([]byte(`{"id":42}`))
	if ev.Dedup != "42" {
		t.Fatalf("dedup: %q", ev.Dedup)
	}
}

// TestParseInboundNonJSON proves a non-JSON body is carried as `body` and gets
// no dedup key.
func TestParseInboundNonJSON(t *testing.T) {
	ev := parseInbound([]byte(`plain text, not json`))
	if ev.Context["body"] != "plain text, not json" {
		t.Fatalf("context body: %#v", ev.Context["body"])
	}
	if ev.Dedup != "" {
		t.Fatalf("dedup should be empty for non-JSON body, got %q", ev.Dedup)
	}
}

// TestRequireWebhookSecret covers the fail-closed default and the explicit
// allow_unsigned opt-out.
func TestRequireWebhookSecret(t *testing.T) {
	if err := requireWebhookSecret("ifttt", "", false); err == nil {
		t.Fatal("expected error for missing secret")
	}
	if err := requireWebhookSecret("ifttt", "", true); err != nil {
		t.Fatalf("allow_unsigned should permit no secret: %v", err)
	}
	if err := requireWebhookSecret("ifttt", "tok", false); err != nil {
		t.Fatalf("a set secret should never error: %v", err)
	}
}

// TestCheckToken covers header, query-param fallback, mismatch, and the
// allow_unsigned bypass.
func TestCheckToken(t *testing.T) {
	mkReq := func(headerTok, queryTok string) *sourcekit.Request {
		r := &sourcekit.Request{Header: http.Header{}, Query: url.Values{}}
		if queryTok != "" {
			r.Query.Set("token", queryTok)
		}
		if headerTok != "" {
			r.Header.Set("X-Conductor-Token", headerTok)
		}
		return r
	}

	cases := []struct {
		name          string
		headerTok     string
		queryTok      string
		secret        string
		allowUnsigned bool
		want          bool
	}{
		{"header match", "s3cret", "", "s3cret", false, true},
		{"header mismatch", "wrong", "", "s3cret", false, false},
		{"query fallback match", "", "s3cret", "s3cret", false, true},
		{"query fallback mismatch", "", "wrong", "s3cret", false, false},
		{"header wins over query", "s3cret", "wrong", "s3cret", false, true},
		{"no token at all", "", "", "s3cret", false, false},
		{"allow_unsigned with empty secret", "", "", "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkToken(mkReq(tc.headerTok, tc.queryTok), tc.secret, tc.allowUnsigned)
			if got != tc.want {
				t.Errorf("checkToken: got %v want %v", got, tc.want)
			}
		})
	}
}

// TestStartSourceWebhookAcceptReject drives the real listener end-to-end: an
// accepted delivery (correct token) emits an event, a rejected one (bad or
// missing token) never reaches emit. Never touches the real IFTTT network —
// it only serves an inbound HTTP listener on loopback.
func TestStartSourceWebhookAcceptReject(t *testing.T) {
	addr := "127.0.0.1:18173"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var events []any
	emit := func(payload any) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, payload)
		return nil
	}

	p := iftttPlugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.StartSource(ctx, plugin.StartSourceRequest{
			Instance: "test",
			Config: map[string]any{
				"webhook": map[string]any{
					"listen": addr,
					"path":   "/ifttt",
					"secret": "s3cret",
				},
			},
		}, emit)
	}()

	reqURL := "http://" + addr + "/ifttt"
	if !waitUp(reqURL) {
		t.Fatal("listener never came up")
	}

	// Rejected: no token.
	resp, err := http.Post(reqURL, "application/json", strings.NewReader(`{"event":"e1","id":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Accepted: correct header token.
	req, _ := http.NewRequest(http.MethodPost, reqURL, strings.NewReader(`{"event":"e2","id":"2"}`))
	req.Header.Set("X-Conductor-Token", "s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid-token request: got %d want 202", resp.StatusCode)
	}

	// Accepted via query param.
	resp, err = http.Post(reqURL+"?token=s3cret", "application/json", strings.NewReader(`{"event":"e3","id":"3"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("query-token request: got %d want 202", resp.StatusCode)
	}

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected 2 emitted events, got %d: %#v", len(events), events)
	}
}

func waitUp(url string) bool {
	for i := 0; i < 50; i++ {
		// A GET against a POST-only handler still proves the listener is up
		// (405, not connection-refused).
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}
