package main

import (
	"bytes"
	"context"
	"mime/multipart"
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

// --- buildRequest --------------------------------------------------------

// TestBuildRequest pins the exact method/path/query each verb builds — the
// whole contract with the Plex Media Server, proven without spawning
// anything.
func TestBuildRequest(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		wantMethod string
		wantPath   string
		wantQuery  url.Values
		wantBody   string
	}{
		{
			name: "sessions", verb: "sessions", opts: map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/status/sessions", wantQuery: url.Values{},
		},
		{
			name: "library_sections", verb: "library_sections", opts: map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/library/sections", wantQuery: url.Values{},
		},
		{
			name: "scan_library", verb: "scan_library", opts: map[string]any{"section_id": "3"},
			wantMethod: http.MethodGet, wantPath: "/library/sections/3/refresh", wantQuery: url.Values{},
		},
		{
			name: "search", verb: "search", opts: map[string]any{"query": "matrix"},
			wantMethod: http.MethodGet, wantPath: "/search", wantQuery: url.Values{"query": {"matrix"}},
		},
		{
			name: "metadata", verb: "metadata", opts: map[string]any{"rating_key": "12345"},
			wantMethod: http.MethodGet, wantPath: "/library/metadata/12345", wantQuery: url.Values{},
		},
		{
			name: "recently_added server-wide", verb: "recently_added", opts: map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/library/recentlyAdded", wantQuery: url.Values{},
		},
		{
			name: "recently_added section", verb: "recently_added", opts: map[string]any{"section_id": "2"},
			wantMethod: http.MethodGet, wantPath: "/library/sections/2/recentlyAdded", wantQuery: url.Values{},
		},
		{
			name: "mark_watched", verb: "mark_watched", opts: map[string]any{"rating_key": "999"},
			wantMethod: http.MethodGet, wantPath: "/:/scrobble",
			wantQuery: url.Values{"identifier": {"com.plexapp.plugins.library"}, "key": {"999"}},
		},
		{
			name: "mark_unwatched", verb: "mark_unwatched", opts: map[string]any{"rating_key": "999"},
			wantMethod: http.MethodGet, wantPath: "/:/unscrobble",
			wantQuery: url.Values{"identifier": {"com.plexapp.plugins.library"}, "key": {"999"}},
		},
		{
			name: "refresh_metadata", verb: "refresh_metadata", opts: map[string]any{"rating_key": "42"},
			wantMethod: http.MethodPut, wantPath: "/library/metadata/42/refresh", wantQuery: url.Values{},
		},
		{
			name: "identity", verb: "identity", opts: map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/identity", wantQuery: url.Values{},
		},
		{
			name: "playlists", verb: "playlists", opts: map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/playlists", wantQuery: url.Values{},
		},
		{
			name: "api default method", verb: "api", opts: map[string]any{"path": "/library/sections/1/all"},
			wantMethod: http.MethodGet, wantPath: "/library/sections/1/all", wantQuery: url.Values{},
		},
		{
			name: "api explicit method + query + body", verb: "api",
			opts: map[string]any{
				"method": "post", "path": "/playlists",
				"query": map[string]any{"title": "favorites"},
				"body":  map[string]any{"smart": false},
			},
			wantMethod: http.MethodPost, wantPath: "/playlists",
			wantQuery: url.Values{"title": {"favorites"}},
			wantBody:  `{"smart":false}`,
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
			if !reflect.DeepEqual(rb.query, tc.wantQuery) {
				t.Errorf("query:\n got: %#v\nwant: %#v", rb.query, tc.wantQuery)
			}
			if tc.wantBody != "" && string(rb.body) != tc.wantBody {
				t.Errorf("body: got %q want %q", rb.body, tc.wantBody)
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
		{"scan_library", map[string]any{}},     // no section_id
		{"search", map[string]any{}},           // no query
		{"metadata", map[string]any{}},         // no rating_key
		{"mark_watched", map[string]any{}},     // no rating_key
		{"mark_unwatched", map[string]any{}},   // no rating_key
		{"refresh_metadata", map[string]any{}}, // no rating_key
		{"api", map[string]any{}},              // no path
		{"nope", map[string]any{}},             // unknown verb
	}
	for _, tc := range cases {
		if _, err := buildRequest(tc.verb, tc.opts); err == nil {
			t.Errorf("buildRequest(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// --- unwrapContainer -------------------------------------------------------

func TestUnwrapContainer(t *testing.T) {
	t.Run("list child becomes items", func(t *testing.T) {
		raw := []byte(`{"MediaContainer":{"size":2,"Video":[{"title":"A"},{"title":"B"}]}}`)
		result, items, err := unwrapContainer(raw)
		if err != nil {
			t.Fatal(err)
		}
		if result["size"] != float64(2) {
			t.Errorf("result: %#v", result)
		}
		if len(items) != 2 {
			t.Errorf("items: %#v", items)
		}
	})

	t.Run("no list child yields empty items", func(t *testing.T) {
		raw := []byte(`{"MediaContainer":{"machineIdentifier":"abc123","version":"1.0"}}`)
		result, items, err := unwrapContainer(raw)
		if err != nil {
			t.Fatal(err)
		}
		if result["machineIdentifier"] != "abc123" {
			t.Errorf("result: %#v", result)
		}
		if len(items) != 0 {
			t.Errorf("items: %#v", items)
		}
	})

	t.Run("empty body yields empty result/items with no error", func(t *testing.T) {
		result, items, err := unwrapContainer([]byte("  "))
		if err != nil {
			t.Fatal(err)
		}
		if len(result) != 0 || len(items) != 0 {
			t.Errorf("result/items: %#v %#v", result, items)
		}
	})

	t.Run("invalid JSON is an error", func(t *testing.T) {
		if _, _, err := unwrapContainer([]byte("not json")); err == nil {
			t.Fatal("expected an error")
		}
	})
}

// --- Invoke ----------------------------------------------------------------

// newFakePlex starts an httptest.Server standing in for a Plex Media Server,
// asserting the request carries X-Plex-Token and Accept: application/json,
// and the expected method + path, then returns the given JSON body.
func newFakePlex(t *testing.T, wantMethod, wantPath string, checkQuery func(t *testing.T, q url.Values), body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != wantMethod {
			t.Errorf("method: got %q want %q", r.Method, wantMethod)
		}
		if r.URL.Path != wantPath {
			t.Errorf("path: got %q want %q", r.URL.Path, wantPath)
		}
		if r.Header.Get("X-Plex-Token") != "test-token" {
			t.Errorf("X-Plex-Token: got %q", r.Header.Get("X-Plex-Token"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept: got %q", r.Header.Get("Accept"))
		}
		if checkQuery != nil {
			checkQuery(t, r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestInvokeSessionsItems(t *testing.T) {
	srv := newFakePlex(t, http.MethodGet, "/status/sessions", nil,
		`{"MediaContainer":{"size":1,"Video":[{"title":"Session A"}]}}`)
	defer srv.Close()

	p := plexPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "sessions",
		Connection: map[string]any{"base_url": srv.URL, "token": "test-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeMetadataResultWithRatingKeyInPath(t *testing.T) {
	srv := newFakePlex(t, http.MethodGet, "/library/metadata/12345", nil,
		`{"MediaContainer":{"size":1,"Metadata":[{"title":"Some Movie","ratingKey":"12345"}]}}`)
	defer srv.Close()

	p := plexPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "metadata",
		Connection: map[string]any{"base_url": srv.URL, "token": "test-token"},
		Options:    map[string]any{"rating_key": "12345"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["size"] != float64(1) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeScanLibraryPathIncludesSectionID(t *testing.T) {
	srv := newFakePlex(t, http.MethodGet, "/library/sections/7/refresh", nil, "")
	defer srv.Close()

	p := plexPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "scan_library",
		Connection: map[string]any{"base_url": srv.URL, "token": "test-token"},
		Options:    map[string]any{"section_id": "7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs)
	}
}

func TestInvokeMarkWatchedQueryParams(t *testing.T) {
	srv := newFakePlex(t, http.MethodGet, "/:/scrobble", func(t *testing.T, q url.Values) {
		if q.Get("identifier") != "com.plexapp.plugins.library" {
			t.Errorf("identifier: %q", q.Get("identifier"))
		}
		if q.Get("key") != "555" {
			t.Errorf("key: %q", q.Get("key"))
		}
	}, "")
	defer srv.Close()

	p := plexPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "mark_watched",
		Connection: map[string]any{"base_url": srv.URL, "token": "test-token"},
		Options:    map[string]any{"rating_key": "555"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInvokeIdentityResult(t *testing.T) {
	srv := newFakePlex(t, http.MethodGet, "/identity", nil,
		`{"MediaContainer":{"machineIdentifier":"abc123"}}`)
	defer srv.Close()

	p := plexPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "identity",
		Connection: map[string]any{"base_url": srv.URL, "token": "test-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["machineIdentifier"] != "abc123" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if items, ok := res.Outputs["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("items should be empty: %#v", res.Outputs["items"])
	}
}

func TestInvokeNon2xxSurfacesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Invalid token"))
	}))
	defer srv.Close()

	p := plexPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "sessions",
		Connection: map[string]any{"base_url": srv.URL, "token": "bad-token"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "401") || !strings.Contains(pe.Message, "Invalid token") {
		t.Fatalf("error message should carry status + body: %q", pe.Message)
	}
}

func TestInvokeMissingConnection(t *testing.T) {
	p := plexPlugin{}
	cases := []map[string]any{
		{},
		{"base_url": "http://plex:32400"},
		{"token": "tok"},
	}
	for _, conn := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "sessions", Connection: conn})
		if err == nil {
			t.Fatalf("connection %#v: expected error", conn)
		}
		var pe *plugin.Error
		if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
			t.Fatalf("connection %#v: want CodeInvalidParams, got %v", conn, err)
		}
	}
}

func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}

// --- webhook source ----------------------------------------------------------

const samplePlexWebhookPayload = `{
	"event": "media.play",
	"user": true,
	"owner": true,
	"Account": {"id": 1, "thumb": "http://example/thumb", "title": "alice"},
	"Server": {"title": "myplex", "uuid": "abc-server"},
	"Player": {"local": true, "publicAddress": "1.2.3.4", "title": "Living Room", "uuid": "abc-player"},
	"Metadata": {
		"librarySectionType": "movie",
		"ratingKey": "12345",
		"key": "/library/metadata/12345",
		"title": "Some Movie",
		"type": "movie",
		"librarySectionTitle": "Movies"
	}
}`

func TestParseWebhook(t *testing.T) {
	f, err := parseWebhook([]byte(samplePlexWebhookPayload))
	if err != nil {
		t.Fatal(err)
	}
	if f.Event != "media.play" {
		t.Errorf("Event: %q", f.Event)
	}
	if f.Account != "alice" {
		t.Errorf("Account: %q", f.Account)
	}
	if f.Player != "Living Room" {
		t.Errorf("Player: %q", f.Player)
	}
	if f.Server != "myplex" {
		t.Errorf("Server: %q", f.Server)
	}
	if f.MediaType != "movie" {
		t.Errorf("MediaType: %q", f.MediaType)
	}
	if f.Title != "Some Movie" {
		t.Errorf("Title: %q", f.Title)
	}
	if f.Library != "Movies" {
		t.Errorf("Library: %q", f.Library)
	}
	if f.RatingKey != "12345" {
		t.Errorf("RatingKey: %q", f.RatingKey)
	}
}

// TestHandleWebhookPayloadEmitsNormalizedEvent proves parsing + context shape
// (including the filter-alias keys) for a typical Plex webhook delivery.
func TestHandleWebhookPayloadEmitsNormalizedEvent(t *testing.T) {
	restore := pinNow(t, time.Unix(1000, 0))
	defer restore()

	dedup := sourcekit.NewDedup(16)
	var got map[string]any
	handleWebhookPayload([]byte(samplePlexWebhookPayload), dedup, func(payload any) error {
		got, _ = payload.(map[string]any)
		return nil
	})
	if got == nil {
		t.Fatal("expected an emitted event")
	}
	if got["event"] != "playback" {
		t.Errorf("event: %#v", got["event"])
	}
	if got["kind"] != "media.play" {
		t.Errorf("kind: %#v", got["kind"])
	}
	ctx, ok := got["context"].(map[string]any)
	if !ok {
		t.Fatalf("context: %#v", got["context"])
	}
	wantCtx := map[string]any{
		"event": "media.play", "account": "alice", "player": "Living Room", "server": "myplex",
		"media_type": "movie", "title": "Some Movie", "library": "Movies", "rating_key": "12345",
		"events": "media.play", "media_types": "movie", "accounts": "alice",
	}
	for k, want := range wantCtx {
		if ctx[k] != want {
			t.Errorf("context[%q]: got %#v want %#v", k, ctx[k], want)
		}
	}
}

// TestHandleWebhookPayloadDedups proves a redelivered notification (same
// event + rating_key within the same second) is dropped the second time.
func TestHandleWebhookPayloadDedups(t *testing.T) {
	restore := pinNow(t, time.Unix(2000, 0))
	defer restore()

	dedup := sourcekit.NewDedup(16)
	n := 0
	emit := func(payload any) error { n++; return nil }
	handleWebhookPayload([]byte(samplePlexWebhookPayload), dedup, emit)
	handleWebhookPayload([]byte(samplePlexWebhookPayload), dedup, emit)
	if n != 1 {
		t.Fatalf("expected exactly one emission, got %d", n)
	}
}

// TestHandleWebhookPayloadDropsUnparseable proves a payload with no event
// name (or invalid JSON) is dropped rather than emitted blind.
func TestHandleWebhookPayloadDropsUnparseable(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	emitted := false
	emit := func(payload any) error { emitted = true; return nil }

	handleWebhookPayload([]byte(`{"Metadata":{"title":"no event field"}}`), dedup, emit)
	handleWebhookPayload([]byte(`not json`), dedup, emit)
	if emitted {
		t.Fatal("expected the payload to be dropped")
	}
}

func pinNow(t *testing.T, ts time.Time) func() {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time { return ts }
	return func() { nowFunc = prev }
}

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

// buildMultipartPayload builds a multipart/form-data body with a single
// `payload` field, mirroring what Plex's webhook feature actually POSTs.
func buildMultipartPayload(t *testing.T, payload string) (body *bytes.Buffer, contentType string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if err := mw.WriteField("payload", payload); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf, mw.FormDataContentType()
}

// TestStartSourceWebhookTokenAcceptReject drives StartSource end to end
// against a real (loopback) listener with a multipart/form-data body: a
// request with the correct token is accepted and emits an event; a request
// with a missing/wrong token is rejected with 401 and nothing is emitted.
func TestStartSourceWebhookTokenAcceptReject(t *testing.T) {
	restore := pinNow(t, time.Unix(3000, 0))
	defer restore()

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
	p := plexPlugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.StartSource(ctx, plugin.StartSourceRequest{
			Instance: "test",
			Config: map[string]any{
				"webhook": map[string]any{
					"listen": addr,
					"path":   "/plex",
					"secret": "s3cret",
				},
			},
		}, emit)
	}()
	waitForListener(t, addr)

	body, contentType := buildMultipartPayload(t, samplePlexWebhookPayload)
	resp := postMultipart(t, addr, "/plex?token=nope", body.Bytes(), contentType)
	if resp != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", resp)
	}
	select {
	case ev := <-events:
		t.Fatalf("expected no emission for the wrong token, got %#v", ev)
	default:
	}

	body, contentType = buildMultipartPayload(t, samplePlexWebhookPayload)
	resp = postMultipart(t, addr, "/plex?token=s3cret", body.Bytes(), contentType)
	if resp != http.StatusAccepted {
		t.Fatalf("correct token: status = %d, want 202", resp)
	}
	select {
	case ev := <-events:
		if ev["kind"] != "media.play" {
			t.Fatalf("emitted event: %#v", ev)
		}
	default:
		t.Fatal("expected an emitted event for the correctly-authenticated request")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("StartSource: %v", err)
	}
}

func postMultipart(t *testing.T, addr, path string, body []byte, contentType string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
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

// --- Describe ----------------------------------------------------------------

func TestDescribe(t *testing.T) {
	d := plexPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "plex" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("capabilities: must not spawn/command: %#v", d.Capabilities)
	}
	if d.Capabilities.Egress == nil || len(d.Capabilities.Egress) != 0 {
		t.Fatalf("capabilities.egress: want declared-empty ([]string{}), got %#v", d.Capabilities.Egress)
	}
	want := []string{
		"sessions", "library_sections", "scan_library", "search", "metadata",
		"recently_added", "mark_watched", "mark_unwatched", "refresh_metadata",
		"identity", "playlists", "api",
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
	if len(d.Events) != 1 || d.Events[0].Name != "playback" {
		t.Fatalf("Describe events: %#v", d.Events)
	}
}
