# `linear` connector

Linear as a connector: issue/comment/archive verbs over the Linear GraphQL API,
plus **source events** derived from Linear webhook deliveries (issue, comment,
project). Built ONLY against the public SDK — no other dependency.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/linear/main.go`](../../connectors/linear/main.go)
- **Provides:** `linear`
- **Capabilities:** egress to `api.linear.app:443` only
- **Bundled in conductor?** No. This plugin is the only way to get a linear
  connector.

```yaml
connectors:
  lin:
    use: linear
    api_key: ${LINEAR_API_KEY}
    webhook:
      listen: ":9100"
      secret: ${LINEAR_WEBHOOK_SECRET}
    network: ["api.linear.app:443"]   # narrow the declared egress
triggers:
  - on: lin.issue
    filters: { actions: [create], teams: [ENG] }
    steps:
      - uses: lin.comment
        options: { issue_id: "{{.id}}", body: "Thanks for filing this!" }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_key` | string | Linear personal API key or app token — sent **raw** (no `Bearer ` prefix) in `Authorization` |
| `api_base` | string | override the GraphQL endpoint (default `https://api.linear.app/graphql`; used for tests) |
| `webhook` | map | source transport: `listen`, `path` (default `/linear`), `secret`, `allow_unsigned` |

## Source events

Trigger with `on: <name>.<event>`. Events are derived from a single Linear
webhook delivery `{action, type, data}` and HMAC-SHA256 verified
(`Linear-Signature`, a raw hex digest — verified with `crypto/hmac` +
`crypto/subtle`, constant-time). Deliveries are deduplicated on `data.id` +
`action`.

| event | fires when |
|-------|-----------|
| `issue` | a Linear issue was created, updated, or removed |
| `comment` | a Linear comment was created, updated, or removed |
| `project` | a Linear project was created, updated, or removed |

**Context**

- `issue`: `id`, `identifier`, `title`, `state`, `priority`, `assignee`, `team`, `url`, `action`
- `comment`: `id`, `body`, `issue_id`, `user`, `action`
- `project`: `id`, `name`, `state`, `url`, `action`

**Filters** (evaluated by the daemon's generic list-contains evaluator):
`actions`, `types`, `states`, `teams`, `priorities` (list forms), and `action`,
`type`, `state`, `team`, `priority` (scalar forms).

> **Unsigned listeners fail closed.** With no `webhook.secret`, the listener
> would accept any POST on the listen address as a real event. Set it, or set
> `webhook.allow_unsigned: true` to opt in explicitly when something else
> authenticates the endpoint.

## Verbs

Selected by `uses: <name>.<verb>`. Every verb builds a GraphQL query/mutation
string and POSTs it to `https://api.linear.app/graphql` (or `api_base`); a
non-2xx response, or a 2xx response carrying a GraphQL `errors` array, is
reported as an internal error with the response body attached.

| verb | GraphQL operation | notes |
|------|--------------------|-------|
| `create_issue` | `issueCreate` | `title`, `team_id` required; `description`, `priority`, `assignee_id`, `state_id`, `labels` optional. Outputs `issue_id`, `identifier`, `url`. |
| `update_issue` | `issueUpdate` | `id` required; `title`, `description`, `priority`, `state_id`, `assignee_id` optional. Outputs `issue_id`, `identifier`, `url`. |
| `comment` | `commentCreate` | `issue_id`, `body` required. Outputs `id`, `url`. |
| `get_issue` | `issue` query | `id` required. Outputs `result` (the issue object). |
| `search_issues` | `issueSearch` or `issues` | `query` (full text) or `filter` (a Linear `IssueFilter`), mutually exclusive. Outputs `items`. |
| `archive_issue` | `issueArchive` | `id` required. Outputs `ok`. |
| `graphql` | any | escape hatch: `query` (required) + `variables`. Outputs `result`. |

## Capabilities & security

Declares egress to `api.linear.app:443` only. Narrow it per instance with
`network:`; it can never be widened past the declaration. Use the
least-privileged API key available for the workspace you act on.
