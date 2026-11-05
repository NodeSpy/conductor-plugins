package githubkit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// apply_diff: apply a unified diff (git's format) to a branch's current
// head and commit it there, through the Git Data API — the file contents
// the diff touches are read at the head, changed in memory, and written as
// one commit on top of the head, as the caller's write identity. The branch
// moves fast-forward only (force: false): a branch that moved again since
// it was read is a rejection, never an overwrite. Nothing is kept between
// calls.
//
// Each diffHunk must apply exactly (its context and removed lines as they are
// at the head, anywhere in the file); a diffHunk whose change the head already
// has is skipped. A file whose hunks do not apply is a conflict, and then
// nothing is committed.

// applyDiffMaxFile bounds a file apply_diff reads or writes: the contents
// API returns a file's content inline up to 1 MB.
const applyDiffMaxFile = 1 << 20

func (c *Client) applyDiff(ctx context.Context, tok, base, repo string, opts map[string]any) (map[string]any, error) {
	branch, _ := opts["branch"].(string)
	diff, _ := opts["diff"].(string)
	message, _ := opts["message"].(string)
	since, _ := opts["base"].(string)
	if branch == "" || message == "" || strings.TrimSpace(diff) == "" {
		return nil, fmt.Errorf("github.apply_diff: options.branch, options.diff and options.message are required")
	}
	if at, _ := opts["start_at"].(string); at != "" {
		// Only the part from a heading on: one section of a combined diff.
		i := strings.Index("\n"+diff, "\n"+at)
		if i < 0 {
			return nil, fmt.Errorf("github.apply_diff: no line of the diff starts with options.start_at %q", at)
		}
		diff = diff[i:]
	}
	refuse := toStrings(opts["refuse_paths"])
	var head struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	// Read fresh: the decision is about the branch as it is now.
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/git/ref/heads/%s", base, repo, escapePath(branch)), &head); err != nil {
		return nil, err
	}
	onto := head.Object.SHA
	out := map[string]any{"onto": onto, "moved": since != "" && since != onto, "pushed": false, "rejected": false,
		"conflicts": []any{}, "refused": []any{}, "files": 0, "head": onto, "reason": ""}

	patches, err := parsePatches(diff)
	if err != nil {
		return nil, fmt.Errorf("github.apply_diff: %w", err)
	}
	var refused []any
	for _, p := range patches {
		for _, path := range []string{p.oldPath, p.newPath} {
			if path != "" && refusedPath(path, refuse) {
				refused = append(refused, path)
			}
		}
	}
	if len(refused) > 0 {
		out["refused"], out["reason"] = dedupe(refused), "the diff changes a path the caller refuses"
		return out, nil
	}
	read := func(path string) (string, bool, error) {
		var ct struct {
			Type     string `json:"type"`
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
			Size     int    `json:"size"`
		}
		err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", base, repo, escapePath(path), url.QueryEscape(onto)), &ct)
		if httpStatus(err) == http.StatusNotFound {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		if ct.Type != "file" || ct.Encoding != "base64" || ct.Size > applyDiffMaxFile {
			return "", true, errUnreadable
		}
		b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(ct.Content, "\n", ""))
		if err != nil {
			return "", true, err
		}
		return string(b), true, nil
	}
	changes, conflicts, err := applyPatches(patches, read)
	if err != nil {
		return nil, fmt.Errorf("github.apply_diff: %w", err)
	}
	if len(conflicts) > 0 {
		out["conflicts"], out["reason"] = conflicts, "the diff does not apply to the branch's head"
		return out, nil
	}
	if len(changes) == 0 {
		out["reason"] = "the branch's head already has every change in the diff"
		return out, nil
	}

	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/git/commits/%s", base, repo, onto), &commit); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(changes))
	for p := range changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var entries []any
	for _, p := range paths {
		text := changes[p]
		if text == nil {
			entries = append(entries, map[string]any{"path": p, "mode": "100644", "type": "blob", "sha": nil})
			continue
		}
		if len(*text) > applyDiffMaxFile {
			out["conflicts"], out["reason"] = []any{p}, "a changed file is larger than apply_diff writes"
			return out, nil
		}
		var blob struct {
			SHA string `json:"sha"`
		}
		if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/git/blobs", base, repo),
			map[string]any{"content": base64.StdEncoding.EncodeToString([]byte(*text)), "encoding": "base64"}, &blob); err != nil {
			return nil, err
		}
		entries = append(entries, map[string]any{"path": p, "mode": "100644", "type": "blob", "sha": blob.SHA})
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/git/trees", base, repo),
		map[string]any{"base_tree": commit.Tree.SHA, "tree": entries}, &tree); err != nil {
		return nil, err
	}
	// No author: the commit is the token's user's — the write identity.
	var made struct {
		SHA string `json:"sha"`
	}
	if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/git/commits", base, repo),
		map[string]any{"message": message, "tree": tree.SHA, "parents": []string{onto}}, &made); err != nil {
		return nil, err
	}
	err = c.patch(ctx, tok, fmt.Sprintf("%s/repos/%s/git/refs/heads/%s", base, repo, escapePath(branch)),
		map[string]any{"sha": made.SHA, "force": false}, nil)
	if httpStatus(err) == http.StatusUnprocessableEntity {
		out["rejected"], out["reason"] = true, "the branch moved again while the diff was applied"
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out["pushed"], out["head"], out["files"] = true, made.SHA, len(changes)
	return out, nil
}

// httpStatus is the HTTP status a failed call answered, 0 when none.
func httpStatus(err error) int {
	var pe *plugin.Error
	if !errors.As(err, &pe) {
		return 0
	}
	status, _ := pe.Data["status"].(int)
	return status
}

var errUnreadable = errors.New("not a regular file apply_diff can read (a directory, a symlink, a submodule, or over 1 MB)")

func refusedPath(path string, refuse []string) bool {
	for _, r := range refuse {
		if strings.HasSuffix(r, "/") {
			if strings.HasPrefix(path, r) {
				return true
			}
		} else if path == r {
			return true
		}
	}
	return false
}

func dedupe(in []any) []any {
	seen := map[any]bool{}
	var out []any
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// --- the diff ---------------------------------------------------------------

// filePatch is one file's section of a git diff.
type filePatch struct {
	oldPath, newPath string // "" for /dev/null
	hunks            []diffHunk
}

type diffHunk struct {
	oldStart int
	old, new []string // the lines without their prefix
	// noEOLOld/noEOLNew: the side's last line has no newline after it.
	noEOLOld, noEOLNew bool
}

// parsePatches reads a git diff: every "diff --git" section, in order. Text
// outside a section (a heading between two diffs) is skipped. A diffHunk cut
// short (a truncated diff) is an error, as are binary changes, renames with
// quoted names, and paths that leave the repository.
func parsePatches(diff string) ([]filePatch, error) {
	lines := strings.Split(diff, "\n")
	var out []filePatch
	var cur *filePatch
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "diff --git "):
			out = append(out, filePatch{})
			cur = &out[len(out)-1]
			a, b, ok := splitGitPaths(strings.TrimPrefix(l, "diff --git "))
			if !ok {
				return nil, fmt.Errorf("unsupported file header %q (quoted or unusual paths)", l)
			}
			cur.oldPath, cur.newPath = a, b
		case cur == nil:
			// Before the first section.
		case strings.HasPrefix(l, "new file mode "):
			cur.oldPath = ""
		case strings.HasPrefix(l, "deleted file mode "):
			cur.newPath = ""
		case strings.HasPrefix(l, "Binary files ") || l == "GIT binary patch":
			return nil, fmt.Errorf("%s: binary changes are not supported", firstNonEmpty(cur.newPath, cur.oldPath))
		case strings.HasPrefix(l, "new mode ") && strings.HasSuffix(l, "120000"),
			strings.HasPrefix(l, "new file mode 120000"), strings.HasPrefix(l, "new file mode 160000"):
			return nil, fmt.Errorf("%s: symlinks and submodules are not supported", cur.newPath)
		case strings.HasPrefix(l, "@@ "):
			h, n, err := parseHunk(lines, i)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", firstNonEmpty(cur.newPath, cur.oldPath), err)
			}
			cur.hunks = append(cur.hunks, h)
			i = n - 1
		}
	}
	for _, p := range out {
		for _, path := range []string{p.oldPath, p.newPath} {
			if path != "" && !safePath(path) {
				return nil, fmt.Errorf("refusing path %q", path)
			}
		}
	}
	return out, nil
}

// splitGitPaths splits "a/X b/Y" (unquoted, X and Y the same when not a rename).
func splitGitPaths(s string) (string, string, bool) {
	if strings.Contains(s, "\"") || !strings.HasPrefix(s, "a/") {
		return "", "", false
	}
	// The common case: the same path on both sides.
	if n := len(s); n%2 == 1 {
		half := (n - 1) / 2
		if s[half] == ' ' && s[2:half] == s[half+3:] && s[half+1:half+3] == "b/" {
			return s[2:half], s[half+3:], true
		}
	}
	i := strings.Index(s, " b/")
	if i < 0 || strings.Contains(s[i+3:], " b/") {
		return "", "", false
	}
	return s[2:i], s[i+3:], true
}

func safePath(p string) bool {
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
			return false
		}
	}
	return true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// parseHunk reads the hunk at lines[i]; it returns the index after it.
func parseHunk(lines []string, i int) (diffHunk, int, error) {
	var h diffHunk
	f := strings.Fields(lines[i])
	if len(f) < 3 {
		return h, 0, fmt.Errorf("bad diffHunk header %q", lines[i])
	}
	os, ol, err1 := hunkRange(f[1], "-")
	_, nl, err2 := hunkRange(f[2], "+")
	if err1 != nil || err2 != nil {
		return h, 0, fmt.Errorf("bad diffHunk header %q", lines[i])
	}
	h.oldStart = os
	i++
	var last byte
	for ol > 0 || nl > 0 {
		if i >= len(lines) {
			return h, 0, fmt.Errorf("the diff ends inside a diffHunk (it was cut short)")
		}
		l := lines[i]
		if l == "" {
			l = " " // a blank context line whose trailing space was stripped
		}
		switch l[0] {
		case ' ':
			if ol == 0 || nl == 0 {
				return h, 0, fmt.Errorf("a diffHunk has more lines than its header says")
			}
			h.old, h.new = append(h.old, l[1:]), append(h.new, l[1:])
			ol, nl = ol-1, nl-1
		case '-':
			if ol == 0 {
				return h, 0, fmt.Errorf("a diffHunk has more lines than its header says")
			}
			h.old = append(h.old, l[1:])
			ol--
		case '+':
			if nl == 0 {
				return h, 0, fmt.Errorf("a diffHunk has more lines than its header says")
			}
			h.new = append(h.new, l[1:])
			nl--
		case '\\':
			markNoEOL(&h, last)
			i++
			continue
		default:
			return h, 0, fmt.Errorf("the diff ends inside a diffHunk (it was cut short)")
		}
		last = l[0]
		i++
	}
	if i < len(lines) && strings.HasPrefix(lines[i], "\\") {
		markNoEOL(&h, last)
		i++
	}
	return h, i, nil
}

func markNoEOL(h *diffHunk, last byte) {
	switch last {
	case '-':
		h.noEOLOld = true
	case '+':
		h.noEOLNew = true
	case ' ':
		h.noEOLOld, h.noEOLNew = true, true
	}
}

func hunkRange(s, sign string) (start, n int, err error) {
	s, ok := strings.CutPrefix(s, sign)
	if !ok {
		return 0, 0, fmt.Errorf("bad range")
	}
	a, b, has := strings.Cut(s, ",")
	if start, err = strconv.Atoi(a); err != nil {
		return 0, 0, err
	}
	n = 1
	if has {
		if n, err = strconv.Atoi(b); err != nil {
			return 0, 0, err
		}
	}
	return start, n, nil
}

// fileText is a file as lines, and whether its last line ends in a newline.
type fileText struct {
	lines []string
	eol   bool
}

func splitText(s string) fileText {
	if s == "" {
		return fileText{eol: true}
	}
	eol := strings.HasSuffix(s, "\n")
	return fileText{lines: strings.Split(strings.TrimSuffix(s, "\n"), "\n"), eol: eol}
}

func (t fileText) String() string {
	if len(t.lines) == 0 {
		return ""
	}
	s := strings.Join(t.lines, "\n")
	if t.eol {
		s += "\n"
	}
	return s
}

// applyPatches applies the patches in order to the files read returns (text,
// exists). It returns each changed path's new text (nil: deleted) — only the
// paths whose content differs from the head — and the paths that conflict.
func applyPatches(patches []filePatch, read func(path string) (string, bool, error)) (map[string]*string, []any, error) {
	type state struct {
		text   string
		exists bool
		orig   string
		origEx bool
	}
	files := map[string]*state{}
	get := func(path string) (*state, error) {
		if s := files[path]; s != nil {
			return s, nil
		}
		text, ok, err := read(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		s := &state{text: text, exists: ok, orig: text, origEx: ok}
		files[path] = s
		return s, nil
	}
	var conflicts []any
	conflicted := map[string]bool{}
	conflict := func(p string) {
		if !conflicted[p] {
			conflicted[p] = true
			conflicts = append(conflicts, p)
		}
	}
	for _, p := range patches {
		src := firstNonEmpty(p.oldPath, p.newPath)
		if conflicted[src] {
			continue
		}
		s, err := get(src)
		if err != nil {
			return nil, nil, err
		}
		var result string
		switch {
		case p.oldPath == "": // a new file
			res, ok := applyHunks("", p.hunks)
			switch {
			case !ok:
				conflict(src)
				continue
			case s.exists && s.text != res:
				conflict(src) // made by both sides, differently
				continue
			}
			result = res
		case !s.exists:
			if p.newPath == "" {
				continue // deleted on both sides
			}
			conflict(src)
			continue
		default:
			res, ok := applyHunks(s.text, p.hunks)
			if !ok {
				conflict(src)
				continue
			}
			result = res
		}
		switch {
		case p.newPath == "":
			if result != "" {
				conflict(src) // the file to delete has changed
				continue
			}
			s.exists, s.text = false, ""
		case p.newPath != src: // a rename
			dst, err := get(p.newPath)
			if err != nil {
				return nil, nil, err
			}
			if dst.exists && dst.text != result {
				conflict(p.newPath)
				continue
			}
			s.exists, s.text = false, ""
			dst.exists, dst.text = true, result
		default:
			s.exists, s.text = true, result
		}
	}
	if len(conflicts) > 0 {
		return nil, conflicts, nil
	}
	changes := map[string]*string{}
	for path, s := range files {
		switch {
		case !s.exists && s.origEx:
			changes[path] = nil
		case s.exists && (!s.origEx || s.text != s.orig):
			t := s.text
			changes[path] = &t
		}
	}
	return changes, nil, nil
}

// applyHunks applies hunks to text, each where its old lines are (nearest
// its stated line, after the diffHunk before it). A diffHunk whose old lines are not
// there but whose new lines are is already applied, and skipped.
func applyHunks(text string, hunks []diffHunk) (string, bool) {
	t := splitText(text)
	cursor, delta := 0, 0
	for _, h := range hunks {
		want := h.oldStart - 1 + delta
		if len(h.old) == 0 {
			want = h.oldStart + delta // a pure insertion goes after that line
		}
		at := find(t.lines, h.old, want, cursor)
		if at < 0 {
			if len(h.new) > 0 && find(t.lines, h.new, want, cursor) >= 0 {
				continue // already applied
			}
			return "", false
		}
		next := append(append(append([]string{}, t.lines[:at]...), h.new...), t.lines[at+len(h.old):]...)
		if at+len(h.old) == len(t.lines) { // the diffHunk reaches the end of the file
			switch {
			case h.noEOLNew:
				t.eol = false
			case h.noEOLOld || len(h.new) > 0:
				t.eol = true
			}
		}
		t.lines = next
		cursor = at + len(h.new)
		delta += len(h.new) - len(h.old)
	}
	return t.String(), true
}

// find is where want (a run of lines) sits in lines at or after from,
// nearest to at; -1 when it does not.
func find(lines, want []string, at, from int) int {
	match := func(i int) bool {
		if i < from || i+len(want) > len(lines) {
			return false
		}
		for j, w := range want {
			if lines[i+j] != w {
				return false
			}
		}
		return true
	}
	if len(want) == 0 {
		if at < from {
			at = from
		}
		if at > len(lines) {
			at = len(lines)
		}
		return at
	}
	for d := 0; d <= len(lines); d++ {
		if match(at - d) {
			return at - d
		}
		if d > 0 && match(at+d) {
			return at + d
		}
	}
	return -1
}
