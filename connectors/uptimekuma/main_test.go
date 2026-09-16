package main

import (
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// downPayload is a captured Uptime Kuma "DOWN" webhook notification.
const downPayload = `{
  "heartbeat": {"monitorID": 7, "status": 0, "time": "2026-09-16 12:00:00", "msg": "Timeout", "important": true, "duration": 60},
  "monitor": {"id": 7, "name": "API", "url": "https://api.example.com/health", "hostname": "", "type": "http"},
  "msg": "[API] [Down] Timeout"
}`

// upPayload is a captured Uptime Kuma "UP" webhook notification (recovery).
const upPayload = `{
  "heartbeat": {"monitorID": 7, "status": 1, "time": "2026-09-16 12:05:00", "msg": "200 OK", "important": true, "duration": 60},
  "monitor": {"id": 7, "name": "API", "url": "https://api.example.com/health", "hostname": "", "type": "http"},
  "msg": "[API] [Up] 200 OK"
}`

func TestParseDown(t *testing.T) {
	f, ok := parse([]byte(downPayload))
	if !ok {
		t.Fatal("parse: expected ok")
	}
	if f.status != "down" {
		t.Errorf("status: got %q want down", f.status)
	}
	if f.monitorID != "7" {
		t.Errorf("monitorID: got %q want 7", f.monitorID)
	}
	if f.monitorName != "API" {
		t.Errorf("monitorName: got %q want API", f.monitorName)
	}
	if f.monitorURL != "https://api.example.com/health" {
		t.Errorf("monitorURL: got %q", f.monitorURL)
	}
	if f.monitorType != "http" {
		t.Errorf("monitorType: got %q want http", f.monitorType)
	}
	if !f.important {
		t.Error("important: got false want true")
	}
	if f.time != "2026-09-16 12:00:00" {
		t.Errorf("time: got %q", f.time)
	}
	if f.msg != "[API] [Down] Timeout" {
		t.Errorf("msg: got %q", f.msg)
	}

	dk := dedupKey(f)
	ev := monitorEvent(f, dk)
	if ev["event"] != "monitor" {
		t.Errorf("event: got %v", ev["event"])
	}
	if ev["title"] != "[API] [Down] Timeout" {
		t.Errorf("title: got %v", ev["title"])
	}
	ctx, ok := ev["context"].(map[string]any)
	if !ok {
		t.Fatal("context: not a map")
	}
	wantCtx := map[string]any{
		"status": "down", "monitor_name": "API", "monitor_url": "https://api.example.com/health",
		"monitor_type": "http", "monitor_id": "7", "msg": "[API] [Down] Timeout",
		"important": true, "time": "2026-09-16 12:00:00",
		"statuses": "down", "monitors": "API", "monitor_types": "http", "monitor": "API",
	}
	for k, want := range wantCtx {
		if got := ctx[k]; got != want {
			t.Errorf("context[%q]: got %v want %v", k, got, want)
		}
	}
}

func TestParseUp(t *testing.T) {
	f, ok := parse([]byte(upPayload))
	if !ok {
		t.Fatal("parse: expected ok")
	}
	if f.status != "up" {
		t.Errorf("status: got %q want up", f.status)
	}
	dk := dedupKey(f)
	if dk == dedupKey(mustParse(t, downPayload)) {
		t.Error("dedup key collided between down and up events")
	}
}

// TestParseStatusMapping proves every documented status code maps correctly.
func TestParseStatusMapping(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, "down"}, {1, "up"}, {2, "pending"}, {3, "maintenance"}, {9, "unknown"},
	}
	for _, tc := range cases {
		body := []byte(`{"heartbeat":{"monitorID":1,"status":` + strconv.Itoa(tc.code) + `,"time":"t"},"monitor":{"id":1,"name":"m"}}`)
		f, ok := parse(body)
		if !ok {
			t.Fatalf("status %d: parse not ok", tc.code)
		}
		if f.status != tc.want {
			t.Errorf("status %d: got %q want %q", tc.code, f.status, tc.want)
		}
	}
}

// TestParseFlatFallback covers Uptime Kuma's alternative payload shape: only
// a flat {"msg": ...} with no nested heartbeat/monitor object.
func TestParseFlatFallback(t *testing.T) {
	f, ok := parse([]byte(`{"msg": "[Site] [Down] something broke"}`))
	if !ok {
		t.Fatal("parse: expected ok for flat payload")
	}
	if f.msg != "[Site] [Down] something broke" {
		t.Errorf("msg: got %q", f.msg)
	}
	if f.status != "" || f.monitorName != "" || f.monitorID != "" {
		t.Errorf("expected empty structured fields, got %+v", f)
	}
	ev := monitorEvent(f, dedupKey(f))
	if ev["title"] != "[Site] [Down] something broke" {
		t.Errorf("title: got %v", ev["title"])
	}
}

// TestParseTitleFallback proves the title falls back to "<monitor> is
// <status>" when msg is empty.
func TestParseTitleFallback(t *testing.T) {
	body := []byte(`{"heartbeat":{"monitorID":2,"status":0,"time":"t"},"monitor":{"id":2,"name":"DB"}}`)
	f, ok := parse(body)
	if !ok {
		t.Fatal("parse: expected ok")
	}
	ev := monitorEvent(f, dedupKey(f))
	if ev["title"] != "DB is down" {
		t.Errorf("title: got %v want %q", ev["title"], "DB is down")
	}
}

// TestParseEmpty proves a body with no usable signal is rejected.
func TestParseEmpty(t *testing.T) {
	if _, ok := parse([]byte(`{}`)); ok {
		t.Error("parse({}): expected not ok")
	}
	if _, ok := parse([]byte(`not json`)); ok {
		t.Error("parse(invalid json): expected not ok")
	}
}

// TestVerifyToken covers both the header and ?token= query forms, now that
// StartSource uses sourcekit.Listener.ServeReq (full request access) instead
// of the header-only Serve.
func TestVerifyToken(t *testing.T) {
	mk := func(header, query string) *sourcekit.Request {
		r := &sourcekit.Request{Header: http.Header{}, Query: url.Values{}}
		if header != "" {
			r.Header.Set("X-Conductor-Token", header)
		}
		if query != "" {
			r.Query.Set("token", query)
		}
		return r
	}

	if !verifyToken("s3cret", mk("s3cret", "")) {
		t.Error("header: expected matching token to verify")
	}
	if verifyToken("s3cret", mk("wrong", "")) {
		t.Error("header: expected mismatched token to fail")
	}
	if !verifyToken("s3cret", mk("", "s3cret")) {
		t.Error("query: expected matching token to verify")
	}
	if verifyToken("s3cret", mk("", "wrong")) {
		t.Error("query: expected mismatched token to fail")
	}
	if !verifyToken("s3cret", mk("s3cret", "wrong")) {
		t.Error("header should win over a mismatched query token")
	}
	if verifyToken("s3cret", mk("", "")) {
		t.Error("no token at all: expected failure")
	}
}

// TestInvokeSourceOnly proves the connector refuses every verb call.
func TestInvokeSourceOnly(t *testing.T) {
	_, err := uptimekuma{}.Invoke(plugin.InvokeRequest{Verb: "anything"})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams plugin.Error, got %v", err)
	}
}

// TestDescribe asserts the declared surface: kind, capabilities, connection,
// and the single source-only event.
func TestDescribe(t *testing.T) {
	d := uptimekuma{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "uptimekuma" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Verbs) != 0 {
		t.Fatalf("expected no verbs, got %v", d.Verbs)
	}
	if !reflect.DeepEqual(d.Capabilities, plugin.Capabilities{}) {
		t.Fatalf("expected empty capabilities, got %#v", d.Capabilities)
	}
	if len(d.Events) != 1 || d.Events[0].Name != "monitor" {
		t.Fatalf("expected single 'monitor' event, got %#v", d.Events)
	}
	for _, key := range []string{"listen", "path", "secret", "allow_unsigned", "smee"} {
		if _, ok := d.Connection[key]; !ok {
			t.Errorf("connection missing key %q", key)
		}
	}
	for _, key := range []string{"status", "monitor_name", "monitor_url", "monitor_type", "monitor_id", "msg", "important", "time"} {
		if _, ok := d.Events[0].Context[key]; !ok {
			t.Errorf("context missing key %q", key)
		}
	}
	for _, key := range []string{"statuses", "monitors", "monitor_types", "status", "monitor", "monitor_type"} {
		if _, ok := d.Events[0].Filters[key]; !ok {
			t.Errorf("filters missing key %q", key)
		}
	}
}

func mustParse(t *testing.T, body string) facts {
	t.Helper()
	f, ok := parse([]byte(body))
	if !ok {
		t.Fatalf("parse(%s): not ok", body)
	}
	return f
}
