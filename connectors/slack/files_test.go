package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeRepliesServer answers conversations.replies, users.info and
// chat.getPermalink with fixed data, and serves file bytes at /files/<id>.
func fakeRepliesServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	var fileBody = []byte("hello world, this is a test file\n")
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":true,"messages":[
			{"user":"U1","text":"root message","ts":"1.000","files":[{"id":"F1","name":"../../etc/passwd","mimetype":"text/plain","size":%d,"url_private":%q}]},
			{"user":"U2","text":"a reply","ts":"2.000","thread_ts":"1.000"}
		]}`, len(fileBody), srv.URL+"/files/F1")
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("user")
		name := "Unknown"
		if id == "U1" {
			name = "Alice"
		} else if id == "U2" {
			name = "Bob"
		}
		fmt.Fprintf(w, `{"ok":true,"user":{"profile":{"display_name":%q}}}`, name)
	})
	mux.HandleFunc("/chat.getPermalink", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"permalink":"https://example.slack.com/archives/C1/p1000"}`)
	})
	mux.HandleFunc("/files/F1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(fileBody)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, string(fileBody)
}

func TestThreadVerb(t *testing.T) {
	srv, _ := fakeRepliesServer(t)
	api := newSlackAPI("xoxb-1", "", srv.URL)
	p := New()
	out, err := p.threadVerb(context.Background(), api, map[string]any{"channel": "C1", "ts": "1.000"})
	if err != nil {
		t.Fatal(err)
	}
	if out["count"] != 2 {
		t.Fatalf("count = %v", out["count"])
	}
	msgs := out["messages"].([]any)
	m0 := msgs[0].(map[string]any)
	if m0["user_name"] != "Alice" {
		t.Fatalf("user_name = %v", m0["user_name"])
	}
	if out["permalink"] != "https://example.slack.com/archives/C1/p1000" {
		t.Fatalf("permalink = %v", out["permalink"])
	}
}

func TestThreadVerbRequiresChannelAndTS(t *testing.T) {
	p := New()
	api := newSlackAPI("xoxb-1", "", "http://unused")
	if _, err := p.threadVerb(context.Background(), api, map[string]any{}); err == nil {
		t.Fatal("expected an error with no channel/ts")
	}
}

func TestDownloadVerbSanitizesAndStages(t *testing.T) {
	srv, want := fakeRepliesServer(t)
	api := newSlackAPI("xoxb-1", "", srv.URL)
	p := New()

	dir := t.TempDir()
	old := stagingRoot
	stagingRoot = func() string { return dir }
	defer func() { stagingRoot = old }()

	out, err := p.downloadVerb(context.Background(), api, map[string]any{"channel": "C1", "ts": "1.000"})
	if err != nil {
		t.Fatal(err)
	}
	if out["count"] != 1 {
		t.Fatalf("count = %v, skipped = %v", out["count"], out["skipped"])
	}
	files := out["files"].([]any)
	f0 := files[0].(map[string]any)
	name := f0["name"].(string)
	// Path traversal in the file's own name must never escape the staging
	// dir, and must not keep directory separators.
	if filepath.Base(name) != name {
		t.Fatalf("file name escaped its base form: %q", name)
	}
	data, err := os.ReadFile(f0["path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("downloaded content mismatch")
	}
	// The staged path must stay under the staging root.
	rel, err := filepath.Rel(dir, f0["path"].(string))
	if err != nil || rel == ".." || len(rel) >= 2 && rel[:2] == ".." {
		t.Fatalf("staged file escaped the root: %q", f0["path"])
	}
}

func TestDownloadVerbCaps(t *testing.T) {
	srv, _ := fakeRepliesServer(t)
	api := newSlackAPI("xoxb-1", "", srv.URL)
	p := New()
	dir := t.TempDir()
	old := stagingRoot
	stagingRoot = func() string { return dir }
	defer func() { stagingRoot = old }()

	out, err := p.downloadVerb(context.Background(), api, map[string]any{
		"channel": "C1", "ts": "1.000", "max_file_bytes": 4, // smaller than the fixture file
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["count"] != 0 {
		t.Fatalf("want 0 downloaded (over max_file_bytes), got %v", out["count"])
	}
	skipped := out["skipped"].([]any)
	if len(skipped) != 1 {
		t.Fatalf("want 1 skipped entry, got %+v", skipped)
	}
}

func TestDownloadVerbRefusesOffSlackURL(t *testing.T) {
	var evil *httptest.Server
	evil = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.replies":
			fmt.Fprintf(w, `{"ok":true,"messages":[{"user":"U1","ts":"1.000","files":[{"id":"F1","name":"a.txt","mimetype":"text/plain","size":10,"url_private":"https://evil.example.com/steal"}]}]}`)
		}
	}))
	defer evil.Close()
	api := newSlackAPI("xoxb-1", "", evil.URL)
	p := New()
	dir := t.TempDir()
	old := stagingRoot
	stagingRoot = func() string { return dir }
	defer func() { stagingRoot = old }()

	out, err := p.downloadVerb(context.Background(), api, map[string]any{"channel": "C1", "ts": "1.000"})
	if err != nil {
		t.Fatal(err)
	}
	if out["count"] != 0 {
		t.Fatalf("an off-slack.com file URL must never be fetched, got count=%v", out["count"])
	}
}

func TestSanitizeFileName(t *testing.T) {
	cases := []struct{ in, fallback, want string }{
		{"../../etc/passwd", "F1", "passwd"},
		{"..\\..\\windows\\system32", "F1", "system32"},
		{"normal-file_1.txt", "F1", "normal-file_1.txt"},
		{"", "F1", "F1"},
		{"...", "F1", "F1"},
	}
	for _, c := range cases {
		if got := sanitizeFileName(c.in, c.fallback); got != c.want {
			t.Errorf("sanitizeFileName(%q,%q) = %q, want %q", c.in, c.fallback, got, c.want)
		}
	}
}
