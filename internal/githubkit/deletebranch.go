package githubkit

import (
	"context"
	"fmt"
	"strings"
)

// deleteBranch deletes a branch as the write identity: what a merged PR leaves
// behind. It refuses the repo's default branch and a protected branch, and,
// given expect_sha, a branch whose head is no longer that commit (someone
// pushed to it since). A branch that is already gone counts as done.
//
// → deleted (this call removed it), status: deleted | gone | moved, reason.
func (c *Client) deleteBranch(ctx context.Context, tok, base, repo string, opts map[string]any) (map[string]any, error) {
	branch, _ := opts["branch"].(string)
	branch = strings.TrimPrefix(branch, "refs/heads/")
	if branch == "" {
		return nil, fmt.Errorf("github.delete_branch: options.branch is required")
	}
	expect, _ := opts["expect_sha"].(string)
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s", base, repo), &r); err != nil {
		return nil, fmt.Errorf("github.delete_branch: %w", err)
	}
	if r.DefaultBranch == "" || branch == r.DefaultBranch {
		return nil, fmt.Errorf("github.delete_branch: %s is %s's default branch; refusing to delete it", branch, repo)
	}
	var b struct {
		Protected bool `json:"protected"`
		Commit    struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/branches/%s", base, repo, escapePath(branch)), &b); err != nil {
		if isNotFound(err) {
			return map[string]any{"deleted": false, "status": "gone", "reason": "the branch does not exist"}, nil
		}
		return nil, fmt.Errorf("github.delete_branch: %w", err)
	}
	if b.Protected {
		return nil, fmt.Errorf("github.delete_branch: %s is protected; refusing to delete it", branch)
	}
	if expect != "" && !strings.EqualFold(b.Commit.SHA, expect) {
		return map[string]any{"deleted": false, "status": "moved",
			"reason": fmt.Sprintf("the branch moved: its head is %s, not %s", b.Commit.SHA, expect)}, nil
	}
	if err := c.del(ctx, tok, fmt.Sprintf("%s/repos/%s/git/refs/heads/%s", base, repo, escapePath(branch)), nil); err != nil {
		if strings.Contains(err.Error(), "Reference does not exist") {
			return map[string]any{"deleted": false, "status": "gone", "reason": "the branch does not exist"}, nil
		}
		return nil, fmt.Errorf("github.delete_branch: %w", err)
	}
	return map[string]any{"deleted": true, "status": "deleted", "reason": ""}, nil
}
