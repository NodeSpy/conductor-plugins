package githubkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// targetKey builds a target_gone's data.target the SAME way
// ghplugin/semantics.go declares the key for the event this verb's call
// responds to, so the host's exact-match requirement (see remapTargetGone)
// can actually be satisfied: a PR/issue-shaped target — prTarget's default —
// is declared "{{.repo}}#{{.number}}"; a repo-level target with no PR/issue
// number of its own (release, deployment_status, alerts — eventSemantics'
// default case overrides s.Target.Key to "{{.repo}}" alone) is declared just
// the repo. number == 0 means exactly that second case, so it must render as
// the bare repo — never a literal "repo#0", a string that templates to
// nothing the host would ever compare it against and so could never be
// honored as target_gone.
func targetKey(repo string, number int) string {
	if number == 0 {
		return repo
	}
	return fmt.Sprintf("%s#%d", repo, number)
}

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
// visible first (repoVisible, a FRESH read that bypasses the Client's
// short-TTL GET cache — a cached "visible" from moments ago would mask a
// token that just lost access, which is exactly the race this check exists
// to catch; the real cost is one extra fresh GET per target 404, every time,
// never amortized across a target's retries); only a visible repo with a
// missing PR/issue is target_gone. A repo that itself 404s/403s answers
// upstream{status, retryable:false} instead, saying plainly that the token
// can't see it (or it's gone) — never target_gone, which would stop the run
// as if ITS own target had closed. A transient failure of the visibility
// check itself (a rate limit, a 5xx, a network blip) is never read as "not
// visible" either — see repoVisible. A 410 Gone is unambiguous (GitHub never
// answers 410 for a merely-invisible repo) and skips the check.
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
		visible, verr := c.repoVisible(ctx, tok, repo)
		if verr != nil {
			// The visibility check itself couldn't confirm anything — a
			// rate limit, a 5xx, a network blip. NEVER read that as "not
			// visible": that would turn a blip in this one extra call into
			// exactly the false non-retryable 404 this whole check exists
			// to prevent. Surface the check's own classification when
			// GitHub's answer to IT gives one worth keeping (rate_limited
			// always propagates; a 5xx on the repo GET is already a
			// retryable upstream) — otherwise fall back to the ORIGINAL
			// error (the real 404 GitHub sent for the PR/issue itself)
			// rather than invent a brand-new verdict out of a check that
			// answered nothing. Either way, never a manufactured
			// non-retryable answer born of an inconclusive check.
			var vpe *plugin.Error
			if errors.As(verr, &vpe) {
				switch vpe.Code {
				case plugin.CodeRateLimited:
					return verr
				case plugin.CodeUpstream:
					if retryable, _ := vpe.Data["retryable"].(bool); retryable {
						return verr
					}
				}
			}
			return err
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
// tok — a FRESH read (getFresh, bypassing the Client's short-TTL GET cache):
// a cached "visible" up to CacheTTL old could mask a token that lost access
// moments ago, which is exactly the false-target_gone race this check exists
// to catch. The real cost is one extra fresh GET per target 404, every call
// (never amortized across a target's own retries — see remapTargetGone).
//
// Three outcomes:
//   - the repo IS visible (ok=true, err=nil): the caller's 404 is the
//     PR/issue itself going away — target_gone.
//   - the repo is DEFINITELY NOT visible (ok=false, err=nil): the repo GET
//     itself answered 404 or 403 — the token can't see it, or it's gone.
//   - the check ITSELF failed inconclusively (ok=false, err!=nil): a rate
//     limit, a 5xx, or an unclassified network error on the repo GET — no
//     answer either way. err is the check's own error (already a
//     rate_limited or a retryable upstream when GitHub's response allows
//     that classification); the caller must never treat this as "not
//     visible".
func (c *Client) repoVisible(ctx context.Context, tok, repo string) (ok bool, err error) {
	gerr := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s", c.base(), repo), nil)
	if gerr == nil {
		return true, nil
	}
	var pe *plugin.Error
	if errors.As(gerr, &pe) && pe.Code == plugin.CodeUpstream {
		if status, _ := pe.Data["status"].(int); status == http.StatusNotFound || status == http.StatusForbidden {
			return false, nil // definitely not visible
		}
	}
	// A rate limit, a 5xx (already a retryable upstream), or a raw,
	// unclassified network error (a dial failure, a context deadline): the
	// check itself is inconclusive either way.
	return false, gerr
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
