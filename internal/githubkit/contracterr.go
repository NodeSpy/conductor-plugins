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
// by number doesn't exist (or no longer does). data.target is set to key —
// the SAME string ghplugin's event semantics render for this target
// ("{{.repo}}#{{.number}}", ghplugin/semantics.go's prTarget) — so the host
// can require an exact match before honoring it: a target_gone naming the
// wrong target is worse than no target_gone at all.
//
// GitHub answers the identical 404 when the calling token simply can't SEE
// the repo, which would otherwise be a false target_gone in the worst way:
// silently ending a live run over a credentials/visibility problem rather
// than a dead target. Before trusting a 404 this confirms the repo itself is
// visible first (repoVisible, through the Client's normal short-TTL GET
// cache — one extra request amortized across an entire dead target's worth
// of retries); only a visible repo with a missing PR/issue is target_gone. A
// repo that itself 404s/403s answers upstream{status, retryable:false}
// instead, saying plainly that the token can't see it (or it's gone) — never
// target_gone, which would stop the run as if ITS own target had closed. A
// 410 Gone is unambiguous (GitHub never answers 410 for a merely-invisible
// repo) and skips the check.
//
// Call it only where the URL's own identity IS the PR/issue number — never
// a nested sub-resource that can 404 on its own account (a review comment
// id, a review id, a label name): remapping those would misreport "the
// target is gone" for a target very much still there. remapGoneIfMissing is
// the variant for the one class of sub-resource where GitHub's own message
// tells the two cases apart.
func (c *Client) remapTargetGone(ctx context.Context, tok, repo, key string, err error) error {
	return c.remapGoneIfMissing(ctx, tok, repo, key, err)
}

// remapGoneIfMissing is remapTargetGone, but skips the remap when the
// upstream's own message names a sub-resource rather than the issue/PR
// itself (e.g. remove_label/add_labels's "Label does not exist" — the issue
// is still there, just without/missing that label).
func (c *Client) remapGoneIfMissing(ctx context.Context, tok, repo, key string, err error, notTargetMessages ...string) error {
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
	if status == http.StatusNotFound {
		visible, rateLimited := c.repoVisible(ctx, tok, repo)
		if rateLimited != nil {
			// The visibility check itself hit a rate limit: propagate that
			// rather than guess at visibility from no answer at all.
			return rateLimited
		}
		if !visible {
			return plugin.Fail(plugin.CodeUpstream,
				fmt.Sprintf("github: can't see repo %s (no access, or the repo itself is gone)", repo),
				map[string]any{"status": http.StatusNotFound, "retryable": false})
		}
	}
	return plugin.Fail(plugin.CodeTargetGone, pe.Message, map[string]any{"target": key})
}

// repoVisible reports whether GET /repos/{repo} answers with any 2xx for
// tok, through the Client's normal short-TTL GET cache (client.go,
// DefaultCacheTTL) — so a burst of 404s against one dead target (a flow's
// several verbs, a step's own retries) costs at most one extra request.
// rateLimited is non-nil when the check itself hit a rate limit, so the
// caller can surface that instead of guessing at visibility.
func (c *Client) repoVisible(ctx context.Context, tok, repo string) (ok bool, rateLimited error) {
	err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s", c.base(), repo), nil)
	if err == nil {
		return true, nil
	}
	var pe *plugin.Error
	if errors.As(err, &pe) && pe.Code == plugin.CodeRateLimited {
		return false, pe
	}
	return false, nil
}

// checkMergeReady reads the PR fresh right before a merge attempt and
// answers the contract code when the merge can't even be attempted yet:
// target_gone when it's already merged or closed (merge_pr needs it open),
// or not_ready when GitHub hasn't computed mergeability yet — null right
// after the head or base moves, per GitHub's own documented behavior, which
// the merge endpoint itself would otherwise answer with the same ambiguous
// 405 "Pull Request is not mergeable" as a real conflict. nil when the merge
// may proceed; the caller still handles the PUT's own failure defensively
// (remapMergeFailure), for the race where the PR goes away, merges, or
// closes between this read and the attempt.
func (c *Client) checkMergeReady(ctx context.Context, tok, base, repo, key string, number int) error {
	var pr struct {
		State     string `json:"state"`
		Merged    bool   `json:"merged"`
		Mergeable *bool  `json:"mergeable"`
	}
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", base, repo, number), &pr); err != nil {
		return c.remapTargetGone(ctx, tok, repo, key, err)
	}
	switch {
	case pr.Merged:
		return plugin.Fail(plugin.CodeTargetGone, fmt.Sprintf("github.merge_pr: #%d is already merged", number), map[string]any{"target": key})
	case pr.State == "closed":
		return plugin.Fail(plugin.CodeTargetGone, fmt.Sprintf("github.merge_pr: #%d is closed", number), map[string]any{"target": key})
	case pr.Mergeable == nil:
		return plugin.Fail(plugin.CodeNotReady, fmt.Sprintf("github.merge_pr: #%d's mergeability is not computed yet", number), nil)
	}
	return nil
}

// remapMergeFailure classifies the merge PUT's own failure, after
// checkMergeReady already confirmed — a moment earlier — that the PR was
// open with mergeability computed. A 404/410 is the ordinary target-gone
// remap (the PR vanished in the gap). A 405 is GitHub's "Pull Request is not
// mergeable": the same answer a real merge conflict gets, but ALSO what a PR
// that merged or closed in that same narrow gap answers (mergeability can go
// stale the instant the head or base moves again) — re-reading the PR fresh
// tells the two apart: merged/closed now is target_gone; anything else
// leaves the 405 standing, a real conflict rather than a vanished target.
func (c *Client) remapMergeFailure(ctx context.Context, tok, base, repo string, number int, key string, err error) error {
	var pe *plugin.Error
	if !errors.As(err, &pe) || pe.Code != plugin.CodeUpstream {
		return err
	}
	status, _ := pe.Data["status"].(int)
	switch status {
	case http.StatusNotFound, http.StatusGone:
		return c.remapTargetGone(ctx, tok, repo, key, err)
	case http.StatusMethodNotAllowed:
		var pr struct {
			State  string `json:"state"`
			Merged bool   `json:"merged"`
		}
		if gerr := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", base, repo, number), &pr); gerr != nil {
			return err // couldn't confirm either way: report the original 405
		}
		switch {
		case pr.Merged:
			return plugin.Fail(plugin.CodeTargetGone, fmt.Sprintf("github.merge_pr: #%d is already merged", number), map[string]any{"target": key})
		case pr.State == "closed":
			return plugin.Fail(plugin.CodeTargetGone, fmt.Sprintf("github.merge_pr: #%d is closed", number), map[string]any{"target": key})
		}
	}
	return err
}
