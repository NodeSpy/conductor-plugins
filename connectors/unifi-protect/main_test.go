package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn builds a connection map pointed at srv.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{"base_url": srv.URL, "api_key": "secret-key"}
}

func checkAPIKey(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("X-API-KEY"); got != "secret-key" {
		t.Errorf("X-API-KEY header: got %q want %q", got, "secret-key")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- meta ---

func TestMeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/proxy/protect/integration/v1/meta/info" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"applicationVersion": "5.1.0"})
	}))
	defer srv.Close()

	p := newProtectPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "meta", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["applicationVersion"] != "5.1.0" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

// --- cameras: bare array -> items ---

func TestCamerasListIsBareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.Path != "/proxy/protect/integration/v1/cameras" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{
			map[string]any{"id": "cam1", "name": "Front Door"},
			map[string]any{"id": "cam2", "name": "Backyard"},
		})
	}))
	defer srv.Close()

	p := newProtectPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "cameras", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if items[0].(map[string]any)["id"] != "cam1" {
		t.Errorf("items[0]: %#v", items[0])
	}
	if _, has := res.Outputs["result"]; has {
		t.Errorf("did not expect result for a list endpoint: %#v", res.Outputs["result"])
	}
}

func TestCameraGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.Path != "/proxy/protect/integration/v1/cameras/cam1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "cam1", "name": "Front Door"})
	}))
	defer srv.Close()

	p := newProtectPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "camera_get", Connection: testConn(srv),
		Options: map[string]any{"camera_id": "cam1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["name"] != "Front Door" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

// --- camera_snapshot: JPEG bytes -> base64 ---

func TestCameraSnapshotReturnsBase64Image(t *testing.T) {
	imageBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.Path != "/proxy/protect/integration/v1/cameras/cam1/snapshot" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if r.URL.Query().Get("highQuality") != "true" {
			t.Errorf("expected ?highQuality=true, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(200)
		_, _ = w.Write(imageBytes)
	}))
	defer srv.Close()

	p := newProtectPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "camera_snapshot", Connection: testConn(srv),
		Options: map[string]any{"camera_id": "cam1", "high_quality": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["content_type"] != "image/jpeg" {
		t.Errorf("content_type: %#v", res.Outputs["content_type"])
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	gotB64, ok := res.Outputs["image_base64"].(string)
	if !ok {
		t.Fatalf("image_base64: %#v", res.Outputs["image_base64"])
	}
	decoded, err := base64.StdEncoding.DecodeString(gotB64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if string(decoded) != string(imageBytes) {
		t.Errorf("decoded image bytes: got %v want %v", decoded, imageBytes)
	}
}

func TestCameraSnapshotWithoutHighQuality(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.RawQuery != "" {
			t.Errorf("expected no query string, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("jpeg-bytes"))
	}))
	defer srv.Close()

	p := newProtectPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "camera_snapshot", Connection: testConn(srv),
		Options: map[string]any{"camera_id": "cam1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	gotB64, _ := res.Outputs["image_base64"].(string)
	decoded, _ := base64.StdEncoding.DecodeString(gotB64)
	if string(decoded) != "jpeg-bytes" {
		t.Errorf("decoded image: got %q", decoded)
	}
}

// --- camera_ptz ---

func TestCameraPTZGoto(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/proxy/protect/integration/v1/cameras/cam1/ptz/goto/2" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	defer srv.Close()

	p := newProtectPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "camera_ptz", Connection: testConn(srv),
		Options: map[string]any{"camera_id": "cam1", "action": "goto", "slot": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestCameraPTZPatrolStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/proxy/protect/integration/v1/cameras/cam1/ptz/patrol/start/3" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newProtectPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "camera_ptz", Connection: testConn(srv),
		Options: map[string]any{"camera_id": "cam1", "action": "patrol_start", "slot": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 204 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestCameraPTZPatrolStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/proxy/protect/integration/v1/cameras/cam1/ptz/patrol/stop" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newProtectPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "camera_ptz", Connection: testConn(srv),
		Options: map[string]any{"camera_id": "cam1", "action": "patrol_stop"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCameraPTZMissingSlot(t *testing.T) {
	p := newProtectPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "api_key": "k"}

	for _, action := range []string{"goto", "patrol_start"} {
		_, err := p.Invoke(plugin.InvokeRequest{
			Verb: "camera_ptz", Connection: conn,
			Options: map[string]any{"camera_id": "cam1", "action": action},
		})
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("action=%s: expected CodeInvalidParams for missing slot, got %v", action, err)
		}
	}
}

func TestCameraPTZUnknownAction(t *testing.T) {
	p := newProtectPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "api_key": "k"}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "camera_ptz", Connection: conn,
		Options: map[string]any{"camera_id": "cam1", "action": "nope"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

// --- nvrs: singleton object -> result ---

func TestNVRs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		if r.URL.Path != "/proxy/protect/integration/v1/nvrs" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "nvr1", "name": "Dream Machine"})
	}))
	defer srv.Close()

	p := newProtectPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "nvrs", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["name"] != "Dream Machine" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

// --- viewers/lights/sensors/chimes: bare arrays -> items ---

func TestListEndpoints(t *testing.T) {
	cases := []struct {
		verb string
		path string
	}{
		{"viewers", "/proxy/protect/integration/v1/viewers"},
		{"lights", "/proxy/protect/integration/v1/lights"},
		{"sensors", "/proxy/protect/integration/v1/sensors"},
		{"chimes", "/proxy/protect/integration/v1/chimes"},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkAPIKey(t, r)
				if r.URL.Path != tc.path {
					t.Errorf("path: got %q want %q", r.URL.Path, tc.path)
				}
				writeJSON(w, 200, []any{map[string]any{"id": "x1"}})
			}))
			defer srv.Close()

			p := newProtectPlugin()
			res, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: testConn(srv)})
			if err != nil {
				t.Fatal(err)
			}
			items, ok := res.Outputs["items"].([]any)
			if !ok || len(items) != 1 {
				t.Fatalf("items: %#v", res.Outputs["items"])
			}
		})
	}
}

func TestListEndpointEmptyArrayIsNotNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []any{})
	}))
	defer srv.Close()

	p := newProtectPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "lights", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok {
		t.Fatalf("items should be an empty slice, not nil/missing: %#v", res.Outputs["items"])
	}
	if len(items) != 0 {
		t.Errorf("items: got %d want 0", len(items))
	}
}

// --- insecure_skip_verify (self-signed TLS server) ---

func TestInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		writeJSON(w, 200, []any{map[string]any{"id": "cam1"}})
	}))
	defer srv.Close()

	p := newProtectPlugin()

	// Without insecure_skip_verify, the self-signed cert must be rejected.
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "cameras",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "secret-key"},
	})
	if err == nil {
		t.Fatal("expected a TLS verification error without insecure_skip_verify")
	}

	// With it set, the same self-signed server must succeed.
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "cameras",
		Connection: map[string]any{
			"base_url": srv.URL, "api_key": "secret-key", "insecure_skip_verify": true,
		},
	})
	if err != nil {
		t.Fatalf("expected success with insecure_skip_verify: %v", err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

// --- api escape hatch ---

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAPIKey(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/protect/integration/v1/viewers/v1":
			writeJSON(w, 200, map[string]any{"id": "v1"})
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/protect/integration/v1/cameras" && r.URL.Query().Get("q") == "1":
			writeJSON(w, 200, []any{map[string]any{"id": "cam1"}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newProtectPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/viewers/v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "v1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/cameras", "query": map[string]any{"q": "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIEscapeHatchMissingPath(t *testing.T) {
	p := newProtectPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: map[string]any{"base_url": "http://example.invalid", "api_key": "k"},
		Options: map[string]any{},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

// --- non-2xx -> CodeInternalError ---

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	p := newProtectPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "cameras", Connection: testConn(srv)})
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
	if !strings.Contains(pe.Message, "401") || !strings.Contains(pe.Message, "invalid api key") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestNon2xxOnSnapshotIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"camera not found"}`))
	}))
	defer srv.Close()

	p := newProtectPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "camera_snapshot", Connection: testConn(srv),
		Options: map[string]any{"camera_id": "nope"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
}

// --- missing connection fields ---

func TestMissingConnection(t *testing.T) {
	p := newProtectPlugin()

	cases := []struct {
		name string
		conn map[string]any
	}{
		{"missing base_url", map[string]any{"api_key": "k"}},
		{"missing api_key", map[string]any{"base_url": "http://x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Invoke(plugin.InvokeRequest{Verb: "cameras", Connection: tc.conn})
			if err == nil {
				t.Fatal("expected error")
			}
			pe, ok := err.(*plugin.Error)
			if !ok || pe.Code != plugin.CodeInvalidParams {
				t.Fatalf("expected CodeInvalidParams, got %v", err)
			}
		})
	}

	if _, err := parseConn(map[string]any{"base_url": "http://x/", "api_key": "k"}); err != nil {
		t.Errorf("parseConn: unexpected error: %v", err)
	}
}

// --- missing required verb options ---

func TestMissingRequiredOptions(t *testing.T) {
	p := newProtectPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "api_key": "k"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"camera_get", map[string]any{}},
		{"camera_snapshot", map[string]any{}},
		{"camera_ptz", map[string]any{}},
		{"camera_ptz", map[string]any{"camera_id": "cam1"}},
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s %v: expected error for missing required options", tc.verb, tc.opts)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", tc.verb, err)
		}
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newProtectPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "api_key": "k"}
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
	p := newProtectPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "unifi-protect" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	for _, key := range []string{"base_url", "api_key"} {
		f, ok := d.Connection[key]
		if !ok || f.Type != "string" || !f.Required {
			t.Errorf("connection.%s: %#v", key, f)
		}
	}
	if f := d.Connection["insecure_skip_verify"]; f.Type != "boolean" || f.Required {
		t.Errorf("connection.insecure_skip_verify: %#v", f)
	}

	want := []string{
		"meta", "cameras", "camera_get", "camera_snapshot", "camera_ptz",
		"nvrs", "viewers", "lights", "sensors", "chimes", "api",
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

func TestParseConnDefaults(t *testing.T) {
	conn, err := parseConn(map[string]any{"base_url": "https://192.168.1.1/", "api_key": "k"})
	if err != nil {
		t.Fatal(err)
	}
	if conn.baseURL != "https://192.168.1.1" {
		t.Errorf("baseURL should have trailing slash trimmed: %q", conn.baseURL)
	}
	if conn.insecureSkipVerify {
		t.Errorf("insecure_skip_verify default: got true want false")
	}
	if conn.apiBase() != "https://192.168.1.1/proxy/protect/integration/v1" {
		t.Errorf("apiBase: got %q", conn.apiBase())
	}
}
