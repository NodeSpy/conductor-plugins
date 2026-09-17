package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// TestBuildRequest pins the exact method/path/query/body each verb builds —
// the whole contract with the Prowlarr API, proven without spawning anything.
func TestBuildRequest(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		wantMethod string
		wantPath   string
		wantQuery  url.Values
		wantBody   any
		wantAsList bool
	}{
		{
			name:       "indexers",
			verb:       "indexers",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/indexer",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name:       "indexer_get",
			verb:       "indexer_get",
			opts:       map[string]any{"id": 42},
			wantMethod: http.MethodGet,
			wantPath:   "/indexer/42",
			wantQuery:  url.Values{},
		},
		{
			name:       "indexer_stats",
			verb:       "indexer_stats",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/indexerstats",
			wantQuery:  url.Values{},
		},
		{
			name:       "applications",
			verb:       "applications",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/applications",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name:       "search minimal",
			verb:       "search",
			opts:       map[string]any{"query": "black hawk down"},
			wantMethod: http.MethodGet,
			wantPath:   "/search",
			wantQuery:  url.Values{"query": {"black hawk down"}},
			wantAsList: true,
		},
		{
			name: "search with indexer_ids and categories",
			verb: "search",
			opts: map[string]any{
				"query":       "foo",
				"indexer_ids": []any{float64(1), float64(2)},
				"categories":  []any{float64(2000), float64(5000)},
				"type":        "tv-search",
			},
			wantMethod: http.MethodGet,
			wantPath:   "/search",
			wantQuery: url.Values{
				"query":      {"foo"},
				"indexerIds": {"1,2"},
				"categories": {"2000", "5000"},
				"type":       {"tv-search"},
			},
			wantAsList: true,
		},
		{
			name:       "search no options",
			verb:       "search",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/search",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name: "command with params merged",
			verb: "command",
			opts: map[string]any{
				"name": "ApplicationIndexerSync", "params": map[string]any{"foo": "bar"},
			},
			wantMethod: http.MethodPost,
			wantPath:   "/command",
			wantQuery:  url.Values{},
			wantBody:   map[string]any{"name": "ApplicationIndexerSync", "foo": "bar"},
		},
		{
			name:       "command minimal",
			verb:       "command",
			opts:       map[string]any{"name": "CheckHealth"},
			wantMethod: http.MethodPost,
			wantPath:   "/command",
			wantQuery:  url.Values{},
			wantBody:   map[string]any{"name": "CheckHealth"},
		},
		{
			name:       "system_status",
			verb:       "system_status",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/system/status",
			wantQuery:  url.Values{},
		},
		{
			name:       "tags",
			verb:       "tags",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/tag",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name:       "api escape hatch default method",
			verb:       "api",
			opts:       map[string]any{"path": "system/status"},
			wantMethod: http.MethodGet,
			wantPath:   "/system/status",
			wantQuery:  url.Values{},
		},
		{
			name: "api escape hatch explicit method + query + body",
			verb: "api",
			opts: map[string]any{
				"method": "put", "path": "/indexer/editor", "query": map[string]any{"x": "1"},
				"body": map[string]any{"a": "b"},
			},
			wantMethod: http.MethodPut,
			wantPath:   "/indexer/editor",
			wantQuery:  url.Values{"x": {"1"}},
			wantBody:   map[string]any{"a": "b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rb, err := buildRequest(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("buildRequest(%s): unexpected error: %v", tc.verb, err)
			}
			if rb.method != tc.wantMethod {
				t.Errorf("method: got %q want %q", rb.method, tc.wantMethod)
			}
			if rb.path != tc.wantPath {
				t.Errorf("path: got %q want %q", rb.path, tc.wantPath)
			}
			if rb.query.Encode() != tc.wantQuery.Encode() {
				t.Errorf("query:\n got: %#v\nwant: %#v", rb.query, tc.wantQuery)
			}
			if tc.wantBody != nil {
				gotJSON, _ := json.Marshal(rb.body)
				wantJSON, _ := json.Marshal(tc.wantBody)
				if string(gotJSON) != string(wantJSON) {
					t.Errorf("body:\n got: %s\nwant: %s", gotJSON, wantJSON)
				}
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
		{"indexer_get", map[string]any{}}, // no id
		{"command", map[string]any{}},     // no name
		{"api", map[string]any{}},         // no path
		{"nope", map[string]any{}},        // unknown verb
	}
	for _, tc := range cases {
		if _, err := buildRequest(tc.verb, tc.opts); err == nil {
			t.Errorf("buildRequest(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// newFakeProwlarr starts an httptest.Server standing in for Prowlarr,
// asserting every request carries the X-Api-Key header and hits /api/v1,
// then delegates to check for verb-specific assertions and returns the given
// JSON body.
func newFakeProwlarr(t *testing.T, wantMethod, wantPath string, check func(t *testing.T, r *http.Request), status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("X-Api-Key: got %q want test-key", r.Header.Get("X-Api-Key"))
		}
		if r.Method != wantMethod {
			t.Errorf("method: got %q want %q", r.Method, wantMethod)
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v1") {
			t.Errorf("path: got %q want prefix /api/v1", r.URL.Path)
		}
		if wantPath != "" && r.URL.Path != "/api/v1"+wantPath {
			t.Errorf("path: got %q want %q", r.URL.Path, "/api/v1"+wantPath)
		}
		if check != nil {
			check(t, r)
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestInvokeIndexersItems(t *testing.T) {
	srv := newFakeProwlarr(t, http.MethodGet, "/indexer", nil, 0,
		`[{"id":1,"name":"Foo"},{"id":2,"name":"Bar"}]`)
	defer srv.Close()

	p := prowlarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "indexers",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if res.Outputs["status_code"] != http.StatusOK {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestInvokeIndexerGetResult(t *testing.T) {
	srv := newFakeProwlarr(t, http.MethodGet, "/indexer/42", nil, 0, `{"id":42,"name":"Foo"}`)
	defer srv.Close()

	p := prowlarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "indexer_get",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"id": 42},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["name"] != "Foo" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeSearchQueryParams(t *testing.T) {
	srv := newFakeProwlarr(t, http.MethodGet, "/search", func(t *testing.T, r *http.Request) {
		if r.URL.Query().Get("query") != "foo" {
			t.Errorf("query: %q", r.URL.Query().Get("query"))
		}
		if r.URL.Query().Get("indexerIds") != "1,2" {
			t.Errorf("indexerIds: %q", r.URL.Query().Get("indexerIds"))
		}
		if got := r.URL.Query()["categories"]; len(got) != 2 || got[0] != "2000" || got[1] != "5000" {
			t.Errorf("categories: %#v", got)
		}
	}, 0, `[{"title":"Some Release"}]`)
	defer srv.Close()

	p := prowlarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "search",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options: map[string]any{
			"query": "foo", "indexer_ids": []any{float64(1), float64(2)},
			"categories": []any{float64(2000), float64(5000)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeCommandJSONBody(t *testing.T) {
	var gotBody map[string]any
	srv := newFakeProwlarr(t, http.MethodPost, "/command", func(t *testing.T, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
	}, 0, `{"id":10,"name":"ApplicationIndexerSync","status":"queued"}`)
	defer srv.Close()

	p := prowlarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "command",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"name": "ApplicationIndexerSync"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["name"] != "ApplicationIndexerSync" {
		t.Fatalf("posted body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "queued" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeApplicationsItems(t *testing.T) {
	srv := newFakeProwlarr(t, http.MethodGet, "/applications", nil, 0, `[{"id":1,"name":"Sonarr"}]`)
	defer srv.Close()

	p := prowlarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "applications",
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

func TestInvokeAPIEscapeHatchArrayIntoItems(t *testing.T) {
	srv := newFakeProwlarr(t, http.MethodGet, "/system/status", nil, 0, `[{"a":1}]`)
	defer srv.Close()

	p := prowlarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "api",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"path": "/system/status"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].([]any); !ok {
		t.Fatalf("result should carry the raw array too: %#v", res.Outputs["result"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeNon2xxSurfacesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`Invalid API Key`))
	}))
	defer srv.Close()

	p := prowlarrPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "indexers",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "bad-key"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "401") || !strings.Contains(pe.Message, "Invalid API Key") {
		t.Fatalf("error message should carry status + body: %q", pe.Message)
	}
}

func TestInvokeMissingConnection(t *testing.T) {
	p := prowlarrPlugin{}
	cases := []map[string]any{
		{},
		{"base_url": "http://prowlarr:9696"},
		{"api_key": "k"},
	}
	for _, conn := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "indexers", Connection: conn})
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

// TestHandleWebhookBodyHealthIssueEvent proves parsing + context shape for a
// typical Prowlarr HealthIssue webhook delivery.
func TestHandleWebhookBodyHealthIssueEvent(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{
		"eventType": "HealthIssue",
		"level": "warning",
		"message": "Indexer XYZ is unavailable",
		"type": "IndexerStatusCheck",
		"wikiUrl": "https://wiki.servarr.com/prowlarr/system#indexers"
	}`)

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
	if got["kind"] != "HealthIssue" {
		t.Errorf("kind: %#v", got["kind"])
	}
	ctx, ok := got["context"].(map[string]any)
	if !ok {
		t.Fatalf("context: %#v", got["context"])
	}
	if ctx["event_type"] != "HealthIssue" || ctx["level"] != "warning" || ctx["message"] != "Indexer XYZ is unavailable" {
		t.Fatalf("context fields: %#v", ctx)
	}
	if ctx["issue_type"] != "IndexerStatusCheck" {
		t.Errorf("issue_type: %#v", ctx["issue_type"])
	}
	if ctx["wiki_url"] != "https://wiki.servarr.com/prowlarr/system#indexers" {
		t.Errorf("wiki_url: %#v", ctx["wiki_url"])
	}
	if ctx["event_types"] != "HealthIssue" {
		t.Errorf("filter alias: %#v", ctx["event_types"])
	}
	payload, ok := ctx["payload"].(map[string]any)
	if !ok || payload["eventType"] != "HealthIssue" {
		t.Fatalf("context.payload: %#v", ctx["payload"])
	}
}

// TestHandleWebhookBodyApplicationUpdateEvent proves an ApplicationUpdate
// event extracts the version fields correctly.
func TestHandleWebhookBodyApplicationUpdateEvent(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{
		"eventType": "ApplicationUpdate",
		"message": "Prowlarr updated from 1.0.0.0 to 1.1.0.0",
		"previousVersion": "1.0.0.0",
		"newVersion": "1.1.0.0"
	}`)

	var got map[string]any
	handleWebhookBody(body, dedup, func(payload any) error {
		got, _ = payload.(map[string]any)
		return nil
	})
	if got == nil {
		t.Fatal("expected an emitted event")
	}
	ctx := got["context"].(map[string]any)
	if ctx["previous_version"] != "1.0.0.0" || ctx["new_version"] != "1.1.0.0" {
		t.Fatalf("version fields: %#v", ctx)
	}
}

// TestHandleWebhookBodyDedups proves a redelivered notification (same
// eventType + message) is dropped the second time.
func TestHandleWebhookBodyDedups(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{"eventType":"HealthIssue","message":"same issue"}`)

	n := 0
	emit := func(payload any) error { n++; return nil }
	handleWebhookBody(body, dedup, emit)
	handleWebhookBody(body, dedup, emit)
	if n != 1 {
		t.Fatalf("expected exactly one emission, got %d", n)
	}
}

// TestHandleWebhookBodyDropsMissingEventType proves a payload with no
// eventType is dropped rather than emitted blind.
func TestHandleWebhookBodyDropsMissingEventType(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	emitted := false
	handleWebhookBody([]byte(`{"message":"no event type"}`), dedup, func(payload any) error {
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
	p := prowlarrPlugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.StartSource(ctx, plugin.StartSourceRequest{
			Instance: "test",
			Config: map[string]any{
				"webhook": map[string]any{
					"listen": addr,
					"path":   "/prowlarr",
					"secret": "s3cret",
				},
			},
		}, emit)
	}()
	waitForListener(t, addr)

	body := []byte(`{"eventType":"HealthIssue","message":"a"}`)

	// Wrong token: nothing emitted.
	postJSON(t, addr, "/prowlarr?token=nope", body, nil)
	select {
	case ev := <-events:
		t.Fatalf("wrong token should not emit, got %#v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	// Correct token via header: accepted and emits.
	postJSON(t, addr, "/prowlarr", body, map[string]string{"X-Conductor-Token": "s3cret"})
	select {
	case ev := <-events:
		if ev["kind"] != "HealthIssue" {
			t.Fatalf("emitted event: %#v", ev)
		}
	default:
		t.Fatal("expected an emitted event for the correctly-authenticated request")
	}

	// Correct token via query param, different message so dedup doesn't hide it.
	body2 := []byte(`{"eventType":"HealthIssue","message":"b"}`)
	postJSON(t, addr, "/prowlarr?token=s3cret", body2, nil)
	select {
	case ev := <-events:
		if ev["dedup"] != "HealthIssue\x00b" {
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
	d := prowlarrPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "prowlarr" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("capabilities: must not spawn/command: %#v", d.Capabilities)
	}
	if d.Capabilities.Egress == nil || len(d.Capabilities.Egress) != 0 {
		t.Fatalf("capabilities.egress: want declared-empty ([]string{}), got %#v", d.Capabilities.Egress)
	}
	want := []string{
		"indexers", "indexer_get", "indexer_stats", "applications",
		"search", "command", "system_status", "tags", "api",
	}
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

// --- test helpers ------------------------------------------------------------

func postJSON(t *testing.T, addr, path string, body []byte, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(string(body)))
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
