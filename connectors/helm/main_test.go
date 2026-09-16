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
// the helm CLI, proven without spawning anything.
func TestVerbArgs(t *testing.T) {
	cases := []struct {
		name string
		verb string
		opts map[string]any
		want []string
	}{
		{
			name: "install full",
			verb: "install",
			opts: map[string]any{
				"name": "web", "chart": "bitnami/nginx", "version": "1.2.3",
				"values":           []any{"a.yaml", "b.yaml"},
				"set":              map[string]any{"b": "2", "a": "1"}, // keys sort a,b
				"set_string":       []any{"tag=v1"},
				"create_namespace": true, "wait": true, "atomic": true, "dry_run": true,
				"timeout": "5m", "repo": "https://charts.example.com",
			},
			want: []string{"install", "web", "bitnami/nginx",
				"--create-namespace", "--wait", "--atomic", "--dry-run", "--timeout", "5m",
				"--version", "1.2.3", "-f", "a.yaml", "-f", "b.yaml",
				"--set", "a=1", "--set", "b=2", "--set-string", "tag=v1",
				"--repo", "https://charts.example.com"},
		},
		{
			name: "install minimal",
			verb: "install",
			opts: map[string]any{"name": "web", "chart": "bitnami/nginx"},
			want: []string{"install", "web", "bitnami/nginx"},
		},
		{
			name: "upgrade install force",
			verb: "upgrade",
			opts: map[string]any{
				"name": "web", "chart": "bitnami/nginx",
				"install": true, "reuse_values": true, "force": true,
			},
			want: []string{"upgrade", "web", "bitnami/nginx", "--install", "--reuse-values", "--force"},
		},
		{
			name: "uninstall",
			verb: "uninstall",
			opts: map[string]any{"name": "web", "keep_history": true, "wait": true, "timeout": "2m"},
			want: []string{"uninstall", "web", "--keep-history", "--wait", "--timeout", "2m"},
		},
		{
			name: "rollback with revision",
			verb: "rollback",
			opts: map[string]any{"name": "web", "revision": 3, "wait": true},
			want: []string{"rollback", "web", "3", "--wait"},
		},
		{
			name: "rollback previous",
			verb: "rollback",
			opts: map[string]any{"name": "web"},
			want: []string{"rollback", "web"},
		},
		{
			name: "list",
			verb: "list",
			opts: map[string]any{"all": true, "all_namespaces": true, "filter": "^web"},
			want: []string{"list", "-a", "-A", "--filter", "^web", "-o", "json"},
		},
		{
			name: "status",
			verb: "status",
			opts: map[string]any{"name": "web", "revision": 2},
			want: []string{"status", "web", "--revision", "2", "-o", "json"},
		},
		{
			name: "history",
			verb: "history",
			opts: map[string]any{"name": "web"},
			want: []string{"history", "web", "-o", "json"},
		},
		{
			name: "get_values",
			verb: "get_values",
			opts: map[string]any{"name": "web", "all": true},
			want: []string{"get", "values", "web", "-a", "-o", "json"},
		},
		{
			name: "template",
			verb: "template",
			opts: map[string]any{
				"name": "web", "chart": "bitnami/nginx", "values": []any{"a.yaml"},
				"set": map[string]any{"x": "1"}, "version": "1.0.0", "show_only": []any{"templates/deploy.yaml"},
			},
			want: []string{"template", "web", "bitnami/nginx", "-f", "a.yaml", "--set", "x=1",
				"--version", "1.0.0", "-s", "templates/deploy.yaml"},
		},
		{
			name: "pull",
			verb: "pull",
			opts: map[string]any{"chart": "bitnami/nginx", "version": "1.2.3", "destination": "/tmp/charts", "untar": true, "repo": "https://x"},
			want: []string{"pull", "bitnami/nginx", "--version", "1.2.3", "-d", "/tmp/charts", "--untar", "--repo", "https://x"},
		},
		{
			name: "repo_add",
			verb: "repo_add",
			opts: map[string]any{"name": "bitnami", "url": "https://charts.bitnami.com/bitnami", "username": "u", "password": "p", "force_update": true},
			want: []string{"repo", "add", "bitnami", "https://charts.bitnami.com/bitnami", "--username", "u", "--password", "p", "--force-update"},
		},
		{
			name: "repo_update names",
			verb: "repo_update",
			opts: map[string]any{"names": []any{"bitnami", "stable"}},
			want: []string{"repo", "update", "bitnami", "stable"},
		},
		{
			name: "repo_update all",
			verb: "repo_update",
			opts: map[string]any{},
			want: []string{"repo", "update"},
		},
		{
			name: "test",
			verb: "test",
			opts: map[string]any{"name": "web", "timeout": "1m"},
			want: []string{"test", "web", "--timeout", "1m"},
		},
		{
			name: "lint",
			verb: "lint",
			opts: map[string]any{"chart": "./charts/web", "values": []any{"a.yaml"}, "strict": true},
			want: []string{"lint", "./charts/web", "-f", "a.yaml", "--strict"},
		},
		{
			name: "set list form",
			verb: "install",
			opts: map[string]any{"name": "web", "chart": "c", "set": []any{"b=2", "a=1"}},
			want: []string{"install", "web", "c", "--set", "b=2", "--set", "a=1"},
		},
		{
			name: "cli escape hatch",
			verb: "cli",
			opts: map[string]any{"args": []any{"version", "--short"}},
			want: []string{"version", "--short"},
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
		{"install", map[string]any{}},             // no name/chart
		{"install", map[string]any{"name": "w"}},  // no chart
		{"install", map[string]any{"chart": "c"}}, // no name
		{"upgrade", map[string]any{}},             // no name/chart
		{"uninstall", map[string]any{}},           // no name
		{"rollback", map[string]any{}},            // no name
		{"status", map[string]any{}},              // no name
		{"history", map[string]any{}},             // no name
		{"get_values", map[string]any{}},          // no name
		{"template", map[string]any{}},            // no name/chart
		{"template", map[string]any{"name": "w"}}, // no chart
		{"pull", map[string]any{}},                // no chart
		{"repo_add", map[string]any{}},            // no name/url
		{"repo_add", map[string]any{"name": "n"}}, // no url
		{"test", map[string]any{}},                // no name
		{"lint", map[string]any{}},                // no chart
		{"cli", map[string]any{}},                 // no args
		{"nope", map[string]any{}},                // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbArgs(tc.verb, tc.opts); err == nil {
			t.Errorf("verbArgs(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers the connection flags and process-env derivation.
func TestParseConn(t *testing.T) {
	c, err := parseConn(map[string]any{
		"kubeconfig": "/home/u/.kube/config", "kube_context": "prod", "namespace": "ns1",
		"env": map[string]any{"HTTP_PROXY": "http://p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "helm" {
		t.Errorf("binary: got %q want helm", c.binary)
	}
	want := []string{"--kubeconfig", "/home/u/.kube/config", "--kube-context", "prod", "-n", "ns1"}
	if got := c.connFlags(); !reflect.DeepEqual(got, want) {
		t.Errorf("connFlags: got %#v want %#v", got, want)
	}
	env := c.procEnv()
	if !contains(env, "HTTP_PROXY=http://p") {
		t.Errorf("procEnv missing HTTP_PROXY")
	}
	// binary override.
	c2, _ := parseConn(map[string]any{"binary": "/usr/local/bin/helm"})
	if c2.binary != "/usr/local/bin/helm" {
		t.Errorf("binary override: got %q", c2.binary)
	}
	// default timeout.
	if c2.timeout.String() != "10m0s" {
		t.Errorf("default timeout: got %v", c2.timeout)
	}
}

// TestDescribe asserts the declared surface: kind, capabilities, and every verb.
func TestDescribe(t *testing.T) {
	d := helmPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "helm" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Commands, "helm") || !d.Capabilities.Spawns {
		t.Fatalf("capabilities: %#v", d.Capabilities)
	}
	want := []string{"install", "upgrade", "uninstall", "rollback", "list", "status",
		"history", "get_values", "template", "pull", "repo_add", "repo_update", "test", "lint", "cli"}
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

// TestInvokeShim drives runHelm + enrich end to end against a fake helm
// binary, proving list -o json parsing wires up.
func TestInvokeShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "fakehelm")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"list) echo '[{\"name\":\"web\",\"status\":\"deployed\"}]' ;;\n" +
		"install) echo installed ;;\n" +
		"*) echo \"unhandled $*\" >&2; exit 2 ;;\nesac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := helmPlugin{}
	conn := map[string]any{"binary": shim}

	// list -o json → parsed releases[]
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "list", Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	list, ok := res.Outputs["releases"].([]map[string]any)
	if !ok || len(list) != 1 || list[0]["name"] != "web" {
		t.Fatalf("list releases: %#v", res.Outputs["releases"])
	}
	if res.Outputs["exit_code"] != 0 {
		t.Fatalf("list exit_code: %#v", res.Outputs["exit_code"])
	}

	// install → non-json stdout passes through untouched, no error.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "install", Connection: conn,
		Options: map[string]any{"name": "web", "chart": "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 || res.Outputs["stdout"] != "installed\n" {
		t.Fatalf("install outputs: %#v", res.Outputs)
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
