package githubkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghfake"
)

// reviewWorld is world with a client posting as the reviewer, rev, and a PR
// (by me) whose one file, main.go, has two hunks —
// new lines 1–7 and 88–118 — the shape of @@ -1,6 +1,7 @@ and
// @@ -87,3 +88,31 @@.
func reviewWorld(t *testing.T) (*ghfake.Fake, *Client, int) {
	t.Helper()
	f, _, _ := world(t)
	kit, err := NewClient(Config{WriteToken: "tok-rev", APIBase: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	lines := append(seqInts(1, 7), seqInts(88, 118)...)
	n := f.OpenPR("acme/w", "me", ghfake.PROpts{Title: "hunks", Head: "hunks",
		Files: []ghfake.File{{Path: "main.go", Status: "modified", Additions: 32, Deletions: 3, Lines: lines}}})
	return f, kit, n
}

func seqInts(a, b int) []int {
	var out []int
	for i := a; i <= b; i++ {
		out = append(out, i)
	}
	return out
}

func comment(path string, line int, body string) map[string]any {
	return map[string]any{"path": path, "line": line, "body": body}
}

func onlyReview(t *testing.T, f *ghfake.Fake, n int) *ghfake.Review {
	t.Helper()
	p := f.PR("acme/w", n)
	if len(p.Reviews) != 1 {
		t.Fatalf("PR has %d reviews, want 1", len(p.Reviews))
	}
	return p.Reviews[0]
}

func TestSubmitReviewInHunkCommentPostsInline(t *testing.T) {
	f, kit, n := reviewWorld(t)
	res, err := kit.Invoke(context.Background(), "submit_review", map[string]any{
		"repo": "acme/w", "pr": n, "event": "COMMENT", "body": "summary",
		"comments": []any{comment("main.go", 3, "first hunk"), comment("main.go", 100, "second hunk")},
	})
	if err != nil {
		t.Fatal(err)
	}
	rv := onlyReview(t, f, n)
	if len(rv.Comments) != 2 || rv.Comments[0].Line != 3 || rv.Comments[1].Line != 100 {
		t.Fatalf("inline comments = %+v, want lines 3 and 100", rv.Comments)
	}
	if rv.Body != "summary" {
		t.Fatalf("body = %q, want just the summary", rv.Body)
	}
	if res["in_body"] != 0 {
		t.Fatalf("in_body = %v, want 0", res["in_body"])
	}
}

// The live failure: comments on new lines 14 and 19 of a file in the PR, but
// between its hunks. They go into the body under main.go:14 / main.go:19 and
// the in-hunk comment still posts inline.
func TestSubmitReviewOutOfHunkCommentGoesToBody(t *testing.T) {
	f, kit, n := reviewWorld(t)
	res, err := kit.Invoke(context.Background(), "submit_review", map[string]any{
		"repo": "acme/w", "pr": n, "event": "REQUEST_CHANGES", "body": "summary",
		"comments": []any{comment("main.go", 14, "about fourteen"), comment("main.go", 19, "about nineteen"), comment("main.go", 90, "in the hunk")},
	})
	if err != nil {
		t.Fatal(err)
	}
	rv := onlyReview(t, f, n)
	if rv.State != "CHANGES_REQUESTED" {
		t.Fatalf("state = %s, want CHANGES_REQUESTED", rv.State)
	}
	if len(rv.Comments) != 1 || rv.Comments[0].Line != 90 {
		t.Fatalf("inline comments = %+v, want only line 90", rv.Comments)
	}
	for _, want := range []string{"summary", "`main.go:14`", "about fourteen", "`main.go:19`", "about nineteen"} {
		if !strings.Contains(rv.Body, want) {
			t.Fatalf("body is missing %q:\n%s", want, rv.Body)
		}
	}
	if res["in_body"] != 2 {
		t.Fatalf("in_body = %v, want 2", res["in_body"])
	}
}

func TestSubmitReviewFileNotInPRGoesToBody(t *testing.T) {
	f, kit, n := reviewWorld(t)
	_, err := kit.Invoke(context.Background(), "submit_review", map[string]any{
		"repo": "acme/w", "pr": n, "event": "COMMENT",
		"comments": []any{comment("other.go", 3, "not in this PR"), comment("main.go", 5, "fine")},
	})
	if err != nil {
		t.Fatal(err)
	}
	rv := onlyReview(t, f, n)
	if len(rv.Comments) != 1 || rv.Comments[0].Path != "main.go" {
		t.Fatalf("inline comments = %+v, want only main.go", rv.Comments)
	}
	if !strings.Contains(rv.Body, "`other.go:3`") || !strings.Contains(rv.Body, "not in this PR") {
		t.Fatalf("body is missing the other.go comment:\n%s", rv.Body)
	}
}

// GitHub can still 422 a review the hunk check passed (the head moved between
// the check and the post, say). The review then posts once more with every
// comment in the body, so the verdict and all text land.
func TestSubmitReview422RetriesWithCommentsInBody(t *testing.T) {
	f, _, n := reviewWorld(t)
	var rejected atomic.Int32
	h := f.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews") && rejected.Add(1) == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Unprocessable Entity","errors":["Pull request review thread line must be part of the diff"],"status":"422"}`))
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	kit, err := NewClient(Config{WriteToken: "tok-rev", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := kit.Invoke(context.Background(), "submit_review", map[string]any{
		"repo": "acme/w", "pr": n, "event": "COMMENT", "body": "summary",
		"comments": []any{comment("main.go", 3, "would have been inline")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Load() != 2 {
		t.Fatalf("review posts = %d, want 2 (rejected, then retried)", rejected.Load())
	}
	rv := onlyReview(t, f, n)
	if len(rv.Comments) != 0 {
		t.Fatalf("inline comments = %+v, want none on the retry", rv.Comments)
	}
	if !strings.Contains(rv.Body, "`main.go:3`") || !strings.Contains(rv.Body, "would have been inline") {
		t.Fatalf("body is missing the moved comment:\n%s", rv.Body)
	}
	if res["in_body"] != 1 {
		t.Fatalf("in_body = %v, want 1", res["in_body"])
	}
}

// A GitHub error carries its errors[] reasons, not just "Unprocessable Entity".
func TestAPIErrorIncludesGitHubErrors(t *testing.T) {
	_, kit, n := world(t) // me, approving my own PR
	_, err := kit.Invoke(context.Background(), "submit_review",
		map[string]any{"repo": "acme/w", "pr": n, "event": "APPROVE", "body": "mine"})
	pe := asContractError(t, err)
	if !strings.Contains(pe.Message, "Unprocessable Entity (Can not approve your own pull request)") {
		t.Fatalf("message = %q, want GitHub's message and errors[]", pe.Message)
	}
}

func TestErrorText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"message":"Not Found"}`, "Not Found"},
		{`{"message":"Validation Failed","errors":[{"resource":"Issue","field":"title","code":"missing_field"},{"message":"custom reason"}]}`,
			"Validation Failed (Issue.title missing_field; custom reason)"},
		{`{"errors":["only a reason"]}`, "only a reason"},
		{`not json`, ""},
	} {
		if got := errorText([]byte(tc.in)); got != tc.want {
			t.Errorf("errorText(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPlaceReviewCommentsSidesAndRanges(t *testing.T) {
	files := map[string][]hunk{
		"a.go":    parseHunks("@@ -1,6 +1,7 @@\n x\n@@ -87,3 +88,31 @@\n x\n"),
		"bin.png": nil,
	}
	cases := []struct {
		c      map[string]any
		inline bool
	}{
		{comment("a.go", 7, "x"), true},
		{comment("a.go", 14, "x"), false},
		{map[string]any{"path": "a.go", "line": 6, "side": "LEFT", "body": "x"}, true},
		{map[string]any{"path": "a.go", "line": 7, "side": "LEFT", "body": "x"}, false},
		{map[string]any{"path": "a.go", "start_line": 90, "line": 100, "body": "x"}, true},
		{map[string]any{"path": "a.go", "start_line": 5, "line": 100, "body": "x"}, false}, // spans two hunks
		{comment("bin.png", 1, "x"), false},
		{map[string]any{"path": "a.go", "body": "x"}, false},
	}
	for i, tc := range cases {
		inline, _ := placeReviewComments([]map[string]any{tc.c}, files)
		if got := len(inline) == 1; got != tc.inline {
			t.Errorf("case %d %v: inline = %v, want %v", i, tc.c, got, tc.inline)
		}
	}
}
