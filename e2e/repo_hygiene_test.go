package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot returns this checkout's top level, so these checks work
// regardless of which directory `go test` happens to run from.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// isExecutableMagic reports whether b opens with a recognized compiled-binary
// magic number: ELF (Linux), Mach-O (macOS, 32/64-bit, either endianness), or
// the PE/COFF "MZ" header (Windows) — the three shapes `go build` can produce
// for this repo's plugins depending on GOOS.
func isExecutableMagic(b []byte) bool {
	if len(b) >= 4 && string(b[:4]) == "\x7fELF" {
		return true
	}
	if len(b) >= 4 {
		switch string(b[:4]) {
		case "\xfe\xed\xfa\xce", "\xfe\xed\xfa\xcf", "\xcf\xfa\xed\xfe", "\xce\xfa\xed\xfe":
			return true // Mach-O
		}
	}
	if len(b) >= 2 && string(b[:2]) == "MZ" {
		return true // PE/COFF
	}
	return false
}

// TestNoTrackedBuildArtifactsAtRepoRoot is the regression test for round-2
// finding #6: aws-sqs, discord and github were each a compiled plugin binary
// (9-10MB) committed at the repo root — the result of running
// `go build ./connectors/<name>/...` from the repo root (which drops a
// binary named after the plugin directly there) and then `git add`ing it by
// accident. No file tracked directly at the repo root may be a compiled
// binary.
func TestNoTrackedBuildArtifactsAtRepoRoot(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if name == "" || strings.Contains(name, "/") {
			continue // only files directly at the repo root
		}
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read tracked root file %q: %v", name, err)
		}
		if isExecutableMagic(b) {
			t.Fatalf("tracked root file %q is a compiled binary (%d bytes) — plugin binaries must never be committed at the repo root", name, len(b))
		}
	}
}

// TestRootGitignoreCatchesStrayPluginBinary proves the repo-root .gitignore
// guard added alongside the finding #6 cleanup actually works: a stray file
// at the repo root — exactly what a `go build ./connectors/<name>/...` run
// from there produces — must be ignored by git regardless of what it's
// named, so it can never again be `git add`ed by accident.
func TestRootGitignoreCatchesStrayPluginBinary(t *testing.T) {
	root := repoRoot(t)
	const probeName = "e2e-hygiene-probe-binary"
	probe := filepath.Join(root, probeName)
	if err := os.WriteFile(probe, []byte("not a real binary, just a probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(probe) })

	out, err := exec.Command("git", "-C", root, "status", "--porcelain", "--ignored", probeName).Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "!!") {
		t.Fatalf("a stray file at the repo root is not ignored by .gitignore (git status: %q) — it would be addable by accident, same as aws-sqs/discord/github were", out)
	}
}
