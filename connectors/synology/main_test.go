package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn builds a connection map pointed at srv.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{
		"base_url": srv.URL,
		"username": "admin",
		"password": "hunter2",
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// synoOK builds a success:true envelope carrying data.
func synoOK(data any) map[string]any {
	return map[string]any{"success": true, "data": data}
}

// synoFail builds a success:false envelope carrying an error code.
func synoFail(code int) map[string]any {
	return map[string]any{"success": false, "error": map[string]any{"code": code}}
}

// formValue reads a value from either the query string (GET) or the
// form-encoded body (POST) — r.ParseForm merges both into r.Form.
func formValue(t *testing.T, r *http.Request, key string) string {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	return r.Form.Get(key)
}

// --- login + sid reuse ---

func TestLoginOnceAndSidReused(t *testing.T) {
	var loginCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/webapi/auth.cgi":
			loginCount++
			if formValue(t, r, "api") != "SYNO.API.Auth" || formValue(t, r, "method") != "login" {
				t.Errorf("login request: %s", r.URL.RawQuery)
			}
			if formValue(t, r, "account") != "admin" || formValue(t, r, "passwd") != "hunter2" {
				t.Errorf("login credentials: %s", r.URL.RawQuery)
			}
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-abc"}))
		case r.URL.Path == "/webapi/entry.cgi":
			if formValue(t, r, "_sid") != "sid-abc" {
				t.Errorf("_sid missing/wrong: %s", r.URL.RawQuery)
			}
			switch formValue(t, r, "api") {
			case "SYNO.Core.System":
				writeJSON(w, 200, synoOK(map[string]any{"model": "DS920+"}))
			case "SYNO.Core.System.Utilization":
				writeJSON(w, 200, synoOK(map[string]any{"cpu": map[string]any{"user_load": 5}}))
			default:
				t.Errorf("unexpected api: %s", r.URL.RawQuery)
			}
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "system_info", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["model"] != "DS920+" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "utilization", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}

	if loginCount != 1 {
		t.Errorf("loginCount: got %d want 1 (sid should be cached and reused)", loginCount)
	}
}

// --- otp_code sent on login ---

func TestLoginSendsOTPCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			if formValue(t, r, "otp_code") != "654321" {
				t.Errorf("otp_code: got %q", formValue(t, r, "otp_code"))
			}
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-abc"}))
		case "/webapi/entry.cgi":
			writeJSON(w, 200, synoOK(map[string]any{"model": "DS920+"}))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	conn := testConn(srv)
	conn["otp_code"] = "654321"
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "system_info", Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
}

// --- a verb sends the right api/method/_sid, and GET vs POST ---

func TestFsListSendsRightRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-abc"}))
		case "/webapi/entry.cgi":
			if r.Method != http.MethodGet {
				t.Errorf("method: got %s want GET", r.Method)
			}
			if formValue(t, r, "api") != "SYNO.FileStation.List" || formValue(t, r, "method") != "list" || formValue(t, r, "version") != "2" {
				t.Errorf("request: %s", r.URL.RawQuery)
			}
			if formValue(t, r, "folder_path") != "/volume1/photo" {
				t.Errorf("folder_path: %s", r.URL.RawQuery)
			}
			if formValue(t, r, "additional") != "size,time" {
				t.Errorf("additional: %s", r.URL.RawQuery)
			}
			writeJSON(w, 200, synoOK(map[string]any{"files": []any{map[string]any{"name": "a.jpg"}}}))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "fs_list", Connection: testConn(srv),
		Options: map[string]any{"folder_path": "/volume1/photo", "additional": []any{"size", "time"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["name"] != "a.jpg" {
		t.Fatalf("items (hoisted from data.files): %#v", res.Outputs["items"])
	}
}

func TestDlCreateIsPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-abc"}))
		case "/webapi/entry.cgi":
			if r.Method != http.MethodPost {
				t.Errorf("method: got %s want POST", r.Method)
			}
			if formValue(t, r, "method") != "create" || formValue(t, r, "uri") != "magnet:one,magnet:two" {
				t.Errorf("request form: %v", r.Form)
			}
			writeJSON(w, 200, synoOK(nil))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "dl_create", Connection: testConn(srv),
		Options: map[string]any{"uri": []any{"magnet:one", "magnet:two"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

// --- storage/dl_tasks hoist a list ---

func TestStorageHoistsVolumesIntoItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-abc"}))
		case "/webapi/entry.cgi":
			if formValue(t, r, "api") != "SYNO.Storage.CGI.Storage" || formValue(t, r, "method") != "load_info" {
				t.Errorf("request: %s", r.URL.RawQuery)
			}
			writeJSON(w, 200, synoOK(map[string]any{"volumes": []any{map[string]any{"id": "volume_1"}}}))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "storage", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "volume_1" {
		t.Fatalf("items (hoisted from data.volumes): %#v", res.Outputs["items"])
	}
}

func TestDlTasksHoistsTasksIntoItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-abc"}))
		case "/webapi/entry.cgi":
			writeJSON(w, 200, synoOK(map[string]any{"tasks": []any{map[string]any{"id": "dbid_1"}}}))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "dl_tasks", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "dbid_1" {
		t.Fatalf("items (hoisted from data.tasks): %#v", res.Outputs["items"])
	}
}

// --- success:false -> CodeInternalError ---

func TestSynoFailureIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-abc"}))
		case "/webapi/entry.cgi":
			// 102: the requested api does not exist — not a session-expired code.
			writeJSON(w, 200, synoFail(102))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "system_info", Connection: testConn(srv)})
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
	if !contains(pe.Message, "SYNO error 102") {
		t.Errorf("message should carry the SYNO error code, got %q", pe.Message)
	}
}

func TestLoginFailureIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, synoFail(400))
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "system_info", Connection: testConn(srv)})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
	if !contains(pe.Message, "SYNO error 400") {
		t.Errorf("message should carry the SYNO error code, got %q", pe.Message)
	}
}

// --- session-expired code triggers re-login ---

func TestSessionExpiredTriggersRelogin(t *testing.T) {
	var loginCount, entryHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			loginCount++
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-" + formValue(t, r, "account")}))
		case "/webapi/entry.cgi":
			entryHits++
			if entryHits == 1 {
				// Simulate an expired/invalid session on the first attempt.
				writeJSON(w, 200, synoFail(119))
				return
			}
			writeJSON(w, 200, synoOK(map[string]any{"model": "DS920+"}))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "system_info", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["model"] != "DS920+" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if loginCount != 2 {
		t.Errorf("loginCount: got %d want 2 (initial + re-login after session-expired code)", loginCount)
	}
	if entryHits != 2 {
		t.Errorf("entryHits: got %d want 2 (initial failure + retry)", entryHits)
	}
}

// A non-session-expired failure code (e.g. 102) must NOT trigger a re-login.
func TestNonSessionExpiredCodeDoesNotRelogin(t *testing.T) {
	var loginCount, entryHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			loginCount++
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-abc"}))
		case "/webapi/entry.cgi":
			entryHits++
			writeJSON(w, 200, synoFail(102))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "system_info", Connection: testConn(srv)})
	if err == nil {
		t.Fatal("expected error")
	}
	if loginCount != 1 {
		t.Errorf("loginCount: got %d want 1 (no re-login for a non-session-expired code)", loginCount)
	}
	if entryHits != 1 {
		t.Errorf("entryHits: got %d want 1 (no retry for a non-session-expired code)", entryHits)
	}
}

// --- api escape hatch ---

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-abc"}))
		case "/webapi/entry.cgi":
			if formValue(t, r, "api") != "SYNO.Core.System" || formValue(t, r, "method") != "info" || formValue(t, r, "version") != "3" {
				t.Errorf("request: %s", r.URL.RawQuery)
			}
			if formValue(t, r, "extra") != "1" {
				t.Errorf("params not forwarded: %s", r.URL.RawQuery)
			}
			writeJSON(w, 200, synoOK(map[string]any{"model": "DS920+"}))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{
			"api": "SYNO.Core.System", "method": "info", "version": 3,
			"params": map[string]any{"extra": "1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["model"] != "DS920+" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatchDefaultsVersionAndHTTPMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			writeJSON(w, 200, synoOK(map[string]any{"sid": "sid-abc"}))
		case "/webapi/entry.cgi":
			if r.Method != http.MethodGet {
				t.Errorf("method: got %s want GET", r.Method)
			}
			if formValue(t, r, "version") != "1" {
				t.Errorf("version default: got %q want 1", formValue(t, r, "version"))
			}
			writeJSON(w, 200, synoOK([]any{map[string]any{"id": "x"}}))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSynologyPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"api": "SYNO.Foo", "method": "bar"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

// --- missing connection / required options / unknown verb ---

func TestMissingConnection(t *testing.T) {
	p := newSynologyPlugin()
	cases := []map[string]any{
		{"username": "u", "password": "p"},
		{"base_url": "http://x", "password": "p"},
		{"base_url": "http://x", "username": "u"},
	}
	for _, conn := range cases {
		if _, err := p.Invoke(plugin.InvokeRequest{Verb: "system_info", Connection: conn}); err == nil {
			t.Errorf("expected error for connection %#v", conn)
		}
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newSynologyPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "username": "u", "password": "p"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"fs_list", map[string]any{}},
		{"fs_info", map[string]any{}},
		{"fs_search", map[string]any{}},
		{"dl_create", map[string]any{}},
		{"dl_delete", map[string]any{}},
		{"api", map[string]any{}},
		{"api", map[string]any{"api": "SYNO.Foo"}},
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
	p := newSynologyPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{"base_url": "http://x", "username": "u", "password": "p"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- Describe ---

func TestDescribe(t *testing.T) {
	p := newSynologyPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "synology" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	for _, key := range []string{"base_url", "username", "password"} {
		f := d.Connection[key]
		if f.Type != "string" || !f.Required {
			t.Errorf("connection.%s: %#v", key, f)
		}
	}
	if f := d.Connection["otp_code"]; f.Type != "string" || f.Required {
		t.Errorf("connection.otp_code: %#v", f)
	}
	if f := d.Connection["insecure_skip_verify"]; f.Type != "boolean" || f.Required {
		t.Errorf("connection.insecure_skip_verify: %#v", f)
	}

	want := []string{
		"system_info", "utilization", "storage", "fs_list", "fs_info",
		"fs_search", "dl_tasks", "dl_create", "dl_delete", "api",
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

// --- parseConn defaults ---

func TestParseConnDefaults(t *testing.T) {
	conn, err := parseConn(map[string]any{"base_url": "http://x/", "username": "u", "password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if conn.baseURL != "http://x" {
		t.Errorf("baseURL should trim trailing slash: got %q", conn.baseURL)
	}
	if conn.insecureSkipVerify {
		t.Errorf("insecure_skip_verify default: got true want false")
	}
	if conn.otpCode != "" {
		t.Errorf("otp_code default: got %q want \"\"", conn.otpCode)
	}
}

// --- joinAny ---

func TestJoinAny(t *testing.T) {
	cases := []struct {
		in   any
		sep  string
		want string
	}{
		{nil, ",", ""},
		{"a", ",", "a"},
		{[]string{"a", "b"}, ",", "a,b"},
		{[]any{"a", "b"}, "|", "a|b"},
		{[]any{}, ",", ""},
	}
	for _, tc := range cases {
		if got := joinAny(tc.in, tc.sep); got != tc.want {
			t.Errorf("joinAny(%#v, %q): got %q want %q", tc.in, tc.sep, got, tc.want)
		}
	}
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
