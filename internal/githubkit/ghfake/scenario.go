package ghfake

import (
	"fmt"
	"sort"
	"time"
)

// The scenario API: tests act as other people — a reviewer, a bot, CI, the
// merger — and the fake changes its model and sends GitHub's webhooks for it.
// The query helpers below read the resulting state for assertions. Every
// method takes the fake's lock; errors (an unknown repo, PR or user) panic,
// because they are mistakes in the test, not outcomes under test.

func (f *Fake) mustRepo(repo string) *Repo {
	r := f.repo(repo)
	if r == nil {
		panic("ghfake: no repo " + repo)
	}
	return r
}

func (f *Fake) mustPull(repo string, n int) (*Repo, *Pull) {
	r := f.mustRepo(repo)
	p := r.pull(n)
	if p == nil {
		panic(fmt.Sprintf("ghfake: no PR %s#%d", repo, n))
	}
	return r, p
}

func (f *Fake) mustUser(login string) *User {
	if u := f.user(login); u != nil {
		return u
	}
	return f.addUser(login, "")
}

// OpenPR opens a pull request as author and returns its number.
func (f *Fake) OpenPR(repo, author string, o PROpts) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	if o.Head == "" {
		o.Head = fmt.Sprintf("feature-%d", r.nextNumber+1)
	}
	if o.Title == "" {
		o.Title = "Change " + o.Head
	}
	return f.openPull(r, f.mustUser(author), o).Issue.Number
}

// PR returns a copy of a PR's current state.
func (f *Fake) PR(repo string, n int) Pull {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, p := f.mustPull(repo, n)
	return *p
}

// Push pushes a new commit to branch as author (synchronizing every open PR
// whose head it is; a PR whose base it is goes behind) and returns its sha.
// paths are the files the commit touches (threads on them go outdated);
// none means every thread on a synchronized PR.
func (f *Fake) Push(repo, branch, author, msg string, paths ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	if msg == "" {
		msg = "push to " + branch
	}
	return f.pushTo(r, branch, f.mustUser(author), msg, "", paths)
}

// PushSHA moves branch to an EXISTING sha pushed elsewhere (a real git
// remote's post-receive hook reporting a push), as author.
func (f *Fake) PushSHA(repo, branch, author, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushTo(f.mustRepo(repo), branch, f.mustUser(author), "push", sha, nil)
}

// SetConflict marks a PR's head as conflicting with its base (dirty) or not.
func (f *Fake) SetConflict(repo string, n int, conflict bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, p := f.mustPull(repo, n)
	p.Conflict = conflict
	p.touched()
}

// Comment posts a conversation comment as author; returns its id.
func (f *Fake) Comment(repo string, n int, author, body string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	is := r.Issues[n]
	if is == nil {
		panic(fmt.Sprintf("ghfake: no issue %s#%d", repo, n))
	}
	return f.comment(r, is, f.mustUser(author), body).ID
}

// SubmitReview submits a review as reviewer (state APPROVED,
// CHANGES_REQUESTED or COMMENTED) with inline comments, delivering the
// review event and one comment event per inline comment in
// Options.ReviewOrder. Returns the review id and its comment ids.
func (f *Fake) SubmitReview(repo string, n int, reviewer, state, body string, inline ...InlineComment) (int64, []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, p := f.mustPull(repo, n)
	for i := range inline {
		if inline[i].Path == "" && len(p.Files) > 0 {
			inline[i].Path = p.Files[0].Path
		}
		if inline[i].Line == 0 && len(p.Files) > 0 && len(p.Files[0].Lines) > 0 {
			inline[i].Line = p.Files[0].Lines[i%len(p.Files[0].Lines)]
		}
	}
	rv := f.submitReview(r, p, f.mustUser(reviewer), state, body, inline)
	var ids []int64
	for _, c := range rv.Comments {
		ids = append(ids, c.ID)
	}
	return rv.ID, ids
}

// EditReview edits a submitted review's body as its author would
// (pull_request_review.edited) — what a review bot does to mark an old
// review stale.
func (f *Fake) EditReview(repo string, n int, reviewID int64, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, p := f.mustPull(repo, n)
	for _, rv := range p.Reviews {
		if rv.ID == reviewID {
			old := rv.Body
			rv.Body = body
			f.emit(r, "pull_request_review", "edited", rv.User, map[string]any{
				"review": f.reviewWebhook(r, p, rv), "pull_request": f.pullWebhook(r, p),
				"changes": map[string]any{"body": map[string]any{"from": old}}})
			return
		}
	}
	panic("ghfake: no review " + fmt.Sprint(reviewID))
}

// DismissReview dismisses a review.
func (f *Fake) DismissReview(repo string, n int, reviewID int64, by string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, p := f.mustPull(repo, n)
	for _, rv := range p.Reviews {
		if rv.ID == reviewID {
			rv.State = "DISMISSED"
			p.touched()
			f.emit(r, "pull_request_review", "dismissed", f.mustUser(by), map[string]any{
				"review": f.reviewWebhook(r, p, rv), "pull_request": f.pullWebhook(r, p)})
			return
		}
	}
}

// Reply replies to an inline comment's thread as author.
func (f *Fake) Reply(repo string, n int, commentID int64, author, body string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, p := f.mustPull(repo, n)
	for _, cm := range p.ReviewComments {
		if cm.ID == commentID {
			return f.reply(r, p, cm, f.mustUser(author), body).ID
		}
	}
	panic("ghfake: no review comment " + fmt.Sprint(commentID))
}

// ResolveThread resolves (or unresolves) the thread a comment belongs to.
func (f *Fake) ResolveThread(repo string, n int, commentID int64, by string, resolved bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, p := f.mustPull(repo, n)
	for _, th := range p.Threads {
		if th.Comments[0].ID != commentID {
			continue
		}
		th.Resolved = resolved
		th.ResolvedBy = f.mustUser(by)
		p.touched()
		action := "resolved"
		if !resolved {
			action = "unresolved"
		}
		var comments []any
		for _, cm := range th.Comments {
			comments = append(comments, f.reviewCommentJSON(r, p, cm, "pull-request-review-comment"))
		}
		f.emit(r, "pull_request_review_thread", action, th.ResolvedBy, map[string]any{
			"thread": map[string]any{"node_id": nodeID("PRRT", th.ID), "comments": comments}, "pull_request": f.pullWebhook(r, p)})
		return
	}
	panic("ghfake: no thread opened by " + fmt.Sprint(commentID))
}

// RequestReview requests reviewer's review on a PR, as by.
func (f *Fake) RequestReview(repo string, n int, by, reviewer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, p := f.mustPull(repo, n)
	f.mustUser(reviewer)
	r.Collaborators[lower(reviewer)] = true
	f.requestReview(r, p, f.mustUser(by), reviewer)
}

// Check runs one CI check to completion on sha as an Actions workflow (one
// job): check_run, check_suite and workflow_run completed. Returns the
// workflow run id and the check run id.
func (f *Fake) Check(repo, sha, name, conclusion string) (runID, checkID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	w := f.startRun(r, sha, f.branchOf(r, sha), name, "pull_request")
	cr := f.addJob(r, w, name)
	f.completeCheck(r, cr, conclusion)
	return w.ID, cr.ID
}

// StartCheck starts a check that does not finish (a stuck run), created at
// startedAt. Returns the workflow run id.
func (f *Fake) StartCheck(repo, sha, name string, startedAt time.Time) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	w := f.startRun(r, sha, f.branchOf(r, sha), name, "pull_request")
	w.CreatedAt, w.RunStartedAt = startedAt.UTC(), startedAt.UTC()
	f.addJob(r, w, name)
	return w.ID
}

// CompleteRerun finishes a re-run workflow run's open checks.
func (f *Fake) CompleteRerun(repo string, runID int64, conclusion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	for _, cr := range r.CheckRuns {
		if cr.RunID == runID && cr.Status != "completed" {
			f.completeCheck(r, cr, conclusion)
		}
	}
}

func (f *Fake) branchOf(r *Repo, sha string) string {
	for _, k := range sortedKeys(r.Branches) {
		if r.Branches[k] == sha {
			return k
		}
	}
	return ""
}

// Status posts a commit status as by (a CI system, or anyone).
func (f *Fake) Status(repo, sha, by, state, context, desc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	f.setStatus(r, sha, f.mustUser(by), state, context, desc, "")
}

// Merge merges a PR as by (pull_request.closed with merged: true, and the
// push to its base).
func (f *Fake) Merge(repo string, n int, by string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, p := f.mustPull(repo, n)
	return f.mergePull(r, p, f.mustUser(by), "")
}

// CloseIssue closes a PR (or issue) without merging.
func (f *Fake) CloseIssue(repo string, n int, by string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	is := r.Issues[n]
	if is.Pull != nil {
		f.closePull(r, is.Pull, f.mustUser(by))
		return
	}
	is.State, is.ClosedAt = "closed", f.now()
	f.emitIssue(r, is, "closed", f.mustUser(by), nil)
}

// SetDraft converts a PR to a draft or marks it ready for review.
func (f *Fake) SetDraft(repo string, n int, by string, draft bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, p := f.mustPull(repo, n)
	if p.Draft == draft {
		return
	}
	p.Draft = draft
	p.touched()
	action := "ready_for_review"
	if draft {
		action = "converted_to_draft"
	}
	f.emitPull(r, p, action, f.mustUser(by), nil)
}

// Label adds a label to an issue or PR as by.
func (f *Fake) Label(repo string, n int, by, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	f.addLabel(r, r.Issues[n], f.mustUser(by), label)
}

// OpenIssue opens an issue as author; returns its number.
func (f *Fake) OpenIssue(repo, author string, o IssueOpts) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	for _, a := range o.Assignees {
		f.mustUser(a)
	}
	return f.openIssue(r, f.mustUser(author), o).Number
}

// Assign assigns login to an issue or PR, as by.
func (f *Fake) Assign(repo string, n int, by, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	f.mustUser(login)
	f.assign(r, r.Issues[n], f.mustUser(by), login)
}

// SetProjectField moves an issue on a Projects v2 board (a single-select
// field value) and delivers projects_v2_item.edited.
func (f *Fake) SetProjectField(repo string, n int, by, field, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	is := r.Issues[n]
	if is.ProjectFields == nil {
		is.ProjectFields = map[string]string{}
	}
	is.ProjectFields[field] = value
	f.emit(r, "projects_v2_item", "edited", f.mustUser(by), map[string]any{
		"projects_v2_item": map[string]any{"id": is.ID, "node_id": nodeID("PVTI", is.ID), "project_node_id": nodeID("PVT", 1),
			"content_node_id": nodeID("I", is.ID), "content_type": "Issue", "creator": f.userJSON(is.User),
			"created_at": ts(is.CreatedAt), "updated_at": ts(f.now()), "archived_at": nil},
	})
}

// Release publishes a release as by.
func (f *Fake) Release(repo, by string, o ReleaseOpts) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishRelease(f.mustRepo(repo), f.mustUser(by), o).ID
}

// DeploymentStatus delivers a deployment_status for sha.
func (f *Fake) DeploymentStatus(repo, sha, env, state, desc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	id := f.id()
	f.emit(r, "deployment_status", "created", f.appBot(), map[string]any{
		"deployment_status": map[string]any{"id": id, "state": state, "environment": env, "description": desc,
			"target_url": htmlBase + "/deploys/" + fmt.Sprint(id), "creator": f.userJSON(f.appBot())},
		"deployment": map[string]any{"id": id + 1, "sha": sha, "ref": f.branchOf(r, sha), "environment": env, "creator": f.userJSON(f.appBot())},
	})
}

// DependabotAlert delivers dependabot_alert.created.
func (f *Fake) DependabotAlert(repo, pkg, severity, summary string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	n := int(f.id() % 10000)
	f.emit(r, "dependabot_alert", "created", f.appBot(), map[string]any{"alert": map[string]any{
		"number": n, "state": "open", "html_url": fmt.Sprintf("%s/%s/security/dependabot/%d", htmlBase, r.FullName(), n),
		"dependency":        map[string]any{"package": map[string]any{"ecosystem": "npm", "name": pkg}},
		"security_advisory": map[string]any{"severity": severity, "summary": summary},
	}})
	return n
}

// SecretScanningAlert delivers secret_scanning_alert.created.
func (f *Fake) SecretScanningAlert(repo, secretType, display string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRepo(repo)
	n := int(f.id() % 10000)
	f.emit(r, "secret_scanning_alert", "created", f.appBot(), map[string]any{"alert": map[string]any{
		"number": n, "html_url": fmt.Sprintf("%s/%s/security/secret-scanning/%d", htmlBase, r.FullName(), n),
		"secret_type": secretType, "secret_type_display_name": display,
	}})
	return n
}

// --- queries for assertions ----------------------------------------------------

// StatusView is one commit status as an assertion reads it.
type StatusView struct {
	SHA, State, Context, Description, Creator string
}

// Statuses lists every status posted on sha, oldest first.
func (f *Fake) Statuses(repo, sha string) []StatusView {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StatusView
	for _, s := range f.mustRepo(repo).Statuses {
		if s.SHA == sha {
			out = append(out, StatusView{s.SHA, s.State, s.Context, s.Description, s.Creator.Login})
		}
	}
	return out
}

// LatestStatus is the latest status of context on sha ("" state if none).
func (f *Fake) LatestStatus(repo, sha, context string) StatusView {
	var last StatusView
	for _, s := range f.Statuses(repo, sha) {
		if s.Context == context {
			last = s
		}
	}
	return last
}

// ReactionView is one reaction.
type ReactionView struct{ User, Content string }

// Reactions lists the reactions on a subject: kind is issue_comment,
// review_comment or review.
func (f *Fake) Reactions(repo, kind string, id int64) []ReactionView {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rs []*Reaction
	for _, is := range f.mustRepo(repo).Issues {
		switch kind {
		case "issue_comment":
			for _, c := range is.Comments {
				if c.ID == id {
					rs = c.Reactions
				}
			}
		case "review_comment":
			if is.Pull != nil {
				for _, c := range is.Pull.ReviewComments {
					if c.ID == id {
						rs = c.Reactions
					}
				}
			}
		case "review":
			if is.Pull != nil {
				for _, rv := range is.Pull.Reviews {
					if rv.ID == id {
						rs = rv.Reactions
					}
				}
			}
		}
	}
	var out []ReactionView
	for _, x := range rs {
		out = append(out, ReactionView{x.User.Login, x.Content})
	}
	return out
}

// CommentView is one conversation comment.
type CommentView struct {
	ID         int64
	User, Body string
}

// Comments lists an issue's or PR's conversation comments, oldest first.
func (f *Fake) Comments(repo string, n int) []CommentView {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []CommentView
	for _, c := range f.mustRepo(repo).Issues[n].Comments {
		out = append(out, CommentView{c.ID, c.User.Login, c.Body})
	}
	return out
}

// RequestedReviewers lists a PR's pending review requests (lowercase logins).
func (f *Fake) RequestedReviewers(repo string, n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, p := f.mustPull(repo, n)
	out := append([]string(nil), p.RequestedUsers...)
	sort.Strings(out)
	return out
}

// ReviewRequestsOf counts the review_requested deliveries for reviewer on a
// PR since the fake started — how many times somebody (re-)requested them.
func (f *Fake) ReviewRequestsOf(repo string, n int, reviewer string) int {
	count := 0
	for _, d := range f.Deliveries() {
		if d.Event != "pull_request" || d.Action != "review_requested" || d.Repo != repo || d.Attempt != 1 {
			continue
		}
		p := d.Payload()
		rr, _ := p["requested_reviewer"].(map[string]any)
		if num, _ := p["number"].(float64); int(num) == n && rr != nil && lower(fmt.Sprint(rr["login"])) == lower(reviewer) {
			count++
		}
	}
	return count
}

// HeadSHA is a PR's current head.
func (f *Fake) HeadSHA(repo string, n int) string { return f.PR(repo, n).HeadSHA }

// IsMerged reports whether a PR is merged.
func (f *Fake) IsMerged(repo string, n int) bool { return f.PR(repo, n).Merged }

// RunAttempts is a workflow run's attempt count (re-runs increment it).
func (f *Fake) RunAttempts(repo string, runID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.mustRepo(repo).Runs {
		if w.ID == runID {
			return w.RunAttempt
		}
	}
	return 0
}
