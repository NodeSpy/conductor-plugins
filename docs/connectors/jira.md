# `jira` connector

Jira Cloud as a connector: issues, comments, transitions, and JQL search as
**verbs** over REST API v3, plus **source events** derived from a single
webhook delivery. Built ONLY against the public SDK (`pkg/plugin`) and the
connector-kit (`pkg/sourcekit`) — no third-party client, no conductor
internals.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/jira/main.go`](../../connectors/jira/main.go)
- **Provides:** `jira`
- **Capabilities:** egress to `*.atlassian.net:443`
- **Self-hosted Jira:** not supported by the v3 REST verbs this connector
  calls (Jira Server/Data Center uses a different REST surface); point
  `base_url` at a Jira Cloud site.

```yaml
connectors:
  jira:
    use: jira
    base_url: https://acme.atlassian.net
    email: bot@acme.com
    api_token: ${JIRA_API_TOKEN}
    webhook:
      listen: ":9097"
      secret: ${JIRA_WEBHOOK_SECRET}
```

## Setup

You'll end up with an Atlassian API token conductor uses alongside your
account email for HTTP Basic auth against the Jira Cloud REST API.

**Prerequisites:** an Atlassian account with access to the Jira Cloud site
(and permission to act on the projects you target — a bot/service account
is recommended over a personal one).

1. Sign in at [id.atlassian.com](https://id.atlassian.com) with the account
   that should own the token.
2. Left sidebar → **Security** → **Create and manage API tokens**.
3. Click **Create API token**, give it a label, and (optionally) set an
   expiry.
4. Copy the token immediately — Atlassian shows it only once.

```yaml
connectors:
  jira:
    use: jira
    base_url: https://acme.atlassian.net
    email: bot@acme.com
    api_token: ${JIRA_API_TOKEN}
```

For the webhook source, register the webhook under Jira's **Settings →
System → WebHooks** (site admin), pointing at your listener URL with the
shared secret templated into the URL's `secret` query parameter (or sent via
`X-Conductor-Token`) — see `## Source events` above for the exact
verification model, and `webhook.smee` when there's no public listener URL.

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | **required.** Jira Cloud site base URL, e.g. `https://acme.atlassian.net` |
| `email` | string | Atlassian account email for HTTP Basic auth |
| `api_token` | string | Atlassian API token for HTTP Basic auth |
| `webhook` | map | source transport: `listen`, `path` (default `/jira`), `secret`, `allow_unsigned`, `smee` (smee.io-style SSE relay URL — receive forwarded deliveries when the endpoint has no public URL; the shared secret is still checked) |

Verbs call `base_url + "/rest/api/3"` with HTTP Basic auth (`email:api_token`,
base64-encoded). A non-2xx response is surfaced as an internal error carrying
the HTTP status and response body.

## Source events

Trigger with `on: <name>.<event>`. Jira Cloud webhooks carry **no signature
header** — unlike GitHub's HMAC or PagerDuty's rotating-key HMAC, there is
nothing to verify cryptographically. Instead, this connector authenticates
deliveries against a **shared secret**, checked constant-time against either:

- the `secret` query parameter (the value Jira lets you template into the
  webhook URL itself), or
- an `X-Conductor-Token` header (for a delivery path that sets a custom
  header instead, e.g. a forwarding proxy).

Omitting `webhook.secret` **fails closed**: the connector refuses to start
its listener unless `webhook.allow_unsigned: true` is also set, which is the
explicit, greppable way to say you accept unauthenticated deliveries (e.g.
because a trusted network path or a separately-authenticating front door
covers it instead).

| event | fires when |
|-------|-----------|
| `issue_created` | a Jira issue was created (`jira:issue_created`) |
| `issue_updated` | a Jira issue was updated (`jira:issue_updated`) |
| `comment_created` | a comment was added to an issue (`comment_created`) |

Deliveries are de-duplicated on `key + webhookEvent + timestamp`, so a
redelivered webhook doesn't emit twice.

Filter context includes `project`/`projects`, `status`/`statuses`,
`issuetype`/`issuetypes`, and `priority`/`priorities` (scalar and plural-list
aliases, so filters can match either form). `issue_created`/`issue_updated`
context also carries `key`, `summary`, `assignee`, `reporter`, `url`, and
`action`. `comment_created` context additionally carries `comment_body` and
`author`.

```yaml
triggers:
  - on: jira.issue_created
    filters: { projects: ["PROJ"], priorities: ["High", "Highest"] }
    steps:
      - uses: jira.add_comment
        options: { key: "{{.key}}", body: "Triaging now." }
```

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb returns `result` (the raw decoded JSON response, or
`nil` on an empty body) and `status_code`; several also extract commonly-used
fields.

| verb | purpose |
|------|---------|
| `create_issue` | create an issue (`project`, `summary`, `issuetype`, optional `description`/`labels`/`assignee_id`/`priority`/`fields` override) → `key`, `id`, `url` |
| `add_comment` | add a comment to an issue → `id` |
| `transition` | move an issue through a workflow transition, by `transition_id` or a looked-up `transition_name` |
| `list_transitions` | list the transitions available on an issue → `transitions` |
| `get_issue` | read an issue, optionally scoped to `fields` |
| `update_issue` | edit an issue's fields (`summary`/`description` shortcuts, or a raw `fields` map) |
| `assign` | assign an issue to an Atlassian `account_id` |
| `add_labels` | add labels to an issue |
| `search` | JQL search (`jql`, `max_results`, `fields`) → `issues` |
| `api` | escape hatch: any `method` + `path` (relative to `/rest/api/3`), with `query`/`body` |

`description` and comment `body` are plain text; the connector wraps them
into a minimal Atlassian Document Format (ADF) document — a single paragraph,
one text node — since Jira's v3 API requires ADF for rich-text fields. Pass a
full ADF document yourself via the `fields` override on `create_issue` /
`update_issue` if you need richer formatting.

## Capabilities & security

Declares egress to `*.atlassian.net:443` only. Narrow it per instance with
`network:`; it can never be widened past the declaration. Provide the
least-privileged API token for the projects you actually act on, and always
set `webhook.secret` in production — `allow_unsigned` is meant for cases where
something else in front of the listener already authenticates the request.
