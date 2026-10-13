package ghsource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// restClient is a small, rate-limit-aware GitHub REST client that authenticates
// with App installation tokens. It's used for the enrichment reads that
// webhook payloads don't carry (chiefly PR mergeable state).
type restClient struct {
	app   *appAuth
	httpc *http.Client
}

func newRESTClient(app *appAuth) *restClient {
	return &restClient{app: app, httpc: &http.Client{Timeout: 20 * time.Second}}
}

type pullInfo struct {
	State          string `json:"state"`           // open|closed
	MergeableState string `json:"mergeable_state"` // clean|dirty|behind|blocked|unstable|draft|unknown
	Mergeable      *bool  `json:"mergeable"`
	Draft          bool   `json:"draft"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"` // PR author
	Head struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	HTMLURL string `json:"html_url"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// pullHeadRefAndLabels fetches a PR's head branch name and current label names in a
// single GET (no mergeable-state wait). Used to enrich feedback triggers (comment /
// review events, whose payloads carry neither): head_ref lets dispatch adopt an open
// workspace, and labels let the engine honor control.pause_label on those triggers.
func (c *restClient) pullHeadRefAndLabels(ctx context.Context, instID int64, owner, repo string, num int) (headRef string, labels []string, err error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.app.apiBase, owner, repo, num)
	var pi pullInfo
	if err := c.get(ctx, instID, url, &pi); err != nil {
		return "", nil, err
	}
	for _, l := range pi.Labels {
		labels = append(labels, l.Name)
	}
	return pi.Head.Ref, labels, nil
}

// pull fetches a PR, retrying briefly while GitHub computes mergeable state.
func (c *restClient) pull(ctx context.Context, instID int64, owner, repo string, num int) (*pullInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.app.apiBase, owner, repo, num)
	for attempt := 0; attempt < 3; attempt++ {
		var pi pullInfo
		if err := c.get(ctx, instID, url, &pi); err != nil {
			return nil, err
		}
		if pi.MergeableState != "" && pi.MergeableState != "unknown" {
			return &pi, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 1500 * time.Millisecond):
		}
	}
	// Return whatever we have (state may be "unknown").
	var pi pullInfo
	if err := c.get(ctx, instID, url, &pi); err != nil {
		return nil, err
	}
	return &pi, nil
}

type prListItem struct {
	Number int  `json:"number"`
	Draft  bool `json:"draft"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	HTMLURL            string `json:"html_url"`
	Title              string `json:"title"`
	RequestedReviewers []struct {
		Login string `json:"login"`
	} `json:"requested_reviewers"`
	RequestedTeams []struct {
		Slug string `json:"slug"`
	} `json:"requested_teams"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// prCommitMessages returns the commit messages of a PR (first page — a
// revert PR is small; the trailer is on the revert commit itself).
func (c *restClient) prCommitMessages(ctx context.Context, instID int64, owner, repo string, num int) ([]string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/commits?per_page=100", c.app.apiBase, owner, repo, num)
	var items []struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := c.get(ctx, instID, url, &items); err != nil {
		return nil, err
	}
	msgs := make([]string, 0, len(items))
	for _, it := range items {
		msgs = append(msgs, it.Commit.Message)
	}
	return msgs, nil
}

// listOpenPRs lists open PRs for a repo (first page; sweep is a coarse catch-up).
func (c *restClient) listOpenPRs(ctx context.Context, instID int64, owner, repo string) ([]prListItem, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=100", c.app.apiBase, owner, repo)
	var out []prListItem
	if err := c.get(ctx, instID, url, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type repoRef struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// listInstallationRepos lists repositories the installation can access (first
// page; used to expand `owner/*` sweep globs).
func (c *restClient) listInstallationRepos(ctx context.Context, instID int64) ([]repoRef, error) {
	url := c.app.apiBase + "/installation/repositories?per_page=100"
	var data struct {
		Repositories []repoRef `json:"repositories"`
	}
	if err := c.get(ctx, instID, url, &data); err != nil {
		return nil, err
	}
	return data.Repositories, nil
}

// get performs a GET with the installation token, honoring rate-limit backoff.
func (c *restClient) get(ctx context.Context, instID int64, url string, out any) error {
	tok, err := c.app.installationToken(ctx, instID)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 4; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.httpc.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			wait := rateLimitWait(resp)
			resp.Body.Close()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return fmt.Errorf("GET %s: rate-limited after retries", url)
}

// graphql runs a GraphQL query with the installation token and decodes the
// `data` field into out. GitHub's GraphQL is the only source for review-thread
// resolution and Projects v2.
func (c *restClient) graphql(ctx context.Context, instID int64, query string, vars map[string]any, out any) error {
	tok, err := c.app.installationToken(ctx, instID)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	url := c.app.apiBase + "/graphql"
	for attempt := 0; attempt < 4; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.httpc.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			wait := rateLimitWait(resp)
			resp.Body.Close()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("graphql: HTTP %d", resp.StatusCode)
		}
		var env struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return err
		}
		if len(env.Errors) > 0 {
			return fmt.Errorf("graphql: %s", env.Errors[0].Message)
		}
		return json.Unmarshal(env.Data, out)
	}
	return fmt.Errorf("graphql: rate-limited after retries")
}

// mergeGate is the composite merge-readiness of a PR (all from one GraphQL query).
type mergeGate struct {
	HeadSHA          string
	MergeStateStatus string // CLEAN, BLOCKED, BEHIND, DIRTY, DRAFT, UNSTABLE, …
	ReviewDecision   string // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED
	IsDraft          bool
	Author           string
	ThreadsResolved  bool
	NonAuthorApprove bool
	Labels           []string
}

// unresolvedThread is one unresolved review thread as the sweep needs it: its
// node id (for the dedup signature) and who opened it (the reviewer to ping
// back once the feedback is addressed).
type unresolvedThread struct {
	ID          string
	Author      string
	AuthorIsBot bool
	// AuthorApproved: the opener's latest opinionated review (APPROVED or
	// CHANGES_REQUESTED) is APPROVED — they've signed off since leaving the
	// thread, so it's not outstanding change-requested feedback.
	AuthorApproved bool
	// The thread's opening comment, so a sweep-recovered changes_requested
	// run carries the feedback it is asked to address.
	CommentID int64
	Path      string
	Line      int
	Body      string
	URL       string
}

// unresolvedThreads returns the PR's unresolved review threads (on App creds),
// sorted by node id. Used by the sweep to reconcile outstanding review comments
// that no live webhook recovered.
func (c *restClient) unresolvedThreads(ctx context.Context, instID int64, owner, name string, number int) ([]unresolvedThread, error) {
	const q = `query($o:String!,$n:String!,$num:Int!){
	  repository(owner:$o,name:$n){ pullRequest(number:$num){
	    reviewThreads(first:100){nodes{id isResolved comments(first:1){nodes{databaseId author{login __typename} path line originalLine body url}}}}
	    latestOpinionatedReviews(first:50){nodes{state author{login}}}
	  }}}`
	var data struct {
		Repository struct {
			PullRequest struct {
				LatestOpinionatedReviews struct {
					Nodes []struct {
						State  string `json:"state"`
						Author *struct {
							Login string `json:"login"`
						} `json:"author"`
					} `json:"nodes"`
				} `json:"latestOpinionatedReviews"`
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						Comments   struct {
							Nodes []struct {
								Author *struct {
									Login    string `json:"login"`
									Typename string `json:"__typename"`
								} `json:"author"`
								DatabaseID   int64  `json:"databaseId"`
								Path         string `json:"path"`
								Line         *int   `json:"line"`
								OriginalLine *int   `json:"originalLine"`
								Body         string `json:"body"`
								URL          string `json:"url"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.graphql(ctx, instID, q, map[string]any{"o": owner, "n": name, "num": number}, &data); err != nil {
		return nil, err
	}
	approved := map[string]bool{}
	for _, r := range data.Repository.PullRequest.LatestOpinionatedReviews.Nodes {
		if r.Author != nil && r.State == "APPROVED" {
			approved[strings.ToLower(r.Author.Login)] = true
		}
	}
	var out []unresolvedThread
	for _, t := range data.Repository.PullRequest.ReviewThreads.Nodes {
		if t.IsResolved {
			continue
		}
		ut := unresolvedThread{ID: t.ID}
		if cs := t.Comments.Nodes; len(cs) > 0 {
			ut.CommentID, ut.Path, ut.Body, ut.URL = cs[0].DatabaseID, cs[0].Path, cs[0].Body, cs[0].URL
			ut.Line = lineOf(cs[0].Line, cs[0].OriginalLine)
		}
		if cs := t.Comments.Nodes; len(cs) > 0 && cs[0].Author != nil {
			ut.Author = cs[0].Author.Login
			ut.AuthorIsBot = isBotActor(cs[0].Author.Typename, cs[0].Author.Login)
			// GraphQL names a Bot actor by its bare slug ("cursor"); REST and
			// webhooks say "cursor[bot]". Use the latter so sweep facts match
			// the webhook's (filters, self, and the bare slug is a different —
			// human — account to the REST API).
			if ut.AuthorIsBot && !isBotLogin(ut.Author) {
				ut.Author += "[bot]"
			}
			ut.AuthorApproved = approved[strings.ToLower(cs[0].Author.Login)]
		}
		out = append(out, ut)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// prComment is a PR comment (issue-level or review-level) as the sweep needs it.
type prComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	// ReviewID is the submitted review an inline comment belongs to (review
	// comments only; 0 for conversation comments), with the comment's place
	// in the diff.
	ReviewID     int64  `json:"pull_request_review_id"`
	Path         string `json:"path"`
	Line         *int   `json:"line"`
	OriginalLine *int   `json:"original_line"`
	// Kind is CommentKindIssue or CommentKindReview — set by
	// recentComments from the endpoint the comment came from, not by GitHub.
	Kind string `json:"-"`
}

// recentComments returns a PR's most-recent comments — both conversation (issue)
// comments and inline review comments — merged, for the sweep's missed-comment
// recovery. Each endpoint returns its first page newest-first; the engine's
// per-kind comment high-water mark filters out ones already handled, so a small
// page is enough to recover a short offline gap without an unbounded walk.
func (c *restClient) recentComments(ctx context.Context, instID int64, owner, name string, number int) ([]prComment, error) {
	issueURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments?per_page=30&sort=created&direction=desc",
		c.app.apiBase, owner, name, number)
	reviewURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/comments?per_page=30&sort=created&direction=desc",
		c.app.apiBase, owner, name, number)
	var issue, review []prComment
	if err := c.get(ctx, instID, issueURL, &issue); err != nil {
		return nil, err
	}
	if err := c.get(ctx, instID, reviewURL, &review); err != nil {
		return nil, err
	}
	for i := range issue {
		issue[i].Kind = CommentKindIssue
	}
	for i := range review {
		review[i].Kind = CommentKindReview
	}
	return append(issue, review...), nil
}

// review returns a submitted review: its state (lowercased, as webhooks spell
// it — "changes_requested", "commented", "approved"), summary body, and author.
func (c *restClient) review(ctx context.Context, instID int64, owner, name string, number int, reviewID int64) (reviewInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews/%d", c.app.apiBase, owner, name, number, reviewID)
	var r struct {
		State string `json:"state"`
		Body  string `json:"body"`
		User  struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	}
	if err := c.get(ctx, instID, url, &r); err != nil {
		return reviewInfo{}, err
	}
	return reviewInfo{State: strings.ToLower(r.State), Body: r.Body,
		Author: r.User.Login, AuthorIsBot: isBotActor(r.User.Type, r.User.Login)}, nil
}

// reviewComment is one inline comment of a submitted review.
type reviewComment struct {
	ID     int64
	Author string
	Path   string
	Line   int
	Body   string
	URL    string
}

// reviewComments returns a submitted review's inline comments (first page of
// 100 — a review with more is capped by the caller anyway).
func (c *restClient) reviewComments(ctx context.Context, instID int64, owner, name string, number int, reviewID int64) ([]reviewComment, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews/%d/comments?per_page=100", c.app.apiBase, owner, name, number, reviewID)
	var items []struct {
		ID           int64  `json:"id"`
		Body         string `json:"body"`
		Path         string `json:"path"`
		Line         *int   `json:"line"`
		OriginalLine *int   `json:"original_line"`
		HTMLURL      string `json:"html_url"`
		User         struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := c.get(ctx, instID, url, &items); err != nil {
		return nil, err
	}
	out := make([]reviewComment, 0, len(items))
	for _, it := range items {
		rc := reviewComment{ID: it.ID, Author: it.User.Login, Path: it.Path, Body: it.Body,
			URL: it.HTMLURL, Line: lineOf(it.Line, it.OriginalLine)}
		out = append(out, rc)
	}
	return out, nil
}

// requestedReviewers returns the PR's currently-pending requested reviewer logins
// and team slugs (those who haven't reviewed yet). Used to make a ready_for_review
// transition reliable when the webhook payload omits them.
func (c *restClient) requestedReviewers(ctx context.Context, instID int64, owner, repo string, num int) (logins, slugs []string, err error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/requested_reviewers", c.app.apiBase, owner, repo, num)
	var d struct {
		Users []struct {
			Login string `json:"login"`
		} `json:"users"`
		Teams []struct {
			Slug string `json:"slug"`
		} `json:"teams"`
	}
	if err := c.get(ctx, instID, url, &d); err != nil {
		return nil, nil, err
	}
	for _, u := range d.Users {
		logins = append(logins, u.Login)
	}
	for _, t := range d.Teams {
		slugs = append(slugs, t.Slug)
	}
	return logins, slugs, nil
}

// stuckRun is a GitHub Actions workflow run that appears stuck.
type stuckRun struct {
	ID     int64
	Name   string
	Status string
}

// stuckRuns lists the PR head's workflow runs that are still in a running state
// (queued/in_progress) and older than olderThan — i.e. likely stuck (a dead runner
// leaves a run "in_progress" until GitHub's multi-hour timeout, so the check never
// completes and the PR stays blocked). now is injectable for tests.
func (c *restClient) stuckRuns(ctx context.Context, instID int64, owner, repo, headSHA string, olderThan time.Duration, now time.Time) ([]stuckRun, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs?head_sha=%s&per_page=50", c.app.apiBase, owner, repo, headSHA)
	var data struct {
		WorkflowRuns []struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
		} `json:"workflow_runs"`
	}
	if err := c.get(ctx, instID, url, &data); err != nil {
		return nil, err
	}
	var out []stuckRun
	for _, r := range data.WorkflowRuns {
		if r.Status != "queued" && r.Status != "in_progress" {
			continue // completed / waiting-on-a-human / etc. — not stuck
		}
		created, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil || now.Sub(created) < olderThan {
			continue // still within its normal running window
		}
		out = append(out, stuckRun{ID: r.ID, Name: r.Name, Status: r.Status})
	}
	return out, nil
}

// jobRunID resolves the workflow run an Actions job (a check_run id) belongs to.
// A check_run id is a *job* id, not a run id — `gh run rerun` needs the latter.
// Non-Actions checks (Bugbot, etc.) 404 here: there's nothing to rerun.
func (c *restClient) jobRunID(ctx context.Context, instID int64, owner, repo string, jobID int64) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d", c.app.apiBase, owner, repo, jobID)
	var data struct {
		RunID int64 `json:"run_id"`
	}
	if err := c.get(ctx, instID, url, &data); err != nil {
		return 0, err
	}
	return data.RunID, nil
}

// suiteRunID resolves the workflow run behind a check_suite id (each Actions run
// owns exactly one suite). Returns 0 when no run matches (a non-Actions suite).
func (c *restClient) suiteRunID(ctx context.Context, instID int64, owner, repo string, suiteID int64) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs?check_suite_id=%d&per_page=1", c.app.apiBase, owner, repo, suiteID)
	var data struct {
		WorkflowRuns []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	if err := c.get(ctx, instID, url, &data); err != nil {
		return 0, err
	}
	if len(data.WorkflowRuns) == 0 {
		return 0, nil
	}
	return data.WorkflowRuns[0].ID, nil
}

// prGate fetches the merge-readiness gate for a PR.
func (c *restClient) prGate(ctx context.Context, instID int64, owner, name string, number int) (*mergeGate, error) {
	const q = `query($o:String!,$n:String!,$num:Int!){
	  repository(owner:$o,name:$n){ pullRequest(number:$num){
	    headRefOid mergeStateStatus reviewDecision isDraft
	    author{login}
	    labels(first:50){nodes{name}}
	    reviewThreads(first:100){nodes{isResolved}}
	    approvals:reviews(first:50,states:[APPROVED]){nodes{author{login}}}
	  }}}`
	var data struct {
		Repository struct {
			PullRequest struct {
				HeadRefOid       string                 `json:"headRefOid"`
				MergeStateStatus string                 `json:"mergeStateStatus"`
				ReviewDecision   string                 `json:"reviewDecision"`
				IsDraft          bool                   `json:"isDraft"`
				Author           struct{ Login string } `json:"author"`
				Labels           struct {
					Nodes []struct{ Name string } `json:"nodes"`
				} `json:"labels"`
				ReviewThreads struct {
					Nodes []struct{ IsResolved bool } `json:"nodes"`
				} `json:"reviewThreads"`
				Approvals struct {
					Nodes []struct {
						Author struct{ Login string } `json:"author"`
					} `json:"nodes"`
				} `json:"approvals"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.graphql(ctx, instID, q, map[string]any{"o": owner, "n": name, "num": number}, &data); err != nil {
		return nil, err
	}
	pr := data.Repository.PullRequest
	g := &mergeGate{
		HeadSHA: pr.HeadRefOid, MergeStateStatus: pr.MergeStateStatus,
		ReviewDecision: pr.ReviewDecision, IsDraft: pr.IsDraft, Author: pr.Author.Login,
		ThreadsResolved: true,
	}
	for _, t := range pr.ReviewThreads.Nodes {
		if !t.IsResolved {
			g.ThreadsResolved = false
		}
	}
	for _, a := range pr.Approvals.Nodes {
		if a.Author.Login != "" && a.Author.Login != pr.Author.Login {
			g.NonAuthorApprove = true
		}
	}
	for _, l := range pr.Labels.Nodes {
		g.Labels = append(g.Labels, l.Name)
	}
	return g, nil
}

// projectItem is a Projects v2 item resolved to its issue + a field value.
type projectItem struct {
	Repo      string // owner/name
	Number    int
	Title     string
	Value     string   // the named single-select field's current option name
	Assignees []string // issue assignee logins (for the assignee gate)
}

// projectItem resolves a projects_v2_item node to its issue content and the
// current value of a single-select field (e.g. "Status").
func (c *restClient) projectItem(ctx context.Context, instID int64, itemNodeID, field string) (*projectItem, error) {
	const q = `query($id:ID!,$field:String!){ node(id:$id){ ... on ProjectV2Item {
	  content{ ... on Issue { number title repository{nameWithOwner} assignees(first:20){nodes{login}} } }
	  fieldValueByName(name:$field){ ... on ProjectV2ItemFieldSingleSelectValue { name } }
	}}}`
	var data struct {
		Node struct {
			Content struct {
				Number     int    `json:"number"`
				Title      string `json:"title"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
				Assignees struct {
					Nodes []struct {
						Login string `json:"login"`
					} `json:"nodes"`
				} `json:"assignees"`
			} `json:"content"`
			FieldValueByName struct {
				Name string `json:"name"`
			} `json:"fieldValueByName"`
		} `json:"node"`
	}
	if err := c.graphql(ctx, instID, q, map[string]any{"id": itemNodeID, "field": field}, &data); err != nil {
		return nil, err
	}
	n := data.Node
	if n.Content.Repository.NameWithOwner == "" || n.Content.Number == 0 {
		return nil, fmt.Errorf("project item has no issue content")
	}
	assignees := make([]string, 0, len(n.Content.Assignees.Nodes))
	for _, a := range n.Content.Assignees.Nodes {
		assignees = append(assignees, a.Login)
	}
	return &projectItem{Repo: n.Content.Repository.NameWithOwner, Number: n.Content.Number,
		Title: n.Content.Title, Value: n.FieldValueByName.Name, Assignees: assignees}, nil
}

// issueFacts is the full state of an issue for matching: the payload fields (so a
// projects_v2_item event, which carries no issue body, can still be matched) plus
// the gate enrichment — linked dev branch / closing PR presence and Projects v2
// field values (field name → value, lower-cased for case-insensitive comparison).
type issueFacts struct {
	Title     string
	Author    string
	Labels    []string
	Assignees []string
	HasBranch bool
	HasPR     bool
	Fields    map[string]string
}

// issueEnrich fetches issueFacts in one GraphQL call (App creds).
func (c *restClient) issueEnrich(ctx context.Context, instID int64, owner, name string, number int) (*issueFacts, error) {
	const q = `query($owner:String!,$name:String!,$num:Int!){
	  repository(owner:$owner,name:$name){ issue(number:$num){
	    title
	    author{ login }
	    labels(first:50){ nodes{ name } }
	    assignees(first:20){ nodes{ login } }
	    linkedBranches(first:1){ totalCount }
	    closedByPullRequestsReferences(first:1){ totalCount }
	    projectItems(first:10){ nodes{ fieldValues(first:20){ nodes{
	      ... on ProjectV2ItemFieldSingleSelectValue{ name field{ ... on ProjectV2FieldCommon{ name } } }
	      ... on ProjectV2ItemFieldTextValue{ text field{ ... on ProjectV2FieldCommon{ name } } }
	    }}}}
	  }}}`
	var data struct {
		Repository struct {
			Issue struct {
				Title  string `json:"title"`
				Author struct {
					Login string `json:"login"`
				} `json:"author"`
				Labels struct {
					Nodes []struct {
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
				Assignees struct {
					Nodes []struct {
						Login string `json:"login"`
					} `json:"nodes"`
				} `json:"assignees"`
				LinkedBranches struct {
					TotalCount int `json:"totalCount"`
				} `json:"linkedBranches"`
				ClosedByPRs struct {
					TotalCount int `json:"totalCount"`
				} `json:"closedByPullRequestsReferences"`
				ProjectItems struct {
					Nodes []struct {
						FieldValues struct {
							Nodes []struct {
								Name  string `json:"name"`
								Text  string `json:"text"`
								Field struct {
									Name string `json:"name"`
								} `json:"field"`
							} `json:"nodes"`
						} `json:"fieldValues"`
					} `json:"nodes"`
				} `json:"projectItems"`
			} `json:"issue"`
		} `json:"repository"`
	}
	if err := c.graphql(ctx, instID, q, map[string]any{"owner": owner, "name": name, "num": number}, &data); err != nil {
		return nil, err
	}
	iss := data.Repository.Issue
	f := &issueFacts{
		Title:     iss.Title,
		Author:    iss.Author.Login,
		HasBranch: iss.LinkedBranches.TotalCount > 0,
		HasPR:     iss.ClosedByPRs.TotalCount > 0,
		Fields:    map[string]string{},
	}
	for _, l := range iss.Labels.Nodes {
		f.Labels = append(f.Labels, l.Name)
	}
	for _, a := range iss.Assignees.Nodes {
		f.Assignees = append(f.Assignees, a.Login)
	}
	for _, item := range iss.ProjectItems.Nodes {
		for _, fv := range item.FieldValues.Nodes {
			if fv.Field.Name == "" {
				continue
			}
			val := fv.Name
			if val == "" {
				val = fv.Text
			}
			if val != "" {
				f.Fields[strings.ToLower(fv.Field.Name)] = strings.ToLower(val)
			}
		}
	}
	return f, nil
}

// issueGatePasses evaluates issue_matched gates against enrichment facts:
//   - no_branch: true  → skip if the issue has a linked dev branch OR closing PR
//   - project: {Field: value|[values]} → each field must equal / be in the set
func issueGatePasses(f *issueFacts, gates map[string]any) bool {
	for k, v := range gates {
		switch strings.ToLower(k) {
		case "no_branch":
			if b, _ := v.(bool); b && (f.HasBranch || f.HasPR) {
				return false
			}
		case "project":
			m, ok := v.(map[string]any)
			if !ok {
				return false
			}
			for field, want := range m {
				if !fieldValueMatches(f.Fields[strings.ToLower(field)], want) {
					return false
				}
			}
		}
	}
	return true
}

// fieldValueMatches reports whether a project field value equals want (string) or
// is a member of want ([]any of strings), case-insensitively.
func fieldValueMatches(got string, want any) bool {
	got = strings.ToLower(strings.TrimSpace(got))
	switch w := want.(type) {
	case string:
		return got == strings.ToLower(strings.TrimSpace(w))
	case []any:
		for _, e := range w {
			if s, ok := e.(string); ok && got == strings.ToLower(strings.TrimSpace(s)) {
				return true
			}
		}
	}
	return false
}

// rateLimitWait derives a backoff from Retry-After / X-RateLimit-Reset.
func rateLimitWait(resp *http.Response) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := time.ParseDuration(ra + "s"); err == nil {
			return secs
		}
	}
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		var epoch int64
		if _, err := fmt.Sscan(reset, &epoch); err == nil {
			d := time.Until(time.Unix(epoch, 0))
			if d > 0 && d < 5*time.Minute {
				return d
			}
		}
	}
	return 5 * time.Second
}

// lineOf is a review comment's line: where it sits now, else (outdated) the
// line it was left on. 0 when it has neither (a file-level comment).
func lineOf(line, original *int) int {
	switch {
	case line != nil:
		return *line
	case original != nil:
		return *original
	}
	return 0
}
