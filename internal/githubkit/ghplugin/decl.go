package ghplugin

import (
	"github.com/NodeSpy/conductor-plugins/internal/githubkit"
	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghsource"
	"github.com/NodeSpy/conductor/pkg/plugin"
)

// Type is the connector type this declaration describes.
const Type = "github"

// Decl is THE github connector declaration: connection fields, events (with
// their unified-filter facts and match keys), and verbs. Conductor's bundled
// github connector builds its TypeDecl from it, and the conductor-github
// plugin returns it from Describe — so the two cannot drift: an operator's
// config means the same thing whichever implementation backs it.
//
// The sweep verb is the name github's poll semantic gives the engine's "poll
// this instance now" verb: the daemon answers it for either implementation.
func Decl() plugin.Decl { return decl() }

// filterSchema turns the source's filter surface (fact/match-key name → value
// kind, defined next to the code that computes and evaluates them) into a
// schema, so a fact cannot be declared here without being published there.
func filterSchema(kinds map[string]string) plugin.Schema {
	if len(kinds) == 0 {
		return nil
	}
	s := make(plugin.Schema, len(kinds))
	for name, kind := range kinds {
		switch kind {
		case ghsource.FilterBool:
			s[name] = plugin.Field{Type: "boolean"}
		case ghsource.FilterList:
			s[name] = plugin.Field{Type: "list"}
		default:
			s[name] = plugin.Field{Type: "string"}
		}
	}
	return s
}

// baseGithubContext are the context facts every github event publishes.
func baseGithubContext() plugin.Schema {
	return plugin.Schema{
		"repo": {Type: "string"}, "owner": {Type: "string"}, "name": {Type: "string"},
		"pr": {Type: "integer"}, "issue": {Type: "integer"}, "number": {Type: "integer"},
		"head": {Type: "string"}, "base": {Type: "string"}, "url": {Type: "string"},
		"kind": {Type: "string"}, "title": {Type: "string"}, "labels": {Type: "list"},
		"me": {Type: "map", Desc: "you, as your writes act: { login } — the login discovered from the write identity (else your first me: login); e.g. a set_status context \"{{.me.login}} / review\""},
	}
}

// reactionSubjectsDesc documents the comment/review events' reaction_subjects.
const reactionSubjectsDesc = "what a run handling this event reacts on, as [{kind, id}] for github.react: the review (kind review), the standalone comment (issue_comment | review_comment), or — sweep-recovered — each unresolved thread's opening comment (capped)"

// githubEvent builds one event declaration on the shared base.
//
// It declares no Filters schema: github's whole filter surface is the unified
// `filter:`, whose facts and match keys come from the integration that computes
// and evaluates them (gh.FilterFacts / gh.FilterMatchKeys). What used to sit in
// `filters:` and is NOT a predicate over the event — repo routing, the
// reviewer/assignee identity gates, per-check suppression, the release
// prerelease switch — is declared in Options instead, next to the other
// source-side knobs.
func githubEvent(name, desc string, contextExtra, options plugin.Schema) plugin.Event {
	c := baseGithubContext()
	for k, v := range contextExtra {
		c[k] = v
	}
	o := plugin.Schema{
		"max_attempts_per_head": {Type: "integer", Desc: "soft attempt threshold before backoff"},
	}
	for k, v := range options {
		o[k] = v
	}
	return plugin.Event{
		Name: name, Desc: desc, Context: c, Options: o,
		Facts:     filterSchema(ghsource.FilterFacts(name)),
		MatchKeys: filterSchema(ghsource.FilterMatchKeys(name)),
		Semantics: eventSemantics(name),
	}
}

func decl() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: Type,
		// The plugin's permission manifest (the bundled connector, in
		// process, has none to declare): the public API, GitHub Enterprise
		// Cloud with data residency, and the smee relay. A GHES host on its
		// own domain, or a test double, is outside it — an INSTALLED plugin
		// is confined to this list; a local development build is not.
		// gh: the write chain's `gh auth token` (Q8); its token variables, for a
		// box that authenticates gh through the environment rather than its
		// config file.
		Capabilities: plugin.Capabilities{Egress: []string{"api.github.com:443", "*.ghe.com:443", "smee.io:443"}, Commands: []string{"gh"},
			Env: []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST", "GH_CONFIG_DIR"}},
		Desc: "GitHub: PR/issue/check/release events in; comments, reviews, and review requests out.",
		Connection: plugin.Schema{
			"app":             {Type: "map", Desc: "GitHub App credentials: app_id, private_key_path"},
			"token":           {Type: "string", Secret: true, Desc: "PAT used when no App is configured (chain: app → token → gh auth token)"},
			"webhook":         {Type: "map", Desc: "event transport and delivery auth: smee_url and/or listen (+ path), secret, verify_signature"},
			"sweep":           {Type: "map", Desc: "catch-up sweep: enabled, interval, min_interval, repos"},
			"me":              {Type: "map", Desc: "your GitHub login(s): { logins: [...] } — defines \"you\""},
			"repos":           {Type: "list", Desc: "default repo globs for triggers whose filter names no repo"},
			"identity":        {Type: "map", Desc: "credential policy: read_token, write_token, commit_author"},
			"retry":           {Type: "map", Desc: "transient dispatch retry: max, backoff"},
			"project_map":     {Type: "map", Desc: "repo -> paseo project checkout remap"},
			"project_rewrite": {Type: "map", Desc: "blanket owner/org rewrite for checkouts"},
			"api_base":        {Type: "string", Desc: "GitHub API base URL (GitHub Enterprise Server); default https://api.github.com, or PC_GITHUB_API_BASE for the bundled connector"},
		},
		Semantics: connSemantics(),
		Events: []plugin.Event{
			githubEvent("review_requested", "your review was requested on a PR",
				nil,
				plugin.Schema{
					"reviewer": {Type: "map", Desc: "whose requested review triggers: { logins: [...], teams: [...] } (default: the connector's me:) — an identity gate, not a fact predicate"},
				}),
			githubEvent("changes_requested", "a review requested changes, or left inline comments without approving, on your PR (or threads went unresolved) — one event per review",
				plugin.Schema{
					"head_ref": {Type: "string"},
					"author":   {Type: "string"}, "author_is_bot": {Type: "boolean", Desc: "the reviewer is an automated bot (account type Bot, or a [bot] login)"},
					"review_id":               {Type: "integer", Desc: "the submitted review's id (absent for a sweep-recovered run over unresolved threads)"},
					"review_body":             {Type: "string", Desc: "the review's summary comment"},
					"review_comments":         {Type: "list", Desc: "the feedback to address: the review's inline comments (or, sweep-recovered, each unresolved thread's opening comment) as {author, path, line, body, url}. A review is ONE event — its inline comments never also fire new_comment"},
					"review_comments_omitted": {Type: "integer", Desc: "how many comments the size cap left out of review_comments (absent when none) — read them on the PR"},
					"review_state":            {Type: "string", Desc: "the review's state: changes_requested or commented (absent for a sweep-recovered run)"},
					"comment_id":              {Type: "integer", Desc: "the highest inline comment id the run covers; the engine dispatches a review once on it"},
					"comment_kind":            {Type: "string"},
					"reaction_subjects":       {Type: "list", Desc: reactionSubjectsDesc},
				}, nil),
			githubEvent("new_comment", "a new comment on your PR — a standalone comment, or ONE submitted review no changes_requested trigger takes (always so for an approval) with all its inline comments",
				plugin.Schema{
					"author": {Type: "string"}, "author_is_bot": {Type: "boolean", Desc: "the commenter (or reviewer) is an automated bot (account type Bot, or a [bot] login)"},
					"comment_body": {Type: "string", Desc: "the comment; for a review, its body then each inline comment as \"path:line: body\""},
					"head_ref":     {Type: "string"},
					"comment_id":   {Type: "integer", Desc: "the comment's id; for a review, its highest inline comment's"}, "comment_kind": {Type: "string"},
					"review_id":               {Type: "integer", Desc: "set when the event is a submitted review"},
					"review_body":             {Type: "string", Desc: "the review's summary comment (review events)"},
					"review_state":            {Type: "string", Desc: "the review's state: commented, approved, changes_requested (review events)"},
					"review_comments":         {Type: "list", Desc: "the review's inline comments as {author, path, line, body, url} (review events)"},
					"review_comments_omitted": {Type: "integer", Desc: "how many comments the size cap left out of review_comments (absent when none)"},
					"reaction_subjects":       {Type: "list", Desc: reactionSubjectsDesc},
				}, nil),
			githubEvent("merge_conflict", "your PR became unmergeable", nil, nil),
			githubEvent("pr_behind", "your PR fell behind its base", nil, nil),
			githubEvent("failing_checks", "CI concluded failing on your PR",
				plugin.Schema{"failing_check": {Type: "string"}, "run_id": {Type: "integer"}},
				plugin.Schema{
					"flaky_rerun": {Type: "map", Desc: "rerun failed jobs once before dispatching: { enabled, max }"},
					// Per-CHECK suppression, not a trigger predicate: it decides
					// which failing check counts as an event at all, one check at
					// a time, before any trigger is consulted. That is the same
					// kind of thing flaky_rerun is, so it lives beside it.
					"ignore_checks": {Type: "list", Desc: "check names that never trigger"},
				}),
			githubEvent("stuck_checks", "a CI run has been running too long on your PR",
				plugin.Schema{"run_id": {Type: "integer"}, "run_name": {Type: "string"}, "run_status": {Type: "string"}},
				plugin.Schema{
					"stuck_after":   {Type: "duration", Desc: "how long a run may take before it is stuck (default 30m)"},
					"poll_interval": {Type: "duration", Desc: "poller cadence (default 15m)"},
				}),
			githubEvent("merge_ready", "your PR turned all-green", nil, nil),
			githubEvent("self_review", "you opened/updated your own PR", nil, nil),
			githubEvent("issue_matched", "an issue matches your criteria",
				nil,
				plugin.Schema{
					"assignee": {Type: "map", Desc: "whose assignment triggers: { logins: [...] } (default: the connector's me:) — an identity gate, not a fact predicate"},
				}),
			githubEvent("release", "a release was published",
				plugin.Schema{"tag_name": {Type: "string"}, "prerelease": {Type: "boolean"}, "draft": {Type: "boolean"}},
				plugin.Schema{"include_prereleases": {Type: "boolean", Desc: "also fire on prereleases (default: skip them)"}}),
			githubEvent("deployment_status", "a deployment failed or errored",
				plugin.Schema{"state": {Type: "string"}, "environment": {Type: "string"}, "description": {Type: "string"}}, nil),
			githubEvent("dependabot_alert", "a new Dependabot alert",
				plugin.Schema{"severity": {Type: "string"}, "package": {Type: "string"}, "summary": {Type: "string"}}, nil),
			githubEvent("secret_scanning_alert", "a new secret-scanning alert",
				plugin.Schema{"secret_type": {Type: "string"}}, nil),
			githubEvent(ClosedEvent, "a PR closed (merged or not) — terminal for the PR: runs on it stop, and the outcome is recorded",
				plugin.Schema{"merged": {Type: "boolean"}, "reverts": {Type: "list", Desc: "PR numbers this one reverts"},
					"reverts_corroborated": {Type: "boolean", Desc: "the revert claim is backed by the commit messages"}}, nil),
		},
		Verbs: []plugin.Verb{
			{
				Name: "comment", Semantics: &plugin.VerbSemantics{ConversationPost: true}, Desc: "post an issue/PR conversation comment",
				Options: plugin.Schema{
					"repo":   {Type: "string", Required: true, Scope: "repo"},
					"number": {Type: "integer", Desc: "issue or PR number (alias: pr)"},
					"pr":     {Type: "integer"},
					"body":   {Type: "string", Required: true},
					"as":     {Type: "string", Enum: []string{"me", "bot"}, Desc: "identity (default me)"},
				},
				Outputs: plugin.Schema{"id": {Type: "integer"}, "url": {Type: "string"}},
			},
			{
				Name: "reply", Semantics: &plugin.VerbSemantics{ConversationPost: true}, Desc: "reply to a PR review comment thread",
				Options: plugin.Schema{
					"repo":        {Type: "string", Required: true, Scope: "repo"},
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
					"repo":           {Type: "string", Required: true, Scope: "repo"},
					"pr":             {Type: "integer", Required: true},
					"reviewers":      {Type: "list", Desc: "user logins"},
					"team_reviewers": {Type: "list", Desc: "team slugs"},
					"as":             {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				// Re-requesting a prior reviewer is the same GitHub call as
				// request_review, but guarded: by default only reviewers still
				// waiting on changes are pinged (see only_outstanding).
				Name: "rerequest_review", Desc: "re-request review from reviewers whose latest review requested changes on an older commit (skips approvers, pending requests, closed PRs)",
				Options: plugin.Schema{
					"repo":             {Type: "string", Required: true, Scope: "repo"},
					"pr":               {Type: "integer", Required: true},
					"reviewers":        {Type: "list", Desc: "logins"},
					"team_reviewers":   {Type: "list", Desc: "team slugs"},
					"as":               {Type: "string", Enum: []string{"me", "bot"}},
					"only_outstanding": {Type: "boolean", Desc: "default true: ping only reviewers whose latest review is CHANGES_REQUESTED on an older commit and who aren't already requested, on an open PR; false re-requests unconditionally"},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}, "skipped": {Type: "string"}},
			},
			{
				Name: "remove_reviewer", Desc: "cancel a pending review request (remove requested users/teams)",
				Options: plugin.Schema{
					"repo":           {Type: "string", Required: true, Scope: "repo"},
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
					"repo":  {Type: "string", Required: true, Scope: "repo"},
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
					"repo": {Type: "string", Required: true, Scope: "repo"}, "pr": {Type: "integer", Required: true},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"diff": {Type: "string"}},
			},
			{
				Name: "read_token", Desc: "the read credential agents receive (identity.read_token: the App's installation token by default)",
				Semantics: &plugin.VerbSemantics{HostOnly: true, MintsCredential: &plugin.MintsCredential{Credential: "read"}},
				Options:   plugin.Schema{"repo": {Type: "string", Required: true, Scope: "repo"}},
				Outputs:   plugin.Schema{"token": {Type: "string"}},
			},
			{
				Name: "write_token", Desc: "the write credential agents receive (identity.write_token: your gh login by default)",
				Semantics: &plugin.VerbSemantics{HostOnly: true, MintsCredential: &plugin.MintsCredential{Credential: "write"}},
				Options:   plugin.Schema{"repo": {Type: "string", Required: true, Scope: "repo"}},
				Outputs:   plugin.Schema{"token": {Type: "string"}},
			},
			{
				Name: "pr_head", Desc: "a PR's current head commit and state (open | closed | merged), read fresh — what the engine's run facts read",
				Semantics: &plugin.VerbSemantics{HostOnly: true, ReadsRevision: &plugin.ReadsRevision{
					Args:     map[string]string{"repo": "{{.repo}}", "pr": "{{.number}}"},
					Revision: "sha", State: "state",
					States:  map[string][]string{"open": {"open"}, "closed": {"closed"}, "accepted": {"merged"}},
					Reasons: map[string]string{"accepted": "the PR merged", "closed": "the PR closed"},
				}},
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true, Scope: "repo"}, "pr": {Type: "integer", Required: true},
				},
				Outputs: plugin.Schema{"sha": {Type: "string"}, "state": {Type: "string"}},
			},
			{
				Name: "pr_get", Desc: "PR metadata + review status: state, merged, base/head, line counts, labels, and the current review decision/approvals",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true, Scope: "repo"}, "pr": {Type: "integer", Required: true},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{
					"title": {Type: "string"}, "body": {Type: "string"}, "state": {Type: "string"},
					"draft": {Type: "boolean"}, "merged": {Type: "boolean"}, "mergeable": {Type: "boolean"},
					"author": {Type: "string"}, "base": {Type: "string"},
					"head": {Type: "string"}, "head_sha": {Type: "string"}, "additions": {Type: "integer"},
					"deletions": {Type: "integer"}, "changed_files": {Type: "integer"}, "labels": {Type: "list"}, "url": {Type: "string"},
					"review_decision": {Type: "string", Desc: "APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED, derived from the latest review per reviewer"},
					"approvals":       {Type: "integer", Desc: "count of reviewers whose latest review is APPROVED"},
					"approvers":       {Type: "list", Desc: "logins of reviewers whose latest review is APPROVED"},
				},
			},
			{
				Name: "pr_files", Desc: "changed files: [{path, status, additions, deletions, changes}] (100/page; pass page for more)",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true, Scope: "repo"}, "pr": {Type: "integer", Required: true},
					"all": {Type: "boolean", Desc: "fetch every page (default: first 100)"},
					"as":  {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"files": {Type: "list"}},
			},
			{
				Name: "review_comments", Desc: "existing inline review comments on the PR: [{path, line, body, user, id}] (100/page)",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true, Scope: "repo"}, "pr": {Type: "integer", Required: true},
					"all": {Type: "boolean", Desc: "fetch every page (default: first 100)"},
					"as":  {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"comments": {Type: "list"}},
			},
			{
				Name: "file", Desc: "a repo file's raw contents at a ref (cached; GitHub's raw media type caps at ~1 MiB)",
				Options: plugin.Schema{
					"repo":     {Type: "string", Required: true, Scope: "repo"},
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
					"repo":  {Type: "string", Required: true, Scope: "repo"},
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
					"repo": {Type: "string", Required: true, Scope: "repo"}, "pr": {Type: "integer", Required: true},
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
					"repo": {Type: "string", Required: true, Scope: "repo"}, "pr": {Type: "integer", Required: true},
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
					"repo": {Type: "string", Required: true, Scope: "repo"}, "title": {Type: "string", Required: true},
					"body":   {Type: "string"},
					"labels": {Type: "list"}, "assignees": {Type: "list", Desc: "logins to assign"},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"number": {Type: "integer"}, "url": {Type: "string"}},
			},
			{
				Name: "update_issue", Desc: "edit an issue: state (open|closed → close/reopen), state_reason, title, body",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true, Scope: "repo"}, "number": {Type: "integer", Required: true},
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
					"repo":   {Type: "string", Required: true, Scope: "repo"},
					"number": {Type: "integer", Desc: "issue or PR number (alias: pr)"}, "pr": {Type: "integer"},
					"add": {Type: "list", Desc: "logins to assign"}, "remove": {Type: "list", Desc: "logins to unassign"},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"assignees": {Type: "list"}},
			},
			{
				Name: "remove_label", Desc: "remove one label from an issue or PR",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true, Scope: "repo"}, "number": {Type: "integer", Required: true},
					"label": {Type: "string", Required: true},
					"as":    {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "get_issue", Desc: "read an issue: title, body, state, labels, assignees, author, url",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true, Scope: "repo"}, "number": {Type: "integer", Required: true},
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
					"repo": {Type: "string", Required: true, Scope: "repo"}, "path": {Type: "string", Required: true},
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
					"repo": {Type: "string", Required: true, Scope: "repo"}, "path": {Type: "string", Required: true},
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
					"repo": {Type: "string", Required: true, Scope: "repo"},
					"ref":  {Type: "string", Required: true, Desc: "branch, tag, or sha"},
					"as":   {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"sha": {Type: "string"}},
			},
			{
				Name: "create_branch", Desc: "create a branch from another ref",
				Options: plugin.Schema{
					"repo":   {Type: "string", Required: true, Scope: "repo"},
					"branch": {Type: "string", Required: true, Desc: "new branch name"},
					"from":   {Type: "string", Desc: "source branch/tag/sha (default: the default branch's HEAD)"},
					"as":     {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"sha": {Type: "string"}},
			},
			{
				Name: "dispatch_workflow", Desc: "trigger a workflow_dispatch run",
				Options: plugin.Schema{
					"repo":     {Type: "string", Required: true, Scope: "repo"},
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
					"repo": {Type: "string", Required: true, Scope: "repo"}, "run_id": {Type: "integer", Required: true},
					"failed_only": {Type: "boolean", Desc: "re-run only failed jobs"},
					"as":          {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "cancel_run", Desc: "cancel a workflow run",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true, Scope: "repo"}, "run_id": {Type: "integer", Required: true},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "list_runs", Desc: "recent workflow runs: [{id, name, status, conclusion, head_branch, head_sha, url}]",
				Options: plugin.Schema{
					"repo":     {Type: "string", Required: true, Scope: "repo"},
					"branch":   {Type: "string", Desc: "filter to a branch"},
					"status":   {Type: "string", Desc: "queued|in_progress|completed|success|failure|…"},
					"per_page": {Type: "integer", Desc: "default 20, max 100"},
					"all":      {Type: "boolean", Desc: "fetch every page"},
					"as":       {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"runs": {Type: "list"}},
			},
			{
				Name: "get_run", Desc: "one workflow run by id: {run_id, name, status, conclusion, head_branch, head_sha, url}",
				Options: plugin.Schema{
					"repo":   {Type: "string", Required: true, Scope: "repo"},
					"run_id": {Type: "integer", Required: true},
					"as":     {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{
					"run_id": {Type: "integer"}, "name": {Type: "string"}, "status": {Type: "string"},
					"conclusion": {Type: "string"}, "head_branch": {Type: "string"},
					"head_sha": {Type: "string"}, "url": {Type: "string"},
				},
			},
			{
				Name: "create_release", Desc: "publish a release for a tag",
				Options: plugin.Schema{
					"repo":   {Type: "string", Required: true, Scope: "repo"},
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
					"repo":         {Type: "string", Required: true, Scope: "repo"},
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
					"repo":     {Type: "string", Required: true, Scope: "repo"},
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
					"repo":     {Type: "string", Required: true, Scope: "repo"},
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
					"repo": {Type: "string", Required: true, Scope: "repo"},
					"ref":  {Type: "string", Required: true, Desc: "branch, tag, or sha"},
					"as":   {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"checks": {Type: "list"}},
			},
			{
				Name: "ready_for_review", Desc: "mark a draft PR ready for review",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true, Scope: "repo"}, "pr": {Type: "integer", Required: true},
					"as": {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "convert_to_draft", Desc: "convert a PR back to a draft",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true, Scope: "repo"}, "pr": {Type: "integer", Required: true},
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
					"repo":   {Type: "string", Required: true, Scope: "repo"},
					"number": {Type: "integer", Required: true},
					"labels": {Type: "list", Required: true},
					"as":     {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "react", Desc: "add (or, remove: true, take away) your reaction on comments/reviews — idempotent both ways",
				Options: plugin.Schema{
					"repo":     {Type: "string", Required: true, Scope: "repo"},
					"pr":       {Type: "integer", Desc: "the PR (required for a review subject)"},
					"subjects": {Type: "list", Desc: "[{kind, id}] — kind is issue_comment | review_comment | review; an event's reaction_subjects is this shape"},
					"kind":     {Type: "string", Enum: []string{githubkit.SubjectIssueComment, githubkit.SubjectReviewComment, githubkit.SubjectReview}, Desc: "single-subject shorthand (with id)"},
					"id":       {Type: "integer", Desc: "single-subject shorthand (with kind)"},
					"content":  {Type: "string", Required: true, Enum: githubkit.ReactionContents()},
					"remove":   {Type: "boolean", Desc: "take the reaction away instead: only the acting user's reaction of this content; a no-op where there is none"},
					"as":       {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}, "reacted": {Type: "integer", Desc: "subjects reacted to"}, "removed": {Type: "integer", Desc: "reactions removed (remove: true)"}},
			},
			{
				Name: "set_status", Desc: "post a commit status on a sha, or on a PR's head as it is at call time (shown on any PR whose head it is)",
				Options: plugin.Schema{
					"repo":        {Type: "string", Required: true, Scope: "repo"},
					"sha":         {Type: "string", Desc: "the commit (one of sha / pr)"},
					"pr":          {Type: "integer", Desc: "a PR whose CURRENT head gets the status, read at call time (one of sha / pr; sha wins when both are set)"},
					"state":       {Type: "string", Required: true, Enum: githubkit.StatusStates()},
					"description": {Type: "string", Desc: "clipped to GitHub's 140 characters"},
					"context":     {Type: "string", Desc: "the status's name on the PR, entirely yours (templates allowed); default only when unset: the login the call acts as"},
					"target_url":  {Type: "string"},
					"as":          {Type: "string", Enum: []string{"me", "bot"}},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}, "context": {Type: "string"}, "sha": {Type: "string", Desc: "the commit the status went on"}},
			},
			{
				Name: "sweep", Desc: "run the catch-up sweep now (daemon-global; same as `conductor sweep --now`)",
				Options: plugin.Schema{},
				Outputs: plugin.Schema{"nudged": {Type: "integer", Desc: "integrations whose sweep was nudged"}},
			},
		},
	}
}
