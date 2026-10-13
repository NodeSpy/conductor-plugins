package ghfake

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// Model mutations, shared by the REST handlers (a write by whoever holds the
// credential) and the scenario API (a write by whoever the test acts as).
// Each one emits the webhook(s) GitHub sends for that change. Callers hold
// f.mu.

// PROpts opens a PR.
type PROpts struct {
	Title, Body string
	Head, Base  string // branch names; Head is created if missing
	Draft       bool
	Labels      []string
	Files       []File // default: one modified file, main.go, lines 1–20 in the diff
}

// IssueOpts opens an issue.
type IssueOpts struct {
	Title, Body       string
	Labels, Assignees []string
}

// ReleaseOpts publishes a release.
type ReleaseOpts struct {
	Tag, Name, Body, Target string
	Draft, Prerelease       bool
}

// InlineComment is one inline comment of a submitted review.
type InlineComment struct {
	Path string
	Line int
	Body string
}

func (f *Fake) commitTo(r *Repo, branch string, author *User, msg string) *Commit {
	parent := r.Branches[branch]
	cm := &Commit{SHA: f.sha(r.FullName() + branch + msg), Message: msg, Author: author, At: f.now()}
	if parent != "" {
		cm.Parents = []string{parent}
	}
	r.Commits[cm.SHA] = cm
	r.Branches[branch] = cm.SHA
	return cm
}

func (f *Fake) newIssue(r *Repo, author *User, title, body string) *Issue {
	r.nextNumber++
	now := f.now()
	is := &Issue{ID: f.id(), Number: r.nextNumber, Title: title, Body: body, State: "open", User: author, CreatedAt: now, UpdatedAt: now}
	r.Issues[is.Number] = is
	return is
}

func (f *Fake) openPull(r *Repo, author *User, o PROpts) *Pull {
	if o.Base == "" {
		o.Base = r.DefaultBranch
	}
	if _, ok := r.Branches[o.Head]; !ok {
		r.Branches[o.Head] = r.Branches[o.Base]
		f.commitTo(r, o.Head, author, "work on "+o.Head)
	}
	files := o.Files
	if len(files) == 0 {
		files = []File{{Path: "main.go", Status: "modified", Additions: 20, Deletions: 2, Lines: seq(1, 20)}}
	}
	is := f.newIssue(r, author, o.Title, o.Body)
	is.Labels = o.Labels
	p := &Pull{ID: f.id(), Issue: is, Draft: o.Draft, HeadRef: o.Head, BaseRef: o.Base,
		HeadSHA: r.Branches[o.Head], MergeBase: r.Branches[o.Base], Files: files}
	is.Pull = p
	r.Collaborators[lower(author.Login)] = true
	f.emitPull(r, p, "opened", author, nil)
	return p
}

func seq(a, b int) []int {
	var out []int
	for i := a; i <= b; i++ {
		out = append(out, i)
	}
	return out
}

// pushTo moves branch to a new commit (or to sha, when given): every open PR
// whose head is the branch synchronizes, every open PR whose base it is goes
// behind, and threads on the touched paths go outdated.
func (f *Fake) pushTo(r *Repo, branch string, author *User, msg string, sha string, paths []string) string {
	before := r.Branches[branch]
	if sha == "" {
		sha = f.commitTo(r, branch, author, msg).SHA
	} else {
		if r.Commits[sha] == nil {
			r.Commits[sha] = &Commit{SHA: sha, Message: msg, Author: author, At: f.now(), Parents: nonEmpty(before)}
		}
		r.Branches[branch] = sha
	}
	f.emit(r, "push", "", author, map[string]any{
		"ref": "refs/heads/" + branch, "before": zeroIf(before), "after": sha,
		"created": before == "", "deleted": false, "forced": false, "base_ref": nil,
		"compare": fmt.Sprintf("%s/%s/compare/%s...%s", htmlBase, r.FullName(), short(before), short(sha)),
		"commits": []any{}, "head_commit": nil,
		"pusher": map[string]any{"name": author.Login, "email": author.Email},
	})
	for _, n := range sortedIssueNumbers(r) {
		p := r.Issues[n].Pull
		if p == nil || p.Issue.State != "open" {
			continue
		}
		if p.HeadRef == branch && p.HeadSHA != sha {
			p.HeadSHA = sha
			p.touched()
			for _, th := range p.Threads {
				if len(paths) == 0 || contains(paths, th.Comments[0].Path) {
					th.Outdated = true
				}
			}
			f.emitPull(r, p, "synchronize", author, map[string]any{"before": zeroIf(before), "after": sha})
		}
		if p.BaseRef == branch {
			p.touched()
		}
	}
	return sha
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func zeroIf(s string) string {
	if s == "" {
		return strings.Repeat("0", 40)
	}
	return s
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func (f *Fake) closePull(r *Repo, p *Pull, by *User) {
	p.Issue.State, p.Issue.ClosedAt, p.Issue.UpdatedAt = "closed", f.now(), f.now()
	p.RequestedUsers, p.RequestedTeams = nil, nil
	f.emitPull(r, p, "closed", by, nil)
}

func (f *Fake) mergePull(r *Repo, p *Pull, by *User, title string) string {
	if title == "" {
		title = fmt.Sprintf("Merge pull request #%d from %s", p.Issue.Number, p.HeadRef)
	}
	cm := &Commit{SHA: f.sha("merge" + p.HeadSHA), Message: title, Author: by, At: f.now(), Parents: []string{r.Branches[p.BaseRef], p.HeadSHA}}
	r.Commits[cm.SHA] = cm
	p.Merged, p.MergedAt, p.MergedBy, p.MergeCommitSHA = true, f.now(), by, cm.SHA
	p.Issue.State, p.Issue.ClosedAt = "closed", f.now()
	p.RequestedUsers, p.RequestedTeams = nil, nil
	f.emitPull(r, p, "closed", by, nil)
	f.pushTo(r, p.BaseRef, by, title, cm.SHA, nil)
	return cm.SHA
}

func (f *Fake) requestReview(r *Repo, p *Pull, by *User, login string) {
	if !contains(p.RequestedUsers, lower(login)) {
		p.RequestedUsers = append(p.RequestedUsers, lower(login))
	}
	f.emitPull(r, p, "review_requested", by, map[string]any{"requested_reviewer": f.userJSON(f.user(login))})
}

// submitReview records a submitted review and its inline comments (each
// opening a thread), and emits pull_request_review.submitted plus one
// pull_request_review_comment.created per inline comment, in
// Options.ReviewOrder.
func (f *Fake) submitReview(r *Repo, p *Pull, by *User, state, body string, inline []InlineComment) *Review {
	now := f.now()
	rv := &Review{ID: f.id(), User: by, State: state, Body: body, CommitID: p.HeadSHA, SubmittedAt: now}
	var cs []*ReviewComment
	for _, ic := range inline {
		cm := &ReviewComment{ID: f.id(), ReviewID: rv.ID, User: by, Body: ic.Body, Path: ic.Path, Line: ic.Line, OriginalLine: ic.Line,
			Side: "RIGHT", CommitID: p.HeadSHA, OriginalCommitID: p.HeadSHA, CreatedAt: now, UpdatedAt: now}
		th := &Thread{ID: f.id(), Comments: []*ReviewComment{cm}}
		cm.Thread = th
		p.Threads = append(p.Threads, th)
		p.ReviewComments = append(p.ReviewComments, cm)
		cs = append(cs, cm)
	}
	rv.Comments = cs
	p.Reviews = append(p.Reviews, rv)
	p.RequestedUsers = remove(p.RequestedUsers, lower(by.Login))
	p.touched()
	if state == "PENDING" {
		return rv
	}
	review := f.prepare(r, "pull_request_review", "submitted", by, map[string]any{"review": f.reviewWebhook(r, p, rv), "pull_request": f.pullWebhook(r, p)})
	var comments []*Delivery
	for _, cm := range cs {
		comments = append(comments, f.prepare(r, "pull_request_review_comment", "created", by,
			map[string]any{"comment": f.reviewCommentJSON(r, p, cm, "pull-request-review-comment"), "pull_request": f.pullWebhook(r, p)})...)
	}
	f.enqueue(f.order(review, comments)...)
	return rv
}

func (f *Fake) reply(r *Repo, p *Pull, parent *ReviewComment, by *User, body string) *ReviewComment {
	now := f.now()
	cm := &ReviewComment{ID: f.id(), User: by, Body: body, Path: parent.Path, Line: parent.Line, OriginalLine: parent.OriginalLine,
		Side: "RIGHT", CommitID: p.HeadSHA, OriginalCommitID: parent.OriginalCommitID, InReplyTo: parent.ID,
		Thread: parent.Thread, CreatedAt: now, UpdatedAt: now}
	// A reply is its own single-comment review on GitHub.
	rv := &Review{ID: f.id(), User: by, State: "COMMENTED", CommitID: p.HeadSHA, SubmittedAt: now, Comments: []*ReviewComment{cm}}
	cm.ReviewID = rv.ID
	p.Reviews = append(p.Reviews, rv)
	if parent.Thread != nil {
		parent.Thread.Comments = append(parent.Thread.Comments, cm)
	}
	p.ReviewComments = append(p.ReviewComments, cm)
	f.emit(r, "pull_request_review_comment", "created", by, map[string]any{
		"comment": f.reviewCommentJSON(r, p, cm, "pull-request-review-comment"), "pull_request": f.pullWebhook(r, p)})
	return cm
}

// react adds by's content reaction; GitHub answers an existing one with 200.
func (f *Fake) react(rs *[]*Reaction, by *User, content string) (*Reaction, bool) {
	for _, x := range *rs {
		if lower(x.User.Login) == lower(by.Login) && x.Content == content {
			return x, false
		}
	}
	x := &Reaction{ID: f.id(), User: by, Content: content, CreatedAt: f.now()}
	*rs = append(*rs, x)
	return x, true
}

func (f *Fake) openIssue(r *Repo, by *User, o IssueOpts) *Issue {
	is := f.newIssue(r, by, o.Title, o.Body)
	is.Labels, is.Assignees = o.Labels, o.Assignees
	f.emitIssue(r, is, "opened", by, nil)
	return is
}

func (f *Fake) comment(r *Repo, is *Issue, by *User, body string) *IssueComment {
	now := f.now()
	cm := &IssueComment{ID: f.id(), Issue: is, User: by, Body: body, CreatedAt: now, UpdatedAt: now}
	is.Comments = append(is.Comments, cm)
	is.UpdatedAt = now
	f.emit(r, "issue_comment", "created", by, map[string]any{"issue": f.issueWebhook(r, is), "comment": f.issueCommentJSON(r, cm)})
	return cm
}

func (f *Fake) assign(r *Repo, is *Issue, by *User, login string) {
	is.Assignees = append(is.Assignees, login)
	extra := map[string]any{"assignee": f.userJSON(f.user(login))}
	if is.Pull != nil {
		f.emitPull(r, is.Pull, "assigned", by, extra)
		return
	}
	f.emitIssue(r, is, "assigned", by, extra)
}

func (f *Fake) addLabel(r *Repo, is *Issue, by *User, label string) {
	if containsFold(is.Labels, label) {
		return
	}
	is.Labels = append(is.Labels, label)
	extra := map[string]any{"label": f.labelJSON(r, label)}
	if is.Pull != nil {
		f.emitPull(r, is.Pull, "labeled", by, extra)
		return
	}
	f.emitIssue(r, is, "labeled", by, extra)
}

func (f *Fake) setStatus(r *Repo, sha string, by *User, state, ctxName, desc, target string) *Status {
	now := f.now()
	s := &Status{ID: f.id(), SHA: sha, State: state, Context: ctxName, Description: desc, TargetURL: target, Creator: by, CreatedAt: now, UpdatedAt: now}
	r.Statuses = append(r.Statuses, s)
	for _, is := range r.Issues {
		if is.Pull != nil && is.Pull.HeadSHA == sha {
			is.Pull.touched()
		}
	}
	f.emit(r, "status", "", by, map[string]any{
		"id": s.ID, "sha": sha, "name": r.FullName(), "state": state, "context": ctxName,
		"description": nullable(desc), "target_url": nullable(target), "avatar_url": nil,
		"created_at": ts(now), "updated_at": ts(now), "branches": []any{},
		"commit": f.commitJSON(r, r.Commits[sha]),
	})
	return s
}

// startRun starts an Actions workflow run with one job (and its check run).
func (f *Fake) startRun(r *Repo, sha, branch, workflow, event string) *WorkflowRun {
	now := f.now()
	suite := &CheckSuite{ID: f.id(), HeadSHA: sha, HeadBranch: branch, Status: "in_progress", CreatedAt: now}
	r.Suites = append(r.Suites, suite)
	w := &WorkflowRun{ID: f.id(), Name: workflow, WorkflowID: int64(len(workflow)) + 9000, SuiteID: suite.ID, HeadSHA: sha, HeadBranch: branch,
		Event: event, Actor: r.Owner, Status: "in_progress", RunAttempt: 1, RunNumber: len(r.Runs) + 1, CreatedAt: now, RunStartedAt: now, UpdatedAt: now}
	r.Runs = append(r.Runs, w)
	return w
}

func (f *Fake) addJob(r *Repo, w *WorkflowRun, name string) *CheckRun {
	now := f.now()
	cr := &CheckRun{ID: f.id(), SuiteID: w.SuiteID, Name: name, HeadSHA: w.HeadSHA, Status: "in_progress", RunID: w.ID, StartedAt: now}
	j := &Job{ID: f.id(), RunID: w.ID, Name: name, Status: "in_progress", CheckRunID: cr.ID, StartedAt: now}
	cr.JobID = j.ID
	r.CheckRuns = append(r.CheckRuns, cr)
	r.Jobs = append(r.Jobs, j)
	return cr
}

// completeCheck finishes a check run and, when it was the run's last open
// job, the workflow run and its suite — emitting check_run, check_suite and
// workflow_run completed.
func (f *Fake) completeCheck(r *Repo, cr *CheckRun, conclusion string) {
	now := f.now()
	cr.Status, cr.Conclusion, cr.CompletedAt = "completed", conclusion, now
	for _, j := range r.Jobs {
		if j.CheckRunID == cr.ID {
			j.Status, j.Conclusion = "completed", conclusion
		}
	}
	for _, is := range r.Issues {
		if is.Pull != nil && is.Pull.HeadSHA == cr.HeadSHA {
			is.Pull.touched()
		}
	}
	f.emit(r, "check_run", "completed", f.appBot(), map[string]any{"check_run": f.checkRunJSON(r, cr)})
	var w *WorkflowRun
	for _, x := range r.Runs {
		if x.ID == cr.RunID {
			w = x
		}
	}
	if w == nil {
		return
	}
	open, worst := false, "success"
	for _, j := range r.Jobs {
		if j.RunID != w.ID {
			continue
		}
		if j.Status != "completed" {
			open = true
		} else if j.Conclusion != "success" && j.Conclusion != "skipped" {
			worst = j.Conclusion
		}
	}
	if !open {
		f.completeRun(r, w, worst)
	}
}

func (f *Fake) completeRun(r *Repo, w *WorkflowRun, conclusion string) {
	now := f.now()
	w.Status, w.Conclusion, w.UpdatedAt = "completed", conclusion, now
	for _, cr := range r.CheckRuns {
		if cr.RunID == w.ID && cr.Status != "completed" {
			cr.Status, cr.Conclusion, cr.CompletedAt = "completed", conclusion, now
		}
	}
	for _, j := range r.Jobs {
		if j.RunID == w.ID && j.Status != "completed" {
			j.Status, j.Conclusion = "completed", conclusion
		}
	}
	for _, s := range r.Suites {
		if s.ID == w.SuiteID {
			s.Status, s.Conclusion = "completed", conclusion
			f.emit(r, "check_suite", "completed", f.appBot(), map[string]any{"check_suite": f.suiteWebhook(r, s)})
		}
	}
	f.emit(r, "workflow_run", "completed", f.appBot(), map[string]any{"workflow_run": f.runJSON(r, w), "workflow": map[string]any{"id": w.WorkflowID, "name": w.Name}})
}

// rerun re-runs a completed workflow run: every job (or only the failed
// ones) gets a FRESH check run on the same head, and the run's attempt
// counter moves.
func (f *Fake) rerun(r *Repo, w *WorkflowRun, failedOnly bool, by *User) {
	w.RunAttempt++
	w.Status, w.Conclusion, w.UpdatedAt = "in_progress", "", f.now()
	for _, j := range append([]*Job(nil), r.Jobs...) {
		if j.RunID != w.ID || (failedOnly && (j.Conclusion == "success" || j.Conclusion == "skipped")) {
			continue
		}
		j.Status, j.Conclusion = "in_progress", ""
		cr := &CheckRun{ID: f.id(), SuiteID: w.SuiteID, Name: j.Name, HeadSHA: w.HeadSHA, Status: "in_progress", RunID: w.ID, JobID: j.ID, StartedAt: f.now()}
		j.CheckRunID = cr.ID
		r.CheckRuns = append(r.CheckRuns, cr)
	}
	for _, s := range r.Suites {
		if s.ID == w.SuiteID {
			s.Status, s.Conclusion = "in_progress", ""
		}
	}
}

func (f *Fake) publishRelease(r *Repo, by *User, o ReleaseOpts) *Release {
	if o.Target == "" {
		o.Target = r.DefaultBranch
	}
	rel := &Release{ID: f.id(), TagName: o.Tag, Name: o.Name, Body: o.Body, Target: o.Target, Draft: o.Draft, Prerelease: o.Prerelease, Author: by, CreatedAt: f.now()}
	r.Releases = append(r.Releases, rel)
	if !rel.Draft {
		f.emit(r, "release", "published", by, map[string]any{"release": f.releaseJSON(r, rel)})
	}
	return rel
}

func (f *Fake) appBot() *User {
	if f.app != nil {
		return f.app.Bot
	}
	return f.addUser("github-actions[bot]", "Bot")
}

// --- gists ---------------------------------------------------------------------

// Gist is a user's gist.
type Gist struct {
	ID          string
	Owner       *User
	Description string
	Public      bool
	Files       map[string]string
	CreatedAt   time.Time
}

func encodeB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func unb64(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(s, "\n", ""))
	return string(b), err
}

func (f *Fake) gistJSON(g *Gist, simple bool) map[string]any {
	files := map[string]any{}
	for name, content := range g.Files {
		fm := map[string]any{"filename": name, "type": "text/plain", "language": nil, "size": len(content),
			"raw_url": fmt.Sprintf("https://gist.github.example/%s/raw/%s", g.ID, name)}
		if simple {
			fm["content"] = content
			fm["truncated"] = false
		}
		files[name] = fm
	}
	api := f.api("/gists/%s", g.ID)
	over := map[string]any{"url": api, "forks_url": api + "/forks", "commits_url": api + "/commits", "id": g.ID,
		"node_id": nodeID("G", int64(len(g.ID))), "git_pull_url": "https://gist.github.example/" + g.ID + ".git",
		"git_push_url": "https://gist.github.example/" + g.ID + ".git", "html_url": "https://gist.github.example/" + g.ID,
		"files": files, "public": g.Public, "created_at": ts(g.CreatedAt), "updated_at": ts(g.CreatedAt),
		"description": nullable(g.Description), "comments": 0, "comments_enabled": true, "user": nil,
		"comments_url": api + "/comments", "owner": f.userJSON(g.Owner), "truncated": false}
	if simple {
		return ShapeOf("gist-simple", over)
	}
	return ShapeOf("base-gist", over)
}

func gistFiles(v any) map[string]string {
	out := map[string]string{}
	m, _ := v.(map[string]any)
	for name, fv := range m {
		fm, _ := fv.(map[string]any)
		if c, ok := fm["content"].(string); ok {
			out[name] = c
		} else if fv == nil {
			out[name] = "\x00delete"
		}
	}
	return out
}

func hListGists(c *call) (int, any) {
	out := []any{}
	for _, k := range sortedKeys(c.f.gists) {
		if g := c.f.gists[k]; lower(g.Owner.Login) == lower(c.actor().Login) {
			out = append(out, c.f.gistJSON(g, false))
		}
	}
	return 200, c.paginate(out)
}

func hUserGists(c *call) (int, any) {
	out := []any{}
	for _, k := range sortedKeys(c.f.gists) {
		if g := c.f.gists[k]; lower(g.Owner.Login) == lower(c.p("username")) && g.Public {
			out = append(out, c.f.gistJSON(g, false))
		}
	}
	return 200, c.paginate(out)
}

func hCreateGist(c *call) (int, any) {
	files := gistFiles(c.body["files"])
	if len(files) == 0 {
		return invalid("Gist", "files", "missing_field", "")
	}
	pub, _ := c.body["public"].(bool)
	g := &Gist{ID: fmt.Sprintf("%x", c.f.id()), Owner: c.actor(), Description: str(c.body, "description"), Public: pub, Files: files, CreatedAt: c.f.now()}
	c.f.gists[g.ID] = g
	return 201, c.f.gistJSON(g, true)
}

func (c *call) gist() *Gist {
	g := c.f.gists[c.p("gist_id")]
	if g == nil || (!g.Public && lower(g.Owner.Login) != lower(c.actor().Login)) {
		return nil
	}
	return g
}

func hGetGist(c *call) (int, any) {
	g := c.gist()
	if g == nil {
		return notFound()
	}
	return 200, c.f.gistJSON(g, true)
}

func hUpdateGist(c *call) (int, any) {
	g := c.gist()
	if g == nil || lower(g.Owner.Login) != lower(c.actor().Login) {
		return notFound()
	}
	if d, ok := c.body["description"].(string); ok {
		g.Description = d
	}
	for name, content := range gistFiles(c.body["files"]) {
		if content == "\x00delete" {
			delete(g.Files, name)
		} else {
			g.Files[name] = content
		}
	}
	return 200, c.f.gistJSON(g, true)
}

func hDeleteGist(c *call) (int, any) {
	g := c.gist()
	if g == nil || lower(g.Owner.Login) != lower(c.actor().Login) {
		return notFound()
	}
	delete(c.f.gists, g.ID)
	return 204, nil
}
