package ghplugin

import (
	"encoding/json"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// What the ENGINE does with each github event, declared in the contract's
// generic terms (docs/design/plugin-contract.md §4.1). Conductor implements
// each semantic once for every plugin; nothing in it knows these names.

// prTarget is the semantics every PR-shaped event shares: the PR as the
// target, its head as the revision, the author and labels facts, and what
// is never shown to an agent.
func prTarget() plugin.EventSemantics {
	return plugin.EventSemantics{
		Target: &plugin.TargetSemantics{
			Key: "{{.repo}}#{{.number}}", URL: "url", Label: "pull request",
			Assigned: json.RawMessage(`true`),
			Scope:    []plugin.ScopeFact{{Dimension: "repo", Fact: "repo"}},
		},
		Revision: &plugin.RevisionSemantics{Fact: "head", Branch: "head_ref", Base: "base"},
		// A PR's code is its pull ref; work is pushed back to its head
		// branch. paseo's PR-aware workspace reads the hints.
		Checkout: &plugin.CheckoutSemantics{
			Remote: "git@github.com:{{.repo}}.git", FetchRef: "refs/pull/{{.number}}/head", PushBranch: "head_ref",
			RuntimeHints: map[string]string{"forge": "github", "pr_number": "{{.number}}"},
		},
		Author:  &plugin.AuthorSemantics{Login: "author", Automated: "author_is_bot"},
		Labels:  "labels",
		Private: []string{"installation_id", "reaction_subjects"},
		Secret:  []string{"app_token", "gh_token"},
	}
}

func level(rearm bool) *plugin.Completion {
	l := &plugin.LevelCompletion{}
	if rearm {
		l.RearmOn = "revision"
	}
	return &plugin.Completion{Level: l}
}

// EventSemantics is the declared semantics of the github event name — the
// same declaration whether the bundled connector or the plugin emits it.
func EventSemantics(name string) *plugin.EventSemantics { return eventSemantics(name) }

func eventSemantics(name string) *plugin.EventSemantics {
	s := prTarget()
	switch name {
	case "review_requested":
		s.Priority = "interactive"
		s.Completion = level(true)
	case "changes_requested":
		s.BoundToTarget, s.Feedback = true, true
		s.Completion = level(false)
		// Its own cursor namespace: the review it folds is dispatched for
		// different comments than new_comment's, and a shared mark would let
		// one starve the other.
		s.Cursor = &plugin.CursorSemantics{ID: "comment_id", Stream: "changes_requested:{{.comment_kind}}"}
	case "new_comment":
		s.BoundToTarget, s.Feedback = true, true
		s.Attempts = &plugin.AttemptsSemantics{Cap: "none"}
		s.Cursor = &plugin.CursorSemantics{ID: "comment_id", Stream: "{{.comment_kind}}"}
	case "merge_conflict":
		s.BoundToTarget = true
		s.Completion = level(false)
	case "pr_behind":
		s.BoundToTarget = true
	case "failing_checks":
		s.BoundToTarget = true
		s.VerificationFailed = true
		s.Remediate = &plugin.RemediateSemantics{
			Option: "flaky_rerun", Run: "run_id", Budget: 1,
			Status: plugin.RemediateCheck{Verb: "get_run", DoneWhen: "status == 'completed'",
				Args: map[string]string{"repo": "{{.repo}}", "run_id": "{{.run_id}}"}},
			Action: plugin.RemediateVerb{Verb: "rerun_run",
				Args: map[string]string{"repo": "{{.repo}}", "run_id": "{{.run_id}}", "failed_only": "true"}},
		}
	case ClosedEvent:
		s.ClosesTarget = &plugin.ClosesTargetSemantics{
			Outcome: &plugin.OutcomeMap{Fact: "merged", True: "accepted", False: "rejected"},
			Reverts: &plugin.RevertsFact{Fact: "reverts", Corroborated: "reverts_corroborated"},
		}
	case "merge_ready", "self_review", "stuck_checks":
		// the PR target, nothing more
	case "issue_matched":
		s.Target.Label = "issue"
		s.Revision = nil
		s.Checkout = repoCheckout()
	default:
		// release, deployment_status, alerts: repo-level events whose target
		// the source assigns, with no revision of their own.
		s.Target.Key = "{{.repo}}"
		s.Target.Label = "repository"
		s.Revision = nil
		s.Checkout = repoCheckout()
	}
	return &s
}

// ClosedEvent is the event a PR's close emits: terminal for its target.
const ClosedEvent = "_closed"

// connSemantics are the connection-level declarations: the credentials an
// agent dispatched on a github event receives. GH_TOKEN is YOUR token, so
// every write the agent makes is attributed to you; the App's token is
// offered for rate-limited reads only.
func connSemantics() *plugin.ConnSemantics {
	repo := map[string]string{"repo": "{{.repo}}"}
	return &plugin.ConnSemantics{
		Credentials: []plugin.Credential{
			{Name: "read", Role: "read", Mint: plugin.CredentialMint{Verb: "read_token", Args: repo},
				Env: []string{"PC_GH_APP_TOKEN"}, Template: "app_token", Refresh: "resume"},
			{Name: "write", Role: "write", Mint: plugin.CredentialMint{Verb: "write_token", Args: repo},
				Env: []string{"GH_TOKEN", "GITHUB_TOKEN", "PC_GH_WRITE_TOKEN"}, Template: "gh_token", Refresh: "resume",
				Guidance: identityGuidance},
		},
		Scope: &plugin.ConnScope{Dimension: "repo", Option: "repos", Consent: true},
		Poll:  &plugin.PollSemantics{VerbName: "sweep"},
		Translate: &plugin.TranslateSemantics{Env: map[string]string{
			"event_path": "GITHUB_EVENT_PATH", "event_name": "GITHUB_EVENT_NAME"}},
	}
}

// identityGuidance tells an agent its GitHub identity IS the operator.
const identityGuidance = "\n\n---\n" +
	"IDENTITY: you act as ME. GH_TOKEN/GITHUB_TOKEN are MY token, so every comment, " +
	"review, reply, and `gh`/API write is attributed to me — and commits and `git push` " +
	"go over SSH as me. NEVER post, submit, approve, or otherwise write anything with the " +
	"App/bot token. If a large read would burn my rate limit you MAY read (only) with the " +
	"App token via `GH_TOKEN=$PC_GH_APP_TOKEN gh ...`, but never write with it."

// repoCheckout is a checkout of the repository itself (branch off its base),
// for an event about the repo rather than a PR.
func repoCheckout() *plugin.CheckoutSemantics {
	return &plugin.CheckoutSemantics{Remote: "git@github.com:{{.repo}}.git"}
}
