package ghsource

import (
	"regexp"
	"strconv"
	"strings"
)

// Revert detection (#36 §18), from the signals GitHub itself writes: the
// web-UI "Revert" button opens a PR titled `Revert "…"` whose body carries
// `Reverts owner/repo#N`, and `git revert` commits keep the `Revert "…"`
// title convention. We only trust an explicit same-repo `Reverts` reference
// on a revert-titled PR — a "#N" mention alone proves nothing.

// revertsRe matches GitHub's auto-written back-reference.
var revertsRe = regexp.MustCompile(`(?mi)^\s*Reverts\s+([\w.-]+/[\w.-]+)#(\d+)\b`)

// revertCommitRe matches the trailer `git revert` (and GitHub's Revert
// button) writes into the revert commit's message. Title and body are
// attacker-editable text on any PR; a commit message is part of the merged
// git history — the corroboration the outcome loop requires before a
// "revert" is allowed to count against an agent or rot a workflow
// (#36 review M9).
var revertCommitRe = regexp.MustCompile(`(?mi)^This reverts commit [0-9a-f]{7,40}\b`)

// corroboratesRevert reports whether any of the PR's commit messages carries
// a real revert trailer.
func corroboratesRevert(messages []string) bool {
	for _, m := range messages {
		if revertCommitRe.MatchString(m) {
			return true
		}
	}
	return false
}

// revertRefs returns the PR numbers of repo that a merged revert PR reverts
// (nil when the PR isn't a revert, or reverts another repo's PRs).
func revertRefs(repo string, pr *prPayload) []int {
	if pr == nil || !pr.Merged || !strings.HasPrefix(strings.TrimSpace(pr.Title), "Revert") {
		return nil
	}
	var out []int
	for _, m := range revertsRe.FindAllStringSubmatch(pr.Body, -1) {
		if !strings.EqualFold(m[1], repo) {
			continue // a cross-repo revert isn't this repo's outcome signal
		}
		if n, err := strconv.Atoi(m[2]); err == nil && n > 0 && n != pr.Number {
			out = append(out, n)
		}
	}
	return out
}
