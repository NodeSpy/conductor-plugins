# `authentik` connector

authentik (self-hosted identity provider / SSO) as a connector: users
(list/get/create/update/delete), groups, applications, providers, flows,
events (audit log), and tokens over the authentik REST API (v3), plus a raw
`api` escape hatch. Built on the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/authentik/main.go`](../../connectors/authentik/main.go)
- **Provides:** `authentik`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own instance)

```yaml
connectors:
  idp:
    use: authentik
    base_url: https://authentik.example.com
    api_token: ${AUTHENTIK_API_TOKEN}
    network: ["authentik.example.com:443"]   # narrow the declared egress to your instance
```

## Setup

Mint an API token for a dedicated service account so this connector can call
the authentik REST API without borrowing a personal admin login.

**Prerequisites:** admin access to the authentik instance.

1. Log in to the authentik admin interface (`https://authentik.example.com/if/admin/`).
2. Recommended: create a dedicated service account first —
   **Directory → Users → Create Service Account** — rather than issuing a
   token against your own admin user.
3. Go to **Directory → Tokens and App passwords → Create**.
4. Set **Identifier** (e.g. `conductor-automation`), **User** to the service
   account, **Intent** to `API Token`, and turn **Expiring** off (an expired
   token auto-rotates and the old value stops matching what you configured
   here).
5. Save, then copy the generated token — it's shown once.

**Configure:**

```yaml
connectors:
  idp:
    use: authentik
    base_url: https://authentik.example.com
    api_token: ${AUTHENTIK_API_TOKEN}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | authentik instance root, e.g. `https://authentik.example.com` (no trailing `/api`) |
| `api_token` | string | authentik API token, sent as `Authorization: Bearer <api_token>` |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) |

Every verb calls `base_url + "/api/v3" + <endpoint>`, authenticated with the
Bearer token (create one under Directory > Tokens, or use a service-account
token). A non-2xx response is returned as an error carrying the status code
and response body — nothing is swallowed.

### `insecure_skip_verify`: read this before enabling

Self-hosted authentik instances sometimes run behind a self-signed
certificate on a LAN, so `insecure_skip_verify: true` exists as an explicit,
greppable opt-out of TLS certificate verification. Enabling it also disables
**all** protection against a man-in-the-middle on the path to the identity
provider — anyone who can intercept the connection can read the API token
and forge responses, which for an IdP means forging identity data. Only
enable it for instances reached over a trusted network (e.g. the same LAN,
or a VPN), and prefer installing a real certificate (e.g. via a reverse
proxy with ACME/Let's Encrypt) instead when possible.

## No source: no stable event-stream endpoint

There is no first-class, generically-consumable webhook or event-stream
endpoint in authentik that this connector can commit to parsing. This
connector is verb-only: it declares no `Events` and does not implement
`StartSource`. The `events` verb polls the audit log instead.

## Pagination

authentik's list endpoints respond with `{"pagination": {...}, "results": [...]}`.
Every list verb hoists `results` into `items`, while also returning the full
decoded body as `result` so pagination metadata (`count`, `next`,
`previous`, etc.) survives. Single-object verbs (`user_get`, `user_create`,
`user_update`) return `result` only.

authentik's DRF routers require a **trailing slash** on collection and
single-resource paths (e.g. `/core/users/`, `/core/users/42/`) — every
first-class verb's endpoint already carries it; the `api` escape hatch does
not add one for you.

## Verbs

Selected by `uses: <name>.<verb>`. Every verb's outputs include
`status_code`. Required options are marked `*`. `user_id` (on the `user_*`
verbs) is a path parameter, URL-escaped before use.

### Users

- **`users`** — list users. `search` (free-text search), `is_active`
  (boolean, filter by active state), `ordering` (field to order by, e.g.
  `username` or `-username`). → `result`, `items` (hoisted from
  `result.results`).
- **`user_get`** — get one user's details. `user_id`* (scoped option). →
  `result`.
- **`user_create`** — create a user. `username`* — plus these fields, passed
  through verbatim (preserving whatever JSON type the caller supplied: bool,
  list, map, string) so authentik itself does the validation: `name`
  (display name), `email`, `is_active` (boolean), `groups` (list, group
  PKs/UUIDs this user belongs to), `path` (authentik user path, default
  `"users"`), `type` (user type, e.g. `internal`, `external`,
  `service_account`), `attributes` (map, arbitrary user attributes). →
  `result`. `username` is the only field authentik requires to create a
  user.
- **`user_update`** — partially update a user (PATCH). `user_id`* (scoped
  option), `username`, plus the same optional fields as `user_create`
  (`name`, `email`, `is_active`, `groups`, `path`, `type`, `attributes`). →
  `result`.
- **`user_delete`** — delete a user. `user_id`* (scoped option). →
  `status_code` (+ `result` if the response carries a body).

### Groups, applications, providers & flows

- **`groups`** — list groups. No options. → `result`, `items` (hoisted from
  `result.results`).
- **`applications`** — list applications. No options. → `result`, `items`
  (hoisted from `result.results`).
- **`providers`** — list all providers (any type). No options. → `result`,
  `items` (hoisted from `result.results`).
- **`flows`** — list flow instances. No options. → `result`, `items`
  (hoisted from `result.results`).

### Events & tokens

- **`events`** — list events (audit log). `action` (filter by event action),
  `username` (filter by the acting user's username). → `result`, `items`
  (hoisted from `result.results`).
- **`tokens`** — list tokens. No options. → `result`, `items` (hoisted from
  `result.results`).

### Escape hatch

- **`api`** — raw escape hatch for any authentik API v3 endpoint without a
  first-class verb. `method` (HTTP method, default `GET`), `path`* (path
  under `/api/v3`, e.g. `/core/users/` — include the trailing slash
  authentik's routers require), `query` (map, query string parameters),
  `body` (any, JSON request body). → `result` (object response, with `items`
  hoisted from `result.results` when present) or `items` (bare array
  response).

## Capabilities & security

Declares empty egress — authentik is self-hosted, so there is no fixed
public host to declare. Set `network: ["<your-host>:443"]` on the connector
instance to narrow it to your own deployment. Provide an API token scoped to
the least privilege the workflow needs (authentik tokens inherit the
permissions of the user or service account they belong to — create a
dedicated service account for automation rather than reusing an admin's
token).
