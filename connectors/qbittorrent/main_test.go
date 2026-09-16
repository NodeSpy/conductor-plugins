package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestDescribe asserts the declared surface: kind, type, capabilities, and
// every verb.
func TestDescribe(t *testing.T) {
	d := (&qbittorrentPlugin{}).Describe()
	if d.Kind != plugin.KindConnector || d.Type != "qbittorrent" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Egress) != 0 || len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("expected an empty capabilities manifest (self-hosted), got %#v", d.Capabilities)
	}
	want := []string{
		"torrents", "torrent_properties", "add", "delete", "pause", "resume",
		"recheck", "set_category", "add_tags", "remove_tags", "set_speed_limits",
		"transfer_info", "app_version", "api",
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

// TestJoinAny covers the hashes/tags/urls list-or-string normalization.
func TestJoinAny(t *testing.T) {
	cases := []struct {
		name string
		v    any
		sep  string
		want string
	}{
		{"nil", nil, "|", ""},
		{"bare string", "abc", "|", "abc"},
		{"list pipe", []any{"a", "b"}, "|", "a|b"},
		{"list comma", []any{"x", "y", "z"}, ",", "x,y,z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinAny(tc.v, tc.sep); got != tc.want {
				t.Errorf("joinAny(%v, %q): got %q want %q", tc.v, tc.sep, got, tc.want)
			}
		})
	}
}

// TestVerbCall pins the exact HTTP method/path/query/form each verb builds —
// proven without sending anything.
func TestVerbCall(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		wantMethod string
		wantPath   string
		wantQuery  url.Values
		wantForm   url.Values
	}{
		{
			name:       "torrents with filters and hashes",
			verb:       "torrents",
			opts:       map[string]any{"filter": "downloading", "category": "movies", "hashes": []any{"h1", "h2"}},
			wantMethod: http.MethodGet,
			wantPath:   "torrents/info",
			wantQuery:  url.Values{"filter": {"downloading"}, "category": {"movies"}, "hashes": {"h1|h2"}},
		},
		{
			name:       "torrent_properties",
			verb:       "torrent_properties",
			opts:       map[string]any{"hash": "abc123"},
			wantMethod: http.MethodGet,
			wantPath:   "torrents/properties",
			wantQuery:  url.Values{"hash": {"abc123"}},
		},
		{
			name: "add with lists",
			verb: "add",
			opts: map[string]any{
				"urls":     []any{"magnet:?xt=urn:btih:aaa", "magnet:?xt=urn:btih:bbb"},
				"category": "movies", "tags": []any{"t1", "t2"}, "paused": true, "savepath": "/dl",
			},
			wantMethod: http.MethodPost,
			wantPath:   "torrents/add",
			wantForm: url.Values{
				"urls":     {"magnet:?xt=urn:btih:aaa\nmagnet:?xt=urn:btih:bbb"},
				"category": {"movies"}, "tags": {"t1,t2"}, "paused": {"true"}, "savepath": {"/dl"},
			},
		},
		{
			name:       "delete",
			verb:       "delete",
			opts:       map[string]any{"hashes": []any{"h1", "h2"}, "delete_files": true},
			wantMethod: http.MethodPost,
			wantPath:   "torrents/delete",
			wantForm:   url.Values{"hashes": {"h1|h2"}, "deleteFiles": {"true"}},
		},
		{
			name:       "pause all",
			verb:       "pause",
			opts:       map[string]any{"hashes": "all"},
			wantMethod: http.MethodPost,
			wantPath:   "torrents/pause",
			wantForm:   url.Values{"hashes": {"all"}},
		},
		{
			name:       "resume",
			verb:       "resume",
			opts:       map[string]any{"hashes": "h1"},
			wantMethod: http.MethodPost,
			wantPath:   "torrents/resume",
			wantForm:   url.Values{"hashes": {"h1"}},
		},
		{
			name:       "recheck",
			verb:       "recheck",
			opts:       map[string]any{"hashes": "h1"},
			wantMethod: http.MethodPost,
			wantPath:   "torrents/recheck",
			wantForm:   url.Values{"hashes": {"h1"}},
		},
		{
			name:       "set_category",
			verb:       "set_category",
			opts:       map[string]any{"hashes": "h1", "category": "tv"},
			wantMethod: http.MethodPost,
			wantPath:   "torrents/setCategory",
			wantForm:   url.Values{"hashes": {"h1"}, "category": {"tv"}},
		},
		{
			name:       "add_tags",
			verb:       "add_tags",
			opts:       map[string]any{"hashes": "h1", "tags": []any{"a", "b"}},
			wantMethod: http.MethodPost,
			wantPath:   "torrents/addTags",
			wantForm:   url.Values{"hashes": {"h1"}, "tags": {"a,b"}},
		},
		{
			name:       "remove_tags",
			verb:       "remove_tags",
			opts:       map[string]any{"hashes": "h1", "tags": "a"},
			wantMethod: http.MethodPost,
			wantPath:   "torrents/removeTags",
			wantForm:   url.Values{"hashes": {"h1"}, "tags": {"a"}},
		},
		{
			name:       "transfer_info",
			verb:       "transfer_info",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "transfer/info",
		},
		{
			name:       "app_version",
			verb:       "app_version",
			opts:       map[string]any{},
			wantMethod: http.MethodGet,
			wantPath:   "app/version",
		},
		{
			name:       "api escape hatch GET",
			verb:       "api",
			opts:       map[string]any{"method": "get", "path": "/sync/maindata", "params": map[string]any{"rid": 1}},
			wantMethod: http.MethodGet,
			wantPath:   "sync/maindata",
			wantQuery:  url.Values{"rid": {"1"}},
		},
		{
			name:       "api escape hatch POST",
			verb:       "api",
			opts:       map[string]any{"method": "POST", "path": "torrents/topPrio", "params": map[string]any{"hashes": "h1"}},
			wantMethod: http.MethodPost,
			wantPath:   "torrents/topPrio",
			wantForm:   url.Values{"hashes": {"h1"}},
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
			if tc.wantQuery != nil && call.query.Encode() != tc.wantQuery.Encode() {
				t.Errorf("query: got %q want %q", call.query.Encode(), tc.wantQuery.Encode())
			}
			if tc.wantForm != nil && call.form.Encode() != tc.wantForm.Encode() {
				t.Errorf("form: got %q want %q", call.form.Encode(), tc.wantForm.Encode())
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
		{"torrent_properties", map[string]any{}},              // no hash
		{"add", map[string]any{}},                             // no urls
		{"delete", map[string]any{}},                          // no hashes
		{"pause", map[string]any{}},                           // no hashes
		{"resume", map[string]any{}},                          // no hashes
		{"recheck", map[string]any{}},                         // no hashes
		{"set_category", map[string]any{"hashes": "h1"}},      // no category
		{"set_category", map[string]any{"category": "x"}},     // no hashes
		{"add_tags", map[string]any{"hashes": "h1"}},          // no tags
		{"remove_tags", map[string]any{"tags": "x"}},          // no hashes
		{"api", map[string]any{}},                             // no method/path
		{"api", map[string]any{"method": "GET"}},              // no path
		{"api", map[string]any{"method": "PUT", "path": "x"}}, // bad method
		{"nope", map[string]any{}},                            // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbCall(tc.verb, tc.opts); err == nil {
			t.Errorf("verbCall(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers connection validation and base_url normalization.
func TestParseConn(t *testing.T) {
	if _, err := parseConn(map[string]any{}); err == nil {
		t.Fatal("expected error for missing base_url")
	}
	c, err := parseConn(map[string]any{"base_url": "http://qbit:8080/", "username": "admin", "password": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if c.base != "http://qbit:8080" {
		t.Errorf("base: got %q want trailing slash trimmed", c.base)
	}
	if c.apiBase() != "http://qbit:8080/api/v2" {
		t.Errorf("apiBase: got %q", c.apiBase())
	}
}

// TestInvokeAgainstFakeQbittorrent drives Invoke end to end against an
// httptest.Server: it asserts the client logs in once, sends the SID cookie
// back on subsequent calls, reuses the cached session across Invoke calls,
// and hits the right path/form for torrents/add/delete/pause.
func TestInvokeAgainstFakeQbittorrent(t *testing.T) {
	var loginCount int
	var gotLoginUser, gotLoginPass string
	var gotAddForm url.Values
	var gotDeleteForm url.Values
	var gotPauseForm url.Values

	const sid = "sid-abc123"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		_ = r.ParseForm()
		gotLoginUser = r.PostForm.Get("username")
		gotLoginPass = r.PostForm.Get("password")
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: sid, Path: "/"})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie("SID"); err != nil || ck.Value != sid {
			t.Errorf("torrents/info: missing/wrong SID cookie")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"hash":"h1","name":"a"}]`))
	})
	mux.HandleFunc("/api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie("SID"); err != nil || ck.Value != sid {
			t.Errorf("torrents/add: missing/wrong SID cookie")
		}
		_ = r.ParseForm()
		gotAddForm = r.PostForm
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/delete", func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie("SID"); err != nil || ck.Value != sid {
			t.Errorf("torrents/delete: missing/wrong SID cookie")
		}
		_ = r.ParseForm()
		gotDeleteForm = r.PostForm
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v2/torrents/pause", func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie("SID"); err != nil || ck.Value != sid {
			t.Errorf("torrents/pause: missing/wrong SID cookie")
		}
		_ = r.ParseForm()
		gotPauseForm = r.PostForm
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &qbittorrentPlugin{}
	conn := map[string]any{"base_url": srv.URL, "username": "admin", "password": "secret"}

	t.Run("torrents logs in, sends cookie, returns items", func(t *testing.T) {
		res, err := p.Invoke(plugin.InvokeRequest{Instance: "i1", Verb: "torrents", Connection: conn})
		if err != nil {
			t.Fatal(err)
		}
		if loginCount != 1 {
			t.Fatalf("expected exactly 1 login, got %d", loginCount)
		}
		if gotLoginUser != "admin" || gotLoginPass != "secret" {
			t.Errorf("login form: user=%q pass=%q", gotLoginUser, gotLoginPass)
		}
		items, ok := res.Outputs["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("items: %#v", res.Outputs["items"])
		}
		if res.Outputs["status_code"] != 200 {
			t.Errorf("status_code: %#v", res.Outputs["status_code"])
		}
	})

	t.Run("second call reuses the cached session", func(t *testing.T) {
		if _, err := p.Invoke(plugin.InvokeRequest{Instance: "i1", Verb: "torrents", Connection: conn}); err != nil {
			t.Fatal(err)
		}
		if loginCount != 1 {
			t.Fatalf("expected session reuse (still 1 login), got %d", loginCount)
		}
	})

	t.Run("add sends form fields", func(t *testing.T) {
		res, err := p.Invoke(plugin.InvokeRequest{
			Instance: "i1", Verb: "add", Connection: conn,
			Options: map[string]any{"urls": "magnet:?xt=urn:btih:aaa", "category": "movies"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotAddForm.Get("urls") != "magnet:?xt=urn:btih:aaa" || gotAddForm.Get("category") != "movies" {
			t.Errorf("add form: %#v", gotAddForm)
		}
		if res.Outputs["result"] != "Ok." {
			t.Errorf("add result: %#v", res.Outputs["result"])
		}
	})

	t.Run("delete sends hashes and deleteFiles", func(t *testing.T) {
		if _, err := p.Invoke(plugin.InvokeRequest{
			Instance: "i1", Verb: "delete", Connection: conn,
			Options: map[string]any{"hashes": []any{"h1", "h2"}, "delete_files": true},
		}); err != nil {
			t.Fatal(err)
		}
		if gotDeleteForm.Get("hashes") != "h1|h2" || gotDeleteForm.Get("deleteFiles") != "true" {
			t.Errorf("delete form: %#v", gotDeleteForm)
		}
	})

	t.Run("pause sends hashes=all", func(t *testing.T) {
		if _, err := p.Invoke(plugin.InvokeRequest{
			Instance: "i1", Verb: "pause", Connection: conn,
			Options: map[string]any{"hashes": "all"},
		}); err != nil {
			t.Fatal(err)
		}
		if gotPauseForm.Get("hashes") != "all" {
			t.Errorf("pause form: %#v", gotPauseForm)
		}
	})
}

// TestLoginFailure asserts that qBittorrent's HTTP-200-with-"Fails."-body
// login failure is surfaced as a plugin error, not treated as a success.
func TestLoginFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Fails."))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &qbittorrentPlugin{}
	conn := map[string]any{"base_url": srv.URL, "username": "admin", "password": "wrong"}
	_, err := p.Invoke(plugin.InvokeRequest{Instance: "i2", Verb: "app_version", Connection: conn})
	if err == nil {
		t.Fatal("expected a login failure error")
	}
	var re *plugin.Error
	if !asPluginError(err, &re) || re.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(re.Message, "login") {
		t.Errorf("error message should mention login: %q", re.Message)
	}
}

// TestNoAuthConfigured proves a connection without username/password never
// attempts to log in (a host with authentication disabled).
func TestNoAuthConfigured(t *testing.T) {
	var loginCount int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("v4.6.0"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &qbittorrentPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "i3", Verb: "app_version",
		Connection: map[string]any{"base_url": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if loginCount != 0 {
		t.Fatalf("expected no login attempt without credentials, got %d", loginCount)
	}
	if res.Outputs["result"] != "v4.6.0" {
		t.Errorf("result: %#v", res.Outputs["result"])
	}
}

// TestNon2xxIsInternalError asserts a non-2xx API response surfaces as
// CodeInternalError carrying the status and body.
func TestNon2xxIsInternalError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/torrents/properties", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Torrent hash was not found"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &qbittorrentPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "i4", Verb: "torrent_properties",
		Connection: map[string]any{"base_url": srv.URL},
		Options:    map[string]any{"hash": "missing"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var re *plugin.Error
	if !asPluginError(err, &re) || re.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(re.Message, "404") || !strings.Contains(re.Message, "not found") {
		t.Errorf("error message should carry status and body: %q", re.Message)
	}
}

func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}
