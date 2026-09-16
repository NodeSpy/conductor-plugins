package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestBuildRequest pins the exact method/path/query/body each verb builds —
// the whole contract with the Cloudflare API, proven without a network call.
func TestBuildRequest(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		conn       map[string]any
		wantMethod string
		wantPath   string
		wantQuery  string // encoded, "" if none expected
		wantBody   any
	}{
		{
			name: "dns_list with type and name filters", verb: "dns_list",
			opts:       map[string]any{"zone": "z1", "type": "A", "name": "www.example.com"},
			wantMethod: http.MethodGet, wantPath: "/zones/z1/dns_records", wantQuery: "name=www.example.com&type=A",
		},
		{
			name: "dns_list falls back to connection zone_id", verb: "dns_list",
			opts: map[string]any{}, conn: map[string]any{"zone_id": "zconn"},
			wantMethod: http.MethodGet, wantPath: "/zones/zconn/dns_records",
		},
		{
			name: "dns_create full", verb: "dns_create",
			opts:       map[string]any{"zone": "z1", "type": "A", "name": "www", "content": "1.2.3.4", "ttl": 300, "proxied": true, "priority": 10},
			wantMethod: http.MethodPost, wantPath: "/zones/z1/dns_records",
			wantBody: map[string]any{"type": "A", "name": "www", "content": "1.2.3.4", "ttl": int64(300), "proxied": true, "priority": int64(10)},
		},
		{
			name: "dns_update partial", verb: "dns_update",
			opts:       map[string]any{"zone": "z1", "record_id": "rec1", "content": "5.6.7.8"},
			wantMethod: http.MethodPut, wantPath: "/zones/z1/dns_records/rec1",
			wantBody: map[string]any{"content": "5.6.7.8"},
		},
		{
			name: "dns_delete", verb: "dns_delete",
			opts:       map[string]any{"zone": "z1", "record_id": "rec1"},
			wantMethod: http.MethodDelete, wantPath: "/zones/z1/dns_records/rec1",
		},
		{
			name: "cache_purge everything", verb: "cache_purge",
			opts:       map[string]any{"zone": "z1", "everything": true},
			wantMethod: http.MethodPost, wantPath: "/zones/z1/purge_cache",
			wantBody: map[string]any{"purge_everything": true},
		},
		{
			name: "cache_purge files and tags", verb: "cache_purge",
			opts:       map[string]any{"zone": "z1", "files": []any{"https://example.com/a"}, "tags": []any{"tag1"}},
			wantMethod: http.MethodPost, wantPath: "/zones/z1/purge_cache",
			wantBody: map[string]any{"files": []string{"https://example.com/a"}, "tags": []string{"tag1"}},
		},
		{
			name: "zone_list", verb: "zone_list",
			opts:       map[string]any{"name": "example.com", "status": "active"},
			wantMethod: http.MethodGet, wantPath: "/zones", wantQuery: "name=example.com&status=active",
		},
		{
			name: "zone_get", verb: "zone_get",
			opts:       map[string]any{"zone": "z1"},
			wantMethod: http.MethodGet, wantPath: "/zones/z1",
		},
		{
			name: "worker_deploy classic", verb: "worker_deploy",
			opts:       map[string]any{"account": "acct1", "name": "my-worker", "script": "addEventListener('fetch', () => {})"},
			wantMethod: http.MethodPut, wantPath: "/accounts/acct1/workers/scripts/my-worker",
		},
		{
			name: "worker_deploy falls back to connection account_id", verb: "worker_deploy",
			opts: map[string]any{"name": "my-worker", "script": "export default {}"}, conn: map[string]any{"account_id": "acctconn"},
			wantMethod: http.MethodPut, wantPath: "/accounts/acctconn/workers/scripts/my-worker",
		},
		{
			name: "ruleset_list", verb: "ruleset_list",
			opts:       map[string]any{"zone": "z1"},
			wantMethod: http.MethodGet, wantPath: "/zones/z1/rulesets",
		},
		{
			name: "firewall_rules_list", verb: "firewall_rules_list",
			opts:       map[string]any{"zone": "z1"},
			wantMethod: http.MethodGet, wantPath: "/zones/z1/firewall/rules",
		},
		{
			name: "api escape hatch", verb: "api",
			opts:       map[string]any{"method": "post", "path": "/zones/z1/dns_records", "query": map[string]any{"per_page": 5}, "body": map[string]any{"type": "TXT"}},
			wantMethod: http.MethodPost, wantPath: "/zones/z1/dns_records", wantQuery: "per_page=5",
			wantBody: map[string]any{"type": "TXT"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rb, err := buildRequest(tc.verb, tc.opts, tc.conn)
			if err != nil {
				t.Fatalf("buildRequest(%s): unexpected error: %v", tc.verb, err)
			}
			if rb.method != tc.wantMethod {
				t.Errorf("method: got %q want %q", rb.method, tc.wantMethod)
			}
			if rb.path != tc.wantPath {
				t.Errorf("path: got %q want %q", rb.path, tc.wantPath)
			}
			if got := rb.query.Encode(); got != tc.wantQuery {
				t.Errorf("query: got %q want %q", got, tc.wantQuery)
			}
			if tc.wantBody != nil && !reflect.DeepEqual(rb.body, tc.wantBody) {
				t.Errorf("body:\n got: %#v\nwant: %#v", rb.body, tc.wantBody)
			}
		})
	}
}

// TestBuildRequestErrors covers required-field validation, the zone/account
// resolution failure, and unknown verbs.
func TestBuildRequestErrors(t *testing.T) {
	cases := []struct {
		name string
		verb string
		opts map[string]any
		conn map[string]any
	}{
		{"dns_list no zone", "dns_list", map[string]any{}, nil},
		{"dns_create missing content", "dns_create", map[string]any{"zone": "z1", "type": "A", "name": "www"}, nil},
		{"dns_update no record_id", "dns_update", map[string]any{"zone": "z1"}, nil},
		{"dns_delete no record_id", "dns_delete", map[string]any{"zone": "z1"}, nil},
		{"cache_purge nothing set", "cache_purge", map[string]any{"zone": "z1"}, nil},
		{"zone_get no zone", "zone_get", map[string]any{}, nil},
		{"worker_deploy no account", "worker_deploy", map[string]any{"name": "n", "script": "s"}, nil},
		{"worker_deploy no script", "worker_deploy", map[string]any{"account": "a", "name": "n"}, nil},
		{"api no method", "api", map[string]any{"path": "/zones"}, nil},
		{"api no path", "api", map[string]any{"method": "GET"}, nil},
		{"unknown verb", "nope", map[string]any{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildRequest(tc.verb, tc.opts, tc.conn); err == nil {
				t.Errorf("buildRequest(%s, %v): expected error, got nil", tc.verb, tc.opts)
			}
		})
	}
}

// TestAuthHeaders covers the api_token vs legacy api_key+email precedence.
func TestAuthHeaders(t *testing.T) {
	if got := authHeaders(map[string]any{"api_token": "tok123"}); got["Authorization"] != "Bearer tok123" {
		t.Errorf("api_token: got %#v", got)
	}
	got := authHeaders(map[string]any{"api_key": "key123", "email": "me@example.com"})
	if got["X-Auth-Key"] != "key123" || got["X-Auth-Email"] != "me@example.com" {
		t.Errorf("legacy: got %#v", got)
	}
	// api_token wins when both are set.
	both := authHeaders(map[string]any{"api_token": "tok123", "api_key": "key123", "email": "me@example.com"})
	if both["Authorization"] != "Bearer tok123" || both["X-Auth-Key"] != "" {
		t.Errorf("precedence: got %#v", both)
	}
	if got := authHeaders(map[string]any{}); got != nil {
		t.Errorf("no creds: expected nil, got %#v", got)
	}
}

// --- Invoke against an httptest.Server: transport + envelope handling ---

func TestInvokeDNSListBearerAuthAndEnvelopeUnwrap(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":[{"id":"rec1","name":"www.example.com","type":"A","content":"1.2.3.4"}]}`))
	}))
	defer srv.Close()

	p := cloudflarePlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "dns_list",
		Connection: map[string]any{"api_token": "tok123", "api_base": srv.URL},
		Options:    map[string]any{"zone": "z1", "type": "A"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/zones/z1/dns_records" {
		t.Fatalf("method/path: %s %s", gotMethod, gotPath)
	}
	if gotQuery != "type=A" {
		t.Fatalf("query: %s", gotQuery)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("Authorization: %q", gotAuth)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
	list, ok := res.Outputs["result"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	rec := list[0].(map[string]any)
	if rec["id"] != "rec1" || rec["name"] != "www.example.com" {
		t.Fatalf("record: %#v", rec)
	}
}

func TestInvokeLegacyAuthHeaders(t *testing.T) {
	var gotKey, gotEmail, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotEmail, gotAuth = r.Header.Get("X-Auth-Key"), r.Header.Get("X-Auth-Email"), r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":{"id":"z1"}}`))
	}))
	defer srv.Close()

	p := cloudflarePlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "zone_get",
		Connection: map[string]any{"api_key": "key123", "email": "me@example.com", "api_base": srv.URL},
		Options:    map[string]any{"zone": "z1"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotKey != "key123" || gotEmail != "me@example.com" {
		t.Fatalf("legacy headers: key=%q email=%q", gotKey, gotEmail)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization must be unset in legacy mode, got %q", gotAuth)
	}
}

func TestInvokeDNSCreateJSONBody(t *testing.T) {
	var gotBody map[string]any
	var gotContentType, gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotContentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":{"id":"rec1"}}`))
	}))
	defer srv.Close()

	p := cloudflarePlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "dns_create",
		Connection: map[string]any{"api_token": "tok123", "api_base": srv.URL},
		Options:    map[string]any{"zone": "z1", "type": "A", "name": "www", "content": "1.2.3.4"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/zones/z1/dns_records" {
		t.Fatalf("method/path: %s %s", gotMethod, gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type: %q", gotContentType)
	}
	if gotBody["type"] != "A" || gotBody["name"] != "www" || gotBody["content"] != "1.2.3.4" {
		t.Fatalf("body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "rec1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestInvokeWorkerDeployRawBody(t *testing.T) {
	var gotBody, gotContentType, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		gotContentType, gotPath = r.Header.Get("Content-Type"), r.URL.Path
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":{"id":"my-worker"}}`))
	}))
	defer srv.Close()

	p := cloudflarePlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "worker_deploy",
		Connection: map[string]any{"api_token": "tok123", "account_id": "acct1", "api_base": srv.URL},
		Options:    map[string]any{"name": "my-worker", "script": "addEventListener('fetch', () => {})"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotPath != "/accounts/acct1/workers/scripts/my-worker" {
		t.Fatalf("path: %s", gotPath)
	}
	if gotContentType != "application/javascript" {
		t.Fatalf("Content-Type: %q", gotContentType)
	}
	if gotBody != "addEventListener('fetch', () => {})" {
		t.Fatalf("body: %q", gotBody)
	}
}

// TestInvokeSuccessFalseIsAnError proves a 2xx response with success:false in
// the envelope is still surfaced as CodeInternalError, naming the Cloudflare
// error and the raw body.
func TestInvokeSuccessFalseIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81044,"message":"Record already exists."}],"result":null}`))
	}))
	defer srv.Close()

	p := cloudflarePlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "dns_create",
		Connection: map[string]any{"api_token": "tok123", "api_base": srv.URL},
		Options:    map[string]any{"zone": "z1", "type": "A", "name": "www", "content": "1.2.3.4"},
	})
	if err == nil {
		t.Fatal("expected an error for success:false")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !contains(pe.Message, "Record already exists.") {
		t.Fatalf("error message should name the Cloudflare error: %q", pe.Message)
	}
}

// TestInvokeNon2xxIsAnError proves an HTTP-level failure (e.g. a 403 with no
// Cloudflare envelope at all) is also surfaced as CodeInternalError.
func TestInvokeNon2xxIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`))
	}))
	defer srv.Close()

	p := cloudflarePlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "zone_get",
		Connection: map[string]any{"api_token": "bad", "api_base": srv.URL},
		Options:    map[string]any{"zone": "z1"},
	})
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !contains(pe.Message, "Authentication error") {
		t.Fatalf("error message should name the Cloudflare error: %q", pe.Message)
	}
}

// TestInvokeAPIEscapeHatchNonEnvelopeResponse proves the `api` verb passes a
// non-standard (no "success" key) JSON response straight through as result.
func TestInvokeAPIEscapeHatchNonEnvelopeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"value":42}`))
	}))
	defer srv.Close()

	p := cloudflarePlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "api",
		Connection: map[string]any{"api_token": "tok123", "api_base": srv.URL},
		Options:    map[string]any{"method": "GET", "path": "/some/custom/endpoint"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["ok"] != true || result["value"] != float64(42) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

// TestParseCF covers the envelope/non-envelope parsing directly.
func TestParseCF(t *testing.T) {
	result, success, isEnvelope, errMsg := parseCF([]byte(`{"success":true,"errors":[],"result":{"a":1}}`))
	if !success || !isEnvelope || errMsg != "" {
		t.Fatalf("standard success: %v %v %q", success, isEnvelope, errMsg)
	}
	if m, ok := result.(map[string]any); !ok || m["a"] != float64(1) {
		t.Fatalf("result: %#v", result)
	}

	_, success, isEnvelope, errMsg = parseCF([]byte(`{"success":false,"errors":[{"code":1,"message":"bad"}]}`))
	if success || !isEnvelope || errMsg == "" {
		t.Fatalf("standard failure: %v %v %q", success, isEnvelope, errMsg)
	}

	result, success, isEnvelope, _ = parseCF([]byte(`[1,2,3]`))
	if !success || isEnvelope {
		t.Fatalf("non-envelope array: %v %v", success, isEnvelope)
	}
	if arr, ok := result.([]any); !ok || len(arr) != 3 {
		t.Fatalf("result: %#v", result)
	}

	result, success, isEnvelope, _ = parseCF(nil)
	if result != nil || !success || isEnvelope {
		t.Fatalf("empty body: %#v %v %v", result, success, isEnvelope)
	}
}

// --- Describe ---

func TestDescribe(t *testing.T) {
	d := cloudflarePlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "cloudflare" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(joinStrs(d.Capabilities.Egress), "api.cloudflare.com:443") {
		t.Fatalf("Capabilities.Egress: %#v", d.Capabilities.Egress)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("expected no commands/spawns: %#v", d.Capabilities)
	}
	want := []string{
		"dns_list", "dns_create", "dns_update", "dns_delete", "cache_purge",
		"zone_list", "zone_get", "worker_deploy", "ruleset_list", "firewall_rules_list", "api",
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
	if _, ok := d.Connection["api_token"]; !ok {
		t.Errorf("Connection missing api_token")
	}
	if _, ok := d.Connection["zone_id"]; !ok {
		t.Errorf("Connection missing zone_id")
	}
}

// --- test helpers ---

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (substr == "" || indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func joinStrs(ss []string) string {
	out := ""
	for _, s := range ss {
		out += s + ","
	}
	return out
}

func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}
