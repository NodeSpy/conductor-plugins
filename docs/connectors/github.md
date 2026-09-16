# `github` connector

GitHub as a connector: a broad **verb** surface (comments, reviews, PRs, issues,
files, workflow runs, releases, gists, labels) over token or GitHub-App auth,
plus **source events** derived from a single webhook delivery. Built on
`pkg/githubkit`.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/github/main.go`](../../connectors/github/main.go)
- **Provides:** `github`
- **Capabilities:** egress to `api.github.com:443` and `*.ghe.com:443`
- **Bundled in conductor?** Yes — this plugin is the additive, sandboxed
  counterpart of the still-bundled github connector. Use it when you want github
  verbs running in a subprocess with a narrowed credential.

```yaml
connectors:
  gh:
    use: github
    token: ${GITHUB_TOKEN}
    network: ["api.github.com:443"]   # narrow the declared egress
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `app` | map | GitHub App credentials: `app_id`, `private_key_path`, `webhook_secret` |
| `token` | string | PAT used when no App is configured (chain: app → token → `gh auth token`) |
| `identity` | map | credential policy: `write_token` |
| `webhook` | map | source transport: `listen`, `path`, `secret` |
| `api_base` | string | override the API base URL (GitHub Enterprise Server, or tests) |

The `as` option on write verbs selects the identity (`me` vs `bot`) when an App
is configured.

## Source events

Trigger with `on: <name>.<event>`. Events are derived from a single webhook
delivery and HMAC-verified (`X-Hub-Signature-256`).

| event | fires when |
|-------|-----------|
| `review_requested` | your review was requested on a PR |
| `changes_requested` | a review requested changes on your PR |
| `new_comment` | a new comment on a PR |
| `release` | a release was published |
| `deployment_status` | a deployment failed or errored |
| `dependabot_alert` | a new Dependabot alert |
| `secret_scanning_alert` | a new secret-scanning alert |

Filter context includes fields like `author`, `author_is_bot`, `head_ref`,
`comment_body`, `tag_name`, `prerelease`, `state`, `environment`, `severity`,
depending on the event. See `Describe()` in the source for the exact per-event
context schema.

```yaml
triggers:
  - on: gh.changes_requested
    filters: { author_is_bot: false }
    steps:
      - uses: gh.pr_diff
        options: { repo: acme/app, pr: "{{.number}}" }
```

## Verbs

Selected by `uses: <name>.<verb>`. Grouped by area; see `Describe()` for each
verb's full option schema.

**Conversation & review**
`comment`, `reply`, `submit_review`, `request_review`, `rerequest_review`
(alias), `remove_reviewer`, `review_comments`.

**Pull requests**
`create_pr`, `merge_pr`, `update_pr`, `pr_get`, `pr_diff`, `pr_files`, `checks`,
`ready_for_review`, `convert_to_draft`.

**Issues**
`create_issue`, `update_issue`, `get_issue`, `list_issues`, `search_issues`,
`assign`, `add_labels`, `remove_label`.

**Repository contents**
`file` (raw contents at a ref), `put_file`, `delete_file`, `get_ref`,
`create_branch`.

**Workflows & runs**
`dispatch_workflow`, `list_runs`, `rerun_run`, `cancel_run`.

**Releases**
`create_release`, `upload_asset`.

**Gists**
`create_gist`, `get_gist`, `update_gist`, `delete_gist`, `list_gists`.

> `sweep` is a daemon-global operation and is **not** available from an external
> plugin instance.

## Capabilities & security

Declares egress to `api.github.com:443` and `*.ghe.com:443` only. Narrow it per
instance with `network:`; it can never be widened past the declaration. Provide
the least-privileged token or a scoped App installation for the repos you
actually act on.
