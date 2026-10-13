package ghfake

import (
	"fmt"
	"strings"
	"time"
)

// Object builders: each is ShapeOf(<GitHub's schema>) — every documented
// required field present and type-correct — with the model's real values
// over it. Callers hold f.mu.

// htmlBase is where html_url fields point. Deliberately not github.com: a
// harness asserting that nothing reaches production must not see it.
const htmlBase = "https://github.example"

func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func (f *Fake) api(format string, a ...any) string { return f.baseURL + fmt.Sprintf(format, a...) }

func (f *Fake) userJSON(u *User) map[string]any {
	if u == nil {
		return nil
	}
	api := f.api("/users/%s", u.Login)
	return ShapeOf("simple-user", map[string]any{
		"login": u.Login, "id": u.ID, "node_id": nodeID(nodePrefixUser(u), u.ID),
		"avatar_url": "https://avatars.github.example/u/" + fmt.Sprint(u.ID), "gravatar_id": "",
		"url": api, "html_url": htmlBase + "/" + u.Login,
		"followers_url": api + "/followers", "following_url": api + "/following{/other_user}",
		"gists_url": api + "/gists{/gist_id}", "starred_url": api + "/starred{/owner}{/repo}",
		"subscriptions_url": api + "/subscriptions", "organizations_url": api + "/orgs",
		"repos_url": api + "/repos", "events_url": api + "/events{/privacy}",
		"received_events_url": api + "/received_events", "type": u.Type, "site_admin": false,
		"user_view_type": "public",
	})
}

func nodePrefixUser(u *User) string {
	switch u.Type {
	case "Bot":
		return "BOT"
	case "Organization":
		return "O"
	}
	return "U"
}

func (f *Fake) repoJSON(r *Repo) map[string]any {
	api := f.api("/repos/%s", r.FullName())
	html := htmlBase + "/" + r.FullName()
	open := 0
	for _, is := range r.Issues {
		if is.State == "open" {
			open++
		}
	}
	urls := map[string]any{}
	for _, k := range []string{"archive", "assignees", "blobs", "branches", "collaborators", "comments", "commits",
		"compare", "contents", "contributors", "deployments", "downloads", "events", "forks", "git_commits",
		"git_refs", "git_tags", "hooks", "issue_comment", "issue_events", "issues", "keys", "labels",
		"languages", "merges", "milestones", "notifications", "pulls", "releases", "stargazers", "statuses",
		"subscribers", "subscription", "tags", "teams", "trees"} {
		urls[k+"_url"] = api + "/" + strings.ReplaceAll(k, "_", "/")
	}
	return ShapeOf("repository", merge(urls, map[string]any{
		"id": r.ID, "node_id": nodeID("R", r.ID), "name": r.Name, "full_name": r.FullName(),
		"owner": f.userJSON(r.Owner), "private": r.Private, "html_url": html, "url": api,
		"description": nil, "fork": false, "homepage": nil, "language": "Go",
		"default_branch": r.DefaultBranch, "visibility": visibility(r),
		"clone_url": html + ".git", "git_url": "git://github.example/" + r.FullName() + ".git",
		"ssh_url": "git@github.example:" + r.FullName() + ".git", "svn_url": html, "mirror_url": nil,
		"forks": 0, "forks_count": 0, "open_issues": open, "open_issues_count": open,
		"has_issues": true, "has_projects": true, "has_wiki": false, "has_pages": false,
		"has_downloads": true, "archived": false, "disabled": false, "license": nil,
		"stargazers_count": 0, "watchers": 0, "watchers_count": 0, "size": 1,
		"created_at": ts(r.CreatedAt), "updated_at": ts(r.CreatedAt), "pushed_at": ts(r.CreatedAt),
	}))
}

func visibility(r *Repo) string {
	if r.Private {
		return "private"
	}
	return "public"
}

func (f *Fake) labelJSON(r *Repo, name string) map[string]any {
	l := r.Labels[lower(name)]
	if l == nil {
		l = &Label{ID: f.id(), Name: name, Color: "ededed"}
		r.Labels[lower(name)] = l
	}
	return ShapeOf("label", map[string]any{
		"id": l.ID, "node_id": nodeID("LA", l.ID), "name": l.Name, "color": l.Color,
		"description": nil, "default": false, "url": f.api("/repos/%s/labels/%s", r.FullName(), l.Name),
	})
}

func (f *Fake) labelsJSON(r *Repo, names []string) []any {
	out := []any{}
	for _, n := range names {
		out = append(out, f.labelJSON(r, n))
	}
	return out
}

func (f *Fake) usersJSON(logins []string) []any {
	out := []any{}
	for _, l := range logins {
		if u := f.user(l); u != nil {
			out = append(out, f.userJSON(u))
		}
	}
	return out
}

// association is author_association for u on r.
func association(r *Repo, u *User) string {
	switch {
	case u == nil:
		return "NONE"
	case lower(u.Login) == lower(r.Owner.Login):
		return "OWNER"
	case r.Collaborators[lower(u.Login)]:
		return "COLLABORATOR"
	}
	return "CONTRIBUTOR"
}

func (f *Fake) branchJSON(r *Repo, ref, sha string) map[string]any {
	return map[string]any{
		"label": r.Owner.Login + ":" + ref, "ref": ref, "sha": sha,
		"user": f.userJSON(r.Owner), "repo": f.repoJSON(r),
	}
}

// pullJSON is a pull request; simple selects the list shape.
func (f *Fake) pullJSON(r *Repo, p *Pull, simple bool) map[string]any {
	is := p.Issue
	api := f.api("/repos/%s/pulls/%d", r.FullName(), is.Number)
	html := fmt.Sprintf("%s/%s/pull/%d", htmlBase, r.FullName(), is.Number)
	state := is.State
	var assignee any
	if len(is.Assignees) > 0 {
		assignee = f.userJSON(f.user(is.Assignees[0]))
	}
	teams := []any{}
	for _, t := range p.RequestedTeams {
		teams = append(teams, f.teamJSON(r, t))
	}
	over := map[string]any{
		"url": api, "id": p.ID, "node_id": nodeID("PR", p.ID), "html_url": html,
		"diff_url": html + ".diff", "patch_url": html + ".patch",
		"issue_url":   f.api("/repos/%s/issues/%d", r.FullName(), is.Number),
		"commits_url": api + "/commits", "review_comments_url": api + "/comments",
		"review_comment_url": f.api("/repos/%s/pulls/comments{/number}", r.FullName()),
		"comments_url":       f.api("/repos/%s/issues/%d/comments", r.FullName(), is.Number),
		"statuses_url":       f.api("/repos/%s/statuses/%s", r.FullName(), p.HeadSHA),
		"number":             is.Number, "state": state, "locked": is.Locked, "title": is.Title,
		"user": f.userJSON(is.User), "body": nullable(is.Body), "labels": f.labelsJSON(r, is.Labels),
		"milestone": nil, "active_lock_reason": nil,
		"created_at": ts(is.CreatedAt), "updated_at": ts(is.UpdatedAt), "closed_at": ts(is.ClosedAt),
		"merged_at": ts(p.MergedAt), "merge_commit_sha": nullable(p.MergeCommitSHA),
		"assignee": assignee, "assignees": f.usersJSON(is.Assignees),
		"requested_reviewers": f.usersJSON(p.RequestedUsers), "requested_teams": teams,
		"head":  f.branchJSON(r, p.HeadRef, p.HeadSHA),
		"base":  f.branchJSON(r, p.BaseRef, p.MergeBase),
		"draft": p.Draft, "author_association": association(r, is.User), "auto_merge": nil,
		"_links": map[string]any{
			"self": link(api), "html": link(html), "issue": link(f.api("/repos/%s/issues/%d", r.FullName(), is.Number)),
			"comments":        link(f.api("/repos/%s/issues/%d/comments", r.FullName(), is.Number)),
			"review_comments": link(api + "/comments"), "review_comment": link(f.api("/repos/%s/pulls/comments{/number}", r.FullName())),
			"commits": link(api + "/commits"), "statuses": link(f.api("/repos/%s/statuses/%s", r.FullName(), p.HeadSHA)),
		},
	}
	if simple {
		return ShapeOf("pull-request-simple", over)
	}
	mergeable, mstate := f.mergeState(r, p)
	var m any
	if mergeable != nil {
		m = *mergeable
	}
	adds, dels := 0, 0
	for _, fl := range p.Files {
		adds += fl.Additions
		dels += fl.Deletions
	}
	over["merged"] = p.Merged
	over["mergeable"] = m
	over["rebaseable"] = m
	over["mergeable_state"] = mstate
	over["merged_by"] = f.userJSON(p.MergedBy)
	over["comments"] = len(is.Comments)
	over["review_comments"] = len(p.ReviewComments)
	over["maintainer_can_modify"] = false
	over["commits"] = 1
	over["additions"] = adds
	over["deletions"] = dels
	over["changed_files"] = len(p.Files)
	return ShapeOf("pull-request", over)
}

func link(href string) map[string]any { return map[string]any{"href": href} }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (f *Fake) teamJSON(r *Repo, slug string) map[string]any {
	return ShapeOf("team", map[string]any{
		"id": int64(len(slug)) + 7000, "node_id": nodeID("T", int64(len(slug))+7000), "name": slug, "slug": slug,
		"url": f.api("/orgs/%s/teams/%s", r.Owner.Login, slug), "html_url": htmlBase + "/orgs/" + r.Owner.Login + "/teams/" + slug,
		"members_url":      f.api("/orgs/%s/teams/%s/members{/member}", r.Owner.Login, slug),
		"repositories_url": f.api("/orgs/%s/teams/%s/repos", r.Owner.Login, slug),
		"permission":       "push", "description": nil, "parent": nil,
	})
}

func (f *Fake) issueJSON(r *Repo, is *Issue) map[string]any {
	api := f.api("/repos/%s/issues/%d", r.FullName(), is.Number)
	html := fmt.Sprintf("%s/%s/issues/%d", htmlBase, r.FullName(), is.Number)
	var assignee any
	if len(is.Assignees) > 0 {
		assignee = f.userJSON(f.user(is.Assignees[0]))
	}
	over := map[string]any{
		"id": is.ID, "node_id": nodeID("I", is.ID), "url": api, "html_url": html,
		"repository_url": f.api("/repos/%s", r.FullName()), "labels_url": api + "/labels{/name}",
		"comments_url": api + "/comments", "events_url": api + "/events",
		"number": is.Number, "state": is.State, "state_reason": nullable(is.StateReason), "title": is.Title,
		"body": nullable(is.Body), "user": f.userJSON(is.User), "labels": f.labelsJSON(r, is.Labels),
		"assignee": assignee, "assignees": f.usersJSON(is.Assignees), "milestone": nil, "locked": is.Locked,
		"active_lock_reason": nil, "comments": len(is.Comments),
		"closed_at": ts(is.ClosedAt), "created_at": ts(is.CreatedAt), "updated_at": ts(is.UpdatedAt),
		"author_association": association(r, is.User),
		"reactions":          f.rollup(api+"/reactions", nil),
	}
	if is.Pull != nil {
		ph := fmt.Sprintf("%s/%s/pull/%d", htmlBase, r.FullName(), is.Number)
		over["html_url"] = ph
		over["pull_request"] = map[string]any{
			"url": f.api("/repos/%s/pulls/%d", r.FullName(), is.Number), "html_url": ph,
			"diff_url": ph + ".diff", "patch_url": ph + ".patch", "merged_at": ts(is.Pull.MergedAt),
		}
		over["draft"] = is.Pull.Draft
	}
	return ShapeOf("issue", over)
}

func (f *Fake) rollup(url string, rs []*Reaction) map[string]any {
	counts := map[string]any{"url": url, "total_count": len(rs)}
	for _, c := range []string{"+1", "-1", "laugh", "confused", "heart", "hooray", "eyes", "rocket"} {
		counts[c] = 0
	}
	for _, x := range rs {
		counts[x.Content] = counts[x.Content].(int) + 1
	}
	return ShapeOf("reaction-rollup", counts)
}

func (f *Fake) issueCommentJSON(r *Repo, c *IssueComment) map[string]any {
	api := f.api("/repos/%s/issues/comments/%d", r.FullName(), c.ID)
	kind := "issues"
	if c.Issue.Pull != nil {
		kind = "pull"
	}
	return ShapeOf("issue-comment", map[string]any{
		"id": c.ID, "node_id": nodeID("IC", c.ID), "url": api,
		"html_url":  fmt.Sprintf("%s/%s/%s/%d#issuecomment-%d", htmlBase, r.FullName(), kind, c.Issue.Number, c.ID),
		"issue_url": f.api("/repos/%s/issues/%d", r.FullName(), c.Issue.Number),
		"body":      c.Body, "user": f.userJSON(c.User),
		"created_at": ts(c.CreatedAt), "updated_at": ts(c.UpdatedAt),
		"author_association": association(r, c.User), "reactions": f.rollup(api+"/reactions", c.Reactions),
	})
}

func (f *Fake) reactionJSON(x *Reaction) map[string]any {
	return ShapeOf("reaction", map[string]any{
		"id": x.ID, "node_id": nodeID("REA", x.ID), "user": f.userJSON(x.User),
		"content": x.Content, "created_at": ts(x.CreatedAt),
	})
}

func (f *Fake) reviewJSON(r *Repo, p *Pull, rv *Review) map[string]any {
	html := fmt.Sprintf("%s/%s/pull/%d#pullrequestreview-%d", htmlBase, r.FullName(), p.Issue.Number, rv.ID)
	return ShapeOf("pull-request-review", map[string]any{
		"id": rv.ID, "node_id": nodeID("PRR", rv.ID), "user": f.userJSON(rv.User), "body": rv.Body,
		"state": rv.State, "html_url": html, "commit_id": rv.CommitID,
		"pull_request_url": f.api("/repos/%s/pulls/%d", r.FullName(), p.Issue.Number),
		"submitted_at":     ts(rv.SubmittedAt), "author_association": association(r, rv.User),
		"_links": map[string]any{"html": link(html), "pull_request": link(f.api("/repos/%s/pulls/%d", r.FullName(), p.Issue.Number))},
	})
}

// reviewCommentJSON is an inline comment; schema is pull-request-review-comment
// (the PR's comment list, a reply) or review-comment (a review's comments).
func (f *Fake) reviewCommentJSON(r *Repo, p *Pull, c *ReviewComment, schema string) map[string]any {
	api := f.api("/repos/%s/pulls/comments/%d", r.FullName(), c.ID)
	html := fmt.Sprintf("%s/%s/pull/%d#discussion_r%d", htmlBase, r.FullName(), p.Issue.Number, c.ID)
	prURL := f.api("/repos/%s/pulls/%d", r.FullName(), p.Issue.Number)
	var line, oline, pos any = c.Line, c.OriginalLine, c.Line
	if c.Thread != nil && c.Thread.Outdated {
		line, pos = nil, nil
	}
	over := map[string]any{
		"url": api, "id": c.ID, "node_id": nodeID("PRRC", c.ID), "pull_request_review_id": c.ReviewID,
		"diff_hunk": fmt.Sprintf("@@ -%d,1 +%d,1 @@", c.OriginalLine, c.OriginalLine), "path": c.Path,
		"position": pos, "original_position": c.OriginalLine, "line": line, "original_line": oline,
		"side": "RIGHT", "start_line": nil, "original_start_line": nil, "start_side": nil,
		"commit_id": c.CommitID, "original_commit_id": c.OriginalCommitID, "user": f.userJSON(c.User),
		"body": c.Body, "created_at": ts(c.CreatedAt), "updated_at": ts(c.UpdatedAt),
		"html_url": html, "pull_request_url": prURL, "author_association": association(r, c.User),
		"subject_type": "line", "reactions": f.rollup(api+"/reactions", c.Reactions),
		"_links": map[string]any{"self": link(api), "html": link(html), "pull_request": link(prURL)},
	}
	if c.InReplyTo != 0 {
		over["in_reply_to_id"] = c.InReplyTo
	}
	return ShapeOf(schema, over)
}

func (f *Fake) statusJSON(r *Repo, s *Status) map[string]any {
	return ShapeOf("status", map[string]any{
		"url": f.api("/repos/%s/statuses/%s", r.FullName(), s.SHA), "avatar_url": nil,
		"id": s.ID, "node_id": nodeID("SC", s.ID), "state": s.State, "description": nullable(s.Description),
		"target_url": nullable(s.TargetURL), "context": s.Context,
		"created_at": ts(s.CreatedAt), "updated_at": ts(s.UpdatedAt), "creator": f.userJSON(s.Creator),
	})
}

func (f *Fake) appJSON() map[string]any {
	a := f.app
	if a == nil {
		return nil
	}
	return ShapeOf("integration", map[string]any{
		"id": a.ID, "slug": a.Slug, "node_id": nodeID("A", a.ID), "name": a.Name, "owner": f.userJSON(a.Bot),
		"description": nil, "external_url": htmlBase, "html_url": htmlBase + "/apps/" + a.Slug,
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
		"permissions": map[string]any{"checks": "write", "contents": "write", "pull_requests": "write", "issues": "write"},
		"events":      []any{"pull_request", "check_run"},
	})
}

func (f *Fake) suiteJSON(r *Repo, s *CheckSuite) map[string]any {
	if s == nil {
		return map[string]any{"id": 0}
	}
	return map[string]any{"id": s.ID}
}

func (f *Fake) prRefs(r *Repo, sha string) []any {
	out := []any{}
	for _, n := range sortedIssueNumbers(r) {
		p := r.Issues[n].Pull
		if p == nil || p.HeadSHA != sha || p.Issue.State != "open" {
			continue
		}
		out = append(out, ShapeOf("pull-request-minimal", map[string]any{
			"id": p.ID, "number": n, "url": f.api("/repos/%s/pulls/%d", r.FullName(), n),
			"head": map[string]any{"ref": p.HeadRef, "sha": p.HeadSHA, "repo": map[string]any{"id": r.ID, "url": f.api("/repos/%s", r.FullName()), "name": r.Name}},
			"base": map[string]any{"ref": p.BaseRef, "sha": p.MergeBase, "repo": map[string]any{"id": r.ID, "url": f.api("/repos/%s", r.FullName()), "name": r.Name}},
		}))
	}
	return out
}

func (f *Fake) checkRunJSON(r *Repo, c *CheckRun) map[string]any {
	html := fmt.Sprintf("%s/%s/runs/%d", htmlBase, r.FullName(), c.ID)
	details := html
	if c.RunID != 0 {
		details = fmt.Sprintf("%s/%s/actions/runs/%d/job/%d", htmlBase, r.FullName(), c.RunID, c.JobID)
	}
	var suite *CheckSuite
	for _, s := range r.Suites {
		if s.ID == c.SuiteID {
			suite = s
		}
	}
	return ShapeOf("check-run", map[string]any{
		"id": c.ID, "node_id": nodeID("CR", c.ID), "head_sha": c.HeadSHA, "name": c.Name,
		"external_id": "", "url": f.api("/repos/%s/check-runs/%d", r.FullName(), c.ID), "html_url": html,
		"details_url": details, "status": c.Status, "conclusion": nullable(c.Conclusion),
		"started_at": ts(c.StartedAt), "completed_at": ts(c.CompletedAt),
		"output": map[string]any{"title": nil, "summary": nil, "text": nil, "annotations_count": 0,
			"annotations_url": f.api("/repos/%s/check-runs/%d/annotations", r.FullName(), c.ID)},
		"check_suite": f.suiteJSON(r, suite), "app": f.appJSON(), "pull_requests": f.prRefs(r, c.HeadSHA),
	})
}

func (f *Fake) runJSON(r *Repo, w *WorkflowRun) map[string]any {
	api := f.api("/repos/%s/actions/runs/%d", r.FullName(), w.ID)
	head := r.Commits[w.HeadSHA]
	var hc any
	if head != nil {
		hc = f.simpleCommitJSON(head)
	}
	return ShapeOf("workflow-run", map[string]any{
		"id": w.ID, "name": w.Name, "node_id": nodeID("WFR", w.ID), "check_suite_id": w.SuiteID,
		"check_suite_node_id": nodeID("CS", w.SuiteID), "head_branch": w.HeadBranch, "head_sha": w.HeadSHA,
		"path": ".github/workflows/" + lower(w.Name) + ".yml", "run_number": w.RunNumber, "run_attempt": w.RunAttempt,
		"display_title": w.Name, "event": w.Event, "status": w.Status, "conclusion": nullable(w.Conclusion),
		"workflow_id": w.WorkflowID, "url": api, "html_url": fmt.Sprintf("%s/%s/actions/runs/%d", htmlBase, r.FullName(), w.ID),
		"pull_requests": f.prRefs(r, w.HeadSHA), "created_at": ts(w.CreatedAt), "updated_at": ts(w.UpdatedAt),
		"run_started_at": ts(w.RunStartedAt), "jobs_url": api + "/jobs", "logs_url": api + "/logs",
		"check_suite_url": f.api("/repos/%s/check-suites/%d", r.FullName(), w.SuiteID), "artifacts_url": api + "/artifacts",
		"cancel_url": api + "/cancel", "rerun_url": api + "/rerun", "previous_attempt_url": nil,
		"workflow_url": f.api("/repos/%s/actions/workflows/%d", r.FullName(), w.WorkflowID),
		"head_commit":  hc, "repository": f.minimalRepoJSON(r), "head_repository": f.minimalRepoJSON(r),
		"actor": f.userJSON(w.Actor), "triggering_actor": f.userJSON(w.Actor),
	})
}

func (f *Fake) minimalRepoJSON(r *Repo) map[string]any {
	full := f.repoJSON(r)
	return ShapeOf("minimal-repository", full)
}

func (f *Fake) simpleCommitJSON(c *Commit) map[string]any {
	who := map[string]any{"name": c.Author.Login, "email": c.Author.Email}
	return ShapeOf("simple-commit", map[string]any{
		"id": c.SHA, "tree_id": c.SHA, "message": c.Message, "timestamp": ts(c.At), "author": who, "committer": who,
	})
}

func (f *Fake) jobJSON(r *Repo, j *Job) map[string]any {
	var sha string
	for _, w := range r.Runs {
		if w.ID == j.RunID {
			sha = w.HeadSHA
		}
	}
	return ShapeOf("job", map[string]any{
		"id": j.ID, "run_id": j.RunID, "run_url": f.api("/repos/%s/actions/runs/%d", r.FullName(), j.RunID),
		"node_id": nodeID("CR", j.CheckRunID), "head_sha": sha, "name": j.Name, "status": j.Status,
		"conclusion": nullable(j.Conclusion), "url": f.api("/repos/%s/actions/jobs/%d", r.FullName(), j.ID),
		"html_url":      fmt.Sprintf("%s/%s/actions/runs/%d/job/%d", htmlBase, r.FullName(), j.RunID, j.ID),
		"check_run_url": f.api("/repos/%s/check-runs/%d", r.FullName(), j.CheckRunID),
		"started_at":    ts(j.StartedAt), "completed_at": nil, "labels": []any{"ubuntu-latest"},
		"runner_id": nil, "runner_name": nil, "runner_group_id": nil, "runner_group_name": nil,
		"workflow_name": nil, "head_branch": nil, "created_at": ts(j.StartedAt),
	})
}

func (f *Fake) commitJSON(r *Repo, c *Commit) map[string]any {
	who := map[string]any{"name": c.Author.Login, "email": c.Author.Email, "date": ts(c.At)}
	parents := []any{}
	for _, p := range c.Parents {
		parents = append(parents, map[string]any{"sha": p, "url": f.api("/repos/%s/commits/%s", r.FullName(), p),
			"html_url": fmt.Sprintf("%s/%s/commit/%s", htmlBase, r.FullName(), p)})
	}
	return ShapeOf("commit", map[string]any{
		"url": f.api("/repos/%s/commits/%s", r.FullName(), c.SHA), "sha": c.SHA, "node_id": nodeID("C", int64(len(c.SHA))),
		"html_url":     fmt.Sprintf("%s/%s/commit/%s", htmlBase, r.FullName(), c.SHA),
		"comments_url": f.api("/repos/%s/commits/%s/comments", r.FullName(), c.SHA),
		"commit": map[string]any{"url": f.api("/repos/%s/git/commits/%s", r.FullName(), c.SHA), "author": who, "committer": who,
			"message": c.Message, "comment_count": 0,
			"tree": map[string]any{"sha": c.SHA, "url": f.api("/repos/%s/git/trees/%s", r.FullName(), c.SHA)}},
		"author": f.userJSON(c.Author), "committer": f.userJSON(c.Author), "parents": parents,
	})
}

func (f *Fake) installationJSON(in *Installation) map[string]any {
	sel := "all"
	if len(in.Repos) > 0 {
		sel = "selected"
	}
	return ShapeOf("installation", map[string]any{
		"id": in.ID, "account": f.userJSON(in.Account), "app_id": f.app.ID, "app_slug": f.app.Slug,
		"target_id": in.Account.ID, "target_type": in.Account.Type, "repository_selection": sel,
		"access_tokens_url": f.api("/app/installations/%d/access_tokens", in.ID),
		"repositories_url":  f.api("/installation/repositories"), "html_url": htmlBase + "/settings/installations/" + fmt.Sprint(in.ID),
		"permissions": map[string]any{"checks": "write", "contents": "write", "pull_requests": "write", "issues": "write"},
		"events":      []any{"pull_request", "check_run"}, "single_file_name": nil,
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z", "suspended_by": nil, "suspended_at": nil,
	})
}

func (f *Fake) releaseJSON(r *Repo, rel *Release) map[string]any {
	api := f.api("/repos/%s/releases/%d", r.FullName(), rel.ID)
	return ShapeOf("release", map[string]any{
		"url": api, "html_url": fmt.Sprintf("%s/%s/releases/tag/%s", htmlBase, r.FullName(), rel.TagName),
		"assets_url": api + "/assets", "upload_url": f.api("/repos/%s/releases/%d/assets{?name,label}", r.FullName(), rel.ID),
		"tarball_url": nil, "zipball_url": nil, "id": rel.ID, "node_id": nodeID("RE", rel.ID),
		"tag_name": rel.TagName, "target_commitish": rel.Target, "name": nullable(rel.Name), "body": nullable(rel.Body),
		"draft": rel.Draft, "prerelease": rel.Prerelease, "created_at": ts(rel.CreatedAt), "published_at": ts(rel.CreatedAt),
		"author": f.userJSON(rel.Author), "assets": []any{},
	})
}
