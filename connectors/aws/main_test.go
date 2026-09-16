package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestVerbArgs pins the exact argv each verb builds — the whole contract with
// the aws CLI, proven without spawning anything.
func TestVerbArgs(t *testing.T) {
	cases := []struct {
		name   string
		verb   string
		opts   map[string]any
		output string
		want   []string
	}{
		{
			name: "run params sorted plus output json",
			verb: "run",
			opts: map[string]any{
				"service": "s3api", "operation": "list-buckets",
				"params": map[string]any{"max-items": "10", "bucket": "b", "all": true, "quiet": false},
				"args":   []any{"--no-paginate"},
			},
			output: "json",
			want: []string{"s3api", "list-buckets",
				"--all", "--bucket", "b", "--max-items", "10", // sorted: all, bucket, max-items (quiet skipped)
				"--no-paginate", "--output", "json"},
		},
		{
			name:   "run repeated list param",
			verb:   "run",
			opts:   map[string]any{"service": "ec2", "operation": "describe-instances", "params": map[string]any{"filters": []any{"Name=x,Values=y", "Name=z,Values=w"}}},
			output: "json",
			want:   []string{"ec2", "describe-instances", "--filters", "Name=x,Values=y", "--filters", "Name=z,Values=w", "--output", "json"},
		},
		{
			name:   "run default output when empty",
			verb:   "run",
			opts:   map[string]any{"service": "sts", "operation": "get-caller-identity"},
			output: "",
			want:   []string{"sts", "get-caller-identity", "--output", "json"},
		},
		{
			name: "s3 sync with exclude/delete does not append output",
			verb: "s3",
			opts: map[string]any{
				"subcommand": "sync", "src": "./dist", "dst": "s3://bucket/path",
				"delete": true, "exclude": []any{"*.tmp", ".git/*"}, "dryrun": true,
			},
			output: "json",
			want:   []string{"s3", "sync", "--delete", "--exclude", "*.tmp", "--exclude", ".git/*", "--dryrun", "./dist", "s3://bucket/path"},
		},
		{
			name:   "s3 ls bucket only",
			verb:   "s3",
			opts:   map[string]any{"subcommand": "ls", "src": "s3://bucket/"},
			output: "json",
			want:   []string{"s3", "ls", "s3://bucket/"},
		},
		{
			name:   "lambda_invoke string payload uses /dev/stdout",
			verb:   "lambda_invoke",
			opts:   map[string]any{"function": "myFn", "payload": `{"k":"v"}`, "invocation_type": "RequestResponse", "log_type": "Tail", "qualifier": "prod"},
			output: "json",
			want: []string{"lambda", "invoke", "--function-name", "myFn", "--payload", `{"k":"v"}`,
				"--invocation-type", "RequestResponse", "--log-type", "Tail", "--qualifier", "prod",
				"--output", "json", "/dev/stdout"},
		},
		{
			name:   "lambda_invoke object payload is marshaled",
			verb:   "lambda_invoke",
			opts:   map[string]any{"function": "myFn", "payload": map[string]any{"k": "v"}},
			output: "json",
			want:   []string{"lambda", "invoke", "--function-name", "myFn", "--payload", `{"k":"v"}`, "--output", "json", "/dev/stdout"},
		},
		{
			name:   "sts_identity",
			verb:   "sts_identity",
			opts:   map[string]any{},
			output: "json",
			want:   []string{"sts", "get-caller-identity", "--output", "json"},
		},
		{
			name:   "cli escape hatch",
			verb:   "cli",
			opts:   map[string]any{"args": []any{"configure", "list"}},
			output: "json",
			want:   []string{"configure", "list"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verbArgs(tc.verb, tc.opts, tc.output)
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
		{"run", map[string]any{}},                   // no service/operation
		{"run", map[string]any{"service": "s3api"}}, // no operation
		{"s3", map[string]any{}},                    // no subcommand
		{"lambda_invoke", map[string]any{}},         // no function
		{"cli", map[string]any{}},                   // no args
		{"nope", map[string]any{}},                  // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbArgs(tc.verb, tc.opts, "json"); err == nil {
			t.Errorf("verbArgs(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers connection defaults, overrides, and process-env
// derivation.
func TestParseConn(t *testing.T) {
	c, err := parseConn(map[string]any{
		"profile": "ci", "region": "us-east-1", "endpoint_url": "http://localhost:4566",
		"env": map[string]any{"AWS_PROFILE": "ci"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "aws" {
		t.Errorf("binary: got %q want aws", c.binary)
	}
	if c.output != "json" {
		t.Errorf("output default: got %q want json", c.output)
	}
	want := []string{"--profile", "ci", "--region", "us-east-1", "--endpoint-url", "http://localhost:4566"}
	if got := c.connFlags(); !reflect.DeepEqual(got, want) {
		t.Errorf("connFlags: got %#v want %#v", got, want)
	}
	if !contains(c.procEnv(), "AWS_PROFILE=ci") {
		t.Errorf("procEnv missing AWS_PROFILE=ci")
	}

	// binary and output overrides.
	c2, err := parseConn(map[string]any{"binary": "/usr/local/bin/aws", "output": "table"})
	if err != nil {
		t.Fatal(err)
	}
	if c2.binary != "/usr/local/bin/aws" {
		t.Errorf("binary override: got %q", c2.binary)
	}
	if c2.output != "table" {
		t.Errorf("output override: got %q", c2.output)
	}

	// bad timeout is an error.
	if _, err := parseConn(map[string]any{"timeout": "not-a-duration"}); err == nil {
		t.Fatal("expected error for invalid timeout")
	}
}

// TestDescribe asserts the declared surface: kind, type, capabilities, and
// every verb.
func TestDescribe(t *testing.T) {
	d := awsPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "aws" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Commands, "aws") || !d.Capabilities.Spawns {
		t.Fatalf("capabilities: %#v", d.Capabilities)
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Fatalf("expected no declared egress, got %#v", d.Capabilities.Egress)
	}
	want := []string{"run", "s3", "lambda_invoke", "sts_identity", "cli"}
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

// TestInvokeShim drives runAWS + enrich end to end against a fake aws binary,
// proving `run`'s JSON parsing into `result` wires up. Never requires a real
// aws installation.
func TestInvokeShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "fakeaws")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"\"s3api list-buckets\") echo '{\"Buckets\":[{\"Name\":\"b\"}]}' ;;\n" +
		"\"sts get-caller-identity\") echo '{\"Account\":\"123\",\"Arn\":\"arn:aws:iam::123:root\"}' ;;\n" +
		"*) echo \"unhandled $*\" >&2; exit 2 ;;\nesac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := awsPlugin{}
	conn := map[string]any{"binary": shim}

	// run → parsed result{}
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "run", Connection: conn,
		Options: map[string]any{"service": "s3api", "operation": "list-buckets"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 {
		t.Fatalf("run exit_code: %#v", res.Outputs["exit_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("run result: %#v", res.Outputs["result"])
	}
	buckets, ok := result["Buckets"].([]any)
	if !ok || len(buckets) != 1 {
		t.Fatalf("run result.Buckets: %#v", result["Buckets"])
	}

	// sts_identity → parsed identity{}
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "sts_identity", Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := res.Outputs["identity"].(map[string]any)
	if !ok || identity["Account"] != "123" {
		t.Fatalf("sts_identity identity: %#v", res.Outputs["identity"])
	}

	// unhandled verb-shaped call surfaces a non-zero exit as DATA, not an error.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "cli", Connection: conn, Options: map[string]any{"args": []any{"bogus"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 2 {
		t.Fatalf("cli exit_code: %#v", res.Outputs["exit_code"])
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
