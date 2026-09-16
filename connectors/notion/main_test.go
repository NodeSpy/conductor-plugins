package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestDescribe asserts the declared surface: kind, type, capabilities, and
// every verb.
func TestDescribe(t *testing.T) {
	d := notionPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "notion" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Egress, "api.notion.com:443") {
		t.Fatalf("egress: %#v", d.Capabilities.Egress)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("expected no commands/spawns, got %#v", d.Capabilities)
	}
	want := []string{
		"create_page", "update_page", "get_page", "get_database", "query_database",
		"create_database", "update_database", "append_blocks", "get_block_children",
		"delete_block", "search", "create_comment", "get_user", "list_users", "api",
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
}

// TestParentValue covers the parent shortcut resolution used by create_page.
func TestParentValue(t *testing.T) {
	cases := []struct {
		name string
		opts map[string]any
		want map[string]any
	}{
		{"full object", map[string]any{"parent": map[string]any{"database_id": "db1"}}, map[string]any{"database_id": "db1"}},
		{"database_id shortcut", map[string]any{"database_id": "db1"}, map[string]any{"database_id": "db1"}},
		{"page_id shortcut", map[string]any{"page_id": "pg1"}, map[string]any{"page_id": "pg1"}},
		{"bare parent string", map[string]any{"parent": "db1"}, map[string]any{"database_id": "db1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parentValue(tc.opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertJSONEqual(t, got, tc.want)
		})
	}
	if _, err := parentValue(map[string]any{}); err == nil {
		t.Error("expected error for missing parent")
	}
}

// TestVerbCall pins the exact HTTP method/path/body each verb builds,
// including the parent/title/rich_text shortcut wrapping — proven without
// sending anything.
func TestVerbCall(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		wantMethod string
		wantPath   string
		wantBody   any
		wantQuery  map[string]string
	}{
		{
			name: "create_page with database_id shortcut",
			verb: "create_page",
			opts: map[string]any{
				"database_id": "db1",
				"properties":  map[string]any{"Name": map[string]any{"title": []any{}}},
			},
			wantMethod: http.MethodPost,
			wantPath:   "pages",
			wantBody: map[string]any{
				"parent":     map[string]any{"database_id": "db1"},
				"properties": map[string]any{"Name": map[string]any{"title": []any{}}},
			},
		},
		{
			name: "create_page with page_id shortcut and children",
			verb: "create_page",
			opts: map[string]any{
				"page_id":    "pg1",
				"properties": map[string]any{"title": map[string]any{}},
				"children":   []any{map[string]any{"paragraph": map[string]any{}}},
			},
			wantMethod: http.MethodPost,
			wantPath:   "pages",
			wantBody: map[string]any{
				"parent":     map[string]any{"page_id": "pg1"},
				"properties": map[string]any{"title": map[string]any{}},
				"children":   []any{map[string]any{"paragraph": map[string]any{}}},
			},
		},
		{
			name:       "update_page",
			verb:       "update_page",
			opts:       map[string]any{"page_id": "pg1", "archived": true},
			wantMethod: http.MethodPatch,
			wantPath:   "pages/pg1",
			wantBody:   map[string]any{"archived": true},
		},
		{
			name:       "get_page",
			verb:       "get_page",
			opts:       map[string]any{"page_id": "pg1"},
			wantMethod: http.MethodGet,
			wantPath:   "pages/pg1",
		},
		{
			name:       "get_database",
			verb:       "get_database",
			opts:       map[string]any{"database_id": "db1"},
			wantMethod: http.MethodGet,
			wantPath:   "databases/db1",
		},
		{
			name: "query_database",
			verb: "query_database",
			opts: map[string]any{
				"database_id": "db1",
				"filter":      map[string]any{"property": "Status"},
				"page_size":   float64(10),
			},
			wantMethod: http.MethodPost,
			wantPath:   "databases/db1/query",
			wantBody: map[string]any{
				"filter":    map[string]any{"property": "Status"},
				"page_size": float64(10),
			},
		},
		{
			name: "create_database with string title",
			verb: "create_database",
			opts: map[string]any{
				"parent":     "pg1",
				"title":      "My DB",
				"properties": map[string]any{"Name": map[string]any{"title": map[string]any{}}},
			},
			wantMethod: http.MethodPost,
			wantPath:   "databases",
			wantBody: map[string]any{
				"parent":     map[string]any{"page_id": "pg1"},
				"title":      []any{map[string]any{"type": "text", "text": map[string]any{"content": "My DB"}}},
				"properties": map[string]any{"Name": map[string]any{"title": map[string]any{}}},
			},
		},
		{
			name: "append_blocks",
			verb: "append_blocks",
			opts: map[string]any{
				"block_id": "blk1",
				"children": []any{map[string]any{"paragraph": map[string]any{}}},
			},
			wantMethod: http.MethodPatch,
			wantPath:   "blocks/blk1/children",
			wantBody:   map[string]any{"children": []any{map[string]any{"paragraph": map[string]any{}}}},
		},
		{
			name:       "get_block_children",
			verb:       "get_block_children",
			opts:       map[string]any{"block_id": "blk1", "page_size": float64(5)},
			wantMethod: http.MethodGet,
			wantPath:   "blocks/blk1/children",
			wantQuery:  map[string]string{"page_size": "5"},
		},
		{
			name:       "delete_block",
			verb:       "delete_block",
			opts:       map[string]any{"block_id": "blk1"},
			wantMethod: http.MethodDelete,
			wantPath:   "blocks/blk1",
		},
		{
			name:       "search",
			verb:       "search",
			opts:       map[string]any{"query": "hello"},
			wantMethod: http.MethodPost,
			wantPath:   "search",
			wantBody:   map[string]any{"query": "hello"},
		},
		{
			name:       "create_comment with page_id parent and string rich_text",
			verb:       "create_comment",
			opts:       map[string]any{"parent": "pg1", "rich_text": "hello world"},
			wantMethod: http.MethodPost,
			wantPath:   "comments",
			wantBody: map[string]any{
				"parent":    map[string]any{"page_id": "pg1"},
				"rich_text": []any{map[string]any{"text": map[string]any{"content": "hello world"}}},
			},
		},
		{
			name: "create_comment with discussion_id",
			verb: "create_comment",
			opts: map[string]any{
				"discussion_id": "disc1",
				"rich_text":     []any{map[string]any{"text": map[string]any{"content": "reply"}}},
			},
			wantMethod: http.MethodPost,
			wantPath:   "comments",
			wantBody: map[string]any{
				"discussion_id": "disc1",
				"rich_text":     []any{map[string]any{"text": map[string]any{"content": "reply"}}},
			},
		},
		{
			name:       "get_user",
			verb:       "get_user",
			opts:       map[string]any{"user_id": "usr1"},
			wantMethod: http.MethodGet,
			wantPath:   "users/usr1",
		},
		{
			name:       "list_users",
			verb:       "list_users",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "users",
		},
		{
			name: "api escape hatch",
			verb: "api",
			opts: map[string]any{
				"method": "post",
				"path":   "/pages/pg1/restore",
				"body":   map[string]any{"x": 1},
			},
			wantMethod: http.MethodPost,
			wantPath:   "pages/pg1/restore",
			wantBody:   map[string]any{"x": float64(1)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, err := verbCall(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("verbCall(%s): unexpected error: %v", tc.verb, err)
			}
			if call.method != tc.wantMethod {
				t.Errorf("method: got %q want %q", call.method, tc.wantMethod)
			}
			if call.path != tc.wantPath {
				t.Errorf("path: got %q want %q", call.path, tc.wantPath)
			}
			if tc.wantBody != nil {
				assertJSONEqual(t, call.body, tc.wantBody)
			}
			for k, v := range tc.wantQuery {
				if call.query[k] != v {
					t.Errorf("query[%s]: got %q want %q", k, call.query[k], v)
				}
			}
		})
	}
}

// TestVerbCallErrors covers required-field validation and unknown verbs.
func TestVerbCallErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"create_page", map[string]any{}},                         // no parent/properties
		{"create_page", map[string]any{"database_id": "db1"}},     // no properties
		{"update_page", map[string]any{}},                         // no page_id
		{"get_page", map[string]any{}},                            // no page_id
		{"get_database", map[string]any{}},                        // no database_id
		{"query_database", map[string]any{}},                      // no database_id
		{"create_database", map[string]any{}},                     // no parent
		{"create_database", map[string]any{"parent": "pg1"}},      // no properties
		{"update_database", map[string]any{}},                     // no database_id
		{"append_blocks", map[string]any{"block_id": "b1"}},       // no children
		{"append_blocks", map[string]any{"children": []any{"x"}}}, // no block_id
		{"get_block_children", map[string]any{}},                  // no block_id
		{"delete_block", map[string]any{}},                        // no block_id
		{"create_comment", map[string]any{}},                      // no parent/discussion_id, no rich_text
		{"create_comment", map[string]any{"parent": "pg1"}},       // no rich_text
		{"get_user", map[string]any{}},                            // no user_id
		{"api", map[string]any{}},                                 // no method/path
		{"api", map[string]any{"method": "GET"}},                  // no path
		{"nope", map[string]any{}},                                // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbCall(tc.verb, tc.opts); err == nil {
			t.Errorf("verbCall(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers connection validation and defaults.
func TestParseConn(t *testing.T) {
	if _, err := parseConn(map[string]any{}); err == nil {
		t.Fatal("expected error for missing token")
	}
	c, err := parseConn(map[string]any{"token": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if c.version != defaultNotionVersion {
		t.Errorf("version default: got %q want %q", c.version, defaultNotionVersion)
	}
	if c.base != defaultAPIBase {
		t.Errorf("base default: got %q want %q", c.base, defaultAPIBase)
	}
	c2, err := parseConn(map[string]any{"token": "secret", "version": "2099-01-01", "api_base": "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	if c2.version != "2099-01-01" || c2.base != "http://x" {
		t.Errorf("overrides not applied: %#v", c2)
	}
}

// TestInvokeAgainstFakeNotion drives Invoke end to end against an
// httptest.Server, asserting method, path, headers, and body for a
// representative set of verbs, plus that list endpoints surface `results`.
func TestInvokeAgainstFakeNotion(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotVersion, gotContentType string
	var gotBody map[string]any
	var respBody string
	var respStatus int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("Notion-Version")
		gotContentType = r.Header.Get("Content-Type")
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
		}
		w.WriteHeader(respStatus)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	conn := map[string]any{"token": "tok-123", "api_base": srv.URL}
	p := notionPlugin{}

	t.Run("create_page headers and body", func(t *testing.T) {
		respStatus = 200
		respBody = `{"id":"pg1","object":"page"}`
		res, err := p.Invoke(plugin.InvokeRequest{
			Verb:       "create_page",
			Connection: conn,
			Options: map[string]any{
				"database_id": "db1",
				"properties":  map[string]any{"Name": map[string]any{}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotMethod != http.MethodPost || gotPath != "/pages" {
			t.Fatalf("method/path: %s %s", gotMethod, gotPath)
		}
		if gotAuth != "Bearer tok-123" {
			t.Errorf("Authorization: got %q", gotAuth)
		}
		if gotVersion != defaultNotionVersion {
			t.Errorf("Notion-Version: got %q", gotVersion)
		}
		if gotContentType != "application/json" {
			t.Errorf("Content-Type: got %q", gotContentType)
		}
		parent, _ := gotBody["parent"].(map[string]any)
		if parent["database_id"] != "db1" {
			t.Errorf("body parent: %#v", gotBody["parent"])
		}
		result, ok := res.Outputs["result"].(map[string]any)
		if !ok || result["id"] != "pg1" {
			t.Fatalf("result: %#v", res.Outputs["result"])
		}
		if res.Outputs["status_code"] != 200 {
			t.Errorf("status_code: %#v", res.Outputs["status_code"])
		}
	})

	t.Run("custom version header", func(t *testing.T) {
		respStatus = 200
		respBody = `{}`
		customConn := map[string]any{"token": "tok-123", "api_base": srv.URL, "version": "2099-01-01"}
		if _, err := p.Invoke(plugin.InvokeRequest{Verb: "get_page", Connection: customConn, Options: map[string]any{"page_id": "pg1"}}); err != nil {
			t.Fatal(err)
		}
		if gotVersion != "2099-01-01" {
			t.Errorf("Notion-Version override: got %q", gotVersion)
		}
	})

	t.Run("query_database exposes results", func(t *testing.T) {
		respStatus = 200
		respBody = `{"object":"list","results":[{"id":"pg1"},{"id":"pg2"}],"has_more":false}`
		res, err := p.Invoke(plugin.InvokeRequest{
			Verb: "query_database", Connection: conn,
			Options: map[string]any{"database_id": "db1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		results, ok := res.Outputs["results"].([]any)
		if !ok || len(results) != 2 {
			t.Fatalf("results: %#v", res.Outputs["results"])
		}
	})

	t.Run("search exposes results", func(t *testing.T) {
		respStatus = 200
		respBody = `{"object":"list","results":[{"id":"pg1"}]}`
		res, err := p.Invoke(plugin.InvokeRequest{Verb: "search", Connection: conn, Options: map[string]any{"query": "x"}})
		if err != nil {
			t.Fatal(err)
		}
		if results, ok := res.Outputs["results"].([]any); !ok || len(results) != 1 {
			t.Fatalf("results: %#v", res.Outputs["results"])
		}
	})

	t.Run("non-2xx is CodeInternalError", func(t *testing.T) {
		respStatus = 404
		respBody = `{"object":"error","message":"not found"}`
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "get_page", Connection: conn, Options: map[string]any{"page_id": "missing"}})
		if err == nil {
			t.Fatal("expected error")
		}
		var re *plugin.Error
		if !asPluginError(err, &re) || re.Code != plugin.CodeInternalError {
			t.Fatalf("want CodeInternalError, got %v", err)
		}
		if !contains([]string{re.Message}, re.Message) || re.Message == "" {
			t.Fatal("expected a non-empty error message")
		}
	})
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// asPluginError unwraps a *plugin.Error without importing errors just for
// the test (the SDK returns the concrete type directly).
func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}

// assertJSONEqual compares two values by JSON round-trip, so map[string]any
// literals in test cases compare equal to whatever verbCall built (which may
// differ in dynamic type but not in JSON shape).
func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	var gotNorm, wantNorm any
	_ = json.Unmarshal(gotJSON, &gotNorm)
	_ = json.Unmarshal(wantJSON, &wantNorm)
	gotNormJSON, _ := json.Marshal(gotNorm)
	wantNormJSON, _ := json.Marshal(wantNorm)
	if string(gotNormJSON) != string(wantNormJSON) {
		t.Errorf("got:  %s\nwant: %s", gotNormJSON, wantNormJSON)
	}
}
