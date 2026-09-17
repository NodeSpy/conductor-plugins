package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// testConn wires a fresh connection map pointed at srv.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{"base_url": srv.URL, "api_key": "secret-key"}
}

func checkBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
		t.Errorf("Authorization header: got %q want %q", got, "Bearer secret-key")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestSystemInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/v2.0/system/info" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"version": "TrueNAS-SCALE-24.04.0", "hostname": "nas"})
	}))
	defer srv.Close()

	p := newTruenasPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "system_info", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["hostname"] != "nas" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

// TestPoolsBareArray covers the "many endpoints return a bare JSON array"
// case: /pool answers with a top-level array, and it must hoist straight
// into items with no wrapper key.
func TestPoolsBareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/v2.0/pool" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{
			map[string]any{"id": 1, "name": "tank"},
			map[string]any{"id": 2, "name": "backup"},
		})
	}))
	defer srv.Close()

	p := newTruenasPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "pools", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if items[0].(map[string]any)["name"] != "tank" {
		t.Errorf("items[0]: %#v", items[0])
	}
}

func TestPoolGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/v2.0/pool/id/1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": 1, "name": "tank", "status": "ONLINE"})
	}))
	defer srv.Close()

	p := newTruenasPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "pool_get", Connection: testConn(srv),
		Options: map[string]any{"pool_id": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "ONLINE" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

// TestDatasetGetURLEncodesID covers the URL-encoded dataset id: "tank/data"
// must arrive on the wire as "tank%2Fdata" in the path.
func TestDatasetGetURLEncodesID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotPath = r.URL.EscapedPath()
		writeJSON(w, 200, map[string]any{"id": "tank/data", "used": map[string]any{"parsed": 1024}})
	}))
	defer srv.Close()

	p := newTruenasPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "dataset_get", Connection: testConn(srv),
		Options: map[string]any{"dataset_id": "tank/data"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v2.0/pool/dataset/id/tank%2Fdata" {
		t.Errorf("path: got %q", gotPath)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "tank/data" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestDatasetsBareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/v2.0/pool/dataset" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{map[string]any{"id": "tank/data"}})
	}))
	defer srv.Close()

	p := newTruenasPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "datasets", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "tank/data" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestSnapshotCreate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2.0/zfs/snapshot" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"id": "tank/data@snap1"})
	}))
	defer srv.Close()

	p := newTruenasPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "snapshot_create", Connection: testConn(srv),
		Options: map[string]any{"dataset": "tank/data", "name": "snap1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["dataset"] != "tank/data" || gotBody["name"] != "snap1" {
		t.Errorf("request body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "tank/data@snap1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestSnapshotsAndReplicationAndAppsAndServicesBareArrays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch r.URL.Path {
		case "/api/v2.0/zfs/snapshot":
			writeJSON(w, 200, []any{map[string]any{"name": "tank/data@snap1"}})
		case "/api/v2.0/replication":
			writeJSON(w, 200, []any{map[string]any{"id": 1, "name": "offsite"}})
		case "/api/v2.0/app":
			writeJSON(w, 200, []any{map[string]any{"name": "plex"}})
		case "/api/v2.0/service":
			writeJSON(w, 200, []any{map[string]any{"service": "cifs", "state": "RUNNING"}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newTruenasPlugin()
	for verb, wantLen := range map[string]int{
		"snapshots": 1, "replication": 1, "apps": 1, "services": 1,
	} {
		res, err := p.Invoke(plugin.InvokeRequest{Verb: verb, Connection: testConn(srv)})
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		items, ok := res.Outputs["items"].([]any)
		if !ok || len(items) != wantLen {
			t.Errorf("%s items: %#v", verb, res.Outputs["items"])
		}
	}
}

func TestAlerts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/api/v2.0/alert/list" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{
			map[string]any{"uuid": "a1", "level": "CRITICAL", "klass": "PoolStatus", "formatted": "pool tank is degraded", "dismissed": false},
		})
	}))
	defer srv.Close()

	p := newTruenasPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "alerts", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["uuid"] != "a1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAlertDismissSendsRawStringBody(t *testing.T) {
	var gotBody []byte
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2.0/alert/dismiss" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
		writeJSON(w, 200, nil)
	}))
	defer srv.Close()

	p := newTruenasPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "alert_dismiss", Connection: testConn(srv),
		Options: map[string]any{"uuid": "a1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(gotBody)) != `"a1"` {
		t.Errorf("body: got %q want %q", gotBody, `"a1"`)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type: got %q", gotContentType)
	}
}

func TestServiceControl(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, true)
	}))
	defer srv.Close()

	p := newTruenasPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "service_control", Connection: testConn(srv),
		Options: map[string]any{"service": "cifs", "action": "start"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v2.0/service/start" {
		t.Errorf("path: got %q", gotPath)
	}
	if gotBody["service"] != "cifs" {
		t.Errorf("body: %#v", gotBody)
	}
	if res.Outputs["result"] != true {
		t.Errorf("result: %#v", res.Outputs["result"])
	}
}

func TestServiceControlInvalidAction(t *testing.T) {
	p := newTruenasPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "service_control", Connection: map[string]any{"base_url": "http://example.invalid", "api_key": "k"},
		Options: map[string]any{"service": "cifs", "action": "restart"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for a bad action, got %v", err)
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/core/ping":
			writeJSON(w, 200, "pong")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/pool" && r.URL.Query().Get("limit") == "1":
			writeJSON(w, 200, []any{map[string]any{"id": 1}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newTruenasPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/core/ping"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["result"] != "pong" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/pool", "query": map[string]any{"limit": "1"}},
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
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer srv.Close()

	p := newTruenasPlugin()
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
	if !strings.Contains(pe.Message, "401") || !strings.Contains(pe.Message, "invalid api key") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingConnection(t *testing.T) {
	p := newTruenasPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "system_info", Connection: map[string]any{"api_key": "k"}}); err == nil {
		t.Error("expected error for missing base_url")
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "system_info", Connection: map[string]any{"base_url": "http://x"}}); err == nil {
		t.Error("expected error for missing api_key")
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newTruenasPlugin()
	conn := map[string]any{"base_url": "http://example.invalid", "api_key": "k"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"pool_get", map[string]any{}},
		{"dataset_get", map[string]any{}},
		{"snapshot_create", map[string]any{}},
		{"snapshot_create", map[string]any{"dataset": "tank/data"}}, // missing name
		{"alert_dismiss", map[string]any{}},
		{"service_control", map[string]any{}},
		{"service_control", map[string]any{"service": "cifs"}}, // missing action
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
	p := newTruenasPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: map[string]any{"base_url": "http://x", "api_key": "k"}})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestInsecureSkipVerify proves the connection flag actually controls TLS
// verification: the same self-signed httptest.NewTLSServer is unreachable
// with the default (verified) client and reachable once
// insecure_skip_verify is set.
func TestInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		writeJSON(w, 200, map[string]any{"hostname": "nas"})
	}))
	defer srv.Close()

	p := newTruenasPlugin()

	// Default: TLS verification on, self-signed cert is rejected.
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "system_info",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "secret-key"},
	})
	if err == nil {
		t.Fatal("expected a TLS verification error with insecure_skip_verify unset")
	}

	// insecure_skip_verify: true — the same self-signed server now succeeds.
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "system_info",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "secret-key", "insecure_skip_verify": true},
	})
	if err != nil {
		t.Fatalf("insecure_skip_verify=true should succeed against a self-signed server: %v", err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["hostname"] != "nas" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestDescribe(t *testing.T) {
	p := newTruenasPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "truenas" {
		t.Errorf("type: got %q", d.Type)
	}
	if d.Connection["base_url"].Type != "string" || !d.Connection["base_url"].Required {
		t.Errorf("connection.base_url: %#v", d.Connection["base_url"])
	}
	if d.Connection["api_key"].Type != "string" || !d.Connection["api_key"].Required {
		t.Errorf("connection.api_key: %#v", d.Connection["api_key"])
	}
	if d.Connection["insecure_skip_verify"].Type != "boolean" {
		t.Errorf("connection.insecure_skip_verify: %#v", d.Connection["insecure_skip_verify"])
	}
	if d.Connection["poll_interval"].Type != "duration" {
		t.Errorf("connection.poll_interval: %#v", d.Connection["poll_interval"])
	}
	want := []string{
		"system_info", "pools", "pool_get", "datasets", "dataset_get",
		"snapshots", "snapshot_create", "replication", "apps",
		"alerts", "alert_dismiss", "services", "service_control", "api",
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
	if len(d.Events) != 1 || d.Events[0].Name != "alert" {
		t.Fatalf("events: %#v", d.Events)
	}
	for _, f := range []string{"levels", "klasses"} {
		if _, ok := d.Events[0].Filters[f]; !ok {
			t.Errorf("alert event missing filter %q", f)
		}
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Errorf("expected empty egress (self-hosted), got %v", d.Capabilities.Egress)
	}
}

// --- source: pure emit-decision tests ---

func TestAlertEventDismissedDoesNotEmit(t *testing.T) {
	a := map[string]any{"uuid": "a1", "level": "WARNING", "klass": "Foo", "dismissed": true}
	if _, ok := alertEvent(a); ok {
		t.Fatal("dismissed alert must not emit")
	}
}

func TestAlertEventActiveEmits(t *testing.T) {
	a := map[string]any{
		"uuid": "a1", "level": "CRITICAL", "klass": "PoolStatus",
		"formatted": "pool tank is degraded", "dismissed": false, "node": "",
		"datetime": map[string]any{"$date": float64(1700000000000)},
	}
	ev, ok := alertEvent(a)
	if !ok {
		t.Fatal("expected an event")
	}
	if ev["event"] != "alert" || ev["kind"] != "alert" {
		t.Errorf("event/kind: %#v", ev)
	}
	if ev["dedup"] != "a1" {
		t.Errorf("dedup: %#v", ev["dedup"])
	}
	wantTitle := "truenas: CRITICAL alert — pool tank is degraded"
	if ev["title"] != wantTitle {
		t.Errorf("title: got %q want %q", ev["title"], wantTitle)
	}
	ctx, _ := ev["context"].(map[string]any)
	for k, want := range map[string]any{
		"id": "a1", "uuid": "a1", "level": "CRITICAL", "klass": "PoolStatus",
		"formatted": "pool tank is degraded", "dismissed": false,
		"datetime": "1.7e+12", "levels": "CRITICAL", "klasses": "PoolStatus",
	} {
		if ctx[k] != want {
			t.Errorf("context[%q] = %#v, want %#v", k, ctx[k], want)
		}
	}
}

func TestAlertEventNoIdentitySkipped(t *testing.T) {
	a := map[string]any{"level": "INFO", "klass": "Foo", "dismissed": false}
	if _, ok := alertEvent(a); ok {
		t.Fatal("an alert with no id/uuid must not emit")
	}
}

// TestAlertEventDedup mirrors the source's dedup-on-uuid contract: a
// persistently-active alert must be emitted once, not once per poll cycle.
func TestAlertEventDedup(t *testing.T) {
	dedup := sourcekit.NewDedup(16)
	a := map[string]any{"uuid": "a1", "level": "CRITICAL", "klass": "PoolStatus", "dismissed": false}
	emitted := 0
	for i := 0; i < 3; i++ {
		ev, ok := alertEvent(a)
		if ok && dedup.Add(ev["dedup"].(string)) {
			emitted++
		}
	}
	if emitted != 1 {
		t.Fatalf("emitted %d times, want 1", emitted)
	}
}

func TestAlertIDFallsBackToNumericID(t *testing.T) {
	a := map[string]any{"id": float64(42), "level": "INFO", "klass": "Foo", "dismissed": false}
	ev, ok := alertEvent(a)
	if !ok {
		t.Fatal("expected an event")
	}
	if ev["dedup"] != "42" {
		t.Errorf("dedup: %#v", ev["dedup"])
	}
}
