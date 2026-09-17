# `gitea` connector

Gitea (and Forgejo — the two share the same v1 API surface) as a connector: a
self-hosted git forge over its REST v1 API — issues, pull requests, releases,
branches, and file contents — plus a raw `api` escape hatch, over a personal
access token. As a **source**, it receives Gitea webhook deliveries (push,
pull_request, issues, issue_comment) and streams a normalized event per
delivery.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/gitea/main.go`](../../connectors/gitea/main.go)
- **Provides:** `gitea`
- **Capabilities:** empty egress manifest — the host is self-hosted and
  operator-specific, so it cannot be declared in advance; narrow `network:` to
  your own instance yourself.

```yaml
connectors:
  ge:
    use: gitea
    url: https://gitea.example.com
    token: ${GITEA_TOKEN}
    network: ["gitea.example.com:443"]   # required: this plugin declares no egress
```

## Setup

You'll end up with an access token for your self-hosted Gitea/Forgejo
instance that conductor uses to call the v1 REST API.

**Prerequisites:** an account on the instance with write access to the
repo(s) you want to act on.

1. Sign in, click your avatar (top right) → **Settings**.
2. Left sidebar → **Applications**.
3. Under "Manage Access Tokens", enter a token name and select scopes — pick
   `write:repository` (or `read:repository` for read-only) plus `write:issue`
   if you need issue/PR comments; leave everything else at no access.
4. Click **Generate Token** and copy it immediately — it's shown only once.

```yaml
connectors:
  ge:
    use: gitea
    url: https://gitea.example.com
    token: ${GITEA_TOKEN}
    network: ["gitea.example.com:443"]
```

For the webhook source, add it under the repo's **Settings → Webhooks → Add
Webhook → Gitea**, set the target URL, and paste the same value into the
webhook's **Secret** as `webhook.secret` below — see `## Source events` for
the HMAC verification model and `smee` fallback when there's no public
listener URL.

## Connection

| key | type | purpose |
|-----|------|---------|
| `url` | string | **required.** base URL of the Gitea/Forgejo instance, e.g. `https://gitea.example.com`; the API base is `url` + `/api/v1` |
| `token` | string | access token, sent as `Authorization: token <token>` |
| `webhook` | map | source transport: `listen` (optional if `smee` is set), `path` (default `/gitea`), `secret`, `allow_unsigned`, `smee` (SSE relay URL, e.g. `https://smee.io/AbC123` — HMAC verification still applies to relayed bodies) |

## Source events

Trigger with `on: <name>.<event>`. Events are derived from a single webhook
delivery, keyed by `X-Gitea-Event`.

| event | fires when |
|-------|-----------|
| `push` | commits were pushed to a branch |
| `pull_request` | a pull request was opened, closed, or otherwise changed |
| `issues` | an issue was opened, closed, or otherwise changed |
| `issue_comment` | a comment was added to an issue or pull request |

Common context: `repo` (owner/name), `action`, `ref`, `branch`, `sender`,
`url`. `pull_request` adds `pr_number`, `title`, `state`, `base`, `head`,
`merged`. `issues` adds `issue_number`, `title`, `state`. `issue_comment` adds
`issue_number`, `comment_body`.

Filters accept both list and scalar forms: `actions`/`action`,
`states`/`state`, `branches`/`branch`.

```yaml
triggers:
  - on: ge.issue_comment
    filters: { actions: [created] }
    steps:
      - uses: ge.comment_issue
        options: { repo: acme/app, index: "{{.issue_number}}", body: "thanks!" }
```

### Webhook security

Gitea signs deliveries with HMAC-SHA256 over the raw request body, hex-encoded,
in `X-Gitea-Signature` — the exact bare-hex shape `sourcekit.VerifyHMAC`
already accepts, so this source verifies it through the shared
`sourcekit.Listener` rather than hand-rolling crypto here. No `webhook.secret`
configured **and** no `webhook.allow_unsigned: true` refuses to start the
listener — a missing secret is far more often a mistake than a choice. Set
`webhook.secret` to the same secret configured on the Gitea webhook, or set
`webhook.allow_unsigned: true` if you genuinely front the listener with
something else that authenticates requests.

## Verbs

Selected by `uses: <name>.<verb>`. Every verb takes `repo` (`owner/name`)
except `api`. Outputs are uniformly `result` (the parsed JSON response) +
`status_code`, except `list_issues`, which returns `items`. A non-2xx Gitea
response is returned as an invocation error carrying the HTTP status and
response body.

**Issues & pull-request conversation**
`comment_issue(repo, index, body)` — posts to `/issues/{index}/comments`,
which works for pull requests too (Gitea shares the numbering).

**Issues**
`create_issue(repo, title, body, labels, assignees)`,
`update_issue(repo, index, state, title, body)`, `get_issue(repo, index)`,
`list_issues(repo, state, type, page, limit)` → `items`,
`add_labels(repo, index, labels)`.

**Pull requests**
`create_pr(repo, head, base, title, body)`,
`merge_pr(repo, index, do)` (merge strategy, default `merge`),
`get_pr(repo, index)`.

**Releases & branches**
`create_release(repo, tag_name, name, body, draft, prerelease)`,
`create_branch(repo, new_branch_name, old_branch_name)`.

**Repository contents**
`put_file(repo, path, content, message, branch)` — `content` may be raw text
or already-base64; it is base64-encoded automatically if not already.

**Escape hatch**
`api(method, path, query, body)` — any Gitea v1 API call, `path` relative to
`/api/v1`.

## Capabilities & security

Declares an empty egress manifest: the Gitea/Forgejo host is self-hosted and
operator-specific, so there is no fixed hostname to declare the way
`api.github.com` is fixed for the `github` connector. You **must** set
`network:` on the connector instance to your own instance's host — otherwise
the plugin has no declared egress at all. Provide the least-privileged access
token for the repos you actually act on.
