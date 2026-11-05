package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// fakeRemoteGit is a git stand-in that fails its first `fails` calls with
// stderr, then succeeds; every call is counted in a file beside it.
func fakeRemoteGit(t *testing.T, fails int, stderr string) (binary string, calls func() int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "calls")
	errf := filepath.Join(dir, "stderr")
	if err := os.WriteFile(errf, []byte(stderr), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho x >> " + shQuote(count) + "\n" +
		"n=$(wc -l < " + shQuote(count) + ")\n" +
		"if [ \"$n\" -le " + itoa(fails) + " ]; then cat " + shQuote(errf) + " >&2; exit 128; fi\necho ok\n"
	binary = filepath.Join(dir, "git")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "\n")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func fastBackoff(t *testing.T) {
	old := netBackoff
	netBackoff = time.Millisecond
	t.Cleanup(func() { netBackoff = old })
}

const (
	connectTimeout = "ssh: connect to host github.com port 22: Connection timed out\r\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n"
	connReset      = "kex_exchange_identification: read: Connection reset by peer\nfatal: Could not read from remote repository.\n"
	authDenied     = "git@github.com: Permission denied (publickey).\r\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n"
	repoNotFound   = "ERROR: Repository not found.\nfatal: Could not read from remote repository.\n"
	httpsAuth      = "remote: Invalid username or token.\nfatal: Authentication failed for 'https://github.com/o/r.git/'\n"
)

// A clone, fetch, pull or push that failed on the network is run again —
// whether a first-class verb or cli made it — and the retry is bounded.
func TestNetworkFailuresAreRetried(t *testing.T) {
	fastBackoff(t)
	for _, c := range []struct {
		name   string
		verb   string
		opts   map[string]any
		stderr string
		fails  int
		calls  int
		exit   int
	}{
		{"clone after a connect timeout", "clone", map[string]any{"url": "git@github.com:o/r.git", "dir": "r"}, connectTimeout, 2, 3, 0},
		{"fetch after a reset", "fetch", map[string]any{"remote": "origin"}, connReset, 1, 2, 0},
		{"push after a connect timeout", "push", map[string]any{"remote": "origin", "refspec": "main"}, connectTimeout, 1, 2, 0},
		{"pull after a connect timeout", "pull", map[string]any{"remote": "origin"}, connectTimeout, 1, 2, 0},
		{"cli fetch with -C", "cli", map[string]any{"args": []any{"-C", "/x", "fetch", "origin"}}, connectTimeout, 1, 2, 0},
		{"gives up after three tries", "fetch", map[string]any{"remote": "origin"}, connectTimeout, 99, 3, 128},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin, calls := fakeRemoteGit(t, c.fails, c.stderr)
			res, err := gitPlugin{}.Invoke(plugin.InvokeRequest{Verb: c.verb, Connection: map[string]any{"binary": bin}, Options: c.opts})
			if err != nil {
				t.Fatal(err)
			}
			if got := calls(); got != c.calls || res.Outputs["exit_code"] != c.exit {
				t.Fatalf("calls = %d, exit_code = %v; want %d calls, exit %d", got, res.Outputs["exit_code"], c.calls, c.exit)
			}
		})
	}
}

// A refusal or a missing repository fails the same way every time, even
// when git also says it "could not read from remote repository"; a local
// command is never retried.
func TestPermanentFailuresAreNotRetried(t *testing.T) {
	fastBackoff(t)
	for _, c := range []struct {
		name, verb, stderr string
		opts               map[string]any
	}{
		{"ssh auth", "fetch", authDenied, map[string]any{"remote": "origin"}},
		{"repository not found", "clone", repoNotFound, map[string]any{"url": "git@github.com:o/r.git"}},
		{"https auth", "push", httpsAuth, map[string]any{"remote": "origin"}},
		{"missing ref", "fetch", "fatal: couldn't find remote ref refs/pull/9/head\n", map[string]any{"remote": "origin", "refspec": "refs/pull/9/head"}},
		{"local command", "status", connectTimeout, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin, calls := fakeRemoteGit(t, 99, c.stderr)
			res, err := gitPlugin{}.Invoke(plugin.InvokeRequest{Verb: c.verb, Connection: map[string]any{"binary": bin}, Options: c.opts})
			if err != nil {
				t.Fatal(err)
			}
			if got := calls(); got != 1 || res.Outputs["exit_code"] != 128 {
				t.Fatalf("calls = %d, exit_code = %v; want one call, exit 128", got, res.Outputs["exit_code"])
			}
		})
	}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), hermeticGitEnv()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func hermeticGitEnv() []string {
	return []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_TERMINAL_PROMPT=0"}
}

// remoteWithPR is a repository whose main moved on after a PR branched off
// it, with the PR's head published as refs/pull/7/head.
func remoteWithPR(t *testing.T) (url, prHead string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	work := t.TempDir()
	gitT(t, work, "init", "-q", "-b", "main")
	for i, f := range []string{"a", "b", "c"} {
		os.WriteFile(filepath.Join(work, f), []byte(f), 0o644)
		gitT(t, work, "add", f)
		gitT(t, work, "commit", "-qm", "base "+itoa(i))
	}
	gitT(t, work, "checkout", "-qb", "pr")
	os.WriteFile(filepath.Join(work, "feature"), []byte("new\n"), 0o644)
	gitT(t, work, "add", "feature")
	gitT(t, work, "commit", "-qm", "the change")
	prHead = gitT(t, work, "rev-parse", "HEAD")
	gitT(t, work, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(work, "later"), []byte("later"), 0o644)
	gitT(t, work, "add", "later")
	gitT(t, work, "commit", "-qm", "main moves on")
	bare := filepath.Join(t.TempDir(), "r.git")
	gitT(t, work, "clone", "-q", "--bare", work, bare)
	gitT(t, bare, "update-ref", "refs/pull/7/head", prHead)
	gitT(t, bare, "branch", "-D", "pr")
	return "file://" + bare, prHead
}

// A temp clone at a ref is a PR tree a reviewer can diff against its base
// — shallow, with no link back to any other clone — and discard removes it.
func TestTempCloneAtARef(t *testing.T) {
	url, prHead := remoteWithPR(t)
	root := t.TempDir()
	conn := map[string]any{"clone_root": root, "env": map[string]any{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1"}}
	res, err := gitPlugin{}.Invoke(plugin.InvokeRequest{Verb: "clone", Connection: conn, Options: map[string]any{
		"url": url, "temp": true, "branch": "main", "single_branch": true, "depth": 2, "ref": "refs/pull/7/head"}})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := res.Outputs["dir"].(string)
	if res.Outputs["exit_code"] != 0 || filepath.Dir(dir) != root {
		t.Fatalf("clone outputs: %#v", res.Outputs)
	}
	if got := gitT(t, dir, "rev-parse", "HEAD"); got != prHead {
		t.Errorf("HEAD = %s, want the PR head %s", got, prHead)
	}
	if got := gitT(t, dir, "diff", "--name-only", "origin/main...HEAD"); got != "feature" {
		t.Errorf("diff origin/main...HEAD = %q, want just the PR's file", got)
	}
	if gitT(t, dir, "rev-parse", "--is-shallow-repository") != "true" {
		t.Error("the clone is not shallow")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Error("the clone borrows objects through alternates")
	}

	for _, bad := range []string{"/etc", filepath.Join(root, "x"), filepath.Join(root, tempClonePrefix+"1", ".."), "relative"} {
		if _, err := (gitPlugin{}).Invoke(plugin.InvokeRequest{Verb: "discard", Connection: conn, Options: map[string]any{"dir": bad}}); err == nil {
			t.Errorf("discard %s: want refused", bad)
		}
	}
	link := filepath.Join(root, tempClonePrefix+"link")
	os.Symlink(dir, link)
	if _, err := (gitPlugin{}).Invoke(plugin.InvokeRequest{Verb: "discard", Connection: conn, Options: map[string]any{"dir": link}}); err == nil {
		t.Error("discard of a symlink: want refused")
	}
	for i := 0; i < 2; i++ { // the second finds it gone, which is fine
		if _, err := (gitPlugin{}).Invoke(plugin.InvokeRequest{Verb: "discard", Connection: conn, Options: map[string]any{"dir": dir}}); err != nil {
			t.Fatalf("discard #%d: %v", i+1, err)
		}
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("discard left %s", dir)
	}
}

// A temp clone that fails — here at its ref — leaves nothing behind.
func TestFailedTempCloneIsRemoved(t *testing.T) {
	url, _ := remoteWithPR(t)
	root := t.TempDir()
	conn := map[string]any{"clone_root": root, "env": map[string]any{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1"}}
	res, err := gitPlugin{}.Invoke(plugin.InvokeRequest{Verb: "clone", Connection: conn, Options: map[string]any{
		"url": url, "temp": true, "depth": 5, "ref": "refs/pull/99/head"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] == 0 || res.Outputs["dir"] != nil {
		t.Fatalf("clone of a missing ref: %#v", res.Outputs)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Errorf("a failed temp clone left %v", ents)
	}
}
