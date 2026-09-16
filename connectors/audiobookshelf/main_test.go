package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// newTestPlugin wires a fresh plugin + connection pointed at srv.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{"base_url": srv.URL, "token": "secret-token"}
}

func checkBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
		t.Errorf("Authorization header: got %q want %q", got, "Bearer secret-token")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestLibraries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/libraries" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"libraries": []any{
			map[string]any{"id": "lib1", "name": "Books"},
		}})
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "libraries", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	got := items[0].(map[string]any)
	if got["id"] != "lib1" {
		t.Errorf("items[0]: %#v", got)
	}
}

func TestLibraryGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/libraries/lib1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "lib1", "name": "Books"})
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "library_get", Connection: testConn(srv),
		Options: map[string]any{"library_id": "lib1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["name"] != "Books" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestLibraryItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/libraries/lib1/items" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("limit") != "10" || q.Get("page") != "2" || q.Get("sort") != "title" || q.Get("filter") != "genre.Fantasy" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{
			"results": []any{map[string]any{"id": "item1"}},
			"total":   1,
		})
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "library_items", Connection: testConn(srv),
		Options: map[string]any{"library_id": "lib1", "limit": 10, "page": 2, "sort": "title", "filter": "genre.Fantasy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "item1" {
		t.Fatalf("items (hoisted from result.results): %#v", res.Outputs["items"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["total"] != float64(1) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestGetItem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/items/item1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if r.URL.Query().Get("expanded") != "1" {
			t.Errorf("expected ?expanded=1, got %q", r.URL.RawQuery)
		}
		writeJSON(w, 200, map[string]any{"id": "item1", "media": map[string]any{"title": "Dune"}})
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "get_item", Connection: testConn(srv),
		Options: map[string]any{"item_id": "item1", "expanded": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "item1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/libraries/lib1/search" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if r.URL.Query().Get("q") != "dune" {
			t.Errorf("expected ?q=dune, got %q", r.URL.RawQuery)
		}
		writeJSON(w, 200, map[string]any{"book": []any{map[string]any{"id": "item1"}}})
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "search", Connection: testConn(srv),
		Options: map[string]any{"library_id": "lib1", "q": "dune"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["book"]; !ok {
		t.Fatalf("result missing book: %#v", result)
	}
}

func TestScan(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "scan", Connection: testConn(srv),
		Options: map[string]any{"library_id": "lib1", "force": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/libraries/lib1/scan" || gotQuery != "force=1" {
		t.Fatalf("request: %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if res.Outputs["status_code"] != 204 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestSeriesAndCollections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch r.URL.Path {
		case "/api/libraries/lib1/series":
			writeJSON(w, 200, map[string]any{"results": []any{map[string]any{"id": "series1"}}})
		case "/api/libraries/lib1/collections":
			writeJSON(w, 200, map[string]any{"results": []any{map[string]any{"id": "col1"}}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "series", Connection: testConn(srv), Options: map[string]any{"library_id": "lib1"}})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "series1" {
		t.Fatalf("series items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "collections", Connection: testConn(srv), Options: map[string]any{"library_id": "lib1"}})
	if err != nil {
		t.Fatal(err)
	}
	items, ok = res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "col1" {
		t.Fatalf("collections items: %#v", res.Outputs["items"])
	}
}

func TestMe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/me" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "user1", "username": "daniel"})
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "me", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["username"] != "daniel" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestGetProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/me/progress/item1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"progress": 0.5})
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "get_progress", Connection: testConn(srv), Options: map[string]any{"item_id": "item1"}})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["progress"] != 0.5 {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestUpdateProgress(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPatch {
			t.Errorf("method: got %s want PATCH", r.Method)
		}
		if r.URL.Path != "/api/me/progress/item1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"success": true})
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "update_progress", Connection: testConn(srv),
		Options: map[string]any{"item_id": "item1", "progress": 0.75, "current_time": 120.5, "is_finished": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"progress": 0.75, "currentTime": 120.5, "isFinished": false}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body: got %#v want %#v", gotBody, want)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestPlaybackSessions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/me/listening-sessions" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"sessions": []any{}})
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "playback_sessions", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAuthorize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api/authorize" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"user": map[string]any{"id": "user1"}})
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "authorize", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/ping":
			writeJSON(w, 200, map[string]any{"ok": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/libraries" && r.URL.Query().Get("minified") == "1":
			writeJSON(w, 200, []any{map[string]any{"id": "lib1"}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/ping"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["ok"] != true {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/libraries", "query": map[string]any{"minified": "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer srv.Close()

	p := newAudiobookshelfPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "me", Connection: testConn(srv)})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok {
		t.Fatalf("expected *plugin.Error, got %T: %v", err, err)
	}
	if pe.Code != plugin.CodeInternalError {
		t.Errorf("code: got %d want %d", pe.Code, plugin.CodeInternalError)
	}
	if !containsAll(pe.Message, "401", "invalid token") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newAudiobookshelfPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "token": "t"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"library_get", map[string]any{}},
		{"library_items", map[string]any{}},
		{"get_item", map[string]any{}},
		{"search", map[string]any{"library_id": "lib1"}},
		{"scan", map[string]any{}},
		{"series", map[string]any{}},
		{"collections", map[string]any{}},
		{"get_progress", map[string]any{}},
		{"update_progress", map[string]any{}},
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s: expected error for missing required options", tc.verb)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", tc.verb, err)
		}
	}
}

func TestMissingConnection(t *testing.T) {
	p := newAudiobookshelfPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "me", Connection: map[string]any{"token": "t"}}); err == nil {
		t.Error("expected error for missing base_url")
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "me", Connection: map[string]any{"base_url": "http://x"}}); err == nil {
		t.Error("expected error for missing token")
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newAudiobookshelfPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: map[string]any{"base_url": "http://x", "token": "t"}})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDescribe(t *testing.T) {
	p := newAudiobookshelfPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "audiobookshelf" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["base_url"].Type != "string" || !d.Connection["base_url"].Required {
		t.Errorf("connection.base_url: %#v", d.Connection["base_url"])
	}
	if d.Connection["token"].Type != "string" || !d.Connection["token"].Required {
		t.Errorf("connection.token: %#v", d.Connection["token"])
	}
	want := []string{
		"libraries", "library_get", "library_items", "get_item", "search", "scan",
		"series", "collections", "me", "get_progress", "update_progress",
		"playback_sessions", "authorize", "api",
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
	if len(d.Capabilities.Egress) != 0 {
		t.Errorf("expected empty egress (self-hosted), got %v", d.Capabilities.Egress)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (sub == "" || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
