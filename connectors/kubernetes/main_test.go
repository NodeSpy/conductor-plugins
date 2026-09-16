package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestVerbArgs pins the exact argv (and stdin, where relevant) each verb
// builds — the whole contract with the kubectl CLI, proven without spawning
// anything.
func TestVerbArgs(t *testing.T) {
	cases := []struct {
		name      string
		verb      string
		opts      map[string]any
		want      []string
		wantStdin string
	}{
		{
			name: "apply files",
			verb: "apply",
			opts: map[string]any{"filename": []any{"a.yaml", "b.yaml"}, "recursive": true, "prune": true, "server_side": true, "force": true, "output": "json"},
			want: []string{"apply", "-f", "a.yaml", "-f", "b.yaml", "-R", "--prune", "--server-side", "--force", "-o", "json"},
		},
		{
			name:      "apply manifest via stdin",
			verb:      "apply",
			opts:      map[string]any{"manifest": "kind: Pod\n"},
			want:      []string{"apply", "-f", "-"},
			wantStdin: "kind: Pod\n",
		},
		{
			name: "delete resource and name",
			verb: "delete",
			opts: map[string]any{"resource": "pod", "name": "web-0", "grace_period": 0, "force": true, "ignore_not_found": true},
			want: []string{"delete", "pod", "web-0", "--grace-period", "0", "--force", "--ignore-not-found"},
		},
		{
			name: "delete by selector all",
			verb: "delete",
			opts: map[string]any{"resource": "pods", "selector": "app=web", "all": true},
			want: []string{"delete", "pods", "-l", "app=web", "--all"},
		},
		{
			name: "delete by filename",
			verb: "delete",
			opts: map[string]any{"filename": []any{"a.yaml"}},
			want: []string{"delete", "-f", "a.yaml"},
		},
		{
			name: "get default json",
			verb: "get",
			opts: map[string]any{"resource": "pods", "selector": "app=web", "all_namespaces": true, "field_selector": "status.phase=Running"},
			want: []string{"get", "pods", "-l", "app=web", "-A", "--field-selector", "status.phase=Running", "-o", "json"},
		},
		{
			name: "get name and output wide",
			verb: "get",
			opts: map[string]any{"resource": "pod", "name": "web-0", "output": "wide"},
			want: []string{"get", "pod", "web-0", "-o", "wide"},
		},
		{
			name: "describe",
			verb: "describe",
			opts: map[string]any{"resource": "deployment", "name": "web"},
			want: []string{"describe", "deployment", "web"},
		},
		{
			name: "logs",
			verb: "logs",
			opts: map[string]any{"pod": "web-0", "container": "app", "tail": 100, "since": "1h", "previous": true, "all_containers": true},
			want: []string{"logs", "web-0", "-c", "app", "--tail", "100", "--since", "1h", "-p", "--all-containers"},
		},
		{
			name: "exec",
			verb: "exec",
			opts: map[string]any{"pod": "web-0", "container": "app", "command": []any{"ls", "-la"}, "stdin": true, "tty": true},
			want: []string{"exec", "-i", "-t", "-c", "app", "web-0", "--", "ls", "-la"},
		},
		{
			name: "rollout status",
			verb: "rollout",
			opts: map[string]any{"subcommand": "status", "resource": "deployment/web"},
			want: []string{"rollout", "status", "deployment/web"},
		},
		{
			name: "scale",
			verb: "scale",
			opts: map[string]any{"resource": "deployment/web", "replicas": 3},
			want: []string{"scale", "deployment/web", "--replicas", "3"},
		},
		{
			name: "patch",
			verb: "patch",
			opts: map[string]any{"resource": "deployment", "name": "web", "patch": `{"spec":{"replicas":3}}`, "type": "merge"},
			want: []string{"patch", "deployment", "web", "-p", `{"spec":{"replicas":3}}`, "--type", "merge"},
		},
		{
			name:      "create manifest via stdin",
			verb:      "create",
			opts:      map[string]any{"manifest": "kind: Pod\n"},
			want:      []string{"create", "-f", "-"},
			wantStdin: "kind: Pod\n",
		},
		{
			name: "create extra_args",
			verb: "create",
			opts: map[string]any{"extra_args": []any{"configmap", "cfg", "--from-literal=k=v"}},
			want: []string{"create", "configmap", "cfg", "--from-literal=k=v"},
		},
		{
			name: "label",
			verb: "label",
			opts: map[string]any{"resource": "pod", "name": "web-0", "pairs": map[string]any{"tier": "frontend", "env": "prod"}, "overwrite": true},
			want: []string{"label", "pod", "web-0", "env=prod", "tier=frontend", "--overwrite"},
		},
		{
			name: "annotate",
			verb: "annotate",
			opts: map[string]any{"resource": "pod", "name": "web-0", "pairs": map[string]any{"note": "hi"}},
			want: []string{"annotate", "pod", "web-0", "note=hi"},
		},
		{
			name: "wait",
			verb: "wait",
			opts: map[string]any{"resource": "pod", "name": "web-0", "for": "condition=Ready", "timeout": "30s"},
			want: []string{"wait", "pod", "web-0", "--for", "condition=Ready", "--timeout", "30s"},
		},
		{
			name: "top pods",
			verb: "top",
			opts: map[string]any{"subcommand": "pods", "selector": "app=web"},
			want: []string{"top", "pods", "-l", "app=web"},
		},
		{
			name: "cordon",
			verb: "cordon",
			opts: map[string]any{"node": "node-1"},
			want: []string{"cordon", "node-1"},
		},
		{
			name: "uncordon",
			verb: "uncordon",
			opts: map[string]any{"node": "node-1"},
			want: []string{"uncordon", "node-1"},
		},
		{
			name: "drain",
			verb: "drain",
			opts: map[string]any{"node": "node-1", "ignore_daemonsets": true, "delete_emptydir_data": true, "force": true},
			want: []string{"drain", "node-1", "--ignore-daemonsets", "--delete-emptydir-data", "--force"},
		},
		{
			name: "cp",
			verb: "cp",
			opts: map[string]any{"src": "web-0:/tmp/f", "dst": "./f", "container": "app"},
			want: []string{"cp", "web-0:/tmp/f", "./f", "-c", "app"},
		},
		{
			name: "cli escape hatch",
			verb: "cli",
			opts: map[string]any{"args": []any{"api-resources"}},
			want: []string{"api-resources"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, stdin, err := verbArgs(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("verbArgs(%s): unexpected error: %v", tc.verb, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("verbArgs(%s)\n got: %#v\nwant: %#v", tc.verb, got, tc.want)
			}
			if stdin != tc.wantStdin {
				t.Fatalf("verbArgs(%s) stdin: got %q want %q", tc.verb, stdin, tc.wantStdin)
			}
		})
	}
}

// TestConnFlagsPrecedeSubcommand proves connection-level global flags
// (--kubeconfig/--context/-n) are emitted before the subcommand argv.
func TestConnFlagsPrecedeSubcommand(t *testing.T) {
	conn, err := parseConn(map[string]any{"context": "prod", "namespace": "web", "kubeconfig": "/k/config"})
	if err != nil {
		t.Fatal(err)
	}
	args, _, err := verbArgs("get", map[string]any{"resource": "pods"})
	if err != nil {
		t.Fatal(err)
	}
	full := append(conn.connFlags(), args...)
	want := []string{"--kubeconfig", "/k/config", "--context", "prod", "-n", "web", "get", "pods", "-o", "json"}
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("full argv\n got: %#v\nwant: %#v", full, want)
	}
}

// TestVerbArgsErrors covers required-field validation and unknown verbs.
func TestVerbArgsErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"apply", map[string]any{}},                               // no filename/manifest
		{"delete", map[string]any{}},                              // no resource/filename
		{"get", map[string]any{}},                                 // no resource
		{"describe", map[string]any{}},                            // no resource
		{"logs", map[string]any{}},                                // no pod
		{"exec", map[string]any{"pod": "x"}},                      // no command
		{"exec", map[string]any{"command": []any{"ls"}}},          // no pod
		{"rollout", map[string]any{}},                             // no subcommand/resource
		{"rollout", map[string]any{"subcommand": "status"}},       // no resource
		{"scale", map[string]any{"resource": "deployment/web"}},   // no replicas
		{"patch", map[string]any{"resource": "pod", "name": "x"}}, // no patch
		{"create", map[string]any{}},                              // nothing to create
		{"label", map[string]any{"resource": "pod", "name": "x"}}, // no pairs
		{"annotate", map[string]any{"resource": "pod"}},           // no name
		{"wait", map[string]any{"resource": "pod"}},               // no for
		{"top", map[string]any{}},                                 // no subcommand
		{"cordon", map[string]any{}},                              // no node
		{"uncordon", map[string]any{}},                            // no node
		{"drain", map[string]any{}},                               // no node
		{"cp", map[string]any{"src": "a"}},                        // no dst
		{"cli", map[string]any{}},                                 // no args
		{"nope", map[string]any{}},                                // unknown verb
	}
	for _, tc := range cases {
		if _, _, err := verbArgs(tc.verb, tc.opts); err == nil {
			t.Errorf("verbArgs(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers binary default/override and timeout parsing.
func TestParseConn(t *testing.T) {
	c, err := parseConn(map[string]any{
		"kubeconfig": "/k/config", "context": "prod", "namespace": "web",
		"env": map[string]any{"HTTP_PROXY": "http://p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "kubectl" {
		t.Errorf("binary: got %q want kubectl", c.binary)
	}
	if got := c.connFlags(); !reflect.DeepEqual(got, []string{"--kubeconfig", "/k/config", "--context", "prod", "-n", "web"}) {
		t.Errorf("connFlags: %#v", got)
	}
	if !contains(c.procEnv(), "HTTP_PROXY=http://p") {
		t.Error("procEnv missing HTTP_PROXY")
	}
	// binary override.
	c2, _ := parseConn(map[string]any{"binary": "/usr/local/bin/kubectl"})
	if c2.binary != "/usr/local/bin/kubectl" {
		t.Errorf("binary override: got %q", c2.binary)
	}
	// timeout override.
	c3, err := parseConn(map[string]any{"timeout": "5m"})
	if err != nil {
		t.Fatal(err)
	}
	if c3.timeout.String() != "5m0s" {
		t.Errorf("timeout: got %v", c3.timeout)
	}
	// invalid timeout.
	if _, err := parseConn(map[string]any{"timeout": "not-a-duration"}); err == nil {
		t.Fatal("expected error for invalid timeout")
	}
}

// TestDescribe asserts the declared surface: kind, type, capabilities, and
// every verb.
func TestDescribe(t *testing.T) {
	d := kubernetesPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "kubernetes" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Commands, "kubectl") || !d.Capabilities.Spawns {
		t.Fatalf("capabilities: %#v", d.Capabilities)
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Fatalf("expected no declared egress, got %#v", d.Capabilities.Egress)
	}
	want := []string{"apply", "delete", "get", "describe", "logs", "exec", "rollout",
		"scale", "patch", "create", "label", "annotate", "wait", "top",
		"cordon", "uncordon", "drain", "cp", "cli"}
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

// TestInvokeShim drives runKubectl + enrich end to end against a fake kubectl
// binary, proving `get -o json` parsing wires up through Invoke.
func TestInvokeShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "fakekubectl")
	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"*' get '*) echo '{\"kind\":\"Pod\",\"metadata\":{\"name\":\"web-0\"}}' ;;\n" +
		"*' apply '*) cat >/dev/null; echo '{\"kind\":\"Pod\",\"metadata\":{\"name\":\"web-0\"}}' ;;\n" +
		"*) echo \"unhandled $*\" >&2; exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := kubernetesPlugin{}
	conn := map[string]any{"binary": shim, "namespace": "default"}

	// get -o json (default) → parsed result
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "get", Connection: conn,
		Options: map[string]any{"resource": "pod", "name": "web-0"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 {
		t.Fatalf("get exit_code: %#v", res.Outputs)
	}
	obj, ok := res.Outputs["result"].(map[string]any)
	if !ok || obj["kind"] != "Pod" {
		t.Fatalf("get result: %#v", res.Outputs["result"])
	}

	// apply with manifest over stdin, output json → parsed result
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "apply", Connection: conn,
		Options: map[string]any{"manifest": "kind: Pod\n", "output": "json"}})
	if err != nil {
		t.Fatal(err)
	}
	obj, ok = res.Outputs["result"].(map[string]any)
	if !ok || obj["kind"] != "Pod" {
		t.Fatalf("apply result: %#v", res.Outputs["result"])
	}

	// unhandled verb-argv combination surfaces as a non-zero exit_code, not an error.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "cli", Connection: conn,
		Options: map[string]any{"args": []any{"version"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 2 {
		t.Fatalf("cli exit_code: %#v", res.Outputs)
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
