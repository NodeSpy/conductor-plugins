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
// the whole contract with the Lidarr API, proven without spawning anything.
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
			name:       "artists",
			verb:       "artists",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "/artist",
			wantQuery:  url.Values{},
			wantAsList: true,
		},
		{
			name:       "artist_get",
			verb:       "artist_get",
			opts:       map[string]any{"id": 42},
			wantMethod: http.MethodGet,
			wantPath:   "/artist/42",
			wantQuery:  url.Values{},
		},
		{
			name:       "lookup",
			verb:       "lookup",
			opts:       map[string]any{"term": "the beatles"},
			wantMethod: http.MethodGet,
			wantPath:   "/artist/lookup",
			wantQuery:  url.Values{"term": {"the beatles"}},
			wantAsList: true,
		},
		{
			name: "add_artist built fields",
			verb: "add_artist",
			opts: map[string]any{
				"foreign_artist_id": "mbid-123", "quality_profile_id": 1, "root_folder_path": "/music",
				"search_for_missing": true,
			},
			wantMethod: http.MethodPost,
			wantPath:   "/artist",
			wantQuery:  url.Values{},
			wantBody: map[string]any{
				"foreignArtistId": "mbid-123", "qualityProfileId": int64(1), "rootFolderPath": "/music",
				"monitored":  true,
				"addOptions": map[string]any{"searchForMissingAlbums": true},
			},
		},
		{
			name: "add_artist monitored false default missing",
			verb: "add_artist",
			opts: map[string]any{
				"foreign_artist_id": "mbid-1", "quality_profile_id": 2, "root_folder_path": "/music", "monitored": false,
			},
			wantMethod: http.MethodPost,
			wantPath:   "/artist",
			wantQuery:  url.Values{},
			wantBody: map[string]any{
				"foreignArtistId": "mbid-1", "qualityProfileId": int64(2), "rootFolderPath": "/music",
				"monitored":  false,
				"addOptions": map[string]any{"searchForMissingAlbums": false},
			},
		},
		{
			name: "add_artist with metadata_profile_id",
			verb: "add_artist",
			opts: map[string]any{
				"foreign_artist_id": "mbid-2", "quality_profile_id": 1, "root_folder_path": "/music",
				"metadata_profile_id": 5,
			},
			wantMethod: http.MethodPost,
			wantPath:   "/artist",
			wantQuery:  url.Values{},
			wantBody: map[string]any{
				"foreignArtistId": "mbid-2", "qualityProfileId": int64(1), "rootFolderPath": "/music",
				"monitored": true, "metadataProfileId": int64(5),
				"addOptions": map[string]any{"searchForMissingAlbums": false},
			},
		},
		{
			name: "add_artist full passthrough",
			verb: "add_artist",
			opts: map[string]any{
				"artist": map[string]any{"artistName": "Foo", "foreignArtistId": "mbid-9"},
				// Ignored when `artist` is present.
				"foreign_artist_id": "should-be-ignored",
			},
			wantMethod: http.MethodPost,
			wantPath:   "/artist",
			wantQuery:  url.Values{},
			wantBody:   map[string]any{"artistName": "Foo", "foreignArtistId": "mbid-9"},
		},
		{
			name:       "delete_artist",
			verb:       "delete_artist",
			opts:       map[string]any{"id": 7, "delete_files": true, "add_import_exclusion": true},
			wantMethod: http.MethodDelete,
			wantPath:   "/artist/7",
			wantQuery:  url.Values{"deleteFiles": {"true"}, "addImportListExclusion": {"true"}},
		},
		{
			name:       "delete_artist minimal",
			verb:       "delete_artist",
			opts:       map[string]any{"id": 7},
			wantMethod: http.MethodDelete,
			wantPath:   "/artist/7",
			wantQuery:  url.Values{},
		},
		{
			name:       "albums",
			verb:       "albums",
			opts:       map[string]any{"artist_id": 3},
			wantMethod: http.MethodGet,
			wantPath:   "/album",
			wantQuery:  url.Values{"artistId": {"3"}},
			wantAsList: true,
		},
		{
			name:       "album_get",
			verb:       "album_get",
			opts:       map[string]any{"id": 99},
			wantMethod: http.MethodGet,
			wantPath:   "/album/99",
			wantQuery:  url.Values{},
		},
		{
			name: "command with convenience fields",
			verb: "command",
			opts: map[string]any{
				"name": "AlbumSearch", "artist_id": 5,
				"album_ids": []any{float64(10), float64(11)},
			},
			wantMethod: http.MethodPost,
			wantPath:   "/command",
			wantQuery:  url.Values{},
			wantBody: map[string]any{
				"name": "AlbumSearch", "artistId": int64(5),
				"albumIds": []any{float64(10), float64(11)},
			},
		},
		{
			name: "command with extra params merged",
			verb: "command",
			opts: map[string]any{
				"name": "RefreshArtist", "params": map[string]any{"artistId": float64(9)},
			},
			wantMethod: http.MethodPost,
			wantPath:   "/command",
			wantQuery:  url.Values{},
			wantBody:   map[string]any{"name": "RefreshArtist", "artistId": float64(9)},
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
				"method": "put", "path": "/artist/editor", "query": map[string]any{"x": "1"},
				"body": map[string]any{"a": "b"},
			},
			wantMethod: http.MethodPut,
			wantPath:   "/artist/editor",
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
		{"artist_get", map[string]any{}},                                                       // no id
		{"lookup", map[string]any{}},                                                           // no term
		{"add_artist", map[string]any{}},                                                       // no foreign_artist_id/quality_profile_id/root_folder_path/artist
		{"add_artist", map[string]any{"foreign_artist_id": "mbid-1"}},                          // no quality_profile_id/root_folder_path
		{"add_artist", map[string]any{"foreign_artist_id": "mbid-1", "quality_profile_id": 1}}, // no root_folder_path
		{"delete_artist", map[string]any{}},                                                    // no id
		{"albums", map[string]any{}},                                                           // no artist_id
		{"album_get", map[string]any{}},                                                        // no id
		{"command", map[string]any{}},                                                          // no name
		{"api", map[string]any{}},                                                              // no path
		{"nope", map[string]any{}},                                                             // unknown verb
	}
	for _, tc := range cases {
		if _, err := buildRequest(tc.verb, tc.opts); err == nil {
			t.Errorf("buildRequest(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// newFakeLidarr starts an httptest.Server standing in for Lidarr, asserting
// every request carries the X-Api-Key header and hits /api/v1, then delegates
// to check for verb-specific assertions and returns the given JSON body.
func newFakeLidarr(t *testing.T, wantMethod, wantPath string, check func(t *testing.T, r *http.Request), status int, body string) *httptest.Server {
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

func TestInvokeArtistsItems(t *testing.T) {
	srv := newFakeLidarr(t, http.MethodGet, "/artist", nil, 0,
		`[{"id":1,"artistName":"Foo"},{"id":2,"artistName":"Bar"}]`)
	defer srv.Close()

	p := lidarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "artists",
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

func TestInvokeArtistGetResult(t *testing.T) {
	srv := newFakeLidarr(t, http.MethodGet, "/artist/42", nil, 0, `{"id":42,"artistName":"Foo"}`)
	defer srv.Close()

	p := lidarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "artist_get",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"id": 42},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["artistName"] != "Foo" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeAddArtistJSONBody(t *testing.T) {
	var gotBody map[string]any
	srv := newFakeLidarr(t, http.MethodPost, "/artist", func(t *testing.T, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
	}, 0, `{"id":1,"artistName":"Foo"}`)
	defer srv.Close()

	p := lidarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "add_artist",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options: map[string]any{
			"foreign_artist_id": "mbid-123", "quality_profile_id": 1, "root_folder_path": "/music",
			"search_for_missing": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["foreignArtistId"] != "mbid-123" || gotBody["rootFolderPath"] != "/music" {
		t.Fatalf("posted body: %#v", gotBody)
	}
	addOptions, ok := gotBody["addOptions"].(map[string]any)
	if !ok || addOptions["searchForMissingAlbums"] != true {
		t.Fatalf("addOptions: %#v", gotBody["addOptions"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["artistName"] != "Foo" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeCommandJSONBody(t *testing.T) {
	var gotBody map[string]any
	srv := newFakeLidarr(t, http.MethodPost, "/command", func(t *testing.T, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
	}, 0, `{"id":10,"name":"AlbumSearch","status":"queued"}`)
	defer srv.Close()

	p := lidarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "command",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"name": "AlbumSearch", "artist_id": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["name"] != "AlbumSearch" || gotBody["artistId"] != float64(5) {
		t.Fatalf("posted body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "queued" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeDeleteArtistQueryParams(t *testing.T) {
	srv := newFakeLidarr(t, http.MethodDelete, "/artist/7", func(t *testing.T, r *http.Request) {
		if r.URL.Query().Get("deleteFiles") != "true" {
			t.Errorf("deleteFiles: %q", r.URL.Query().Get("deleteFiles"))
		}
		if r.URL.Query().Get("addImportListExclusion") != "true" {
			t.Errorf("addImportListExclusion: %q", r.URL.Query().Get("addImportListExclusion"))
		}
	}, http.StatusOK, ``)
	defer srv.Close()

	p := lidarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "delete_artist",
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
	srv := newFakeLidarr(t, http.MethodGet, "/artist/lookup", func(t *testing.T, r *http.Request) {
		if r.URL.Query().Get("term") != "the beatles" {
			t.Errorf("term: %q", r.URL.Query().Get("term"))
		}
	}, 0, `[{"artistName":"The Beatles","foreignArtistId":"mbid-1"}]`)
	defer srv.Close()

	p := lidarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "lookup",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"term": "the beatles"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeAlbumsItems(t *testing.T) {
	srv := newFakeLidarr(t, http.MethodGet, "/album", func(t *testing.T, r *http.Request) {
		if r.URL.Query().Get("artistId") != "3" {
			t.Errorf("artistId: %q", r.URL.Query().Get("artistId"))
		}
	}, 0, `[{"id":1,"title":"Abbey Road"}]`)
	defer srv.Close()

	p := lidarrPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "albums",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"artist_id": 3},
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
	srv := newFakeLidarr(t, http.MethodGet, "/system/status", nil, 0, `[{"a":1}]`)
	defer srv.Close()

	p := lidarrPlugin{}
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

	p := lidarrPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "artists",
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
	p := lidarrPlugin{}
	cases := []map[string]any{
		{},
		{"base_url": "http://lidarr:8686"},
		{"api_key": "k"},
	}
	for _, conn := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "artists", Connection: conn})
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
// typical Lidarr Grab webhook delivery.
func TestHandleWebhookBodyGrabEvent(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{
		"eventType": "Grab",
		"artist": {"name": "The Beatles", "mbId": "b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d"},
		"albums": [{"title": "Abbey Road"}],
		"release": {"quality": "FLAC"},
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
	if ctx["event_type"] != "Grab" || ctx["artist_title"] != "The Beatles" || ctx["mbid"] != "b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d" {
		t.Fatalf("context fields: %#v", ctx)
	}
	if ctx["quality"] != "FLAC" {
		t.Errorf("quality: %#v", ctx["quality"])
	}
	if ctx["event_types"] != "Grab" || ctx["artists"] != "The Beatles" {
		t.Errorf("filter aliases: %#v", ctx)
	}
	albums, ok := ctx["albums"].([]any)
	if !ok || len(albums) != 1 {
		t.Fatalf("albums: %#v", ctx["albums"])
	}
	payload, ok := ctx["payload"].(map[string]any)
	if !ok || payload["eventType"] != "Grab" {
		t.Fatalf("context.payload: %#v", ctx["payload"])
	}
}

// TestHandleWebhookBodyDownloadEvent proves a Download event (single `album`
// wrapped into a one-element list, quality read from trackFiles[0].quality)
// also extracts fields correctly.
func TestHandleWebhookBodyDownloadEvent(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{
		"eventType": "Download",
		"artist": {"name": "Foo", "mbId": "mbid-1"},
		"album": {"title": "Bar"},
		"trackFiles": [{"quality": "MP3-320"}],
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
	if ctx["quality"] != "MP3-320" {
		t.Errorf("quality: %#v", ctx["quality"])
	}
	albums, ok := ctx["albums"].([]any)
	if !ok || len(albums) != 1 {
		t.Fatalf("albums: %#v", ctx["albums"])
	}
	alb, ok := albums[0].(map[string]any)
	if !ok || alb["title"] != "Bar" {
		t.Fatalf("albums[0]: %#v", albums[0])
	}
}

// TestHandleWebhookBodyNoAlbumsPresent proves a Health/ApplicationUpdate/Test
// delivery (no albums, no album) yields a nil albums list rather than an
// error.
func TestHandleWebhookBodyNoAlbumsPresent(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{"eventType":"Health","level":"warning","message":"disk low"}`)

	var got map[string]any
	handleWebhookBody(body, dedup, func(payload any) error {
		got, _ = payload.(map[string]any)
		return nil
	})
	if got == nil {
		t.Fatal("expected an emitted event")
	}
	ctx := got["context"].(map[string]any)
	if albums, ok := ctx["albums"].([]any); !ok || len(albums) != 0 {
		t.Errorf("albums: want empty/nil, got %#v", ctx["albums"])
	}
	if ctx["artist_title"] != "" {
		t.Errorf("artist_title: want empty, got %#v", ctx["artist_title"])
	}
}

// TestHandleWebhookBodyDedups proves a redelivered notification (same
// eventType + mbId + downloadId) is dropped the second time.
func TestHandleWebhookBodyDedups(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	body := []byte(`{"eventType":"Grab","artist":{"name":"Foo","mbId":"mbid-1"},"downloadId":"same"}`)

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
	handleWebhookBody([]byte(`{"artist":{"name":"Foo"}}`), dedup, func(payload any) error {
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
	p := lidarrPlugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.StartSource(ctx, plugin.StartSourceRequest{
			Instance: "test",
			Config: map[string]any{
				"webhook": map[string]any{
					"listen": addr,
					"path":   "/lidarr",
					"secret": "s3cret",
				},
			},
		}, emit)
	}()
	waitForListener(t, addr)

	body := []byte(`{"eventType":"Grab","artist":{"name":"Foo","mbId":"mbid-1"},"downloadId":"a"}`)

	// Wrong token: nothing emitted.
	postJSON(t, addr, "/lidarr?token=nope", body, nil)
	select {
	case ev := <-events:
		t.Fatalf("wrong token should not emit, got %#v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	// Correct token via header: accepted and emits.
	postJSON(t, addr, "/lidarr", body, map[string]string{"X-Conductor-Token": "s3cret"})
	select {
	case ev := <-events:
		if ev["kind"] != "Grab" {
			t.Fatalf("emitted event: %#v", ev)
		}
	default:
		t.Fatal("expected an emitted event for the correctly-authenticated request")
	}

	// Correct token via query param, different downloadId so dedup doesn't hide it.
	body2 := []byte(`{"eventType":"Grab","artist":{"name":"Foo","mbId":"mbid-1"},"downloadId":"b"}`)
	postJSON(t, addr, "/lidarr?token=s3cret", body2, nil)
	select {
	case ev := <-events:
		if ev["dedup"] != "Grab\x00mbid-1\x00b" {
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
	d := lidarrPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "lidarr" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("capabilities: must not spawn/command: %#v", d.Capabilities)
	}
	if d.Capabilities.Egress == nil || len(d.Capabilities.Egress) != 0 {
		t.Fatalf("capabilities.egress: want declared-empty ([]string{}), got %#v", d.Capabilities.Egress)
	}
	want := []string{
		"artists", "artist_get", "lookup", "add_artist", "delete_artist",
		"albums", "album_get", "command", "queue", "calendar", "api",
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
