package ghsource

import (
	"context"
	"fmt"
)

// Force builds and emits trigger(s) for `kind` on repo#number on demand (the
// `force` command). It bypasses the usual applicability filters (reviewer match,
// draft gate, exclude, comment_author) and — via Trigger.Force — the engine's
// dedup/liveness/backoff gates, so the action runs now even if conductor thinks
// the state is already handled.
//
// It fetches the PR to fill the target + tokens, then emits every enabled variant
// of the kind's configured action. Kinds that need event-specific context (a
// comment body, a CI run id) get empty values — force is aimed at PR-state kinds
// like review_requested / merge_conflict / pr_behind / self_review / merge_ready.
func (g *Source) Force(ctx context.Context, kind, repo string, number int, emit EmitFunc) (int, error) {
	if err := g.ensureClients(); err != nil {
		return 0, err
	}
	if len(g.actionsFor(repo, kind)) == 0 {
		return 0, fmt.Errorf("no %q action configured for %s", kind, repo)
	}
	owner, name := splitRepo(repo)
	if owner == "" || name == "" {
		return 0, fmt.Errorf("bad repo %q (want owner/name)", repo)
	}
	instID, err := g.app.repoInstallationID(ctx, owner, name)
	if err != nil {
		return 0, fmt.Errorf("installation for %s: %w", repo, err)
	}
	info, err := g.rest.pull(ctx, instID, owner, name, number)
	if err != nil {
		return 0, fmt.Errorf("fetch %s#%d: %w", repo, number, err)
	}
	t := g.target(repo, number, info.Head.SHA, info.Base.Ref, info.HTMLURL)
	labels := make([]string, 0, len(info.Labels))
	for _, l := range info.Labels {
		labels = append(labels, l.Name)
	}
	extra := map[string]any{"author": info.User.Login, "head_ref": info.Head.Ref, "labels": labels}
	if kind == "changes_requested" {
		// On changes_requested `author` is the reviewer (webhook: the review's
		// user; sweep: the unresolved threads' opener) — the login a flow's
		// "{{.author}}" re-requests. The PR author is never it: GitHub 422s a
		// review request to them. Resolve it the way the sweep does.
		delete(extra, "author")
		if threads, err := g.rest.unresolvedThreads(ctx, instID, owner, name, number); err == nil {
			for k, v := range g.threadReviewerFacts(threads) {
				extra[k] = v
			}
		}
	}
	// keep=nil → every enabled variant of the kind, no applicability filter.
	trs := g.emit(repo, kind, t, fmt.Sprintf("force %s on %s#%d", kind, repo, number),
		fmt.Sprintf("force:%s@%s", kind, info.Head.SHA), extra, nil)
	if len(trs) == 0 {
		return 0, fmt.Errorf("%q action for %s is disabled", kind, repo)
	}
	tok, _ := g.app.installationToken(ctx, instID)
	for i := range trs {
		trs[i].Force = true
		trs[i].Context["installation_id"] = instID
		if tok != "" {
			trs[i].Context["app_token"] = tok
		}
		emit(ctx, trs[i])
	}
	return len(trs), nil
}
