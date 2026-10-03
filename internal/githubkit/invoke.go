package githubkit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Invoke runs one github verb against opts (the same shape a connector/plugin
// receives as its verb call options) and returns its outputs. It does not
// know about the daemon-global "sweep" verb — a caller that wants to support
// it must intercept that verb before calling Invoke.
func (c *Client) Invoke(ctx context.Context, verb string, opts map[string]any) (map[string]any, error) {
	repo, _ := opts["repo"].(string)
	// Gists are user-scoped, not repo-scoped — they don't require a repo.
	if repo == "" && !isGistVerb(verb) {
		return nil, fmt.Errorf("github.%s: options.repo is required", verb)
	}
	if verb == "read_token" || verb == "write_token" {
		// The credentials agents receive (mints_credential, host-only): each
		// resolves by its own identity policy, not the call's `as`.
		tok, err := c.MintCredential(ctx, strings.TrimSuffix(verb, "_token"), repo)
		if err != nil {
			return nil, fmt.Errorf("github.%s: %w", verb, err)
		}
		return map[string]any{"token": tok}, nil
	}
	as, _ := opts["as"].(string)
	tok, err := c.TokenFor(ctx, as, repo)
	if err != nil {
		return nil, fmt.Errorf("github.%s: %w", verb, err)
	}
	number := toInt(opts["number"])
	if number == 0 {
		number = toInt(opts["pr"])
	}
	base := c.base()
	switch verb {
	case "comment":
		if number == 0 {
			return nil, fmt.Errorf("github.comment: options.number (or pr) is required")
		}
		var out struct {
			ID      int64  `json:"id"`
			HTMLURL string `json:"html_url"`
		}
		err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/issues/%d/comments", base, repo, number),
			map[string]any{"body": opts["body"]}, &out)
		if err != nil {
			return nil, remapTargetGone(err)
		}
		return map[string]any{"id": out.ID, "url": out.HTMLURL}, nil
	case "reply":
		id := toInt(opts["in_reply_to"])
		if number == 0 || id == 0 {
			return nil, fmt.Errorf("github.reply: options.pr and options.in_reply_to are required")
		}
		var out struct {
			ID      int64  `json:"id"`
			HTMLURL string `json:"html_url"`
		}
		err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d/comments/%d/replies", base, repo, number, id),
			map[string]any{"body": opts["body"]}, &out)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": out.ID, "url": out.HTMLURL}, nil
	case "request_review", "rerequest_review", "remove_reviewer":
		if number == 0 {
			return nil, fmt.Errorf("github.%s: options.pr is required", verb)
		}
		rs := reviewerLogins(opts["reviewers"])
		if verb == "rerequest_review" {
			// A re-request pings back whoever reviewed — typically "{{.author}}"
			// — and review bots (Cursor Bugbot, …) can't be requested: GitHub
			// 422s the WHOLE request, so one bot would also drop the humans.
			rs = dropBots(rs)
		}
		if verb != "remove_reviewer" && len(rs) > 0 {
			rs = dropPRAuthor(ctx, c, tok, base, repo, number, rs)
		}
		if verb == "rerequest_review" && len(rs) > 0 && optBool(opts, "only_outstanding", true) {
			var why string
			if rs, why = outstandingReviewers(ctx, c, tok, base, repo, number, rs); len(rs) == 0 {
				if len(reviewerLogins(opts["team_reviewers"])) == 0 {
					return map[string]any{"ok": true, "skipped": why}, nil
				}
			}
		}
		body := map[string]any{}
		if len(rs) > 0 {
			body["reviewers"] = rs
		}
		if ts := reviewerLogins(opts["team_reviewers"]); len(ts) > 0 {
			body["team_reviewers"] = ts
		}
		if len(body) == 0 {
			if opts["reviewers"] != nil || opts["team_reviewers"] != nil {
				// Reviewers were configured but every entry rendered empty or
				// was the PR author — typically a "{{.author}}" template on an
				// event that carries no reviewer. There is nobody to ping: skip
				// rather than POST a bogus login or fail the flow over it.
				return map[string]any{"ok": true, "skipped": "no reviewer resolved"}, nil
			}
			return nil, fmt.Errorf("github.%s: set options.reviewers and/or team_reviewers", verb)
		}
		u := fmt.Sprintf("%s/repos/%s/pulls/%d/requested_reviewers", base, repo, number)
		// Same endpoint: POST requests reviewers (and re-requests a prior one),
		// DELETE cancels a pending request.
		var err error
		if verb == "remove_reviewer" {
			err = c.del(ctx, tok, u, body)
		} else {
			err = c.post(ctx, tok, u, body, nil)
		}
		if err != nil && verb == "rerequest_review" && notCollaborator(err) {
			// The reviewer can't be re-requested (left the org, an outside
			// reviewer, a bot we couldn't recognise by login). The ping is a
			// courtesy after the real work (the fix) already landed: skip it
			// rather than fail the flow. An explicit request_review still errors.
			return map[string]any{"ok": true, "skipped": "reviewer is not a collaborator"}, nil
		}
		if err != nil {
			return nil, remapTargetGone(err)
		}
		return map[string]any{"ok": true}, nil
	case "submit_review":
		if number == 0 {
			return nil, fmt.Errorf("github.submit_review: options.pr is required")
		}
		event, _ := opts["event"].(string)
		if event == "" {
			return nil, fmt.Errorf("github.submit_review: options.event (APPROVE|REQUEST_CHANGES|COMMENT) is required")
		}
		var out struct {
			ID int64 `json:"id"`
		}
		body := map[string]any{"event": event}
		if b, _ := opts["body"].(string); b != "" {
			body["body"] = b
		}
		comments, err := ReviewComments(opts["comments"])
		if err != nil {
			return nil, err
		}
		if len(comments) > 0 {
			body["comments"] = comments
		}
		if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d/reviews", base, repo, number), body, &out); err != nil {
			return nil, remapTargetGone(err)
		}
		return map[string]any{"id": out.ID, "comments": len(comments)}, nil
	case "pr_head":
		// The revision read the engine's run facts take (reads_revision):
		// uncached, since a caller compares heads across a push.
		if number == 0 {
			return nil, fmt.Errorf("github.pr_head: options.pr is required")
		}
		sha, state, err := c.PRHead(ctx, as, repo, number)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sha": sha, "state": state}, nil
	case "pr_diff":
		if number == 0 {
			return nil, fmt.Errorf("github.pr_diff: options.pr is required")
		}
		diff, err := c.getText(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", base, repo, number), "application/vnd.github.diff")
		if err != nil {
			return nil, remapTargetGone(err)
		}
		return map[string]any{"diff": diff}, nil
	case "pr_get":
		if number == 0 {
			return nil, fmt.Errorf("github.pr_get: options.pr is required")
		}
		var pr struct {
			Title        string `json:"title"`
			Body         string `json:"body"`
			State        string `json:"state"`
			Draft        bool   `json:"draft"`
			Merged       bool   `json:"merged"`
			Mergeable    *bool  `json:"mergeable"`
			Additions    int    `json:"additions"`
			Deletions    int    `json:"deletions"`
			ChangedFiles int    `json:"changed_files"`
			HTMLURL      string `json:"html_url"`
			User         struct {
				Login string `json:"login"`
			} `json:"user"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
			Head struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		}
		if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", base, repo, number), &pr); err != nil {
			return nil, remapTargetGone(err)
		}
		labels := make([]string, 0, len(pr.Labels))
		for _, l := range pr.Labels {
			labels = append(labels, l.Name)
		}
		res := map[string]any{
			"title": pr.Title, "body": pr.Body, "state": pr.State, "draft": pr.Draft,
			"merged": pr.Merged, "author": pr.User.Login, "base": pr.Base.Ref,
			"head": pr.Head.Ref, "head_sha": pr.Head.SHA, "additions": pr.Additions,
			"deletions": pr.Deletions, "changed_files": pr.ChangedFiles, "labels": labels, "url": pr.HTMLURL,
		}
		if pr.Mergeable != nil {
			res["mergeable"] = *pr.Mergeable
		}
		// Review status: fold the review list into the current decision. GitHub's
		// REST API has no reviewDecision field, so derive it from the LATEST review
		// per reviewer — APPROVED/CHANGES_REQUESTED/DISMISSED count, COMMENTED/PENDING
		// don't change a reviewer's standing. (Required-reviewer/CODEOWNERS gating is
		// GraphQL-only and not modelled here; this is "did a human approve / request
		// changes", which is what a review hand-off watches.)
		latest := map[string]string{}
		order := []string{}
		err := c.listAll(ctx, tok, true, 100, func(page int) string {
			return fmt.Sprintf("%s/repos/%s/pulls/%d/reviews?per_page=100&page=%d", base, repo, number, page)
		}, func(b []byte) (int, error) {
			var raw []struct {
				User  struct{ Login string } `json:"user"`
				State string                 `json:"state"`
			}
			if uerr := json.Unmarshal(b, &raw); uerr != nil {
				return 0, uerr
			}
			for _, rv := range raw {
				switch rv.State {
				case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
					if _, seen := latest[rv.User.Login]; !seen {
						order = append(order, rv.User.Login)
					}
					latest[rv.User.Login] = rv.State
				}
			}
			return len(raw), nil
		})
		if err != nil {
			return nil, remapTargetGone(err)
		}
		approvers := []string{}
		changesRequested := false
		for _, login := range order {
			switch latest[login] {
			case "APPROVED":
				approvers = append(approvers, login)
			case "CHANGES_REQUESTED":
				changesRequested = true
			}
		}
		decision := "REVIEW_REQUIRED"
		if changesRequested {
			decision = "CHANGES_REQUESTED"
		} else if len(approvers) > 0 {
			decision = "APPROVED"
		}
		res["approvers"] = approvers
		res["approvals"] = len(approvers)
		res["review_decision"] = decision
		return res, nil
	case "pr_files":
		if number == 0 {
			return nil, fmt.Errorf("github.pr_files: options.pr is required")
		}
		all, _ := opts["all"].(bool)
		files := []any{}
		err := c.listAll(ctx, tok, all, 100, func(page int) string {
			return fmt.Sprintf("%s/repos/%s/pulls/%d/files?per_page=100&page=%d", base, repo, number, page)
		}, func(b []byte) (int, error) {
			var raw []struct {
				Filename  string `json:"filename"`
				Status    string `json:"status"`
				Additions int    `json:"additions"`
				Deletions int    `json:"deletions"`
				Changes   int    `json:"changes"`
			}
			if err := json.Unmarshal(b, &raw); err != nil {
				return 0, err
			}
			for _, f := range raw {
				files = append(files, map[string]any{
					"path": f.Filename, "status": f.Status,
					"additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes,
				})
			}
			return len(raw), nil
		})
		if err != nil {
			return nil, remapTargetGone(err)
		}
		return map[string]any{"files": files}, nil
	case "review_comments":
		if number == 0 {
			return nil, fmt.Errorf("github.review_comments: options.pr is required")
		}
		all, _ := opts["all"].(bool)
		comments := []any{}
		err := c.listAll(ctx, tok, all, 100, func(page int) string {
			return fmt.Sprintf("%s/repos/%s/pulls/%d/comments?per_page=100&page=%d", base, repo, number, page)
		}, func(b []byte) (int, error) {
			var raw []struct {
				ID           int64  `json:"id"`
				Path         string `json:"path"`
				Line         int    `json:"line"`
				OriginalLine int    `json:"original_line"`
				Body         string `json:"body"`
				User         struct {
					Login string `json:"login"`
				} `json:"user"`
			}
			if err := json.Unmarshal(b, &raw); err != nil {
				return 0, err
			}
			for _, cc := range raw {
				line := cc.Line
				if line == 0 {
					line = cc.OriginalLine
				}
				comments = append(comments, map[string]any{
					"id": cc.ID, "path": cc.Path, "line": line, "body": cc.Body, "user": cc.User.Login,
				})
			}
			return len(raw), nil
		})
		if err != nil {
			return nil, remapTargetGone(err)
		}
		return map[string]any{"comments": comments}, nil
	case "file":
		path, _ := opts["path"].(string)
		if path == "" {
			return nil, fmt.Errorf("github.file: options.path is required")
		}
		u := fmt.Sprintf("%s/repos/%s/contents/%s", base, repo, escapePath(path))
		if ref, _ := opts["ref"].(string); ref != "" {
			u += "?ref=" + url.QueryEscape(ref)
		}
		text, err := c.getText(ctx, tok, u, "application/vnd.github.raw")
		if err != nil {
			// optional: a missing file (404) is not an error — return empty
			// text so a workflow can inline convention files that may not exist
			// without a failed step in the logs.
			if opt, _ := opts["optional"].(bool); opt && strings.Contains(err.Error(), "HTTP 404") {
				return map[string]any{"text": ""}, nil
			}
			return nil, err
		}
		return map[string]any{"text": text}, nil
	case "create_pr":
		title, _ := opts["title"].(string)
		head, _ := opts["head"].(string)
		baseRef, _ := opts["base"].(string)
		if title == "" || head == "" || baseRef == "" {
			return nil, fmt.Errorf("github.create_pr: title, head and base are required")
		}
		reqBody := map[string]any{"title": title, "head": head, "base": baseRef}
		if b, _ := opts["body"].(string); b != "" {
			reqBody["body"] = b
		}
		if d, _ := opts["draft"].(bool); d {
			reqBody["draft"] = true
		}
		var out struct {
			Number  int64  `json:"number"`
			HTMLURL string `json:"html_url"`
		}
		if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls", base, repo), reqBody, &out); err != nil {
			return nil, err
		}
		return map[string]any{"number": out.Number, "url": out.HTMLURL}, nil
	case "merge_pr":
		if number == 0 {
			return nil, fmt.Errorf("github.merge_pr: options.pr is required")
		}
		if err := c.checkMergeReady(ctx, tok, base, repo, number); err != nil {
			return nil, err
		}
		reqBody := map[string]any{}
		if m, _ := opts["method"].(string); m != "" {
			reqBody["merge_method"] = m
		}
		if s, _ := opts["commit_title"].(string); s != "" {
			reqBody["commit_title"] = s
		}
		if s, _ := opts["commit_message"].(string); s != "" {
			reqBody["commit_message"] = s
		}
		if s, _ := opts["sha"].(string); s != "" {
			reqBody["sha"] = s
		}
		var out struct {
			Merged bool   `json:"merged"`
			SHA    string `json:"sha"`
		}
		if err := c.put(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d/merge", base, repo, number), reqBody, &out); err != nil {
			// The common cases (already merged/closed, mergeability still
			// computing) were already caught by checkMergeReady above; this
			// only remaps the race where the PR went away in between.
			return nil, remapTargetGone(err)
		}
		return map[string]any{"merged": out.Merged, "sha": out.SHA}, nil
	case "update_pr":
		if number == 0 {
			return nil, fmt.Errorf("github.update_pr: options.pr is required")
		}
		reqBody := stringFields(opts, "state", "title", "body", "base")
		if len(reqBody) == 0 {
			return nil, fmt.Errorf("github.update_pr: nothing to change (set state/title/body/base)")
		}
		var out struct {
			Number int64  `json:"number"`
			State  string `json:"state"`
		}
		if err := c.patch(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", base, repo, number), reqBody, &out); err != nil {
			return nil, remapTargetGone(err)
		}
		return map[string]any{"number": out.Number, "state": out.State}, nil
	case "create_issue":
		title, _ := opts["title"].(string)
		if title == "" {
			return nil, fmt.Errorf("github.create_issue: options.title is required")
		}
		reqBody := map[string]any{"title": title}
		if b, _ := opts["body"].(string); b != "" {
			reqBody["body"] = b
		}
		if l := toStrings(opts["labels"]); len(l) > 0 {
			reqBody["labels"] = l
		}
		if a := toStrings(opts["assignees"]); len(a) > 0 {
			reqBody["assignees"] = a
		}
		var out struct {
			Number  int64  `json:"number"`
			HTMLURL string `json:"html_url"`
		}
		if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/issues", base, repo), reqBody, &out); err != nil {
			return nil, err
		}
		return map[string]any{"number": out.Number, "url": out.HTMLURL}, nil
	case "update_issue":
		if number == 0 {
			return nil, fmt.Errorf("github.update_issue: options.number is required")
		}
		reqBody := stringFields(opts, "state", "state_reason", "title", "body")
		if len(reqBody) == 0 {
			return nil, fmt.Errorf("github.update_issue: nothing to change")
		}
		var out struct {
			Number int64  `json:"number"`
			State  string `json:"state"`
		}
		if err := c.patch(ctx, tok, fmt.Sprintf("%s/repos/%s/issues/%d", base, repo, number), reqBody, &out); err != nil {
			return nil, remapTargetGone(err)
		}
		return map[string]any{"number": out.Number, "state": out.State}, nil
	case "assign":
		if number == 0 {
			return nil, fmt.Errorf("github.assign: options.number (or pr) is required")
		}
		add, rem := toStrings(opts["add"]), toStrings(opts["remove"])
		if len(add) == 0 && len(rem) == 0 {
			return nil, fmt.Errorf("github.assign: set add and/or remove")
		}
		var out struct {
			Assignees []struct {
				Login string `json:"login"`
			} `json:"assignees"`
		}
		u := fmt.Sprintf("%s/repos/%s/issues/%d/assignees", base, repo, number)
		if len(add) > 0 {
			if err := c.send(ctx, http.MethodPost, tok, u, map[string]any{"assignees": add}, &out); err != nil {
				return nil, remapTargetGone(err)
			}
		}
		if len(rem) > 0 {
			if err := c.send(ctx, http.MethodDelete, tok, u, map[string]any{"assignees": rem}, &out); err != nil {
				return nil, remapTargetGone(err)
			}
		}
		logins := make([]string, 0, len(out.Assignees))
		for _, a := range out.Assignees {
			logins = append(logins, a.Login)
		}
		return map[string]any{"assignees": logins}, nil
	case "remove_label":
		if number == 0 {
			return nil, fmt.Errorf("github.remove_label: options.number is required")
		}
		label, _ := opts["label"].(string)
		if label == "" {
			return nil, fmt.Errorf("github.remove_label: options.label is required")
		}
		u := fmt.Sprintf("%s/repos/%s/issues/%d/labels/%s", base, repo, number, url.PathEscape(label))
		if err := c.del(ctx, tok, u, nil); err != nil {
			// "Label does not exist" means the issue is still there, just
			// without that label — not the issue/PR itself being gone.
			return nil, remapGoneIfMissing(err, "Label does not exist")
		}
		return map[string]any{"ok": true}, nil
	case "get_issue":
		if number == 0 {
			return nil, fmt.Errorf("github.get_issue: options.number is required")
		}
		var iss struct {
			Title   string `json:"title"`
			Body    string `json:"body"`
			State   string `json:"state"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
			Assignees []struct {
				Login string `json:"login"`
			} `json:"assignees"`
		}
		if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/issues/%d", base, repo, number), &iss); err != nil {
			return nil, remapTargetGone(err)
		}
		labels := make([]string, 0, len(iss.Labels))
		for _, l := range iss.Labels {
			labels = append(labels, l.Name)
		}
		assignees := make([]string, 0, len(iss.Assignees))
		for _, a := range iss.Assignees {
			assignees = append(assignees, a.Login)
		}
		return map[string]any{
			"title": iss.Title, "body": iss.Body, "state": iss.State,
			"labels": labels, "assignees": assignees, "author": iss.User.Login, "url": iss.HTMLURL,
		}, nil
	case "put_file":
		path, _ := opts["path"].(string)
		content, _ := opts["content"].(string)
		message, _ := opts["message"].(string)
		if path == "" || message == "" {
			return nil, fmt.Errorf("github.put_file: path and message are required")
		}
		reqBody := map[string]any{"message": message, "content": base64.StdEncoding.EncodeToString([]byte(content))}
		if b, _ := opts["branch"].(string); b != "" {
			reqBody["branch"] = b
		}
		if s, _ := opts["sha"].(string); s != "" {
			reqBody["sha"] = s
		}
		var out struct {
			Content struct {
				SHA string `json:"sha"`
			} `json:"content"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := c.put(ctx, tok, fmt.Sprintf("%s/repos/%s/contents/%s", base, repo, escapePath(path)), reqBody, &out); err != nil {
			return nil, err
		}
		return map[string]any{"commit": out.Commit.SHA, "sha": out.Content.SHA}, nil
	case "delete_file":
		path, _ := opts["path"].(string)
		message, _ := opts["message"].(string)
		sha, _ := opts["sha"].(string)
		if path == "" || message == "" || sha == "" {
			return nil, fmt.Errorf("github.delete_file: path, message and sha are required")
		}
		reqBody := map[string]any{"message": message, "sha": sha}
		if b, _ := opts["branch"].(string); b != "" {
			reqBody["branch"] = b
		}
		var out struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := c.send(ctx, http.MethodDelete, tok, fmt.Sprintf("%s/repos/%s/contents/%s", base, repo, escapePath(path)), reqBody, &out); err != nil {
			return nil, err
		}
		return map[string]any{"commit": out.Commit.SHA}, nil
	case "get_ref":
		ref, _ := opts["ref"].(string)
		if ref == "" {
			return nil, fmt.Errorf("github.get_ref: options.ref is required")
		}
		var out struct {
			SHA string `json:"sha"`
		}
		if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/commits/%s", base, repo, url.PathEscape(ref)), &out); err != nil {
			return nil, err
		}
		return map[string]any{"sha": out.SHA}, nil
	case "create_branch":
		newBranch, _ := opts["branch"].(string)
		if newBranch == "" {
			return nil, fmt.Errorf("github.create_branch: options.branch is required")
		}
		from, _ := opts["from"].(string)
		if from == "" {
			from = "HEAD"
		}
		var src struct {
			SHA string `json:"sha"`
		}
		if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/commits/%s", base, repo, url.PathEscape(from)), &src); err != nil {
			return nil, err
		}
		if src.SHA == "" {
			return nil, fmt.Errorf("github.create_branch: could not resolve %q", from)
		}
		var out struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/git/refs", base, repo),
			map[string]any{"ref": "refs/heads/" + newBranch, "sha": src.SHA}, &out); err != nil {
			return nil, err
		}
		return map[string]any{"sha": out.Object.SHA}, nil
	case "dispatch_workflow":
		wf, _ := opts["workflow"].(string)
		ref, _ := opts["ref"].(string)
		if wf == "" || ref == "" {
			return nil, fmt.Errorf("github.dispatch_workflow: workflow and ref are required")
		}
		reqBody := map[string]any{"ref": ref}
		if in, ok := opts["inputs"].(map[string]any); ok && len(in) > 0 {
			reqBody["inputs"] = in
		}
		if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/actions/workflows/%s/dispatches", base, repo, url.PathEscape(wf)), reqBody, nil); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	case "rerun_run":
		runID := toInt(opts["run_id"])
		if runID == 0 {
			return nil, fmt.Errorf("github.rerun_run: options.run_id is required")
		}
		endpoint := "rerun"
		if f, _ := opts["failed_only"].(bool); f {
			endpoint = "rerun-failed-jobs"
		}
		if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/actions/runs/%d/%s", base, repo, runID, endpoint), nil, nil); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	case "cancel_run":
		runID := toInt(opts["run_id"])
		if runID == 0 {
			return nil, fmt.Errorf("github.cancel_run: options.run_id is required")
		}
		if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/actions/runs/%d/cancel", base, repo, runID), nil, nil); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	case "list_runs":
		all, _ := opts["all"].(bool)
		perPage := toInt(opts["per_page"])
		if perPage <= 0 {
			perPage = 20
		}
		if all {
			perPage = 100
		}
		runs := []any{}
		err := c.listAll(ctx, tok, all, perPage, func(page int) string {
			q := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
			if b, _ := opts["branch"].(string); b != "" {
				q.Set("branch", b)
			}
			if s, _ := opts["status"].(string); s != "" {
				q.Set("status", s)
			}
			return fmt.Sprintf("%s/repos/%s/actions/runs?%s", base, repo, q.Encode())
		}, func(b []byte) (int, error) {
			var out struct {
				Runs []struct {
					ID         int64  `json:"id"`
					Name       string `json:"name"`
					Status     string `json:"status"`
					Conclusion string `json:"conclusion"`
					HeadBranch string `json:"head_branch"`
					HeadSHA    string `json:"head_sha"`
					HTMLURL    string `json:"html_url"`
				} `json:"workflow_runs"`
			}
			if err := json.Unmarshal(b, &out); err != nil {
				return 0, err
			}
			for _, r := range out.Runs {
				runs = append(runs, map[string]any{
					"id": r.ID, "name": r.Name, "status": r.Status, "conclusion": r.Conclusion,
					"head_branch": r.HeadBranch, "head_sha": r.HeadSHA, "url": r.HTMLURL,
				})
			}
			return len(out.Runs), nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"runs": runs}, nil
	case "get_run":
		runID := toInt(opts["run_id"])
		if runID == 0 {
			return nil, fmt.Errorf("github.get_run: options.run_id is required")
		}
		var run struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HeadBranch string `json:"head_branch"`
			HeadSHA    string `json:"head_sha"`
			HTMLURL    string `json:"html_url"`
		}
		if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/actions/runs/%d", base, repo, runID), &run); err != nil {
			return nil, err
		}
		return map[string]any{
			"run_id": run.ID, "name": run.Name, "status": run.Status, "conclusion": run.Conclusion,
			"head_branch": run.HeadBranch, "head_sha": run.HeadSHA, "url": run.HTMLURL,
		}, nil
	case "create_release":
		tag, _ := opts["tag"].(string)
		if tag == "" {
			return nil, fmt.Errorf("github.create_release: options.tag is required")
		}
		reqBody := map[string]any{"tag_name": tag}
		if s, _ := opts["target"].(string); s != "" {
			reqBody["target_commitish"] = s
		}
		if s, _ := opts["name"].(string); s != "" {
			reqBody["name"] = s
		}
		if s, _ := opts["body"].(string); s != "" {
			reqBody["body"] = s
		}
		if b, _ := opts["draft"].(bool); b {
			reqBody["draft"] = true
		}
		if b, _ := opts["prerelease"].(bool); b {
			reqBody["prerelease"] = true
		}
		var out struct {
			ID        int64  `json:"id"`
			HTMLURL   string `json:"html_url"`
			UploadURL string `json:"upload_url"`
		}
		if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/releases", base, repo), reqBody, &out); err != nil {
			return nil, err
		}
		return map[string]any{"id": out.ID, "url": out.HTMLURL, "upload_url": out.UploadURL}, nil
	case "upload_asset":
		relID := toInt(opts["release_id"])
		name, _ := opts["name"].(string)
		if relID == 0 || name == "" {
			return nil, fmt.Errorf("github.upload_asset: release_id and name are required")
		}
		var data []byte
		if cnt, ok := opts["content"].(string); ok && cnt != "" {
			data = []byte(cnt)
		} else if p, _ := opts["path"].(string); p != "" {
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, fmt.Errorf("github.upload_asset: read %s: %w", p, err)
			}
			data = b
		} else {
			return nil, fmt.Errorf("github.upload_asset: set content or path")
		}
		var rel struct {
			UploadURL string `json:"upload_url"`
		}
		if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/releases/%d", base, repo, relID), &rel); err != nil {
			return nil, err
		}
		up := rel.UploadURL
		if i := strings.IndexByte(up, '{'); i >= 0 { // strip the {?name,label} template
			up = up[:i]
		}
		if up == "" {
			return nil, fmt.Errorf("github.upload_asset: release %d has no upload URL", relID)
		}
		up += "?name=" + url.QueryEscape(name)
		ct, _ := opts["content_type"].(string)
		if ct == "" {
			ct = "application/octet-stream"
		}
		var out struct {
			ID                 int64  `json:"id"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}
		if err := c.postRaw(ctx, tok, up, ct, data, &out); err != nil {
			return nil, err
		}
		return map[string]any{"id": out.ID, "url": out.BrowserDownloadURL}, nil
	case "list_issues":
		all, _ := opts["all"].(bool)
		pp := toInt(opts["per_page"])
		if pp <= 0 {
			pp = 30
		}
		if all {
			pp = 100
		}
		issues := []any{}
		err := c.listAll(ctx, tok, all, pp, func(page int) string {
			q := url.Values{"per_page": {strconv.Itoa(pp)}, "page": {strconv.Itoa(page)}}
			if s, _ := opts["state"].(string); s != "" {
				q.Set("state", s)
			}
			if l := toStrings(opts["labels"]); len(l) > 0 {
				q.Set("labels", strings.Join(l, ","))
			}
			if s, _ := opts["assignee"].(string); s != "" {
				q.Set("assignee", s)
			}
			return fmt.Sprintf("%s/repos/%s/issues?%s", base, repo, q.Encode())
		}, func(b []byte) (int, error) {
			var raw []struct {
				Number  int64  `json:"number"`
				Title   string `json:"title"`
				State   string `json:"state"`
				HTMLURL string `json:"html_url"`
				User    struct {
					Login string `json:"login"`
				} `json:"user"`
				Labels []struct {
					Name string `json:"name"`
				} `json:"labels"`
				PullRequest *struct{} `json:"pull_request"`
			}
			if err := json.Unmarshal(b, &raw); err != nil {
				return 0, err
			}
			for _, i := range raw {
				if i.PullRequest != nil { // the issues endpoint returns PRs too — drop them
					continue
				}
				labels := make([]string, 0, len(i.Labels))
				for _, l := range i.Labels {
					labels = append(labels, l.Name)
				}
				issues = append(issues, map[string]any{
					"number": i.Number, "title": i.Title, "state": i.State,
					"author": i.User.Login, "labels": labels, "url": i.HTMLURL,
				})
			}
			return len(raw), nil // count includes PRs, so pagination still advances correctly
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"issues": issues}, nil
	case "search_issues":
		query, _ := opts["q"].(string)
		if query == "" {
			return nil, fmt.Errorf("github.search_issues: options.q is required")
		}
		all, _ := opts["all"].(bool)
		pp := toInt(opts["per_page"])
		if pp <= 0 {
			pp = 30
		}
		if all {
			pp = 100
		}
		scoped := url.QueryEscape(query + " repo:" + repo)
		items := []any{}
		total := 0
		err := c.listAll(ctx, tok, all, pp, func(page int) string {
			return fmt.Sprintf("%s/search/issues?q=%s&per_page=%d&page=%d", base, scoped, pp, page)
		}, func(b []byte) (int, error) {
			var out struct {
				TotalCount int `json:"total_count"`
				Items      []struct {
					Number      int64     `json:"number"`
					Title       string    `json:"title"`
					State       string    `json:"state"`
					HTMLURL     string    `json:"html_url"`
					PullRequest *struct{} `json:"pull_request"`
				} `json:"items"`
			}
			if err := json.Unmarshal(b, &out); err != nil {
				return 0, err
			}
			total = out.TotalCount
			for _, it := range out.Items {
				items = append(items, map[string]any{
					"number": it.Number, "title": it.Title, "state": it.State,
					"is_pr": it.PullRequest != nil, "url": it.HTMLURL,
				})
			}
			return len(out.Items), nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"total": total, "items": items}, nil
	case "checks":
		ref, _ := opts["ref"].(string)
		if ref == "" {
			return nil, fmt.Errorf("github.checks: options.ref is required")
		}
		var out struct {
			CheckRuns []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				HTMLURL    string `json:"html_url"`
			} `json:"check_runs"`
		}
		if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/commits/%s/check-runs", base, repo, url.PathEscape(ref)), &out); err != nil {
			return nil, err
		}
		checks := make([]any, 0, len(out.CheckRuns))
		for _, cr := range out.CheckRuns {
			checks = append(checks, map[string]any{
				"name": cr.Name, "status": cr.Status, "conclusion": cr.Conclusion, "url": cr.HTMLURL,
			})
		}
		return map[string]any{"checks": checks}, nil
	case "ready_for_review", "convert_to_draft":
		if number == 0 {
			return nil, fmt.Errorf("github.%s: options.pr is required", verb)
		}
		var pr struct {
			NodeID string `json:"node_id"`
		}
		if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", base, repo, number), &pr); err != nil {
			return nil, remapTargetGone(err)
		}
		if pr.NodeID == "" {
			return nil, fmt.Errorf("github.%s: could not resolve the PR's node id", verb)
		}
		mutation := "mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){clientMutationId}}"
		if verb == "convert_to_draft" {
			mutation = "mutation($id:ID!){convertPullRequestToDraft(input:{pullRequestId:$id}){clientMutationId}}"
		}
		if err := c.graphql(ctx, tok, mutation, map[string]any{"id": pr.NodeID}, nil); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	case "create_gist":
		files := gistFiles(opts["files"])
		if len(files) == 0 {
			return nil, fmt.Errorf("github.create_gist: options.files is required ({filename: content})")
		}
		reqBody := map[string]any{"files": files}
		if d, _ := opts["description"].(string); d != "" {
			reqBody["description"] = d
		}
		if p, _ := opts["public"].(bool); p {
			reqBody["public"] = true
		}
		var out struct {
			ID      string `json:"id"`
			HTMLURL string `json:"html_url"`
		}
		if err := c.post(ctx, tok, base+"/gists", reqBody, &out); err != nil {
			return nil, err
		}
		return map[string]any{"id": out.ID, "url": out.HTMLURL}, nil
	case "get_gist":
		id, _ := opts["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("github.get_gist: options.id is required")
		}
		var out struct {
			HTMLURL     string `json:"html_url"`
			Description string `json:"description"`
			Public      bool   `json:"public"`
			Files       map[string]struct {
				Content string `json:"content"`
			} `json:"files"`
		}
		if err := c.get(ctx, tok, base+"/gists/"+url.PathEscape(id), &out); err != nil {
			return nil, err
		}
		files := make(map[string]any, len(out.Files))
		for name, f := range out.Files {
			files[name] = f.Content
		}
		return map[string]any{"files": files, "description": out.Description, "public": out.Public, "url": out.HTMLURL}, nil
	case "update_gist":
		id, _ := opts["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("github.update_gist: options.id is required")
		}
		reqBody := map[string]any{}
		if files := gistFiles(opts["files"]); len(files) > 0 {
			reqBody["files"] = files
		}
		if d, _ := opts["description"].(string); d != "" {
			reqBody["description"] = d
		}
		if len(reqBody) == 0 {
			return nil, fmt.Errorf("github.update_gist: set files and/or description")
		}
		var out struct {
			ID      string `json:"id"`
			HTMLURL string `json:"html_url"`
		}
		if err := c.patch(ctx, tok, base+"/gists/"+url.PathEscape(id), reqBody, &out); err != nil {
			return nil, err
		}
		return map[string]any{"id": out.ID, "url": out.HTMLURL}, nil
	case "delete_gist":
		id, _ := opts["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("github.delete_gist: options.id is required")
		}
		if err := c.del(ctx, tok, base+"/gists/"+url.PathEscape(id), nil); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	case "list_gists":
		all, _ := opts["all"].(bool)
		perPage := toInt(opts["per_page"])
		if perPage <= 0 {
			perPage = 30
		}
		if all {
			perPage = 100
		}
		path := "/gists"
		if user, _ := opts["user"].(string); user != "" {
			path = "/users/" + url.PathEscape(user) + "/gists"
		}
		gists := []any{}
		err := c.listAll(ctx, tok, all, perPage, func(page int) string {
			return fmt.Sprintf("%s%s?per_page=%d&page=%d", base, path, perPage, page)
		}, func(b []byte) (int, error) {
			var raw []struct {
				ID          string `json:"id"`
				HTMLURL     string `json:"html_url"`
				Description string `json:"description"`
				Public      bool   `json:"public"`
			}
			if err := json.Unmarshal(b, &raw); err != nil {
				return 0, err
			}
			for _, gg := range raw {
				gists = append(gists, map[string]any{"id": gg.ID, "description": gg.Description, "public": gg.Public, "url": gg.HTMLURL})
			}
			return len(raw), nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"gists": gists}, nil
	case "react":
		return c.react(ctx, tok, base, repo, number, opts)
	case "set_status":
		return c.setStatus(ctx, tok, base, repo, number, opts)
	case "add_labels":
		if number == 0 {
			return nil, fmt.Errorf("github.add_labels: options.number is required")
		}
		labels := toStrings(opts["labels"])
		if len(labels) == 0 {
			return nil, fmt.Errorf("github.add_labels: options.labels is required")
		}
		if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/issues/%d/labels", base, repo, number),
			map[string]any{"labels": labels}, nil); err != nil {
			return nil, remapTargetGone(err)
		}
		return map[string]any{"ok": true}, nil
	}
	return nil, fmt.Errorf("github: unknown verb %q", verb)
}

// listAll gathers items across pages of urlFor(page): each page's body goes
// to add (which decodes + appends and returns that page's item count). It
// stops on a short page or the page cap. all=false fetches just page 1.
func (c *Client) listAll(ctx context.Context, token string, all bool, perPage int, urlFor func(page int) string, add func(body []byte) (int, error)) error {
	maxPages := 1
	if all {
		maxPages = 50 // backstop: ~5000 items at perPage 100
	}
	for page := 1; page <= maxPages; page++ {
		b, err := c.cachedGet(ctx, token, urlFor(page), "application/vnd.github+json")
		if err != nil {
			return err
		}
		n, err := add(b)
		if err != nil {
			return err
		}
		if n < perPage {
			return nil
		}
	}
	return nil
}

// --- shared option coercion helpers ---

// toStrings coerces a YAML/JSON list (or single string) into []string.
// reviewerLogins is toStrings for reviewer lists, dropping entries that are
// not real logins: nil, blanks, and what an unresolved template renders to
// ("<nil>", "<no value>"). Otherwise a "{{.author}}" on an event with no
// author would be requested as a reviewer named "<nil>".
// dropPRAuthor removes the PR's author from a reviewer request: GitHub rejects
// the whole POST with a 422 ("Review cannot be requested from pull request
// author"), so a "{{.author}}" that resolved to the author would otherwise fail
// the flow. Best-effort — if the PR can't be fetched the list goes as-is.
func dropPRAuthor(ctx context.Context, c *Client, tok, base, repo string, number int, rs []string) []string {
	var pr struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", base, repo, number), &pr); err != nil || pr.User.Login == "" {
		return rs
	}
	kept := rs[:0:0]
	for _, r := range rs {
		if !strings.EqualFold(r, pr.User.Login) {
			kept = append(kept, r)
		}
	}
	return kept
}

// outstandingReviewers narrows a re-request to the reviewers still waiting on
// changes: each one's latest APPROVED/CHANGES_REQUESTED/DISMISSED review must be
// CHANGES_REQUESTED, submitted on an older commit than the current head (so there
// is something new to look at), and they must not already be a pending requested
// reviewer. Anyone who has since approved is dropped — re-requesting an approver
// puts the PR back in their queue as if the approval didn't count. A closed PR
// keeps nobody. State is read fresh (not from the GET cache): the approval this
// guards against can land seconds before the call. A failed read keeps nobody —
// the ping is a courtesy, never worth sending on a guess. The reason string says
// why the list came back empty.
func outstandingReviewers(ctx context.Context, c *Client, tok, base, repo string, number int, rs []string) ([]string, string) {
	var pr struct {
		State string `json:"state"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
		RequestedReviewers []struct {
			Login string `json:"login"`
		} `json:"requested_reviewers"`
	}
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", base, repo, number), &pr); err != nil {
		return nil, "couldn't read PR state: " + err.Error()
	}
	if pr.State != "open" {
		return nil, "PR is " + pr.State
	}
	var reviews []struct {
		State    string `json:"state"`
		CommitID string `json:"commit_id"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	// Reviews list oldest-first; the last page holds the latest. 100 per page
	// covers any PR a human reviews; beyond that the newest may be missed and
	// the reviewer is (safely) skipped.
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d/reviews?per_page=100", base, repo, number), &reviews); err != nil {
		return nil, "couldn't read reviews: " + err.Error()
	}
	type verdict struct{ state, commit string }
	latest := map[string]verdict{}
	for _, r := range reviews {
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			latest[strings.ToLower(r.User.Login)] = verdict{r.State, r.CommitID}
		}
	}
	pending := map[string]bool{}
	for _, u := range pr.RequestedReviewers {
		pending[strings.ToLower(u.Login)] = true
	}
	kept := rs[:0:0]
	var why []string
	for _, r := range rs {
		l := strings.ToLower(r)
		v := latest[l]
		switch {
		case pending[l]:
			why = append(why, r+" already requested")
		case v.state != "CHANGES_REQUESTED":
			st := strings.ToLower(v.state)
			if st == "" {
				st = "not requested changes"
			}
			why = append(why, r+" "+st)
		case v.commit != "" && v.commit == pr.Head.SHA:
			why = append(why, r+" has no new commits to review")
		default:
			kept = append(kept, r)
		}
	}
	return kept, strings.Join(why, "; ")
}

// dropBots removes bot accounts ("…[bot]" logins) from a reviewer list.
func dropBots(rs []string) []string {
	kept := rs[:0:0]
	for _, r := range rs {
		if !strings.HasSuffix(strings.ToLower(r), "[bot]") {
			kept = append(kept, r)
		}
	}
	return kept
}

// notCollaborator reports GitHub's 422 for a review request naming someone who
// can't review the repo ("Reviews may only be requested from collaborators").
func notCollaborator(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "HTTP 422") && strings.Contains(msg, "only be requested from collaborators")
}

// optBool reads a boolean option, accepting a rendered-template string
// ("true"/"false") as well as a bool; def when unset or unparseable.
func optBool(opts map[string]any, key string, def bool) bool {
	switch v := opts[key].(type) {
	case bool:
		return v
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func reviewerLogins(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, e := range xs {
			if e == nil {
				continue
			}
			out = append(out, toStrings([]any{e})...)
		}
	} else {
		out = toStrings(v)
	}
	kept := make([]string, 0, len(out))
	for _, s := range out {
		s = strings.TrimSpace(s)
		if s == "" || s == "<nil>" || s == "<no value>" {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			} else {
				out = append(out, fmt.Sprintf("%v", e))
			}
		}
		return out
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	}
	return nil
}

// toInt coerces YAML/JSON integer shapes.
func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case uint64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// ReviewComments coerces the submit_review `comments` option into GitHub
// review comment objects. Each needs a path + body; line/side/start_line/
// start_side are passed through when set (a line-based comment defaults to
// side RIGHT — the new version of the file). GitHub requires every commented
// line to fall within the PR's diff; a comment outside it makes the whole
// review 422, so callers should only comment on changed lines. nil/empty is
// fine — a review with no inline comments, just a summary + verdict.
func ReviewComments(v any) ([]map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("github.submit_review: comments must be a list of {path, line, body}, got %T", v)
	}
	out := make([]map[string]any, 0, len(raw))
	for i, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("github.submit_review: comments[%d] must be an object {path, line, body}, got %T", i, e)
		}
		path, _ := m["path"].(string)
		cbody, _ := m["body"].(string)
		if path == "" || cbody == "" {
			return nil, fmt.Errorf("github.submit_review: comments[%d] needs a non-empty path and body", i)
		}
		c := map[string]any{"path": path, "body": cbody}
		if n := toInt(m["line"]); n > 0 {
			c["line"] = n
		}
		if s, _ := m["side"].(string); s != "" {
			c["side"] = s
		}
		if n := toInt(m["start_line"]); n > 0 {
			c["start_line"] = n
		}
		if s, _ := m["start_side"].(string); s != "" {
			c["start_side"] = s
		}
		out = append(out, c)
	}
	return out, nil
}

// isGistVerb reports whether a verb operates on gists (user-scoped, no repo).
func isGistVerb(verb string) bool {
	switch verb {
	case "create_gist", "get_gist", "update_gist", "delete_gist", "list_gists":
		return true
	}
	return false
}

// gistFiles turns a {name: content} option map into the gist API's
// {name: {content}} shape. Returns nil for an empty/absent map.
func gistFiles(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for name, content := range m {
		s, _ := content.(string)
		out[name] = map[string]any{"content": s}
	}
	return out
}

// stringFields collects the named options that are present and non-empty
// into a request body — the shape of a partial PATCH (only send what's being
// changed).
func stringFields(opts map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if s, _ := opts[k].(string); s != "" {
			out[k] = s
		}
	}
	return out
}

// escapePath percent-escapes a content path SEGMENT BY SEGMENT, so the
// separators survive but nothing inside a segment can end the path or
// start a query.
//
// The neighbouring ref/label/id interpolations already escape; the three
// content-path ones did not, so a `path` containing `?`, `#`, or an
// encoded traversal reached the API as structure rather than as a name —
// a caller-supplied path could address a different endpoint or a
// different file than the one it named.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}
