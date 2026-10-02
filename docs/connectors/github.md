# `github` connector

GitHub as a connector: the full **verb** surface (comments, reviews, PRs, issues,
files, workflow runs, releases, gists, labels, reactions, commit statuses) and the
full **event source** — webhook deliveries plus the adaptive catch-up **sweep** —
of the github connector conductor bundles.

It is not a re-implementation. The plugin serves conductor's own public
`pkg/githubkit/ghplugin` handler: the same github declaration the bundled
connector is built from, the same verb client (`pkg/githubkit`), and the same
event source (`pkg/githubkit/ghsource`) — review folding with dispatch-once
claims, the own-status guard, closed-PR drops, merge-state triggers, `me`
identity, the unified `filter:`, the sweep. It runs out of process over the
plugin SDK's **source extension** (connector ABI 1). A shared conformance suite
proves the two fire the same triggers ([parity](#parity-with-the-bundled-connector)).

- **Kind:** connector (verbs **and** source), connector ABI 1
- **Source:** [`connectors/github/main.go`](../../connectors/github/main.go)
- **Provides:** `github`
- **Capabilities:** egress to `api.github.com:443`, `*.ghe.com:443`, `smee.io:443`

```yaml
connectors:
  gh:
    use: NodeSpy/conductor-plugins/connectors/github   # while `github` is still bundled, name the plugin explicitly
    trusted_source: true          # see "Trust" — required for the builtin's behavior
    token: ${GITHUB_TOKEN}
    me: { logins: [your-login] }
```

## Using the plugin instead of the builtin

While conductor still bundles `github`, a bare `use: github` is the builtin. To
run the plugin, point `use:` at it (the official repo path above, or a local
`./conductor-github` build). Two rules:

- **One or the other.** A config cannot use the builtin and the plugin side by
  side — every `github` connector in it must be backed by the same one. Conductor
  refuses a mixed config at boot, naming the connectors. (With no builtin user,
  the plugin stands in for the bundled `github` type; nothing is redirected.)
- **`trusted_source: true`.** See [Trust](#trust). Without it the plugin runs,
  but conductor treats it as untrusted third-party input and drops the events
  its engine acts on.

Everything else — every connection key, event, filter key, option, verb — is
the builtin's, and means what it means there.

## Trust

Conductor does not let a plugin source assert facts its engine acts on, or
claim a target as the platform's, unless the operator vouches for it:

```yaml
    trusted_source: true
```

With it, this connector's events are treated as the bundled connector's are:
their targets get own-repo trust (an agent may address the repo the event
names without a grant), and the engine-interpreted kinds — `new_comment`,
`review_requested`, `merge_conflict`, `failing_checks`, and the `_closed`
lifecycle event — are accepted (only for events the plugin declares). Without
it, those kinds are dropped (logged once, naming the setting) and every target
is untrusted. `trusted_source` is refused on a builtin connector and is never
passed to the plugin.

Grant it to a plugin build you trust to verify GitHub's deliveries — this one
checks every delivery's `X-Hub-Signature-256` and reads everything else with
your connector's own credentials.

## Setup

Two ways to authenticate. A **personal access token** is the quickest and is
enough for every verb; a **GitHub App** additionally carries the webhook
subscription (the source events), a separate API rate pool, and a bot identity
for attributed writes. Reads resolve credentials in order: `app:` → `token:` →
the `gh` CLI's stored login.

### Option A — personal access token (quickest)

1. GitHub → **Settings → Developer settings → Personal access tokens**. A
   **fine-grained** token scoped to the repos you act on, with **Contents**,
   **Pull requests**, and **Issues** set to read/write (add **Actions** for the
   workflow verbs). A classic token with the `repo` scope also works.
2. Put it in `token:` (or omit `token:` and the connector falls back to
   `gh auth token`).

Events then arrive through a plain repository webhook pointed at
`webhook.listen` (with the same secret in the repo webhook and `webhook.secret`),
or through the sweep alone (the default when no webhook is configured).

### Option B — GitHub App (full-featured)

**1. Register the App** at `https://github.com/settings/apps/new` (personal) or
`https://github.com/organizations/<org>/settings/apps/new` (org).

| Field | Value |
| --- | --- |
| GitHub App name | Globally unique — e.g. `conductor-<your-handle>`. Becomes the bot login. |
| Homepage URL | Anything valid. |
| Webhook URL | A smee.io channel URL, or your own listener's public address (see [Webhook transport](#webhook-transport)). |
| Webhook secret | A random string — `openssl rand -hex 32`. Store it as `GH_WEBHOOK_SECRET`. |

**2. Set permissions** (before events — GitHub only lists events for granted
permissions): Contents read & write, Pull requests read & write, Issues read &
write, Checks read, Actions read (write for `rerun_run`/`dispatch_workflow`),
Commit statuses read & write (for `set_status`), Metadata read.

**3. Subscribe to webhook events:**

```
pull_request   pull_request_review   pull_request_review_comment   pull_request_review_thread
issue_comment   issues   check_run   check_suite   workflow_run   status
release   deployment_status   dependabot_alert   secret_scanning_alert   projects_v2_item
```

**4. Generate a private key**, save the `.pem`, **install the App** on your
repos/orgs, and note the **App ID**.

**5. Configure:**

```yaml
connectors:
  gh:
    use: NodeSpy/conductor-plugins/connectors/github
    trusted_source: true
    app:
      app_id: 123456
      private_key_path: ~/.config/conductor/github-app.pem
    webhook:
      smee_url: ${GH_SMEE_URL}        # https://smee.io/<channel> — or a direct listener:
      # listen: "127.0.0.1:8787"
      # path: /webhook
      secret: ${GH_WEBHOOK_SECRET}    # verifies each delivery's HMAC
    identity:
      write_token: ${GH_WRITE_TOKEN}  # writes act as you with this; "gh_auth" (default) = `gh auth token`
    me: { logins: [your-login] }      # optional: discovered from the write identity when unset
```

### Webhook transport

Set `webhook.smee_url`, `webhook.listen`, or both. Every delivery's
`X-Hub-Signature-256` is verified against `webhook.secret`; verification is on
unless `webhook.verify_signature: false`, and a webhook with verification on and
no secret is refused at start. With no webhook at all, the sweep is the event
source and polls at a fixed `sweep.min_interval`.

## Connection

The bundled connector's connection block, key for key:

| key | type | purpose |
|-----|------|---------|
| `app` | map | GitHub App credentials: `app_id`, `private_key_path` (the retired `app.webhook_secret` / `app.verify_signature` are refused — they live under `webhook:`) |
| `token` | string | PAT for reads when no App is configured (chain: app → token → `gh auth token`) |
| `webhook` | map | `smee_url` and/or `listen` (+ `path`, default `/webhook`), `secret`, `verify_signature` (default true) |
| `sweep` | map | catch-up sweep: `enabled` (default true), `interval` (adaptive ceiling, default 1h), `min_interval` (floor / fixed poll, default 2m), `repos` (default: the connector's `repos:`, else every installed repo) |
| `me` | map | your login(s): `{ logins: [...] }` — defines "you" (your PRs, your reviews); discovered from the write identity when unset |
| `repos` | list | default repo globs for triggers whose filter names no repo |
| `identity` | map | `read_token` (`app` default), `write_token` (`gh_auth` default, or a literal), `commit_author` (`self`) — also the credentials conductor hands the agents this connector's events dispatch |
| `retry` | map | transient dispatch retry: `max`, `backoff` |
| `project_map` | map | repo → paseo project checkout remap |
| `project_rewrite` | map | blanket `org:` rewrite for checkouts |
| `api_base` | string | GitHub API base URL (GitHub Enterprise Server). A plugin's environment is scrubbed, so it cannot inherit `PC_GITHUB_API_BASE` — set this. |

## Source events

Trigger with `on: <name>.<event>`. Every event publishes the base context
(`repo`, `owner`, `name`, `pr`, `issue`, `number`, `head`, `base`, `url`,
`kind`, `title`, `labels`, `me`), and events whose fixer pushes to the PR branch
(`changes_requested`, `new_comment`, `failing_checks`, `merge_conflict`,
`pr_behind`) carry `head_ref` and are never emitted for a closed or merged PR.
Autopilot events (`new_comment`, `changes_requested`, `failing_checks`,
`merge_conflict`, `pr_behind`, `merge_ready`, `self_review`, `stuck_checks`)
fire for PRs **you** authored; `review_requested` for reviews requested of you
(or the trigger's `reviewer`), `issue_matched` for issues assigned to you (or the
trigger's `assignee`).

| event | fires when | extra context | options |
|-------|-----------|---------------|---------|
| `review_requested` | your review was requested (webhook, a draft becoming ready, or the sweep finding it pending) | — | `reviewer` |
| `changes_requested` | a review requested changes, or left inline comments without approving — ONE event per review; or (sweep) unresolved threads from a reviewer who hasn't since approved | `head_ref`, `author`, `author_is_bot`, `review_id`, `review_body`, `review_state`, `review_comments`, `review_comments_omitted`, `comment_id`, `comment_kind`, `reaction_subjects` | — |
| `new_comment` | a standalone comment, or ONE submitted review no `changes_requested` trigger takes (always so for an approval) with all its inline comments; or (sweep) a comment the webhook missed | `author`, `author_is_bot`, `comment_body`, `head_ref`, `comment_id`, `comment_kind`, `review_*`, `reaction_subjects` | — |
| `merge_conflict` | your PR became unmergeable | — | — |
| `pr_behind` | your PR fell behind its base | — | — |
| `failing_checks` | CI concluded failing | `failing_check`, `run_id` | `flaky_rerun`, `ignore_checks` |
| `stuck_checks` | a run has been in progress too long (own poller) | `run_id`, `run_name`, `run_status` | `stuck_after` (30m), `poll_interval` (15m) |
| `merge_ready` | your PR turned all-green (clean, approved by someone else, threads resolved, not draft) | — | — |
| `self_review` | you opened/updated your own PR | — | — |
| `issue_matched` | an issue matches (assigned to you by default) — on issue changes and Projects moves | — | `assignee` |
| `release` | a release was published (prereleases skipped by default) | `tag_name`, `prerelease`, `draft` | `include_prereleases` |
| `deployment_status` | a deployment failed or errored | `state`, `environment`, `description` | — |
| `dependabot_alert` | a new Dependabot alert | `severity`, `package`, `summary` | — |
| `secret_scanning_alert` | a new secret-scanning alert | `secret_type` | — |

Every event also takes `max_attempts_per_head`. Closing a PR emits conductor's
`_closed` lifecycle event (merged or not, with revert facts) — no trigger is on
it; the engine consumes it.

**Review folding.** A submitted review reaches GitHub's webhook as one
`pull_request_review` plus one `pull_request_review_comment` per inline comment,
in no fixed order. Whichever delivery arrives first emits the review's ONE
event and claims it; the rest emit nothing. Its `comment_id` is the review's
highest inline comment id, so conductor's comment high-water mark dispatches it
once durably — a redelivery, a later sweep still seeing its threads, or an edited
review all fall at or below the mark.

### Filtering

A trigger's whole predicate is its `filter:` — the unified grammar: a condition
string (`expr`), a map of match keys (AND), a list (OR), `not_` on any key. Each
event declares its **facts** (readable in an expr) and **match keys**:

| event | facts | match keys |
|---|---|---|
| `review_requested` | `head_branch`, `base_branch`, `title`, `labels`, `is_draft`, `author` | `branch`, `base_branch`, `title`, `label_any`, `label_all`, `require_label`, `author`, `draft` |
| `changes_requested` | `head_branch`, `base_branch`, `title`, `labels`, `author`, `reviewer`, `author_is_bot` | `branch`, `base_branch`, `title`, `label_any`, `label_all`, `require_label`, `author`, `author_bot` |
| `new_comment` | `comment_author`, `comment_body`, `author_is_bot` | `comment_author`, `author_bot` |
| `issue_matched` | `title`, `labels`, `author`, `sole_assignee` | `title`, `label_any`, `label_all`, `require_label`, `author`, `sole_assignee` |
| `merge_ready` | `labels`, `author`, `is_draft`, `merge_state`, `review_decision`, `non_author_approval`, `threads_resolved` | `label_any`, `label_all`, `require_label`, `author`, `draft`, `merge_state`, `review_decision`, `non_author_approval`, `threads_resolved` |
| every event | — | `repo` (routing: scopes the trigger, its sweep, its stuck poller) |

A filter that is only `repo` / `not_repo` keeps the event's intrinsic default
(merge_ready's gates stay enforced); anything else replaces it.

```yaml
triggers:
  - on: gh.new_comment
    filter: { repo: [acme/api], not_comment_author: ["ci[bot]"] }
    steps: [ … ]
  - on: gh.merge_ready
    filter: { repo: [acme/api], threads_resolved: false }   # waive one gate
    steps: [ … ]
```

### The sweep

The sweep runs inside the plugin with the connector's credentials: on start,
then on an adaptive cadence (`min_interval` → `interval`) when a webhook carries
real time, or a fixed `min_interval` when it is the only source. It recovers
pending review requests, merge conflicts / behind PRs, unresolved review threads
(`changes_requested`), and comments a dropped webhook missed (`new_comment`,
within 24h) — each marked catch-up, so conductor skips a PR an agent is already
working. `SIGUSR1`, `conductor sweep --now`, and the `sweep` verb nudge it
(conductor calls the plugin's `plugin.nudge`); a smee reconnect resets the
cadence. `stuck_checks` has its own poller.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- **`repo`** — `owner/name`; required on every repo-scoped verb (all but the
  gist verbs).
- **`as`** — `me` (default) or `bot`; picks the identity on write verbs when an
  App is configured. Omitted below for brevity — every verb that mutates accepts it.
- **`number` vs `pr`** — issue/PR verbs take `number`; PR-only verbs take `pr`.
  `comment`/`assign` accept either (`number`, alias `pr`).
- **Pagination** — list verbs return the first page; pass `all: true` for every
  page, or `per_page` (max 100) to size it.
- **`sweep`** — conductor-defined: the DAEMON answers it (a daemon-wide catch-up
  sweep, as `conductor sweep --now`), it is never sent to the plugin. → `nudged`.

Required options are marked `*`.

### Conversation & review

- **`comment`** — post an issue/PR conversation comment. `repo`*, `number`* (alias `pr`), `body`*. → `id`, `url`.
- **`reply`** — reply to a PR review-comment thread. `repo`*, `pr`*, `in_reply_to`* (the review-comment id), `body`*. → `id`, `url`.
- **`submit_review`** — submit a PR review: a summary + verdict, with optional inline comments. `repo`*, `pr`*, `event`* (`APPROVE` | `REQUEST_CHANGES` | `COMMENT`), `body` (the summary), `comments` (a list of `{path, line, body, side?, start_line?, start_side?}`; `line` is the file line, `side` defaults to `RIGHT`; every commented line **must** fall inside the PR diff or GitHub rejects the whole review). → `id`, `comments` (count posted).
- **`request_review`** — request review from users/teams (also re-requests someone who already reviewed). `repo`*, `pr`*, `reviewers` (user logins), `team_reviewers` (team slugs). → `ok`.
- **`rerequest_review`** — `request_review` for the re-review-after-a-fix flow, guarded. Same options, plus `only_outstanding` (default `true`): only reviewers whose latest review is `CHANGES_REQUESTED` on an older commit than the PR head, who aren't already requested, on an open PR, are pinged — never one who has since approved. State is read fresh; if it can't be read nobody is pinged. `only_outstanding: false` re-requests unconditionally. Review bots and the PR author are always dropped. → `ok`, and `skipped` (the reason) when nobody was left to ping.
- **`remove_reviewer`** — cancel a pending review request. `repo`*, `pr`*, `reviewers`, `team_reviewers`. → `ok`.
- **`review_comments`** — existing inline review comments on the PR: `[{path, line, body, user, id}]` (100/page). `repo`*, `pr`*, `all`. → `comments`.
- **`react`** — add a reaction to comments or reviews, or with `remove: true` take yours of that content away (only yours, never anyone else's). `repo`*, `content`* (`+1` `-1` `laugh` `confused` `heart` `hooray` `rocket` `eyes`), and the subjects: `subjects` (`[{kind, id}]`, the shape of an event's `reaction_subjects`) or the `kind` + `id` shorthand. `kind` is `issue_comment`, `review_comment`, or `review`; a `review` subject also needs `pr` (a review is reachable only over GraphQL: `addReaction` / `removeReaction`). Idempotent both ways: a repeat isn't duplicated, and removing one that isn't there is a no-op. → `ok`, `reacted` / `removed`.
- **`set_status`** — post a commit status; it shows on any PR whose head that commit is. `repo`*, `sha` **or** `pr` (the PR's head as it is at call time, read fresh; `sha` wins when both are set), `state`* (`pending` | `success` | `failure` | `error`), `context` (the row's name, entirely yours, templates included; only when unset does it default to the login the call acts as), `description` (clipped to GitHub's 140 characters), `target_url`. Last write to a (commit, context) wins, as on GitHub. → `ok`, `context`, `sha`.

Progress on a PR is ordinary hooks calling these two verbs: 👀 / 🚀 / 👍 / 😕
and a status row. See conductor's
[Configuration](https://github.com/NodeSpy/conductor/wiki/Configuration#showing-progress-on-the-pr-github)
and the pr-autopilot pack. The bundled connector also publishes `{{.me.login}}`
and the `{{.run.*}}` head facts. This plugin doesn't (it has no head face), so a
flow on it passes `pr:` and names its own context.

### Pull requests

- **`create_pr`** — open a pull request. `repo`*, `title`*, `head`* (branch with your changes; `owner:branch` for a fork), `base`* (branch to merge into), `body`, `draft`. → `number`, `url`.
- **`merge_pr`** — merge a PR. `repo`*, `pr`*, `method` (`merge` | `squash` | `rebase`, default `merge`), `commit_title`, `commit_message`, `sha` (require the head to match, as a safety check). → `merged`, `sha`.
- **`update_pr`** — edit a PR. `repo`*, `pr`*, `state` (`open` | `closed` → reopen/close), `title`, `body`, `base` (retarget). → `number`, `state`.
- **`pr_get`** — PR metadata. `repo`*, `pr`*. → `title`, `body`, `state`, `draft`, `author`, `base`, `head`, `head_sha`, `additions`, `deletions`, `changed_files`, `labels`, `url`.
- **`pr_diff`** — the PR's unified diff (cached; GitHub caps the `.diff` type around 300 files). `repo`*, `pr`*. → `diff`.
- **`pr_files`** — changed files: `[{path, status, additions, deletions, changes}]` (100/page). `repo`*, `pr`*, `all`. → `files`.
- **`checks`** — check-run status for a ref: `[{name, status, conclusion, url}]`. `repo`*, `ref`* (branch/tag/sha). → `checks`.
- **`ready_for_review`** — mark a draft PR ready. `repo`*, `pr`*. → `ok`.
- **`convert_to_draft`** — convert a PR back to a draft. `repo`*, `pr`*. → `ok`.

### Issues

- **`create_issue`** — open an issue. `repo`*, `title`*, `body`, `labels`, `assignees` (logins). → `number`, `url`.
- **`update_issue`** — edit an issue. `repo`*, `number`*, `state` (`open` | `closed`), `state_reason` (`completed` | `not_planned` | `reopened`), `title`, `body`. → `number`, `state`.
- **`get_issue`** — read an issue. `repo`*, `number`*. → `title`, `body`, `state`, `labels`, `assignees`, `author`, `url`.
- **`list_issues`** — list issues (PRs excluded): `[{number, title, state, labels, author, url}]`. `repo`*, `state` (`open` | `closed` | `all`, default open), `labels` (must have all), `assignee` (a login, or `*` / `none`), `per_page`, `all`. → `issues`.
- **`search_issues`** — search issues/PRs in this repo: `[{number, title, state, is_pr, url}]`. `repo`*, `q`* (GitHub search query, scoped to the repo automatically), `per_page`, `all`. → `total`, `items`.
- **`assign`** — add and/or remove assignees. `repo`*, `number`* (alias `pr`), `add` (logins), `remove` (logins). → `assignees`.
- **`add_labels`** — add labels. `repo`*, `number`*, `labels`*. → `ok`.
- **`remove_label`** — remove one label. `repo`*, `number`*, `label`*. → `ok`.

### Repository contents

- **`file`** — a file's raw contents at a ref (cached; GitHub's raw type caps ~1 MiB). `repo`*, `path`*, `ref` (branch/tag/sha, default: the default branch), `optional` (return empty text instead of erroring on a 404 — for optional convention files). → `text`.
- **`put_file`** — create or update a file in one commit. `repo`*, `path`*, `content`* (UTF-8; base64-encoded for the API automatically), `message`*, `branch` (default: the default branch), `sha` (the blob sha being replaced — **required to update** an existing file; get it from `file`/`get_ref`). → `commit`, `sha` (the new blob sha).
- **`delete_file`** — delete a file in one commit. `repo`*, `path`*, `message`*, `sha`* (blob sha to delete), `branch`. → `commit`.
- **`get_ref`** — the commit sha a branch/tag/ref points at. `repo`*, `ref`*. → `sha`.
- **`create_branch`** — create a branch from another ref. `repo`*, `branch`* (new name), `from` (source branch/tag/sha, default: the default branch HEAD). → `sha`.

### Workflows & runs

- **`dispatch_workflow`** — trigger a `workflow_dispatch` run. `repo`*, `workflow`* (file name like `ci.yml`, or numeric id), `ref`* (branch/tag to run on), `inputs` (map of `workflow_dispatch` inputs). → `ok`.
- **`list_runs`** — recent workflow runs: `[{id, name, status, conclusion, head_branch, head_sha, url}]`. `repo`*, `branch`, `status` (`queued` | `in_progress` | `completed` | `success` | `failure` | …), `per_page` (default 20, max 100), `all`. → `runs`.
- **`rerun_run`** — re-run a workflow run. `repo`*, `run_id`*, `failed_only` (re-run only failed jobs). → `ok`.
- **`cancel_run`** — cancel a workflow run. `repo`*, `run_id`*. → `ok`.

### Releases

- **`create_release`** — publish a release for a tag. `repo`*, `tag`* (created on `target` if it doesn't exist), `target` (commitish for a new tag, default: default branch), `name` (title), `body` (notes), `draft`, `prerelease`. → `id`, `url`, `upload_url`.
- **`upload_asset`** — attach a file to a release. `repo`*, `release_id`* (from `create_release`), `name`* (asset file name), `content` (inline bytes) **or** `path` (local file), `content_type` (default `application/octet-stream`). → `id`, `url`.

### Gists  *(user-scoped, no `repo`)*
### Gists  *(user-scoped, no `repo`)*

- **`create_gist`** — `files`* (`{filename: content}`), `description`, `public` (default false = secret). → `id`, `url`.
- **`get_gist`** — `id`*. → `files` (`{filename: content}`), `description`, `public`, `url`.
- **`update_gist`** — `id`*, `files` (`{filename: content}`; a null/empty content deletes that file), `description`. → `id`, `url`.
- **`delete_gist`** — `id`*. → `ok`.
- **`list_gists`** — `user` (whose public gists; default: your own, incl. secret), `per_page`, `all`. → `gists`.

## Capabilities & security

Declares egress to `api.github.com:443`, `*.ghe.com:443`, and `smee.io:443`,
and spawns nothing it names (the `gh auth token` fallback shells out to `gh`).
An installed build is confined to that list; narrow it per instance with
`network:`. A GitHub Enterprise Server on its own domain is outside the
declaration — see [gaps](#known-gaps). Write attribution is
`identity.write_token` and the per-call `as: me|bot`.

## Parity with the bundled connector

The bundled connector and this plugin run one implementation, and conductor's
conformance suite (`pkg/githubkit/ghsource/ghsourcetest`) proves they agree. The
same table runs against the bundled connector, against this plugin through
conductor's real plugin path (spawn, describe, `start_source` with triggers,
routed events, `trusted_source`), and — in this repository's `e2e/` — against
this repository's build over the bare wire. Conductor's hermetic e2e suite also
runs with every github connector on the plugin (`make e2e-plugin`).

| surface | builtin | plugin | proven by |
|---|---|---|---|
| `review_requested` (webhook, ready-for-review, sweep) | ✓ | ✓ | conformance "review_requested for your review", "the sweep recovers …" |
| `changes_requested`: one event per review, either delivery order | ✓ | ✓ | conformance "… are ONE changes_requested", "… arriving FIRST still fold into one" |
| `new_comment`: own PRs only, never your own comments | ✓ | ✓ | conformance "new_comment on your PR, not on others or by you" |
| approval with suggestions → one `new_comment` | ✓ | ✓ | conformance "an approval with suggestions is ONE new_comment" |
| closed-PR drop; `_closed` on close | ✓ | ✓ | conformance "a closed PR's feedback is dropped; closing emits _closed" |
| `failing_checks` + `ignore_checks`, `run_id` from details_url | ✓ | ✓ | conformance "failing_checks on your PR, minus ignore_checks" |
| `merge_conflict` / `pr_behind` from merge state, own PRs only | ✓ | ✓ | conformance "merge_conflict and pr_behind …" |
| `merge_ready` gate | ✓ | ✓ | conformance "merge_ready when all-green, held by a failing gate" |
| `self_review` | ✓ | ✓ | conformance "self_review when you open a PR" |
| `issue_matched` + assignee default + filter | ✓ | ✓ | conformance "issue_matched: assigned to you, and the trigger's filter" |
| `release` + `include_prereleases` | ✓ | ✓ | conformance "release, with prereleases only where asked" |
| `deployment_status`, `dependabot_alert`, `secret_scanning_alert` | ✓ | ✓ | conformance "deployment failures and security alerts" |
| `stuck_checks` (own poller, stale runs, own PRs) | ✓ | ✓ | conformance "stuck_checks from the poller, on your PR only" |
| repo routing between triggers on one event | ✓ | ✓ | conformance "triggers on one event route by their repo filters" |
| unified `filter:` predicates | ✓ | ✓ | conformance "a unified filter predicate drops a commenter" |
| sweep: pending reviews, conflicts, threads, missed comments, catch-up mark | ✓ | ✓ | conformance "the sweep recovers …" |
| sweep nudge (`SIGUSR1`, `sweep --now`, `sweep` verb) | ✓ | ✓ | conformance "a nudge runs the sweep again"; conductor `TestSweepVerbIsAnsweredByTheDaemon` |
| own-status guard (`set_status` contexts are conductor's own) | ✓ | ✓ | conductor `TestSetStatusMakesTheContextOwn` |
| writes as `identity.write_token` | ✓ | ✓ | conductor `TestSetStatusMakesTheContextOwn`; e2e H4/I1 in plugin mode |
| `conductor force` | ✓ | ✓ | conductor `TestABISourceExtensionCalls`; e2e (force-driven groups) in plugin mode |
| App token re-mint on resume | ✓ | ✓ | conductor `TestABISourceExtensionCalls` |
| run-fact head reads (`{{.run.start_sha}}`) | ✓ | ✓ | conductor `TestABITargetHeadOnlyForOwnTrustedTargets`; e2e H4 in plugin mode |
| dispatch credential policy (`identity`, `retry`) | ✓ | ✓ | conductor `TestIdentitySourceReadsTheConnection`; e2e I1 in plugin mode |
| declaration (connection, events, facts, match keys, verbs) | ✓ | ✓ | `e2e/TestGithubDeclIsTheBundledDecl` (byte-identical) |
| every verb | ✓ | ✓ | same `pkg/githubkit` client; `e2e/github_test.go` |

### Known gaps

Not papered over — these differ from the builtin today:

- **Nested secret references.** Conductor resolves secret references only in
  the connection's top-level string fields for a plugin; a `vault:`-style
  reference nested in `webhook.secret` or `identity.write_token` is passed
  through unresolved (`${ENV}` expansion, which happens at config load, is
  unaffected). The builtin resolves `token` and `webhook.secret`.
- **`conductor validate`** does not run the connector's own config checks for a
  plugin (missing webhook secret, unreadable App key, a sweep glob without an
  App); they run when the source starts and are logged.
- **GitHub Enterprise Server on its own domain** is outside the declared egress,
  so an *installed* build cannot reach it (a local build is unconfined). The
  builtin reaches any `api_base`.
- **Own-status contexts** noted by `set_status` live as long as the plugin
  process; the builtin keeps them for the daemon's lifetime. (Status deliveries
  never fire a trigger either way, so this affects only the guard's bookkeeping.)
- **A bad webhook signature** is answered `202` and dropped (logged), exactly as
  the builtin does; the previous plugin answered `401`.
