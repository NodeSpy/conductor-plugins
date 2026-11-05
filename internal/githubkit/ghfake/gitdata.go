package ghfake

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// The Git Data API, over a small object model: blobs by content sha, flat
// trees (path → blob sha), commits naming a tree. Commits made the older way
// (commitTo: put_file, PRs) have no tree and read as the repo's Contents.

func gitBlobSHA(text string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(text), text)))
	return hex.EncodeToString(sum[:])
}

func (r *Repo) gitObjects() {
	if r.Trees == nil {
		r.Trees = map[string]map[string]string{}
	}
	if r.Blobs == nil {
		r.Blobs = map[string]string{}
	}
}

func (r *Repo) putBlob(text string) string {
	r.gitObjects()
	sha := gitBlobSHA(text)
	r.Blobs[sha] = text
	return sha
}

func (r *Repo) putTree(files map[string]string) string {
	r.gitObjects()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha1.New()
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00%s\n", p, files[p])
	}
	sha := hex.EncodeToString(h.Sum(nil))
	r.Trees[sha] = files
	return sha
}

// filesAt is the commit's files, path → text.
func (r *Repo) filesAt(cm *Commit) map[string]string {
	out := map[string]string{}
	if cm != nil && cm.Tree != "" {
		for p, b := range r.Trees[cm.Tree] {
			out[p] = r.Blobs[b]
		}
		return out
	}
	for p, ct := range r.Contents {
		out[p] = ct.Text
	}
	return out
}

// treeOf is the commit's tree sha, minting one from Contents for an old-style commit.
func (r *Repo) treeOf(cm *Commit) string {
	if cm.Tree == "" {
		files := map[string]string{}
		for p, t := range r.filesAt(cm) {
			files[p] = r.putBlob(t)
		}
		cm.Tree = r.putTree(files)
	}
	return cm.Tree
}

// isAncestor reports whether a is b or one of its ancestors.
func (r *Repo) isAncestor(a, b string) bool {
	seen := map[string]bool{}
	stack := []string{b}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s == a {
			return true
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		if cm := r.Commits[s]; cm != nil {
			stack = append(stack, cm.Parents...)
		}
	}
	return false
}

// CommitFiles commits files (path → text) and deletes onto branch as login:
// a commit with a tree of its own, its parent's files changed by these.
func (f *Fake) CommitFiles(repo, branch, login, msg string, files map[string]string, deletes ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repo(repo)
	parent := r.Commits[r.Branches[branch]]
	next := r.filesAt(parent)
	for p, t := range files {
		next[p] = t
	}
	for _, p := range deletes {
		delete(next, p)
	}
	tree := map[string]string{}
	for p, t := range next {
		tree[p] = r.putBlob(t)
	}
	cm := f.commitTo(r, branch, f.user(login), msg)
	cm.Tree = r.putTree(tree)
	return cm.SHA
}

// FilesAt is the branch head's files (path → text), and its sha.
func (f *Fake) FilesAt(repo, branch string) (map[string]string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repo(repo)
	sha := r.Branches[branch]
	return r.filesAt(r.Commits[sha]), sha
}

// CommitAuthor is the login a commit was made by.
func (f *Fake) CommitAuthor(repo, sha string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cm := f.repo(repo).Commits[sha]; cm != nil && cm.Author != nil {
		return cm.Author.Login
	}
	return ""
}

func (f *Fake) gitRefJSON(r *Repo, branch, sha string) map[string]any {
	ref := "refs/heads/" + branch
	return ShapeOf("git-ref", map[string]any{"ref": ref, "node_id": nodeID("REF", f.id()),
		"url":    f.api("/repos/%s/git/%s", r.FullName(), ref),
		"object": map[string]any{"type": "commit", "sha": sha, "url": f.api("/repos/%s/git/commits/%s", r.FullName(), sha)}})
}

func (f *Fake) gitDataCommitJSON(r *Repo, cm *Commit) map[string]any {
	who := map[string]any{"name": cm.Author.Login, "email": cm.Author.Email, "date": ts(cm.At)}
	parents := []any{}
	for _, p := range cm.Parents {
		parents = append(parents, map[string]any{"sha": p, "url": f.api("/repos/%s/git/commits/%s", r.FullName(), p),
			"html_url": fmt.Sprintf("%s/%s/commit/%s", htmlBase, r.FullName(), p)})
	}
	tree := r.treeOf(cm)
	return ShapeOf("git-commit", map[string]any{"sha": cm.SHA, "node_id": nodeID("C", f.id()),
		"url":      f.api("/repos/%s/git/commits/%s", r.FullName(), cm.SHA),
		"html_url": fmt.Sprintf("%s/%s/commit/%s", htmlBase, r.FullName(), cm.SHA), "author": who, "committer": who,
		"message": cm.Message, "tree": map[string]any{"sha": tree, "url": f.api("/repos/%s/git/trees/%s", r.FullName(), tree)},
		"parents": parents, "verification": map[string]any{"verified": false, "reason": "unsigned", "signature": nil, "payload": nil, "verified_at": nil}})
}

func hGetGitRef(c *call) (int, any) {
	ref := c.p("ref")
	branch, ok := strings.CutPrefix(ref, "heads/")
	sha := c.repo.Branches[branch]
	if !ok || sha == "" {
		return notFound()
	}
	return 200, c.f.gitRefJSON(c.repo, branch, sha)
}

func hUpdateGitRef(c *call) (int, any) {
	branch, ok := strings.CutPrefix(c.p("ref"), "heads/")
	cur := c.repo.Branches[branch]
	if !ok || cur == "" {
		return invalid("Reference", "ref", "invalid", "Reference does not exist")
	}
	sha := str(c.body, "sha")
	if _, ok := c.repo.Commits[sha]; !ok {
		return invalid("Reference", "sha", "invalid", "Object does not exist")
	}
	if force, _ := c.body["force"].(bool); !force && !c.repo.isAncestor(cur, sha) {
		return invalid("Reference", "sha", "invalid", "Update is not a fast forward")
	}
	c.repo.Branches[branch] = sha
	return 200, c.f.gitRefJSON(c.repo, branch, sha)
}

func hGetGitCommit(c *call) (int, any) {
	cm := c.repo.Commits[c.p("commit_sha")]
	if cm == nil {
		return notFound()
	}
	return 200, c.f.gitDataCommitJSON(c.repo, cm)
}

func hCreateBlob(c *call) (int, any) {
	text := str(c.body, "content")
	if str(c.body, "encoding") == "base64" {
		t, err := unb64(text)
		if err != nil {
			return invalid("Blob", "content", "invalid", "content is not valid Base64")
		}
		text = t
	}
	sha := c.repo.putBlob(text)
	return 201, ShapeOf("short-blob", map[string]any{"sha": sha, "url": c.f.api("/repos/%s/git/blobs/%s", c.repo.FullName(), sha)})
}

func hCreateTree(c *call) (int, any) {
	c.repo.gitObjects()
	files := map[string]string{}
	if base := str(c.body, "base_tree"); base != "" {
		t, ok := c.repo.Trees[base]
		if !ok {
			return invalid("Tree", "base_tree", "invalid", "Invalid tree info")
		}
		for p, b := range t {
			files[p] = b
		}
	}
	entries, _ := c.body["tree"].([]any)
	for _, e := range entries {
		m, _ := e.(map[string]any)
		path := str(m, "path")
		if path == "" || str(m, "type") != "blob" {
			return invalid("Tree", "tree", "invalid", "only blob entries with a path")
		}
		switch sha, has := m["sha"]; {
		case has && sha == nil:
			delete(files, path)
		case str(m, "sha") != "":
			if _, ok := c.repo.Blobs[str(m, "sha")]; !ok {
				return invalid("Tree", "sha", "invalid", "Object does not exist")
			}
			files[path] = str(m, "sha")
		default:
			files[path] = c.repo.putBlob(str(m, "content"))
		}
	}
	sha := c.repo.putTree(files)
	out := []any{}
	for p, b := range files {
		out = append(out, map[string]any{"path": p, "mode": "100644", "type": "blob", "sha": b, "size": len(c.repo.Blobs[b]),
			"url": c.f.api("/repos/%s/git/blobs/%s", c.repo.FullName(), b)})
	}
	return 201, ShapeOf("git-tree", map[string]any{"sha": sha, "url": c.f.api("/repos/%s/git/trees/%s", c.repo.FullName(), sha),
		"truncated": false, "tree": out})
}

func hCreateGitCommit(c *call) (int, any) {
	tree := str(c.body, "tree")
	if _, ok := c.repo.Trees[tree]; !ok {
		return invalid("Commit", "tree", "invalid", "Tree SHA does not exist")
	}
	parents := strs(c.body["parents"])
	for _, p := range parents {
		if _, ok := c.repo.Commits[p]; !ok {
			return invalid("Commit", "parents", "invalid", "Parent SHA does not exist")
		}
	}
	cm := &Commit{SHA: c.f.sha(tree + str(c.body, "message")), Message: str(c.body, "message"), Author: c.actor(),
		At: c.f.now(), Tree: tree, Parents: parents}
	c.repo.Commits[cm.SHA] = cm
	return 201, c.f.gitDataCommitJSON(c.repo, cm)
}
