package githubkit

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const cloneToken = "ghs_cloneTokenNeverOnDisk123"

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// prRemote is <root>/acme/w.git: main has `before` commits, the PR branches
// off there with one commit (published as refs/pull/7/head), and main moves
// on by `after` more — so the PR's own depth misses the merge-base.
func prRemote(t *testing.T, before, after int) (root, prHead, fork string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	work := t.TempDir()
	gitIn(t, work, "init", "-q", "-b", "main")
	for i := 0; i < before; i++ {
		gitIn(t, work, "commit", "-q", "--allow-empty", "-m", "base "+strconv.Itoa(i))
	}
	fork = gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "checkout", "-qb", "pr")
	os.WriteFile(filepath.Join(work, "feature"), []byte("new\n"), 0o644)
	gitIn(t, work, "add", "feature")
	gitIn(t, work, "commit", "-qm", "the change")
	prHead = gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "checkout", "-q", "main")
	for i := 0; i < after; i++ {
		gitIn(t, work, "commit", "-q", "--allow-empty", "-m", "later "+strconv.Itoa(i))
	}
	root = t.TempDir()
	bare := filepath.Join(root, "acme", "w.git")
	gitIn(t, work, "clone", "-q", "--bare", work, bare)
	gitIn(t, bare, "update-ref", "refs/pull/7/head", prHead)
	gitIn(t, bare, "branch", "-D", "pr")
	return root, prHead, fork
}

// cloneClient answers PR 7's metadata and clones from root, with a literal
// read credential.
func cloneClient(t *testing.T, root string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/w/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"base":{"ref":"main"},"commits":1}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	kit, err := NewClient(Config{WriteToken: "w", ReadToken: cloneToken, APIBase: ts.URL, GitBase: "file://" + root})
	if err != nil {
		t.Fatal(err)
	}
	return kit
}

// recordingGit stands in for git: it logs every argv, fails the first
// `fails` fetches with stderr, and runs the real git otherwise.
func recordingGit(t *testing.T, fails int, stderr string) (log func() string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	logf, errf := filepath.Join(dir, "argv"), filepath.Join(dir, "stderr")
	os.WriteFile(errf, []byte(stderr), 0o644)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logf + "'\n" +
		"case \" $* \" in *\" fetch \"*) n=$(grep -c ' fetch ' '" + logf + "'); " +
		"if [ \"$n\" -le " + strconv.Itoa(fails) + " ]; then cat '" + errf + "' >&2; exit 128; fi;; esac\n" +
		"exec '" + real + "' \"$@\"\n"
	bin := filepath.Join(dir, "git")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old, oldBackoff := gitBinary, prCloneBackoff
	gitBinary, prCloneBackoff = bin, time.Millisecond
	t.Cleanup(func() { gitBinary, prCloneBackoff = old, oldBackoff })
	return func() string { b, _ := os.ReadFile(logf); return string(b) }
}

func privateTmp(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("TMPDIR", d)
	return d
}

// A PR whose base moved on past the PR's own depth: the clone deepens until
// the merge-base is in it, and stays shallow — enough for base...HEAD, not
// the whole history.
func TestPRCloneDeepensToTheMergeBase(t *testing.T) {
	tmp := privateTmp(t)
	root, prHead, fork := prRemote(t, 100, 20)
	argv := recordingGit(t, 0, "")
	out, err := cloneClient(t, root).Invoke(context.Background(), "pr_clone", map[string]any{"repo": "acme/w", "pr": 7})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := out["dir"].(string)
	if filepath.Dir(dir) != tmp || !strings.HasPrefix(filepath.Base(dir), prCloneDirPrefix) {
		t.Fatalf("dir = %q, want a pr_clone dir in %s", dir, tmp)
	}
	if out["head_sha"] != prHead || out["merge_base"] != fork || out["base"] != "main" {
		t.Fatalf("outputs %#v; want head %s, merge_base %s", out, prHead, fork)
	}
	if got := gitIn(t, dir, "rev-parse", "HEAD"); got != prHead {
		t.Errorf("HEAD = %s, want the PR head", got)
	}
	if got := gitIn(t, dir, "diff", "--name-only", "origin/main...HEAD"); got != "feature" {
		t.Errorf("diff origin/main...HEAD = %q, want just the PR's file", got)
	}
	if gitIn(t, dir, "rev-parse", "--is-shallow-repository") != "true" {
		t.Error("the clone fetched the whole history")
	}
	if n, _ := strconv.Atoi(gitIn(t, dir, "rev-list", "--count", "origin/main")); n >= 120 {
		t.Errorf("origin/main has %d commits: the whole history", n)
	}
	if !strings.Contains(argv(), "--deepen=") {
		t.Error("the merge-base was found without deepening; the test does not cover it")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Error("the clone borrows objects through alternates")
	}
}

// The credential never reaches argv or disk: it rides git's environment as
// an Authorization header scoped to the clone host, and nothing in the
// clone holds it.
func TestPRCloneKeepsTheTokenOutOfArgvAndDisk(t *testing.T) {
	privateTmp(t)
	root, _, _ := prRemote(t, 3, 1)
	argv := recordingGit(t, 0, "")
	out, err := cloneClient(t, root).Invoke(context.Background(), "pr_clone", map[string]any{"repo": "acme/w", "pr": 7})
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + cloneToken))
	if a := argv(); strings.Contains(a, cloneToken) || strings.Contains(a, b64) {
		t.Fatalf("the token is in git's argv:\n%s", a)
	}
	filepath.Walk(out["dir"].(string), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), cloneToken) || strings.Contains(string(b), b64) {
				t.Errorf("the token is on disk in %s", p)
			}
		}
		return nil
	})
}

func TestPRCloneGitEnvironment(t *testing.T) {
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GIT_SSH_COMMAND", "ssh -i key")
	env := strings.Join(gitEnv("https://github.com", "tok"), "\n")
	for _, want := range []string{
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"=protocol.allow\n", "=never\n", "=protocol.https.allow\n",
		"=http.https://github.com/.extraHeader\n",
		"=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok")),
	} {
		if !strings.Contains(env+"\n", want) {
			t.Errorf("git's environment lacks %q:\n%s", want, env)
		}
	}
	for _, bad := range []string{"GIT_DIR=", "GIT_SSH_COMMAND="} {
		if strings.Contains(env, bad) {
			t.Errorf("git's environment inherits %s", bad)
		}
	}
}

// A fetch that failed on the network is retried; one the remote refused
// is not, and a failed clone leaves nothing behind or in its error.
func TestPRCloneRetriesOnlyNetworkFailures(t *testing.T) {
	for _, c := range []struct {
		name    string
		fails   int
		stderr  string
		fetches int
		ok      bool
	}{
		{"connect timeout", 2, "fatal: unable to access 'https://github.com/acme/w.git/': Failed to connect to github.com port 443: Connection timed out\n", 3, true},
		{"reset", 1, "error: RPC failed; curl 56 Recv failure: Connection reset by peer\nfatal: the remote end hung up unexpectedly\n", 2, true},
		{"gives up", 99, "fatal: unable to access 'https://github.com/acme/w.git/': Failed to connect to github.com port 443: Connection timed out\n", 3, false},
		{"auth", 99, "remote: Invalid username or token " + cloneToken + "\nfatal: Authentication failed for 'https://github.com/acme/w.git/'\n", 1, false},
		{"not found", 99, "remote: Repository not found.\nfatal: repository 'https://github.com/acme/w.git/' not found\n", 1, false},
		// a refusal that also reads like a dropped connection is still a refusal
		{"auth, then hung up", 99, "fatal: Authentication failed for 'https://github.com/acme/w.git/'\nfatal: the remote end hung up unexpectedly\n", 1, false},
		{"not found, could not read", 99, "ERROR: Repository not found.\nfatal: Could not read from remote repository.\n", 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := privateTmp(t)
			root, _, _ := prRemote(t, 3, 1)
			argv := recordingGit(t, c.fails, c.stderr)
			out, err := cloneClient(t, root).Invoke(context.Background(), "pr_clone", map[string]any{"repo": "acme/w", "pr": 7})
			if got := strings.Count(argv(), " fetch "); got != c.fetches {
				t.Errorf("%d fetches, want %d", got, c.fetches)
			}
			if c.ok {
				if err != nil {
					t.Fatalf("clone: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("clone succeeded: %#v", out)
			}
			if strings.Contains(err.Error(), cloneToken) {
				t.Errorf("the error carries the token: %v", err)
			}
			if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
				t.Errorf("a failed clone left %v", ents)
			}
		})
	}
}

// A PR the remote doesn't serve fails at once and leaves nothing.
func TestPRCloneOfAMissingPRLeavesNothing(t *testing.T) {
	tmp := privateTmp(t)
	root, _, _ := prRemote(t, 3, 1)
	gitIn(t, filepath.Join(root, "acme", "w.git"), "update-ref", "-d", "refs/pull/7/head")
	argv := recordingGit(t, 0, "")
	if _, err := cloneClient(t, root).Invoke(context.Background(), "pr_clone", map[string]any{"repo": "acme/w", "pr": 7}); err == nil {
		t.Fatal("clone of a missing PR ref succeeded")
	}
	if got := strings.Count(argv(), " fetch "); got != 1 {
		t.Errorf("%d fetches of a missing ref, want 1", got)
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Errorf("a failed clone left %v", ents)
	}
}

func TestPRCloneDiscard(t *testing.T) {
	tmp := privateTmp(t)
	kit, _ := NewClient(Config{WriteToken: "w"})
	dir, err := os.MkdirTemp("", prCloneDirPrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(tmp, "other")
	os.Mkdir(other, 0o755)
	link := filepath.Join(tmp, prCloneDirPrefix+"link")
	os.Symlink(other, link)
	for _, bad := range []string{"/etc", other, link, filepath.Join(dir, "x"), dir + "/../" + filepath.Base(dir), "relative", ""} {
		if _, err := kit.Invoke(context.Background(), "pr_clone_discard", map[string]any{"dir": bad}); err == nil {
			t.Errorf("discard %q: want refused", bad)
		}
	}
	for i := 0; i < 2; i++ { // the second finds it gone, which is fine
		if _, err := kit.Invoke(context.Background(), "pr_clone_discard", map[string]any{"dir": dir}); err != nil {
			t.Fatalf("discard #%d: %v", i+1, err)
		}
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("discard left %s", dir)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("discard reached through a symlink: %v", err)
	}
}

func TestPRCloneBase(t *testing.T) {
	for api, want := range map[string]string{
		"https://api.github.com":         "https://github.com",
		"https://api.acme.ghe.com":       "https://acme.ghe.com",
		"https://ghe.example.com/api/v3": "https://ghe.example.com",
		"http://api.github.com":          "",
	} {
		kit, _ := NewClient(Config{WriteToken: "w", APIBase: api})
		got, err := kit.cloneBase()
		if got != want || (want == "") != (err != nil) {
			t.Errorf("cloneBase(%s) = %q, %v; want %q", api, got, err, want)
		}
	}
}
