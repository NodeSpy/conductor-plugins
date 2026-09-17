package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn wires a connection pointed at srv with fixed api_key/api_secret.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{"base_url": srv.URL, "api_key": "my-key", "api_secret": "my-secret"}
}

func checkBasicAuth(t *testing.T, r *http.Request) {
	t.Helper()
	user, pass, ok := r.BasicAuth()
	if !ok {
		t.Error("expected HTTP Basic auth, got none")
		return
	}
	if user != "my-key" || pass != "my-secret" {
		t.Errorf("basic auth: got user=%q pass=%q want user=%q pass=%q", user, pass, "my-key", "my-secret")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestFirmwareStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/core/firmware/status" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"status": "ok", "os_version": "24.7"})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "firmware_status", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "ok" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestFirmwareUpgrade(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, 200, map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "firmware_upgrade", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/core/firmware/upgrade" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestServices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/core/service/search" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{
			"rows":     []any{map[string]any{"id": "unbound", "running": true}},
			"rowCount": 1,
			"total":    1,
		})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "services", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "unbound" {
		t.Fatalf("items (hoisted from result.rows): %#v", res.Outputs["items"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["total"] != float64(1) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestServiceActions(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, 200, map[string]any{"response": "OK"})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	cases := []struct {
		verb string
		path string
	}{
		{"service_restart", "/api/core/service/restart/unbound"},
		{"service_start", "/api/core/service/start/unbound"},
		{"service_stop", "/api/core/service/stop/unbound"},
	}
	for _, tc := range cases {
		res, err := p.Invoke(plugin.InvokeRequest{
			Verb: tc.verb, Connection: testConn(srv),
			Options: map[string]any{"name": "unbound"},
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		if gotMethod != http.MethodPost || gotPath != tc.path {
			t.Errorf("%s: request: %s %s want POST %s", tc.verb, gotMethod, gotPath, tc.path)
		}
		result, ok := res.Outputs["result"].(map[string]any)
		if !ok || result["response"] != "OK" {
			t.Errorf("%s: result: %#v", tc.verb, res.Outputs["result"])
		}
	}
}

func TestFirewallAliases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/firewall/alias/searchItem" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"rows": []any{map[string]any{"uuid": "abc-123", "name": "lan_hosts"}}})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "firewall_aliases", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["name"] != "lan_hosts" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAliasGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/firewall/alias/getItem/abc-123" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"alias": map[string]any{"name": "lan_hosts"}})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "alias_get", Connection: testConn(srv),
		Options: map[string]any{"uuid": "abc-123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["alias"]; !ok {
		t.Fatalf("result missing alias: %#v", result)
	}
}

func TestAliasAdd(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api/firewall/alias/addItem" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"result": "saved", "uuid": "new-uuid"})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "alias_add", Connection: testConn(srv),
		Options: map[string]any{"alias": map[string]any{"name": "new_hosts", "type": "host", "content": "10.0.0.1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	aliasBody, ok := gotBody["alias"].(map[string]any)
	if !ok || aliasBody["name"] != "new_hosts" {
		t.Fatalf("request body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["uuid"] != "new-uuid" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAliasToggle(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		gotPath = r.URL.Path
		writeJSON(w, 200, map[string]any{"result": "ok", "changed": true})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()

	// Explicit enabled=true -> trailing /1
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "alias_toggle", Connection: testConn(srv),
		Options: map[string]any{"uuid": "abc-123", "enabled": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/firewall/alias/toggleItem/abc-123/1" {
		t.Errorf("enabled=true path: got %q", gotPath)
	}

	// Explicit enabled=false -> trailing /0
	_, err = p.Invoke(plugin.InvokeRequest{
		Verb: "alias_toggle", Connection: testConn(srv),
		Options: map[string]any{"uuid": "abc-123", "enabled": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/firewall/alias/toggleItem/abc-123/0" {
		t.Errorf("enabled=false path: got %q", gotPath)
	}

	// Omitted enabled -> no trailing segment (server-side toggle)
	_, err = p.Invoke(plugin.InvokeRequest{
		Verb: "alias_toggle", Connection: testConn(srv),
		Options: map[string]any{"uuid": "abc-123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/firewall/alias/toggleItem/abc-123" {
		t.Errorf("omitted enabled path: got %q", gotPath)
	}
}

func TestFirewallApply(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, 200, map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "firewall_apply", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/firewall/alias/reconfigure" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
}

func TestInterfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/interfaces/overview/interfacesInfo" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"lan": map[string]any{"descr": "LAN", "enable": true}})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "interfaces", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["lan"]; !ok {
		t.Fatalf("result missing lan: %#v", result)
	}
	if _, ok := res.Outputs["items"]; ok {
		t.Errorf("expected no items for a rows-less object, got %#v", res.Outputs["items"])
	}
}

func TestDHCPLeases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/dhcpv4/leases/searchLease" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"rows": []any{map[string]any{"address": "10.0.0.5", "hostname": "laptop"}}})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "dhcp_leases", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["hostname"] != "laptop" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestGatewayStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/routes/gateway/status" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"status": "ok", "items": []any{map[string]any{"name": "WAN_GW"}}})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "gateway_status", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "ok" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestUnboundSettings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/unbound/settings/get" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"unbound": map[string]any{"general": map[string]any{"enabled": "1"}}})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "unbound_settings", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestSystemReboot(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, 200, map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "system_reboot", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/core/system/reboot" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
}

func TestSystemStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/core/system/status" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"uptime": "3 days"})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "system_status", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["uptime"] != "3 days" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/core/firmware/status":
			writeJSON(w, 200, map[string]any{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/firewall/alias/searchItem" && r.URL.Query().Get("minified") == "1":
			writeJSON(w, 200, []any{map[string]any{"uuid": "abc"}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newOpnsensePlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/core/firmware/status"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "ok" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/firewall/alias/searchItem", "query": map[string]any{"minified": "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBasicAuth(t, r)
		writeJSON(w, 200, map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	p := newOpnsensePlugin()

	// Without insecure_skip_verify, the self-signed test server's cert is
	// rejected and the request fails.
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "firmware_status",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "my-key", "api_secret": "my-secret"},
	})
	if err == nil {
		t.Fatal("expected TLS verification error without insecure_skip_verify")
	}

	// With insecure_skip_verify: true, the same self-signed cert is accepted.
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "firmware_status",
		Connection: map[string]any{
			"base_url": srv.URL, "api_key": "my-key", "api_secret": "my-secret",
			"insecure_skip_verify": true,
		},
	})
	if err != nil {
		t.Fatalf("expected success with insecure_skip_verify=true, got %v", err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid credentials"}`))
	}))
	defer srv.Close()

	p := newOpnsensePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "firmware_status", Connection: testConn(srv)})
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
	if !containsAll(pe.Message, "401", "invalid credentials") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newOpnsensePlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "api_key": "k", "api_secret": "s"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"service_restart", map[string]any{}},
		{"service_start", map[string]any{}},
		{"service_stop", map[string]any{}},
		{"alias_get", map[string]any{}},
		{"alias_add", map[string]any{}},
		{"alias_toggle", map[string]any{}},
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
	p := newOpnsensePlugin()
	cases := []map[string]any{
		{"api_key": "k", "api_secret": "s"},         // missing base_url
		{"base_url": "http://x", "api_secret": "s"}, // missing api_key
		{"base_url": "http://x", "api_key": "k"},    // missing api_secret
	}
	for _, conn := range cases {
		if _, err := p.Invoke(plugin.InvokeRequest{Verb: "firmware_status", Connection: conn}); err == nil {
			t.Errorf("expected error for connection %#v", conn)
		}
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newOpnsensePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{"base_url": "http://x", "api_key": "k", "api_secret": "s"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDescribe(t *testing.T) {
	p := newOpnsensePlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "opnsense" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	for _, key := range []string{"base_url", "api_key", "api_secret"} {
		f := d.Connection[key]
		if f.Type != "string" || !f.Required {
			t.Errorf("connection.%s: %#v", key, f)
		}
	}
	if f := d.Connection["insecure_skip_verify"]; f.Type != "boolean" || f.Required {
		t.Errorf("connection.insecure_skip_verify: %#v", f)
	}
	want := []string{
		"firmware_status", "firmware_upgrade", "services", "service_restart", "service_start", "service_stop",
		"firewall_aliases", "alias_get", "alias_add", "alias_toggle", "firewall_apply",
		"interfaces", "dhcp_leases", "gateway_status", "unbound_settings",
		"system_reboot", "system_status", "api",
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
