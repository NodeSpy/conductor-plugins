# `wiz` connector

Wiz ([wiz.io](https://www.wiz.io)) is a cloud-security platform. This connector
drives its tenant **GraphQL API** for issues, findings, and projects
(OAuth2 client-credentials auth), and — as a source — receives
operator-configured Wiz Integration webhooks for new/updated issues.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/wiz/main.go`](../../connectors/wiz/main.go)
- **Provides:** `wiz`
- **Capabilities:** egress to `auth.app.wiz.io:443` (the fixed OAuth2 token
  endpoint) and `api.*.app.wiz.io:443` (the tenant GraphQL API pattern — see
  below)
- **Bundled in conductor?** No. This plugin is the only way to get a wiz
  connector.

```yaml
connectors:
  wiz:
    use: wiz
    client_id: ${WIZ_CLIENT_ID}
    client_secret: ${WIZ_CLIENT_SECRET}
    api_url: https://api.us1.app.wiz.io/graphql
    webhook:
      listen: ":9097"
      secret: ${WIZ_WEBHOOK_TOKEN}
    network: ["auth.app.wiz.io:443", "api.us1.app.wiz.io:443"]

triggers:
  - on: wiz.issue
    filters: { severities: [CRITICAL, HIGH] }
    steps:
      - uses: wiz.issue_get
        options: { id: "{{.issue_id}}" }
```

## Setup

**Prerequisites:** a Wiz admin account (Global Admin or equivalent).

1. **Service account:** in Wiz, go to **Settings → Access Management →
   Service Accounts → Add Service Account**. Name it, set **Type** to
   **Custom Integration (GraphQL API)**, scope **Projects** as needed, and
   grant the API scopes your verbs need (e.g. `read:issues`,
   `read:vulnerabilities`, `update:issues`). Copy the **Client ID** and
   **Client Secret** immediately — the secret is shown once.
2. **Tenant endpoints:** under **User Settings → Tenant**, note your region's
   API endpoint (`https://api.<region>.app.wiz.io/graphql`) for `api_url`,
   and its token endpoint for `auth_url` if it differs from the default.
3. **Webhook (source):** add a Wiz Integration for issue create/update
   events pointed at `http://<host>:<port>/wiz`, with the JSON body template
   from **Source: `issue` event** below. Set a shared token and send it back
   as a header or `?token=` — see **Verifying webhook deliveries** below.

```yaml
connectors:
  wiz:
    use: wiz
    client_id: ${WIZ_CLIENT_ID}
    client_secret: ${WIZ_CLIENT_SECRET}
    api_url: https://api.us1.app.wiz.io/graphql
    webhook:
      listen: ":9097"
      secret: ${WIZ_WEBHOOK_TOKEN}
    network: ["auth.app.wiz.io:443", "api.us1.app.wiz.io:443"]
```

No public URL? Set `webhook.smee: https://smee.io/<channel>` instead of (or
alongside) `webhook.listen`.

## Connection

| key | type | purpose |
|-----|------|---------|
| `client_id` | string | **required.** OAuth2 client-credentials `client_id` |
| `client_secret` | string | **required.** OAuth2 client-credentials `client_secret` |
| `api_url` | string | **required.** tenant GraphQL endpoint, e.g. `https://api.us1.app.wiz.io/graphql` |
| `auth_url` | string | OAuth2 token endpoint (default `https://auth.app.wiz.io/oauth/token`) |
| `audience` | string | OAuth2 audience (default `wiz-api`) |
| `webhook` | map | source transport: `listen`, `path`, `secret`, `header`, `allow_unsigned`, `smee` (smee.io-style SSE relay URL — receive forwarded deliveries when the endpoint has no public URL; the shared token is still checked) |

### Tenant host varies — narrow `network:` yourself

Wiz's GraphQL API is served from a **per-tenant, per-region host**
(`api.us1.app.wiz.io`, `api.eu1.app.wiz.io`, `api.us2.app.wiz.io`, ...) — there
is no single fixed hostname. The declared `Egress` capability documents this as
the pattern `api.*.app.wiz.io:443`; it is **not** narrowed to your specific
tenant automatically. Set `network:` on the connector instance to your actual
`api_url` host so the runtime confinement is as tight as your deployment.

### OAuth2 client-credentials

On first use (and whenever the cached token has expired) the connector POSTs
form-encoded `grant_type=client_credentials`, `audience`, `client_id`, and
`client_secret` to `auth_url`, and caches the returned `access_token` in memory
against its `expires_in` (mutex-guarded, so concurrent invocations share one
fetch and never race the cache). Every GraphQL request then carries
`Authorization: Bearer <access_token>`.

## Verbs

| verb | purpose |
|------|---------|
| `issues` | list issues matching a `filterBy`-shaped `filter` map; paginated (`first`/`after`) |
| `issue_get` | fetch a single issue by `id` |
| `update_issue` | update an issue's `status`, `note`, or `resolution_reason` |
| `findings` (alias `vulnerabilities`) | list vulnerability findings matching `filter`; paginated |
| `projects` | list Wiz projects |
| `graphql` | escape hatch: run any `query`/`variables` against the tenant API |

`issues` and `findings` normalize the GraphQL `nodes`/`pageInfo` shape into
`items` / `page_info` outputs. `issue_get`, `update_issue`, and `graphql` return
a single `result`.

```yaml
- uses: wiz.issues
  options:
    filter: { status: [OPEN, IN_PROGRESS], severity: [CRITICAL, HIGH] }
    first: 50

- uses: wiz.update_issue
  options: { id: "{{.issue_id}}", status: RESOLVED, resolution_reason: "Remediated by automation" }
```

A non-2xx HTTP response **or** a GraphQL `errors` array is surfaced as a single
invocation error carrying both the error message(s) and the raw response body
— nothing about a failed call is silently dropped.

## Source: `issue` event

Wiz Integrations let an operator configure an outbound webhook — on new or
updated issues — with an **arbitrary JSON template** (Wiz does not mandate a
fixed payload shape the way GitHub or Stripe do). Configure your Wiz
Integration's webhook body template to this recommended shape:

```json
{
  "issue_id": "{{issue.id}}",
  "title": "{{issue.title}}",
  "severity": "{{issue.severity}}",
  "status": "{{issue.status}}",
  "entity": "{{issue.entitySnapshot.name}}",
  "project": "{{issue.projects[0].name}}",
  "url": "{{issue.url}}"
}
```

(The exact template syntax is whatever Wiz's Integration UI exposes at
configuration time — the keys above are what this connector reads.)

| event | fires when |
|-------|-----------|
| `issue` | a Wiz Integration webhook fired for a new or updated issue |

**Context:** `issue_id`, `title`, `severity`, `status`, `entity`, `project`,
`url`.

**Filters** (list-contains): `severities`, `statuses`, `projects` (empty =
any), plus scalar `severity`, `status`, `project`.

Deliveries are deduplicated on `issue_id` + `status`, so a redelivered webhook
for the same issue at the same status is not re-emitted; a status change on the
same issue (e.g. `OPEN` → `RESOLVED`) is treated as a new event.

### Verifying webhook deliveries

Wiz's outbound webhook is a **bare shared token**, not an HMAC-signed body
(unlike the sentry/pagerduty/github connectors' `Sig-Header` schemes). Set
`webhook.secret` to a token value, and configure your Wiz Integration to send
it back on every delivery — either as a request header (default
`X-Conductor-Token`, overridable via `webhook.header`) or as a `?token=` query
parameter. The connector compares it to `webhook.secret` in constant time
(`crypto/subtle`).

| key | type | purpose |
|-----|------|---------|
| `webhook.listen` | string | HTTP listener address, e.g. `:9097` |
| `webhook.path` | string | request path (default `/wiz`) |
| `webhook.secret` | string | shared token compared against the header/query value |
| `webhook.header` | string | header carrying the token (default `X-Conductor-Token`) |
| `webhook.allow_unsigned` | boolean | explicit opt-out of the fail-closed default |
| `webhook.smee` | string | optional smee.io-style SSE relay URL — receive forwarded deliveries when the listener has no public URL; the shared token is still checked |

> **Unverified listeners fail closed.** With no `webhook.secret`, the listener
> would accept any POST on the address as a real issue event. Set the secret,
> or set `webhook.allow_unsigned: true` to opt in explicitly when something
> else in front of the listener already authenticates the request.

## Capabilities & security

Declares egress to `auth.app.wiz.io:443` (the fixed token endpoint) and the
`api.*.app.wiz.io:443` pattern (your tenant's actual GraphQL host). Narrow
`network:` per instance to your tenant's real API host; it can never be
widened past the declaration. No `Commands`/`Spawns` — this connector never
shells out.
