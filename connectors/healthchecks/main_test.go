package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestDescribe asserts the declared surface: kind, type, capabilities, verbs,
// and the source event.
func TestDescribe(t *testing.T) {
	d := (&healthchecksPlugin{}).Describe()
	if d.Kind != plugin.KindConnector || d.Type != "healthchecks" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	wantEgress := map[string]bool{"healthchecks.io:443": true, "hc-ping.com:443": true}
	if len(d.Capabilities.Egress) != len(wantEgress) {
		t.Fatalf("egress: %#v", d.Capabilities.Egress)
	}
	for _, e := range d.Capabilities.Egress {
		if !wantEgress[e] {
			t.Errorf("unexpected egress %q", e)
		}
	}
	wantVerbs := []string{"list_checks", "get_check", "create_check", "update_check", "pause_check", "delete_check", "ping", "get_pings", "api"}
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
	if len(d.Events) != 1 || d.Events[0].Name != "check" {
		t.Fatalf("events: %#v", d.Events)
	}
}

// --- management API verbs, against an httptest.Server ---

func newMgmtServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestListChecks(t *testing.T) {
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/checks/" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Api-Key"); got != "test-key" {
			t.Fatalf("X-Api-Key: %q", got)
		}
		if got := r.URL.Query().Get("tag"); got != "prod" {
			t.Fatalf("tag query: %q", got)
		}
		w.Write([]byte(`{"checks":[{"name":"job-a"},{"name":"job-b"}]}`))
	})
	p := &healthchecksPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "list_checks",
		Connection: map[string]any{"api_key": "test-key", "api_base": srv.URL},
		Options:    map[string]any{"tag": "prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestGetCheck(t *testing.T) {
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/checks/abc-123" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Fatalf("method: %s", r.Method)
		}
		w.Write([]byte(`{"name":"job-a","status":"up"}`))
	})
	p := &healthchecksPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get_check",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"uuid": "abc-123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.Outputs["result"].(map[string]any)
	if !ok || m["name"] != "job-a" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestCreateCheck(t *testing.T) {
	var gotBody map[string]any
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/checks/" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method: %s", r.Method)
		}
		if got := r.Header.Get("X-Api-Key"); got != "k" {
			t.Fatalf("X-Api-Key: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"uuid":"new-uuid","name":"job-a"}`))
	})
	p := &healthchecksPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "create_check",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options: map[string]any{
			"name": "job-a", "tags": []any{"prod", "web"}, "timeout": 3600, "grace": 600,
			"schedule": "* * * * *", "tz": "UTC",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["name"] != "job-a" || gotBody["tags"] != "prod web" || gotBody["schedule"] != "* * * * *" {
		t.Fatalf("request body: %#v", gotBody)
	}
	m, ok := res.Outputs["result"].(map[string]any)
	if !ok || m["uuid"] != "new-uuid" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestUpdateCheck(t *testing.T) {
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/checks/abc-123" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method: %s", r.Method)
		}
		w.Write([]byte(`{"uuid":"abc-123","name":"renamed"}`))
	})
	p := &healthchecksPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "update_check",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"uuid": "abc-123", "name": "renamed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Outputs["result"].(map[string]any)
	if m["name"] != "renamed" {
		t.Fatalf("result: %#v", m)
	}
}

func TestPauseCheck(t *testing.T) {
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/checks/abc-123/pause" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method: %s", r.Method)
		}
		w.Write([]byte(`{"uuid":"abc-123","status":"paused"}`))
	})
	p := &healthchecksPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "pause_check",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"uuid": "abc-123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Outputs["result"].(map[string]any)
	if m["status"] != "paused" {
		t.Fatalf("result: %#v", m)
	}
}

func TestDeleteCheck(t *testing.T) {
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/checks/abc-123" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if r.Method != http.MethodDelete {
			t.Fatalf("method: %s", r.Method)
		}
		w.Write([]byte(`{}`))
	})
	p := &healthchecksPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "delete_check",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"uuid": "abc-123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["ok"] != true {
		t.Fatalf("ok: %#v", res.Outputs)
	}
}

func TestGetPings(t *testing.T) {
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/checks/abc-123/pings/" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"pings":[{"type":"success"},{"type":"start"}]}`))
	})
	p := &healthchecksPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get_pings",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"uuid": "abc-123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIVerb(t *testing.T) {
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/channels/" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"channels":[]}`))
	})
	p := &healthchecksPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "api",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"method": "GET", "path": "/api/v3/channels/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := m["channels"]; !ok {
		t.Fatalf("result missing channels: %#v", m)
	}
}

// TestMgmtNonOK proves a non-2xx management response becomes a
// CodeInternalError carrying the status and body, per spec.
func TestMgmtNonOK(t *testing.T) {
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	})
	p := &healthchecksPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get_check",
		Connection: map[string]any{"api_key": "k", "api_base": srv.URL},
		Options:    map[string]any{"uuid": "missing"},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
	if !contains(pe.Message, "404") || !contains(pe.Message, "not found") {
		t.Fatalf("error message should carry status+body: %q", pe.Message)
	}
}

// --- pinging: a second httptest.Server, NO api key ---

func TestPingSuccess(t *testing.T) {
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping-uuid" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Fatalf("method: %s", r.Method)
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Fatalf("ping must not send X-Api-Key, got %q", got)
		}
		w.Write([]byte("OK"))
	})
	p := &healthchecksPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "ping",
		Connection: map[string]any{"api_key": "should-not-be-sent", "ping_base": srv.URL},
		Options:    map[string]any{"uuid": "ping-uuid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["ok"] != true {
		t.Fatalf("ok: %#v", res.Outputs)
	}
}

func TestPingFailWithBody(t *testing.T) {
	var gotBody string
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping-uuid/fail" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method: %s", r.Method)
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte("OK"))
	})
	p := &healthchecksPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "ping",
		Connection: map[string]any{"ping_base": srv.URL},
		Options:    map[string]any{"uuid": "ping-uuid", "status": "fail", "body": "boom"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["ok"] != true {
		t.Fatalf("ok: %#v", res.Outputs)
	}
	if gotBody != "boom" {
		t.Fatalf("ping body: %q", gotBody)
	}
}

func TestPingStartAndLog(t *testing.T) {
	var gotPath string
	srv := newMgmtServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("OK"))
	})
	p := &healthchecksPlugin{}
	for _, tc := range []struct{ status, wantPath string }{
		{"start", "/ping-uuid/start"},
		{"log", "/ping-uuid/log"},
	} {
		_, err := p.Invoke(plugin.InvokeRequest{
			Verb:       "ping",
			Connection: map[string]any{"ping_base": srv.URL},
			Options:    map[string]any{"uuid": "ping-uuid", "status": tc.status},
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotPath != tc.wantPath {
			t.Fatalf("status %s: path %q, want %q", tc.status, gotPath, tc.wantPath)
		}
	}
}

// --- webhook source ---

func startSourceForTest(t *testing.T, p *healthchecksPlugin, cfg map[string]any) (emitted chan map[string]any, addr string) {
	t.Helper()
	ln := freeAddr(t)
	if webhook, ok := cfg["webhook"].(map[string]any); ok {
		webhook["listen"] = ln
	}
	ctx, cancel := contextWithCancel()
	t.Cleanup(cancel)
	emitted = make(chan map[string]any, 8)
	go func() {
		_ = p.StartSource(ctx, plugin.StartSourceRequest{Instance: "test", Config: cfg}, func(payload any) error {
			b, _ := json.Marshal(payload)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			emitted <- m
			return nil
		})
	}()
	waitForListener(t, ln)
	return emitted, ln
}

func TestWebhookAcceptsValidToken(t *testing.T) {
	p := &healthchecksPlugin{}
	emitted, addr := startSourceForTest(t, p, map[string]any{
		"webhook": map[string]any{"path": "/healthchecks", "secret": "shh"},
	})
	body := `{"check":"job-a","status":"down","uuid":"u-1","tags":"prod web"}`
	resp, err := http.Post("http://"+addr+"/healthchecks?token=shh", "application/json", jsonReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	select {
	case m := <-emitted:
		if m["event"] != "check" || m["dedup"] != "u-1\x00down" {
			t.Fatalf("emitted: %#v", m)
		}
		ctxMap := m["context"].(map[string]any)
		if ctxMap["name"] != "job-a" || ctxMap["status"] != "down" || ctxMap["uuid"] != "u-1" {
			t.Fatalf("context: %#v", ctxMap)
		}
		tags, _ := ctxMap["tags"].([]any)
		if len(tags) != 2 || tags[0] != "prod" || tags[1] != "web" {
			t.Fatalf("tags: %#v", ctxMap["tags"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for emitted event")
	}
}

// TestWebhookRejectsWrongToken proves a wrong token never emits an event.
// The shared sourcekit.Listener always responds 202 once it has read the
// body (there is no per-request status hook for a rejection that happens
// inside the callback), so the assertion is on the absence of an emit, not
// the HTTP status.
func TestWebhookRejectsWrongToken(t *testing.T) {
	p := &healthchecksPlugin{}
	emitted, addr := startSourceForTest(t, p, map[string]any{
		"webhook": map[string]any{"path": "/healthchecks", "secret": "shh"},
	})
	body := `{"check":"job-a","status":"down","uuid":"u-1","tags":""}`
	resp, err := http.Post("http://"+addr+"/healthchecks?token=wrong", "application/json", jsonReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	select {
	case m := <-emitted:
		t.Fatalf("unexpected emit on bad token: %#v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWebhookAcceptsHeaderToken(t *testing.T) {
	p := &healthchecksPlugin{}
	emitted, addr := startSourceForTest(t, p, map[string]any{
		"webhook": map[string]any{"path": "/healthchecks", "secret": "shh"},
	})
	body := `{"check":"job-a","status":"up","uuid":"u-2","tags":""}`
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/healthchecks", jsonReader(body))
	req.Header.Set("X-Conductor-Token", "shh")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	select {
	case m := <-emitted:
		if m["dedup"] != "u-2\x00up" {
			t.Fatalf("emitted: %#v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for emitted event")
	}
}

// TestStartSourceFailsClosedWithoutSecret proves an unset webhook.secret
// refuses to start unless allow_unsigned is explicitly set.
func TestStartSourceFailsClosedWithoutSecret(t *testing.T) {
	p := &healthchecksPlugin{}
	ctx, cancel := contextWithCancel()
	defer cancel()
	err := p.StartSource(ctx, plugin.StartSourceRequest{
		Instance: "test",
		Config:   map[string]any{"webhook": map[string]any{"listen": freeAddr(t)}},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected error when webhook.secret is unset and allow_unsigned is false")
	}
}

// --- test helpers ---

// asPluginError unwraps a *plugin.Error without importing errors just for the
// test (the SDK returns the concrete type directly).
func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}

func contains(s, substr string) bool { return strings.Contains(s, substr) }

// freeAddr picks a free localhost TCP port for a webhook listener under test.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func jsonReader(s string) *strings.Reader { return strings.NewReader(s) }

// waitForListener polls until addr accepts TCP connections (the background
// StartSource goroutine has bound its listener) or fails the test.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener at %s never came up", addr)
}
