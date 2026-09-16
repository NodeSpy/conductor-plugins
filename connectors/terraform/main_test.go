package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestVerbArgs pins the exact argv each verb builds — the whole contract with
// the terraform CLI, proven without spawning anything.
func TestVerbArgs(t *testing.T) {
	cases := []struct {
		name string
		verb string
		opts map[string]any
		want []string
	}{
		{
			name: "init full",
			verb: "init",
			opts: map[string]any{
				"backend_config": []any{"bucket=tf-state", "key=prod.tfstate"},
				"upgrade":        true, "reconfigure": true, "no_color": true,
			},
			want: []string{"init", "-backend-config=bucket=tf-state", "-backend-config=key=prod.tfstate",
				"-upgrade", "-reconfigure", "-no-color"},
		},
		{
			name: "init bare",
			verb: "init",
			opts: map[string]any{},
			want: []string{"init"},
		},
		{
			name: "validate json",
			verb: "validate",
			opts: map[string]any{"json": true},
			want: []string{"validate", "-json"},
		},
		{
			name: "plan with vars, var_files, targets",
			verb: "plan",
			opts: map[string]any{
				"out":       "plan.tfplan",
				"var":       map[string]any{"b": "2", "a": "1"}, // keys sort a,b
				"var_files": []any{"prod.tfvars"}, "target": []any{"aws_instance.web"},
				"json": true,
			},
			want: []string{"plan", "-out=plan.tfplan", "-var", "a=1", "-var", "b=2",
				"-var-file=prod.tfvars", "-target=aws_instance.web", "-json", "-input=false"},
		},
		{
			name: "plan destroy refresh-only detailed-exitcode",
			verb: "plan",
			opts: map[string]any{"destroy": true, "refresh_only": true, "detailed_exitcode": true, "no_color": true},
			want: []string{"plan", "-destroy", "-refresh-only", "-detailed-exitcode", "-no-color", "-input=false"},
		},
		{
			name: "apply default auto-approve",
			verb: "apply",
			opts: map[string]any{},
			want: []string{"apply", "-auto-approve", "-input=false"},
		},
		{
			name: "apply auto-approve disabled with plan file",
			verb: "apply",
			opts: map[string]any{"auto_approve": false, "plan_file": "plan.tfplan"},
			want: []string{"apply", "-input=false", "plan.tfplan"},
		},
		{
			name: "apply with vars and targets",
			verb: "apply",
			opts: map[string]any{"var": map[string]any{"x": "1"}, "target": []any{"aws_instance.web"}, "json": true},
			want: []string{"apply", "-auto-approve", "-var", "x=1", "-target=aws_instance.web", "-json", "-input=false"},
		},
		{
			name: "destroy default auto-approve",
			verb: "destroy",
			opts: map[string]any{"var_files": []any{"prod.tfvars"}},
			want: []string{"destroy", "-auto-approve", "-var-file=prod.tfvars", "-input=false"},
		},
		{
			name: "destroy auto-approve disabled",
			verb: "destroy",
			opts: map[string]any{"auto_approve": false},
			want: []string{"destroy", "-input=false"},
		},
		{
			name: "output default json",
			verb: "output",
			opts: map[string]any{},
			want: []string{"output", "-json"},
		},
		{
			name: "output named raw",
			verb: "output",
			opts: map[string]any{"name": "endpoint", "raw": true},
			want: []string{"output", "-raw", "endpoint"},
		},
		{
			name: "output json false",
			verb: "output",
			opts: map[string]any{"json": false, "name": "endpoint"},
			want: []string{"output", "endpoint"},
		},
		{
			name: "show default json",
			verb: "show",
			opts: map[string]any{},
			want: []string{"show", "-json"},
		},
		{
			name: "show path json false",
			verb: "show",
			opts: map[string]any{"path": "plan.tfplan", "json": false},
			want: []string{"show", "plan.tfplan"},
		},
		{
			name: "fmt defaults",
			verb: "fmt",
			opts: map[string]any{"check": true, "diff": true, "recursive": true},
			want: []string{"fmt", "-check", "-diff", "-recursive"},
		},
		{
			name: "fmt write false",
			verb: "fmt",
			opts: map[string]any{"write": false},
			want: []string{"fmt", "-write=false"},
		},
		{
			name: "workspace select",
			verb: "workspace",
			opts: map[string]any{"subcommand": "select", "name": "prod"},
			want: []string{"workspace", "select", "prod"},
		},
		{
			name: "workspace list",
			verb: "workspace",
			opts: map[string]any{"subcommand": "list"},
			want: []string{"workspace", "list"},
		},
		{
			name: "state show",
			verb: "state",
			opts: map[string]any{"subcommand": "show", "args": []any{"aws_instance.web"}},
			want: []string{"state", "show", "aws_instance.web"},
		},
		{
			name: "state list",
			verb: "state",
			opts: map[string]any{"subcommand": "list"},
			want: []string{"state", "list"},
		},
		{
			name: "import with vars",
			verb: "import",
			opts: map[string]any{"address": "aws_instance.web", "id": "i-1234", "var": map[string]any{"x": "1"}},
			want: []string{"import", "-var", "x=1", "aws_instance.web", "i-1234"},
		},
		{
			name: "refresh",
			verb: "refresh",
			opts: map[string]any{"target": []any{"aws_instance.web"}, "no_color": true},
			want: []string{"refresh", "-target=aws_instance.web", "-no-color", "-input=false"},
		},
		{
			name: "providers",
			verb: "providers",
			opts: map[string]any{},
			want: []string{"providers"},
		},
		{
			name: "version json",
			verb: "version",
			opts: map[string]any{"json": true},
			want: []string{"version", "-json"},
		},
		{
			name: "cli escape hatch",
			verb: "cli",
			opts: map[string]any{"args": []any{"graph"}},
			want: []string{"graph"},
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

// TestChdirPrecedesSubcommand proves -chdir is a connFlag emitted before the
// subcommand argv, matching terraform's global-flag placement rule.
func TestChdirPrecedesSubcommand(t *testing.T) {
	conn, err := parseConn(map[string]any{"chdir": "/infra/prod"})
	if err != nil {
		t.Fatal(err)
	}
	args, err := verbArgs("plan", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	full := append(conn.connFlags(), args...)
	want := []string{"-chdir=/infra/prod", "plan", "-input=false"}
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("full argv:\n got: %#v\nwant: %#v", full, want)
	}
}

// TestEngineTofu proves OpenTofu is a 100% drop-in: engine=tofu resolves the
// "tofu" binary and builds byte-identical argv (including -chdir placement)
// to plain terraform.
func TestEngineTofu(t *testing.T) {
	conn, err := parseConn(map[string]any{"engine": "tofu", "chdir": "/infra/prod"})
	if err != nil {
		t.Fatal(err)
	}
	if conn.binary != "tofu" {
		t.Errorf("tofu binary: got %q", conn.binary)
	}
	args, err := verbArgs("apply", map[string]any{"var": map[string]any{"x": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	full := append(conn.connFlags(), args...)
	want := []string{"-chdir=/infra/prod", "apply", "-auto-approve", "-var", "x=1", "-input=false"}
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("tofu full argv:\n got: %#v\nwant: %#v", full, want)
	}
}

// TestEngineTerragruntChdirAndRunAll proves terragrunt has no -chdir flag —
// chdir must instead become the spawned process's working directory — and
// that run_all prefixes the subcommand with run-all.
func TestEngineTerragruntChdirAndRunAll(t *testing.T) {
	conn, err := parseConn(map[string]any{"engine": "terragrunt", "chdir": "/infra/prod"})
	if err != nil {
		t.Fatal(err)
	}
	if conn.binary != "terragrunt" {
		t.Errorf("terragrunt binary: got %q", conn.binary)
	}
	if got := conn.connFlags(); got != nil {
		t.Errorf("terragrunt connFlags should never emit -chdir: %#v", got)
	}

	args, err := verbArgs("apply", map[string]any{"run_all": true})
	if err != nil {
		t.Fatal(err)
	}
	full := append(conn.connFlags(), args...)
	want := []string{"run-all", "apply", "-auto-approve", "-input=false"}
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("terragrunt run-all argv:\n got: %#v\nwant: %#v", full, want)
	}
}

// TestTerragruntCwd proves runTerraform sets cmd.Dir (not -chdir) for the
// terragrunt engine, by spawning a fake binary that prints its own working
// directory.
func TestTerragruntCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "faketerragrunt")
	script := "#!/bin/sh\npwd\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	conn, err := parseConn(map[string]any{"engine": "terragrunt", "binary": shim, "chdir": workdir})
	if err != nil {
		t.Fatal(err)
	}
	args, err := verbArgs("plan", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if hasChdirFlag(append(conn.connFlags(), args...)) {
		t.Fatalf("terragrunt argv must not contain -chdir: %#v", append(conn.connFlags(), args...))
	}
	res, err := runTerraform(context.Background(), conn, args)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(res["stdout"].(string))
	// resolve symlinks (e.g. /tmp -> /private/tmp on macOS) before comparing.
	wantDir, _ := filepath.EvalSymlinks(workdir)
	gotDir, _ := filepath.EvalSymlinks(got)
	if gotDir != wantDir {
		t.Fatalf("terragrunt cwd: got %q want %q", got, workdir)
	}
}

func hasChdirFlag(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-chdir=") {
			return true
		}
	}
	return false
}

// TestParseConnEngine validates the engine enum.
func TestParseConnEngine(t *testing.T) {
	for _, e := range []string{"terraform", "tofu", "terragrunt"} {
		c, err := parseConn(map[string]any{"engine": e})
		if err != nil {
			t.Fatalf("engine %q: unexpected error: %v", e, err)
		}
		if c.binary != e {
			t.Errorf("engine %q: default binary got %q", e, c.binary)
		}
	}
	if _, err := parseConn(map[string]any{"engine": "opentofu"}); err == nil {
		t.Errorf("engine \"opentofu\": expected validation error, got nil")
	}
	if c, err := parseConn(map[string]any{}); err != nil || c.engine != "terraform" {
		t.Errorf("default engine: got %q, err %v", c.engine, err)
	}
}

// TestVerbArgsErrors covers required-field validation and unknown verbs.
func TestVerbArgsErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"workspace", map[string]any{}},                           // no subcommand
		{"workspace", map[string]any{"subcommand": "select"}},     // no name for select
		{"workspace", map[string]any{"subcommand": "new"}},        // no name for new
		{"workspace", map[string]any{"subcommand": "delete"}},     // no name for delete
		{"state", map[string]any{}},                               // no subcommand
		{"import", map[string]any{}},                              // no address/id
		{"import", map[string]any{"address": "aws_instance.web"}}, // no id
		{"import", map[string]any{"id": "i-1234"}},                // no address
		{"cli", map[string]any{}},                                 // no args
		{"nope", map[string]any{}},                                // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbArgs(tc.verb, tc.opts); err == nil {
			t.Errorf("verbArgs(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers binary default/override, chdir, and process-env
// derivation.
func TestParseConn(t *testing.T) {
	c, err := parseConn(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "terraform" {
		t.Errorf("binary default: got %q", c.binary)
	}
	if got := c.connFlags(); got != nil {
		t.Errorf("connFlags with no chdir: %#v", got)
	}

	c2, err := parseConn(map[string]any{
		"binary": "/usr/local/bin/terraform", "chdir": "/infra",
		"env": map[string]any{"TF_VAR_region": "us-east-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c2.binary != "/usr/local/bin/terraform" {
		t.Errorf("binary override: got %q", c2.binary)
	}
	if got := c2.connFlags(); !reflect.DeepEqual(got, []string{"-chdir=/infra"}) {
		t.Errorf("connFlags: %#v", got)
	}
	if !contains(c2.procEnv(), "TF_VAR_region=us-east-1") {
		t.Errorf("procEnv missing TF_VAR_region")
	}
}

// TestDescribe asserts the declared surface: kind, capabilities, and every verb.
func TestDescribe(t *testing.T) {
	d := terraformPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "terraform" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Commands, "terraform") || !d.Capabilities.Spawns {
		t.Fatalf("capabilities: %#v", d.Capabilities)
	}
	want := []string{"init", "validate", "plan", "apply", "destroy", "output", "show",
		"fmt", "workspace", "state", "import", "refresh", "providers", "version", "cli"}
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

// TestInvokeShim drives runTerraform + enrich end to end against a fake
// terraform binary, proving output -json parsing wires up to `outputs`.
func TestInvokeShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "faketerraform")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"output) echo '{\"endpoint\":{\"value\":\"https://example.com\",\"type\":\"string\"}}' ;;\n" +
		"plan) exit 2 ;;\n" +
		"*) echo \"unhandled $*\" >&2; exit 2 ;;\nesac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := terraformPlugin{}
	conn := map[string]any{"binary": shim}

	// output -json → parsed outputs
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "output", Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 {
		t.Fatalf("output exit_code: %#v", res.Outputs)
	}
	outputs, ok := res.Outputs["outputs"].(map[string]any)
	if !ok {
		t.Fatalf("outputs not parsed: %#v", res.Outputs["outputs"])
	}
	endpoint, ok := outputs["endpoint"].(map[string]any)
	if !ok || endpoint["value"] != "https://example.com" {
		t.Fatalf("outputs.endpoint: %#v", outputs["endpoint"])
	}

	// plan with detailed_exitcode returning 2 (changes present) is DATA, not
	// an RPC error — and enrich must not try to parse the non-json stdout.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "plan", Connection: conn, Options: map[string]any{"detailed_exitcode": true}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 2 {
		t.Fatalf("plan exit_code: %#v", res.Outputs)
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
