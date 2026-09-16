package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestVerbArgs pins the exact argv each verb builds — the whole contract with
// LibationCli, proven without Libation, an Audible account, or a network.
func TestVerbArgs(t *testing.T) {
	cases := []struct {
		name string
		verb string
		opts map[string]any
		want []string
	}{
		{
			name: "scan every account",
			verb: "scan",
			opts: map[string]any{},
			want: []string{"scan"},
		},
		{
			name: "scan named accounts",
			verb: "scan",
			opts: map[string]any{"accounts": []any{"me@example.com"}},
			want: []string{"scan", "me@example.com"},
		},
		{
			name: "export defaults to json",
			verb: "export",
			opts: map[string]any{"path": "/data/library.json"},
			want: []string{"export", "-p", "/data/library.json", "-j"},
		},
		{
			name: "export csv with asins",
			verb: "export",
			opts: map[string]any{"path": "/data/lib.csv", "format": "csv", "asins": []any{"B00FJLGQO2"}},
			want: []string{"export", "-p", "/data/lib.csv", "-c", "B00FJLGQO2"},
		},
		{
			name: "liberate everything pending",
			verb: "liberate",
			opts: map[string]any{},
			want: []string{"liberate"},
		},
		{
			name: "liberate named asins",
			verb: "liberate",
			opts: map[string]any{"asins": []any{"B00FJLGQO2", "B017V4IM1G"}},
			want: []string{"liberate", "-i", "B00FJLGQO2", "-i", "B017V4IM1G"},
		},
		{
			name: "liberate pdf force limited",
			verb: "liberate",
			opts: map[string]any{"pdf": true, "force": true, "limit_books": 5},
			want: []string{"liberate", "--pdf", "--force", "--limit-books", "5"},
		},
		{
			name: "liberate limit gb",
			verb: "liberate",
			opts: map[string]any{"limit_gb": 20},
			want: []string{"liberate", "--limit-gb", "20"},
		},
		{
			name: "set_status downloaded over the whole library",
			verb: "set_status",
			opts: map[string]any{"status": "downloaded"},
			want: []string{"set-status", "--downloaded"},
		},
		{
			name: "set_status pending forced on named asins",
			verb: "set_status",
			opts: map[string]any{"status": "pending", "force": true, "asins": []any{"B00FJLGQO2"}},
			want: []string{"set-status", "--download-pending", "--force", "B00FJLGQO2"},
		},
		{
			name: "search query is positional and last",
			verb: "search",
			opts: map[string]any{"query": "Sanderson", "count": 25, "bare": true},
			want: []string{"search", "-n", "25", "--bare", "Sanderson"},
		},
		{
			name: "list_accounts bare",
			verb: "list_accounts",
			opts: map[string]any{"bare": true},
			want: []string{"list-accounts", "--bare"},
		},
		{
			name: "cli escape hatch",
			verb: "cli",
			opts: map[string]any{"args": []any{"get-setting", "-b"}},
			want: []string{"get-setting", "-b"},
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
}

// TestVerbArgsErrors covers required-field validation, the enums, the
// mutually-exclusive run limits, and unknown verbs.
func TestVerbArgsErrors(t *testing.T) {
	cases := []struct {
		name string
		verb string
		opts map[string]any
	}{
		{"export without a path", "export", map[string]any{}},
		{"export with an unknown format", "export", map[string]any{"path": "/x", "format": "yaml"}},
		{"liberate with two run limits", "liberate", map[string]any{"limit_books": 5, "limit_gb": 20}},
		{"set_status without a status", "set_status", map[string]any{}},
		{"set_status with an unknown status", "set_status", map[string]any{"status": "liberated"}},
		{"search without a query", "search", map[string]any{}},
		{"cli without args", "cli", map[string]any{}},
		{"unknown verb", "nope", map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verbArgs(tc.verb, tc.opts); err == nil {
				t.Errorf("verbArgs(%s, %v): expected error, got nil", tc.verb, tc.opts)
			}
		})
	}
}

// TestParseConn covers the defaults — notably the 60m timeout a download needs —
// and the process env Libation reads its config directory from.
func TestParseConn(t *testing.T) {
	c, err := parseConn(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "LibationCli" || c.dir != "" || c.timeout != 60*time.Minute {
		t.Fatalf("defaults: %+v", c)
	}

	c, err = parseConn(map[string]any{
		"binary": "/libation/LibationCli", "dir": "/data", "timeout": "4h",
		"env": map[string]any{"LIBATION_FILES_DIR": "/config"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "/libation/LibationCli" || c.dir != "/data" || c.timeout != 4*time.Hour {
		t.Fatalf("connection: %+v", c)
	}
	if !contains(c.procEnv(), "LIBATION_FILES_DIR=/config") {
		t.Error("procEnv missing LIBATION_FILES_DIR=/config")
	}

	if _, err := parseConn(map[string]any{"timeout": "nope"}); err == nil {
		t.Error("expected error for an unparseable timeout")
	}
}

// TestDescribe asserts the declared surface: kind, capabilities, and every verb.
func TestDescribe(t *testing.T) {
	d := libationPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "libation" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 1 || d.Capabilities.Commands[0] != "LibationCli" || !d.Capabilities.Spawns {
		t.Fatalf("capabilities: %#v", d.Capabilities)
	}
	// Libation reaches Audible; this connector does not dial anything itself,
	// so declaring egress would widen what the operator is asked to accept.
	if len(d.Capabilities.Egress) != 0 {
		t.Fatalf("declared egress: %v", d.Capabilities.Egress)
	}
	want := []string{"scan", "export", "liberate", "set_status", "search", "list_accounts", "cli"}
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
	if len(d.Events) != 0 {
		t.Errorf("libation is verb-only, but declares events: %#v", d.Events)
	}
}

// TestInvokeShim drives verbArgs + runLibation + enrich end to end against a
// fake LibationCli, proving the two behaviours that matter: a non-zero exit is
// passed through as DATA (liberate exits non-zero when Audible refuses one
// title of many, and the pass still made progress), and `export` reads the
// manifest it just wrote back into the library output.
func TestInvokeShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "fakelibationcli")
	manifest := filepath.Join(dir, "library.json")
	// argv is `export -p <path> -j …`, so the manifest path is $3.
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"export) printf '[{\"Asin\":\"B00FJLGQO2\",\"BookStatus\":\"Liberated\"},{\"Asin\":\"B017V4IM1G\",\"BookStatus\":\"NotLiberated\"}]' > \"$3\" ;;\n" +
		"liberate) echo 'Content License denied'; exit 1 ;;\n" +
		"list-accounts) echo 'me@example.com' ;;\n" +
		"*) echo \"unhandled $*\" >&2; exit 2 ;;\nesac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := libationPlugin{}
	conn := map[string]any{"binary": shim}

	// export with parse: the manifest is read back and counted.
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "export", Connection: conn,
		Options: map[string]any{"path": manifest, "parse": true}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 {
		t.Fatalf("export exit_code: %#v", res.Outputs["exit_code"])
	}
	if res.Outputs["count"] != 2 {
		t.Fatalf("count: %#v, want 2", res.Outputs["count"])
	}
	lib, ok := res.Outputs["library"].([]any)
	if !ok || len(lib) != 2 {
		t.Fatalf("library: %#v", res.Outputs["library"])
	}
	first, _ := lib[0].(map[string]any)
	if first["BookStatus"] != "Liberated" {
		t.Fatalf("library[0]: %#v", lib[0])
	}

	// Without parse, the manifest stays a file — no library output.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "export", Connection: conn,
		Options: map[string]any{"path": manifest}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["library"]; ok {
		t.Fatalf("library parsed without parse: true: %#v", res.Outputs["library"])
	}

	// A partial liberate is DATA, not an RPC error.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "liberate", Connection: conn,
		Options: map[string]any{"asins": []any{"B017V4IM1G"}}})
	if err != nil {
		t.Fatalf("a refused title must not be an RPC error: %v", err)
	}
	if res.Outputs["exit_code"] != 1 {
		t.Fatalf("liberate exit_code: %#v, want 1 passed through", res.Outputs["exit_code"])
	}
	if out, _ := res.Outputs["stdout"].(string); out != "Content License denied\n" {
		t.Fatalf("liberate stdout: %q", out)
	}

	// list_accounts: plain stdout, exit 0.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "list_accounts", Connection: conn,
		Options: map[string]any{"bare": true}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 {
		t.Fatalf("list_accounts exit_code: %#v", res.Outputs["exit_code"])
	}
	if out, _ := res.Outputs["stdout"].(string); out != "me@example.com\n" {
		t.Fatalf("list_accounts stdout: %q", out)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
