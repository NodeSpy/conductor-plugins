package githubkit

import (
	"context"
	"strings"
	"testing"
)

func deleteBranch(t *testing.T, kit *Client, opts map[string]any) (map[string]any, error) {
	t.Helper()
	o := map[string]any{"repo": "acme/w"}
	for k, v := range opts {
		o[k] = v
	}
	return kit.Invoke(context.Background(), "delete_branch", o)
}

func TestDeleteBranchDeletesAMergedHead(t *testing.T) {
	f, kit, _ := world(t)
	head := f.CommitFiles("acme/w", "feat", "me", "work", map[string]string{"a.txt": "a\n"})
	out, err := deleteBranch(t, kit, map[string]any{"branch": "feat", "expect_sha": head})
	if err != nil || out["deleted"] != true || out["status"] != "deleted" {
		t.Fatalf("out = %v, err = %v", out, err)
	}
	if f.BranchSHA("acme/w", "feat") != "" {
		t.Fatal("the branch is still there")
	}
	// Again: already gone counts as done.
	out, err = deleteBranch(t, kit, map[string]any{"branch": "feat"})
	if err != nil || out["deleted"] != false || out["status"] != "gone" {
		t.Fatalf("second delete: out = %v, err = %v", out, err)
	}
}

func TestDeleteBranchLeavesABranchThatMoved(t *testing.T) {
	f, kit, _ := world(t)
	merged := f.CommitFiles("acme/w", "feat", "me", "work", map[string]string{"a.txt": "a\n"})
	moved := f.CommitFiles("acme/w", "feat", "rev", "more", map[string]string{"b.txt": "b\n"})
	out, err := deleteBranch(t, kit, map[string]any{"branch": "feat", "expect_sha": merged})
	if err != nil || out["deleted"] != false || out["status"] != "moved" || !strings.Contains(out["reason"].(string), moved) {
		t.Fatalf("out = %v, err = %v", out, err)
	}
	if f.BranchSHA("acme/w", "feat") != moved {
		t.Fatal("a branch that moved was deleted")
	}
}

func TestDeleteBranchRefusesTheDefaultAndProtectedBranches(t *testing.T) {
	f, kit, _ := world(t)
	if _, err := deleteBranch(t, kit, map[string]any{"branch": "main"}); err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("default branch: err = %v", err)
	}
	f.CommitFiles("acme/w", "release", "me", "r", map[string]string{"r.txt": "r\n"})
	f.ProtectBranch("acme/w", "release")
	if _, err := deleteBranch(t, kit, map[string]any{"branch": "release"}); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected branch: err = %v", err)
	}
	if f.BranchSHA("acme/w", "main") == "" || f.BranchSHA("acme/w", "release") == "" {
		t.Fatal("a refused branch was deleted")
	}
	if _, err := deleteBranch(t, kit, map[string]any{"branch": "feat", "as": "bot"}); err == nil {
		t.Fatal("as: bot accepted")
	}
	if _, err := deleteBranch(t, kit, map[string]any{}); err == nil {
		t.Fatal("no branch accepted")
	}
}
