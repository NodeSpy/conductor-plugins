package ghsourcetest

import (
	"fmt"
	"time"
)

// The cases. Every one is App-less (a static token — the source's reads use
// it in place of an installation token) with `me` = "me", on repo acme/w.

const repo = "acme/w"

// base is the connection every case starts from: a direct listener, the mock
// API, `me`, and the sweep off unless a case turns it on.
func base(env Env) map[string]any {
	return map[string]any{
		"token":    "tok",
		"api_base": env.API,
		"me":       map[string]any{"logins": []any{"me"}},
		"webhook":  map[string]any{"listen": env.Listen, "path": env.Path, "secret": Secret},
		"sweep":    map[string]any{"enabled": false},
	}
}

func withSweep(env Env) map[string]any {
	c := base(env)
	c["sweep"] = map[string]any{"enabled": true, "interval": "1h", "min_interval": "1h", "repos": []any{repo}}
	return c
}

// pull is the REST pull-request read: author, state, head/base, mergeability.
func pull(n int, author, state, mergeable string) Route {
	return Route{Method: "GET", Path: fmt.Sprintf("/repos/%s/pulls/%d", repo, n), Body: map[string]any{
		"number": n, "state": state, "title": fmt.Sprintf("PR %d", n),
		"html_url":        fmt.Sprintf("https://github.example/%s/pull/%d", repo, n),
		"head":            map[string]any{"sha": fmt.Sprintf("h%d", n), "ref": fmt.Sprintf("feat-%d", n)},
		"base":            map[string]any{"ref": "main"},
		"user":            map[string]any{"login": author},
		"labels":          []any{map[string]any{"name": "area"}},
		"mergeable_state": mergeable,
	}}
}

const repoJSON = `"repository":{"full_name":"acme/w","name":"w","default_branch":"main","owner":{"login":"acme"}},"installation":{"id":1}`

func prJSON(n int, author, state string) string {
	return fmt.Sprintf(`{"number":%d,"state":%q,"title":"PR %d","html_url":"https://github.example/acme/w/pull/%d",`+
		`"head":{"sha":"h%d","ref":"feat-%d"},"base":{"ref":"main"},"user":{"login":%q},"labels":[]}`,
		n, state, n, n, n, n, author)
}

func issueComment(n, id int, prAuthor, commenter, body string) string {
	return fmt.Sprintf(`{"action":"created",%s,"issue":{"number":%d,"state":"open","pull_request":{},"user":{"login":%q},"labels":[]},`+
		`"comment":{"id":%d,"body":%q,"user":{"login":%q,"type":"User"}}}`, repoJSON, n, prAuthor, id, body, commenter)
}

func review(n int, prState string, id int, state, reviewer string) string {
	return fmt.Sprintf(`{"action":"submitted",%s,"pull_request":%s,"review":{"id":%d,"state":%q,"body":"see inline","user":{"login":%q,"type":"User"}}}`,
		repoJSON, prJSON(n, "me", prState), id, state, reviewer)
}

func reviewComment(n, id, reviewID int, reviewer string) string {
	return fmt.Sprintf(`{"action":"created",%s,"pull_request":%s,"comment":{"id":%d,"body":"fix this","pull_request_review_id":%d,"user":{"login":%q,"type":"User"}}}`,
		repoJSON, prJSON(n, "me", "open"), id, reviewID, reviewer)
}

func reviewComments(n, reviewID int, ids ...int) Route {
	var cs []any
	for _, id := range ids {
		cs = append(cs, map[string]any{"id": id, "user": map[string]any{"login": "rev"}, "path": "a.go", "line": 3,
			"body": "fix this", "html_url": fmt.Sprintf("https://github.example/c/%d", id)})
	}
	return Route{Method: "GET", Path: fmt.Sprintf("/repos/%s/pulls/%d/reviews/%d/comments", repo, n, reviewID), Body: cs}
}

func checkRun(n int, name, conclusion string, run int) string {
	return fmt.Sprintf(`{"action":"completed",%s,"check_run":{"id":555,"name":%q,"conclusion":%q,"head_sha":"h%d",`+
		`"details_url":"https://github.example/acme/w/actions/runs/%d/job/555","pull_requests":[{"number":%d}]}}`,
		repoJSON, name, conclusion, n, run, n)
}

// gate is the merge-readiness GraphQL read for one PR (told apart by the
// query's $num variable).
func gate(n int, mergeState, decision string) Route {
	return Route{Method: "POST", Path: "/graphql", BodyHas: fmt.Sprintf(`"num":%d,`, n), Body: map[string]any{"data": map[string]any{
		"repository": map[string]any{"pullRequest": map[string]any{
			"headRefOid": fmt.Sprintf("h%d", n), "mergeStateStatus": mergeState, "reviewDecision": decision, "isDraft": false,
			"author": map[string]any{"login": "me"}, "labels": map[string]any{"nodes": []any{}},
			"reviewThreads": map[string]any{"nodes": []any{}},
			"approvals":     map[string]any{"nodes": []any{map[string]any{"author": map[string]any{"login": "rev"}}}},
		}}}}}
}

func prEvent(action string, n int, author string) string {
	return fmt.Sprintf(`{"action":%q,%s,"pull_request":%s}`, action, repoJSON, prJSON(n, author, "open"))
}

// Cases returns the conformance table.
func Cases() []Case {
	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	stale := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	return []Case{
		{
			Name:       "new_comment on your PR, not on others or by you",
			Connection: base,
			Triggers:   []Trigger{{On: "new_comment", Name: "nc"}},
			Routes:     []Route{pull(7, "me", "open", "clean")},
			Steps: []Step{
				{Event: "issue_comment", Body: issueComment(7, 501, "me", "rev", "please fix")},
				{Event: "issue_comment", Body: issueComment(7, 502, "me", "me", "my own")},
				{Event: "issue_comment", Body: issueComment(8, 503, "someone", "rev", "not yours")},
			},
			Want: []Want{{Kind: "new_comment", Trigger: "nc", Repo: repo, Number: 7, Context: map[string]any{
				"author": "rev", "comment_id": 501, "comment_kind": "issue", "head_ref": "feat-7",
				"installation_id": 1, "comment_body": "please fix", "author_is_bot": false,
			}, Len: map[string]int{"reaction_subjects": 1}}},
		},
		{
			Name:       "review_requested for your review",
			Connection: base,
			Triggers:   []Trigger{{On: "review_requested", Name: "rr"}},
			Steps: []Step{{Event: "pull_request", Body: fmt.Sprintf(`{"action":"review_requested",%s,"pull_request":%s,"requested_reviewer":{"login":"me"}}`,
				repoJSON, prJSON(9, "someone", "open"))}},
			Want: []Want{{Kind: "review_requested", Trigger: "rr", Repo: repo, Number: 9}},
		},
		{
			Name:       "reviewer and assignee options gate on the named identity, not on me",
			Connection: base,
			Triggers: []Trigger{
				{On: "review_requested", Name: "lead", Options: map[string]any{"reviewer": map[string]any{"logins": []any{"teamlead"}}}},
				{On: "issue_matched", Name: "ops", Options: map[string]any{"assignee": map[string]any{"logins": []any{"oncall"}}}},
			},
			Steps: []Step{
				{Event: "pull_request", Body: fmt.Sprintf(`{"action":"review_requested",%s,"pull_request":%s,"requested_reviewer":{"login":"teamlead"}}`, repoJSON, prJSON(30, "someone", "open"))},
				{Event: "pull_request", Body: fmt.Sprintf(`{"action":"review_requested",%s,"pull_request":%s,"requested_reviewer":{"login":"me"}}`, repoJSON, prJSON(31, "someone", "open"))},
				{Event: "issues", Body: fmt.Sprintf(`{"action":"assigned",%s,"issue":{"number":32,"state":"open","title":"page","user":{"login":"x"},"assignees":[{"login":"oncall"}],"labels":[]}}`, repoJSON)},
				{Event: "issues", Body: fmt.Sprintf(`{"action":"assigned",%s,"issue":{"number":33,"state":"open","title":"mine","user":{"login":"x"},"assignees":[{"login":"me"}],"labels":[]}}`, repoJSON)},
			},
			Want: []Want{
				{Kind: "review_requested", Trigger: "lead", Repo: repo, Number: 30},
				{Kind: "issue_matched", Trigger: "ops", Repo: repo, Number: 32},
			},
		},
		{
			Name:       "a changes-request and its inline comments are ONE changes_requested",
			Connection: base,
			Triggers:   []Trigger{{On: "changes_requested", Name: "cr"}, {On: "new_comment", Name: "nc"}},
			Routes:     []Route{pull(7, "me", "open", "clean"), reviewComments(7, 900, 1001, 1002)},
			Steps: []Step{
				{Event: "pull_request_review", Body: review(7, "open", 900, "changes_requested", "rev")},
				{Event: "pull_request_review_comment", Body: reviewComment(7, 1001, 900, "rev")},
				{Event: "pull_request_review_comment", Body: reviewComment(7, 1002, 900, "rev")},
			},
			Want: []Want{{Kind: "changes_requested", Trigger: "cr", Repo: repo, Number: 7, Context: map[string]any{
				"review_id": 900, "comment_id": 1002, "comment_kind": "review", "author": "rev",
				"review_state": "changes_requested", "head_ref": "feat-7",
			}, Len: map[string]int{"review_comments": 2}}},
		},
		{
			Name:       "the inline comments arriving FIRST still fold into one",
			Connection: base,
			Triggers:   []Trigger{{On: "changes_requested", Name: "cr"}, {On: "new_comment", Name: "nc"}},
			Routes: []Route{pull(7, "me", "open", "clean"), reviewComments(7, 910, 1011),
				{Method: "GET", Path: "/repos/" + repo + "/pulls/7/reviews/910", Body: map[string]any{
					"id": 910, "state": "COMMENTED", "body": "bot review", "user": map[string]any{"login": "bugbot[bot]", "type": "Bot"}}}},
			Steps: []Step{
				{Event: "pull_request_review_comment", Body: reviewComment(7, 1011, 910, "bugbot[bot]")},
				{Event: "pull_request_review", Body: review(7, "open", 910, "commented", "bugbot[bot]")},
			},
			Want: []Want{{Kind: "changes_requested", Trigger: "cr", Repo: repo, Number: 7, Context: map[string]any{
				"review_id": 910, "comment_id": 1011, "author_is_bot": true, "review_state": "commented",
			}}},
		},
		{
			Name:       "an approval with suggestions is ONE new_comment",
			Connection: base,
			Triggers:   []Trigger{{On: "changes_requested", Name: "cr"}, {On: "new_comment", Name: "nc"}},
			Routes:     []Route{pull(7, "me", "open", "clean"), reviewComments(7, 901, 1003)},
			Steps: []Step{
				{Event: "pull_request_review", Body: review(7, "open", 901, "approved", "rev")},
				{Event: "pull_request_review_comment", Body: reviewComment(7, 1003, 901, "rev")},
			},
			Want: []Want{{Kind: "new_comment", Trigger: "nc", Repo: repo, Number: 7, Context: map[string]any{
				"review_id": 901, "comment_id": 1003, "review_state": "approved", "comment_kind": "review",
			}}},
		},
		{
			Name:       "a closed PR's feedback is dropped; closing emits _closed",
			Connection: base,
			Triggers:   []Trigger{{On: "changes_requested", Name: "cr"}},
			Routes:     []Route{reviewComments(7, 902, 1004)},
			Steps: []Step{
				{Event: "pull_request_review", Body: review(7, "closed", 902, "changes_requested", "rev")},
				{Event: "pull_request", Body: fmt.Sprintf(`{"action":"closed",%s,"pull_request":{"number":7,"state":"closed","merged":true,"title":"PR 7","head":{"sha":"h7"},"base":{"ref":"main"},"user":{"login":"me"}}}`, repoJSON)},
			},
			Want: []Want{{Kind: "_closed", Repo: repo, Number: 7, Context: map[string]any{"merged": true}}},
		},
		{
			Name:       "failing_checks on your PR, minus ignore_checks",
			Connection: base,
			Triggers:   []Trigger{{On: "failing_checks", Name: "fc", Options: map[string]any{"ignore_checks": []any{"docs"}}}},
			Routes:     []Route{pull(7, "me", "open", "clean")},
			Steps: []Step{
				{Event: "check_run", Body: checkRun(7, "lint", "failure", 77)},
				{Event: "check_run", Body: checkRun(7, "docs", "failure", 78)},
				{Event: "check_run", Body: checkRun(7, "unit", "success", 79)},
			},
			Want: []Want{{Kind: "failing_checks", Trigger: "fc", Repo: repo, Number: 7, Context: map[string]any{
				"failing_check": "lint", "run_id": 77, "head_ref": "feat-7"}}},
		},
		{
			Name:       "merge_conflict and pr_behind from the PR's merge state",
			Connection: base,
			Triggers:   []Trigger{{On: "merge_conflict", Name: "mc"}, {On: "pr_behind", Name: "pb"}},
			Routes:     []Route{pull(7, "me", "open", "dirty"), pull(8, "me", "open", "behind"), pull(10, "someone", "open", "dirty")},
			Steps: []Step{
				{Event: "pull_request", Body: prEvent("synchronize", 7, "me")},
				{Event: "pull_request", Body: prEvent("synchronize", 8, "me")},
				{Event: "pull_request", Body: prEvent("synchronize", 10, "someone")},
			},
			Want: []Want{
				{Kind: "merge_conflict", Trigger: "mc", Repo: repo, Number: 7, Context: map[string]any{"head_ref": "feat-7"}},
				{Kind: "pr_behind", Trigger: "pb", Repo: repo, Number: 8, Context: map[string]any{"head_ref": "feat-8"}},
			},
		},
		{
			Name:       "merge_ready when all-green, held by a failing gate",
			Connection: base,
			Triggers:   []Trigger{{On: "merge_ready", Name: "mr"}},
			Routes: []Route{
				gate(7, "CLEAN", "APPROVED"),
				gate(8, "BLOCKED", "REVIEW_REQUIRED"),
			},
			Steps: []Step{
				{Event: "check_run", Body: checkRun(7, "unit", "success", 80)},
				{Event: "check_run", Body: checkRun(8, "unit", "success", 81)},
			},
			Want: []Want{{Kind: "merge_ready", Trigger: "mr", Repo: repo, Number: 7}},
		},
		{
			Name:       "self_review when you open a PR",
			Connection: base,
			Triggers:   []Trigger{{On: "self_review", Name: "sr"}},
			Routes:     []Route{pull(11, "me", "open", "clean")},
			Steps: []Step{
				{Event: "pull_request", Body: prEvent("opened", 11, "me")},
				{Event: "pull_request", Body: prEvent("opened", 12, "someone")},
			},
			Want: []Want{{Kind: "self_review", Trigger: "sr", Repo: repo, Number: 11}},
		},
		{
			Name:       "issue_matched: assigned to you, and the trigger's filter",
			Connection: base,
			Triggers:   []Trigger{{On: "issue_matched", Name: "im", Filter: map[string]any{"label_any": []any{"bug"}}}},
			Steps: []Step{
				{Event: "issues", Body: fmt.Sprintf(`{"action":"labeled",%s,"issue":{"number":21,"state":"open","title":"broken","user":{"login":"x"},"assignees":[{"login":"me"}],"labels":[{"name":"bug"}]}}`, repoJSON)},
				{Event: "issues", Body: fmt.Sprintf(`{"action":"labeled",%s,"issue":{"number":22,"state":"open","title":"docs","user":{"login":"x"},"assignees":[{"login":"me"}],"labels":[{"name":"docs"}]}}`, repoJSON)},
				{Event: "issues", Body: fmt.Sprintf(`{"action":"labeled",%s,"issue":{"number":23,"state":"open","title":"theirs","user":{"login":"x"},"assignees":[{"login":"them"}],"labels":[{"name":"bug"}]}}`, repoJSON)},
			},
			Want: []Want{{Kind: "issue_matched", Trigger: "im", Repo: repo, Number: 21}},
		},
		{
			Name:       "release, with prereleases only where asked",
			Connection: base,
			Triggers: []Trigger{{On: "release", Name: "rel"},
				{On: "release", Name: "relpre", Options: map[string]any{"include_prereleases": true}}},
			Steps: []Step{
				{Event: "release", Body: fmt.Sprintf(`{"action":"published",%s,"release":{"tag_name":"v1","html_url":"u1","target_commitish":"main","draft":false,"prerelease":false}}`, repoJSON)},
				{Event: "release", Body: fmt.Sprintf(`{"action":"published",%s,"release":{"tag_name":"v2-rc","html_url":"u2","target_commitish":"main","draft":false,"prerelease":true}}`, repoJSON)},
			},
			Want: []Want{
				{Kind: "release", Trigger: "rel", Repo: repo, Context: map[string]any{"tag_name": "v1"}},
				{Kind: "release", Trigger: "relpre", Repo: repo, Context: map[string]any{"tag_name": "v1"}},
				{Kind: "release", Trigger: "relpre", Repo: repo, Context: map[string]any{"tag_name": "v2-rc", "prerelease": true}},
			},
		},
		{
			Name:       "deployment failures and security alerts",
			Connection: base,
			Triggers: []Trigger{{On: "deployment_status", Name: "ds"}, {On: "dependabot_alert", Name: "da"},
				{On: "secret_scanning_alert", Name: "ss"}},
			Steps: []Step{
				{Event: "deployment_status", Body: fmt.Sprintf(`{%s,"deployment_status":{"state":"failure","environment":"prod","description":"boom","target_url":"u"},"deployment":{"sha":"d1","ref":"main"}}`, repoJSON)},
				{Event: "deployment_status", Body: fmt.Sprintf(`{%s,"deployment_status":{"state":"success","environment":"prod"},"deployment":{"sha":"d2","ref":"main"}}`, repoJSON)},
				{Event: "dependabot_alert", Body: fmt.Sprintf(`{"action":"created",%s,"alert":{"number":3,"html_url":"u","dependency":{"package":{"name":"lodash"}},"security_advisory":{"severity":"high","summary":"proto"}}}`, repoJSON)},
				{Event: "secret_scanning_alert", Body: fmt.Sprintf(`{"action":"created",%s,"alert":{"number":4,"html_url":"u","secret_type":"t","secret_type_display_name":"Token"}}`, repoJSON)},
			},
			Want: []Want{
				{Kind: "deployment_status", Trigger: "ds", Repo: repo, Context: map[string]any{"state": "failure", "environment": "prod"}},
				{Kind: "dependabot_alert", Trigger: "da", Repo: repo, Context: map[string]any{"severity": "high", "package": "lodash"}},
				{Kind: "secret_scanning_alert", Trigger: "ss", Repo: repo, Context: map[string]any{"secret_type": "Token"}},
			},
		},
		{
			Name:       "triggers on one event route by their repo filters",
			Connection: base,
			Triggers: []Trigger{
				{On: "release", Name: "w", Filter: map[string]any{"repo": []any{"acme/w"}}},
				{On: "release", Name: "x", Filter: map[string]any{"repo": []any{"acme/x"}}},
			},
			Steps: []Step{
				{Event: "release", Body: fmt.Sprintf(`{"action":"published",%s,"release":{"tag_name":"v9","draft":false,"prerelease":false}}`, repoJSON)},
				{Event: "release", Body: `{"action":"published","repository":{"full_name":"acme/x","name":"x","owner":{"login":"acme"}},"release":{"tag_name":"v9","draft":false,"prerelease":false}}`},
			},
			Want: []Want{
				{Kind: "release", Trigger: "w", Repo: "acme/w"},
				{Kind: "release", Trigger: "x", Repo: "acme/x"},
			},
		},
		{
			Name:       "a unified filter predicate drops a commenter",
			Connection: base,
			Triggers:   []Trigger{{On: "new_comment", Name: "human", Filter: map[string]any{"not_comment_author": []any{"ci[bot]"}}}},
			Routes:     []Route{pull(7, "me", "open", "clean")},
			Steps: []Step{
				{Event: "issue_comment", Body: issueComment(7, 601, "me", "ci[bot]", "report")},
				{Event: "issue_comment", Body: issueComment(7, 602, "me", "rev", "a real one")},
			},
			Want: []Want{{Kind: "new_comment", Trigger: "human", Repo: repo, Number: 7, Context: map[string]any{"comment_id": 602}}},
		},
		{
			Name:       "the sweep recovers pending reviews, conflicts, open threads, missed comments",
			Connection: withSweep,
			Triggers: []Trigger{{On: "review_requested", Name: "rr"}, {On: "merge_conflict", Name: "mc"},
				{On: "changes_requested", Name: "cr"}, {On: "new_comment", Name: "nc"}},
			Routes: []Route{
				{Method: "GET", Path: "/repos/" + repo + "/pulls", Body: []any{
					map[string]any{"number": 7, "title": "PR 7", "user": map[string]any{"login": "me"},
						"head": map[string]any{"sha": "h7", "ref": "feat-7"}, "base": map[string]any{"ref": "main"}},
					map[string]any{"number": 8, "title": "PR 8", "user": map[string]any{"login": "someone"},
						"head": map[string]any{"sha": "h8", "ref": "feat-8"}, "base": map[string]any{"ref": "main"},
						"requested_reviewers": []any{map[string]any{"login": "me"}}},
				}},
				pull(7, "me", "open", "dirty"),
				{Method: "POST", Path: "/graphql", BodyHas: "latestOpinionatedReviews", Body: map[string]any{"data": map[string]any{
					"repository": map[string]any{"pullRequest": map[string]any{
						"latestOpinionatedReviews": map[string]any{"nodes": []any{}},
						"reviewThreads": map[string]any{"nodes": []any{map[string]any{"id": "T1", "isResolved": false,
							"comments": map[string]any{"nodes": []any{map[string]any{"databaseId": 3001, "author": map[string]any{"login": "rev", "__typename": "User"},
								"path": "a.go", "line": 4, "body": "still wrong", "url": "u"}}}}}},
					}}}}},
				{Method: "GET", Path: "/repos/" + repo + "/issues/7/comments", Body: []any{
					map[string]any{"id": 4001, "body": "ping", "user": map[string]any{"login": "rev"}, "created_at": recent}}},
				{Method: "GET", Path: "/repos/" + repo + "/pulls/7/comments", Body: []any{}},
			},
			Want: []Want{
				{Kind: "review_requested", Trigger: "rr", Repo: repo, Number: 8, CatchUp: true},
				{Kind: "merge_conflict", Trigger: "mc", Repo: repo, Number: 7, CatchUp: true, Context: map[string]any{"head_ref": "feat-7"}},
				{Kind: "changes_requested", Trigger: "cr", Repo: repo, Number: 7, CatchUp: true,
					Context: map[string]any{"comment_id": 3001, "author": "rev"}, Absent: []string{"review_id"}},
				{Kind: "new_comment", Trigger: "nc", Repo: repo, Number: 7, CatchUp: true, Context: map[string]any{"comment_id": 4001}},
			},
		},
		{
			// The stuck-check poller runs on its own cadence, independent of
			// the sweep (off here), over the repos its trigger is scoped to.
			// Its 2s interval fires once inside the runner's wait window.
			Name: "stuck_checks from the poller, on your PR only",
			Connection: func(env Env) map[string]any {
				c := base(env)
				c["repos"] = []any{repo}
				return c
			},
			Triggers: []Trigger{{On: "stuck_checks", Name: "st", Options: map[string]any{"stuck_after": "30m", "poll_interval": "2s"}}},
			Routes: []Route{
				{Method: "GET", Path: "/repos/" + repo + "/pulls", Body: []any{
					map[string]any{"number": 7, "title": "PR 7", "user": map[string]any{"login": "me"},
						"head": map[string]any{"sha": "h7", "ref": "feat-7"}, "base": map[string]any{"ref": "main"}},
					map[string]any{"number": 8, "title": "PR 8", "user": map[string]any{"login": "someone"},
						"head": map[string]any{"sha": "h8", "ref": "feat-8"}, "base": map[string]any{"ref": "main"}},
				}},
				{Method: "GET", Path: "/repos/" + repo + "/actions/runs", Body: map[string]any{"workflow_runs": []any{
					map[string]any{"id": 91, "name": "ci", "status": "in_progress", "created_at": stale},
					map[string]any{"id": 92, "name": "lint", "status": "in_progress", "created_at": time.Now().UTC().Format(time.RFC3339)},
					map[string]any{"id": 93, "name": "old-done", "status": "completed", "created_at": stale},
				}}},
			},
			Want: []Want{{Kind: "stuck_checks", Trigger: "st", Repo: repo, Number: 7, CatchUp: true,
				Context: map[string]any{"run_id": 91, "run_name": "ci", "run_status": "in_progress"}}},
		},
		{
			Name:       "a nudge runs the sweep again",
			Connection: withSweep,
			Triggers:   []Trigger{{On: "review_requested", Name: "rr"}},
			Routes: []Route{{Method: "GET", Path: "/repos/" + repo + "/pulls", Body: []any{
				map[string]any{"number": 8, "title": "PR 8", "user": map[string]any{"login": "someone"},
					"head": map[string]any{"sha": "h8", "ref": "feat-8"}, "base": map[string]any{"ref": "main"},
					"requested_reviewers": []any{map[string]any{"login": "me"}}},
			}}},
			Steps: []Step{{Sweep: true}},
			Want: []Want{
				{Kind: "review_requested", Trigger: "rr", Repo: repo, Number: 8, CatchUp: true},
				{Kind: "review_requested", Trigger: "rr", Repo: repo, Number: 8, CatchUp: true},
			},
		},
	}
}
