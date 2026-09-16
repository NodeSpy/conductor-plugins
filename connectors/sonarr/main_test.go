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
// the whole contract with the Sonarr API, proven without spawning anything.
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
			name:       "series",
			verb:       "series",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/series",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name:       "series_get",
			verb:       "series_get",
			opts:       map[string]any{"id": 42},
			wantMethod: http.MethodGet,
			wantPath:   "/series/42",
			wantQuery:  url.Values{},
		},
		{
			name:       "lookup",
			verb:       "lookup",
			opts:       map[string]any{"term": "breaking bad"},
			wantMethod: http.MethodGet,
			wantPath:   "/series/lookup",
			wantQuery:  url.Values{"term": {"breaking bad"}},
			wantAsList: true,
		},
		{
			name: "add_series built fields",
			verb: "add_series",
			opts: map[string]any{
				"tvdb_id": 81189, "quality_profile_id": 1, "root_folder_path": "/tv",
				"season_folder": true, "search_for_missing": true,
			},
			wantMethod: http.MethodPost,
			wantPath:   "/series",
			wantQuery:  url.Values{},
			wantBody: map[string]any{
				"tvdbId": int64(81189), "qualityProfileId": int64(1), "rootFolderPath": "/tv",
				"monitored": true, "seasonFolder": true,
				"addOptions": map[string]any{"searchForMissingEpisodes": true},
			},
		},
		{
			name: "add_series monitored false default missing",
			verb: "add_series",
			opts: map[string]any{
				"tvdb_id": 1, "quality_profile_id": 2, "root_folder_path": "/tv", "monitored": false,
			},
			wantMethod: http.MethodPost,
			wantPath:   "/series",
			wantQuery:  url.Values{},
			wantBody: map[string]any{
				"tvdbId": int64(1), "qualityProfileId": int64(2), "rootFolderPath": "/tv",
				"monitored":  false,
				"addOptions": map[string]any{"searchForMissingEpisodes": false},
			},
		},
		{
			name: "add_series full passthrough",
			verb: "add_series",
			opts: map[string]any{
				"series": map[string]any{"title": "Foo", "tvdbId": float64(1)},
				// Ignored when `series` is present.
				"tvdb_id": 999,
			},
			wantMethod: http.MethodPost,
			wantPath:   "/series",
			wantQuery:  url.Values{},
			wantBody:   map[string]any{"title": "Foo", "tvdbId": float64(1)},
		},
		{
			name:       "delete_series",
			verb:       "delete_series",
			opts:       map[string]any{"id": 7, "delete_files": true, "add_import_exclusion": true},
			wantMethod: http.MethodDelete,
			wantPath:   "/series/7",
			wantQuery:  url.Values{"deleteFiles": {"true"}, "addImportListExclusion": {"true"}},
		},
		{
			name:       "delete_series minimal",
			verb:       "delete_series",
			opts:       map[string]any{"id": 7},
			wantMethod: http.MethodDelete,
			wantPath:   "/series/7",
			wantQuery:  url.Values{},
		},
		{
			name:       "episodes",
			verb:       "episodes",
			opts:       map[string]any{"series_id": 3},
			wantMethod: http.MethodGet,
			wantPath:   "/episode",
			wantQuery:  url.Values{"seriesId": {"3"}},
			wantAsList: true,
		},
		{
			name:       "episode_get",
			verb:       "episode_get",
			opts:       map[string]any{"id": 99},
			wantMethod: http.MethodGet,
			wantPath:   "/episode/99",
			wantQuery:  url.Values{},
		},
		{
			name: "command with convenience fields",
			verb: "command",
			opts: map[string]any{
				"name": "SeasonSearch", "series_id": 5, "season_number": 2,
				"episode_ids": []any{float64(10), float64(11)},
			},
			wantMethod: http.MethodPost,
			wantPath:   "/command",
			wantQuery:  url.Values{},
			wantBody: map[string]any{
				"name": "SeasonSearch", "seriesId": int64(5), "seasonNumber": int64(2),
				"episodeIds": []any{float64(10), float64(11)},
			},
		},
		{
			name: "command with extra params merged",
			verb: "command",
			opts: map[string]any{
				"name": "RefreshSeries", "params": map[string]any{"seriesId": float64(9)},
			},
			wantMethod: http.MethodPost,
			wantPath:   "/command",
			wantQuery:  url.Values{},
			wantBody:   map[string]any{"name": "RefreshSeries", "seriesId": float64(9)},
		},
		{
			name:       "queue",
			verb:       "queue",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/queue",
			wantQuery:  url.Values{},
		},
		{
			name:       "calendar",
			verb:       "calendar",
			opts:       map[string]any{"start": "2024-01-01", "end": "2024-01-31"},
			wantMethod: http.MethodGet,
			wantPath:   "/calendar",
			wantQuery:  url.Values{"start": {"2024-01-01"}, "end": {"2024-01-31"}},
			wantAsList: true,
		},
		{
			name:       "wanted_missing",
			verb:       "wanted_missing",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/wanted/missing",
			wantQuery:  url.Values{},
		},
		{
			name:       "quality_profiles",
			verb:       "quality_profiles",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/qualityprofile",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name:       "root_folders",
			verb:       "root_folders",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/rootfolder",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name:       "health",
			verb:       "health",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/health",
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
				"method": "put", "path": "/series/editor", "query": map[string]any{"x": "1"},
				"body": map[string]any{"a": "b"},
			},
			wantMethod: http.MethodPut,
			wantPath:   "/series/editor",
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
		{"series_get", map[string]any{}},                                      // no id
		{"lookup", map[string]any{}},                                          // no term
		{"add_series", map[string]any{}},                                      // no tvdb_id/quality_profile_id/root_folder_path/series
		{"add_series", map[string]any{"tvdb_id": 1}},                          // no quality_profile_id/root_folder_path
		{"add_series", map[string]any{"tvdb_id": 1, "quality_profile_id": 1}}, // no root_folder_path
		{"delete_series", map[string]any{}},                                   // no id
		{"episodes", map[string]any{}},                                        // no series_id
		{"episode_get", map[string]any{}},                                     // no id
		{"command", map[string]any{}},                                         // no name
		{"api", map[string]any{}},                                             // no path
		{"nope", map[string]any{}},                                            // unknown verb
	}
	for _, tc := range cases {
		if _, err := buildRequest(tc.verb, tc.opts); err == nil {
			t.Errorf("buildRequest(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// newFakeSonarr starts an httptest.Server standing in for Sonarr, asserting
// every request carries the X-Api-Key header and hits /api/v3, then delegates
// to check for verb-specific assertions and returns the given JSON body.
func newFakeSonarr(t *testing.T, wantMethod, wantPath string, check func(t *testing.T, r *http.Request), status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("X-Api-Key: got %q want test-key", r.Header.Get("X-Api-Key"))
		}
		if r.Method != wantMethod {
			t.Errorf("method: got %q want %q", r.Method, wantMethod)
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v3") {
			t.Errorf("path: got %q want prefix /api/v3", r.URL.Path)
		}
		if wantPath != "" && r.URL.Path != "/api/v3"+wantPath {
			t.Errorf("path: got %q want %q", r.URL.Path, "/api/v3"+wantPath)
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

func TestInvokeSeriesItems(t *testing.T) {
	srv := newFakeSonarr(t, http.MethodGet, "/series", nil, 0,
		`[{"id":1,"title":"Foo"},{"id":2,"title":"Bar"}]`)
	defer srv.Close()

	p := sonarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "series",
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

func TestInvokeSeriesGetResult(t *testing.T) {
	srv := newFakeSonarr(t, http.MethodGet, "/series/42", nil, 0, `{"id":42,"title":"Foo"}`)
	defer srv.Close()

	p := sonarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "series_get",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"id": 42},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["title"] != "Foo" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeAddSeriesJSONBody(t *testing.T) {
	var gotBody map[string]any
	srv := newFakeSonarr(t, http.MethodPost, "/series", func(t *testing.T, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
	}, 0, `{"id":1,"title":"Foo"}`)
	defer srv.Close()

	p := sonarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "add_series",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options: map[string]any{
			"tvdb_id": 81189, "quality_profile_id": 1, "root_folder_path": "/tv",
			"search_for_missing": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["tvdbId"] != float64(81189) || gotBody["rootFolderPath"] != "/tv" {
		t.Fatalf("posted body: %#v", gotBody)
	}
	addOptions, ok := gotBody["addOptions"].(map[string]any)
	if !ok || addOptions["searchForMissingEpisodes"] != true {
		t.Fatalf("addOptions: %#v", gotBody["addOptions"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["title"] != "Foo" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeCommandJSONBody(t *testing.T) {
	var gotBody map[string]any
	srv := newFakeSonarr(t, http.MethodPost, "/command", func(t *testing.T, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
	}, 0, `{"id":10,"name":"SeasonSearch","status":"queued"}`)
	defer srv.Close()

	p := sonarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "command",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"name": "SeasonSearch", "series_id": 5, "season_number": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["name"] != "SeasonSearch" || gotBody["seriesId"] != float64(5) || gotBody["seasonNumber"] != float64(1) {
		t.Fatalf("posted body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "queued" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeDeleteSeriesQueryParams(t *testing.T) {
	srv := newFakeSonarr(t, http.MethodDelete, "/series/7", func(t *testing.T, r *http.Request) {
		if r.URL.Query().Get("deleteFiles") != "true" {
			t.Errorf("deleteFiles: %q", r.URL.Query().Get("deleteFiles"))
		}
		if r.URL.Query().Get("addImportListExclusion") != "true" {
			t.Errorf("addImportListExclusion: %q", r.URL.Query().Get("addImportListExclusion"))
		}
	}, http.StatusOK, ``)
	defer srv.Close()

	p := sonarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "delete_series",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"id": 7, "delete_files": true, "add_import_exclusion": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != http.StatusOK {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestInvokeLookupItems(t *testing.T) {
	srv := newFakeSonarr(t, http.MethodGet, "/series/lookup", func(t *testing.T, r *http.Request) {
		if r.URL.Query().Get("term") != "breaking bad" {
			t.Errorf("term: %q", r.URL.Query().Get("term"))
		}
	}, 0, `[{"title":"Breaking Bad","tvdbId":81189}]`)
	defer srv.Close()

	p := sonarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "lookup",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"term": "breaking bad"},
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
	srv := newFakeSonarr(t, http.MethodGet, "/system/status", nil, 0, `[{"a":1}]`)
	defer srv.Close()

	p := sonarrPlugin{}
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

	p := sonarrPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "series",
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
	p := sonarrPlugin{}
	cases := []map[string]any{
		{},
		{"base_url": "http://sonarr:8989"},
		{"api_key": "k"},
	}
	for _, conn := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "series", Connection: conn})
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

// TestHandleWebhookBodyGrabEvent proves parsing + context shape for a
// typical Sonarr Grab webhook delivery.
func TestHandleWebhookBodyGrabEvent(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{
		"eventType": "Grab",
		"series": {"title": "Breaking Bad", "tvdbId": 81189},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1}],
		"release": {"quality": {"quality": {"name": "HDTV-720p"}}},
		"downloadId": "abc123"
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
	if got["kind"] != "Grab" {
		t.Errorf("kind: %#v", got["kind"])
	}
	ctx, ok := got["context"].(map[string]any)
	if !ok {
		t.Fatalf("context: %#v", got["context"])
	}
	if ctx["event_type"] != "Grab" || ctx["series_title"] != "Breaking Bad" || ctx["tvdb_id"] != int64(81189) {
		t.Fatalf("context fields: %#v", ctx)
	}
	if ctx["quality"] != "HDTV-720p" {
		t.Errorf("quality: %#v", ctx["quality"])
	}
	if ctx["event_types"] != "Grab" || ctx["series"] != "Breaking Bad" {
		t.Errorf("filter aliases: %#v", ctx)
	}
	episodes, ok := ctx["episodes"].([]any)
	if !ok || len(episodes) != 1 {
		t.Fatalf("episodes: %#v", ctx["episodes"])
	}
	payload, ok := ctx["payload"].(map[string]any)
	if !ok || payload["eventType"] != "Grab" {
		t.Fatalf("context.payload: %#v", ctx["payload"])
	}
}

// TestHandleWebhookBodyDownloadEvent proves a Download event (episodeFile
// quality, no release) also extracts quality correctly.
func TestHandleWebhookBodyDownloadEvent(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{
		"eventType": "Download",
		"series": {"title": "Foo", "tvdbId": 1},
		"episodeFile": {"quality": {"quality": {"name": "WEBDL-1080p"}}},
		"downloadId": "xyz"
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
	if ctx["quality"] != "WEBDL-1080p" {
		t.Errorf("quality: %#v", ctx["quality"])
	}
}

// TestHandleWebhookBodyDedups proves a redelivered notification (same
// eventType + tvdbId + downloadId) is dropped the second time.
func TestHandleWebhookBodyDedups(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{"eventType":"Grab","series":{"title":"Foo","tvdbId":1},"downloadId":"same"}`)

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
	handleWebhookBody([]byte(`{"series":{"title":"Foo"}}`), dedup, func(payload any) error {
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
	p := sonarrPlugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.StartSource(ctx, plugin.StartSourceRequest{
			Instance: "test",
			Config: map[string]any{
				"webhook": map[string]any{
					"listen": addr,
					"path":   "/sonarr",
					"secret": "s3cret",
				},
			},
		}, emit)
	}()
	waitForListener(t, addr)

	body := []byte(`{"eventType":"Grab","series":{"title":"Foo","tvdbId":1},"downloadId":"a"}`)

	// Wrong token: nothing emitted.
	postJSON(t, addr, "/sonarr?token=nope", body, nil)
	select {
	case ev := <-events:
		t.Fatalf("wrong token should not emit, got %#v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	// Correct token via header: accepted and emits.
	postJSON(t, addr, "/sonarr", body, map[string]string{"X-Conductor-Token": "s3cret"})
	select {
	case ev := <-events:
		if ev["kind"] != "Grab" {
			t.Fatalf("emitted event: %#v", ev)
		}
	default:
		t.Fatal("expected an emitted event for the correctly-authenticated request")
	}

	// Correct token via query param, different downloadId so dedup doesn't hide it.
	body2 := []byte(`{"eventType":"Grab","series":{"title":"Foo","tvdbId":1},"downloadId":"b"}`)
	postJSON(t, addr, "/sonarr?token=s3cret", body2, nil)
	select {
	case ev := <-events:
		if ev["dedup"] != "Grab\x001\x00b" {
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
	d := sonarrPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "sonarr" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("capabilities: must not spawn/command: %#v", d.Capabilities)
	}
	if d.Capabilities.Egress == nil || len(d.Capabilities.Egress) != 0 {
		t.Fatalf("capabilities.egress: want declared-empty ([]string{}), got %#v", d.Capabilities.Egress)
	}
	want := []string{
		"series", "series_get", "lookup", "add_series", "delete_series",
		"episodes", "episode_get", "command", "queue", "calendar",
		"wanted_missing", "quality_profiles", "root_folders", "health", "api",
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
