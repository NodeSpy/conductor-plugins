package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// testConn wires a connection pointed at srv with a fixed token.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{
		"base_url":     srv.URL,
		"token_id":     "root@pam!conductor",
		"token_secret": "11111111-2222-3333-4444-555555555555",
	}
}

const wantAuthHeader = "PVEAPIToken=root@pam!conductor=11111111-2222-3333-4444-555555555555"

func checkAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != wantAuthHeader {
		t.Errorf("Authorization header: got %q want %q", got, wantAuthHeader)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestNodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.URL.Path != "/api2/json/nodes" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"data": []any{
			map[string]any{"node": "pve1", "status": "online"},
		}})
	}))
	defer srv.Close()

	p := proxmoxPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "nodes", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items (hoisted from data): %#v", res.Outputs["items"])
	}
	if items[0].(map[string]any)["node"] != "pve1" {
		t.Errorf("items[0]: %#v", items[0])
	}
}

func TestNodeStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.URL.Path != "/api2/json/nodes/pve1/status" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"uptime": float64(12345)}})
	}))
	defer srv.Close()

	p := proxmoxPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "node_status", Connection: testConn(srv),
		Options: map[string]any{"node": "pve1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["uptime"] != float64(12345) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestClusterResources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.URL.Path != "/api2/json/cluster/resources" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if r.URL.Query().Get("type") != "vm" {
			t.Errorf("expected ?type=vm, got %q", r.URL.RawQuery)
		}
		writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"vmid": float64(100)}}})
	}))
	defer srv.Close()

	p := proxmoxPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "cluster_resources", Connection: testConn(srv),
		Options: map[string]any{"type": "vm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestQemuStatusAndStart(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/qemu/100/status/current":
			writeJSON(w, 200, map[string]any{"data": map[string]any{"status": "running"}})
		case "/api2/json/nodes/pve1/qemu/100/status/start":
			writeJSON(w, 200, map[string]any{"data": "UPID:pve1:00001234:00005678:00000000:qmstart:100:root@pam:"})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := proxmoxPlugin{}

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "qemu_status", Connection: testConn(srv),
		Options: map[string]any{"node": "pve1", "vmid": 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["status"] != "running" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "qemu_start", Connection: testConn(srv),
		Options: map[string]any{"node": "pve1", "vmid": 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api2/json/nodes/pve1/qemu/100/status/start" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["result"] == nil {
		t.Fatalf("expected a result (the task UPID), got %#v", res.Outputs)
	}
}

func TestQemuClone(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api2/json/nodes/pve1/qemu/9000/clone" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"data": "UPID:pve1:...:qmclone:9000:root@pam:"})
	}))
	defer srv.Close()

	p := proxmoxPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "qemu_clone", Connection: testConn(srv),
		Options: map[string]any{"node": "pve1", "vmid": 9000, "newid": 101, "name": "clone1", "full": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["newid"] != "101" || gotBody["name"] != "clone1" || gotBody["full"] != float64(1) {
		t.Fatalf("request body: %#v", gotBody)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestLxcListAndStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/lxc":
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"vmid": float64(200)}}})
		case "/api2/json/nodes/pve1/lxc/200/status/stop":
			if r.Method != http.MethodPost {
				t.Errorf("method: got %s", r.Method)
			}
			writeJSON(w, 200, map[string]any{"data": "UPID:pve1:...:vzstop:200:root@pam:"})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := proxmoxPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "lxc_list", Connection: testConn(srv), Options: map[string]any{"node": "pve1"}})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "lxc_stop", Connection: testConn(srv), Options: map[string]any{"node": "pve1", "vmid": 200}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["result"] == nil {
		t.Fatalf("expected result: %#v", res.Outputs)
	}
}

func TestStorageAndTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/storage":
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"storage": "local"}}})
		case "/api2/json/nodes/pve1/tasks":
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"upid": "UPID:..."}}})
		case "/api2/json/nodes/pve1/tasks/UPID:abc/status":
			writeJSON(w, 200, map[string]any{"data": map[string]any{"status": "stopped", "exitstatus": "OK"}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := proxmoxPlugin{}

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "storage", Connection: testConn(srv), Options: map[string]any{"node": "pve1"}})
	if err != nil {
		t.Fatal(err)
	}
	if items, ok := res.Outputs["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("storage items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{Verb: "tasks", Connection: testConn(srv), Options: map[string]any{"node": "pve1"}})
	if err != nil {
		t.Fatal(err)
	}
	if items, ok := res.Outputs["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("tasks items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "task_status", Connection: testConn(srv),
		Options: map[string]any{"node": "pve1", "upid": "UPID:abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["exitstatus"] != "OK" {
		t.Fatalf("task_status result: %#v", res.Outputs["result"])
	}
}

func TestBackup(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api2/json/nodes/pve1/vzdump" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"data": "UPID:pve1:...:vzdump:100:root@pam:"})
	}))
	defer srv.Close()

	p := proxmoxPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "backup", Connection: testConn(srv),
		Options: map[string]any{"node": "pve1", "vmid": 100, "storage": "backups", "mode": "snapshot", "compress": "zstd"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"vmid": "100", "storage": "backups", "mode": "snapshot", "compress": "zstd"}
	for k, v := range want {
		if gotBody[k] != v {
			t.Errorf("body[%q] = %#v want %#v", k, gotBody[k], v)
		}
	}
	if res.Outputs["result"] == nil {
		t.Fatalf("expected result: %#v", res.Outputs)
	}
}

func TestSnapshots(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu/100/snapshot":
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"name": "pre-upgrade"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/100/snapshot":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			writeJSON(w, 200, map[string]any{"data": "UPID:pve1:...:qmsnapshot:100:root@pam:"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := proxmoxPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "snapshots", Connection: testConn(srv), Options: map[string]any{"node": "pve1", "vmid": 100}})
	if err != nil {
		t.Fatal(err)
	}
	if items, ok := res.Outputs["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("snapshots items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "snapshot_create", Connection: testConn(srv),
		Options: map[string]any{"node": "pve1", "vmid": 100, "snapname": "pre-upgrade"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["snapname"] != "pre-upgrade" {
		t.Fatalf("body: %#v", gotBody)
	}
	if res.Outputs["result"] == nil {
		t.Fatalf("expected result: %#v", res.Outputs)
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/version":
			writeJSON(w, 200, map[string]any{"data": map[string]any{"version": "8.1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes" && r.URL.Query().Get("foo") == "bar":
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"node": "pve1"}}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := proxmoxPlugin{}

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/version"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["version"] != "8.1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/nodes", "query": map[string]any{"foo": "bar"}},
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
	p := proxmoxPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: map[string]any{"base_url": "http://example.invalid", "token_id": "u@r!t", "token_secret": "s"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":{"token":"invalid PVEAPIToken"}}`))
	}))
	defer srv.Close()

	p := proxmoxPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nodes", Connection: testConn(srv)})
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
	if !containsAll(pe.Message, "401", "invalid PVEAPIToken") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingRequiredConnection(t *testing.T) {
	p := proxmoxPlugin{}
	cases := []map[string]any{
		{"token_id": "u@r!t", "token_secret": "s"},    // missing base_url
		{"base_url": "http://x", "token_secret": "s"}, // missing token_id
		{"base_url": "http://x", "token_id": "u@r!t"}, // missing token_secret
	}
	for i, conn := range cases {
		if _, err := p.Invoke(plugin.InvokeRequest{Verb: "nodes", Connection: conn}); err == nil {
			t.Errorf("case %d: expected error for %#v", i, conn)
		} else if pe, ok := err.(*plugin.Error); !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("case %d: expected CodeInvalidParams, got %v", i, err)
		}
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := proxmoxPlugin{}
	conn := map[string]any{"base_url": "http://example.invalid", "token_id": "u@r!t", "token_secret": "s"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"node_status", map[string]any{}},
		{"qemu_list", map[string]any{}},
		{"qemu_status", map[string]any{"node": "pve1"}}, // missing vmid
		{"qemu_status", map[string]any{"vmid": 100}},    // missing node
		{"qemu_start", map[string]any{"node": "pve1"}},
		{"qemu_clone", map[string]any{"node": "pve1", "vmid": 100}}, // missing newid
		{"lxc_list", map[string]any{}},
		{"lxc_status", map[string]any{"node": "pve1"}},
		{"storage", map[string]any{}},
		{"tasks", map[string]any{}},
		{"task_status", map[string]any{"node": "pve1"}}, // missing upid
		{"backup", map[string]any{"node": "pve1"}},      // missing vmid
		{"snapshots", map[string]any{"node": "pve1"}},
		{"snapshot_create", map[string]any{"node": "pve1", "vmid": 100}}, // missing snapname
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
	p := proxmoxPlugin{}
	conn := map[string]any{"base_url": "http://example.invalid", "token_id": "u@r!t", "token_secret": "s"}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: conn})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

// --- source: the pure emit decision ---

func TestTaskEvent(t *testing.T) {
	t.Run("running task does not emit", func(t *testing.T) {
		doc := map[string]any{"upid": "UPID:pve1:1:2:3:qmstart:100:root@pam:", "node": "pve1", "type": "qmstart"}
		if _, ok := taskEvent(doc); ok {
			t.Fatal("expected no event for a running task")
		}
	})

	t.Run("stopped task (status field) emits", func(t *testing.T) {
		doc := map[string]any{
			"upid": "UPID:pve1:1:2:3:vzdump:100:root@pam:", "node": "pve1", "type": "vzdump",
			"id": "100", "user": "root@pam", "status": "OK",
			"starttime": float64(1000), "endtime": float64(1050),
		}
		ev, ok := taskEvent(doc)
		if !ok {
			t.Fatal("expected an event")
		}
		if ev["event"] != "task" || ev["kind"] != "task" {
			t.Errorf("event/kind: %#v", ev)
		}
		if ev["dedup"] != "UPID:pve1:1:2:3:vzdump:100:root@pam:" {
			t.Errorf("dedup: %#v", ev["dedup"])
		}
		ctx, _ := ev["context"].(map[string]any)
		want := map[string]any{
			"node": "pve1", "upid": "UPID:pve1:1:2:3:vzdump:100:root@pam:", "type": "vzdump",
			"id": "100", "user": "root@pam", "status": "OK", "exitstatus": "OK",
			"starttime": int64(1000), "endtime": int64(1050),
			"nodes": "pve1", "types": "vzdump", "statuses": "OK",
		}
		for k, v := range want {
			if ctx[k] != v {
				t.Errorf("context[%q] = %#v want %#v", k, ctx[k], v)
			}
		}
	})

	t.Run("stopped task with only endtime (no status) still emits", func(t *testing.T) {
		doc := map[string]any{
			"upid": "UPID:pve1:1:2:3:qmclone:100:root@pam:", "node": "pve1", "type": "qmclone",
			"endtime": float64(500),
		}
		ev, ok := taskEvent(doc)
		if !ok {
			t.Fatal("expected an event")
		}
		ctx, _ := ev["context"].(map[string]any)
		if ctx["status"] != "" || ctx["exitstatus"] != "" {
			t.Errorf("expected empty status/exitstatus when the API didn't send one, got %#v / %#v", ctx["status"], ctx["exitstatus"])
		}
	})

	t.Run("failed task uses its error string as exitstatus", func(t *testing.T) {
		doc := map[string]any{
			"upid": "UPID:pve1:1:2:3:vzdump:100:root@pam:", "node": "pve1", "type": "vzdump",
			"status": "job errors",
		}
		ev, ok := taskEvent(doc)
		if !ok {
			t.Fatal("expected an event")
		}
		ctx, _ := ev["context"].(map[string]any)
		if ctx["exitstatus"] != "job errors" || ctx["statuses"] != "job errors" {
			t.Errorf("context: %#v", ctx)
		}
	})

	t.Run("no upid drops the task entirely", func(t *testing.T) {
		doc := map[string]any{"node": "pve1", "type": "vzdump", "status": "OK"}
		if _, ok := taskEvent(doc); ok {
			t.Fatal("expected no event without an upid to identify/dedup on")
		}
	})

	t.Run("dedup suppresses a task already reported", func(t *testing.T) {
		dedup := sourcekit.NewDedup(16)
		doc := map[string]any{"upid": "UPID:pve1:1:2:3:vzdump:100:root@pam:", "node": "pve1", "type": "vzdump", "status": "OK"}
		emitted := 0
		for i := 0; i < 3; i++ {
			ev, ok := taskEvent(doc)
			if ok && dedup.Add(str(ev["dedup"])) {
				emitted++
			}
		}
		if emitted != 1 {
			t.Fatalf("emitted %d times, want 1", emitted)
		}
	})
}

func TestParseConnPollInterval(t *testing.T) {
	c, err := parseConn(map[string]any{"base_url": "http://x", "token_id": "u@r!t", "token_secret": "s"})
	if err != nil {
		t.Fatal(err)
	}
	if c.pollInterval.String() != "30s" {
		t.Errorf("default poll_interval: got %s want 30s", c.pollInterval)
	}

	c, err = parseConn(map[string]any{"base_url": "http://x", "token_id": "u@r!t", "token_secret": "s", "poll_interval": "1m"})
	if err != nil {
		t.Fatal(err)
	}
	if c.pollInterval.String() != "1m0s" {
		t.Errorf("poll_interval: got %s want 1m0s", c.pollInterval)
	}

	if _, err := parseConn(map[string]any{"base_url": "http://x", "token_id": "u@r!t", "token_secret": "s", "poll_interval": "not-a-duration"}); err == nil {
		t.Error("expected an error for an invalid poll_interval")
	}
}

func TestApiBaseAndAuthHeader(t *testing.T) {
	c, err := parseConn(map[string]any{"base_url": "https://pve.example.com:8006/", "token_id": "root@pam!conductor", "token_secret": "secret-uuid"})
	if err != nil {
		t.Fatal(err)
	}
	if c.apiBase() != "https://pve.example.com:8006/api2/json" {
		t.Errorf("apiBase: got %q", c.apiBase())
	}
	if c.authHeader() != "PVEAPIToken=root@pam!conductor=secret-uuid" {
		t.Errorf("authHeader: got %q", c.authHeader())
	}
}

func TestDescribe(t *testing.T) {
	p := proxmoxPlugin{}
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "proxmox" {
		t.Errorf("type: got %q", d.Type)
	}
	for _, key := range []string{"base_url", "token_id", "token_secret"} {
		f, ok := d.Connection[key]
		if !ok || f.Type != "string" || !f.Required {
			t.Errorf("connection.%s: %#v", key, f)
		}
	}
	if f := d.Connection["insecure_skip_verify"]; f.Type != "boolean" || f.Required {
		t.Errorf("connection.insecure_skip_verify: %#v", f)
	}
	if f := d.Connection["poll_interval"]; f.Type != "duration" {
		t.Errorf("connection.poll_interval: %#v", f)
	}

	want := []string{
		"nodes", "node_status", "cluster_resources",
		"qemu_list", "qemu_status", "qemu_start", "qemu_stop", "qemu_shutdown", "qemu_reboot", "qemu_clone",
		"lxc_list", "lxc_status", "lxc_start", "lxc_stop",
		"storage", "tasks", "task_status", "backup", "snapshots", "snapshot_create", "api",
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

	if len(d.Events) != 1 || d.Events[0].Name != "task" {
		t.Fatalf("events: %#v", d.Events)
	}
	for _, f := range []string{"nodes", "types", "statuses"} {
		if _, ok := d.Events[0].Filters[f]; !ok {
			t.Errorf("task event missing filter %q", f)
		}
	}
	for _, f := range []string{"node", "upid", "type", "id", "user", "status", "exitstatus", "starttime", "endtime"} {
		if _, ok := d.Events[0].Context[f]; !ok {
			t.Errorf("task event missing context %q", f)
		}
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
