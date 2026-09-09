// Command conductor-github is the GitHub connector as a standalone external
// conductor plugin (#59). It implements every verb the bundled github
// connector implements (github.com/NodeSpy/conductor/internal/connector's
// githubDecl, minus the daemon-global `sweep` verb, which has no meaning
// outside the daemon process) by delegating to pkg/githubkit — the same
// tested HTTP client, GitHub App auth, and per-verb operations the bundled
// connector itself now wraps.
//
// As a source, it receives GitHub webhook deliveries (X-Hub-Signature-256
// HMAC), and streams a normalized event per delivery for the subset of events
// derivable from a single webhook payload with no further API calls or
// stateful history: new_comment, review_requested, changes_requested,
// release, deployment_status, dependabot_alert, secret_scanning_alert. The
// bundled connector's remaining events (merge_conflict, pr_behind,
// failing_checks, stuck_checks, merge_ready, self_review, issue_matched)
// require the daemon's own polling sweep and/or "me"-identity
// cross-referencing across multiple API calls, which a stateless plugin
// process does not have — those stay the bundled connector's job.
//
// Built ONLY against the public SDK (pkg/plugin), the connector-kit
// (pkg/sourcekit), and the public GitHub client (pkg/githubkit) — no other
// internal daemon package.
//
// Connection (used for both Invoke and StartSource):
//
//	token: "<PAT>"                      # fallback for `as: me`
//	api_base: "https://api.example.com" # override the GitHub API base (GHES, or tests)
//	identity:
//	  write_token: "<token>"            # literal, or "" / "gh_auth" to shell out to `gh auth token`
//	app:
//	  app_id: 12345
//	  private_key_path: "/path/to/app.pem"
//	  webhook_secret: "<secret>"         # HMAC secret for incoming webhooks
//	webhook:
//	  listen: ":9099"                    # HTTP listener address (StartSource only)
//	  path: "/github"                    # request path (default /github)
//	  secret: "<secret>"                 # webhook HMAC secret, if not under app.webhook_secret
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/NodeSpy/conductor/pkg/githubkit"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// baseGithubFilters/baseGithubContext mirror the bundled connector's shared
// event schema fragments (internal/connector/github.go) so operators see the
// same filter/context vocabulary regardless of which implementation runs.
func baseGithubFilters() plugin.Schema {
	return plugin.Schema{
		"repos":         {Type: "list", Desc: "repo globs this trigger fires for (default: the connector's repos:)"},
		"exclude_repos": {Type: "list", Desc: "repo globs this trigger never fires for"},
	}
}

func baseGithubContext() plugin.Schema {
	return plugin.Schema{
		"repo": {Type: "string"}, "owner": {Type: "string"}, "name": {Type: "string"},
		"pr": {Type: "integer"}, "issue": {Type: "integer"}, "number": {Type: "integer"},
		"head": {Type: "string"}, "base": {Type: "string"}, "url": {Type: "string"},
		"kind": {Type: "string"}, "title": {Type: "string"}, "labels": {Type: "list"},
	}
}

func githubEvent(name, desc string, filters, contextExtra plugin.Schema) plugin.Event {
	f := baseGithubFilters()
	for k, v := range filters {
		f[k] = v
	}
	c := baseGithubContext()
	for k, v := range contextExtra {
		c[k] = v
	}
	return plugin.Event{Name: name, Desc: desc, Filters: f, Context: c}
}

type githubPlugin struct {
	mu      sync.Mutex
	clients map[string]*githubkit.Client
}

func (g *githubPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Type: "github",
		Desc: "GitHub: PR/issue/release/deployment/alert events in (webhook-derived subset); comments, reviews, and review requests out. External-plugin counterpart of the bundled github connector (#59).",
		Connection: plugin.Schema{
			"app":      {Type: "map", Desc: "GitHub App credentials: app_id, private_key_path, webhook_secret"},
			"token":    {Type: "string", Desc: "PAT used when no App is configured (chain: app → token → gh auth token)"},
			"identity": {Type: "map", Desc: "credential policy: write_token"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path, secret"},
			"api_base": {Type: "string", Desc: "override the GitHub API base URL (GitHub Enterprise Server, or tests)"},
		},
		Events: []plugin.Event{
			githubEvent("review_requested", "your review was requested on a PR", nil, nil),
			githubEvent("changes_requested", "a review requested changes on your PR",
				nil,
				plugin.Schema{
					"head_ref": {Type: "string"},
					"author":   {Type: "string"}, "author_is_bot": {Type: "boolean", Desc: "the reviewer is an automated bot (account type Bot, or a [bot] login)"},
				}),
			githubEvent("new_comment", "a new comment on a PR",
				nil,
				plugin.Schema{
					"author": {Type: "string"}, "author_is_bot": {Type: "boolean", Desc: "the commenter is an automated bot (account type Bot, or a [bot] login)"},
					"comment_body": {Type: "string"}, "head_ref": {Type: "string"},
					"comment_id": {Type: "integer"}, "comment_kind": {Type: "string"},
				}),
			githubEvent("release", "a release was published",
				nil,
				plugin.Schema{"tag_name": {Type: "string"}, "prerelease": {Type: "boolean"}, "draft": {Type: "boolean"}}),
			githubEvent("deployment_status", "a deployment failed or errored",
				nil, plugin.Schema{"state": {Type: "string"}, "environment": {Type: "string"}, "description": {Type: "string"}}),
			githubEvent("dependabot_alert", "a new Dependabot alert",
				nil, plugin.Schema{"severity": {Type: "string"}, "package": {Type: "string"}, "summary": {Type: "string"}}),
			githubEvent("secret_scanning_alert", "a new secret-scanning alert",
				nil, plugin.Schema{"secret_type": {Type: "string"}}),
		},
		Verbs: []plugin.Verb{
			{
				Name: "comment", Desc: "post an issue/PR conversation comment",
				Options: plugin.Schema{
					"repo":   {Type: "string", Required: true},
					"number": {Type: "integer", Desc: "issue or PR number (alias: pr)"},
					"pr":     {Type: "integer"},
					"body":   {Type: "string", Required: true},
					"as":     {Type: "string", Enum: []string{"me", "bot"}, Desc: "identity (default me)"},
				},
				Outputs: plugin.Schema{"id": {Type: "integer"}, "url": {Type: "string"}},
			},
			{
				Name: "reply", Desc: "reply to a PR review comment thread",
				Options: plugin.Schema{
					"repo":        {Type: "string", Required: true},
					"pr":          {Type: "integer", Required: true},
					"in_reply_to": {Type: "integer", Required: true, Desc: "review comment id to reply to"},
					"body":        {Type: "string", Required: true},
					"as":          {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"id": {Type: "integer"}, "url": {Type: "string"}},
			},
			{
				Name: "request_review", Desc: "request review from users/teams on a PR (also re-requests one who already reviewed)",
				Options: plugin.Schema{
					"repo":           {Type: "string", Required: true},
					"pr":             {Type: "integer", Required: true},
					"reviewers":      {Type: "list", Desc: "user logins"},
					"team_reviewers": {Type: "list", Desc: "team slugs"},
					"as":             {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				// Back-compat alias of request_review: GitHub has one endpoint for
				// requesting reviewers, and re-requesting a prior reviewer is the
				// same call. Kept because live configs reference it for the
				// re-review-on-new-changes flow.
				Name: "rerequest_review", Desc: "re-request review (alias of request_review)",
				Options: plugin.Schema{
					"repo":           {Type: "string", Required: true},
					"pr":             {Type: "integer", Required: true},
					"reviewers":      {Type: "list", Desc: "logins"},
					"team_reviewers": {Type: "list", Desc: "team slugs"},
					"as":             {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "remove_reviewer", Desc: "cancel a pending review request (remove requested users/teams)",
				Options: plugin.Schema{
					"repo":           {Type: "string", Required: true},
					"pr":             {Type: "integer", Required: true},
					"reviewers":      {Type: "list", Desc: "user logins to un-request"},
					"team_reviewers": {Type: "list", Desc: "team slugs to un-request"},
					"as":             {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "submit_review", Desc: "submit a PR review: a summary + verdict, with optional inline file:line comments",
				Options: plugin.Schema{
					"repo":  {Type: "string", Required: true},
					"pr":    {Type: "integer", Required: true},
					"body":  {Type: "string", Desc: "the review summary (top-level comment)"},
					"event": {Type: "string", Enum: []string{"APPROVE", "REQUEST_CHANGES", "COMMENT"}, Required: true},
					"comments": {Type: "list", Desc: "inline comments posted with the review: a list of " +
						"{path, line, body, side?, start_line?, start_side?}. line is the file's line number; " +
						"side defaults to RIGHT (the new version). start_line/start_side make a multi-line range. " +
						"Every commented line MUST fall within the PR's diff, or GitHub rejects the whole review."},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"id": {Type: "integer"}, "comments": {Type: "integer", Desc: "inline comments posted"}},
			},
			{
				Name: "pr_diff", Desc: "the PR's unified diff (cached; GitHub caps the .diff media type around 300 files)",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "pr": {Type: "integer", Required: true},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"diff": {Type: "string"}},
			},
			{
				Name: "pr_get", Desc: "PR metadata: title, body, state, author, base/head, line counts, labels",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "pr": {Type: "integer", Required: true},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{
					"title": {Type: "string"}, "body": {Type: "string"}, "state": {Type: "string"},
					"draft": {Type: "boolean"}, "author": {Type: "string"}, "base": {Type: "string"},
					"head": {Type: "string"}, "head_sha": {Type: "string"}, "additions": {Type: "integer"},
					"deletions": {Type: "integer"}, "changed_files": {Type: "integer"}, "labels": {Type: "list"}, "url": {Type: "string"},
				},
			},
			{
				Name: "pr_files", Desc: "changed files: [{path, status, additions, deletions, changes}] (100/page; pass page for more)",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "pr": {Type: "integer", Required: true},
					"all": {Type: "boolean", Desc: "fetch every page (default: first 100)"},
					"as":  {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"files": {Type: "list"}},
			},
			{
				Name: "review_comments", Desc: "existing inline review comments on the PR: [{path, line, body, user, id}] (100/page)",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "pr": {Type: "integer", Required: true},
					"all": {Type: "boolean", Desc: "fetch every page (default: first 100)"},
					"as":  {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"comments": {Type: "list"}},
			},
			{
				Name: "file", Desc: "a repo file's raw contents at a ref (cached; GitHub's raw media type caps at ~1 MiB)",
				Options: plugin.Schema{
					"repo":     {Type: "string", Required: true},
					"path":     {Type: "string", Required: true, Desc: "repo-relative file path"},
					"ref":      {Type: "string", Desc: "branch / tag / sha (default: the repo's default branch)"},
					"as":       {Type: "string", Enum: []string{"me", "bot"}},
					"optional": {Type: "boolean", Desc: "return empty text instead of erroring when the file is missing (404) — for optional convention files"},
				},
				Outputs: plugin.Schema{"text": {Type: "string"}},
			},
			{
				Name: "create_pr", Desc: "open a pull request",
				Options: plugin.Schema{
					"repo":  {Type: "string", Required: true},
					"title": {Type: "string", Required: true},
					"head":  {Type: "string", Required: true, Desc: "the branch with your changes (owner:branch for a fork)"},
					"base":  {Type: "string", Required: true, Desc: "the branch to merge into"},
					"body":  {Type: "string"},
					"draft": {Type: "boolean"},
					"as":    {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"number": {Type: "integer"}, "url": {Type: "string"}},
			},
			{
				Name: "merge_pr", Desc: "merge a pull request",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "pr": {Type: "integer", Required: true},
					"method":         {Type: "string", Enum: []string{"merge", "squash", "rebase"}, Desc: "default merge"},
					"commit_title":   {Type: "string"},
					"commit_message": {Type: "string"},
					"sha":            {Type: "string", Desc: "require the PR head to match this sha (safety)"},
					"as":             {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"merged": {Type: "boolean"}, "sha": {Type: "string"}},
			},
			{
				Name: "update_pr", Desc: "edit a PR: state (open|closed → close/reopen), title, body, base",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "pr": {Type: "integer", Required: true},
					"state": {Type: "string", Enum: []string{"open", "closed"}},
					"title": {Type: "string"}, "body": {Type: "string"},
					"base": {Type: "string", Desc: "retarget the PR onto this branch"},
					"as":   {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"number": {Type: "integer"}, "state": {Type: "string"}},
			},
			{
				Name: "create_issue", Desc: "open an issue",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "title": {Type: "string", Required: true},
					"body":   {Type: "string"},
					"labels": {Type: "list"}, "assignees": {Type: "list", Desc: "logins to assign"},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"number": {Type: "integer"}, "url": {Type: "string"}},
			},
			{
				Name: "update_issue", Desc: "edit an issue: state (open|closed → close/reopen), state_reason, title, body",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "number": {Type: "integer", Required: true},
					"state":        {Type: "string", Enum: []string{"open", "closed"}},
					"state_reason": {Type: "string", Enum: []string{"completed", "not_planned", "reopened"}},
					"title":        {Type: "string"}, "body": {Type: "string"},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"number": {Type: "integer"}, "state": {Type: "string"}},
			},
			{
				Name: "assign", Desc: "add and/or remove issue/PR assignees",
				Options: plugin.Schema{
					"repo":   {Type: "string", Required: true},
					"number": {Type: "integer", Desc: "issue or PR number (alias: pr)"}, "pr": {Type: "integer"},
					"add": {Type: "list", Desc: "logins to assign"}, "remove": {Type: "list", Desc: "logins to unassign"},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"assignees": {Type: "list"}},
			},
			{
				Name: "remove_label", Desc: "remove one label from an issue or PR",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "number": {Type: "integer", Required: true},
					"label": {Type: "string", Required: true},
					"as":    {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "get_issue", Desc: "read an issue: title, body, state, labels, assignees, author, url",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "number": {Type: "integer", Required: true},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{
					"title": {Type: "string"}, "body": {Type: "string"}, "state": {Type: "string"},
					"labels": {Type: "list"}, "assignees": {Type: "list"}, "author": {Type: "string"}, "url": {Type: "string"},
				},
			},
			{
				Name: "put_file", Desc: "create or update a file in one commit",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "path": {Type: "string", Required: true},
					"content": {Type: "string", Required: true, Desc: "the new file content (UTF-8 text; base64-encoded for the API automatically)"},
					"message": {Type: "string", Required: true, Desc: "commit message"},
					"branch":  {Type: "string", Desc: "branch to commit on (default: the repo's default branch)"},
					"sha":     {Type: "string", Desc: "blob sha of the file being replaced (required to UPDATE an existing file; get it from `file`)"},
					"as":      {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"commit": {Type: "string"}, "sha": {Type: "string", Desc: "the new blob sha"}},
			},
			{
				Name: "delete_file", Desc: "delete a file in one commit",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "path": {Type: "string", Required: true},
					"message": {Type: "string", Required: true},
					"sha":     {Type: "string", Required: true, Desc: "blob sha of the file to delete"},
					"branch":  {Type: "string"},
					"as":      {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"commit": {Type: "string"}},
			},
			{
				Name: "get_ref", Desc: "the commit sha a branch/tag/ref points at",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true},
					"ref":  {Type: "string", Required: true, Desc: "branch, tag, or sha"},
					"as":   {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"sha": {Type: "string"}},
			},
			{
				Name: "create_branch", Desc: "create a branch from another ref",
				Options: plugin.Schema{
					"repo":   {Type: "string", Required: true},
					"branch": {Type: "string", Required: true, Desc: "new branch name"},
					"from":   {Type: "string", Desc: "source branch/tag/sha (default: the default branch's HEAD)"},
					"as":     {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"sha": {Type: "string"}},
			},
			{
				Name: "dispatch_workflow", Desc: "trigger a workflow_dispatch run",
				Options: plugin.Schema{
					"repo":     {Type: "string", Required: true},
					"workflow": {Type: "string", Required: true, Desc: "workflow file name (ci.yml) or numeric id"},
					"ref":      {Type: "string", Required: true, Desc: "branch or tag to run on"},
					"inputs":   {Type: "map", Desc: "workflow_dispatch inputs"},
					"as":       {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "rerun_run", Desc: "re-run a workflow run (optionally only its failed jobs)",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "run_id": {Type: "integer", Required: true},
					"failed_only": {Type: "boolean", Desc: "re-run only failed jobs"},
					"as":          {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "cancel_run", Desc: "cancel a workflow run",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "run_id": {Type: "integer", Required: true},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "list_runs", Desc: "recent workflow runs: [{id, name, status, conclusion, head_branch, head_sha, url}]",
				Options: plugin.Schema{
					"repo":     {Type: "string", Required: true},
					"branch":   {Type: "string", Desc: "filter to a branch"},
					"status":   {Type: "string", Desc: "queued|in_progress|completed|success|failure|…"},
					"per_page": {Type: "integer", Desc: "default 20, max 100"},
					"all":      {Type: "boolean", Desc: "fetch every page"},
					"as":       {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"runs": {Type: "list"}},
			},
			{
				Name: "create_release", Desc: "publish a release for a tag",
				Options: plugin.Schema{
					"repo":   {Type: "string", Required: true},
					"tag":    {Type: "string", Required: true, Desc: "the tag to release (created if it doesn't exist, on target)"},
					"target": {Type: "string", Desc: "commitish the tag points at when created (default: default branch)"},
					"name":   {Type: "string", Desc: "release title"}, "body": {Type: "string", Desc: "release notes"},
					"draft": {Type: "boolean"}, "prerelease": {Type: "boolean"},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"id": {Type: "integer"}, "url": {Type: "string"}, "upload_url": {Type: "string"}},
			},
			{
				Name: "upload_asset", Desc: "attach a file to a release",
				Options: plugin.Schema{
					"repo":         {Type: "string", Required: true},
					"release_id":   {Type: "integer", Required: true, Desc: "id from create_release"},
					"name":         {Type: "string", Required: true, Desc: "asset file name"},
					"content":      {Type: "string", Desc: "inline asset bytes (mutually exclusive with path)"},
					"path":         {Type: "string", Desc: "local file to upload"},
					"content_type": {Type: "string", Desc: "MIME type (default application/octet-stream)"},
					"as":           {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"id": {Type: "integer"}, "url": {Type: "string"}},
			},
			{
				Name: "list_issues", Desc: "list issues (PRs excluded): [{number, title, state, labels, author, url}]",
				Options: plugin.Schema{
					"repo":     {Type: "string", Required: true},
					"state":    {Type: "string", Desc: "open|closed|all (default open)"},
					"labels":   {Type: "list", Desc: "filter to issues with all these labels"},
					"assignee": {Type: "string", Desc: "filter to this assignee (or * / none)"},
					"per_page": {Type: "integer", Desc: "default 30, max 100"},
					"all":      {Type: "boolean", Desc: "fetch every page"},
					"as":       {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"issues": {Type: "list"}},
			},
			{
				Name: "search_issues", Desc: "search issues/PRs in this repo: [{number, title, state, is_pr, url}]",
				Options: plugin.Schema{
					"repo":     {Type: "string", Required: true},
					"q":        {Type: "string", Required: true, Desc: "GitHub search query (scoped to this repo automatically)"},
					"per_page": {Type: "integer", Desc: "default 30, max 100"},
					"all":      {Type: "boolean", Desc: "fetch every page"},
					"as":       {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"total": {Type: "integer"}, "items": {Type: "list"}},
			},
			{
				Name: "checks", Desc: "check-run status for a ref: [{name, status, conclusion, url}]",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true},
					"ref":  {Type: "string", Required: true, Desc: "branch, tag, or sha"},
					"as":   {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"checks": {Type: "list"}},
			},
			{
				Name: "ready_for_review", Desc: "mark a draft PR ready for review",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "pr": {Type: "integer", Required: true},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "convert_to_draft", Desc: "convert a PR back to a draft",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "pr": {Type: "integer", Required: true},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "create_gist", Desc: "create a gist (user-scoped, no repo)",
				Options: plugin.Schema{
					"files":       {Type: "map", Required: true, Desc: "{filename: content} — the gist's files"},
					"description": {Type: "string"},
					"public":      {Type: "boolean", Desc: "default false (secret gist)"},
				},
				Outputs: plugin.Schema{"id": {Type: "string"}, "url": {Type: "string"}},
			},
			{
				Name: "get_gist", Desc: "read a gist: its files, description, visibility",
				Options: plugin.Schema{
					"id": {Type: "string", Required: true},
				},
				Outputs: plugin.Schema{"files": {Type: "map", Desc: "{filename: content}"}, "description": {Type: "string"}, "public": {Type: "boolean"}, "url": {Type: "string"}},
			},
			{
				Name: "update_gist", Desc: "edit a gist's files and/or description",
				Options: plugin.Schema{
					"id":          {Type: "string", Required: true},
					"files":       {Type: "map", Desc: "{filename: content}; a null/empty content deletes that file"},
					"description": {Type: "string"},
				},
				Outputs: plugin.Schema{"id": {Type: "string"}, "url": {Type: "string"}},
			},
			{
				Name: "delete_gist", Desc: "delete a gist",
				Options: plugin.Schema{"id": {Type: "string", Required: true}},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "list_gists", Desc: "list gists: [{id, description, public, url}]",
				Options: plugin.Schema{
					"user":     {Type: "string", Desc: "whose public gists (default: your own, incl. secret)"},
					"per_page": {Type: "integer", Desc: "default 30, max 100"},
					"all":      {Type: "boolean", Desc: "fetch every page, not just the first"},
				},
				Outputs: plugin.Schema{"gists": {Type: "list"}},
			},
			{
				Name: "add_labels", Desc: "add labels to an issue or PR",
				Options: plugin.Schema{
					"repo":   {Type: "string", Required: true},
					"number": {Type: "integer", Required: true},
					"labels": {Type: "list", Required: true},
					"as":     {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
		},
		Capabilities: plugin.Capabilities{Egress: []string{"api.github.com", "*.ghe.com"}},
	}
}

// clientFor builds (or reuses) the githubkit.Client for one connector
// instance, so the GET cache and rate-limit state persist across Invoke
// calls exactly as the bundled connector's per-instance client does.
func (g *githubPlugin) clientFor(instance string, conn map[string]any) (*githubkit.Client, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.clients == nil {
		g.clients = map[string]*githubkit.Client{}
	}
	if c, ok := g.clients[instance]; ok {
		return c, nil
	}
	cfg := githubkit.Config{
		Token:   str(conn["token"]),
		APIBase: str(conn["api_base"]),
	}
	if identity, ok := conn["identity"].(map[string]any); ok {
		cfg.WriteToken = str(identity["write_token"])
	}
	if app, ok := conn["app"].(map[string]any); ok {
		appID := toInt64(app["app_id"])
		keyPath := str(app["private_key_path"])
		if appID > 0 && keyPath != "" {
			cfg.App = &githubkit.AppConfig{AppID: appID, PrivateKeyPath: keyPath}
		}
	}
	c, err := githubkit.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	g.clients[instance] = c
	return c, nil
}

func (g *githubPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	if req.Verb == "sweep" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "sweep is daemon-global; not available from an external plugin instance")
	}
	kit, err := g.clientFor(req.Instance, req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	out, err := kit.Invoke(context.Background(), req.Verb, req.Options)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (g *githubPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	app, _ := cfg["app"].(map[string]any)
	secret := ""
	if app != nil {
		secret = str(app["webhook_secret"])
	}
	if secret == "" && webhook != nil {
		secret = str(webhook["secret"])
	}
	addr, path := "", "/github"
	if webhook != nil {
		addr = str(webhook["listen"])
		if p := str(webhook["path"]); p != "" {
			path = p
		}
	}
	ln := sourcekit.Listener{Addr: addr, Path: path, Secret: secret, SigHeader: "X-Hub-Signature-256"}
	if ln.Addr == "" {
		return fmt.Errorf("github: no webhook.listen address configured")
	}
	dedup := sourcekit.NewDedup(4096)
	fmt.Fprintf(os.Stderr, "github[%s]: listening on %s%s\n", req.Instance, ln.Addr, ln.Path)
	return ln.Serve(ctx, func(h http.Header, body []byte) {
		kind := h.Get("X-GitHub-Event")
		if kind == "" {
			return
		}
		for _, ev := range parseWebhook(kind, body) {
			if ev.Dedup != "" && !dedup.Add(ev.Dedup) {
				continue
			}
			_ = emit(ev)
		}
	})
}

// wireEvent is the normalized event streamed to the daemon's plugin source
// adapter (internal/connector/pluginsource.go). Target's keys must match
// core.Target's exact (untagged, capitalized) Go field names, since the
// daemon decodes this JSON straight into that struct.
type wireEvent struct {
	Event   string         `json:"event"`
	Kind    string         `json:"kind,omitempty"`
	Title   string         `json:"title,omitempty"`
	Target  map[string]any `json:"target,omitempty"`
	Context map[string]any `json:"context,omitempty"`
	Dedup   string         `json:"dedup,omitempty"`
}

func target(repo, owner, name string, num int, head, base, url string) map[string]any {
	t := map[string]any{"Repo": repo, "Owner": owner, "Name": name, "HeadSHA": head, "BaseRef": base, "HTMLURL": url}
	if num != 0 {
		t["PR"] = num
		t["Number"] = num
	}
	return t
}

// ghPayload is the subset of GitHub's webhook payload shapes this plugin
// reads, mirroring internal/integrations/github/events.go's ghPayload (the
// daemon's own reference decoder for the same deliveries).
type ghPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	PullRequest *struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"pull_request"`
	Issue *struct {
		Number      int             `json:"number"`
		HTMLURL     string          `json:"html_url"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Comment *struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"comment"`
	Review *struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
		User  struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"review"`
	Release *struct {
		TagName         string `json:"tag_name"`
		HTMLURL         string `json:"html_url"`
		TargetCommitish string `json:"target_commitish"`
		Draft           bool   `json:"draft"`
		Prerelease      bool   `json:"prerelease"`
	} `json:"release"`
	Deployment *struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"deployment"`
	DeploymentStatus *struct {
		State       string `json:"state"`
		Environment string `json:"environment"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	} `json:"deployment_status"`
	Alert *struct {
		Number         int    `json:"number"`
		HTMLURL        string `json:"html_url"`
		SecretType     string `json:"secret_type"`
		SecretTypeName string `json:"secret_type_display_name"`
		Dependency     *struct {
			Package struct {
				Name string `json:"name"`
			} `json:"package"`
		} `json:"dependency"`
		SecurityAdvisory *struct {
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
		} `json:"security_advisory"`
	} `json:"alert"`
}

func parseWebhook(eventType string, body []byte) []wireEvent {
	var p ghPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	repo := p.Repository.FullName
	owner, name := splitRepo(repo)
	switch eventType {
	case "issue_comment", "pull_request_review_comment":
		return commentEvents(eventType, repo, owner, name, p)
	case "pull_request":
		return pullRequestEvents(repo, owner, name, p)
	case "pull_request_review":
		return reviewEvents(repo, owner, name, p)
	case "release":
		return releaseEvents(repo, owner, name, p)
	case "deployment_status":
		return deploymentStatusEvents(repo, owner, name, p)
	case "dependabot_alert":
		return dependabotAlertEvents(repo, owner, name, p)
	case "secret_scanning_alert":
		return secretScanningAlertEvents(repo, owner, name, p)
	}
	return nil
}

func commentEvents(eventType, repo, owner, name string, p ghPayload) []wireEvent {
	if p.Action != "created" || p.Comment == nil {
		return nil
	}
	var num int
	var head, base, url, headRef string
	switch {
	case p.PullRequest != nil:
		num = p.PullRequest.Number
		head, base, url = p.PullRequest.Head.SHA, p.PullRequest.Base.Ref, p.PullRequest.HTMLURL
		headRef = p.PullRequest.Head.Ref
	case eventType == "issue_comment" && p.Issue != nil && len(p.Issue.PullRequest) > 0:
		num = p.Issue.Number
		url = p.Issue.HTMLURL
	default:
		return nil // comment on a plain issue, not a PR — not this event
	}
	kind := "issue"
	if eventType == "pull_request_review_comment" {
		kind = "review"
	}
	authorIsBot := isBotActor(p.Comment.User.Type, p.Comment.User.Login)
	return []wireEvent{{
		Event: "new_comment", Kind: "new_comment",
		Title:  fmt.Sprintf("new comment by %s on %s#%d", p.Comment.User.Login, repo, num),
		Dedup:  fmt.Sprintf("comment:%d", p.Comment.ID),
		Target: target(repo, owner, name, num, head, base, url),
		Context: map[string]any{
			"repo": repo, "owner": owner, "name": name, "pr": num, "number": num,
			"head": head, "base": base, "url": url, "kind": kind,
			"author": p.Comment.User.Login, "author_is_bot": authorIsBot,
			"comment_body": p.Comment.Body, "head_ref": headRef,
			"comment_id": p.Comment.ID, "comment_kind": kind,
		},
	}}
}

func pullRequestEvents(repo, owner, name string, p ghPayload) []wireEvent {
	if p.PullRequest == nil || p.Action != "review_requested" {
		return nil
	}
	pr := p.PullRequest
	return []wireEvent{{
		Event: "review_requested", Kind: "review_requested",
		Title:  fmt.Sprintf("review requested on %s#%d", repo, pr.Number),
		Dedup:  "reviewreq@" + pr.Head.SHA,
		Target: target(repo, owner, name, pr.Number, pr.Head.SHA, pr.Base.Ref, pr.HTMLURL),
		Context: map[string]any{
			"repo": repo, "owner": owner, "name": name, "pr": pr.Number, "number": pr.Number,
			"head": pr.Head.SHA, "base": pr.Base.Ref, "url": pr.HTMLURL,
			"kind": "review_requested", "title": pr.Title, "labels": labelNames(pr.Labels),
		},
	}}
}

func reviewEvents(repo, owner, name string, p ghPayload) []wireEvent {
	if p.Action != "submitted" || p.Review == nil || p.PullRequest == nil || p.Review.State != "changes_requested" {
		return nil
	}
	pr := p.PullRequest
	reviewerIsBot := isBotActor(p.Review.User.Type, p.Review.User.Login)
	return []wireEvent{{
		Event: "changes_requested", Kind: "changes_requested",
		Title:  fmt.Sprintf("changes requested on %s#%d", repo, pr.Number),
		Dedup:  fmt.Sprintf("review:%d@%s", p.Review.ID, pr.Head.SHA),
		Target: target(repo, owner, name, pr.Number, pr.Head.SHA, pr.Base.Ref, pr.HTMLURL),
		Context: map[string]any{
			"repo": repo, "owner": owner, "name": name, "pr": pr.Number, "number": pr.Number,
			"head": pr.Head.SHA, "base": pr.Base.Ref, "url": pr.HTMLURL, "kind": "changes_requested",
			"head_ref": pr.Head.Ref, "author": p.Review.User.Login, "author_is_bot": reviewerIsBot,
		},
	}}
}

func releaseEvents(repo, owner, name string, p ghPayload) []wireEvent {
	if p.Release == nil || p.Action != "published" || p.Release.Draft {
		return nil
	}
	return []wireEvent{{
		Event: "release", Kind: "release",
		Title:  fmt.Sprintf("release %s published on %s", p.Release.TagName, repo),
		Dedup:  "release:" + repo + ":" + p.Release.TagName,
		Target: target(repo, owner, name, 0, "", p.Release.TargetCommitish, p.Release.HTMLURL),
		Context: map[string]any{
			"repo": repo, "owner": owner, "name": name, "url": p.Release.HTMLURL, "kind": "release",
			"tag_name": p.Release.TagName, "prerelease": p.Release.Prerelease, "draft": p.Release.Draft,
		},
	}}
}

func deploymentStatusEvents(repo, owner, name string, p ghPayload) []wireEvent {
	if p.DeploymentStatus == nil {
		return nil
	}
	st := strings.ToLower(p.DeploymentStatus.State)
	if st != "failure" && st != "error" {
		return nil
	}
	sha, ref := "", p.Repository.DefaultBranch
	if p.Deployment != nil {
		sha = p.Deployment.SHA
		if p.Deployment.Ref != "" {
			ref = p.Deployment.Ref
		}
	}
	return []wireEvent{{
		Event: "deployment_status", Kind: "deployment_status",
		Title:  fmt.Sprintf("deployment %s on %s (%s)", st, repo, p.DeploymentStatus.Environment),
		Dedup:  fmt.Sprintf("deploy:%s:%s:%s:%s", repo, p.DeploymentStatus.Environment, sha, st),
		Target: target(repo, owner, name, 0, sha, ref, p.DeploymentStatus.TargetURL),
		Context: map[string]any{
			"repo": repo, "owner": owner, "name": name, "url": p.DeploymentStatus.TargetURL, "kind": "deployment_status",
			"state": st, "environment": p.DeploymentStatus.Environment, "description": p.DeploymentStatus.Description,
		},
	}}
}

func dependabotAlertEvents(repo, owner, name string, p ghPayload) []wireEvent {
	if p.Alert == nil || p.Action != "created" {
		return nil
	}
	pkg, sev, summary := "", "", ""
	if p.Alert.Dependency != nil {
		pkg = p.Alert.Dependency.Package.Name
	}
	if p.Alert.SecurityAdvisory != nil {
		sev, summary = p.Alert.SecurityAdvisory.Severity, p.Alert.SecurityAdvisory.Summary
	}
	return []wireEvent{{
		Event: "dependabot_alert", Kind: "dependabot_alert",
		Title:  fmt.Sprintf("dependabot %s alert on %s: %s", sev, repo, pkg),
		Dedup:  fmt.Sprintf("dependabot:%s:%d", repo, p.Alert.Number),
		Target: target(repo, owner, name, 0, "", p.Repository.DefaultBranch, p.Alert.HTMLURL),
		Context: map[string]any{
			"repo": repo, "owner": owner, "name": name, "url": p.Alert.HTMLURL, "kind": "dependabot_alert",
			"severity": sev, "package": pkg, "summary": summary,
		},
	}}
}

func secretScanningAlertEvents(repo, owner, name string, p ghPayload) []wireEvent {
	if p.Alert == nil || p.Action != "created" {
		return nil
	}
	stype := p.Alert.SecretTypeName
	if stype == "" {
		stype = p.Alert.SecretType
	}
	return []wireEvent{{
		Event: "secret_scanning_alert", Kind: "secret_scanning_alert",
		Title:  fmt.Sprintf("secret scanning alert on %s: %s", repo, stype),
		Dedup:  fmt.Sprintf("secretscan:%s:%d", repo, p.Alert.Number),
		Target: target(repo, owner, name, 0, "", p.Repository.DefaultBranch, p.Alert.HTMLURL),
		Context: map[string]any{
			"repo": repo, "owner": owner, "name": name, "url": p.Alert.HTMLURL, "kind": "secret_scanning_alert",
			"secret_type": stype,
		},
	}}
}

func splitRepo(full string) (owner, name string) {
	owner, name, _ = strings.Cut(full, "/")
	return owner, name
}

func labelNames(labels []struct {
	Name string `json:"name"`
}) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}

// isBotActor mirrors the bundled connector's bot-actor detection: GitHub
// marks App/bot accounts with account type "Bot", and bot logins
// conventionally end in "[bot]" (e.g. dependabot[bot]).
func isBotActor(accountType, login string) bool {
	return strings.EqualFold(accountType, "Bot") || strings.HasSuffix(login, "[bot]")
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strOr(v any, def string) string {
	if s := str(v); s != "" {
		return s
	}
	return def
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

func main() {
	if err := plugin.Serve(&githubPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-github:", err)
		os.Exit(1)
	}
}
