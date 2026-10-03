package githubkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// remapTargetGone turns a target-keyed call's 404/410 upstream answer into
// target_gone (plugin-contract.md §1.11): the PR or issue the URL addresses
// by number doesn't exist (or no longer does). Call it only where the URL's
// own identity IS the PR/issue number — never a nested sub-resource that can
// 404 on its own account (a review comment id, a label name): remapping
// those would misreport "the target is gone" for a target very much still
// there. remapGoneIfMissing is the variant for the one class of sub-resource
// where GitHub's own message tells the two cases apart.
func remapTargetGone(err error) error {
	return remapGoneIfMissing(err)
}

// remapGoneIfMissing is remapTargetGone, but skips the remap when the
// upstream's own message names a sub-resource rather than the issue/PR
// itself (e.g. remove_label's "Label does not exist" — the issue is still
// there, just without that label).
func remapGoneIfMissing(err error, notTargetMessages ...string) error {
	var pe *plugin.Error
	if !errors.As(err, &pe) || pe.Code != plugin.CodeUpstream {
		return err
	}
	status, _ := pe.Data["status"].(int)
	if status != http.StatusNotFound && status != http.StatusGone {
		return err
	}
	for _, m := range notTargetMessages {
		if m != "" && strings.Contains(pe.Message, m) {
			return err
		}
	}
	return plugin.Fail(plugin.CodeTargetGone, pe.Message, nil)
}

// checkMergeReady reads the PR fresh right before a merge attempt and
// answers the contract code when the merge can't even be attempted yet:
// target_gone when it's already merged or closed (merge_pr needs it open),
// or not_ready when GitHub hasn't computed mergeability yet — null right
// after the head or base moves, per GitHub's own documented behavior, which
// the merge endpoint itself would otherwise answer with the same ambiguous
// 405 "Pull Request is not mergeable" as a real conflict. nil when the merge
// may proceed; the caller still remaps the PUT's own 404/410 defensively,
// for the race where the PR goes away between this read and the attempt.
func (c *Client) checkMergeReady(ctx context.Context, tok, base, repo string, number int) error {
	var pr struct {
		State     string `json:"state"`
		Merged    bool   `json:"merged"`
		Mergeable *bool  `json:"mergeable"`
	}
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", base, repo, number), &pr); err != nil {
		return remapTargetGone(err)
	}
	switch {
	case pr.Merged:
		return plugin.Fail(plugin.CodeTargetGone, fmt.Sprintf("github.merge_pr: #%d is already merged", number), nil)
	case pr.State == "closed":
		return plugin.Fail(plugin.CodeTargetGone, fmt.Sprintf("github.merge_pr: #%d is closed", number), nil)
	case pr.Mergeable == nil:
		return plugin.Fail(plugin.CodeNotReady, fmt.Sprintf("github.merge_pr: #%d's mergeability is not computed yet", number), nil)
	}
	return nil
}
