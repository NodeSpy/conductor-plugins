package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// TestVerbArgs pins the exact argv each verb builds — the whole contract with
// smartctl, proven without a disk and without spawning anything.
func TestVerbArgs(t *testing.T) {
	cases := []struct {
		name string
		verb string
		opts map[string]any
		want []string
	}{
		{
			name: "scan",
			verb: "scan",
			opts: map[string]any{},
			want: []string{"--json", "--scan"},
		},
		{
			name: "scan open",
			verb: "scan",
			opts: map[string]any{"open": true},
			want: []string{"--json", "--scan-open"},
		},
		{
			name: "info",
			verb: "info",
			opts: map[string]any{"device": "/dev/sda"},
			want: []string{"--json", "-i", "/dev/sda"},
		},
		{
			name: "health",
			verb: "health",
			opts: map[string]any{"device": "/dev/sda"},
			want: []string{"--json", "-H", "/dev/sda"},
		},
		{
			name: "attributes",
			verb: "attributes",
			opts: map[string]any{"device": "/dev/nvme0"},
			want: []string{"--json", "-A", "/dev/nvme0"},
		},
		{
			name: "all default is -a",
			verb: "all",
			opts: map[string]any{"device": "/dev/sdb"},
			want: []string{"--json", "-a", "/dev/sdb"},
		},
		{
			name: "all extended is -x",
			verb: "all",
			opts: map[string]any{"device": "/dev/sdb", "extended": true},
			want: []string{"--json", "-x", "/dev/sdb"},
		},
		{
			name: "capabilities",
			verb: "capabilities",
			opts: map[string]any{"device": "/dev/sda"},
			want: []string{"--json", "-c", "/dev/sda"},
		},
		{
			name: "test type precedes device",
			verb: "test",
			opts: map[string]any{"device": "/dev/sda", "type": "long"},
			want: []string{"--json", "-t", "long", "/dev/sda"},
		},
		{
			name: "log type",
			verb: "log",
			opts: map[string]any{"device": "/dev/sda", "type": "selftest"},
			want: []string{"--json", "-l", "selftest", "/dev/sda"},
		},
		{
			name: "log defaults to error",
			verb: "log",
			opts: map[string]any{"device": "/dev/sda"},
			want: []string{"--json", "-l", "error", "/dev/sda"},
		},
		{
			// cli is raw by definition: no --json is injected.
			name: "cli escape hatch is raw",
			verb: "cli",
			opts: map[string]any{"args": []any{"-s", "on", "/dev/sda"}},
			want: []string{"-s", "on", "/dev/sda"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verbArgs(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("verbArgs(%s): unexpected error: %v", tc.verb, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("verbArgs(%s)\n got: %#v\nwant: %#v", tc.verb, got, tc.want)
			}
		})
	}

	// sudo is a connection concern, so it wraps the WHOLE command line rather
	// than appearing in verbArgs: the binary becomes sudo's first argument.
	t.Run("sudo prefixes the binary", func(t *testing.T) {
		args, _ := verbArgs("health", map[string]any{"device": "/dev/sda"})
		plain := smartConn{binary: "smartctl"}.argv(args)
		if !reflect.DeepEqual(plain, []string{"smartctl", "--json", "-H", "/dev/sda"}) {
			t.Fatalf("argv without sudo: %#v", plain)
		}
		elevated := smartConn{binary: "smartctl", sudo: true}.argv(args)
		if !reflect.DeepEqual(elevated, []string{"sudo", "smartctl", "--json", "-H", "/dev/sda"}) {
			t.Fatalf("argv with sudo: %#v", elevated)
		}
	})
}

// TestVerbArgsErrors covers required-field validation and unknown verbs.
func TestVerbArgsErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"info", map[string]any{}},                     // no device
		{"health", map[string]any{}},                   // no device
		{"attributes", map[string]any{}},               // no device
		{"all", map[string]any{}},                      // no device
		{"capabilities", map[string]any{}},             // no device
		{"test", map[string]any{"device": "/dev/sda"}}, // no type
		{"test", map[string]any{"type": "short"}},      // no device
		{"log", map[string]any{}},                      // no device
		{"cli", map[string]any{}},                      // no args
		{"nope", map[string]any{}},                     // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbArgs(tc.verb, tc.opts); err == nil {
			t.Errorf("verbArgs(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers the defaults, the duration fields, and the process env.
func TestParseConn(t *testing.T) {
	c, err := parseConn(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "smartctl" || c.sudo || c.timeout != 2*time.Minute || c.pollInterval != 5*time.Minute {
		t.Fatalf("defaults: %+v", c)
	}

	c, err = parseConn(map[string]any{
		"binary": "/usr/sbin/smartctl", "sudo": true,
		"devices": []any{"/dev/sda", "/dev/sdb"},
		"timeout": "30s", "poll_interval": "1m",
		"env": map[string]any{"LC_ALL": "C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "/usr/sbin/smartctl" || !c.sudo {
		t.Errorf("binary/sudo: %+v", c)
	}
	if !reflect.DeepEqual(c.devices, []string{"/dev/sda", "/dev/sdb"}) {
		t.Errorf("devices: %#v", c.devices)
	}
	if c.timeout != 30*time.Second || c.pollInterval != time.Minute {
		t.Errorf("durations: %v %v", c.timeout, c.pollInterval)
	}
	if !contains(c.procEnv(), "LC_ALL=C") {
		t.Error("procEnv missing LC_ALL=C")
	}

	if _, err := parseConn(map[string]any{"timeout": "nope"}); err == nil {
		t.Error("expected error for an unparseable timeout")
	}
	if _, err := parseConn(map[string]any{"poll_interval": "nope"}); err == nil {
		t.Error("expected error for an unparseable poll_interval")
	}
}

// TestDescribe asserts the declared surface: kind, capabilities, every verb,
// and the source event.
func TestDescribe(t *testing.T) {
	d := smartPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "smart" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Commands, "smartctl") || !contains(d.Capabilities.Commands, "sudo") || !d.Capabilities.Spawns {
		t.Fatalf("capabilities: %#v", d.Capabilities)
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Fatalf("smartctl dials nothing, but egress is declared: %v", d.Capabilities.Egress)
	}
	want := []string{"scan", "info", "health", "attributes", "all", "capabilities", "test", "log", "cli"}
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
	if len(d.Events) != 1 || d.Events[0].Name != "health" {
		t.Fatalf("events: %#v", d.Events)
	}
	for _, f := range []string{"devices", "device", "models", "model"} {
		if _, ok := d.Events[0].Filters[f]; !ok {
			t.Errorf("health event missing filter %q", f)
		}
	}
}

// canned `smartctl --json -H -A` documents, trimmed to the fields the source
// reads.
const (
	healthyJSON = `{
	  "model_name": "Samsung SSD 870",
	  "serial_number": "S5SXNF0R",
	  "smart_status": {"passed": true},
	  "temperature": {"current": 31}
	}`
	failingJSON = `{
	  "model_name": "WDC WD40EFRX-68N32N0",
	  "serial_number": "WD-WCC7K1YDZ8XN",
	  "smart_status": {"passed": false},
	  "temperature": {"current": 44},
	  "power_on_time": {"hours": 41231},
	  "ata_smart_attributes": {"table": [
	    {"id": 1, "name": "Raw_Read_Error_Rate", "raw": {"value": 0}},
	    {"id": 5, "name": "Reallocated_Sector_Ct", "raw": {"value": 137}}
	  ]}
	}`
	// SMART is off, or the enclosure cannot be interrogated: no status at all.
	unknownJSON = `{"model_name": "Generic Enclosure", "device": {"name": "/dev/sdz"}}`
)

// TestHealthEvent drives the source's pure decision function over canned
// smartctl JSON: only a device that explicitly FAILED its self-assessment
// emits, and the event it builds carries the documented context and filter
// keys.
func TestHealthEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"passed disks do not emit", healthyJSON},
		{"unknown status is not an alert", unknownJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := healthEvent("/dev/sda", mustDoc(t, tc.doc)); ok {
				t.Fatal("expected no event")
			}
		})
	}

	t.Run("failing disk emits", func(t *testing.T) {
		ev, ok := healthEvent("/dev/sdb", mustDoc(t, failingJSON))
		if !ok {
			t.Fatal("expected an event")
		}
		if ev["event"] != "health" || ev["kind"] != "health" {
			t.Errorf("event/kind: %#v", ev)
		}
		if ev["dedup"] != "/dev/sdb\x00FAILED" {
			t.Errorf("dedup: %#v", ev["dedup"])
		}
		if title, _ := ev["title"].(string); title != "smart: /dev/sdb FAILED its self-assessment (WDC WD40EFRX-68N32N0)" {
			t.Errorf("title: %q", title)
		}
		ctx, _ := ev["context"].(map[string]any)
		for k, want := range map[string]any{
			"device": "/dev/sdb", "model_name": "WDC WD40EFRX-68N32N0",
			"serial_number": "WD-WCC7K1YDZ8XN", "passed": false,
			"temperature_c": int64(44), "power_on_hours": int64(41231),
			"reallocated_sectors": int64(137), "health": "FAILED",
			// plural aliases the documented filters match against
			"devices": "/dev/sdb", "models": "WDC WD40EFRX-68N32N0",
		} {
			if ctx[k] != want {
				t.Errorf("context[%q] = %#v, want %#v", k, ctx[k], want)
			}
		}
	})

	// A persistently-failing disk emits ONCE: the second cycle's identical
	// device+status key is already in the dedup set.
	t.Run("dedup suppresses the repeat cycle", func(t *testing.T) {
		dedup := sourcekit.NewDedup(16)
		emitted := 0
		for i := 0; i < 3; i++ {
			ev, ok := healthEvent("/dev/sdb", mustDoc(t, failingJSON))
			if ok && dedup.Add(str(ev["dedup"])) {
				emitted++
			}
		}
		if emitted != 1 {
			t.Fatalf("emitted %d times, want 1", emitted)
		}
	})
}

// TestScanDevices covers the device discovery the source falls back to when no
// devices are configured.
func TestScanDevices(t *testing.T) {
	doc := mustDoc(t, `{"devices": [
	  {"name": "/dev/sda", "type": "sat"},
	  {"name": "/dev/nvme0", "type": "nvme"},
	  {"type": "sat"}
	]}`)
	if got := scanDevices(doc); !reflect.DeepEqual(got, []string{"/dev/sda", "/dev/nvme0"}) {
		t.Fatalf("scanDevices: %#v", got)
	}
}

// TestInvokeShim drives verbArgs + runSmartctl + enrich end to end against a
// fake smartctl, proving the two behaviours that matter: a non-zero exit is
// passed through as DATA (smartctl's status is a bitmask — 8 means "failing"),
// and the JSON is parsed anyway.
func TestInvokeShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "fakesmartctl")
	script := "#!/bin/sh\ncase \"$2\" in\n" +
		"--scan-open) echo '{\"devices\":[{\"name\":\"/dev/sda\",\"type\":\"sat\"}]}' ;;\n" +
		"-H) echo '{\"model_name\":\"WDC\",\"smart_status\":{\"passed\":false}}'; exit 8 ;;\n" +
		"*) echo \"unhandled $*\" >&2; exit 2 ;;\nesac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := smartPlugin{}
	conn := map[string]any{"binary": shim}

	// health on a failing disk: exit 8 is data, and the document still parses.
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "health", Connection: conn,
		Options: map[string]any{"device": "/dev/sda"}})
	if err != nil {
		t.Fatalf("a failing disk must not be an RPC error: %v", err)
	}
	if res.Outputs["exit_code"] != 8 {
		t.Fatalf("exit_code: %#v, want 8 passed through", res.Outputs["exit_code"])
	}
	if res.Outputs["passed"] != false {
		t.Fatalf("passed: %#v, want false", res.Outputs["passed"])
	}
	doc, ok := res.Outputs["result"].(map[string]any)
	if !ok || doc["model_name"] != "WDC" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	// scan --open: the parsed document plus the flattened device names.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "scan", Connection: conn,
		Options: map[string]any{"open": true}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 {
		t.Fatalf("scan exit_code: %#v", res.Outputs["exit_code"])
	}
	if got, _ := res.Outputs["devices"].([]string); !reflect.DeepEqual(got, []string{"/dev/sda"}) {
		t.Fatalf("scan devices: %#v", res.Outputs["devices"])
	}

	// cli is raw: no --json, and the shim's non-zero exit is still data.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "cli", Connection: conn,
		Options: map[string]any{"args": []any{"-s", "on", "/dev/sda"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 2 {
		t.Fatalf("cli exit_code: %#v, want 2", res.Outputs["exit_code"])
	}
	if _, ok := res.Outputs["result"]; ok {
		t.Fatalf("cli must not parse a result: %#v", res.Outputs["result"])
	}
}

func mustDoc(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("canned JSON: %v", err)
	}
	return m
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
