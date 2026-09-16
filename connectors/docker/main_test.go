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
// the engine CLI, proven without spawning anything.
func TestVerbArgs(t *testing.T) {
	cases := []struct {
		name string
		verb string
		opts map[string]any
		want []string
	}{
		{
			name: "run full",
			verb: "run",
			opts: map[string]any{
				"image": "alpine:3", "cmd": []any{"echo", "hi"},
				"env":     map[string]any{"B": "2", "A": "1"}, // keys sort A,B
				"volumes": []any{"/src:/dst"}, "ports": []any{"8080:80"},
				"workdir": "/w", "name": "job", "detach": true, "remove": true,
				"network": "host", "entrypoint": "/bin/sh", "user": "1000",
				"platform": "linux/arm64", "pull": "always", "extra_args": []any{"--cap-add", "NET_ADMIN"},
			},
			want: []string{"run", "--rm", "-d", "--name", "job",
				"-e", "A=1", "-e", "B=2", "-v", "/src:/dst", "-p", "8080:80",
				"-w", "/w", "--user", "1000", "--network", "host",
				"--entrypoint", "/bin/sh", "--platform", "linux/arm64", "--pull", "always",
				"--cap-add", "NET_ADMIN", "alpine:3", "echo", "hi"},
		},
		{
			name: "run minimal",
			verb: "run",
			opts: map[string]any{"image": "hello-world", "remove": true},
			want: []string{"run", "--rm", "hello-world"},
		},
		{
			name: "exec",
			verb: "exec",
			opts: map[string]any{"container": "web", "cmd": []any{"ls", "-la"}, "workdir": "/app", "user": "root"},
			want: []string{"exec", "-w", "/app", "--user", "root", "web", "ls", "-la"},
		},
		{
			name: "build",
			verb: "build",
			opts: map[string]any{"tag": "img:1", "dockerfile": "Dockerfile.prod",
				"build_args": map[string]any{"VER": "9"}, "target": "final", "no_cache": true, "context": "./svc"},
			want: []string{"build", "-f", "Dockerfile.prod", "-t", "img:1", "--build-arg", "VER=9", "--target", "final", "--no-cache", "./svc"},
		},
		{
			name: "build default context",
			verb: "build",
			opts: map[string]any{"tag": "img:1"},
			want: []string{"build", "-t", "img:1", "."},
		},
		{
			name: "pull with platform",
			verb: "pull",
			opts: map[string]any{"image": "nginx", "platform": "linux/amd64"},
			want: []string{"pull", "--platform", "linux/amd64", "nginx"},
		},
		{
			name: "push",
			verb: "push",
			opts: map[string]any{"image": "reg/img:1"},
			want: []string{"push", "reg/img:1"},
		},
		{
			name: "ps",
			verb: "ps",
			opts: map[string]any{"all": true, "filter": []any{"status=running"}, "limit": 5},
			want: []string{"ps", "-a", "--filter", "status=running", "-n", "5", "--format", "{{json .}}"},
		},
		{
			name: "images",
			verb: "images",
			opts: map[string]any{"filter": []any{"dangling=true"}},
			want: []string{"images", "--filter", "dangling=true", "--format", "{{json .}}"},
		},
		{
			name: "logs",
			verb: "logs",
			opts: map[string]any{"container": "web", "tail": "100", "since": "1h", "timestamps": true},
			want: []string{"logs", "--tail", "100", "--since", "1h", "-t", "web"},
		},
		{
			name: "stop many",
			verb: "stop",
			opts: map[string]any{"container": []any{"a", "b"}, "timeout": 3},
			want: []string{"stop", "-t", "3", "a", "b"},
		},
		{
			name: "start",
			verb: "start",
			opts: map[string]any{"container": "web"},
			want: []string{"start", "web"},
		},
		{
			name: "rm force",
			verb: "rm",
			opts: map[string]any{"container": []any{"a", "b"}, "force": true, "volumes": true},
			want: []string{"rm", "-f", "-v", "a", "b"},
		},
		{
			name: "inspect typed",
			verb: "inspect",
			opts: map[string]any{"target": []any{"web", "db"}, "type": "container"},
			want: []string{"inspect", "--type", "container", "web", "db"},
		},
		{
			name: "compose globals precede subcommand",
			verb: "compose",
			opts: map[string]any{"subcommand": "up", "files": []any{"a.yml", "b.yml"}, "project": "proj",
				"profiles": []any{"gpu"}, "detach": true, "build": true, "remove_orphans": true, "services": []any{"web"}},
			want: []string{"compose", "-f", "a.yml", "-f", "b.yml", "-p", "proj", "--profile", "gpu",
				"up", "-d", "--build", "--remove-orphans", "web"},
		},
		{
			name: "compose down volumes",
			verb: "compose",
			opts: map[string]any{"subcommand": "down", "volumes": true, "remove_orphans": true},
			want: []string{"compose", "down", "--remove-orphans", "-v"},
		},
		{
			name: "compose ps json",
			verb: "compose",
			opts: map[string]any{"subcommand": "ps", "json": true},
			want: []string{"compose", "ps", "--format", "json"},
		},
		{
			name: "buildx multi-platform push",
			verb: "buildx",
			opts: map[string]any{"tag": "img:1", "platform": []any{"linux/amd64", "linux/arm64"},
				"push": true, "builder": "ci", "cache_from": []any{"type=gha"}, "context": "."},
			want: []string{"buildx", "build", "--builder", "ci", "-t", "img:1",
				"--platform", "linux/amd64", "--platform", "linux/arm64", "--push", "--cache-from", "type=gha", "."},
		},
		{
			name: "bake set map and targets",
			verb: "bake",
			opts: map[string]any{"files": []any{"docker-bake.hcl"}, "targets": []any{"api", "web"},
				"set": map[string]any{"api.tags": "img:1"}, "push": true, "print": true},
			want: []string{"buildx", "bake", "-f", "docker-bake.hcl", "--set", "api.tags=img:1", "--push", "--print", "api", "web"},
		},
		{
			name: "bake set list",
			verb: "bake",
			opts: map[string]any{"set": []any{"api.tags=img:1", "*.platform=linux/arm64"}},
			want: []string{"buildx", "bake", "--set", "api.tags=img:1", "--set", "*.platform=linux/arm64"},
		},
		{
			name: "cli escape hatch",
			verb: "cli",
			opts: map[string]any{"args": []any{"system", "df", "-v"}},
			want: []string{"system", "df", "-v"},
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
		{"run", map[string]any{}},                    // no image
		{"exec", map[string]any{"container": "x"}},   // no cmd
		{"exec", map[string]any{"cmd": []any{"ls"}}}, // no container
		{"pull", map[string]any{}},                   // no image
		{"push", map[string]any{}},                   // no image
		{"logs", map[string]any{}},                   // no container
		{"stop", map[string]any{}},                   // no container
		{"start", map[string]any{}},                  // no container
		{"rm", map[string]any{}},                     // no container
		{"inspect", map[string]any{}},                // no target
		{"compose", map[string]any{}},                // no subcommand
		{"cli", map[string]any{}},                    // no args
		{"nope", map[string]any{}},                   // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbArgs(tc.verb, tc.opts); err == nil {
			t.Errorf("verbArgs(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers engine validation, the --context connection flag, and
// process-env derivation.
func TestParseConn(t *testing.T) {
	if _, err := parseConn(map[string]any{"engine": "lxc"}); err == nil {
		t.Fatal("expected error for unknown engine")
	}
	c, err := parseConn(map[string]any{
		"engine": "podman", "context": "remote",
		"docker_host": "ssh://ci@box", "tls_verify": true, "cert_path": "/certs",
		"env": map[string]any{"HTTP_PROXY": "http://p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "podman" {
		t.Errorf("binary: got %q want podman", c.binary)
	}
	if got := c.connFlags(); !reflect.DeepEqual(got, []string{"--context", "remote"}) {
		t.Errorf("connFlags: %#v", got)
	}
	env := c.procEnv()
	for _, want := range []string{"HTTP_PROXY=http://p", "DOCKER_HOST=ssh://ci@box", "DOCKER_TLS_VERIFY=1", "DOCKER_CERT_PATH=/certs"} {
		if !contains(env, want) {
			t.Errorf("procEnv missing %q", want)
		}
	}
	// binary override wins over engine.
	c2, _ := parseConn(map[string]any{"engine": "docker", "binary": "/usr/local/bin/docker"})
	if c2.binary != "/usr/local/bin/docker" {
		t.Errorf("binary override: got %q", c2.binary)
	}
}

// TestBuildxBakeEngineGuard proves buildx/bake refuse podman BEFORE spawning.
func TestBuildxBakeEngineGuard(t *testing.T) {
	p := dockerPlugin{}
	for _, verb := range []string{"buildx", "bake"} {
		_, err := p.Invoke(plugin.InvokeRequest{
			Verb:       verb,
			Connection: map[string]any{"engine": "podman"},
			Options:    map[string]any{"tag": "img:1"},
		})
		if err == nil {
			t.Fatalf("%s on podman: expected error", verb)
		}
		var re *plugin.Error
		if !asPluginError(err, &re) || re.Code != plugin.CodeInvalidParams {
			t.Fatalf("%s on podman: want CodeInvalidParams, got %v", verb, err)
		}
	}
}

// TestDescribe asserts the declared surface: kind, capabilities, and every verb.
func TestDescribe(t *testing.T) {
	d := dockerPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "docker" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Commands, "docker") || !contains(d.Capabilities.Commands, "podman") || !d.Capabilities.Spawns {
		t.Fatalf("capabilities: %#v", d.Capabilities)
	}
	want := []string{"run", "exec", "build", "pull", "push", "ps", "images", "logs",
		"stop", "start", "rm", "inspect", "compose", "buildx", "bake", "cli"}
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

// TestInvokeShim drives runDocker + enrich end to end against a fake engine
// binary, proving container_id extraction and ps json parsing wire up.
func TestInvokeShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "fakedocker")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"run) echo deadbeefcafe ;;\n" +
		"ps) echo '{\"Names\":\"web\",\"State\":\"running\"}' ;;\n" +
		"*) echo \"unhandled $*\" >&2; exit 2 ;;\nesac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := dockerPlugin{}
	conn := map[string]any{"binary": shim}

	// run --detach → container_id from stdout
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "run", Connection: conn,
		Options: map[string]any{"image": "x", "detach": true}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 || res.Outputs["container_id"] != "deadbeefcafe" {
		t.Fatalf("run outputs: %#v", res.Outputs)
	}

	// ps → parsed containers[]
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "ps", Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	list, ok := res.Outputs["containers"].([]map[string]any)
	if !ok || len(list) != 1 || list[0]["Names"] != "web" {
		t.Fatalf("ps containers: %#v", res.Outputs["containers"])
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
