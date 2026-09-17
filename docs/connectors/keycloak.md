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

| verb | request | outputs | notes |
|------|---------|---------|-------|
| `realms` | `GET /admin/realms` | `items` | realms visible to this client |
| `realm_get` | `GET /admin/realms/{realm}` | `result` | the connection's target realm |
| `users` | `GET .../users` | `items` | options: `search`, `username`, `email`, `first`, `max` |
| `user_get` | `GET .../users/{id}` | `result` | options: `id` * |
| `user_create` | `POST .../users` | `status_code` + `id` | options: `body` * (a Keycloak `UserRepresentation`) |
| `user_update` | `PUT .../users/{id}` | `status_code` | options: `id` *, `body` * |
| `user_delete` | `DELETE .../users/{id}` | `status_code` | options: `id` * |
| `user_reset_password` | `PUT .../users/{id}/reset-password` | `status_code` | options: `id` *, `value` *, `temporary`, `type` |
| `user_logout` | `POST .../users/{id}/logout` | `status_code` | options: `id` * |
| `groups` | `GET .../groups` | `items` | top-level groups |
| `clients` | `GET .../clients` | `items` | |
| `roles` | `GET .../roles` | `items` | realm-level roles |
| `sessions` | `GET .../client-session-stats` | `items` | per-client session counts |
| `events` | `GET .../events` | `items` | options: `type`, `user`, `max` |
| `api` | any `method`/`path`/`query`/`body` | `result` (object) or `items` (array) | `path` is relative to `base_url` (not `{realm}`), so it can reach `/admin/realms/...` or `/realms/...` alike |

`*` required. Every verb also returns `status_code` (the HTTP status). `id`
on every `user_*` verb is a **scoped** option (`user`), so conductor gates
which user an agent-driven dispatch may name.

`user_create`'s Keycloak response is a `201 Created` with **no response
body** — the new user's id is only in the `Location` response header
(`.../users/<id>`). This connector parses that id out for you into
`outputs.id`, so a following step can reference
`{{ steps.<id>.outputs.id }}` without a separate lookup.

See `Describe()` in
[`connectors/keycloak/main.go`](../../connectors/keycloak/main.go) for each
verb's full option schema.

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
