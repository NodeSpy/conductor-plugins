# `gitlab` connector

GitLab as a connector: merge requests, issues, notes, and pipelines over
REST v4, plus a raw `api` escape hatch, over a personal/project access token.
As a **source**, it receives GitLab webhook deliveries (Push, Merge Request,
Pipeline, Issue, Note hooks) and streams a normalized event per delivery.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/gitlab/main.go`](../../connectors/gitlab/main.go)
- **Provides:** `gitlab`
- **Capabilities:** egress to `gitlab.com:443` (self-managed instances:
  override with your own host in `network:`)

```yaml
connectors:
  gl:
    use: gitlab
    url: https://gitlab.com   # or your self-managed instance
    token: ${GITLAB_TOKEN}
    network: ["gitlab.com:443"]   # narrow the declared egress to your instance
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `url` | string | GitLab instance base URL (default `https://gitlab.com`); the API base is `url` + `/api/v4` |
| `token` | string | personal/project access token, sent as `PRIVATE-TOKEN` |
| `webhook` | map | source transport: `listen`, `path` (default `/gitlab`), `secret`, `allow_unsigned` |

## Source events

Trigger with `on: <name>.<event>`. GitLab authenticates webhooks with a
**plain shared-secret header** (`X-Gitlab-Token`), not an HMAC signature — this
source compares it in constant time (`crypto/subtle`) rather than treating it
as an HMAC digest.

| event | fires on (`object_kind`) |
|-------|--------------------------|
| `push` | a push to a branch |
| `merge_request` | a merge request was opened/updated/merged/closed |
| `pipeline` | a pipeline changed status |
| `issue` | an issue was opened/updated/closed |
| `note` | a comment was posted on an MR, issue, commit, or snippet |

Common context: `project` (path_with_namespace), `user`, `url`, `action`,
`ref`/`branch`. `merge_request` adds `mr_iid`, `title`, `state`,
`source_branch`, `target_branch`. `pipeline` adds `status`, `pipeline_id`.
`issue` adds `issue_iid`, `title`, `state`.

Filters accept both list and scalar forms: `actions`/`action`,
`states`/`state`, `statuses`/`status`, `branches`/`branch`,
`target_branches`/`target_branch`.

```yaml
triggers:
  - on: gl.merge_request
    filters: { actions: [open, reopen] }
    steps:
      - uses: gl.comment_mr
        options: { project: acme/app, mr: "{{.mr_iid}}", body: "on it" }
```

### Webhook security

No secret configured **and** no `webhook.allow_unsigned: true` refuses to
start the listener — a missing secret is far more often a mistake than a
choice. Set `webhook.secret` to the same Secret Token configured on the
GitLab webhook, or set `allow_unsigned: true` if you genuinely front the
listener with something else that authenticates requests.

## Verbs

Selected by `uses: <name>.<verb>`. Every verb takes `project`
(`path_with_namespace` or numeric id, URL-encoded automatically) except `api`.
Outputs are uniformly `result` (the parsed JSON response) + `status_code`,
except `list_mrs`, which returns `items`. A non-2xx GitLab response is
returned as an invocation error carrying the HTTP status and response body.

**Notes**
`comment_mr(project, mr, body)`, `comment_issue(project, issue, body)`.

**Issues**
`create_issue(project, title, description, labels)`,
`update_issue(project, issue, state_event, title, description)`,
`get_issue(project, issue)`, `add_labels(project, issue, labels)`.

**Merge requests**
`create_mr(project, source_branch, target_branch, title, description)`,
`update_mr(project, mr, state_event, title, description, target_branch)`,
`merge_mr(project, mr, squash, merge_when_pipeline_succeeds)`,
`get_mr(project, mr)`,
`list_mrs(project, state, target_branch)` → `items`.

**Repository**
`create_branch(project, branch, ref)`.

**Pipelines**
`trigger_pipeline(project, ref)`, `retry_pipeline(project, pipeline)`,
`cancel_pipeline(project, pipeline)`.

**Escape hatch**
`api(method, path, query, body)` — any GitLab REST v4 call, `path` relative
to `/api/v4`.

## Capabilities & security

Declares egress to `gitlab.com:443` only. Self-managed instances must narrow
or replace this via `network:` to their own host — it can never be widened
past the declaration. Provide the least-privileged token (project access
token scoped to the projects you actually act on) rather than a
broadly-scoped personal token where possible.
