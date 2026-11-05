package githubkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghfake"
)

const applyBase = "package main\n\nfunc a() int {\n\treturn 1\n}\n\nfunc b() int {\n\treturn 2\n}\n"

// fixDiff changes a()'s return, the agent's work on the PR branch.
const fixDiff = `# uncommitted (vs HEAD)
diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -2,5 +2,5 @@
 
 func a() int {
-	return 1
+	return 10
 }
 
`

func applyWorld(t *testing.T) (*ghfake.Fake, *Client, string) {
	t.Helper()
	f, kit, _ := world(t)
	base := f.CommitFiles("acme/w", "feat", "me", "base", map[string]string{"main.go": applyBase, "README.md": "hi\n"})
	return f, kit, base
}

func applyDiff(t *testing.T, kit *Client, opts map[string]any) map[string]any {
	t.Helper()
	o := map[string]any{"repo": "acme/w", "branch": "feat", "message": "the agent's fix"}
	for k, v := range opts {
		o[k] = v
	}
	out, err := kit.Invoke(context.Background(), "apply_diff", o)
	if err != nil {
		t.Fatalf("apply_diff: %v", err)
	}
	return out
}

func TestApplyDiffOntoAMovedHead(t *testing.T) {
	f, kit, base := applyWorld(t)
	// Someone else pushed b()'s change while the agent worked.
	moved := f.CommitFiles("acme/w", "feat", "rev", "theirs", map[string]string{"main.go": strings.Replace(applyBase, "return 2", "return 20", 1)})
	out := applyDiff(t, kit, map[string]any{"diff": fixDiff, "base": base})
	if out["pushed"] != true || out["moved"] != true || out["onto"] != moved || out["files"] != 1 || out["reason"] != "" {
		t.Fatalf("out = %v", out)
	}
	files, head := f.FilesAt("acme/w", "feat")
	if head != out["head"] || head == moved {
		t.Fatalf("head %s, out %v", head, out)
	}
	want := strings.NewReplacer("return 1", "return 10", "return 2", "return 20").Replace(applyBase)
	if files["main.go"] != want || files["README.md"] != "hi\n" {
		t.Fatalf("files = %q", files)
	}
	if a := f.CommitAuthor("acme/w", head); a != "me" {
		t.Fatalf("the commit is %q's, want the write identity's", a)
	}
}

func TestApplyDiffUnmovedHead(t *testing.T) {
	_, kit, base := applyWorld(t)
	out := applyDiff(t, kit, map[string]any{"diff": fixDiff, "base": base})
	if out["pushed"] != true || out["moved"] != false {
		t.Fatalf("out = %v", out)
	}
}

func TestApplyDiffConflictCommitsNothing(t *testing.T) {
	f, kit, base := applyWorld(t)
	moved := f.CommitFiles("acme/w", "feat", "rev", "theirs", map[string]string{"main.go": strings.Replace(applyBase, "return 1", "return 99", 1)})
	out := applyDiff(t, kit, map[string]any{"diff": fixDiff, "base": base})
	if out["pushed"] != false || !reflect.DeepEqual(out["conflicts"], []any{"main.go"}) || out["reason"] == "" {
		t.Fatalf("out = %v", out)
	}
	if _, head := f.FilesAt("acme/w", "feat"); head != moved {
		t.Fatal("a conflict must commit nothing")
	}
}

func TestApplyDiffAlreadyApplied(t *testing.T) {
	f, kit, _ := applyWorld(t)
	same := f.CommitFiles("acme/w", "feat", "rev", "same fix", map[string]string{"main.go": strings.Replace(applyBase, "return 1", "return 10", 1)})
	out := applyDiff(t, kit, map[string]any{"diff": fixDiff})
	if out["pushed"] != false || out["head"] != same || len(out["conflicts"].([]any)) != 0 || !strings.Contains(out["reason"].(string), "already") {
		t.Fatalf("out = %v", out)
	}
}

func TestApplyDiffRejectedWhenTheBranchMovesAgain(t *testing.T) {
	f, _, _ := applyWorld(t)
	target, _ := url.Parse(f.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	var again string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/git/refs/") && again == "" {
			again = f.CommitFiles("acme/w", "feat", "rev", "racing", map[string]string{"other.txt": "x\n"})
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	kit, err := NewClient(Config{WriteToken: "tok-me", APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	out := applyDiff(t, kit, map[string]any{"diff": fixDiff})
	if out["pushed"] != false || out["rejected"] != true {
		t.Fatalf("out = %v", out)
	}
	if _, head := f.FilesAt("acme/w", "feat"); head != again {
		t.Fatal("a rejected update must leave the branch where the racer put it")
	}
}

func TestApplyDiffRefusesPaths(t *testing.T) {
	f, kit, base := applyWorld(t)
	diff := fixDiff + `diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml
new file mode 100644
--- /dev/null
+++ b/.github/workflows/ci.yml
@@ -0,0 +1 @@
+on: push
`
	out := applyDiff(t, kit, map[string]any{"diff": diff, "refuse_paths": []any{".github/workflows/", "Jenkinsfile"}})
	if out["pushed"] != false || !reflect.DeepEqual(out["refused"], []any{".github/workflows/ci.yml"}) {
		t.Fatalf("out = %v", out)
	}
	if _, head := f.FilesAt("acme/w", "feat"); head != base {
		t.Fatal("a refused diff must commit nothing")
	}
}

func TestApplyDiffNewDeleteRename(t *testing.T) {
	f, kit, _ := applyWorld(t)
	diff := `# committed, not pushed (vs base)
diff --git a/new.txt b/new.txt
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+one
+two
diff --git a/README.md b/README.md
deleted file mode 100644
index 1111111..0000000
--- a/README.md
+++ /dev/null
@@ -1 +0,0 @@
-hi
diff --git a/main.go b/cmd.go
similarity 90%
rename from main.go
rename to cmd.go
index 1111111..2222222 100644
--- a/main.go
+++ b/cmd.go
@@ -8,3 +8,3 @@ func a() int {
 func b() int {
-	return 2
+	return 3
 }
diff --git a/new.txt b/new.txt
index 1111111..3333333 100644
--- a/new.txt
+++ b/new.txt
@@ -2 +2 @@
-two
\ No newline at end of file
+three
\ No newline at end of file
`
	out := applyDiff(t, kit, map[string]any{"diff": diff})
	if out["pushed"] != true || out["files"] != 4 {
		t.Fatalf("out = %v", out)
	}
	files, _ := f.FilesAt("acme/w", "feat")
	want := map[string]string{"new.txt": "one\nthree", "cmd.go": strings.Replace(applyBase, "return 2", "return 3", 1)}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %q", files)
	}
}

func TestApplyDiffRefusesBadInput(t *testing.T) {
	_, kit, _ := applyWorld(t)
	cut := strings.TrimSuffix(fixDiff, " }\n \n") + "\n… (diff clipped)"
	for name, opts := range map[string]map[string]any{
		"truncated": {"diff": cut},
		"escape":    {"diff": strings.ReplaceAll(fixDiff, "main.go", "../main.go")},
		"dot-git":   {"diff": strings.ReplaceAll(fixDiff, "main.go", ".git/config")},
		"binary":    {"diff": "diff --git a/x.png b/x.png\nBinary files a/x.png and b/x.png differ\n"},
		"bot":       {"diff": fixDiff, "as": "bot"},
	} {
		o := map[string]any{"repo": "acme/w", "branch": "feat", "message": "m"}
		for k, v := range opts {
			o[k] = v
		}
		if _, err := kit.Invoke(context.Background(), "apply_diff", o); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
