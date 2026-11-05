package githubkit

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// pr_clone makes a pull request's tree for a step that hands it to others (a
// panel of reviewers sharing one fetch): a fresh temp directory holding a
// shallow clone of the PR head and its base branch, deep enough that the
// base...HEAD merge-base is in it, with HEAD detached at the PR head.
//
// It runs git over HTTPS only, with the connector's read credential. The
// token travels as an http.<host>.extraHeader in git's environment
// (GIT_CONFIG_COUNT), so it is never in argv and never written to disk; the
// user's and system's git config are ignored (no credential helper, no
// insteadOf that could turn the URL into ssh). A fetch that fails on the
// network is retried; one the remote refused is not.

// gitBinary is the git pr_clone runs.
var gitBinary = "git"

const (
	prCloneDirPrefix = "conductor-github-pr-"
	prCloneAttempts  = 3
	// prCloneDeepenRounds bounds the deepening: the depth doubles each
	// round, and the last round fetches the whole history.
	prCloneDeepenRounds = 12
)

var prCloneBackoff = 2 * time.Second

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

func (c *Client) prClone(ctx context.Context, repo string, number int) (map[string]any, error) {
	if !repoName.MatchString(repo) || strings.Contains(repo, "..") {
		return nil, fmt.Errorf("github.pr_clone: options.repo must be owner/name, got %q", repo)
	}
	if number <= 0 {
		return nil, fmt.Errorf("github.pr_clone: options.pr is required")
	}
	gitBase, err := c.cloneBase()
	if err != nil {
		return nil, fmt.Errorf("github.pr_clone: %w", err)
	}
	tok, err := c.MintCredential(ctx, "read", repo)
	if err != nil {
		return nil, fmt.Errorf("github.pr_clone: %w", err)
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
	dir, err := os.MkdirTemp("", prCloneDirPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("github.pr_clone: %w", err)
	}
	g := &prGit{dir: dir, env: gitEnv(gitBase, tok), secret: tok}
	out, err := g.clone(ctx, gitBase+"/"+repo+".git", pr.Base.Ref, number, pr.Commits)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("github.pr_clone %s#%d: %w", repo, number, err)
	}
	out["dir"] = dir
	return out, nil
}

// gitEnv is git's environment for a clone from base: the process's, minus
// any GIT_* that could point it elsewhere, with global and system config
// off, no prompting, only base's scheme allowed, and the token (when there
// is one) as an Authorization header scoped to base.
func gitEnv(base, tok string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
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
	env = append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
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
		log.Printf("github: pr_clone: fetch failed on the network (attempt %d of %d), retrying: %v", attempt, prCloneAttempts, err)
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

// discardPRClone removes a pr_clone directory — and only that: a directory
// directly under the temp dir carrying pr_clone's prefix, never a symlink.
// One already gone is fine, so a cleanup hook can run twice.
func discardPRClone(dir string) (map[string]any, error) {
	root := filepath.Clean(os.TempDir())
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || filepath.Dir(dir) != root ||
		!strings.HasPrefix(filepath.Base(dir), prCloneDirPrefix) {
		return nil, fmt.Errorf("github.pr_clone_discard: %q is not a pr_clone directory", dir)
	}
	switch fi, err := os.Lstat(dir); {
	case os.IsNotExist(err):
	case err != nil:
		return nil, fmt.Errorf("github.pr_clone_discard: %w", err)
	case !fi.IsDir():
		return nil, fmt.Errorf("github.pr_clone_discard: %q is not a pr_clone directory", dir)
	default:
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("github.pr_clone_discard: %w", err)
		}
	}
	return map[string]any{"ok": true}, nil
}
