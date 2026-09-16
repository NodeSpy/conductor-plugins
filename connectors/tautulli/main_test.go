package main

import (
	"bytes"
	"context"
	"net"
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

// TestBuildRequest pins the exact cmd + query params each verb builds — the
// whole contract with the Tautulli API, proven without spawning anything.
func TestBuildRequest(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		wantCmd    string
		wantQuery  url.Values
		wantAsList bool
	}{
		{
			name:      "activity",
			verb:      "activity",
			opts:      map[string]any{},
			wantCmd:   "get_activity",
			wantQuery: url.Values{},
		},
		{
			name:       "history",
			verb:       "history",
			opts:       map[string]any{"user": "alice", "section_id": "2", "length": 25, "start": 0},
			wantCmd:    "get_history",
			wantQuery:  url.Values{"user": {"alice"}, "section_id": {"2"}, "length": {"25"}, "start": {"0"}},
			wantAsList: true,
		},
		{
			name:       "history no options",
			verb:       "history",
			opts:       map[string]any{},
			wantCmd:    "get_history",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name:      "home_stats",
			verb:      "home_stats",
			opts:      map[string]any{},
			wantCmd:   "get_home_stats",
			wantQuery: url.Values{},
		},
		{
			name:       "libraries",
			verb:       "libraries",
			opts:       map[string]any{},
			wantCmd:    "get_libraries",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name:       "users",
			verb:       "users",
			opts:       map[string]any{},
			wantCmd:    "get_users",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name:      "metadata",
			verb:      "metadata",
			opts:      map[string]any{"rating_key": "12345"},
			wantCmd:   "get_metadata",
			wantQuery: url.Values{"rating_key": {"12345"}},
		},
		{
			name:       "recently_added",
			verb:       "recently_added",
			opts:       map[string]any{"count": 10, "section_id": "3"},
			wantCmd:    "get_recently_added",
			wantQuery:  url.Values{"count": {"10"}, "section_id": {"3"}},
			wantAsList: true,
		},
		{
			name:      "server_info",
			verb:      "server_info",
			opts:      map[string]any{},
			wantCmd:   "get_server_info",
			wantQuery: url.Values{},
		},
		{
			name:      "notify",
			verb:      "notify",
			opts:      map[string]any{"notifier_id": 1, "subject": "hi", "body": "there"},
			wantCmd:   "notify",
			wantQuery: url.Values{"notifier_id": {"1"}, "subject": {"hi"}, "body": {"there"}},
		},
		{
			name:      "terminate_session",
			verb:      "terminate_session",
			opts:      map[string]any{"session_key": "42", "message": "bye"},
			wantCmd:   "terminate_session",
			wantQuery: url.Values{"session_key": {"42"}, "message": {"bye"}},
		},
		{
			name:      "terminate_session no message",
			verb:      "terminate_session",
			opts:      map[string]any{"session_key": "42"},
			wantCmd:   "terminate_session",
			wantQuery: url.Values{"session_key": {"42"}},
		},
		{
			name:      "api generic",
			verb:      "api",
			opts:      map[string]any{"cmd": "get_plex_log", "params": map[string]any{"log_type": "server"}},
			wantCmd:   "get_plex_log",
			wantQuery: url.Values{"log_type": {"server"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rb, err := buildRequest(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("buildRequest(%s): unexpected error: %v", tc.verb, err)
			}
			if rb.cmd != tc.wantCmd {
				t.Errorf("cmd: got %q want %q", rb.cmd, tc.wantCmd)
			}
			if !reflect.DeepEqual(rb.query, tc.wantQuery) {
				t.Errorf("query:\n got: %#v\nwant: %#v", rb.query, tc.wantQuery)
			}
			if rb.asList != tc.wantAsList {
				t.Errorf("asList: got %v want %v", rb.asList, tc.wantAsList)
			}
		})
	}
}

// TestBuildRequestErrors covers required-field validation and unknown verbs.
func TestBuildRequestErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"metadata", map[string]any{}},                               // no rating_key
		{"notify", map[string]any{}},                                 // no notifier_id/subject/body
		{"notify", map[string]any{"notifier_id": 1}},                 // no subject/body
		{"notify", map[string]any{"notifier_id": 1, "subject": "x"}}, // no body
		{"terminate_session", map[string]any{}},                      // no session_key
		{"api", map[string]any{}},                                    // no cmd
		{"nope", map[string]any{}},                                   // unknown verb
	}
	for _, tc := range cases {
		if _, err := buildRequest(tc.verb, tc.opts); err == nil {
			t.Errorf("buildRequest(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// newFakeTautulli starts an httptest.Server standing in for Tautulli,
// asserting the GET /api/v2 request carries apikey/cmd, and returning the
// given JSON body.
func newFakeTautulli(t *testing.T, wantCmd string, checkQuery func(t *testing.T, q url.Values), body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2" {
			t.Errorf("path: got %q want /api/v2", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("apikey") != "test-key" {
			t.Errorf("apikey: got %q want test-key", q.Get("apikey"))
		}
		if q.Get("cmd") != wantCmd {
			t.Errorf("cmd: got %q want %q", q.Get("cmd"), wantCmd)
		}
		if checkQuery != nil {
			checkQuery(t, q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestInvokeActivityResult(t *testing.T) {
	srv := newFakeTautulli(t, "get_activity", nil,
		`{"response":{"result":"success","message":null,"data":{"stream_count":"1"}}}`)
	defer srv.Close()

	p := tautulliPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "activity",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["stream_count"] != "1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := res.Outputs["items"]; ok {
		t.Fatalf("activity should not produce items: %#v", res.Outputs)
	}
}

func TestInvokeHistoryHoistsData(t *testing.T) {
	srv := newFakeTautulli(t, "get_history", func(t *testing.T, q url.Values) {
		if q.Get("user") != "alice" {
			t.Errorf("user: %q", q.Get("user"))
		}
	}, `{"response":{"result":"success","data":{"recordsTotal":2,"data":[{"user":"alice"},{"user":"bob"}]}}}`)
	defer srv.Close()

	p := tautulliPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "history",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"user": "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeLibrariesHoistsTopLevelList(t *testing.T) {
	srv := newFakeTautulli(t, "get_libraries", nil,
		`{"response":{"result":"success","data":[{"section_id":"1"},{"section_id":"2"}]}}`)
	defer srv.Close()

	p := tautulliPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "libraries",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeRecentlyAddedHoistsNamedKey(t *testing.T) {
	srv := newFakeTautulli(t, "get_recently_added", nil,
		`{"response":{"result":"success","data":{"recently_added":[{"title":"Movie A"}]}}}`)
	defer srv.Close()

	p := tautulliPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "recently_added",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeMetadataResult(t *testing.T) {
	srv := newFakeTautulli(t, "get_metadata", func(t *testing.T, q url.Values) {
		if q.Get("rating_key") != "999" {
			t.Errorf("rating_key: %q", q.Get("rating_key"))
		}
	}, `{"response":{"result":"success","data":{"title":"Some Movie"}}}`)
	defer srv.Close()

	p := tautulliPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "metadata",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"rating_key": "999"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["title"] != "Some Movie" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeResultErrorSurfacesMessage(t *testing.T) {
	srv := newFakeTautulli(t, "get_metadata", nil,
		`{"response":{"result":"error","message":"Rating key not found","data":null}}`)
	defer srv.Close()

	p := tautulliPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "metadata",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"rating_key": "999"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "Rating key not found") {
		t.Fatalf("error message should carry the api message: %q", pe.Message)
	}
}

func TestInvokeNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`Invalid apikey`))
	}))
	defer srv.Close()

	p := tautulliPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "activity",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "bad-key"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "401") || !strings.Contains(pe.Message, "Invalid apikey") {
		t.Fatalf("error message should carry status + body: %q", pe.Message)
	}
}

func TestInvokeMissingConnection(t *testing.T) {
	p := tautulliPlugin{}
	cases := []map[string]any{
		{},
		{"base_url": "http://tautulli:8181"},
		{"api_key": "k"},
	}
	for _, conn := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "activity", Connection: conn})
		if err == nil {
			t.Fatalf("connection %#v: expected error", conn)
		}
		var pe *plugin.Error
		if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
			t.Fatalf("connection %#v: want CodeInvalidParams, got %v", conn, err)
		}
	}
}

// --- webhook source tests ---------------------------------------------------

// TestHandleWebhookBodyEmitsNormalizedEvent proves parsing + context shape
// for a typical Tautulli Webhook agent delivery.
func TestHandleWebhookBodyEmitsNormalizedEvent(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{"action":"play","title":"Some Movie","user":"alice","player":"Living Room","media_type":"movie","rating_key":"12345"}`)

	var got map[string]any
	handleWebhookBody(body, dedup, func(payload any) error {
		got, _ = payload.(map[string]any)
		return nil
	})
	if got == nil {
		t.Fatal("expected an emitted event")
	}
	if got["event"] != "event" {
		t.Errorf("event: %#v", got["event"])
	}
	if got["kind"] != "play" {
		t.Errorf("kind: %#v", got["kind"])
	}
	ctx, ok := got["context"].(map[string]any)
	if !ok {
		t.Fatalf("context: %#v", got["context"])
	}
	if ctx["action"] != "play" || ctx["user"] != "alice" || ctx["rating_key"] != "12345" {
		t.Fatalf("context fields: %#v", ctx)
	}
	payload, ok := ctx["payload"].(map[string]any)
	if !ok || payload["title"] != "Some Movie" {
		t.Fatalf("context.payload: %#v", ctx["payload"])
	}
}

// TestHandleWebhookBodyDedups proves a redelivered notification (same
// rating_key + action) is dropped the second time.
func TestHandleWebhookBodyDedups(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{"action":"play","rating_key":"1"}`)

	n := 0
	emit := func(payload any) error { n++; return nil }
	handleWebhookBody(body, dedup, emit)
	handleWebhookBody(body, dedup, emit)
	if n != 1 {
		t.Fatalf("expected exactly one emission, got %d", n)
	}
}

// TestHandleWebhookBodyDropsEmpty proves a payload with neither action nor
// rating_key is dropped rather than emitted blind.
func TestHandleWebhookBodyDropsEmpty(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	emitted := false
	handleWebhookBody([]byte(`{"title":"no action or rating_key"}`), dedup, func(payload any) error {
		emitted = true
		return nil
	})
	if emitted {
		t.Fatal("expected the payload to be dropped")
	}
}

// TestTokenEqual proves the constant-time token comparison accepts a matching
// token and rejects everything else, including different-length tokens.
func TestTokenEqual(t *testing.T) {
	if !tokenEqual("s3cret", "s3cret") {
		t.Fatal("matching token rejected")
	}
	if tokenEqual("wrong", "s3cret") {
		t.Fatal("mismatched token accepted")
	}
	if tokenEqual("", "s3cret") {
		t.Fatal("empty token accepted")
	}
	if tokenEqual("s3cretlonger", "s3cret") {
		t.Fatal("different-length token accepted")
	}
}

// TestRequireToken proves the fail-closed contract: no secret refuses to
// start unless allow_unsigned is set.
func TestRequireToken(t *testing.T) {
	if err := requireToken("", false); err == nil {
		t.Fatal("expected an error with no secret and no allow_unsigned")
	}
	if err := requireToken("", true); err != nil {
		t.Fatalf("allow_unsigned should permit no secret: %v", err)
	}
	if err := requireToken("s3cret", false); err != nil {
		t.Fatalf("a configured secret should never error: %v", err)
	}
}

// TestVerifyToken proves the header/query precedence (header wins) and that
// only a matching token passes.
func TestVerifyToken(t *testing.T) {
	secret := "s3cret"
	mk := func(header, query string) *sourcekit.Request {
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

// TestStartSourceWebhookTokenAcceptReject drives StartSource end to end
// against a real (loopback) listener: a request with the correct token is
// accepted and emits an event; a request with a missing/wrong token emits
// nothing.
func TestStartSourceWebhookTokenAcceptReject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan map[string]any, 4)
	emit := func(payload any) error {
		if m, ok := payload.(map[string]any); ok {
			events <- m
		}
		return nil
	}

	addr := freeLoopbackAddr(t)
	p := tautulliPlugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.StartSource(ctx, plugin.StartSourceRequest{
			Instance: "test",
			Config: map[string]any{
				"webhook": map[string]any{
					"listen": addr,
					"path":   "/tautulli",
					"secret": "s3cret",
				},
			},
		}, emit)
	}()
	waitForListener(t, addr)

	body := []byte(`{"action":"play","title":"Some Movie","rating_key":"1"}`)

	// Wrong token: nothing emitted.
	postJSON(t, addr, "/tautulli?token=nope", body, nil)
	select {
	case ev := <-events:
		t.Fatalf("wrong token should not emit, got %#v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	// Correct token via header: accepted and emits.
	postJSON(t, addr, "/tautulli", body, map[string]string{"X-Conductor-Token": "s3cret"})
	select {
	case ev := <-events:
		if ev["kind"] != "play" {
			t.Fatalf("emitted event: %#v", ev)
		}
	default:
		t.Fatal("expected an emitted event for the correctly-authenticated request")
	}

	// Correct token via query param, different rating_key so dedup doesn't hide it.
	body2 := []byte(`{"action":"play","title":"Some Movie","rating_key":"2"}`)
	postJSON(t, addr, "/tautulli?token=s3cret", body2, nil)
	select {
	case ev := <-events:
		if ev["dedup"] != "2\x00play" {
			t.Fatalf("emitted event: %#v", ev)
		}
	default:
		t.Fatal("expected an emitted event for the query-token request")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("StartSource: %v", err)
	}
}

// TestDescribe asserts the declared surface: kind, type, capabilities, verbs
// and events.
func TestDescribe(t *testing.T) {
	d := tautulliPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "tautulli" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("capabilities: must not spawn/command: %#v", d.Capabilities)
	}
	if d.Capabilities.Egress == nil || len(d.Capabilities.Egress) != 0 {
		t.Fatalf("capabilities.egress: want declared-empty ([]string{}), got %#v", d.Capabilities.Egress)
	}
	want := []string{"activity", "history", "home_stats", "libraries", "users",
		"metadata", "recently_added", "server_info", "notify", "terminate_session", "api"}
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
		t.Fatalf("Describe events: %#v", d.Events)
	}
}

func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}

// --- test helpers over the package's own types (kept here to avoid
// widening main.go's exported surface just for tests) -----------------------

func postJSON(t *testing.T, addr, path string, body []byte, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// freeLoopbackAddr reserves a free loopback port and returns its address as
// "127.0.0.1:PORT", releasing the listener immediately so the plugin's own
// http.Server can bind it. Small window for a race with another process
// grabbing the same port; acceptable in a test.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// waitForListener polls until addr accepts a TCP connection or the deadline
// passes.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener at %s never came up", addr)
}
