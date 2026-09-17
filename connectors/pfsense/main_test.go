package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn wires a connection pointed at srv with a fixed api_key.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{"base_url": srv.URL, "api_key": "my-key"}
}

func checkAPIKey(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("X-API-Key"); got != "my-key" {
		t.Errorf("X-API-Key: got %q want %q", got, "my-key")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// envelope builds a pfSense REST API v2 response envelope.
func envelope(code int, data any) map[string]any {
	return map[string]any{
		"code":        code,
		"status":      "ok",
		"response_id": "SUCCESS",
		"message":     "",
		"data":        data,
	}
}

func TestFirewallRules(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/firewall/rules" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, envelope(200, []any{map[string]any{"id": "0", "descr": "allow lan"}}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "firewall_rules", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["descr"] != "allow lan" {
		t.Fatalf("items (hoisted from data): %#v", res.Outputs["items"])
	}
	if _, ok := res.Outputs["result"]; ok {
		t.Errorf("expected no result for a list data payload, got %#v", res.Outputs["result"])
	}
}

func TestRuleGet(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("id")
		if r.Method != http.MethodGet {
			t.Errorf("method: %s", r.Method)
		}
		writeJSON(w, 200, envelope(200, map[string]any{"id": "5", "descr": "block wan"}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "rule_get", Connection: testConn(srv),
		Options: map[string]any{"id": "5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v2/firewall/rule" || gotQuery != "5" {
		t.Fatalf("request: path=%q id=%q", gotPath, gotQuery)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["descr"] != "block wan" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestRuleGetNumericID(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotQuery = r.URL.Query().Get("id")
		writeJSON(w, 200, envelope(200, map[string]any{"id": "5"}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	// A JSON number (float64, as arrives over the wire from most callers)
	// should still render as a clean integer query value.
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "rule_get", Connection: testConn(srv),
		Options: map[string]any{"id": float64(5)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "5" {
		t.Fatalf("id query: got %q want %q", gotQuery, "5")
	}
}

func TestRuleCreate(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, envelope(200, map[string]any{"id": "10"}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "rule_create", Connection: testConn(srv),
		Options: map[string]any{"rule": map[string]any{"type": "pass", "interface": "lan", "descr": "new rule"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v2/firewall/rule" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if gotBody["type"] != "pass" || gotBody["descr"] != "new rule" {
		t.Fatalf("request body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "10" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestRuleDelete(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.Query().Get("id")
		writeJSON(w, 200, envelope(200, map[string]any{"id": "5"}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "rule_delete", Connection: testConn(srv),
		Options: map[string]any{"id": "5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v2/firewall/rule" || gotQuery != "5" {
		t.Fatalf("request: %s %s?id=%s", gotMethod, gotPath, gotQuery)
	}
}

func TestFirewallApply(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, 200, envelope(200, nil))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "firewall_apply", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v2/firewall/apply" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
}

func TestAliases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/firewall/aliases" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, envelope(200, []any{map[string]any{"id": "0", "name": "lan_hosts"}}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "aliases", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["name"] != "lan_hosts" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAliasGet(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("id")
		writeJSON(w, 200, envelope(200, map[string]any{"name": "lan_hosts"}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "alias_get", Connection: testConn(srv),
		Options: map[string]any{"id": "lan_hosts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v2/firewall/alias" || gotQuery != "lan_hosts" {
		t.Fatalf("request: path=%q id=%q", gotPath, gotQuery)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["name"] != "lan_hosts" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInterfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/status/interfaces" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, envelope(200, []any{map[string]any{"name": "lan", "status": "up"}}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "interfaces", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["name"] != "lan" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestServices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/status/services" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, envelope(200, []any{map[string]any{"name": "unbound", "status": true}}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "services", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["name"] != "unbound" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestServiceControl(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, envelope(200, map[string]any{"name": "unbound", "status": true}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "service_control", Connection: testConn(srv),
		Options: map[string]any{"name": "unbound", "action": "restart"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v2/status/service" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if gotBody["name"] != "unbound" || gotBody["action"] != "restart" {
		t.Fatalf("request body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["name"] != "unbound" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestDHCPLeases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/status/dhcp_server/leases" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, envelope(200, []any{map[string]any{"ip": "10.0.0.5", "hostname": "laptop"}}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "dhcp_leases", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["hostname"] != "laptop" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestSystemStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/status/system" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, envelope(200, map[string]any{"pfsense_version": "2.7.2"}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "system_status", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["pfsense_version"] != "2.7.2" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestGateways(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/status/gateways" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, envelope(200, []any{map[string]any{"name": "WAN_GW", "status": "online"}}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "gateways", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["name"] != "WAN_GW" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/status/system":
			writeJSON(w, 200, envelope(200, map[string]any{"pfsense_version": "2.7.2"}))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/firewall/rules" && r.URL.Query().Get("limit") == "1":
			writeJSON(w, 200, envelope(200, []any{map[string]any{"id": "0"}}))
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newPfsensePlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/status/system"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["pfsense_version"] != "2.7.2" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/firewall/rules", "query": map[string]any{"limit": "1"}},
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
		checkAPIKey(t, r)
		writeJSON(w, 200, envelope(200, map[string]any{"pfsense_version": "2.7.2"}))
	}))
	defer srv.Close()

	p := newPfsensePlugin()

	// Without insecure_skip_verify, the self-signed test server's cert is
	// rejected and the request fails.
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "system_status",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "my-key"},
	})
	if err == nil {
		t.Fatal("expected TLS verification error without insecure_skip_verify")
	}

	// With insecure_skip_verify: true, the same self-signed cert is accepted.
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "system_status",
		Connection: map[string]any{
			"base_url": srv.URL, "api_key": "my-key",
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
		writeJSON(w, 401, map[string]any{
			"code":        401,
			"status":      "unauthorized",
			"response_id": "AUTH_INVALID_API_KEY",
			"message":     "Invalid API key",
			"data":        []any{},
		})
	}))
	defer srv.Close()

	p := newPfsensePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "system_status", Connection: testConn(srv)})
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
	if !containsAll(pe.Message, "401", "Invalid API key") {
		t.Errorf("message should carry status + pfSense error message, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newPfsensePlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "api_key": "k"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"rule_get", map[string]any{}},
		{"rule_create", map[string]any{}},
		{"rule_delete", map[string]any{}},
		{"alias_get", map[string]any{}},
		{"service_control", map[string]any{}},
		{"service_control", map[string]any{"name": "unbound"}},
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s: expected error for missing required options %#v", tc.verb, tc.opts)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", tc.verb, err)
		}
	}
}

func TestMissingConnection(t *testing.T) {
	p := newPfsensePlugin()
	cases := []map[string]any{
		{"api_key": "k"},         // missing base_url
		{"base_url": "http://x"}, // missing api_key
	}
	for _, conn := range cases {
		if _, err := p.Invoke(plugin.InvokeRequest{Verb: "system_status", Connection: conn}); err == nil {
			t.Errorf("expected error for connection %#v", conn)
		}
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newPfsensePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{"base_url": "http://x", "api_key": "k"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDescribe(t *testing.T) {
	p := newPfsensePlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "pfsense" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	for _, key := range []string{"base_url", "api_key"} {
		f := d.Connection[key]
		if f.Type != "string" || !f.Required {
			t.Errorf("connection.%s: %#v", key, f)
		}
	}
	if f := d.Connection["insecure_skip_verify"]; f.Type != "boolean" || f.Required {
		t.Errorf("connection.insecure_skip_verify: %#v", f)
	}
	want := []string{
		"firewall_rules", "rule_get", "rule_create", "rule_delete", "firewall_apply",
		"aliases", "alias_get",
		"interfaces", "services", "service_control",
		"dhcp_leases", "system_status", "gateways", "api",
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
