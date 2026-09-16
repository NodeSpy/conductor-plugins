package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestVerbArgs pins the exact argv each verb builds — the whole contract with
// the terraspace CLI, proven without spawning anything.
func TestVerbArgs(t *testing.T) {
	cases := []struct {
		name string
		verb string
		opts map[string]any
		want []string
	}{
		{
			name: "up default yes",
			verb: "up",
			opts: map[string]any{"stack": "vpc"},
			want: []string{"up", "vpc", "-y"},
		},
		{
			name: "up explicit no",
			verb: "up",
			opts: map[string]any{"stack": "vpc", "yes": false},
			want: []string{"up", "vpc"},
		},
		{
			name: "up with extra_args",
			verb: "up",
			opts: map[string]any{"stack": "vpc", "extra_args": []any{"--force"}},
			want: []string{"up", "vpc", "-y", "--force"},
		},
		{
			name: "down default yes",
			verb: "down",
			opts: map[string]any{"stack": "vpc"},
			want: []string{"down", "vpc", "-y"},
		},
		{
			name: "down explicit no with extra_args",
			verb: "down",
			opts: map[string]any{"stack": "vpc", "yes": false, "extra_args": []any{"--verbose"}},
			want: []string{"down", "vpc", "--verbose"},
		},
		{
			name: "plan with out",
			verb: "plan",
			opts: map[string]any{"stack": "vpc", "out": "vpc.tfplan"},
			want: []string{"plan", "vpc", "--out", "vpc.tfplan"},
		},
		{
			name: "plan minimal",
			verb: "plan",
			opts: map[string]any{"stack": "vpc"},
			want: []string{"plan", "vpc"},
		},
		{
			name: "plan with extra_args",
			verb: "plan",
			opts: map[string]any{"stack": "vpc", "out": "vpc.tfplan", "extra_args": []any{"-var", "region=us-east-1"}},
			want: []string{"plan", "vpc", "--out", "vpc.tfplan", "-var", "region=us-east-1"},
		},
		{
			name: "all_up default yes",
			verb: "all_up",
			opts: map[string]any{},
			want: []string{"all", "up", "-y"},
		},
		{
			name: "all_up explicit no",
			verb: "all_up",
			opts: map[string]any{"yes": false},
			want: []string{"all", "up"},
		},
		{
			name: "all_down default yes",
			verb: "all_down",
			opts: map[string]any{},
			want: []string{"all", "down", "-y"},
		},
		{
			name: "all_plan",
			verb: "all_plan",
			opts: map[string]any{},
			want: []string{"all", "plan"},
		},
		{
			name: "all_plan with extra_args",
			verb: "all_plan",
			opts: map[string]any{"extra_args": []any{"-var-file", "prod.tfvars"}},
			want: []string{"all", "plan", "-var-file", "prod.tfvars"},
		},
		{
			name: "output no name",
			verb: "output",
			opts: map[string]any{"stack": "vpc"},
			want: []string{"output", "vpc"},
		},
		{
			name: "output with name",
			verb: "output",
			opts: map[string]any{"stack": "vpc", "name": "vpc_id"},
			want: []string{"output", "vpc", "vpc_id"},
		},
		{
			name: "logs no stack",
			verb: "logs",
			opts: map[string]any{},
			want: []string{"logs"},
		},
		{
			name: "logs with stack and follow",
			verb: "logs",
			opts: map[string]any{"stack": "vpc", "follow": true},
			want: []string{"logs", "vpc", "-f"},
		},
		{
			name: "list",
			verb: "list",
			opts: map[string]any{},
			want: []string{"list"},
		},
		{
			name: "new",
			verb: "new",
			opts: map[string]any{"subcommand": "stack", "name": "vpc"},
			want: []string{"new", "stack", "vpc"},
		},
		{
			name: "new with extra_args",
			verb: "new",
			opts: map[string]any{"subcommand": "module", "name": "vpc", "extra_args": []any{"--dry-run"}},
			want: []string{"new", "module", "vpc", "--dry-run"},
		},
		{
			name: "import",
			verb: "import",
			opts: map[string]any{"stack": "vpc", "address": "aws_vpc.this", "id": "vpc-123"},
			want: []string{"import", "vpc", "aws_vpc.this", "vpc-123"},
		},
		{
			name: "console",
			verb: "console",
			opts: map[string]any{"stack": "vpc"},
			want: []string{"console", "vpc"},
		},
		{
			name: "state",
			verb: "state",
			opts: map[string]any{"stack": "vpc", "args": []any{"show", "aws_vpc.this"}},
			want: []string{"state", "vpc", "show", "aws_vpc.this"},
		},
		{
			name: "state no args",
			verb: "state",
			opts: map[string]any{"stack": "vpc"},
			want: []string{"state", "vpc"},
		},
		{
			name: "build no stack",
			verb: "build",
			opts: map[string]any{},
			want: []string{"build"},
		},
		{
			name: "build with stack",
			verb: "build",
			opts: map[string]any{"stack": "vpc"},
			want: []string{"build", "vpc"},
		},
		{
			name: "clean no target",
			verb: "clean",
			opts: map[string]any{},
			want: []string{"clean"},
		},
		{
			name: "clean with target",
			verb: "clean",
			opts: map[string]any{"target": "cache"},
			want: []string{"clean", "cache"},
		},
		{
			name: "fmt",
			verb: "fmt",
			opts: map[string]any{},
			want: []string{"fmt"},
		},
		{
			name: "validate",
			verb: "validate",
			opts: map[string]any{"stack": "vpc"},
			want: []string{"validate", "vpc"},
		},
		{
			name: "test",
			verb: "test",
			opts: map[string]any{},
			want: []string{"test"},
		},
		{
			name: "info",
			verb: "info",
			opts: map[string]any{},
			want: []string{"info"},
		},
		{
			name: "cli escape hatch",
			verb: "cli",
			opts: map[string]any{"args": []any{"doctor"}},
			want: []string{"doctor"},
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

// TestVerbArgsErrors covers required-field validation and unknown verbs.
func TestVerbArgsErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"up", map[string]any{}},                                              // no stack
		{"down", map[string]any{}},                                            // no stack
		{"plan", map[string]any{}},                                            // no stack
		{"output", map[string]any{}},                                          // no stack
		{"new", map[string]any{}},                                             // no subcommand/name
		{"new", map[string]any{"subcommand": "stack"}},                        // no name
		{"import", map[string]any{}},                                          // no stack/address/id
		{"import", map[string]any{"stack": "vpc"}},                            // no address/id
		{"import", map[string]any{"stack": "vpc", "address": "aws_vpc.this"}}, // no id
		{"console", map[string]any{}},                                         // no stack
		{"state", map[string]any{}},                                           // no stack
		{"validate", map[string]any{}},                                        // no stack
		{"cli", map[string]any{}},                                             // no args
		{"nope", map[string]any{}},                                            // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbArgs(tc.verb, tc.opts); err == nil {
			t.Errorf("verbArgs(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers binary/dir defaults and the TS_ENV env derivation.
func TestParseConn(t *testing.T) {
	c, err := parseConn(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "terraspace" {
		t.Errorf("binary default: got %q want terraspace", c.binary)
	}
	if c.dir != "" {
		t.Errorf("dir default: got %q want empty", c.dir)
	}

	c2, err := parseConn(map[string]any{
		"binary": "/usr/local/bin/terraspace", "dir": "/srv/infra", "ts_env": "prod",
		"env": map[string]any{"AWS_PROFILE": "prod-ci"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c2.binary != "/usr/local/bin/terraspace" {
		t.Errorf("binary override: got %q", c2.binary)
	}
	if c2.dir != "/srv/infra" {
		t.Errorf("dir: got %q", c2.dir)
	}
	env := c2.procEnv()
	if !contains(env, "TS_ENV=prod") {
		t.Errorf("procEnv missing TS_ENV=prod, got %v", env)
	}
	if !contains(env, "AWS_PROFILE=prod-ci") {
		t.Errorf("procEnv missing AWS_PROFILE=prod-ci, got %v", env)
	}

	// ts_env unset → no TS_ENV entry.
	c3, _ := parseConn(map[string]any{})
	for _, e := range c3.procEnv() {
		if strings.HasPrefix(e, "TS_ENV=") {
			t.Errorf("procEnv should not set TS_ENV when ts_env is unset, got %q", e)
		}
	}

	if _, err := parseConn(map[string]any{"timeout": "not-a-duration"}); err == nil {
		t.Error("expected error for invalid timeout")
	}
}

// TestDescribe asserts the declared surface: kind, type, capabilities, and
// every verb.
func TestDescribe(t *testing.T) {
	d := terraspacePlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "terraspace" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Commands, "terraspace") || !contains(d.Capabilities.Commands, "terraform") || !contains(d.Capabilities.Commands, "tofu") {
		t.Fatalf("capabilities.Commands: %#v", d.Capabilities.Commands)
	}
	if !d.Capabilities.Spawns {
		t.Fatalf("capabilities.Spawns: want true")
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Fatalf("capabilities.Egress: want empty, got %#v", d.Capabilities.Egress)
	}
	want := []string{"up", "down", "plan", "all_up", "all_down", "all_plan",
		"output", "logs", "list", "new", "import", "console", "state",
		"build", "clean", "fmt", "validate", "test", "info", "cli"}
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
}

// TestInvokeShim drives runTerraspace end to end against a fake terraspace
// binary, proving TS_ENV/dir wiring and the exit_code/data (not error)
// contract for a non-zero exit.
func TestInvokeShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "faketerraspace")
	script := "#!/bin/sh\n" +
		"echo \"TS_ENV=$TS_ENV\"\n" +
		"echo \"PWD=$(pwd -P)\"\n" +
		"case \"$1\" in\n" +
		"up) exit 0 ;;\n" +
		"down) exit 1 ;;\n" +
		"*) echo \"unhandled $*\" >&2; exit 2 ;;\nesac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	projDir := t.TempDir()
	p := terraspacePlugin{}

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "up",
		Connection: map[string]any{"binary": shim, "dir": projDir, "ts_env": "staging"},
		Options:    map[string]any{"stack": "vpc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 {
		t.Fatalf("up outputs: %#v", res.Outputs)
	}
	stdout, _ := res.Outputs["stdout"].(string)
	if !strings.Contains(stdout, "TS_ENV=staging") {
		t.Errorf("stdout missing TS_ENV=staging, got %q", stdout)
	}
	resolvedProj, _ := filepath.EvalSymlinks(projDir)
	if !strings.Contains(stdout, "PWD="+resolvedProj) {
		t.Errorf("stdout missing PWD=%s, got %q", resolvedProj, stdout)
	}

	// A non-zero exit is DATA, not an RPC error.
	res, err = p.Invoke(plugin.InvokeRequest{
		Verb:       "down",
		Connection: map[string]any{"binary": shim, "dir": projDir},
		Options:    map[string]any{"stack": "vpc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 1 {
		t.Fatalf("down outputs: %#v", res.Outputs)
	}
}

// TestRequiredOptionErrors proves a missing required option surfaces as
// CodeInvalidParams, and an unknown verb likewise.
func TestRequiredOptionErrors(t *testing.T) {
	p := terraspacePlugin{}
	cases := []struct {
		name string
		req  plugin.InvokeRequest
	}{
		{"missing stack", plugin.InvokeRequest{Verb: "up", Options: map[string]any{}}},
		{"unknown verb", plugin.InvokeRequest{Verb: "nope", Options: map[string]any{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Invoke(tc.req)
			if err == nil {
				t.Fatal("expected error")
			}
			var re *plugin.Error
			if !asPluginError(err, &re) || re.Code != plugin.CodeInvalidParams {
				t.Fatalf("want CodeInvalidParams, got %v", err)
			}
		})
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

// asPluginError unwraps a *plugin.Error without importing errors just for the
// test (the SDK returns the concrete type directly).
func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}
