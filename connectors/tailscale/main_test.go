package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// stubTool writes an executable named name into a dir prepended to PATH.
func stubTool(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// testConnManaged builds a connection map carrying conductor's managed
// OAuth2 token (the preferred credential path).
func testConnManaged(srv *httptest.Server) map[string]any {
	return map[string]any{
		plugin.AccessTokenKey: "managed-tok",
		"tailnet":             "example.com",
		"api_base":            srv.URL,
	}
}

// testConnAPIKey builds a connection map with NO managed token, only the
// plain api_key fallback.
func testConnAPIKey(srv *httptest.Server) map[string]any {
	return map[string]any{
		"tailnet":  "example.com",
		"api_key":  "tskey-api-fallback",
		"api_base": srv.URL,
	}
}

func checkBearer(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+want {
		t.Errorf("Authorization header: got %q want %q", got, "Bearer "+want)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestManagedTokenTakesPriority(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		writeJSON(w, 200, map[string]any{"devices": []any{}})
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	// Both a managed token AND an api_key are present; the managed token wins.
	conn := testConnManaged(srv)
	conn["api_key"] = "should-not-be-used"
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "devices", Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestAPIKeyFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "tskey-api-fallback")
		writeJSON(w, 200, map[string]any{"devices": []any{}})
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "devices", Connection: testConnAPIKey(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestNoCredentialsIsInvalidParams(t *testing.T) {
	p := newTailscalePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "devices",
		Connection: map[string]any{"tailnet": "example.com"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing credentials, got %v", err)
	}
	if !strings.Contains(pe.Message, "conductor connector auth tailscale") {
		t.Errorf("message should point at auth setup, got %q", pe.Message)
	}
}

func TestMissingTailnet(t *testing.T) {
	p := newTailscalePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "devices",
		Connection: map[string]any{plugin.AccessTokenKey: "t"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing tailnet, got %v", err)
	}
}

func TestDevicesHoistsItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		if r.URL.Path != "/api/v2/tailnet/example.com/devices" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"devices": []any{
			map[string]any{"id": "dev1", "hostname": "laptop"},
		}})
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "devices", Connection: testConnManaged(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if items[0].(map[string]any)["id"] != "dev1" {
		t.Errorf("items[0]: %#v", items[0])
	}
}

func TestKeysHoistsItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		if r.URL.Path != "/api/v2/tailnet/example.com/keys" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"keys": []any{
			map[string]any{"id": "key1"},
		}})
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "keys", Connection: testConnManaged(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "key1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestDeviceAuthorizeSendsBody(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		gotMethod, gotPath = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "device_authorize", Connection: testConnManaged(srv),
		Options: map[string]any{"device_id": "dev1", "authorized": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v2/device/dev1/authorized" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	want := map[string]any{"authorized": true}
	if gotBody["authorized"] != want["authorized"] {
		t.Errorf("body: got %#v want %#v", gotBody, want)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestDeviceSetTagsSendsBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/device/dev1/tags" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "device_set_tags", Connection: testConnManaged(srv),
		Options: map[string]any{"device_id": "dev1", "tags": []any{"tag:server"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tags, ok := gotBody["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "tag:server" {
		t.Fatalf("body: got %#v", gotBody)
	}
}

func TestDeviceGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		if r.URL.Path != "/api/v2/device/dev1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "dev1", "hostname": "laptop"})
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "device_get", Connection: testConnManaged(srv),
		Options: map[string]any{"device_id": "dev1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["hostname"] != "laptop" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestDeviceDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v2/device/dev1" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "device_delete", Connection: testConnManaged(srv),
		Options: map[string]any{"device_id": "dev1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestACLGetAndSet(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/tailnet/example.com/acl":
			writeJSON(w, 200, map[string]any{"acls": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/tailnet/example.com/acl":
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatalf("body decode: %v", err)
			}
			writeJSON(w, 200, map[string]any{"ok": true})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "acl_get", Connection: testConnManaged(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "acl_set", Connection: testConnManaged(srv),
		Options: map[string]any{"acl": map[string]any{"acls": []any{map[string]any{"action": "accept"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["acls"] == nil {
		t.Errorf("acl_set body: %#v", gotBody)
	}
}

func TestDNSVerbs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		switch r.URL.Path {
		case "/api/v2/tailnet/example.com/dns/nameservers":
			if r.Method == http.MethodGet {
				writeJSON(w, 200, map[string]any{"dns": []any{"1.1.1.1"}})
			} else {
				writeJSON(w, 200, map[string]any{"dns": []any{"8.8.8.8"}})
			}
		case "/api/v2/tailnet/example.com/dns/preferences":
			writeJSON(w, 200, map[string]any{"magicDNS": true})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newTailscalePlugin()

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "dns_nameservers", Connection: testConnManaged(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if result, ok := res.Outputs["result"].(map[string]any); !ok || result["dns"] == nil {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "dns_set_nameservers", Connection: testConnManaged(srv),
		Options: map[string]any{"dns": []any{"8.8.8.8"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "dns_preferences", Connection: testConnManaged(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if result, ok := res.Outputs["result"].(map[string]any); !ok || result["magicDNS"] != true {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestKeyCreateGetDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/tailnet/example.com/keys":
			writeJSON(w, 200, map[string]any{"id": "key1", "key": "tskey-auth-..."})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/tailnet/example.com/keys/key1":
			writeJSON(w, 200, map[string]any{"id": "key1"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v2/tailnet/example.com/keys/key1":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newTailscalePlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "key_create", Connection: testConnManaged(srv),
		Options: map[string]any{"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, ok := res.Outputs["result"].(map[string]any); !ok || result["id"] != "key1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "key_get", Connection: testConnManaged(srv), Options: map[string]any{"key_id": "key1"}})
	if err != nil {
		t.Fatal(err)
	}
	if result, ok := res.Outputs["result"].(map[string]any); !ok || result["id"] != "key1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "key_delete", Connection: testConnManaged(srv), Options: map[string]any{"key_id": "key1"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestDeviceRoutesGetAndSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/device/dev1/routes":
			writeJSON(w, 200, map[string]any{"advertisedRoutes": []any{"10.0.0.0/24"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/device/dev1/routes":
			writeJSON(w, 200, map[string]any{"enabledRoutes": []any{"10.0.0.0/24"}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "device_routes", Connection: testConnManaged(srv), Options: map[string]any{"device_id": "dev1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "device_set_routes", Connection: testConnManaged(srv),
		Options: map[string]any{"device_id": "dev1", "routes": []any{"10.0.0.0/24"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "managed-tok")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/tailnet/example.com/devices" && r.URL.Query().Get("fields") == "all":
			writeJSON(w, 200, []any{map[string]any{"id": "dev1"}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConnManaged(srv),
		Options: map[string]any{"path": "/tailnet/example.com/devices", "query": map[string]any{"fields": "all"}},
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
		_, _ = w.Write([]byte(`{"message":"invalid token"}`))
	}))
	defer srv.Close()

	p := newTailscalePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "devices", Connection: testConnManaged(srv)})
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
	if !strings.Contains(pe.Message, "401") || !strings.Contains(pe.Message, "invalid token") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newTailscalePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{plugin.AccessTokenKey: "t", "tailnet": "example.com"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for unknown verb, got %v", err)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newTailscalePlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t", "tailnet": "example.com"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"device_get", map[string]any{}},
		{"device_delete", map[string]any{}},
		{"device_authorize", map[string]any{"device_id": "dev1"}},
		{"device_set_tags", map[string]any{"device_id": "dev1"}},
		{"device_routes", map[string]any{}},
		{"device_set_routes", map[string]any{"device_id": "dev1"}},
		{"key_get", map[string]any{}},
		{"key_create", map[string]any{}},
		{"key_delete", map[string]any{}},
		{"acl_set", map[string]any{}},
		{"dns_set_nameservers", map[string]any{}},
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

func TestDescribe(t *testing.T) {
	p := newTailscalePlugin()
	d := p.Describe()

	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "tailscale" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["tailnet"].Type != "string" || !d.Connection["tailnet"].Required {
		t.Errorf("connection.tailnet: %#v", d.Connection["tailnet"])
	}
	if d.Connection["api_key"].Type != "string" || d.Connection["api_key"].Required {
		t.Errorf("connection.api_key: %#v", d.Connection["api_key"])
	}

	want := []string{
		"devices", "device_get", "device_delete", "device_authorize", "device_set_tags",
		"device_routes", "device_set_routes", "keys", "key_get", "key_create", "key_delete",
		"acl_get", "acl_set", "dns_nameservers", "dns_set_nameservers", "dns_preferences", "api",
		"funnel_open", "funnel_close",
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

	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "api.tailscale.com:443" {
		t.Errorf("Capabilities.Egress: got %v", d.Capabilities.Egress)
	}

	if d.Auth == nil {
		t.Fatal("expected Describe().Auth to be non-nil (managed OAuth2 client_credentials)")
	}
	if d.Auth.TokenURL != "https://api.tailscale.com/api/v2/oauth/token" {
		t.Errorf("Auth.TokenURL: got %q", d.Auth.TokenURL)
	}
	foundClientCreds := false
	for _, g := range d.Auth.Grants {
		if g == "client_credentials" {
			foundClientCreds = true
		}
	}
	if !foundClientCreds {
		t.Errorf("Auth.Grants missing client_credentials: %v", d.Auth.Grants)
	}
}

// ---- funnel_open / funnel_close (the local tailscale CLI, not the API) ----

func TestFunnelOpenNeedsNoAPICredentials(t *testing.T) {
	stubTool(t, "tailscale", `case "$1" in
  serve|funnel) echo "Available at https://box.tailnet.ts.net/" ;;
  status) echo '{"Self":{"DNSName":"box.tailnet.ts.net."}}' ;;
esac`)
	p := newTailscalePlugin()
	// No tailnet, no api_key, no managed token — funnel_open must not need them.
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}})
	if err != nil || !strings.HasPrefix(res.Outputs["public_url"].(string), "https://box.tailnet.ts.net") {
		t.Fatalf("funnel_open: %v %v", res.Outputs, err)
	}
	lease, _ := res.Outputs["lease"].(string)
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_close", Options: map[string]any{"lease": lease}}); err != nil {
		t.Fatal(err)
	}
	if p.leases.Len() != 0 {
		t.Fatalf("leases after close = %d, want 0", p.leases.Len())
	}
}

func TestFunnelStatusFallbackWhenNoURLInOutput(t *testing.T) {
	stubTool(t, "tailscale", `case "$1" in
  serve|funnel) echo "ok" ;;
  status) echo '{"Self":{"DNSName":"fb.tailnet.ts.net."}}' ;;
esac`)
	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"},
		Connection: map[string]any{"funnel_mode": "serve"},
	})
	if err != nil || res.Outputs["public_url"] != "https://fb.tailnet.ts.net" {
		t.Fatalf("funnel_open status fallback: %v %v", res.Outputs, err)
	}
	p.leases.Release(res.Outputs["lease"].(string))
}

func TestFunnelOpenBadLocalAddrErrors(t *testing.T) {
	p := newTailscalePlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "bad"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestFunnelOpenBadModeErrors(t *testing.T) {
	p := newTailscalePlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"funnel_mode": "bogus"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestFunnelOpenMissingBinaryErrorsClearly(t *testing.T) {
	p := newTailscalePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"binary": "no-such-tailscale-xyz"}})
	if err == nil || !strings.Contains(err.Error(), "no-such-tailscale-xyz") {
		t.Fatalf("expected a clear missing-binary error, got %v", err)
	}
}

// REGRESSION: close used to run a blanket `tailscale serve --https=443 off`,
// wiping any serve mapping the operator had configured BEFORE the lease.
// Close now tears 443 down only when no mapping existed beforehand. Ported
// from conductor's pre-contract-cutover internal/handoff/tunnel_more_test.go.
func TestFunnelClosePreservesPreexistingServe(t *testing.T) {
	dir := stubTool(t, "tailscale", `echo "$@" >> "$(dirname "$0")/calls"
case "$1 $2" in
  "serve status") echo "https://box.tailnet.ts.net (tailnet only)"; exit 0 ;;
esac
case "$1" in
  serve) echo "Available at https://box.tailnet.ts.net/" ;;
  status) echo '{"Self":{"DNSName":"box.tailnet.ts.net."}}' ;;
esac`)
	p := newTailscalePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"funnel_mode": "serve"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_close", Options: map[string]any{"lease": res.Outputs["lease"]}}); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(calls), "off") {
		t.Fatalf("close clobbered a pre-existing serve mapping: %s", calls)
	}

	// No pre-existing mapping → close tears our own mapping down.
	dir = stubTool(t, "tailscale", `echo "$@" >> "$(dirname "$0")/calls"
case "$1 $2" in
  "serve status") echo "No serve config"; exit 0 ;;
esac
case "$1" in
  serve) echo "Available at https://box.tailnet.ts.net/" ;;
  status) echo '{"Self":{"DNSName":"box.tailnet.ts.net."}}' ;;
esac`)
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"funnel_mode": "serve"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_close", Options: map[string]any{"lease": res.Outputs["lease"]}}); err != nil {
		t.Fatal(err)
	}
	calls, _ = os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "--https=443 off") {
		t.Fatalf("close did not tear down conductor's own mapping: %s", calls)
	}
}

// TestFunnelOpenRefCountsTheSharedMapping is the regression test for finding
// #5: `tailscale serve`/`funnel --bg` is one global 443 mapping on the box,
// not one per lease. Two concurrent funnel_open calls on the SAME
// (binary, mode, port) share it, and closing one must NOT tear it down while
// the other is still open — regardless of which lease is closed first. (A
// different port while a lease is outstanding is refused outright — see
// TestFunnelOpenRefusesDifferentPortWhileLeaseOutstanding.)
func TestFunnelOpenRefCountsTheSharedMapping(t *testing.T) {
	for _, name := range []string{"close first-opened then second-opened", "close second-opened then first-opened"} {
		t.Run(name, func(t *testing.T) {
			dir := stubTool(t, "tailscale", `echo "$@" >> "$(dirname "$0")/calls"
case "$1 $2" in
  "serve status") echo "No serve config"; exit 0 ;;
esac
case "$1" in
  serve) echo "Available at https://box.tailnet.ts.net/" ;;
  status) echo '{"Self":{"DNSName":"box.tailnet.ts.net."}}' ;;
esac`)
			p := newTailscalePlugin()
			resA, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"funnel_mode": "serve"}})
			if err != nil {
				t.Fatalf("open A: %v", err)
			}
			resB, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"funnel_mode": "serve"}})
			if err != nil {
				t.Fatalf("open B: %v", err)
			}
			leaseA, _ := resA.Outputs["lease"].(string)
			leaseB, _ := resB.Outputs["lease"].(string)

			first, second := leaseA, leaseB
			if strings.HasPrefix(name, "close second") {
				first, second = leaseB, leaseA
			}

			if _, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_close", Options: map[string]any{"lease": first}}); err != nil {
				t.Fatalf("close first: %v", err)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Contains(string(calls), "off") {
				t.Fatalf("closing one of two leases on a shared mapping tore it down early: %s", calls)
			}

			if _, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_close", Options: map[string]any{"lease": second}}); err != nil {
				t.Fatalf("close second: %v", err)
			}
			calls, _ = os.ReadFile(filepath.Join(dir, "calls"))
			if !strings.Contains(string(calls), "--https=443 off") {
				t.Fatalf("closing the LAST lease on the mapping did not tear it down: %s", calls)
			}
			if p.leases.Len() != 0 {
				t.Fatalf("leases after both closes = %d, want 0", p.leases.Len())
			}
		})
	}
}

// TestFunnelOpenRefusesDifferentPortWhileLeaseOutstanding is the regression
// test for round-2 finding #2: two concurrent funnel_open calls on the same
// (binary, mode) but DIFFERENT ports must not silently retarget the one
// shared 443 mapping out from under the lease that already holds it. The
// second open must be refused with a clear error and must not touch the
// CLI or add a lease; once the first lease fully releases the mapping, a
// different port may open fresh.
func TestFunnelOpenRefusesDifferentPortWhileLeaseOutstanding(t *testing.T) {
	dir := stubTool(t, "tailscale", `echo "$@" >> "$(dirname "$0")/calls"
case "$1 $2" in
  "serve status") echo "No serve config"; exit 0 ;;
esac
case "$1" in
  serve) echo "Available at https://box.tailnet.ts.net/" ;;
  status) echo '{"Self":{"DNSName":"box.tailnet.ts.net."}}' ;;
esac`)
	p := newTailscalePlugin()
	resA, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"funnel_mode": "serve"}})
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	leaseA, _ := resA.Outputs["lease"].(string)

	callsBefore, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8100"}, Connection: map[string]any{"funnel_mode": "serve"}}); err == nil {
		t.Fatal("expected an error opening a different port while a lease is outstanding on the shared mapping")
	}
	callsAfter, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(callsAfter) != string(callsBefore) {
		t.Fatalf("a refused open must never touch the tailscale CLI: before=%q after=%q", callsBefore, callsAfter)
	}
	if p.leases.Len() != 1 {
		t.Fatalf("a refused open must not add a lease: leases = %d, want 1", p.leases.Len())
	}

	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_close", Options: map[string]any{"lease": leaseA}}); err != nil {
		t.Fatalf("close A: %v", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "--https=443 off") {
		t.Fatalf("closing the only lease did not tear down the mapping: %s", calls)
	}

	// Once the mapping is fully released, a different port may open fresh.
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8100"}, Connection: map[string]any{"funnel_mode": "serve"}}); err != nil {
		t.Fatalf("open after full release: %v", err)
	}
}

// TestFunnelOpenTearsDownMappingWhenNoURLDeterminable is the regression test
// for round-2 finding #1: when `<mode> --bg <port>` brings the mapping up
// but no URL can be determined — neither printed by the command itself nor
// recoverable from the `status --json` fallback — funnel_open must tear the
// mapping it just brought up back down, not merely release its own refcount
// and leave a lease-less mapping dangling on the box (which the next open
// would then mistake for a pre-existing, operator-owned config and refuse to
// ever clean up).
func TestFunnelOpenTearsDownMappingWhenNoURLDeterminable(t *testing.T) {
	dir := stubTool(t, "tailscale", `echo "$@" >> "$(dirname "$0")/calls"
case "$1 $2" in
  "serve status") echo "No serve config"; exit 0 ;;
esac
case "$1" in
  serve) exit 0 ;;   # "succeeds" but prints no URL
  status) exit 1 ;;  # the status --json fallback also fails
esac`)
	p := newTailscalePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"funnel_mode": "serve"}})
	if err == nil {
		t.Fatal("expected an error when no URL can be determined")
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "--https=443 off") {
		t.Fatalf("a mapping this plugin brought up must be torn down when the open fails with no URL, got calls: %s", calls)
	}
	if p.leases.Len() != 0 {
		t.Fatalf("a failed open must not leave a lease: leases = %d, want 0", p.leases.Len())
	}
}

func TestFunnelStopReleasesOnlyThatInstance(t *testing.T) {
	stubTool(t, "tailscale", `case "$1" in
  serve|funnel) echo "Available at https://box.tailnet.ts.net/" ;;
  status) echo '{"Self":{"DNSName":"box.tailnet.ts.net."}}' ;;
esac`)
	p := newTailscalePlugin()
	for _, inst := range []string{"a", "b"} {
		if _, err := p.Invoke(plugin.InvokeRequest{Instance: inst, Verb: "funnel_open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}}); err != nil {
			t.Fatal(err)
		}
	}
	if p.leases.Len() != 2 {
		t.Fatalf("leases = %d, want 2", p.leases.Len())
	}
	if err := p.Stop(context.Background(), plugin.StopRequest{Instance: "a"}); err != nil {
		t.Fatal(err)
	}
	if p.leases.Len() != 1 {
		t.Fatalf("leases after stop a = %d, want 1", p.leases.Len())
	}
}

func TestDeclarationsAreValid(t *testing.T) {
	d := newTailscalePlugin().Describe()
	d.ProtocolVersion = plugin.ProtocolVersion
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if p := plugin.CheckSemantics(raw); len(p) > 0 {
		t.Fatalf("CheckSemantics: %v", p)
	}
	if p := plugin.ValidateSemantics(d); len(p) > 0 {
		t.Fatalf("ValidateSemantics: %v", p)
	}
}
