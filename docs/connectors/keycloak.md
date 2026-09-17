# `keycloak` connector

Drive a self-hosted [Keycloak](https://www.keycloak.org/) instance's identity/SSO
admin surface over its Admin REST API: realms, users (list/get/create/update/
delete/reset-password/logout), groups, clients, realm roles, per-client
session stats, the event log, and a generic `api` escape hatch for any
endpoint a first-class verb does not cover.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/keycloak/main.go`](../../connectors/keycloak/main.go)
- **Provides:** `keycloak`
- **Capabilities:** no declared egress (self-hosted; the operator narrows
  `network:` to their own instance)

```yaml
connectors:
  kc:
    use: keycloak
    base_url: https://keycloak.example.com
    realm: corp                        # target realm for operations (default "master")
    client_id: ${KEYCLOAK_CLIENT_ID}
    client_secret: ${KEYCLOAK_CLIENT_SECRET}
    network: ["keycloak.example.com:443"]   # narrow the (empty) declared egress

steps:
  - uses: kc.user_create
    with:
      body: { username: "new.hire", email: "new.hire@corp.example.com", enabled: true }
  - uses: kc.user_reset_password
    with: { id: "{{ steps.user_create.id }}", value: "{{ secrets.temp_password }}", temporary: true }
```

## Setup

Register a confidential OAuth2 client with service-account roles so this
connector can authenticate to the Admin REST API on its own.

**Prerequisites:** admin access to the Keycloak instance (or at least the
realm you'll manage).

1. In the Keycloak admin console, pick the realm the client should live in
   (often the same realm you'll manage) → **Clients** → **Create client**.
2. Set a **Client ID** (e.g. `conductor-admin`), leave type `OpenID Connect`,
   click **Next**.
3. Under **Capability config**, turn on **Client authentication** and
   **Service accounts roles**; leave Standard/Direct access flows off unless
   you need them separately. Save.
4. Open the client's **Service accounts roles** tab → **Assign role** →
   filter by client **realm-management** → grant only what this connector
   instance needs (e.g. `manage-users`), not `realm-admin`.
5. Open the client's **Credentials** tab and copy the **Client secret**.
6. Note the realm the client authenticates against (`auth_realm`) versus the
   realm it operates on (`realm`) — identical in most setups.

**Configure:**

```yaml
connectors:
  kc:
    use: keycloak
    base_url: https://keycloak.example.com
    realm: corp
    auth_realm: corp
    client_id: ${KEYCLOAK_CLIENT_ID}
    client_secret: ${KEYCLOAK_CLIENT_SECRET}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | **required.** Keycloak server root, e.g. `https://keycloak.example.com` |
| `realm` | string | target realm for admin operations (default `master`) |
| `auth_realm` | string | realm whose token endpoint authenticates `client_id`/`client_secret` (default = `realm`) |
| `client_id` | string | **required.** OAuth2 `client_credentials` client id (a Keycloak client with service-account roles granting the admin operations you call) |
| `client_secret` | string | **required.** OAuth2 `client_credentials` client secret |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) — see below |

### Why this connector manages its own OAuth2 token, not conductor's managed OAuth2

Conductor's managed-OAuth2 connectors (`Decl.Auth`) bake a **fixed** provider
token endpoint into the plugin and let the daemon run the token lifecycle.
Keycloak's admin token endpoint is not fixed: it is
`{base_url}/realms/{auth_realm}/protocol/openid-connect/token`, which varies
by **both** the operator's own deployment (`base_url`) and the realm the
client authenticates against (`auth_realm`) — there is no single endpoint a
plugin binary could hard-code. So this connector fetches and caches its own
admin token via the OAuth2 `client_credentials` grant instead (the same
pattern the [`wiz`](./wiz.md) connector uses for its tenant-specific OAuth2
token, and the [`unifi`](./unifi.md) connector uses for its cookie-session
re-login):

- The token is cached in memory and refreshed automatically shortly before
  its `expires_in` runs out.
- If the admin API ever answers `401` with a token the cache believed was
  still valid (e.g. it was revoked server-side early), the connector
  transparently discards it, fetches a fresh one, and retries the request
  **once** before giving up.
- Almost every deployment has `auth_realm` equal to `realm` (the default);
  set it separately only if your service-account client actually lives in a
  different realm than the one it operates on.

Every admin verb calls `{base_url}/admin/realms/{realm}/<endpoint>`,
authenticated with `Authorization: Bearer <admin token>`. A non-2xx response
(after the one 401 retry) is surfaced as a plugin error carrying the status
code and response body — nothing is swallowed.

### `insecure_skip_verify` — read this before setting it

Self-hosted Keycloak commonly runs behind a self-signed certificate, which a
normal TLS client will reject. Setting `insecure_skip_verify: true` disables
certificate verification **entirely** — a network position able to intercept
traffic to `base_url` can impersonate your Keycloak host undetected, with
nothing tying the connection to your real server. Only set it when
`base_url` is reachable exclusively over a network you already trust (a LAN
or VPN, not the open internet), and prefer installing a real certificate
where you can instead.

## Verbs

Selected by `uses: <name>.<verb>`. Every verb returns `status_code` (the
HTTP status). Required options are marked `*`. `id` on every `user_*` verb is
a **scoped** option (`user`), so conductor gates which user an agent-driven
dispatch may name.

### Realms

- **`realms`** — realms visible to this client. No options. → `items`.
- **`realm_get`** — the connection's target realm's details. No options. →
  `result`.

### Users

- **`users`** — list/search users in the target realm. `search` (substring
  match across username/first/last/email), `username` (exact username
  filter), `email` (exact email filter), `first` (integer, pagination
  offset), `max` (integer, page size). → `items`.
- **`user_get`** — get one user by id. `id`* (Keycloak user id, UUID). →
  `result`.
- **`user_create`** — create a user. `body`* (map; a Keycloak
  `UserRepresentation`, e.g. `{username, email, enabled, firstName, lastName,
  credentials, attributes, ...}`). → `id` (the created user's id, parsed from
  the response's `Location` header — Keycloak's `201` carries no body, so a
  following step can reference `{{ steps.<id>.outputs.id }}` without a
  separate lookup).
- **`user_update`** — update a user (full replace of the given fields). `id`*,
  `body`* (map; `UserRepresentation` fields to update). → (no extra outputs
  beyond `status_code`).
- **`user_delete`** — delete a user. `id`*. → (no extra outputs beyond
  `status_code`).
- **`user_reset_password`** — set (or reset) a user's password. `id`*,
  `value`* (the new password), `temporary` (boolean, force the user to
  change it at next login, default false), `type` (credential type, default
  `password`). → (no extra outputs beyond `status_code`).
- **`user_logout`** — invalidate a user's active sessions. `id`*. → (no
  extra outputs beyond `status_code`).

### Groups, clients & roles

- **`groups`** — list top-level groups in the target realm. No options. →
  `items`.
- **`clients`** — list clients in the target realm. No options. → `items`.
- **`roles`** — list realm-level roles in the target realm. No options. →
  `items`.

### Sessions & events

- **`sessions`** — per-client session counts in the target realm. No
  options. → `items`.
- **`events`** — login/admin event log for the target realm. `type` (event
  type filter, e.g. `LOGIN`, `LOGIN_ERROR`), `user` (user id filter), `max`
  (integer, page size). → `items`.

### Escape hatch

- **`api`** — raw escape hatch for any Keycloak endpoint. `method` (HTTP
  method, default `GET`), `path`* (path relative to `base_url` — not scoped
  under `{realm}`, so it can reach `/admin/realms/...` or `/realms/...`
  alike, e.g. `/admin/realms/master/users/count`), `query` (map, query
  string parameters), `body` (any, JSON request body). → `result` (object
  response) or `items` (array response).
## No source

This connector is verb-only: it declares no `Events` and does not implement
`StartSource`. Poll the `events` verb, or `sessions`, from a scheduled
workflow if you need to react to Keycloak activity.

## Capabilities & security

Declares **no** egress — Keycloak is always self-hosted, so unlike a
connector with a fixed public hostname, there is no address this plugin can
declare on the operator's behalf. Set `network:` on the connector instance
to your own Keycloak host (`host:port`) to scope its egress.

- Prefer a **dedicated service-account client** (Clients > your client >
  Service accounts roles) scoped to only the admin roles this connector
  instance needs (e.g. `manage-users`), over a client granted
  `realm-admin`.
- Review `insecure_skip_verify` above before enabling it — it disables TLS
  verification entirely, not just hostname checking.
- `client_secret` and any password passed to `user_reset_password` should
  come from your secret store (`${VAR}` / `secrets.*`), never a literal in
  the workflow file.
