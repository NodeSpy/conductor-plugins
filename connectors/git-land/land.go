package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// keep, replay and push land commits that were made somewhere else — a
// workspace another process committed in — from the connection's own
// repository (`dir`), never by running git with a working tree or hooks in
// the repository the commits came from:
//
//   - keep copies a revision out of another repository into this one,
//     under a ref, by fetching it (the other side runs only upload-pack).
//   - replay re-creates a run of commits on another base without a working
//     tree (git merge-tree), and reports the paths a commit conflicts on.
//     Given remote + branch it fetches the branch first and says whether it
//     moved: its head is not an ancestor of the commits, so pushing them
//     would not be a fast-forward.
//
//   - push sends a commit to a remote branch, fast-forward only: no force
//     flag, and a refspec starting with + is refused.

func landVerbs() []plugin.Verb {
	return []plugin.Verb{
		{
			Name: "keep",
			Desc: "copy a revision out of another repository (a workspace) into the connection's repository under a ref — a fetch, so nothing runs in that repository but upload-pack. The connection's dir is created as a bare repository when it does not exist.",
			Options: plugin.Schema{
				"from": {Type: "string", Required: true, Desc: "path of the repository or worktree holding the commits"},
				"rev":  {Type: "string", Desc: "revision there (default HEAD)"},
				"ref":  {Type: "string", Required: true, Desc: "the ref to keep it under here, refs/…"},
			},
			Outputs: withOutputs(plugin.Schema{
				"sha": {Type: "string", Desc: "the kept commit"},
				"ref": {Type: "string"},
			}),
		},
		{
			Name: "replay",
			Desc: "replay the commits base..head onto another base without a working tree or hooks (git merge-tree), in the connection's repository: clean, the new tip; otherwise the commit and paths that conflict. With remote + branch the branch is fetched first. Only when it moved — its head is not an ancestor of head, so pushing head would not fast-forward — is anything replayed.",
			Options: plugin.Schema{
				"head":   {Type: "string", Required: true, Desc: "the tip of the commits to replay (a kept ref)"},
				"base":   {Type: "string", Desc: "the commit they were made on (exclusive), an ancestor of head; default the merge base of head and the new base"},
				"onto":   {Type: "string", Desc: "the new base (a revision here); or remote + branch"},
				"remote": {Type: "string", Scope: "repo", Desc: "fetch the new base from this remote URL"},
				"branch": {Type: "string", Desc: "the remote branch whose head is the new base"},
			},
			Outputs: withOutputs(plugin.Schema{
				"onto":            {Type: "string", Desc: "the new base"},
				"moved":           {Type: "boolean", Desc: "the new base is not an ancestor of head: pushing head would not fast-forward, so it was replayed"},
				"base":            {Type: "string", Desc: "the base the commits were replayed from"},
				"clean":           {Type: "boolean", Desc: "every commit replayed without a conflict"},
				"head":            {Type: "string", Desc: "the replayed tip (clean); onto itself when the new base already holds every change"},
				"commits":         {Type: "integer", Desc: "commits replayed"},
				"dropped":         {Type: "integer", Desc: "commits whose change the new base already holds"},
				"conflicts":       {Type: "array", Desc: "paths the conflicting commit conflicts on"},
				"conflict_commit": {Type: "string", Desc: "the commit that conflicts"},
				"reason":          {Type: "string", Desc: "why the replay stopped, when not a conflict (a merge commit in the range)"},
			}),
		},
		{
			Name: "push",
			Desc: "push a commit here to a remote branch, fast-forward only: never forced, so a branch that moved again since is a rejection (exit_code non-zero, rejected true). No hooks run.",
			Options: plugin.Schema{
				"remote": {Type: "string", Required: true, Scope: "repo", Desc: "the remote URL"},
				"rev":    {Type: "string", Required: true, Desc: "the commit to push (a replayed head)"},
				"branch": {Type: "string", Required: true, Desc: "the remote branch to update"},
			},
			Outputs: withOutputs(plugin.Schema{
				"pushed":   {Type: "boolean", Desc: "the branch now points at rev"},
				"rejected": {Type: "boolean", Desc: "the remote refused it (not a fast-forward, or a protected branch)"},
			}),
		},
	}
}

// gitRunner runs git in the connection's repository: the connection's -c
// flags and credentials, and never a hook.
type gitRunner struct {
	ctx    context.Context
	binary string
	dir    string
	env    []string
	flags  []string
	sign   bool // replay signs the commits it writes
}

func (g gitRunner) run(stdin string, args ...string) (stdout, stderr string, exit int, err error) {
	full := append(append(append([]string{}, g.flags...), "-c", "core.hooksPath="+os.DevNull), args...)
	res, err := runGitIn(g.ctx, g.binary, g.dir, g.env, full, stdin)
	if err != nil {
		return "", "", 0, err
	}
	return res["stdout"].(string), res["stderr"].(string), res["exit_code"].(int), nil
}

// must runs git and fails on a non-zero exit.
func (g gitRunner) must(args ...string) (string, error) {
	out, errOut, exit, err := g.run("", args...)
	if err != nil {
		return "", err
	}
	if exit != 0 {
		return "", fmt.Errorf("git %s: exit %d: %s", args[0], exit, strings.TrimSpace(errOut))
	}
	return strings.TrimSpace(out), nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func invokeLand(g gitRunner, verb string, o map[string]any) (map[string]any, error) {
	if g.dir == "" {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, verb+": the connection's dir (the repository commits are kept and replayed in) is required")
	}
	switch verb {
	case "keep":
		return keep(g, o)
	case "replay":
		return replay(g, o)
	case "push":
		return push(g, o)
	}
	return nil, plugin.Errorf(plugin.CodeMethodNotFound, "unknown verb "+verb)
}

func push(g gitRunner, o map[string]any) (map[string]any, error) {
	remote, rev, branch := str(o["remote"]), str(o["rev"]), str(o["branch"])
	if remote == "" || rev == "" || branch == "" || strings.HasPrefix(remote, "-") || strings.HasPrefix(rev, "-") || strings.HasPrefix(rev, "+") {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, "push: remote, rev and branch are required, and none may start with - or +")
	}
	if _, err := g.must("check-ref-format", "--branch", branch); err != nil {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, "push: "+err.Error())
	}
	sha, err := g.must("rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, "push: "+err.Error())
	}
	// No '+', no --force: a fast-forward or nothing.
	out, errOut, exit, err := g.run("", "push", "--porcelain", "--no-verify", "--end-of-options", remote, sha+":refs/heads/"+branch)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, "push: "+err.Error())
	}
	// --porcelain marks a ref the remote refused with a leading "!".
	rejected := false
	for _, line := range strings.Split(out, "\n") {
		rejected = rejected || strings.HasPrefix(line, "!\t")
	}
	return map[string]any{"stdout": out, "stderr": errOut, "exit_code": exit, "pushed": exit == 0, "rejected": rejected}, nil
}

func keep(g gitRunner, o map[string]any) (map[string]any, error) {
	from, ref, rev := str(o["from"]), str(o["ref"]), strOr(o["rev"], "HEAD")
	if from == "" || !strings.HasPrefix(ref, "refs/") || strings.HasPrefix(rev, "-") {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, "keep: from, a rev and a ref under refs/ are required")
	}
	if _, err := os.Stat(g.dir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(g.dir), 0o700); err != nil {
			return nil, plugin.Errorf(plugin.CodeInternalError, "keep: "+err.Error())
		}
		init := g
		init.dir = filepath.Dir(g.dir)
		if _, err := init.must("init", "-q", "--bare", g.dir); err != nil {
			return nil, plugin.Errorf(plugin.CodeInternalError, "keep: "+err.Error())
		}
	}
	if _, err := g.must("check-ref-format", ref); err != nil {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, "keep: "+err.Error())
	}
	// A fetch from a local path: the other repository runs upload-pack
	// only. --no-write-fetch-head and no tags: nothing here moves but ref.
	res := map[string]any{}
	out, errOut, exit, err := g.run("", "-c", "protocol.file.allow=always", "fetch", "-q", "--no-tags", "--no-write-fetch-head",
		"--no-recurse-submodules", "--end-of-options", from, "+"+rev+":"+ref)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, "keep: "+err.Error())
	}
	res["stdout"], res["stderr"], res["exit_code"] = out, errOut, exit
	if exit == 0 {
		sha, err := g.must("rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		if err != nil {
			return nil, plugin.Errorf(plugin.CodeInternalError, "keep: "+err.Error())
		}
		res["sha"], res["ref"] = sha, ref
	}
	return res, nil
}

func replay(g gitRunner, o map[string]any) (map[string]any, error) {
	head, base, onto := str(o["head"]), str(o["base"]), str(o["onto"])
	remote, branch := str(o["remote"]), str(o["branch"])
	for _, v := range []string{head, base, onto, remote, branch} {
		if strings.HasPrefix(v, "-") {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "replay: an option may not start with -")
		}
	}
	if head == "" || (onto == "") == (remote == "" || branch == "") {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, "replay: head, and either onto or remote + branch, are required")
	}
	res := map[string]any{"stdout": "", "stderr": "", "exit_code": 0, "clean": false, "commits": 0, "dropped": 0, "conflicts": []any{}}
	fail := func(err error) (map[string]any, error) {
		return nil, plugin.Errorf(plugin.CodeInternalError, "replay: "+err.Error())
	}
	if onto == "" {
		if _, err := g.must("check-ref-format", "--branch", branch); err != nil {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "replay: "+err.Error())
		}
		tracking := "refs/replay/" + branch
		if _, err := g.must("fetch", "-q", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules",
			"--end-of-options", remote, "+refs/heads/"+branch+":"+tracking); err != nil {
			return fail(err)
		}
		onto = tracking
	}
	resolve := func(rev string) (string, error) {
		return g.must("rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	}
	ontoSHA, err := resolve(onto)
	if err != nil {
		return fail(err)
	}
	headSHA, err := resolve(head)
	if err != nil {
		return fail(err)
	}
	isAncestor := func(a, b string) (bool, error) {
		_, errOut, exit, err := g.run("", "merge-base", "--is-ancestor", a, b)
		if err != nil || exit > 1 {
			return false, fmt.Errorf("git merge-base --is-ancestor: exit %d: %s %v", exit, strings.TrimSpace(errOut), err)
		}
		return exit == 0, nil
	}
	ff, err := isAncestor(ontoSHA, headSHA)
	if err != nil {
		return fail(err)
	}
	res["onto"], res["moved"] = ontoSHA, !ff
	if ff {
		return res, nil // a fast-forward: nothing to replay
	}
	var baseSHA string
	if base != "" {
		if baseSHA, err = resolve(base); err != nil {
			return fail(err)
		}
		if ok, err := isAncestor(baseSHA, headSHA); err != nil {
			return fail(err)
		} else if !ok {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "replay: base "+base+" is not an ancestor of head")
		}
	} else if baseSHA, err = g.must("merge-base", ontoSHA, headSHA); err != nil {
		return fail(err)
	}
	res["base"] = baseSHA
	list, err := g.must("rev-list", "--reverse", "--topo-order", baseSHA+".."+headSHA)
	if err != nil {
		return fail(err)
	}
	cur := ontoSHA
	replayed, dropped := 0, 0
	for _, c := range strings.Fields(list) {
		parents, err := g.must("rev-parse", c+"^@")
		if err != nil {
			return fail(err)
		}
		if ps := strings.Fields(parents); len(ps) != 1 {
			res["reason"], res["conflict_commit"] = "commit "+c+" is a merge: only a linear run of commits is replayed", c
			return res, nil
		}
		// The commit's own change (against its parent), merged into cur.
		mt, _, exit, err := g.run("", "merge-tree", "--write-tree", "--name-only", "--no-messages", "--merge-base="+c+"^", cur, c)
		if err != nil {
			return fail(err)
		}
		lines := strings.Split(strings.TrimSpace(mt), "\n")
		if exit == 1 {
			files := []any{}
			for _, f := range lines[1:] {
				if f != "" {
					files = append(files, f)
				}
			}
			res["conflicts"], res["conflict_commit"] = files, c
			return res, nil
		}
		if exit != 0 || !shaRE.MatchString(lines[0]) {
			return fail(fmt.Errorf("git merge-tree: exit %d: %s", exit, strings.TrimSpace(mt)))
		}
		tree := lines[0]
		curTree, err := g.must("rev-parse", cur+"^{tree}")
		if err != nil {
			return fail(err)
		}
		if tree == curTree {
			dropped++ // the new base already holds this change
			continue
		}
		next, err := recommit(g, c, tree, cur, g.sign)
		if err != nil {
			return fail(err)
		}
		cur = next
		replayed++
	}
	res["clean"], res["head"], res["commits"], res["dropped"] = true, cur, replayed, dropped
	return res, nil
}

// recommit writes tree as a commit on parent with c's author, committer and
// message (git commit-tree, -S when signing).
func recommit(g gitRunner, c, tree, parent string, sign bool) (string, error) {
	meta, err := g.must("log", "-1", "--format=%an%x00%ae%x00%ad%x00%cn%x00%ce%x00%cd", "--date=raw", c)
	if err != nil {
		return "", err
	}
	f := strings.Split(meta, "\x00")
	if len(f) != 6 {
		return "", fmt.Errorf("commit %s: unreadable author/committer", c)
	}
	msg, _, exit, err := g.run("", "log", "-1", "--format=%B", c)
	if err != nil || exit != 0 {
		return "", fmt.Errorf("commit %s: unreadable message", c)
	}
	args := []string{"commit-tree", tree, "-p", parent, "-F", "-"}
	if sign {
		args = append(args, "-S")
	}
	ident := g
	ident.env = append(append([]string{}, g.env...),
		"GIT_AUTHOR_NAME="+f[0], "GIT_AUTHOR_EMAIL="+f[1], "GIT_AUTHOR_DATE="+f[2],
		"GIT_COMMITTER_NAME="+f[3], "GIT_COMMITTER_EMAIL="+f[4], "GIT_COMMITTER_DATE="+f[5])
	out, errOut, exit, err := ident.run(strings.TrimSuffix(msg, "\n"), args...)
	if err != nil {
		return "", err
	}
	if exit != 0 {
		return "", fmt.Errorf("git commit-tree: exit %d: %s", exit, strings.TrimSpace(errOut))
	}
	return strings.TrimSpace(out), nil
}
