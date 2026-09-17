# `nginx-proxy-manager` connector

Drive a self-hosted Nginx Proxy Manager (NPM) instance over its REST API
(`/api/*`). Proxy hosts (list/get/create/update/delete/enable/disable),
redirection hosts, streams, dead (404) hosts, access lists, certificates, a
host report summary, and a raw `api` escape hatch for any endpoint a
first-class verb does not cover. Built on the standard library's `net/http`
only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/nginx-proxy-manager/main.go`](../../connectors/nginx-proxy-manager/main.go)
- **Provides:** `nginx-proxy-manager`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own instance)

```yaml
connectors:
  npm:
    use: nginx-proxy-manager
    base_url: http://npm.example.com:81
    email: admin@example.com
    password: ${NPM_PASSWORD}
    network: ["npm.example.com:81"]   # narrow the declared egress to your instance

steps:
  - uses: npm.proxy_hosts
  - uses: npm.proxy_host_create
    with:
      body:
        domain_names: ["app.example.com"]
        forward_scheme: http
        forward_host: 10.0.0.9
        forward_port: 8080
```

## Setup

Produces the NPM admin login the connector exchanges for a JWT.

**Prerequisites:** a running Nginx Proxy Manager instance and admin access to it.

1. Open the NPM admin UI (default `http://<host>:81`).
2. On first run, log in with the default admin (`admin@example.com` /
   `changeme`) — NPM immediately prompts you to set a real name/email/
   password; do that rather than leaving the default in place.
3. To use a dedicated automation account instead, open **Users** (top-right
   avatar menu) > **Add User**, give it Admin permissions, and set its
   email/password there.
4. The connector exchanges this email/password for a JWT via
   `POST /api/tokens` — there is no separate static API key.

```yaml
connectors:
  npm:
    use: nginx-proxy-manager
    base_url: http://npm.example.com:81
    email: admin@example.com
    password: ${NPM_PASSWORD}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string (required) | NPM admin root, e.g. `http://npm.example.com:81` (no trailing `/api`) |
| `email` | string (required) | NPM user identity, exchanged (with password) for a JWT via `POST /api/tokens` |
| `password` | string (required) | NPM user password |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) — common with self-signed certs on a home-lab NPM box, but disables protection against a man-in-the-middle; prefer importing the box's real certificate instead |

Every verb calls `base_url + "/api" + <endpoint>` (the token exchange itself
is `base_url + "/api/tokens"`). A non-2xx response is returned as an error
carrying the status code and response body — nothing is swallowed.

## Token (JWT) login flow

NPM authenticates by exchanging an email/password identity pair for a JWT,
not a static API key:

1. The first request for a connector instance calls `POST {base_url}/api/tokens`
   with `{"identity": "<email>", "secret": "<password>"}` as a JSON body.
2. On success, NPM answers with `{"token": "<jwt>", "expires": ...}`. The
   connector caches the `token` on the connector instance's client.
3. Every subsequent request sends the token via `Authorization: Bearer <jwt>`.
4. Login happens once per connector instance, not once per verb call — the
   token is cached and reused across `Invoke` calls.
5. If a later call gets back HTTP 401 (an expired or invalid token), the
   connector discards the cached token, logs in again, and retries the call
   once before surfacing an error.
6. Bad credentials (or another login failure) are returned as an error at the
   first verb call, carrying NPM's status code and response body.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; NPM's list
endpoints return a bare JSON array, hoisted into `items` — everything else
(an object, or a bare boolean like the `enable`/`disable` response) is
carried in `result`.

| verb | endpoint | notes |
|------|----------|-------|
| `proxy_hosts` | `GET /api/nginx/proxy-hosts` | `items` |
| `proxy_host_get` | `GET /api/nginx/proxy-hosts/{id}` | `id`\*; `result` |
| `proxy_host_create` | `POST /api/nginx/proxy-hosts` | `body`\* (the full NPM proxy host object — `domain_names`, `forward_scheme`, `forward_host`, `forward_port`, `certificate_id`, `access_list_id`, `ssl_forced`, `block_exploits`, `allow_websocket_upgrade`, `locations`, `advanced_config`, ...); `result` |
| `proxy_host_update` | `PUT /api/nginx/proxy-hosts/{id}` | `id`\*, `body`\* (same shape); `result` |
| `proxy_host_delete` | `DELETE /api/nginx/proxy-hosts/{id}` | `id`\*; `result` |
| `proxy_host_enable` | `POST /api/nginx/proxy-hosts/{id}/enable` | `id`\*; `result` |
| `proxy_host_disable` | `POST /api/nginx/proxy-hosts/{id}/disable` | `id`\*; `result` |
| `redirection_hosts` | `GET /api/nginx/redirection-hosts` | `items` |
| `streams` | `GET /api/nginx/streams` | `items` |
| `dead_hosts` | `GET /api/nginx/dead-hosts` | `items` (404 hosts) |
| `access_lists` | `GET /api/nginx/access-lists` | `items` |
| `certificates` | `GET /api/nginx/certificates` | `items` |
| `reports` | `GET /api/reports/hosts` | `result` (host counts summary) |
| `api` | `method` + `path` (under `/api`) + `query` + `body` — escape hatch for anything without a first-class verb | `result` (object/scalar response) or `items` (array response) |

`proxy_host_create`/`proxy_host_update` accept a `body` map matching NPM's
proxy host schema verbatim rather than modeling each field individually —
NPM validates the payload and any rejection surfaces as an error carrying its
status code and response body.

## Capabilities & security

Declares empty egress — NPM is self-hosted, so there is no fixed public host
to declare. Set `network: ["<your-host>:<port>"]` on the connector instance
to narrow it to your own server. `insecure_skip_verify` is available for
self-signed certificates but disables TLS certificate verification for that
connector instance; prefer importing the box's real certificate when
possible.
