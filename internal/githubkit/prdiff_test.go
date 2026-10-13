package githubkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// prDiffServer serves PR 7's whole diff with status diffStatus (and body
// diffBody), and its per-file patches across pages of 100.
func prDiffServer(t *testing.T, diffStatus int, diffBody string, files []map[string]any) (*httptest.Server, *int) {
	t.Helper()
	filePages := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/w/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(diffStatus)
		_, _ = w.Write([]byte(diffBody))
	})
	mux.HandleFunc("/repos/acme/w/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
		filePages++
		var page int
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		lo, hi := (page-1)*100, page*100
		if lo > len(files) {
			lo = len(files)
		}
		if hi > len(files) {
			hi = len(files)
		}
		_ = json.NewEncoder(w).Encode(files[lo:hi])
	})
	mux.HandleFunc("/repos/acme/w", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"full_name":"acme/w"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &filePages
}

const tooLarge = `{"message":"Sorry, the diff exceeded the maximum number of lines (20000)","errors":[{"resource":"PullRequest","field":"diff","code":"too_large"}]}`

func TestPRDiffTooLargeRebuildsFromFilePatches(t *testing.T) {
	files := []map[string]any{
		{"filename": "main.go", "status": "modified", "patch": "@@ -1 +1 @@\n-a\n+b"},
		{"filename": "new.go", "status": "added", "patch": "@@ -0,0 +1 @@\n+x"},
		{"filename": "old.go", "status": "removed", "patch": "@@ -1 +0,0 @@\n-y"},
		{"filename": "to.go", "previous_filename": "from.go", "status": "renamed", "patch": "@@ -1 +1 @@\n-p\n+q"},
		{"filename": "logo.png", "status": "added"},
	}
	// Pad past one page so the rebuild has to paginate.
	for i := 0; i < 100; i++ {
		files = append(files, map[string]any{"filename": fmt.Sprintf("gen/%03d.go", i), "status": "added", "patch": "@@ -0,0 +1 @@\n+g"})
	}
	ts, pages := prDiffServer(t, http.StatusNotAcceptable, tooLarge, files)
	kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	out, err := kit.Invoke(context.Background(), "pr_diff", map[string]any{"repo": "acme/w", "pr": 7})
	if err != nil {
		t.Fatalf("a too-large diff must be rebuilt, not fail: %v", err)
	}
	diff, _ := out["diff"].(string)
	for _, want := range []string{
		"diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-a\n+b\n",
		"diff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n",
		"diff --git a/old.go b/old.go\ndeleted file mode 100644\n--- a/old.go\n+++ /dev/null\n",
		"diff --git a/from.go b/to.go\nrename from from.go\nrename to to.go\n--- a/from.go\n+++ b/to.go\n",
		"diff --git a/logo.png b/logo.png\nnew file mode 100644\n# GitHub sent no patch for this file",
		"diff --git a/gen/099.go b/gen/099.go\n",
	} {
		if !strings.Contains(diff, want) {
			t.Fatalf("rebuilt diff is missing %q:\n%s", want, diff[:min(len(diff), 600)])
		}
	}
	if *pages != 2 {
		t.Fatalf("want 2 pages of files fetched, got %d", *pages)
	}
}

func TestPRDiffUnderTheLimitIsUnchanged(t *testing.T) {
	whole := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-1\n+2\n"
	ts, pages := prDiffServer(t, http.StatusOK, whole, nil)
	kit, _ := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	out, err := kit.Invoke(context.Background(), "pr_diff", map[string]any{"repo": "acme/w", "pr": 7})
	if err != nil {
		t.Fatal(err)
	}
	if out["diff"] != whole || *pages != 0 {
		t.Fatalf("a served diff must pass through untouched with no file listing (pages=%d): %q", *pages, out["diff"])
	}
}

// Only GitHub's size refusal falls back. Any other failure surfaces as
// before (here a 500, which stays a retryable upstream error).
func TestPRDiffOtherErrorsDoNotFallBack(t *testing.T) {
	ts, pages := prDiffServer(t, http.StatusInternalServerError, `{"message":"boom"}`, nil)
	kit, _ := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	_, err := kit.Invoke(context.Background(), "pr_diff", map[string]any{"repo": "acme/w", "pr": 7})
	var pe *plugin.Error
	if !errors.As(err, &pe) || pe.Code != plugin.CodeUpstream || pe.Data["retryable"] != true {
		t.Fatalf("want a retryable upstream error, got %v", err)
	}
	if *pages != 0 {
		t.Fatalf("a 500 must not trigger the per-file rebuild (pages=%d)", *pages)
	}
}
