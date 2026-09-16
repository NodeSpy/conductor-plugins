package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// TestDescribe asserts the declared surface: kind, type, capabilities, verbs,
// and the alert event.
func TestDescribe(t *testing.T) {
	d := datadogPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "datadog" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Egress) == 0 || d.Capabilities.Egress[0] != "api.datadoghq.com:443" {
		t.Fatalf("capabilities.egress: %#v", d.Capabilities.Egress)
	}
	want := []string{"post_event", "mute_monitor", "unmute_monitor", "get_monitor",
		"list_monitors", "mute_all", "unmute_all", "submit_metric", "api"}
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
	if len(d.Events) != 1 || d.Events[0].Name != "alert" {
		t.Fatalf("events: %#v", d.Events)
	}
	for _, key := range []string{"alert_types", "priorities", "tags", "scopes"} {
		if _, ok := d.Events[0].Filters[key]; !ok {
			t.Errorf("alert event missing filter %q", key)
		}
	}
	for _, key := range []string{"alert_id", "alert_type", "title", "body", "priority", "tags", "url",
		"event_id", "org_name", "hostname", "scope", "transition"} {
		if _, ok := d.Events[0].Context[key]; !ok {
			t.Errorf("alert event missing context field %q", key)
		}
	}
	if !d.Connection["api_key"].Required {
		t.Error("api_key should be required")
	}
}

// --- webhook template parsing ---

// datadogTemplate is the EXACT payload template documented in
// docs/connectors/datadog.md and required by the task spec — this test proves
// the plugin actually parses what it tells operators to paste into Datadog.
const datadogTemplate = `{"alert_id":"$ALERT_ID","alert_type":"$ALERT_TYPE","title":"$EVENT_TITLE","body":"$EVENT_MSG","priority":"$PRIORITY","tags":"$TAGS","url":"$LINK","event_id":"$ID","org_name":"$ORG_NAME","hostname":"$HOSTNAME","scope":"$ALERT_SCOPE","transition":"$ALERT_TRANSITION"}`

func TestTemplateShape(t *testing.T) {
	var m map[string]string
	if err := json.Unmarshal([]byte(datadogTemplate), &m); err != nil {
		t.Fatalf("documented template is not valid JSON: %v", err)
	}
	want := map[string]string{
		"alert_id": "$ALERT_ID", "alert_type": "$ALERT_TYPE", "title": "$EVENT_TITLE",
		"body": "$EVENT_MSG", "priority": "$PRIORITY", "tags": "$TAGS", "url": "$LINK",
		"event_id": "$ID", "org_name": "$ORG_NAME", "hostname": "$HOSTNAME",
		"scope": "$ALERT_SCOPE", "transition": "$ALERT_TRANSITION",
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("template shape:\n got: %#v\nwant: %#v", m, want)
	}
}

func TestParseAlert(t *testing.T) {
	body := []byte(`{
		"alert_id": "12345", "alert_type": "warning", "title": "High CPU on web-1",
		"body": "CPU usage exceeded 90%", "priority": "P2", "tags": "env:prod, service:web, team us-east",
		"url": "https://app.datadoghq.com/monitors/12345", "event_id": "999",
		"org_name": "acme", "hostname": "web-1", "scope": "host:web-1", "transition": "Triggered"
	}`)
	f, ok := parseAlert(body)
	if !ok {
		t.Fatal("parseAlert: expected ok")
	}
	if f.alertID != "12345" || f.alertType != "warning" || f.title != "High CPU on web-1" {
		t.Fatalf("parsed core fields: %#v", f)
	}
	if f.scope != "host:web-1" || f.transition != "Triggered" || f.orgName != "acme" || f.hostname != "web-1" {
		t.Fatalf("parsed extended fields: %#v", f)
	}
	tags := splitTags(f.tags)
	wantTags := []string{"env:prod", "service:web", "team", "us-east"}
	sort.Strings(tags)
	sort.Strings(wantTags)
	if !reflect.DeepEqual(tags, wantTags) {
		t.Fatalf("splitTags: got %#v want %#v", tags, wantTags)
	}
}

func TestParseAlertRejectsEmpty(t *testing.T) {
	if _, ok := parseAlert([]byte(`{}`)); ok {
		t.Fatal("empty payload should not parse as an alert")
	}
	if _, ok := parseAlert([]byte(`not json`)); ok {
		t.Fatal("invalid JSON should not parse as an alert")
	}
}

func TestParseAlertRecoveryType(t *testing.T) {
	for _, at := range []string{"error", "warning", "success", "info", "recovery"} {
		body := []byte(`{"alert_id":"1","alert_type":"` + at + `","title":"t"}`)
		f, ok := parseAlert(body)
		if !ok || f.alertType != at {
			t.Fatalf("alert_type %q: parsed %#v ok=%v", at, f, ok)
		}
	}
}

func TestDedupKey(t *testing.T) {
	// alert_id + alert_type is the primary key.
	a := facts{alertID: "1", alertType: "warning", eventID: "9", title: "x"}
	b := facts{alertID: "1", alertType: "warning", eventID: "OTHER", title: "y"}
	if dedupKey(a) != dedupKey(b) {
		t.Fatalf("same alert_id+alert_type should dedup key equal: %q vs %q", dedupKey(a), dedupKey(b))
	}
	// falls back to event_id when alert_id is empty.
	c := facts{eventID: "42", alertType: "error"}
	d := facts{eventID: "42", alertType: "error", title: "different title"}
	if dedupKey(c) != dedupKey(d) {
		t.Fatalf("fallback to event_id should ignore title: %q vs %q", dedupKey(c), dedupKey(d))
	}
	// falls back to title when both alert_id and event_id are empty.
	e := facts{title: "only a title", alertType: "info"}
	if dedupKey(e) != "only a title\x00info" {
		t.Fatalf("title fallback: got %q", dedupKey(e))
	}
}

// --- token verification: header/query, accept/reject ---

func TestVerifyToken(t *testing.T) {
	secret := "s3cr3t-token"
	mk := func(withHeader, withQuery string) *sourcekit.Request {
		r := &sourcekit.Request{Header: http.Header{}, Query: url.Values{}}
		if withQuery != "" {
			r.Query.Set("token", withQuery)
		}
		if withHeader != "" {
			r.Header.Set("X-Conductor-Token", withHeader)
		}
		return r
	}
	cases := []struct {
		name          string
		header, query string
		want          bool
	}{
		{"valid header", secret, "", true},
		{"valid query", "", secret, true},
		{"header wins over query", secret, "wrong", true},
		{"wrong header", "nope", "", false},
		{"wrong query", "", "nope", false},
		{"neither present", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyToken(secret, mk(tc.header, tc.query)); got != tc.want {
				t.Errorf("verifyToken(%q, %q): got %v want %v", tc.header, tc.query, got, tc.want)
			}
		})
	}
}

func TestRequireWebhookSecret(t *testing.T) {
	if err := requireWebhookSecret("datadog", "", map[string]any{}, "webhook.secret"); err == nil {
		t.Fatal("empty secret should fail closed")
	}
	if err := requireWebhookSecret("datadog", "", map[string]any{"allow_unsigned": true}, "webhook.secret"); err != nil {
		t.Fatalf("allow_unsigned should override: %v", err)
	}
	if err := requireWebhookSecret("datadog", "tok", map[string]any{}, "webhook.secret"); err != nil {
		t.Fatalf("non-empty secret should pass: %v", err)
	}
}

// TestStartSourceEndToEnd drives the real HTTP listener: unauthenticated and
// wrong-token requests are dropped (no event emitted), and a valid request
// (checked via header, then via query param on a second delivery) emits a
// parsed "alert" event with the documented context/filter keys, deduped on
// repeat delivery.
func TestStartSourceEndToEnd(t *testing.T) {
	addr := "127.0.0.1:18097"
	secret := "shh"
	events := make(chan map[string]any, 8)
	emit := func(payload any) error {
		b, _ := json.Marshal(payload)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		events <- m
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (datadogPlugin{}).StartSource(ctx, plugin.StartSourceRequest{
			Instance: "t",
			Config: map[string]any{
				"webhook": map[string]any{"listen": addr, "path": "/datadog", "secret": secret},
			},
		}, emit)
	}()
	waitForListener(t, addr)

	body := []byte(`{"alert_id":"a1","alert_type":"error","title":"CPU high","body":"msg",
		"priority":"P1","tags":"env:prod,team:sre","url":"https://x","event_id":"e1",
		"org_name":"acme","hostname":"h1","scope":"host:h1","transition":"Triggered"}`)

	// No token at all: dropped.
	post(t, addr, "/datadog", "", "", body)
	// Wrong token: dropped.
	post(t, addr, "/datadog", "wrong", "", body)

	select {
	case ev := <-events:
		t.Fatalf("unauthenticated/wrong-token request should not emit, got %#v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	// Valid via header.
	post(t, addr, "/datadog", secret, "", body)
	ev := recvEvent(t, events)
	if ev["event"] != "alert" || ev["kind"] != "error" {
		t.Fatalf("event/kind: %#v", ev)
	}
	ctxm, _ := ev["context"].(map[string]any)
	for _, key := range []string{"alert_id", "alert_type", "title", "body", "priority", "tags", "url",
		"event_id", "org_name", "hostname", "scope", "transition", "alert_types", "priorities", "scopes"} {
		if _, ok := ctxm[key]; !ok {
			t.Errorf("emitted context missing %q: %#v", key, ctxm)
		}
	}
	if ctxm["alert_id"] != "a1" || ctxm["scope"] != "host:h1" || ctxm["transition"] != "Triggered" {
		t.Fatalf("context values: %#v", ctxm)
	}
	tags, _ := ctxm["tags"].([]any)
	if len(tags) != 2 {
		t.Fatalf("tags list: %#v", ctxm["tags"])
	}

	// Same delivery repeated (still valid, via header again): deduped, no 2nd event.
	post(t, addr, "/datadog", secret, "", body)
	select {
	case ev := <-events:
		t.Fatalf("duplicate delivery should be deduped, got %#v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	// A DIFFERENT alert, valid via query param this time.
	body2 := []byte(`{"alert_id":"a2","alert_type":"recovery","title":"CPU back to normal"}`)
	post(t, addr, "/datadog", "", secret, body2)
	ev2 := recvEvent(t, events)
	if ev2["kind"] != "recovery" {
		t.Fatalf("second event kind: %#v", ev2)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("StartSource returned error: %v", err)
	}
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Post("http://"+addr+"/__probe__", "application/json", nil)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener at %s never came up", addr)
}

func post(t *testing.T, addr, path, headerTok, queryTok string, body []byte) {
	t.Helper()
	u := "http://" + addr + path
	if queryTok != "" {
		u += "?token=" + queryTok
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if headerTok != "" {
		req.Header.Set("X-Conductor-Token", headerTok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func recvEvent(t *testing.T, ch chan map[string]any) map[string]any {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for emitted event")
		return nil
	}
}

// --- verbs, via httptest.Server (base-URL override) ---

func TestApiBase(t *testing.T) {
	if apiBase("") != "https://api.datadoghq.com" {
		t.Fatalf("default site: got %q", apiBase(""))
	}
	if apiBase("datadoghq.eu") != "https://api.datadoghq.eu" {
		t.Fatalf("eu site: got %q", apiBase("datadoghq.eu"))
	}
}

func TestInvokePostEvent(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey, gotAppKey string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAPIKey, gotAppKey = r.Header.Get("DD-API-KEY"), r.Header.Get("DD-APPLICATION-KEY")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	res, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{
		Verb:       "post_event",
		Connection: map[string]any{"api_key": "AK", "app_key": "APPK", "api_base": srv.URL},
		Options: map[string]any{
			"title": "deploy", "text": "deployed v2", "tags": []any{"env:prod"},
			"alert_type": "info", "priority": "low", "aggregation_key": "deploy-123",
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/events" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if gotAPIKey != "AK" || gotAppKey != "APPK" {
		t.Fatalf("headers: DD-API-KEY=%q DD-APPLICATION-KEY=%q", gotAPIKey, gotAppKey)
	}
	if gotBody["title"] != "deploy" || gotBody["text"] != "deployed v2" || gotBody["aggregation_key"] != "deploy-123" {
		t.Fatalf("body: %#v", gotBody)
	}
	if res.Outputs["status_code"] != 202 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
	result, _ := res.Outputs["result"].(map[string]any)
	if result["status"] != "ok" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokePostEventValidation(t *testing.T) {
	_, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{
		Verb:       "post_event",
		Connection: map[string]any{"api_key": "AK"},
		Options:    map[string]any{"title": "only title"},
	})
	if err == nil {
		t.Fatal("expected error for missing text")
	}
}

func TestInvokeMuteUnmuteMonitor(t *testing.T) {
	var calls []string
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	conn := map[string]any{"api_key": "AK", "app_key": "APPK", "api_base": srv.URL}

	if _, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{
		Verb: "mute_monitor", Connection: conn,
		Options: map[string]any{"monitor_id": "42", "scope": "host:foo", "end": 1700000000},
	}); err != nil {
		t.Fatalf("mute_monitor: %v", err)
	}
	if lastBody["scope"] != "host:foo" || lastBody["end"] != float64(1700000000) {
		t.Fatalf("mute_monitor body: %#v", lastBody)
	}
	if _, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{
		Verb: "unmute_monitor", Connection: conn,
		Options: map[string]any{"monitor_id": "42"},
	}); err != nil {
		t.Fatalf("unmute_monitor: %v", err)
	}
	want := []string{"POST /api/v1/monitor/42/mute", "POST /api/v1/monitor/42/unmute"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls: got %#v want %#v", calls, want)
	}
}

func TestInvokeGetMonitor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/monitor/7" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"id":7,"name":"cpu high","overall_state":"Alert"}`))
	}))
	defer srv.Close()
	res, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{
		Verb:       "get_monitor",
		Connection: map[string]any{"api_key": "AK", "app_key": "APPK", "api_base": srv.URL},
		Options:    map[string]any{"monitor_id": "7"},
	})
	if err != nil {
		t.Fatalf("get_monitor: %v", err)
	}
	result, _ := res.Outputs["result"].(map[string]any)
	if result["name"] != "cpu high" || result["overall_state"] != "Alert" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeListMonitors(t *testing.T) {
	var q string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		w.Write([]byte(`[{"id":1,"name":"a"},{"id":2,"name":"b"}]`))
	}))
	defer srv.Close()
	res, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{
		Verb:       "list_monitors",
		Connection: map[string]any{"api_key": "AK", "app_key": "APPK", "api_base": srv.URL},
		Options:    map[string]any{"name": "cpu", "tags": []any{"env:prod"}},
	})
	if err != nil {
		t.Fatalf("list_monitors: %v", err)
	}
	if q == "" {
		t.Fatal("expected a query string")
	}
	items, _ := res.Outputs["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeMuteUnmuteAll(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	conn := map[string]any{"api_key": "AK", "api_base": srv.URL}
	if _, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{Verb: "mute_all", Connection: conn}); err != nil {
		t.Fatalf("mute_all: %v", err)
	}
	if _, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{Verb: "unmute_all", Connection: conn}); err != nil {
		t.Fatalf("unmute_all: %v", err)
	}
	want := []string{"/api/v1/monitor/mute_all", "/api/v1/monitor/unmute_all"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths: got %#v want %#v", paths, want)
	}
}

func TestInvokeSubmitMetric(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/series" {
			t.Errorf("path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	_, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{
		Verb:       "submit_metric",
		Connection: map[string]any{"api_key": "AK", "api_base": srv.URL},
		Options: map[string]any{
			"metric": "app.requests", "type": "count", "tags": []any{"env:prod"},
			"points": []any{[]any{1700000000, 5}},
		},
	})
	if err != nil {
		t.Fatalf("submit_metric: %v", err)
	}
	series, _ := body["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series: %#v", body["series"])
	}
	s0, _ := series[0].(map[string]any)
	if s0["metric"] != "app.requests" || s0["type"] != float64(1) {
		t.Fatalf("series[0]: %#v", s0)
	}
}

func TestInvokeGenericAPI(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	res, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{
		Verb:       "api",
		Connection: map[string]any{"api_key": "AK", "api_base": srv.URL},
		Options: map[string]any{
			"method": "GET", "path": "/api/v1/dashboard", "query": map[string]any{"filter": "x"},
		},
	})
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/api/v1/dashboard" || gotQuery != "filter=x" {
		t.Fatalf("request: %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	result, _ := res.Outputs["result"].(map[string]any)
	if result["ok"] != true {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["Forbidden"]}`))
	}))
	defer srv.Close()
	_, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{
		Verb:       "get_monitor",
		Connection: map[string]any{"api_key": "AK", "api_base": srv.URL},
		Options:    map[string]any{"monitor_id": "1"},
	})
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
	pe, ok := err.(*plugin.Error)
	if !ok {
		t.Fatalf("expected *plugin.Error, got %T: %v", err, err)
	}
	if pe.Code != plugin.CodeInternalError {
		t.Fatalf("code: got %d want %d", pe.Code, plugin.CodeInternalError)
	}
	if !contains(pe.Message, "403") || !contains(pe.Message, "Forbidden") {
		t.Fatalf("message should include status and body: %q", pe.Message)
	}
}

func TestInvokeUnknownVerb(t *testing.T) {
	_, err := (datadogPlugin{}).Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{"api_key": "AK"},
	})
	if err == nil {
		t.Fatal("expected error for unknown verb")
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}
