package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn builds a connection map pointed at srv.
func testConn(srv *httptest.Server, unifiOS bool) map[string]any {
	return map[string]any{
		"base_url": srv.URL,
		"username": "admin",
		"password": "hunter2",
		"site":     "default",
		"unifi_os": unifiOS,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// hasCookie reports whether the request carries a cookie named name.
func hasCookie(r *http.Request, name string) bool {
	c, err := r.Cookie(name)
	return err == nil && c.Value != ""
}

// --- login + prefix + cookie reuse ---

func TestLoginOnceAndCookieReusedUniFiOS(t *testing.T) {
	var loginCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			loginCount++
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("login body decode: %v", err)
			}
			if body["username"] != "admin" || body["password"] != "hunter2" {
				t.Errorf("login body: %#v", body)
			}
			http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "sess-abc", Path: "/"})
			w.Header().Set("X-CSRF-Token", "csrf-1")
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/network/api/self/sites":
			if !hasCookie(r, "unifises") {
				t.Errorf("sites request missing session cookie")
			}
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{
				map[string]any{"name": "default"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/network/api/s/default/stat/device":
			if !hasCookie(r, "unifises") {
				t.Errorf("devices request missing session cookie")
			}
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{
				map[string]any{"mac": "aa:bb:cc:dd:ee:ff"},
			}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newUnifiPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "sites", Connection: testConn(srv, true)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("sites items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "devices", Connection: testConn(srv, true)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok = res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("devices items: %#v", res.Outputs["items"])
	}
	if got := items[0].(map[string]any)["mac"]; got != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("devices[0].mac: %#v", got)
	}

	if loginCount != 1 {
		t.Errorf("loginCount: got %d want 1 (session should be cached and reused)", loginCount)
	}
}

// --- legacy (non UniFi-OS) controller: no /proxy/network prefix, /api/login ---

func TestLegacyControllerLoginAndPrefix(t *testing.T) {
	var loginCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/login":
			loginCount++
			http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "sess-legacy", Path: "/"})
			// Legacy controllers do not return an X-CSRF-Token.
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/s/default/stat/device":
			if !hasCookie(r, "unifises") {
				t.Errorf("devices request missing session cookie")
			}
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{
				map[string]any{"mac": "11:22:33:44:55:66"},
			}})
		default:
			t.Errorf("unexpected request (legacy controller must not use /proxy/network): %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newUnifiPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "devices", Connection: testConn(srv, false)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["mac"] != "11:22:33:44:55:66" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if loginCount != 1 {
		t.Errorf("loginCount: got %d want 1", loginCount)
	}
}

// --- CSRF echoed on a mutating (POST) request ---

func TestCSRFEchoedOnDeviceRestart(t *testing.T) {
	const csrfToken = "csrf-xyz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "sess-abc", Path: "/"})
			w.Header().Set("X-CSRF-Token", csrfToken)
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/network/api/s/default/cmd/devmgr":
			if got := r.Header.Get("X-CSRF-Token"); got != csrfToken {
				t.Errorf("X-CSRF-Token: got %q want %q", got, csrfToken)
			}
			if !hasCookie(r, "unifises") {
				t.Errorf("devmgr request missing session cookie")
			}
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("body decode: %v", err)
			}
			if body["cmd"] != "restart" || body["mac"] != "aa:bb:cc:dd:ee:ff" {
				t.Errorf("devmgr body: %#v", body)
			}
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{
				map[string]any{"mac": "aa:bb:cc:dd:ee:ff"},
			}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newUnifiPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "device_restart", Connection: testConn(srv, true),
		Options: map[string]any{"mac": "aa:bb:cc:dd:ee:ff"},
	})
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
}

// --- client_block / client_unblock / client_reconnect send the right cmd ---

func TestClientCommands(t *testing.T) {
	cases := []struct {
		verb string
		cmd  string
	}{
		{"client_block", "block-sta"},
		{"client_unblock", "unblock-sta"},
		{"client_reconnect", "kick-sta"},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
					http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "sess-abc", Path: "/"})
					w.Header().Set("X-CSRF-Token", "csrf-1")
					writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{}})
				case r.Method == http.MethodPost && r.URL.Path == "/proxy/network/api/s/default/cmd/stamgr":
					raw, _ := io.ReadAll(r.Body)
					if err := json.Unmarshal(raw, &gotBody); err != nil {
						t.Fatalf("body decode: %v", err)
					}
					writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{}})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()

			p := newUnifiPlugin()
			_, err := p.Invoke(plugin.InvokeRequest{
				Verb: tc.verb, Connection: testConn(srv, true),
				Options: map[string]any{"mac": "aa:bb:cc:dd:ee:ff"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if gotBody["cmd"] != tc.cmd || gotBody["mac"] != "aa:bb:cc:dd:ee:ff" {
				t.Errorf("body: got %#v want cmd=%q mac=aa:bb:cc:dd:ee:ff", gotBody, tc.cmd)
			}
		})
	}
}

// --- data hoist ---

func TestDataHoistIntoItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "sess-abc", Path: "/"})
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/network/api/s/default/stat/sta":
			writeJSON(w, 200, map[string]any{
				"meta": map[string]any{"rc": "ok"},
				"data": []any{
					map[string]any{"mac": "a1"}, map[string]any{"mac": "a2"},
				},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newUnifiPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "clients", Connection: testConn(srv, true)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	if _, has := res.Outputs["result"]; has {
		t.Errorf("did not expect result when data is a list: %#v", res.Outputs["result"])
	}
}

// --- non-2xx -> CodeInternalError ---

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "sess-abc", Path: "/"})
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/network/api/s/default/stat/device":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"meta":{"rc":"error","msg":"boom"}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newUnifiPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "devices", Connection: testConn(srv, true)})
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
	if !strings.Contains(pe.Message, "500") || !strings.Contains(pe.Message, "boom") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

// --- 401 triggers a re-login and retry ---

func TestReloginOn401(t *testing.T) {
	var loginCount int
	var deviceHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			loginCount++
			http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "sess-abc", Path: "/"})
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/network/api/s/default/stat/sta":
			deviceHits++
			if deviceHits == 1 {
				// Simulate an expired session on the very first attempt.
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"meta":{"rc":"error","msg":"api.err.LoginRequired"}}`))
				return
			}
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{
				map[string]any{"mac": "a1"},
			}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newUnifiPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "clients", Connection: testConn(srv, true)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if loginCount != 2 {
		t.Errorf("loginCount: got %d want 2 (initial + re-login after 401)", loginCount)
	}
	if deviceHits != 2 {
		t.Errorf("deviceHits: got %d want 2 (initial 401 + retry)", deviceHits)
	}
}

// --- api escape hatch ---

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "sess-abc", Path: "/"})
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/network/api/s/default/stat/health" && r.URL.Query().Get("verbose") == "1":
			writeJSON(w, 200, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{
				map[string]any{"subsystem": "wlan", "status": "ok"},
			}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newUnifiPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv, true),
		Options: map[string]any{"path": "/api/s/default/stat/health", "query": map[string]any{"verbose": "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

// --- missing connection fields ---

func TestMissingConnection(t *testing.T) {
	p := newUnifiPlugin()
	base := map[string]any{"base_url": "http://example.invalid", "username": "u", "password": "p"}

	cases := []struct {
		name string
		conn map[string]any
	}{
		{"missing base_url", map[string]any{"username": "u", "password": "p"}},
		{"missing username", map[string]any{"base_url": "http://x", "password": "p"}},
		{"missing password", map[string]any{"base_url": "http://x", "username": "u"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Invoke(plugin.InvokeRequest{Verb: "devices", Connection: tc.conn})
			if err == nil {
				t.Fatal("expected error")
			}
			pe, ok := err.(*plugin.Error)
			if !ok || pe.Code != plugin.CodeInvalidParams {
				t.Fatalf("expected CodeInvalidParams, got %v", err)
			}
		})
	}

	// Sanity: a fully-populated connection does not error at parse time
	// (it just never reaches the network in this sub-test).
	if _, err := parseConn(base); err != nil {
		t.Errorf("parseConn: unexpected error: %v", err)
	}
}

// --- missing required verb options ---

func TestMissingRequiredOptions(t *testing.T) {
	p := newUnifiPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "username": "u", "password": "p"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"device_restart", map[string]any{}},
		{"client_block", map[string]any{}},
		{"client_unblock", map[string]any{}},
		{"client_reconnect", map[string]any{}},
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
	p := newUnifiPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "username": "u", "password": "p"}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: conn})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

// --- Describe ---

func TestDescribe(t *testing.T) {
	p := newUnifiPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "unifi" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	for _, key := range []string{"base_url", "username", "password"} {
		f, ok := d.Connection[key]
		if !ok || f.Type != "string" || !f.Required {
			t.Errorf("connection.%s: %#v", key, f)
		}
	}
	if f := d.Connection["site"]; f.Required {
		t.Errorf("connection.site should be optional: %#v", f)
	}
	if f := d.Connection["unifi_os"]; f.Type != "boolean" || f.Required {
		t.Errorf("connection.unifi_os: %#v", f)
	}
	if f := d.Connection["insecure_skip_verify"]; f.Type != "boolean" || f.Required {
		t.Errorf("connection.insecure_skip_verify: %#v", f)
	}

	want := []string{
		"sites", "devices", "device_restart", "clients", "client_block",
		"client_unblock", "client_reconnect", "wlans", "networks",
		"port_forwards", "firewall_rules", "health", "alarms", "events", "api",
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

// --- insecure_skip_verify parsing (no network call; just the connection field) ---

func TestParseConnDefaults(t *testing.T) {
	conn, err := parseConn(map[string]any{"base_url": "https://unifi.example.com/", "username": "u", "password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if conn.baseURL != "https://unifi.example.com" {
		t.Errorf("baseURL should have trailing slash trimmed: %q", conn.baseURL)
	}
	if conn.site != "default" {
		t.Errorf("site default: got %q want %q", conn.site, "default")
	}
	if !conn.unifiOS {
		t.Errorf("unifi_os default: got false want true")
	}
	if conn.insecureSkipVerify {
		t.Errorf("insecure_skip_verify default: got true want false")
	}
	if conn.prefix() != "/proxy/network" {
		t.Errorf("prefix: got %q want /proxy/network", conn.prefix())
	}
	if conn.loginPath() != "/api/auth/login" {
		t.Errorf("loginPath: got %q want /api/auth/login", conn.loginPath())
	}

	legacy, err := parseConn(map[string]any{"base_url": "https://10.0.0.1", "username": "u", "password": "p", "unifi_os": false})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.prefix() != "" {
		t.Errorf("legacy prefix: got %q want \"\"", legacy.prefix())
	}
	if legacy.loginPath() != "/api/login" {
		t.Errorf("legacy loginPath: got %q want /api/login", legacy.loginPath())
	}
}
