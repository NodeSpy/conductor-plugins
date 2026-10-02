package ghsource

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"path"
	"strings"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit"
)

// sweepLoop runs the optional catch-up sweep on an ADAPTIVE cadence. It's off
// unless configured; it exists to reconcile anything missed while the daemon was
// offline or disconnected (webhooks aren't redelivered reliably).
//
// The cadence starts tight (MinInterval) after startup and backs off ×2 toward the
// ceiling (Interval) while nothing disrupts us — so a quiet, connected daemon
// settles at Interval and doesn't sweep for nothing. A signal on `renew` (a smee
// reconnect — a likely dropped-webhook window) resets it to the tight floor and
// sweeps promptly, catching the gap in a minute or two instead of up to a full
// Interval. `renew` may be nil (no smee transport); a nil channel simply never
// fires.
func (g *Source) sweepLoop(ctx context.Context, emit EmitFunc, renew <-chan struct{}) {
	min, max := sweepBounds(g.cfg.Sweep)
	log.Printf("github[%s]: sweep enabled — adaptive %s→%s (webhook carries real-time)", g.name, min, max)
	runSweep := func() {
		if err := g.sweep(ctx, emit); err != nil {
			log.Printf("github[%s]: sweep error: %v", g.name, err)
		}
	}
	if ctx.Err() != nil {
		return
	}
	runSweep() // once on start — reconcile anything missed while offline
	cur := min
	t := time.NewTimer(cur)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-renew:
			cur = min
			log.Printf("github[%s]: sweep cadence reset to %s (renew: reconnect or manual sweep)", g.name, cur)
			runSweep()
			resetTimer(t, cur)
		case <-t.C:
			runSweep()
			cur = backoffInterval(cur, max)
			resetTimer(t, cur)
		}
	}
}

// sweepFloor is the hard minimum for any sweep cadence (adaptive floor or the
// no-webhook fixed interval), so a mistyped `min_interval: 5s` across a whole App
// installation can't flood the API.
const sweepFloor = 1 * time.Minute

// sweepBounds resolves the tight floor and the ceiling from config (defaults: 2m
// floor, 1h ceiling; the floor is clamped to [sweepFloor, ceiling]). Note the
// sweep runs immediately on startup and on a reconnect renewal regardless of the
// floor — the floor only sets the follow-up rhythm.
func sweepBounds(s SweepConfig) (min, max time.Duration) {
	max = s.Interval
	if max <= 0 {
		max = 1 * time.Hour
	}
	min = s.MinInterval
	if min <= 0 {
		min = 2 * time.Minute
	}
	if min < sweepFloor {
		min = sweepFloor
	}
	if min > max {
		min = max
	}
	return min, max
}

// fixedSweepInterval is the no-webhook cadence: the sweep is the sole event source,
// so it polls steadily at min_interval (default 2m, floored) with no backoff.
func fixedSweepInterval(s SweepConfig) time.Duration {
	iv := s.MinInterval
	if iv <= 0 {
		iv = 2 * time.Minute
	}
	if iv < sweepFloor {
		iv = sweepFloor
	}
	return iv
}

// fixedSweepLoop runs the sweep on a FIXED cadence — the mode when no webhook is
// configured, so the sweep IS the event source and must poll predictably rather
// than back off toward a slow ceiling (which would leave events unseen for up to an
// Interval). Mirrors stuckLoop. `renew` (a manual SweepNow) triggers an immediate
// sweep without changing the cadence.
func (g *Source) fixedSweepLoop(ctx context.Context, emit EmitFunc, renew <-chan struct{}) {
	iv := fixedSweepInterval(g.cfg.Sweep)
	log.Printf("github[%s]: sweep enabled — fixed %s (no webhook; the sweep is the event source)", g.name, iv)
	runSweep := func() {
		if err := g.sweep(ctx, emit); err != nil {
			log.Printf("github[%s]: sweep error: %v", g.name, err)
		}
	}
	if ctx.Err() != nil {
		return
	}
	runSweep() // once on start — reconcile anything missed while offline
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-renew:
			runSweep()
		case <-t.C:
			runSweep()
		}
	}
}

// backoffInterval doubles cur, capped at max.
func backoffInterval(cur, max time.Duration) time.Duration {
	cur *= 2
	if cur > max {
		cur = max
	}
	return cur
}

// resetTimer safely rearms a timer to d (draining a pending fire if needed).
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// sweep reconciles the configured repos. Entries may be concrete (`owner/name`)
// or an owner glob (`owner/*`, `owner/svc-*`), which is expanded to the repos
// the App installation can access. It emits `review_requested` for PRs where
// your review is pending, and for PRs you authored re-checks merge state
// (conflict/behind) and outstanding review-comment threads (changes_requested) —
// recovering feedback that no live webhook picked up.
func (g *Source) sweep(ctx context.Context, emit EmitFunc) error {
	st := &sweepStats{}
	cb := func(instID int64, owner, name, repo string) {
		g.sweepRepo(ctx, emit, instID, owner, name, repo, st)
	}
	if len(g.cfg.Sweep.Repos) == 0 {
		// No explicit repos → sweep every repo the App is installed on. In
		// App-less (static-token) mode there is no installation to enumerate, so
		// this warns and sweeps nothing rather than erroring the daemon; an
		// App-less operator lists `repos:` explicitly.
		if err := g.sweepAllInstalled(ctx, cb); err != nil {
			log.Printf("github[%s]: sweep: %v", g.name, err)
		}
	} else {
		log.Printf("github[%s]: sweep starting (%d repo entr%s)", g.name, len(g.cfg.Sweep.Repos), plural(len(g.cfg.Sweep.Repos)))
		g.eachRepo(ctx, "sweep", g.cfg.Sweep.Repos, cb)
	}
	log.Printf("github[%s]: sweep done — repos=%d prs=%d review_requested=%d (skipped draft=%d, excluded=%d) merge_conflict=%d pr_behind=%d changes_requested=%d new_comment=%d",
		g.name, st.repos, st.prs, st.review, st.reviewDraft, st.reviewExcluded, st.conflict, st.behind, st.comments, st.newComments)
	return nil
}

// sweepAllInstalled enumerates every App installation and every repo within it,
// invoking fn per repo. It is the default when no `repos:` is configured: the App
// installation is already the event boundary (webhooks arrive for exactly these
// repos), so sweeping all of them ingests nothing new — action stays gated by the
// trigger filters. Returns an error in App-less mode (no installations concept).
func (g *Source) sweepAllInstalled(ctx context.Context, fn func(instID int64, owner, name, repo string)) error {
	instIDs, err := g.app.listInstallations(ctx)
	if err != nil {
		return err
	}
	insts := "s"
	if len(instIDs) == 1 {
		insts = ""
	}
	log.Printf("github[%s]: sweep starting (all installed repos across %d installation%s)", g.name, len(instIDs), insts)
	for _, instID := range instIDs {
		repos, err := g.rest.listInstallationRepos(ctx, instID)
		if err != nil {
			log.Printf("github[%s]: sweep: installation %d: %v", g.name, instID, err)
			continue
		}
		for _, r := range repos {
			fn(instID, r.Owner.Login, r.Name, r.FullName)
		}
	}
	return nil
}

// eachRepo resolves repo entries (concrete owner/name or an owner glob expanded via
// the installation's repo list) and calls fn for each, with the resolved
// installation id. Shared by the full sweep (sweep.repos) and the stuck-check poller
// (the repos of rules that configure stuck_checks).
func (g *Source) eachRepo(ctx context.Context, tag string, entries []string, fn func(instID int64, owner, name, repo string)) {
	for _, entry := range entries {
		owner, _ := splitRepo(entry)
		if owner == "" {
			continue
		}
		if strings.Contains(entry, "*") {
			if strings.Contains(owner, "*") {
				// A wildcard OWNER ("*/*", "*/foo") can't resolve to a single
				// account's installation, and expanding it would need
				// enumerating every App installation. Skip with a clear note
				// rather than a misleading GET /users/*/installation 404. Use an
				// explicit owner (e.g. "acme/*") to glob repos within one
				// installation.
				log.Printf("github[%s]: %s %s: skipped — a wildcard owner can't be expanded; use an explicit owner like acme/*", g.name, tag, entry)
				continue
			}
			instID, err := g.app.accountInstallationID(ctx, owner)
			if err != nil {
				log.Printf("github[%s]: %s %s: %v", g.name, tag, entry, err)
				continue
			}
			repos, err := g.rest.listInstallationRepos(ctx, instID)
			if err != nil {
				log.Printf("github[%s]: %s %s: %v", g.name, tag, entry, err)
				continue
			}
			for _, r := range repos {
				if ok, _ := path.Match(entry, r.FullName); ok {
					fn(instID, r.Owner.Login, r.Name, r.FullName)
				}
			}
			continue
		}
		_, name := splitRepo(entry)
		if name == "" {
			continue
		}
		instID, err := g.app.repoInstallationID(ctx, owner, name)
		if err != nil {
			log.Printf("github[%s]: %s %s: %v", g.name, tag, entry, err)
			continue
		}
		fn(instID, owner, name, entry)
	}
}

// stuckLoop polls for stuck CI on your open PRs on its OWN fixed cadence (default
// 15m) — independent of the adaptive sweep, whose ceiling can be hours away. Runs
// when a stuck_checks action is configured and there are watch repos.
func (g *Source) stuckLoop(ctx context.Context, emit EmitFunc) {
	iv := g.stuckPollInterval()
	log.Printf("github[%s]: stuck-check poller enabled, every %s", g.name, iv)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.stuckPass(ctx, emit)
		}
	}
}

// stuckPass scans each watched repo's open PRs (yours) for stuck CI and emits
// stuck_checks. Uses the PR list payload directly (head SHA is there) — no extra
// per-PR fetch beyond the stuck-run lookup.
func (g *Source) stuckPass(ctx context.Context, emit EmitFunc) {
	n := 0
	g.eachRepo(ctx, "stuck", g.stuckRepos(), func(instID int64, owner, name, repo string) {
		prs, err := g.rest.listOpenPRs(ctx, instID, owner, name)
		if err != nil {
			log.Printf("github[%s]: stuck %s: %v", g.name, repo, err)
			return
		}
		for _, pr := range prs {
			if !g.self[strings.ToLower(pr.User.Login)] {
				continue // your PRs only
			}
			t := g.target(repo, pr.Number, pr.Head.SHA, pr.Base.Ref, pr.HTMLURL)
			for _, tr := range g.sweepStuckChecks(ctx, instID, owner, name, repo, t, pr.Head.SHA) {
				tr.CatchUp = true
				emit(ctx, tr)
				n++
			}
		}
	})
	if n > 0 {
		log.Printf("github[%s]: stuck-check pass emitted %d stuck_checks", g.name, n)
	}
}

// anyStuckChecks reports whether any rule (defaults or specific) configures an
// enabled stuck_checks action — the gate for starting the stuck poller.
func (g *Source) anyStuckChecks() bool {
	for _, r := range append([]Rule{g.cfg.Defaults}, g.cfg.Rules...) {
		if stuckEnabled(r.Actions["stuck_checks"]) {
			return true
		}
	}
	return false
}

// stuckEnabled reports whether an action set has an enabled stuck_checks variant.
func stuckEnabled(set ActionSet) bool {
	for _, a := range set {
		if a.IsEnabled() {
			return true
		}
	}
	return false
}

// stuckRepos is the watch list for the stuck poller: the match.repos of every rule
// that configures stuck_checks (all rules if it's set in defaults), deduped. This is
// what makes stuck_checks self-contained — it watches the repos its rule targets,
// not sweep.repos.
func (g *Source) stuckRepos() []string {
	defaultsHas := stuckEnabled(g.cfg.Defaults.Actions["stuck_checks"])
	seen := map[string]bool{}
	var out []string
	add := func(repos []string) {
		for _, repo := range repos {
			if !seen[repo] {
				seen[repo] = true
				out = append(out, repo)
			}
		}
	}
	for _, r := range g.cfg.Rules {
		set := r.Actions["stuck_checks"]
		if !defaultsHas && !stuckEnabled(set) {
			continue
		}
		// The connectors lowering (github.go Source) makes every rule a "*/*"
		// catch-all and carries each trigger's real repo scope on its action;
		// legacy configs put the scope on the rule's Match.Repos. Prefer the
		// enabled stuck_checks action's own Repos so the poller targets the
		// configured repos — not the "*/*" catch-all, which would send eachRepo
		// into an installation lookup for a wildcard owner (github: stuck */*:
		// users/*/installation 404). Fall back to Match.Repos for the legacy
		// rule-scoped shape.
		scoped := false
		for _, a := range set {
			if a.IsEnabled() && len(a.Repos) > 0 {
				add(a.Repos)
				scoped = true
			}
		}
		if !scoped {
			add(r.Match.Repos)
		}
	}
	return out
}

// stuckPollInterval reads the poll cadence from the first enabled stuck_checks
// action (defaults or a rule), defaulting to 15m.
func (g *Source) stuckPollInterval() time.Duration {
	for _, r := range append([]Rule{g.cfg.Defaults}, g.cfg.Rules...) {
		for _, a := range r.Actions["stuck_checks"] {
			if a.IsEnabled() {
				return a.PollIntervalDur()
			}
		}
	}
	return 15 * time.Minute
}

// sweepStats is a per-run tally so a sweep is never a black box: it says what it
// scanned and, crucially, WHY review candidates were skipped (draft/excluded).
type sweepStats struct {
	repos, prs                          int
	review, reviewDraft, reviewExcluded int
	conflict, behind, comments          int
	newComments                         int
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// sweepRepo reconciles one repo's open PRs: review_requested for PRs where your
// review is pending (recovers missed review-request webhooks), and conflict/behind
// for PRs you authored.
func (g *Source) sweepRepo(ctx context.Context, emit EmitFunc, instID int64, owner, name, repo string, st *sweepStats) {
	prs, err := g.rest.listOpenPRs(ctx, instID, owner, name)
	if err != nil {
		log.Printf("github[%s]: sweep %s: %v", g.name, repo, err)
		return
	}
	st.repos++
	for _, pr := range prs {
		st.prs++
		// review_requested applies to *others'* PRs where your review is pending;
		// the list payload already carries requested reviewers (no extra fetch).
		for _, tr := range g.sweepReviewRequested(repo, pr, st) {
			tr.CatchUp = true
			emit(ctx, tr)
		}

		if !g.self[strings.ToLower(pr.User.Login)] {
			continue // conflict/behind autopilot is for PRs you authored
		}
		info, err := g.rest.pull(ctx, instID, owner, name, pr.Number)
		if err != nil {
			continue
		}
		t := g.target(repo, pr.Number, info.Head.SHA, info.Base.Ref, info.HTMLURL)
		headRef := info.Head.Ref
		if headRef == "" {
			headRef = pr.Head.Ref
		}
		var trs []Trigger
		switch info.MergeableState {
		case "dirty":
			trs = g.single(repo, "merge_conflict", t, "sweep: merge conflict",
				"conflict:"+info.Base.Ref+"/"+info.Head.SHA, nil)
			st.conflict += len(trs)
		case "behind":
			trs = g.single(repo, "pr_behind", t, "sweep: behind base",
				"behind:"+info.Base.Ref+"/"+info.Head.SHA, nil)
			st.behind += len(trs)
		}
		ct := g.sweepUnresolvedComments(ctx, instID, owner, name, repo, t)
		st.comments += len(ct)
		trs = append(trs, ct...)
		nc := g.sweepMissedComments(ctx, instID, owner, name, repo, t, info.Head.Ref)
		st.newComments += len(nc)
		trs = append(trs, nc...)
		// NOTE: stuck_checks is NOT detected here — it runs on its own tight cadence
		// (stuckLoop) since a stuck check is time-sensitive and the sweep's ceiling can
		// be hours away.
		for _, tr := range trs {
			tr.CatchUp = true
			// Every sweep trigger here is about this PR: carry its head branch,
			// as the webhook path does. Without it a worktree dispatch can't
			// land on the PR branch — it falls back to a local pr-<n> branch,
			// and the agent's push publishes that as a new branch instead of
			// updating the PR.
			if headRef != "" && emptyStr(tr.Context["head_ref"]) {
				if tr.Context == nil {
					tr.Context = map[string]any{}
				}
				tr.Context["head_ref"] = headRef
			}
			emit(ctx, tr)
		}
	}
}

// sweepUnresolvedComments reconciles outstanding review-comment threads on your
// PR — recovering feedback no live webhook picked up — by emitting changes_requested
// for the fixer to address. The dedup signature includes the set of unresolved
// thread ids, so it re-fires when new threads appear and stops once they're
// resolved (acted per state). Only runs when changes_requested is configured, to
// avoid the extra GraphQL call otherwise.
//
// A thread whose opener has since APPROVED is not outstanding: the reviewer
// signed off, and threads are often left open after an approval. Counting them
// launched a fresh changes_requested fixer on an approved PR every time the head
// moved (the signature includes the head) — which then re-requested the
// approver's review. Their comments still reach the new_comment autopilot.
func (g *Source) sweepUnresolvedComments(ctx context.Context, instID int64, owner, name, repo string, t Target) []Trigger {
	act, ok := g.actionFor(repo, "changes_requested")
	if !ok || !act.IsEnabled() {
		return nil
	}
	all, err := g.rest.unresolvedThreads(ctx, instID, owner, name, t.Number)
	if err != nil {
		return nil
	}
	var threads []unresolvedThread
	for _, th := range all {
		if !th.AuthorApproved {
			threads = append(threads, th)
		}
	}
	if len(threads) == 0 {
		return nil
	}
	ids := make([]string, 0, len(threads))
	for _, th := range threads {
		ids = append(ids, th.ID)
	}
	sig := "threads:" + t.HeadSHA + ":" + threadSig(ids)
	extra := g.threadReviewerFacts(threads)
	if extra == nil {
		extra = map[string]any{}
	}
	// The threads' opening comments are the feedback this run addresses —
	// the same review_comments the webhook path carries for a review.
	cs := make([]reviewComment, 0, len(threads))
	for _, th := range threads {
		cs = append(cs, reviewComment{ID: th.CommentID, Author: th.Author, Path: th.Path, Line: th.Line, Body: th.Body, URL: th.URL})
	}
	addReviewComments(extra, cs)
	// What the run reacts on: each thread's opening comment, capped — a PR
	// with dozens of open threads shouldn't fan out dozens of reactions twice.
	openers := make([]int64, 0, len(cs))
	for _, c := range cs {
		if len(openers) == maxThreadReactions {
			break
		}
		openers = append(openers, c.ID)
	}
	extra["reaction_subjects"] = reactionSubjects(githubkit.SubjectReviewComment, openers...)
	// The threads' highest comment id gates this on the engine's comment
	// high-water mark, the same mark the webhook's changes_requested for a
	// review advances: a review already dispatched is not re-dispatched by a
	// sweep still finding its threads unresolved (nor after the reviewer
	// edits it), while a new review's threads are above the mark.
	if id := maxCommentID(cs); id > 0 {
		extra["comment_id"], extra["comment_kind"] = id, CommentKindReview
	}
	return g.single(repo, "changes_requested", t,
		fmt.Sprintf("sweep: %d unresolved comment thread(s) on %s#%d", len(ids), repo, t.Number), sig,
		extra)
}

// maxThreadReactions caps the thread openers a sweep-recovered run reacts on.
const maxThreadReactions = 10

// threadReviewerFacts names the reviewer behind a sweep-recovered
// changes_requested — the opener of the first unresolved thread that isn't
// you, preferring a human over a bot — as the same `author`/`author_is_bot`
// facts the webhook path carries, so a flow's "{{.author}}" (the re-request
// step) pings a real reviewer. A review bot (Cursor Bugbot, Copilot, …) can't
// be re-requested — GitHub 422s it as "not a collaborator" — so it is named
// only when no human opened a thread, with author_is_bot telling the flow so.
// nil when no thread names one (only self-authored threads, or ghost authors).
func (g *Source) threadReviewerFacts(threads []unresolvedThread) map[string]any {
	var bot *unresolvedThread
	for i, th := range threads {
		if th.Author == "" || g.self[strings.ToLower(th.Author)] {
			continue
		}
		if !th.AuthorIsBot {
			return map[string]any{"author": th.Author, "author_is_bot": false}
		}
		if bot == nil {
			bot = &threads[i]
		}
	}
	if bot != nil {
		return map[string]any{"author": bot.Author, "author_is_bot": true}
	}
	return nil
}

// commentRecoveryWindow bounds the sweep's missed-comment recovery: a comment older
// than this is never re-emitted. Recovery exists for comments whose webhook was
// dropped while the daemon was offline — an offline gap this long is a different
// problem, and a stale comment shouldn't suddenly spawn a fixer. It also caps the
// blast radius when a high-water mark is missing (e.g. a state file from before
// marks were kept per comment kind, where the review mark starts at 0).
const commentRecoveryWindow = 24 * time.Hour

// sweepMissedComments recovers PR comments (issue + review) whose live webhook the
// daemon missed while offline. It re-lists recent comments and emits new_comment for
// each non-self one within commentRecoveryWindow; the engine's per-PR, per-kind
// comment high-water mark (advanced on a successful new_comment dispatch) drops any
// already handled, so only genuinely-newer comments dispatch. Only runs when
// new_comment is configured (avoids the extra fetch), and only on your own PRs
// (new_comment autopilot pushes fixes to PRs you authored — matching the webhook
// gate in commentTriggers).
func (g *Source) sweepMissedComments(ctx context.Context, instID int64, owner, name, repo string, t Target, headRef string) []Trigger {
	act, ok := g.actionFor(repo, "new_comment")
	if !ok || !act.IsEnabled() {
		return nil
	}
	comments, err := g.rest.recentComments(ctx, instID, owner, name, t.Number)
	if err != nil || len(comments) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-commentRecoveryWindow)
	// Standalone comments recover one event each, as on the webhook path. An
	// inline comment that names its review is part of that review's ONE event
	// (reviewfold.go), so they are recovered per review instead.
	var out []Trigger
	byReview := map[int64][]prComment{}
	var reviews []int64
	for _, c := range comments {
		author := strings.ToLower(c.User.Login)
		if g.self[author] {
			continue // our own comments never drive a fix
		}
		if !c.CreatedAt.IsZero() && c.CreatedAt.Before(cutoff) {
			continue // too old to be a "missed while offline" comment
		}
		if c.Kind == CommentKindReview && c.ReviewID != 0 {
			if _, seen := byReview[c.ReviewID]; !seen {
				reviews = append(reviews, c.ReviewID)
			}
			byReview[c.ReviewID] = append(byReview[c.ReviewID], c)
			continue
		}
		out = append(out, g.sweepComment(repo, t, headRef, c)...)
	}
	for _, id := range reviews {
		out = append(out, g.sweepReview(ctx, instID, repo, t, headRef, id, byReview[id])...)
	}
	return out
}

// sweepReview recovers one submitted review whose deliveries the daemon missed
// as the review's one event — the same one the webhook path emits, and
// deduped against it: a review already emitted (claimed) is skipped, and past
// a restart the engine's comment high-water mark drops it (its comment_id is
// the review's highest). A review that isn't an approval is skipped when a
// changes_requested trigger is configured — it is a changes_requested event
// (reviewfold.go), and sweepUnresolvedComments carries its threads. An
// unreadable review recovers its comments one by one.
func (g *Source) sweepReview(ctx context.Context, instID int64, repo string, t Target, headRef string, reviewID int64, group []prComment) []Trigger {
	if g.reviews.claimed(reviewID, time.Now()) {
		return nil
	}
	ri, ok := g.reviewFacts(ctx, instID, repo, t.Number, reviewID)
	if !ok {
		var out []Trigger
		for _, c := range group {
			out = append(out, g.sweepComment(repo, t, headRef, c)...)
		}
		return out
	}
	if ri.State != "approved" && g.wouldEmit(repo, "changes_requested", nil) {
		return nil
	}
	if ri.Author == "" {
		ri.Author, ri.AuthorIsBot = group[0].User.Login, isBotLogin(group[0].User.Login)
	}
	cs, err := g.listReviewComments(ctx, instID, repo, t.Number, reviewID)
	if err != nil || len(cs) == 0 {
		// The listing already holds this review's recent comments.
		cs = cs[:0]
		for _, c := range group {
			cs = append(cs, reviewComment{ID: c.ID, Author: c.User.Login, Path: c.Path,
				Line: lineOf(c.Line, c.OriginalLine), Body: c.Body, URL: c.HTMLURL})
		}
	}
	trs := g.reviewNewComment(repo, t, headRef, reviewID, ri, cs)
	for i := range trs {
		trs[i].Title = "sweep: " + trs[i].Title
	}
	return trs
}

// sweepComment recovers one standalone comment as its own new_comment.
func (g *Source) sweepComment(repo string, t Target, headRef string, c prComment) []Trigger {
	extra := map[string]any{"author": c.User.Login, "comment_body": c.Body, "head_ref": headRef,
		"comment_id": c.ID, "comment_kind": c.Kind,
		"reaction_subjects": reactionSubjects(commentSubjectKind(c.Kind), c.ID)}
	return g.emit(repo, "new_comment", t,
		fmt.Sprintf("sweep: comment by %s on %s#%d", c.User.Login, repo, t.Number),
		fmt.Sprintf("comment:%d", c.ID), extra, func(act Action) bool {
			// The comment LISTING carries no account type, so bot-ness is
			// the login convention alone — which is why the legacy
			// lowering here omits author_bot (see lowerComment). A
			// hand-written `filter:` may still read author_is_bot; it
			// just sees the weaker signal on this path.
			return g.filterPasses(act, "sweep new_comment", repo,
				commentFilterFacts(c.User.Login, c.Body, isBotLogin(c.User.Login)),
				lowerComment(act, false))
		})
}

// sweepStuckChecks fires stuck_checks for CI runs on your PR that are still
// running well past their normal window — a dead runner leaves a run "in_progress"
// so the check never completes (failing_checks needs a terminal failure, so it
// never catches this) and the PR stays blocked. The configured action (e.g. a
// gh-rerun command) gets the stuck run in Context (run_id/run_name/run_status);
// dedup per run id, so a given stuck run fires once (a fresh re-run gets a new id).
// Only runs when stuck_checks is configured.
func (g *Source) sweepStuckChecks(ctx context.Context, instID int64, owner, name, repo string, t Target, headSHA string) []Trigger {
	act, ok := g.actionFor(repo, "stuck_checks")
	if !ok || !act.IsEnabled() {
		return nil
	}
	runs, err := g.rest.stuckRuns(ctx, instID, owner, name, headSHA, act.StuckAfterDur(), time.Now())
	if err != nil || len(runs) == 0 {
		return nil
	}
	var out []Trigger
	for _, r := range runs {
		extra := map[string]any{"run_id": r.ID, "run_name": r.Name, "run_status": r.Status}
		out = append(out, g.single(repo, "stuck_checks", t,
			fmt.Sprintf("sweep: check %q stuck (%s) on %s#%d", r.Name, r.Status, repo, t.Number),
			fmt.Sprintf("stuck:%d", r.ID), extra)...)
	}
	return out
}

// threadSig is a compact, stable signature for a set of unresolved thread ids.
func threadSig(ids []string) string {
	h := fnv.New64a()
	for _, id := range ids { // ids arrive sorted
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("%d:%x", len(ids), h.Sum64())
}

// sweepReviewRequested emits a review_requested trigger when your review is a
// pending requested reviewer on pr. The dedup signature matches the webhook path
// ("reviewreq@<head>"), so a request already handled live isn't re-fired.
func (g *Source) sweepReviewRequested(repo string, pr prListItem, st *sweepStats) []Trigger {
	labels := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		labels = append(labels, l.Name)
	}
	// Is your review pending on this PR for ANY configured review_requested variant?
	pending := false
	for _, act := range g.actionsFor(repo, "review_requested") {
		if act.IsEnabled() && g.prReviewerMatches(g.reviewerFor(repo, act), pr) {
			pending = true
			break
		}
	}
	if !pending {
		return nil // not a review pending on you — not a candidate
	}
	t := g.target(repo, pr.Number, pr.Head.SHA, pr.Base.Ref, pr.HTMLURL)
	trs := g.emit(repo, "review_requested", t,
		fmt.Sprintf("sweep: review requested on %s#%d", repo, pr.Number),
		"reviewreq@"+pr.Head.SHA, map[string]any{"labels": labels}, func(act Action) bool {
			return g.prReviewerMatches(g.reviewerFor(repo, act), pr) &&
				g.filterPasses(act, "sweep review_requested", repo, prFilterFacts(
					pr.Head.Ref, pr.Base.Ref, pr.Title, pr.User.Login, labels, pr.Draft),
					lowerReviewRequested(act))
		})
	if len(trs) == 0 { // pending, but every matching variant is gated out
		if pr.Draft {
			st.reviewDraft++
			log.Printf("github[%s]: sweep %s#%d review pending but skipped (draft)", g.name, repo, pr.Number)
		} else {
			st.reviewExcluded++
			log.Printf("github[%s]: sweep %s#%d review pending but skipped (exclude)", g.name, repo, pr.Number)
		}
		return nil
	}
	st.review += len(trs)
	log.Printf("github[%s]: sweep %s#%d review pending -> emitting review_requested", g.name, repo, pr.Number)
	return trs
}

// prReviewerMatches reports whether the configured reviewer (defaulting to the
// `me` identity when unset) is among the PR's pending requested reviewers/teams.
func (g *Source) prReviewerMatches(rev Actors, pr prListItem) bool {
	logins := make([]string, 0, len(pr.RequestedReviewers))
	for _, rr := range pr.RequestedReviewers {
		logins = append(logins, rr.Login)
	}
	slugs := make([]string, 0, len(pr.RequestedTeams))
	for _, tm := range pr.RequestedTeams {
		slugs = append(slugs, tm.Slug)
	}
	return g.reviewerInList(rev, logins, slugs)
}
