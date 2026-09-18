package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// stubPaseo writes a paseo stand-in that prints wsJSON for `workspace ls --json`.
func stubPaseo(t *testing.T, wsJSON string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub is a bash script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "paseo")
	script := "#!/usr/bin/env bash\n" +
		"if [ \"$1\" = workspace ] && [ \"$2\" = ls ]; then cat <<'EOF'\n" + wsJSON + "\nEOF\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// worktreeOnBranch is the plugin's create-or-reuse recognition: paseo names a
// branch-off workspace after its branch, so an exact name match on a worktree
// workspace is a reuse. A local workspace on the same name must NOT match.
func TestWorktreeOnBranch(t *testing.T) {
	bin := stubPaseo(t, `[
      {"workspaceId":"wks_local","name":"conductor/comment-7","isolation":"local","cwd":"/x"},
      {"workspaceId":"wks_hit","name":"conductor/comment-7","isolation":"worktree","cwd":"/wt/7"},
      {"workspaceId":"wks_other","name":"conductor/comment-9","isolation":"worktree","cwd":"/wt/9"}
    ]`)

	id, cwd, ok := worktreeOnBranch(bin, "conductor/comment-7")
	if !ok || id != "wks_hit" || cwd != "/wt/7" {
		t.Fatalf("match: ok=%v id=%q cwd=%q (want wks_hit /wt/7)", ok, id, cwd)
	}

	if _, _, ok := worktreeOnBranch(bin, "conductor/comment-404"); ok {
		t.Fatal("no worktree on that branch must not match")
	}
	if _, _, ok := worktreeOnBranch(bin, ""); ok {
		t.Fatal("empty branch must never match")
	}
}
