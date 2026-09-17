package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn builds a connection map with the managed-OAuth2 token already
// injected (as the daemon would do), pointed at srv for both the Drive API
// and the upload host.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{
		plugin.AccessTokenKey: "test-token",
		"api_base":            srv.URL,
		"upload_base":         srv.URL,
	}
}

func checkBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization header: got %q want %q", got, "Bearer test-token")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestFilesHoistsItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/files" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("q") != "trashed = false" || q.Get("pageSize") != "10" || q.Get("orderBy") != "modifiedTime desc" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{
			"nextPageToken": "tok-2",
			"files":         []any{map[string]any{"id": "f1", "name": "a.txt"}},
		})
	}))
	defer srv.Close()

	p := newGdrivePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "files", Connection: testConn(srv),
		Options: map[string]any{"q": "trashed = false", "pageSize": 10, "orderBy": "modifiedTime desc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "f1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["nextPageToken"] != "tok-2" {
		t.Fatalf("result should carry nextPageToken: %#v", res.Outputs["result"])
	}
}

func TestFileGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/files/f1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if r.URL.Query().Get("fields") != "id,name" {
			t.Errorf("fields query: got %q", r.URL.Query().Get("fields"))
		}
		writeJSON(w, 200, map[string]any{"id": "f1", "name": "a.txt"})
	}))
	defer srv.Close()

	p := newGdrivePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "file_get", Connection: testConn(srv),
		Options: map[string]any{"file_id": "f1", "fields": "id,name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["name"] != "a.txt" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestFileDownloadReturnsBase64Content(t *testing.T) {
	raw := []byte("hello drive")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/files/f1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if r.URL.Query().Get("alt") != "media" {
			t.Errorf("alt=media missing: %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	p := newGdrivePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "file_download", Connection: testConn(srv),
		Options: map[string]any{"file_id": "f1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := res.Outputs["content_base64"].(string)
	if !ok {
		t.Fatalf("content_base64: %#v", res.Outputs["content_base64"])
	}
	decoded, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Errorf("decoded content: got %q want %q", decoded, raw)
	}
	if res.Outputs["content_type"] != "text/plain; charset=utf-8" {
		t.Errorf("content_type: %#v", res.Outputs["content_type"])
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestFileUploadSendsMultipartRelated(t *testing.T) {
	tmp := t.TempDir() + "/note.txt"
	content := []byte("upload me")
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		t.Fatal(err)
	}

	var gotMetaName string
	var gotFileBytes []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/files" || r.URL.Query().Get("uploadType") != "multipart" {
			t.Errorf("path/query: got %q %q", r.URL.Path, r.URL.RawQuery)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/related" {
			t.Fatalf("Content-Type: got %q err %v", r.Header.Get("Content-Type"), err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])

		metaPart, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		var meta map[string]any
		if err := json.NewDecoder(metaPart).Decode(&meta); err != nil {
			t.Fatal(err)
		}
		gotMetaName, _ = meta["name"].(string)

		filePart, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		buf := &bytes.Buffer{}
		if _, err := buf.ReadFrom(filePart); err != nil {
			t.Fatal(err)
		}
		gotFileBytes = buf.Bytes()

		writeJSON(w, 200, map[string]any{"id": "new-file", "name": meta["name"]})
	}))
	defer srv.Close()

	p := newGdrivePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "file_upload", Connection: testConn(srv),
		Options: map[string]any{"path": tmp, "name": "note.txt", "mime_type": "text/plain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMetaName != "note.txt" {
		t.Errorf("metadata name: got %q", gotMetaName)
	}
	if !bytes.Equal(gotFileBytes, content) {
		t.Errorf("file bytes: got %q want %q", gotFileBytes, content)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "new-file" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestFileCreateFolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/files" {
			t.Errorf("method/path: got %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["mimeType"] != folderMimeType || body["name"] != "New Folder" {
			t.Errorf("body: %#v", body)
		}
		writeJSON(w, 200, map[string]any{"id": "folder-1", "name": "New Folder"})
	}))
	defer srv.Close()

	p := newGdrivePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "file_create_folder", Connection: testConn(srv),
		Options: map[string]any{"name": "New Folder", "parents": []any{"root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "folder-1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestFileUpdateMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPatch || r.URL.Path != "/files/f1" {
			t.Errorf("method/path: got %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["name"] != "renamed.txt" {
			t.Errorf("body: %#v", body)
		}
		writeJSON(w, 200, map[string]any{"id": "f1", "name": "renamed.txt"})
	}))
	defer srv.Close()

	p := newGdrivePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "file_update_metadata", Connection: testConn(srv),
		Options: map[string]any{"file_id": "f1", "metadata": map[string]any{"name": "renamed.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["name"] != "renamed.txt" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestFileDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodDelete || r.URL.Path != "/files/f1" {
			t.Errorf("method/path: got %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	p := newGdrivePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "file_delete", Connection: testConn(srv),
		Options: map[string]any{"file_id": "f1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 204 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestPermissionsAndPermissionCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/files/f1/permissions":
			writeJSON(w, 200, map[string]any{"permissions": []any{map[string]any{"id": "p1", "role": "reader"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/files/f1/permissions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["role"] != "writer" || body["type"] != "user" || body["emailAddress"] != "a@example.com" {
				t.Errorf("body: %#v", body)
			}
			writeJSON(w, 200, map[string]any{"id": "p2", "role": "writer"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newGdrivePlugin()

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "permissions", Connection: testConn(srv), Options: map[string]any{"file_id": "f1"}})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "p1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "permission_create", Connection: testConn(srv),
		Options: map[string]any{"file_id": "f1", "role": "writer", "type": "user", "emailAddress": "a@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "p2" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/about" && r.URL.Query().Get("fields") == "user":
			writeJSON(w, 200, map[string]any{"user": map[string]any{"displayName": "Bob"}})
		case r.Method == http.MethodPost && r.URL.Path == "/files/f1/copy":
			writeJSON(w, 200, map[string]any{"id": "copy-1"})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newGdrivePlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/about", "query": map[string]any{"fields": "user"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["user"]; !ok {
		t.Fatalf("result missing user: %#v", result)
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "POST", "path": "files/f1/copy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatchArrayResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/drives" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{map[string]any{"id": "d1"}})
	}))
	defer srv.Close()

	p := newGdrivePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/drives"},
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
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid Credentials"}}`))
	}))
	defer srv.Close()

	p := newGdrivePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "file_get", Connection: testConn(srv), Options: map[string]any{"file_id": "f1"}})
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
	if !containsAll(pe.Message, "401", "Invalid Credentials") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingAccessTokenIsInvalidParams(t *testing.T) {
	p := newGdrivePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "files",
		Connection: map[string]any{}, // no access_token injected
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing access token, got %v", err)
	}
	if !containsAll(pe.Message, "access token", "conductor connector auth google-drive") {
		t.Errorf("message should point at the auth: block / login flow, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newGdrivePlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"file_get", map[string]any{}},
		{"file_download", map[string]any{}},
		{"file_upload", map[string]any{}},
		{"file_create_folder", map[string]any{}},
		{"file_update_metadata", map[string]any{"file_id": "f1"}},
		{"file_delete", map[string]any{}},
		{"permissions", map[string]any{}},
		{"permission_create", map[string]any{"file_id": "f1"}},
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

func TestUnknownVerb(t *testing.T) {
	p := newGdrivePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{plugin.AccessTokenKey: "t"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for unknown verb, got %v", err)
	}
}

func TestDescribe(t *testing.T) {
	p := newGdrivePlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "google-drive" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}

	want := []string{
		"files", "file_get", "file_download", "file_upload", "file_create_folder",
		"file_update_metadata", "file_delete", "permissions", "permission_create", "api",
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

	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "www.googleapis.com:443" {
		t.Errorf("egress: got %v want [www.googleapis.com:443]", d.Capabilities.Egress)
	}

	if d.Auth == nil {
		t.Fatal("expected Describe().Auth to be non-nil (managed OAuth2)")
	}
	if d.Auth.TokenURL != "https://oauth2.googleapis.com/token" {
		t.Errorf("Auth.TokenURL: got %q", d.Auth.TokenURL)
	}
	if d.Auth.AuthURL != "https://accounts.google.com/o/oauth2/v2/auth" {
		t.Errorf("Auth.AuthURL: got %q", d.Auth.AuthURL)
	}
	foundDriveScope := false
	for _, s := range d.Auth.Scopes {
		if s == "https://www.googleapis.com/auth/drive" {
			foundDriveScope = true
		}
	}
	if !foundDriveScope {
		t.Errorf("Auth.Scopes missing drive scope: %v", d.Auth.Scopes)
	}
	if d.Auth.AuthParams["access_type"] != "offline" {
		t.Errorf("Auth.AuthParams[access_type]: got %q want %q", d.Auth.AuthParams["access_type"], "offline")
	}
	if d.Auth.AuthParams["prompt"] != "consent" {
		t.Errorf("Auth.AuthParams[prompt]: got %q want %q", d.Auth.AuthParams["prompt"], "consent")
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
