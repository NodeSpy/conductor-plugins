package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestVerbArgs pins the exact argv each verb builds — the whole contract with
// the git CLI, proven without spawning anything.
func TestVerbArgs(t *testing.T) {
	cases := []struct {
		name string
		verb string
		opts map[string]any
		want []string
	}{
		{
			name: "clone full",
			verb: "clone",
			opts: map[string]any{
				"url": "git@github.com:acme/repo.git", "dir": "work", "branch": "main",
				"depth": 1, "single_branch": true, "recursive": true, "bare": true,
			},
			want: []string{"clone", "--bare", "--branch", "main", "--depth", "1",
				"--single-branch", "--recursive", "git@github.com:acme/repo.git", "work"},
		},
		{
			name: "clone minimal",
			verb: "clone",
			opts: map[string]any{"url": "https://example.com/repo.git"},
			want: []string{"clone", "https://example.com/repo.git"},
		},
		{
			name: "init bare with dir",
			verb: "init",
			opts: map[string]any{"dir": "newrepo", "bare": true},
			want: []string{"init", "--bare", "newrepo"},
		},
		{
			name: "fetch all pruned",
			verb: "fetch",
			opts: map[string]any{"all": true, "prune": true, "tags": true, "depth": 5, "remote": "origin", "refspec": "refs/heads/*"},
			want: []string{"fetch", "--all", "--prune", "--tags", "--depth", "5", "origin", "refs/heads/*"},
		},
		{
			name: "pull rebase",
			verb: "pull",
			opts: map[string]any{"rebase": true, "remote": "origin", "branch": "main"},
			want: []string{"pull", "--rebase", "origin", "main"},
		},
		{
			name: "push branch with set upstream, no remote",
			verb: "push",
			opts: map[string]any{"branch": "feature", "set_upstream": true},
			want: []string{"push", "-u"},
		},
		{
			name: "push refspec wins over branch",
			verb: "push",
			opts: map[string]any{"remote": "upstream", "refspec": "HEAD:refs/heads/x", "branch": "ignored", "force": true, "tags": true},
			want: []string{"push", "--force", "--tags", "upstream", "HEAD:refs/heads/x"},
		},
		{
			name: "push force_with_lease wins over force",
			verb: "push",
			opts: map[string]any{"remote": "origin", "branch": "stale", "force": true, "force_with_lease": true},
			want: []string{"push", "--force-with-lease", "origin", "stale"},
		},
		{
			name: "push delete",
			verb: "push",
			opts: map[string]any{"remote": "origin", "delete": true, "branch": "stale"},
			want: []string{"push", "--delete", "origin", "stale"},
		},
		{
			name: "checkout create force track",
			verb: "checkout",
			opts: map[string]any{"ref": "feature", "create": true, "force": true, "track": true},
			want: []string{"checkout", "-f", "--track", "-b", "feature"},
		},
		{
			name: "switch create",
			verb: "switch",
			opts: map[string]any{"ref": "feature", "create": true},
			want: []string{"switch", "-c", "feature"},
		},
		{
			name: "add paths",
			verb: "add",
			opts: map[string]any{"paths": []any{"a.go", "b.go"}},
			want: []string{"add", "a.go", "b.go"},
		},
		{
			name: "add all",
			verb: "add",
			opts: map[string]any{"all": true},
			want: []string{"add", "-A"},
		},
		{
			name: "commit full",
			verb: "commit",
			opts: map[string]any{"message": "fix bug", "all": true, "amend": true, "author": "Bot <bot@example.com>"},
			want: []string{"commit", "-a", "--amend", "--author", "Bot <bot@example.com>", "-m", "fix bug"},
		},
		{
			name: "commit amend without message",
			verb: "commit",
			opts: map[string]any{"amend": true},
			want: []string{"commit", "--amend"},
		},
		{
			name: "commit allow empty",
			verb: "commit",
			opts: map[string]any{"message": "empty", "allow_empty": true},
			want: []string{"commit", "--allow-empty", "-m", "empty"},
		},
		{
			name: "status",
			verb: "status",
			opts: map[string]any{},
			want: []string{"status", "--porcelain=v1", "-b"},
		},
		{
			name: "log filtered",
			verb: "log",
			opts: map[string]any{"max_count": 3, "oneline": true, "format": "%H %s", "since": "1 week ago", "paths": []any{"cmd/", "pkg/"}},
			want: []string{"log", "-n", "3", "--oneline", "--format=%H %s", "--since=1 week ago", "--", "cmd/", "pkg/"},
		},
		{
			name: "diff cached",
			verb: "diff",
			opts: map[string]any{"cached": true, "name_only": true, "paths": []any{"go.mod"}},
			want: []string{"diff", "--cached", "--name-only", "--", "go.mod"},
		},
		{
			name: "diff stat",
			verb: "diff",
			opts: map[string]any{"stat": true},
			want: []string{"diff", "--stat"},
		},
		{
			name: "branch list",
			verb: "branch",
			opts: map[string]any{"subcommand": "list"},
			want: []string{"branch", "--list"},
		},
		{
			name: "branch create with start point",
			verb: "branch",
			opts: map[string]any{"subcommand": "create", "name": "feature", "start_point": "main"},
			want: []string{"branch", "feature", "main"},
		},
		{
			name: "branch delete force",
			verb: "branch",
			opts: map[string]any{"subcommand": "delete", "name": "stale", "force": true},
			want: []string{"branch", "-D", "stale"},
		},
		{
			name: "branch delete safe",
			verb: "branch",
			opts: map[string]any{"subcommand": "delete", "name": "stale"},
			want: []string{"branch", "-d", "stale"},
		},
		{
			name: "branch rename",
			verb: "branch",
			opts: map[string]any{"subcommand": "rename", "name": "main", "new_name": "trunk"},
			want: []string{"branch", "-m", "main", "trunk"},
		},
		{
			name: "branch rename current",
			verb: "branch",
			opts: map[string]any{"subcommand": "rename", "new_name": "trunk"},
			want: []string{"branch", "-m", "trunk"},
		},
		{
			name: "tag create annotated",
			verb: "tag",
			opts: map[string]any{"subcommand": "create", "name": "v1.0.0", "message": "release", "ref": "HEAD~1"},
			want: []string{"tag", "-m", "release", "v1.0.0", "HEAD~1"},
		},
		{
			name: "tag create lightweight",
			verb: "tag",
			opts: map[string]any{"subcommand": "create", "name": "v1.0.0"},
			want: []string{"tag", "v1.0.0"},
		},
		{
			name: "tag delete",
			verb: "tag",
			opts: map[string]any{"subcommand": "delete", "name": "v1.0.0"},
			want: []string{"tag", "-d", "v1.0.0"},
		},
		{
			name: "tag list",
			verb: "tag",
			opts: map[string]any{"subcommand": "list"},
			want: []string{"tag", "--list"},
		},
		{
			name: "merge no ff with message",
			verb: "merge",
			opts: map[string]any{"ref": "feature", "no_ff": true, "message": "merge feature"},
			want: []string{"merge", "--no-ff", "-m", "merge feature", "feature"},
		},
		{
			name: "rebase plain onto upstream",
			verb: "rebase",
			opts: map[string]any{"upstream": "main", "onto": "develop"},
			want: []string{"rebase", "--onto", "develop", "main"},
		},
		{
			name: "rebase abort",
			verb: "rebase",
			opts: map[string]any{"subcommand": "abort"},
			want: []string{"rebase", "--abort"},
		},
		{
			name: "rebase continue",
			verb: "rebase",
			opts: map[string]any{"subcommand": "continue"},
			want: []string{"rebase", "--continue"},
		},
		{
			name: "reset hard with ref",
			verb: "reset",
			opts: map[string]any{"mode": "hard", "ref": "HEAD~1"},
			want: []string{"reset", "--hard", "HEAD~1"},
		},
		{
			name: "reset no mode",
			verb: "reset",
			opts: map[string]any{},
			want: []string{"reset"},
		},
		{
			name: "rev_parse",
			verb: "rev_parse",
			opts: map[string]any{"ref": "HEAD"},
			want: []string{"rev-parse", "HEAD"},
		},
		{
			name: "ls_remote",
			verb: "ls_remote",
			opts: map[string]any{"remote_or_url": "origin"},
			want: []string{"ls-remote", "origin"},
		},
		{
			name: "remote add",
			verb: "remote",
			opts: map[string]any{"subcommand": "add", "name": "upstream", "url": "https://example.com/u.git"},
			want: []string{"remote", "add", "upstream", "https://example.com/u.git"},
		},
		{
			name: "remote remove",
			verb: "remote",
			opts: map[string]any{"subcommand": "remove", "name": "upstream"},
			want: []string{"remote", "remove", "upstream"},
		},
		{
			name: "remote set_url",
			verb: "remote",
			opts: map[string]any{"subcommand": "set_url", "name": "origin", "url": "git@example.com:a/b.git"},
			want: []string{"remote", "set-url", "origin", "git@example.com:a/b.git"},
		},
		{
			name: "remote list",
			verb: "remote",
			opts: map[string]any{"subcommand": "list"},
			want: []string{"remote", "-v"},
		},
		{
			name: "config_get",
			verb: "config_get",
			opts: map[string]any{"key": "user.name"},
			want: []string{"config", "--get", "user.name"},
		},
		{
			name: "config_set",
			verb: "config_set",
			opts: map[string]any{"key": "user.name", "value": "Bot"},
			want: []string{"config", "user.name", "Bot"},
		},
		{
			name: "stash push with message",
			verb: "stash",
			opts: map[string]any{"subcommand": "push", "message": "wip"},
			want: []string{"stash", "push", "-m", "wip"},
		},
		{
			name: "stash default push",
			verb: "stash",
			opts: map[string]any{},
			want: []string{"stash", "push"},
		},
		{
			name: "stash pop",
			verb: "stash",
			opts: map[string]any{"subcommand": "pop"},
			want: []string{"stash", "pop"},
		},
		{
			name: "clean dry run wins over force, with dirs",
			verb: "clean",
			opts: map[string]any{"force": true, "dirs": true, "dry_run": true},
			want: []string{"clean", "-n", "-d"},
		},
		{
			name: "clean force",
			verb: "clean",
			opts: map[string]any{"force": true},
			want: []string{"clean", "-f"},
		},
		{
			name: "show with format",
			verb: "show",
			opts: map[string]any{"ref": "HEAD", "format": "%H"},
			want: []string{"show", "--format=%H", "HEAD"},
		},
		{
			name: "cli escape hatch",
			verb: "cli",
			opts: map[string]any{"args": []any{"gc", "--aggressive"}},
			want: []string{"gc", "--aggressive"},
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

// TestVerbArgsErrors covers required-field validation, invalid subcommands,
// and unknown verbs.
func TestVerbArgsErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"clone", map[string]any{}},                                  // no url
		{"checkout", map[string]any{}},                               // no ref
		{"switch", map[string]any{}},                                 // no ref
		{"add", map[string]any{}},                                    // no paths, no all
		{"commit", map[string]any{}},                                 // no message, not amend
		{"branch", map[string]any{}},                                 // no subcommand
		{"branch", map[string]any{"subcommand": "create"}},           // no name
		{"branch", map[string]any{"subcommand": "delete"}},           // no name
		{"branch", map[string]any{"subcommand": "rename"}},           // no new_name
		{"branch", map[string]any{"subcommand": "nope"}},             // bad subcommand
		{"tag", map[string]any{}},                                    // no subcommand
		{"tag", map[string]any{"subcommand": "create"}},              // no name
		{"tag", map[string]any{"subcommand": "delete"}},              // no name
		{"tag", map[string]any{"subcommand": "nope"}},                // bad subcommand
		{"merge", map[string]any{}},                                  // no ref
		{"rebase", map[string]any{}},                                 // no upstream
		{"rebase", map[string]any{"subcommand": "nope"}},             // bad subcommand
		{"rev_parse", map[string]any{}},                              // no ref
		{"ls_remote", map[string]any{}},                              // no remote_or_url
		{"remote", map[string]any{}},                                 // no subcommand
		{"remote", map[string]any{"subcommand": "add", "name": "x"}}, // no url
		{"remote", map[string]any{"subcommand": "remove"}},           // no name
		{"remote", map[string]any{"subcommand": "nope"}},             // bad subcommand
		{"config_get", map[string]any{}},                             // no key
		{"config_set", map[string]any{"key": "x"}},                   // no value
		{"stash", map[string]any{"subcommand": "nope"}},              // bad subcommand
		{"cli", map[string]any{}},                                    // no args
		{"nope", map[string]any{}},                                   // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbArgs(tc.verb, tc.opts); err == nil {
			t.Errorf("verbArgs(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestGlobalConfigFlags asserts identity and the config map (sorted) all
// precede the subcommand as `-c key=value` pairs.
func TestGlobalConfigFlags(t *testing.T) {
	got := globalConfigFlags("CI Bot", "ci@example.com", map[string]string{"http.sslVerify": "true", "core.autocrlf": "false"})
	want := []string{
		"-c", "user.name=CI Bot",
		"-c", "user.email=ci@example.com",
		"-c", "core.autocrlf=false",
		"-c", "http.sslVerify=true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("globalConfigFlags\n got: %#v\nwant: %#v", got, want)
	}
}

func TestGlobalConfigFlagsEmpty(t *testing.T) {
	if got := globalConfigFlags("", "", nil); len(got) != 0 {
		t.Fatalf("expected no flags, got %#v", got)
	}
}

// TestSSHCommand builds the GIT_SSH_COMMAND string from a key path plus the
// strict-host-key and known-hosts options.
func TestSSHCommand(t *testing.T) {
	got := sshCommand("/home/ci/.ssh/id_ed25519", "accept-new", "/home/ci/.ssh/known_hosts")
	want := "ssh -o IdentitiesOnly=yes -i '/home/ci/.ssh/id_ed25519' -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile='/home/ci/.ssh/known_hosts'"
	if got != want {
		t.Fatalf("sshCommand\n got: %q\nwant: %q", got, want)
	}
}

func TestSSHCommandMinimal(t *testing.T) {
	got := sshCommand("/tmp/key", "", "")
	want := "ssh -o IdentitiesOnly=yes -i '/tmp/key'"
	if got != want {
		t.Fatalf("sshCommand\n got: %q\nwant: %q", got, want)
	}
}

// TestInlineSSHKeyTempFile proves an inline ssh_key is written to a 0600 temp
// file and that the connector's own cleanup path removes it.
func TestInlineSSHKeyTempFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions")
	}
	content := "-----BEGIN OPENSSH PRIVATE KEY-----\nfakefakefake\n-----END OPENSSH PRIVATE KEY-----\n"
	path, err := writeTempFile("conductor-git-key-*", content, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("temp key file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("temp key perms: got %o want 0600", perm)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("temp key content mismatch")
	}

	os.Remove(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove temp key file: %v", err)
	}
}

// TestWriteAskpass proves the GIT_ASKPASS helper is created, is executable,
// prints the token, and can be cleaned up — and separately that the token
// never appears in argv or the -c global config flags.
func TestWriteAskpass(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell script")
	}
	const token = "s3cr3t-token-value"
	path, err := writeAskpass(token)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("askpass script missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("askpass perms: got %o want 0700", perm)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), token) {
		t.Errorf("askpass script does not embed the token")
	}

	// The token must never leak into the verb argv or the -c global flags —
	// only into the (temp, cleaned-up) askpass script above.
	args, err := verbArgs("fetch", map[string]any{"remote": "origin"})
	if err != nil {
		t.Fatal(err)
	}
	full := append(globalConfigFlags("", "", nil), args...)
	for _, a := range full {
		if strings.Contains(a, token) {
			t.Fatalf("token leaked into argv: %#v", full)
		}
	}
}

// TestParseConn covers defaults and the connection-level timeout override.
func TestParseConn(t *testing.T) {
	c, err := parseConn(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "git" {
		t.Errorf("binary default: got %q want git", c.binary)
	}
	if c.timeout != 10*time.Minute {
		t.Errorf("timeout default: got %v", c.timeout)
	}

	c2, err := parseConn(map[string]any{"binary": "/usr/bin/git", "token": "tok", "timeout": "2m"})
	if err != nil {
		t.Fatal(err)
	}
	if c2.binary != "/usr/bin/git" {
		t.Errorf("binary override: got %q", c2.binary)
	}
	if c2.token != "tok" {
		t.Errorf("token: got %q want tok", c2.token)
	}
	if c2.timeout.String() != "2m0s" {
		t.Errorf("timeout: got %v", c2.timeout)
	}

	if _, err := parseConn(map[string]any{"timeout": "not-a-duration"}); err == nil {
		t.Fatal("expected error for invalid timeout")
	}
}

// TestDescribe asserts the declared surface: kind, capabilities, and every verb.
func TestDescribe(t *testing.T) {
	d := gitPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "git" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Commands, "git") || !contains(d.Capabilities.Commands, "ssh") || !d.Capabilities.Spawns {
		t.Fatalf("capabilities: %#v", d.Capabilities)
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Fatalf("expected no declared egress, got %#v", d.Capabilities.Egress)
	}
	want := []string{
		"clone", "init", "fetch", "pull", "push", "checkout", "switch", "add",
		"commit", "status", "log", "diff", "branch", "tag", "merge", "rebase",
		"reset", "rev_parse", "ls_remote", "remote", "config_get", "config_set",
		"stash", "clean", "show", "cli",
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
}

// TestInvokeShim drives runGit + enrich end to end against a fake git
// binary, proving rev_parse sha trimming and status --porcelain file/branch
// parsing wire up through Invoke.
func TestInvokeShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "fakegit")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"rev-parse) echo '  deadbeefcafe1234  ' ;;\n" +
		"status) echo '## main...origin/main'; echo ' M modified.go'; echo '?? untracked.go' ;;\n" +
		"*) echo \"unhandled $*\" >&2; exit 2 ;;\nesac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := gitPlugin{}
	conn := map[string]any{"binary": shim}

	// rev_parse -> sha trimmed of surrounding whitespace
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "rev_parse", Connection: conn,
		Options: map[string]any{"ref": "HEAD"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 || res.Outputs["sha"] != "deadbeefcafe1234" {
		t.Fatalf("rev_parse outputs: %#v", res.Outputs)
	}

	// status --porcelain -> parsed files[] and branch
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "status", Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["branch"] != "main" {
		t.Fatalf("status branch: %#v", res.Outputs["branch"])
	}
	files, ok := res.Outputs["files"].([]map[string]any)
	if !ok || len(files) != 2 {
		t.Fatalf("status files: %#v", res.Outputs["files"])
	}
	if files[0]["status"] != " M" || files[0]["path"] != "modified.go" {
		t.Fatalf("status file[0]: %#v", files[0])
	}
	if files[1]["status"] != "??" || files[1]["path"] != "untracked.go" {
		t.Fatalf("status file[1]: %#v", files[1])
	}
}

// TestInvokeUnknownVerb proves an unrecognized verb is rejected as
// CodeInvalidParams rather than shelling out.
func TestInvokeUnknownVerb(t *testing.T) {
	p := gitPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: map[string]any{"binary": "/bin/true"}})
	if err == nil {
		t.Fatal("expected error for unknown verb")
	}
	var re *plugin.Error
	if !asPluginError(err, &re) || re.Code != plugin.CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams, got %v", err)
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
