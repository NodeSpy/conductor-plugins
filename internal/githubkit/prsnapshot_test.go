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

// snapshot runs pr_snapshot of acme/w#7 with a fresh staging dir, which it
// returns too.
func snapshot(t *testing.T, root string) (map[string]any, string, error) {
	t.Helper()
	staging := t.TempDir()
	out, err := cloneClient(t, root).PRSnapshot(context.Background(), map[string]any{"repo": "acme/w", "pr": 7}, staging)
	return out, staging, err
}

// unpack extracts a snapshot archive into a fresh dir and returns it.
func unpack(t *testing.T, archive string) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("tar", "-C", dir, "-xf", archive).CombinedOutput(); err != nil {
		t.Fatalf("untar: %v\n%s", err, out)
	}
	return dir
}

func empty(t *testing.T, what, dir string) {
	t.Helper()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("%s holds %v", what, ents)
	}
}

// A PR whose base moved on past the PR's own depth: the clone deepens until
// the merge-base is in it, and stays shallow — enough for base...HEAD, not
// the whole history. The archive of it, .git included, lands in the staging
// dir; the clone itself is gone when the call returns.
func TestPRSnapshotDeepensToTheMergeBase(t *testing.T) {
	tmp := privateTmp(t)
	root, prHead, fork := prRemote(t, 100, 20)
	argv := recordingGit(t, 0, "")
	out, staging, err := snapshot(t, root)
	if err != nil {
		t.Fatal(err)
	}
	empty(t, "TMPDIR after the call", tmp)
	archive, _ := out["archive"].(string)
	// The clone was made in the staging dir (on disk, owned by the host) and
	// is gone: only the archive is left there.
	if !strings.Contains(argv(), filepath.Join(staging, prCloneDirPrefix)) {
		t.Errorf("git never ran in a clone dir inside the staging dir %s", staging)
	}
	if ents, _ := os.ReadDir(staging); len(ents) != 1 {
		t.Errorf("staging holds %v, want only the archive", ents)
	}
	if filepath.Dir(archive) != staging || !strings.HasPrefix(filepath.Base(archive), "pr-7-") || !strings.HasSuffix(archive, ".tar") {
		t.Fatalf("archive = %q, want a pr-7-*.tar in the staging dir %s", archive, staging)
	}
	if out["archive_media_type"] != SnapshotMediaType {
		t.Errorf("archive_media_type = %v", out["archive_media_type"])
	}
	dir := unpack(t, archive)
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
	if st := gitIn(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("the unpacked tree is not clean: %q", st)
	}
}

// The credential never reaches argv or disk: it rides git's environment as
// an Authorization header scoped to the clone host, and nothing in the
// clone holds it.
func TestPRSnapshotKeepsTheTokenOutOfArgvAndDisk(t *testing.T) {
	privateTmp(t)
	root, _, _ := prRemote(t, 3, 1)
	argv := recordingGit(t, 0, "")
	out, _, err := snapshot(t, root)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + cloneToken))
	if a := argv(); strings.Contains(a, cloneToken) || strings.Contains(a, b64) {
		t.Fatalf("the token is in git's argv:\n%s", a)
	}
	if b, _ := os.ReadFile(out["archive"].(string)); strings.Contains(string(b), cloneToken) || strings.Contains(string(b), b64) {
		t.Error("the token is in the archive")
	}
	filepath.Walk(unpack(t, out["archive"].(string)), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), cloneToken) || strings.Contains(string(b), b64) {
				t.Errorf("the token is on disk in %s", p)
			}
		}
		return nil
	})
}

func TestPRSnapshotGitEnvironment(t *testing.T) {
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
func TestPRSnapshotRetriesOnlyNetworkFailures(t *testing.T) {
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
			out, staging, err := snapshot(t, root)
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
			empty(t, "TMPDIR after a failed clone", tmp)
			empty(t, "staging after a failed clone", staging)
		})
	}
}

// A PR the remote doesn't serve fails at once and leaves nothing.
func TestPRSnapshotOfAMissingPRLeavesNothing(t *testing.T) {
	tmp := privateTmp(t)
	root, _, _ := prRemote(t, 3, 1)
	gitIn(t, filepath.Join(root, "acme", "w.git"), "update-ref", "-d", "refs/pull/7/head")
	argv := recordingGit(t, 0, "")
	_, staging, err := snapshot(t, root)
	if err == nil {
		t.Fatal("clone of a missing PR ref succeeded")
	}
	if got := strings.Count(argv(), " fetch "); got != 1 {
		t.Errorf("%d fetches of a missing ref, want 1", got)
	}
	empty(t, "TMPDIR after a failed clone", tmp)
	empty(t, "staging after a failed clone", staging)
}

// Without a staging dir there is nowhere to put the archive: the call fails
// before it fetches anything.
func TestPRSnapshotNeedsAStagingDir(t *testing.T) {
	tmp := privateTmp(t)
	root, _, _ := prRemote(t, 3, 1)
	argv := recordingGit(t, 0, "")
	if _, err := cloneClient(t, root).PRSnapshot(context.Background(), map[string]any{"repo": "acme/w", "pr": 7}, ""); err == nil {
		t.Fatal("pr_snapshot without a staging dir succeeded")
	}
	if a := argv(); a != "" {
		t.Errorf("git ran: %s", a)
	}
	empty(t, "TMPDIR", tmp)
}

// The archive holds the tree's dirs, files and in-tree symlinks; a symlink
// out of the tree is left out, since it would fail the unpack.
func TestTarTreeLeavesOutEscapingLinks(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "pkg"), 0o755)
	os.WriteFile(filepath.Join(src, "pkg", "a.go"), []byte("package pkg\n"), 0o644)
	for link, target := range map[string]string{"pkg/in": "a.go", "up": "pkg/../pkg/a.go", "abs": "/etc/passwd", "pkg/out": "../../x"} {
		if err := os.Symlink(target, filepath.Join(src, link)); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "t.tar")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := tarTree(f, src); err != nil {
		t.Fatal(err)
	}
	f.Close()
	dir := unpack(t, archive)
	if b, err := os.ReadFile(filepath.Join(dir, "pkg", "in")); err != nil || string(b) != "package pkg\n" {
		t.Errorf("pkg/in: %q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "up")); err != nil {
		t.Errorf("the in-tree link up is missing: %v", err)
	}
	for _, gone := range []string{"abs", "pkg/out"} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("the escaping link %s is in the archive", gone)
		}
	}
}

func TestPRSnapshotCloneBase(t *testing.T) {
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

// A pr_snapshot whose call is cancelled stops: git and anything git started
// (git-remote-https, index-pack) are killed, and the call returns at once —
// even when a helper of git's holds git's stderr open, as git-remote-https
// does. Left running, an abandoned clone holds the plugin's bandwidth and the
// staging dir long after the daemon gave up on it.
func TestPRSnapshotStopsWhenTheCallIsCancelled(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep")
	}
	dir := t.TempDir()
	started, helperPid := filepath.Join(dir, "started"), filepath.Join(dir, "helper.pid")
	// init/remote/check-ref-format succeed; fetch starts a helper that keeps
	// stderr open (like git-remote-https), then blocks like a stalled remote.
	script := "#!/bin/sh\ncase \" $* \" in *\" fetch \"*) " +
		"sleep 120 & echo $! > '" + helperPid + "'; : > '" + started + "'; sleep 120;; esac\nexit 0\n"
	bin := filepath.Join(dir, "git")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := gitBinary
	gitBinary = bin
	t.Cleanup(func() { gitBinary = old })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cloneClient(t, t.TempDir()).PRSnapshot(ctx, map[string]any{"repo": "acme/w", "pr": 7}, t.TempDir())
		done <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fetch never started")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled snapshot must fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pr_snapshot kept running 10s after its call was cancelled")
	}
	b, _ := os.ReadFile(helperPid)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("helper pid: %v", err)
	}
	// The helper is git's child, reaped by init once killed; give it a moment.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err != nil {
			break
		}
		if s, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")); strings.Contains(string(s), ") Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a helper git started outlived the cancelled call")
		}
	}
}
