package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// TestBuildRequest pins the exact method/path/query/body each verb builds —
// the whole contract with the Radarr API, proven without spawning anything.
func TestBuildRequest(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		wantMethod string
		wantPath   string
		wantQuery  url.Values
		wantBody   any
		wantList   bool
	}{
		{
			name:       "movies",
			verb:       "movies",
			opts:       map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/movie", wantQuery: url.Values{}, wantList: true,
		},
		{
			name:       "movie_get",
			verb:       "movie_get",
			opts:       map[string]any{"id": 42},
			wantMethod: http.MethodGet, wantPath: "/movie/42", wantQuery: url.Values{},
		},
		{
			name:       "lookup",
			verb:       "lookup",
			opts:       map[string]any{"term": "tmdb:603"},
			wantMethod: http.MethodGet, wantPath: "/movie/lookup", wantQuery: url.Values{"term": {"tmdb:603"}}, wantList: true,
		},
		{
			name: "add_movie built fields",
			verb: "add_movie",
			opts: map[string]any{
				"tmdb_id": 603, "quality_profile_id": 4, "root_folder_path": "/movies",
				"search_for_movie": true,
			},
			wantMethod: http.MethodPost, wantPath: "/movie", wantQuery: url.Values{},
			wantBody: map[string]any{
				"tmdbId": int64(603), "qualityProfileId": int64(4), "rootFolderPath": "/movies",
				"monitored": true, "minimumAvailability": "released",
				"addOptions": map[string]any{"searchForMovie": true},
			},
		},
		{
			name: "add_movie monitored false and availability override",
			verb: "add_movie",
			opts: map[string]any{
				"tmdb_id": 603, "quality_profile_id": 4, "root_folder_path": "/movies",
				"monitored": false, "minimum_availability": "announced",
			},
			wantMethod: http.MethodPost, wantPath: "/movie", wantQuery: url.Values{},
			wantBody: map[string]any{
				"tmdbId": int64(603), "qualityProfileId": int64(4), "rootFolderPath": "/movies",
				"monitored": false, "minimumAvailability": "announced",
				"addOptions": map[string]any{"searchForMovie": false},
			},
		},
		{
			name:       "add_movie full passthrough",
			verb:       "add_movie",
			opts:       map[string]any{"movie": map[string]any{"tmdbId": float64(603), "title": "The Matrix"}},
			wantMethod: http.MethodPost, wantPath: "/movie", wantQuery: url.Values{},
			wantBody: map[string]any{"tmdbId": float64(603), "title": "The Matrix"},
		},
		{
			name:       "delete_movie plain",
			verb:       "delete_movie",
			opts:       map[string]any{"id": 7},
			wantMethod: http.MethodDelete, wantPath: "/movie/7", wantQuery: url.Values{},
		},
		{
			name:       "delete_movie with files and exclusion",
			verb:       "delete_movie",
			opts:       map[string]any{"id": 7, "delete_files": true, "add_import_exclusion": true},
			wantMethod: http.MethodDelete, wantPath: "/movie/7",
			wantQuery: url.Values{"deleteFiles": {"true"}, "addImportExclusion": {"true"}},
		},
		{
			name:       "command with movie_ids",
			verb:       "command",
			opts:       map[string]any{"name": "MoviesSearch", "movie_ids": []any{1, 2, 3}},
			wantMethod: http.MethodPost, wantPath: "/command", wantQuery: url.Values{},
			wantBody: map[string]any{"name": "MoviesSearch", "movieIds": []any{1, 2, 3}},
		},
		{
			name:       "command with extra params",
			verb:       "command",
			opts:       map[string]any{"name": "RefreshMovie", "params": map[string]any{"movieId": 5}},
			wantMethod: http.MethodPost, wantPath: "/command", wantQuery: url.Values{},
			wantBody: map[string]any{"name": "RefreshMovie", "movieId": 5},
		},
		{
			name:       "queue",
			verb:       "queue",
			opts:       map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/queue", wantQuery: url.Values{},
		},
		{
			name:       "calendar",
			verb:       "calendar",
			opts:       map[string]any{"start": "2026-01-01", "end": "2026-01-31"},
			wantMethod: http.MethodGet, wantPath: "/calendar",
			wantQuery: url.Values{"start": {"2026-01-01"}, "end": {"2026-01-31"}}, wantList: true,
		},
		{
			name:       "wanted_missing",
			verb:       "wanted_missing",
			opts:       map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/wanted/missing", wantQuery: url.Values{},
		},
		{
			name:       "quality_profiles",
			verb:       "quality_profiles",
			opts:       map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/qualityprofile", wantQuery: url.Values{}, wantList: true,
		},
		{
			name:       "root_folders",
			verb:       "root_folders",
			opts:       map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/rootfolder", wantQuery: url.Values{}, wantList: true,
		},
		{
			name:       "health",
			verb:       "health",
			opts:       map[string]any{},
			wantMethod: http.MethodGet, wantPath: "/health", wantQuery: url.Values{}, wantList: true,
		},
		{
			name:       "api escape hatch",
			verb:       "api",
			opts:       map[string]any{"path": "system/status", "query": map[string]any{"x": "1"}},
			wantMethod: http.MethodGet, wantPath: "/system/status", wantQuery: url.Values{"x": {"1"}},
		},
		{
			name:       "api escape hatch POST with body",
			verb:       "api",
			opts:       map[string]any{"method": "post", "path": "/command", "body": map[string]any{"name": "Backup"}},
			wantMethod: http.MethodPost, wantPath: "/command", wantQuery: url.Values{},
			wantBody: map[string]any{"name": "Backup"},
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
			if tc.wantBody != nil && !reflect.DeepEqual(rb.body, tc.wantBody) {
				t.Errorf("body:\n got: %#v\nwant: %#v", rb.body, tc.wantBody)
			}
			if rb.asList != tc.wantList {
				t.Errorf("asList: got %v want %v", rb.asList, tc.wantList)
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
		{"movie_get", map[string]any{}},                                      // no id
		{"lookup", map[string]any{}},                                         // no term
		{"add_movie", map[string]any{}},                                      // no tmdb_id
		{"add_movie", map[string]any{"tmdb_id": 1}},                          // no quality_profile_id
		{"add_movie", map[string]any{"tmdb_id": 1, "quality_profile_id": 1}}, // no root_folder_path
		{"delete_movie", map[string]any{}},                                   // no id
		{"command", map[string]any{}},                                        // no name
		{"api", map[string]any{}},                                            // no path
		{"nope", map[string]any{}},                                           // unknown verb
	}
	for _, tc := range cases {
		if _, err := buildRequest(tc.verb, tc.opts); err == nil {
			t.Errorf("buildRequest(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// newFakeRadarr starts an httptest.Server standing in for Radarr, asserting
// the request carries X-Api-Key and the expected method/path, and returning
// the given status/body.
func newFakeRadarr(t *testing.T, wantMethod, wantPath string, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("X-Api-Key: got %q want test-key", r.Header.Get("X-Api-Key"))
		}
		if r.Method != wantMethod {
			t.Errorf("method: got %q want %q", r.Method, wantMethod)
		}
		if r.URL.Path != wantPath {
			t.Errorf("path: got %q want %q", r.URL.Path, wantPath)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestInvokeMoviesHoistsItems(t *testing.T) {
	srv := newFakeRadarr(t, http.MethodGet, "/api/v3/movie", 200, `[{"id":1,"title":"The Matrix"},{"id":2,"title":"Inception"}]`)
	defer srv.Close()

	p := radarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "movies",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeMovieGetResult(t *testing.T) {
	srv := newFakeRadarr(t, http.MethodGet, "/api/v3/movie/42", 200, `{"id":42,"title":"The Matrix"}`)
	defer srv.Close()

	p := radarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "movie_get",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"id": 42},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["title"] != "The Matrix" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeAddMovieBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/movie" || r.Method != http.MethodPost {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type: %q", ct)
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		_ = json.Unmarshal(buf.Bytes(), &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":10}`))
	}))
	defer srv.Close()

	p := radarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "add_movie",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options: map[string]any{
			"tmdb_id": 603, "quality_profile_id": 4, "root_folder_path": "/movies",
			"search_for_movie": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["tmdbId"] != float64(603) || gotBody["rootFolderPath"] != "/movies" {
		t.Fatalf("posted body: %#v", gotBody)
	}
	addOpts, ok := gotBody["addOptions"].(map[string]any)
	if !ok || addOpts["searchForMovie"] != true {
		t.Fatalf("addOptions: %#v", gotBody["addOptions"])
	}
	if res.Outputs["result"] == nil {
		t.Fatal("result should be set")
	}
}

func TestInvokeCommandBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/command" {
			t.Errorf("path: %q", r.URL.Path)
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		_ = json.Unmarshal(buf.Bytes(), &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":99,"status":"queued"}`))
	}))
	defer srv.Close()

	p := radarrPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "command",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"name": "MoviesSearch", "movie_ids": []any{1, 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["name"] != "MoviesSearch" {
		t.Fatalf("body: %#v", gotBody)
	}
}

func TestInvokeNon2xx(t *testing.T) {
	srv := newFakeRadarr(t, http.MethodGet, "/api/v3/queue", http.StatusUnauthorized, `{"message":"Api Key is invalid"}`)
	defer srv.Close()

	p := radarrPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "queue",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "401") || !strings.Contains(pe.Message, "Api Key is invalid") {
		t.Fatalf("error message should carry status + body: %q", pe.Message)
	}
}

func TestInvokeMissingConnection(t *testing.T) {
	p := radarrPlugin{}
	cases := []map[string]any{
		{},
		{"base_url": "http://radarr:7878"},
		{"api_key": "k"},
	}
	for _, conn := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "queue", Connection: conn})
		if err == nil {
			t.Fatalf("connection %#v: expected error", conn)
		}
		var pe *plugin.Error
		if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
			t.Fatalf("connection %#v: want CodeInvalidParams, got %v", conn, err)
		}
	}
}

// TestDescribe asserts the declared surface: kind, type, capabilities,
// events, and every verb.
func TestDescribe(t *testing.T) {
	d := radarrPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "radarr" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("capabilities: connector must not spawn/command: %#v", d.Capabilities)
	}
	if d.Capabilities.Egress == nil || len(d.Capabilities.Egress) != 0 {
		t.Fatalf("capabilities.egress: want declared-empty ([]string{}), got %#v", d.Capabilities.Egress)
	}
	want := []string{"movies", "movie_get", "lookup", "add_movie", "delete_movie", "command",
		"queue", "calendar", "wanted_missing", "quality_profiles", "root_folders", "health", "api"}
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

// --- source: webhook parsing + auth -----------------------------------------

func TestHandleWebhookBodyGrab(t *testing.T) {
	dedup := sourcekit.NewDedup(64)
	var got map[string]any
	emit := func(payload any) error {
		got = payload.(map[string]any)
		return nil
	}
	body := []byte(`{
		"eventType": "Grab",
		"movie": {"title": "The Matrix", "year": 1999, "tmdbId": 603},
		"release": {"quality": {"quality": {"name": "Bluray-1080p"}}},
		"downloadId": "abc123"
	}`)
	handleWebhookBody(body, dedup, emit)
	if got == nil {
		t.Fatal("expected an emitted event")
	}
	if got["event"] != "event" || got["kind"] != "Grab" {
		t.Fatalf("event/kind: %#v", got)
	}
	ctx := got["context"].(map[string]any)
	if ctx["event_type"] != "Grab" || ctx["movie_title"] != "The Matrix" || ctx["tmdb_id"] != int64(603) {
		t.Fatalf("context: %#v", ctx)
	}
	if ctx["year"] != int64(1999) {
		t.Fatalf("year: %#v", ctx["year"])
	}
	if ctx["quality"] != "Bluray-1080p" {
		t.Fatalf("quality: %#v", ctx["quality"])
	}
	if ctx["event_types"] != "Grab" || ctx["movies"] != "The Matrix" {
		t.Fatalf("filter aliases: %#v", ctx)
	}
}

func TestHandleWebhookBodyDownload(t *testing.T) {
	dedup := sourcekit.NewDedup(64)
	var got map[string]any
	emit := func(payload any) error {
		got = payload.(map[string]any)
		return nil
	}
	body := []byte(`{
		"eventType": "Download",
		"movie": {"title": "Inception", "year": 2010, "tmdbId": 27205},
		"movieFile": {"quality": {"quality": {"name": "WEBDL-1080p"}}}
	}`)
	handleWebhookBody(body, dedup, emit)
	if got == nil {
		t.Fatal("expected an emitted event")
	}
	ctx := got["context"].(map[string]any)
	if ctx["quality"] != "WEBDL-1080p" {
		t.Fatalf("quality: %#v", ctx["quality"])
	}
}

func TestHandleWebhookBodyMissingEventTypeDropped(t *testing.T) {
	dedup := sourcekit.NewDedup(64)
	called := false
	emit := func(payload any) error {
		called = true
		return nil
	}
	handleWebhookBody([]byte(`{"movie":{"title":"x"}}`), dedup, emit)
	if called {
		t.Fatal("expected no emit when eventType is missing")
	}
}

func TestHandleWebhookBodyDedup(t *testing.T) {
	dedup := sourcekit.NewDedup(64)
	count := 0
	emit := func(payload any) error {
		count++
		return nil
	}
	body := []byte(`{"eventType":"Grab","movie":{"tmdbId":603},"downloadId":"same-id"}`)
	handleWebhookBody(body, dedup, emit)
	handleWebhookBody(body, dedup, emit)
	if count != 1 {
		t.Fatalf("expected dedup to collapse redelivery, got %d emits", count)
	}
}

func TestRequireToken(t *testing.T) {
	if err := requireToken("", false); err == nil {
		t.Fatal("expected error for empty secret without allow_unsigned")
	}
	if err := requireToken("", true); err != nil {
		t.Fatalf("allow_unsigned should permit empty secret: %v", err)
	}
	if err := requireToken("shhh", false); err != nil {
		t.Fatalf("non-empty secret should be fine: %v", err)
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

func TestStartSourceTokenAuth(t *testing.T) {
	events := make(chan map[string]any, 4)
	emit := func(payload any) error {
		events <- payload.(map[string]any)
		return nil
	}

	p := radarrPlugin{}
	ctx, cancel := testContext(t)
	defer cancel()

	// Find a free port by letting net pick one via listen on :0 is not
	// directly supported by StartSource's addr string, so bind to a fixed
	// high port unlikely to collide within this test run.
	addr := "127.0.0.1:18917"
	req := plugin.StartSourceRequest{
		Instance: "test",
		Config: map[string]any{
			"webhook": map[string]any{"listen": addr, "path": "/radarr", "secret": "topsecret"},
		},
	}

	done := make(chan error, 1)
	go func() { done <- p.StartSource(ctx, req, emit) }()
	waitForListener(t, addr)

	// Wrong token: nothing emitted.
	resp, err := http.Post("http://"+addr+"/radarr?token=wrong", "application/json",
		bytes.NewReader([]byte(`{"eventType":"Test"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case ev := <-events:
		t.Fatalf("wrong token should not emit, got %#v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	// Correct token via query param: accepted.
	resp, err = http.Post("http://"+addr+"/radarr?token=topsecret", "application/json",
		bytes.NewReader([]byte(`{"eventType":"Test","movie":{"title":"X","tmdbId":1}}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case ev := <-events:
		if ev["kind"] != "Test" {
			t.Fatalf("event: %#v", ev)
		}
	default:
		t.Fatal("expected an emitted event for the authorized request")
	}

	cancel()
	<-done
}

func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}

// testContext returns a context cancelled when the test ends, plus its
// cancel func for early cancellation.
func testContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}

// waitForListener polls until addr accepts TCP connections, or fails the
// test after a short timeout.
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
	t.Fatalf("listener at %s did not come up in time", addr)
}
