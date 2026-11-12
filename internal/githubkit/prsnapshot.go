package githubkit

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/exposurekit"
)

// pr_snapshot makes a pull request's tree for a step that hands it to others
// (a panel of reviewers sharing one fetch): a tar archive of a shallow clone
// of the PR head and its base branch, .git included, deep enough that the
// base...HEAD merge-base is in it, with HEAD detached at the PR head. The
// archive is a binary output: written to the call's staging directory, it
// is stored by conductor as a run-scoped blob. The clone is made in a temp
// directory and removed before the call returns.
//
// It runs git over HTTPS only, with the connector's read credential. The
// token travels as an http.<host>.extraHeader in git's environment
// (GIT_CONFIG_COUNT), so it is never in argv and never written to disk; the
// user's and system's git config are ignored (no credential helper, no
// insteadOf that could turn the URL into ssh). A fetch that fails on the
// network is retried; one the remote refused is not.

// gitBinary is the git pr_snapshot runs.
var gitBinary = "git"

const (
	prCloneDirPrefix = "clone-"
	// SnapshotMediaType is the archive's media type.
	SnapshotMediaType = "application/x-tar"
	prCloneAttempts   = 3
	// prCloneDeepenRounds bounds the deepening: the depth doubles each
	// round, and the last round fetches the whole history.
	prCloneDeepenRounds = 12
)

var prCloneBackoff = 2 * time.Second

// gitWaitDelay is how long a git cut off by its ctx gets to let go of its
// output pipes before the call stops waiting for them.
const gitWaitDelay = 5 * time.Second

// prCloneTimeout bounds the whole clone, retries and deepening included.
const prCloneTimeout = 20 * time.Minute

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// cloneBase is where repositories are cloned from: Config.GitBase, else the
// API base's web host — https://github.com for the public API,
// https://<sub>.ghe.com for api.<sub>.ghe.com, https://<host> for an
// Enterprise Server's https://<host>/api/v3.
func (c *Client) cloneBase() (string, error) {
	if c.gitBase != "" {
		return strings.TrimRight(c.gitBase, "/"), nil
	}
	u, err := url.Parse(c.base())
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("no https host to clone from in the API base %q", c.base())
	}
	host := u.Host
	if strings.TrimRight(u.Path, "/") != "/api/v3" {
		host = strings.TrimPrefix(host, "api.")
	}
	return "https://" + host, nil
}

// PRSnapshot runs pr_snapshot: it clones the PR into a temp directory,
// archives the clone into a new file under staging (the call's staging
// directory), removes the clone, and returns the archive's path as the
// binary output "archive".
func (c *Client) PRSnapshot(ctx context.Context, opts map[string]any, staging string) (map[string]any, error) {
	repo, _ := opts["repo"].(string)
	number := toInt(opts["pr"])
	if !repoName.MatchString(repo) || strings.Contains(repo, "..") {
		return nil, fmt.Errorf("github.pr_snapshot: options.repo must be owner/name, got %q", repo)
	}
	if number <= 0 {
		return nil, fmt.Errorf("github.pr_snapshot: options.pr is required")
	}
	if staging == "" {
		return nil, fmt.Errorf("github.pr_snapshot: the host gave this call no staging directory to write the archive to")
	}
	gitBase, err := c.cloneBase()
	if err != nil {
		return nil, fmt.Errorf("github.pr_snapshot: %w", err)
	}
	tok, err := c.MintCredential(ctx, "read", repo)
	if err != nil {
		return nil, fmt.Errorf("github.pr_snapshot: %w", err)
	}
	var pr struct {
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Commits int `json:"commits"`
	}
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", c.base(), repo, number), &pr); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, prCloneTimeout)
	defer cancel()
	// The clone goes in the call's staging dir, on disk, never os.TempDir()
	// (often a RAM-backed tmpfs). The host owns that dir and removes it after
	// the call whatever happens, so a clone cut off by the call deadline is
	// cleaned up too.
	dir, err := os.MkdirTemp(staging, prCloneDirPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("github.pr_snapshot: %w", err)
	}
	defer os.RemoveAll(dir)
	g := &prGit{dir: dir, env: gitEnv(gitBase, tok), secret: tok}
	out, err := g.clone(ctx, gitBase+"/"+repo+".git", pr.Base.Ref, number, pr.Commits)
	if err != nil {
		return nil, fmt.Errorf("github.pr_snapshot %s#%d: %w", repo, number, err)
	}
	f, err := os.CreateTemp(staging, fmt.Sprintf("pr-%d-*.tar", number))
	if err != nil {
		return nil, fmt.Errorf("github.pr_snapshot: %w", err)
	}
	err = tarTree(f, dir)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return nil, fmt.Errorf("github.pr_snapshot: archive: %w", err)
	}
	out["archive"] = f.Name()
	out["archive_media_type"] = SnapshotMediaType
	return out, nil
}

// tarTree writes the tree under root to w as a tar archive: directories,
// regular files and symlinks, with owner ids dropped. A symlink whose target
// is absolute or climbs out of the tree is left out (conductor refuses to
// unpack one); anything else that is not a file or directory is too.
func tarTree(w io.Writer, root string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: rel, Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime().Truncate(time.Second)}
		switch m := fi.Mode(); {
		case m.IsDir():
			hdr.Typeflag, hdr.Name = tar.TypeDir, rel+"/"
		case m.IsRegular():
			hdr.Typeflag, hdr.Size = tar.TypeReg, fi.Size()
		case m&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if t := path.Clean(path.Join(path.Dir(rel), filepath.ToSlash(target))); path.IsAbs(target) || filepath.IsAbs(target) || t == ".." || strings.HasPrefix(t, "../") {
				log.Printf("github: pr_snapshot: left out %s, a symlink to %s outside the tree", rel, target)
				return nil
			}
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, target
		default:
			return nil
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.CopyN(tw, f, hdr.Size)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// gitEnv is git's environment for a clone from base: the process's, minus
// any GIT_* that could point it elsewhere, with global and system config
// off, no prompting, only base's scheme allowed, and the token (when there
// is one) as an Authorization header scoped to base.
func gitEnv(base, tok string) []string {
	var env []string
	for _, kv := range os.Environ() {
		// SSH_ASKPASS too: git runs it for a credential before it would
		// prompt on a terminal, and a desktop askpass waits for a person.
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "SSH_ASKPASS=") {
			env = append(env, kv)
		}
	}
	scheme, _, _ := strings.Cut(base, "://")
	// A stalled transfer (under 1 KB/s for a minute) fails, as a network
	// failure, instead of holding the step.
	cfg := [][2]string{{"protocol.allow", "never"}, {"protocol." + scheme + ".allow", "always"},
		{"http.lowSpeedLimit", "1000"}, {"http.lowSpeedTime", "60"}}
	if tok != "" {
		cfg = append(cfg, [2]string{"http." + base + "/.extraHeader",
			"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))})
	}
	env = append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=",
		"GIT_CONFIG_COUNT="+strconv.Itoa(len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

type prGit struct {
	dir    string
	env    []string
	secret string // scrubbed from anything git prints
}

func (g *prGit) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, gitBinary, append([]string{"-C", g.dir}, args...)...)
	cmd.Env = g.env
	// git fetch runs helpers (git-remote-https, index-pack) that inherit its
	// stdout and stderr. Killing only git when ctx ends leaves them running,
	// holding the pipes open, and Run waiting on them: so git gets its own
	// process group, the whole group is killed, and Run stops waiting for the
	// pipes soon after.
	exposurekit.SetNewProcessGroup(cmd)
	cmd.Cancel = func() error { exposurekit.KillProcessGroup(cmd); return nil }
	cmd.WaitDelay = gitWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if g.secret != "" {
			msg = strings.ReplaceAll(msg, g.secret, "[redacted]")
		}
		return "", &gitError{args: args[0], stderr: msg, err: err}
	}
	return strings.TrimSpace(stdout.String()), nil
}

type gitError struct {
	args, stderr string
	err          error
}

func (e *gitError) Error() string { return "git " + e.args + ": " + e.err.Error() + ": " + e.stderr }

// fetch runs a fetch, again after a pause when it failed on the network.
func (g *prGit) fetch(ctx context.Context, args ...string) error {
	_, err := g.run(ctx, append([]string{"fetch", "-q", "--no-tags"}, args...)...)
	for attempt := 1; attempt < prCloneAttempts && transientGitFailure(err); attempt++ {
		log.Printf("github: pr_snapshot: fetch failed on the network (attempt %d of %d), retrying: %v", attempt, prCloneAttempts, err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(prCloneBackoff << (attempt - 1)):
		}
		_, err = g.run(ctx, append([]string{"fetch", "-q", "--no-tags"}, args...)...)
	}
	return err
}

var (
	transientGit = []string{"timed out", "connection reset", "could not read from remote repository",
		"the remote end hung up unexpectedly", "early eof", "unexpected disconnect", "too slow"}
	permanentGit = []string{"authentication failed", "could not read username", "could not read password",
		"permission denied", "access denied", "not found", "couldn't find remote ref",
		"returned error: 401", "returned error: 403", "returned error: 404", "http 401", "http 403", "http 404"}
)

// transientGitFailure reports a git failure the network caused — and never
// one where the remote refused the credential or has no such repo or ref,
// even when git also says it could not read from the remote.
func transientGitFailure(err error) bool {
	ge, ok := err.(*gitError)
	if !ok {
		return false
	}
	s := strings.ToLower(ge.stderr)
	for _, m := range permanentGit {
		if strings.Contains(s, m) {
			return false
		}
	}
	for _, m := range transientGit {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// clone fetches the PR head and base at the PR's own depth, deepening both
// until their merge-base is in the clone, then checks out the head.
func (g *prGit) clone(ctx context.Context, remote, base string, number, commits int) (map[string]any, error) {
	if base == "" || strings.HasPrefix(base, "-") || strings.ContainsAny(base, ": \t\n") {
		return nil, fmt.Errorf("the PR's base branch %q is not a branch name", base)
	}
	baseRef := "refs/remotes/origin/" + base
	headRef := fmt.Sprintf("refs/pull/%d/head", number)
	if _, err := g.run(ctx, "init", "-q"); err != nil {
		return nil, err
	}
	if _, err := g.run(ctx, "check-ref-format", baseRef); err != nil {
		return nil, fmt.Errorf("the PR's base branch %q is not a branch name", base)
	}
	if _, err := g.run(ctx, "remote", "add", "origin", remote); err != nil {
		return nil, err
	}
	refspecs := []string{"origin", "+refs/heads/" + base + ":" + baseRef, "+" + headRef + ":" + headRef}
	// The head's own commits plus the one it branched from; a base that
	// moved on since needs more, which the loop finds.
	depth := commits + 1
	if depth < 2 {
		depth = 2
	}
	if err := g.fetch(ctx, append([]string{"--depth=" + strconv.Itoa(depth)}, refspecs...)...); err != nil {
		return nil, err
	}
	mergeBase := ""
	for round := 0; ; round++ {
		if mb, err := g.run(ctx, "merge-base", baseRef, headRef); err == nil {
			mergeBase = mb
			break
		}
		shallow, err := g.run(ctx, "rev-parse", "--is-shallow-repository")
		if err != nil {
			return nil, err
		}
		if shallow != "true" {
			return nil, fmt.Errorf("the PR head and %s share no history", base)
		}
		deepen := []string{"--deepen=" + strconv.Itoa(depth)}
		if round+1 >= prCloneDeepenRounds {
			deepen = []string{"--unshallow"}
		}
		if err := g.fetch(ctx, append(deepen, refspecs...)...); err != nil {
			return nil, err
		}
		depth *= 2
	}
	if _, err := g.run(ctx, "checkout", "-q", "--detach", headRef); err != nil {
		return nil, err
	}
	head, err := g.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return map[string]any{"head_sha": head, "base": base, "merge_base": mergeBase}, nil
}
