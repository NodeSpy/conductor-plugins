# `github` connector

GitHub as a connector: a broad **verb** surface (comments, reviews, PRs, issues,
files, workflow runs, releases, gists, labels) over token or GitHub-App auth,
plus **source events** derived from a single webhook delivery. Built on
`pkg/githubkit`.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/github/main.go`](../../connectors/github/main.go)
- **Provides:** `github`
- **Capabilities:** egress to `api.github.com:443` and `*.ghe.com:443`

```yaml
connectors:
  gh:
    use: github
    token: ${GITHUB_TOKEN}
    network: ["api.github.com:443"]   # narrow the declared egress
```

## Setup

Two ways to authenticate. A **personal access token** is the quickest and is
enough for every verb; a **GitHub App** additionally carries the webhook
subscription (the source events), a separate API rate pool, and a bot identity
for attributed writes. Credentials resolve in order: `app:` → `token:` → the
`gh` CLI's stored login.

### Option A — personal access token (quickest)

1. GitHub → **Settings → Developer settings → Personal access tokens**. A
   **fine-grained** token scoped to the repos you act on, with **Contents**,
   **Pull requests**, and **Issues** set to read/write (add **Actions** for the
   workflow verbs). A classic token with the `repo` scope also works.
2. Put it in `token:` (or omit `token:` and the connector falls back to
   `gh auth token`).

```yaml
connectors:
  gh:
    use: github
    token: ${GITHUB_TOKEN}
```

That's it for verbs. Events (`on: gh.*`) need a webhook — the App path below (or
a plain repo webhook pointed at `webhook.listen`).

### Option B — GitHub App (full-featured)

An App is the full path: it owns the webhook subscription, reads on its own rate
pool, and lets writes be attributed to a bot identity.

**1. Register the App** at `https://github.com/settings/apps/new` (personal) or
`https://github.com/organizations/<org>/settings/apps/new` (org).

| Field | Value |
| --- | --- |
| GitHub App name | Globally unique — e.g. `conductor-<your-handle>`. Becomes the bot login. |
| Homepage URL | Anything valid (your repo, or `https://paseo.sh`). |
| Webhook URL | A smee.io channel URL, or your own listener's public address (see [Webhook transport](#webhook-transport)). |
| Webhook secret | A random string — `openssl rand -hex 32`. Store it as `GH_WEBHOOK_SECRET`. |

**2. Set permissions** (do this *before* events — GitHub only lists events for
granted permissions):

| Scope | Permission | Access |
| --- | --- | --- |
| Repository | Contents | Read & write |
| Repository | Pull requests | Read & write |
| Repository | Issues | Read & write |
| Repository | Checks | Read-only |
| Repository | Metadata | Read-only |

**3. Subscribe to webhook events** — the ones the source events below derive from:

```
pull_request   pull_request_review   pull_request_review_comment   issue_comment
check_run   check_suite   workflow_run   push   issues   release   deployment_status
```

**4. Generate a private key** (App settings → "Generate a private key"), save the
`.pem`, then **install the App** on your repos/orgs and note the **App ID** (top
of the App settings page).

**5. Configure:**

```yaml
connectors:
  gh:
    use: github
    app:
      app_id: 123456                                        # numeric App id
      private_key_path: ~/.config/conductor/github-app.pem  # the generated .pem
      webhook_secret: ${GH_WEBHOOK_SECRET}                  # verifies each delivery's HMAC
    webhook:
      smee: ${GH_SMEE_URL}          # https://smee.io/<channel> — or a direct listener:
      # listen: ":9099"
      # path: /webhook
    identity:
      write_token: ${GH_WRITE_TOKEN}   # a PAT writes are attributed to; "gh_auth" shells out to `gh auth token`
```

The **installation id** is implicit — conductor resolves it from the App's
installations at startup; the App only needs to be installed on the target
repos/orgs.

### Webhook transport

The source events need deliveries to reach conductor. Set `webhook.smee`,
`webhook.listen`, or both. Either way the delivery's `X-Hub-Signature-256` HMAC
is checked against the webhook secret; set `webhook.allow_unsigned: true` only if
something else already authenticates the listener.

- **smee.io (no inbound port).** Open <https://smee.io/new>, and use the **same**
  channel URL as the App's Webhook URL and as `webhook.smee`. Conductor
  subscribes to the channel itself (auto-reconnecting) — you don't run the `smee`
  client. smee.io doesn't buffer, so a delivery sent while conductor is offline is
  lost.
- **Direct HTTP.** `webhook.listen: ":9099"` (with optional `webhook.path`) runs a
  plain receiver; point the App's Webhook URL at it, typically via your own
  tunnel.

### Running without an App

An App isn't required. App-less: events arrive via a **plain repository webhook**
pointed at `webhook.listen` (set the same secret in the repo webhook and in
`webhook.secret`, no `app:` block); reads use the PAT / `gh` token; writes are
you. Verb calls that need App credentials (e.g. the bot identity) fail with a
clear error without them.

## Connection

| key | type | purpose |
|-----|------|---------|
| `app` | map | GitHub App credentials: `app_id`, `private_key_path`, `webhook_secret` |
| `token` | string | PAT used when no App is configured (chain: app → token → `gh auth token`) |
| `identity` | map | credential policy: `write_token` (the credential writes are attributed to; `"gh_auth"` shells out to `gh auth token`) |
| `webhook` | map | source transport: `listen` (optional if `smee` is set), `path`, `secret` (or `app.webhook_secret`), `smee` (SSE relay URL, e.g. `https://smee.io/AbC123`), `allow_unsigned` |
| `api_base` | string | override the API base URL (GitHub Enterprise Server, or tests) |

## Source events

Trigger with `on: <name>.<event>`. Events are derived from a single webhook
delivery and HMAC-verified (`X-Hub-Signature-256`). Each event publishes a set of
**context fields** — the facts a trigger's `filter:` matches on and that
templates (`{{.field}}`) read. `repo` and `number`/`pr` are always available.

| event | fires when | context fields (filter / template) |
|-------|-----------|-------------------------------------|
| `review_requested` | your review was requested on a PR | — |
| `changes_requested` | a review requested changes on your PR | `head_ref`, `author`, `author_is_bot` |
| `new_comment` | a new comment on a PR | `author`, `author_is_bot`, `comment_body`, `head_ref`, `comment_id`, `comment_kind` |
| `release` | a release was published | `tag_name`, `prerelease`, `draft` |
| `deployment_status` | a deployment failed or errored | `state`, `environment`, `description` |
| `dependabot_alert` | a new Dependabot alert | `severity`, `package`, `summary` |
| `secret_scanning_alert` | a new secret-scanning alert | `secret_type` |

`author_is_bot` is true when the actor's account type is `Bot` or its login ends
in `[bot]` — the usual way to skip automated actors.

### Filtering

A trigger's `filter:` matches an event's published fields (the table above),
plus `repo` and `number`/`pr` which every event carries. The grammar:

- A key set to a value must match; a **list matches any of** its values —
  `repo: [acme/api, acme/infra]`.
- Prefix **`not_`** to negate a field — `not_author_is_bot: true` skips bots,
  `not_repo: [acme/sandbox]` excludes.
- **`expr:` / `not_expr:`** take an expression over the fields —
  `not_expr: "startswith(comment_body, '/skip')"`.
- Keys within one filter object are **AND**ed. A top-level **array** of filter
  objects is **OR** across them (one arm per rule).

### Examples

Scope a trigger to specific repos and re-request review from the author when they
ask for changes:

```yaml
triggers:
  - on: gh.changes_requested
    filter:
      repo: [acme/api, acme/infra]          # any of these repos
    steps:
      - uses: gh.rerequest_review
        options: { repo: "{{.repo}}", pr: "{{.pr}}", reviewers: ["{{.author}}"] }
```

Act on new PR comments, but ignore automated ones:

```yaml
triggers:
  - on: gh.new_comment
    filter:
      not_author_is_bot: true               # skip github-actions[bot] & friends
    steps:
      - uses: gh.comment
        options: { repo: "{{.repo}}", pr: "{{.pr}}", body: "on it" }
```

Different rules per org, as an OR of arms (each arm ANDs its own keys):

```yaml
triggers:
  - on: gh.changes_requested
    filter:
      - repo: [globex/web]                  # globex: any change request
      - repo: [acme/api, acme/infra]        # acme: only from humans
        not_author_is_bot: true
    steps:
      - uses: gh.pr_diff
        options: { repo: "{{.repo}}", pr: "{{.pr}}" }
```

> `sweep` (the catch-up reconciliation source) is a daemon-global operation and
> is **not** available from an external plugin instance.

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

Required options are marked `*`.

### Conversation & review

- **`comment`** — post an issue/PR conversation comment. `repo`*, `number`* (alias `pr`), `body`*. → `id`, `url`.
- **`reply`** — reply to a PR review-comment thread. `repo`*, `pr`*, `in_reply_to`* (the review-comment id), `body`*. → `id`, `url`.
- **`submit_review`** — submit a PR review: a summary + verdict, with optional inline comments. `repo`*, `pr`*, `event`* (`APPROVE` | `REQUEST_CHANGES` | `COMMENT`), `body` (the summary), `comments` (a list of `{path, line, body, side?, start_line?, start_side?}`; `line` is the file line, `side` defaults to `RIGHT`; every commented line **must** fall inside the PR diff or GitHub rejects the whole review). → `id`, `comments` (count posted).
- **`request_review`** — request review from users/teams (also re-requests someone who already reviewed). `repo`*, `pr`*, `reviewers` (user logins), `team_reviewers` (team slugs). → `ok`.
- **`rerequest_review`** — alias of `request_review` (GitHub has one endpoint); kept for the re-review-on-new-changes flow. Same options. → `ok`.
- **`remove_reviewer`** — cancel a pending review request. `repo`*, `pr`*, `reviewers`, `team_reviewers`. → `ok`.
- **`review_comments`** — existing inline review comments on the PR: `[{path, line, body, user, id}]` (100/page). `repo`*, `pr`*, `all`. → `comments`.

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

- **`create_gist`** — `files`* (`{filename: content}`), `description`, `public` (default false = secret). → `id`, `url`.
- **`get_gist`** — `id`*. → `files` (`{filename: content}`), `description`, `public`, `url`.
- **`update_gist`** — `id`*, `files` (`{filename: content}`; a null/empty content deletes that file), `description`. → `id`, `url`.
- **`delete_gist`** — `id`*. → `ok`.
- **`list_gists`** — `user` (whose public gists; default: your own, incl. secret), `per_page`, `all`. → `gists`.

## Capabilities & security

Declares egress to `api.github.com:443` and `*.ghe.com:443` only, and spawns no
commands. Narrow it per instance with `network:`; it can never be widened past
the declaration. Provide the least-privileged token or a scoped App installation
for the repos you actually act on. Write attribution is controlled by
`identity.write_token` and the per-call `as: me|bot`.
