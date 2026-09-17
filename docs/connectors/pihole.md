# `pihole` connector

Drive a self-hosted Pi-hole v6 instance over its modern REST API (`/api/*`).
Stats summary/history/query log, top-domains/top-clients/upstreams
breakdowns, blocking on/off, allow/deny domain management, lists/groups/
clients, and a gravity (blocklist) update trigger are exposed as verbs, plus
an `api` escape hatch for any endpoint a first-class verb does not cover.
Built on the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/pihole/main.go`](../../connectors/pihole/main.go)
- **Provides:** `pihole`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own instance)

```yaml
connectors:
  pihole:
    use: pihole
    base_url: https://pihole.example.com
    password: ${PIHOLE_PASSWORD}
    network: ["pihole.example.com:443"]   # narrow the declared egress to your instance

steps:
  - uses: pihole.summary
  - uses: pihole.top_domains
    with: { count: 10, blocked: true }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string (required) | Pi-hole v6 web/API root, e.g. `https://pihole.example.com` (no trailing `/api`) |
| `password` | string (required) | Pi-hole web/app password, exchanged for a session via `POST /api/auth` |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) — common with self-signed certs on a home-lab Pi-hole, but disables protection against a man-in-the-middle; prefer importing the box's real certificate instead |

Every verb calls `base_url + "/api" + <endpoint>` (the auth exchange itself is
`base_url + "/api/auth"`). A non-2xx response is returned as an error
carrying the status code and response body — nothing is swallowed.

## Session (password → SID) login flow

Pi-hole v6 authenticates by exchanging a password for a session, not a static
API token:

1. The first request for a connector instance calls `POST {base_url}/api/auth`
   with `{"password": "<password>"}` as a JSON body.
2. On success, Pi-hole answers with `{"session": {"valid": true, "sid": "...",
   "csrf": "...", "validity": <seconds>}}`. The connector caches the `sid` and
   `csrf` on the connector instance's client.
3. Every subsequent request sends the session ID via the `X-FTL-SID` header.
   Any state-changing request (anything other than `GET` — `set_blocking`,
   `domain_add`, `domain_remove`, `gravity_update`, and a non-`GET` `api`
   call) also sends the CSRF token via the `X-FTL-CSRF` header.
4. Login happens once per connector instance, not once per verb call — the
   session is cached and reused across `Invoke` calls.
5. If a later call gets back HTTP 401 (an expired or invalid SID), the
   connector discards the cached session, logs in again, and retries the call
   once before surfacing an error.
6. A bad password (or another login failure) is returned as an error at the
   first verb call, carrying Pi-hole's status code and response body.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; most also return
either `result` (a single object) or `items` (a list, alongside the full
decoded body in `result`).

| verb | endpoint | outputs |
|------|----------|---------|
| `summary` | `GET /api/stats/summary` | `result` |
| `history` | `GET /api/history` (`from`, `until`) | `result` + `items` (hoisted from `history`) |
| `queries` | `GET /api/queries` (`from`, `until`, `length`, `cursor`, `domain`, `client`, `upstream`, `type`, `status`, `blocked`) | `result` + `items` (hoisted from `queries`) |
| `top_domains` | `GET /api/stats/top_domains` (`count`, `blocked`) | `result` + `items` (hoisted from `domains`) |
| `top_clients` | `GET /api/stats/top_clients` (`count`, `blocked`) | `result` + `items` (hoisted from `clients`) |
| `upstreams` | `GET /api/stats/upstreams` | `result` + `items` (hoisted from `upstreams`) |
| `blocking` | `GET /api/dns/blocking` | `result` |
| `set_blocking` | `POST /api/dns/blocking` (`blocking`\*, `timer`) | `result` |
| `domains` | `GET /api/domains` (`type`, `kind` filters) | `result` + `items` (hoisted from `domains`) |
| `domain_add` | `POST /api/domains/{type}/{kind}` (`type`\*, `kind`\*, `domain`\*, `comment`, `enabled`) | `status_code` (+ `result` if the body is non-empty) |
| `domain_remove` | `DELETE /api/domains/{type}/{kind}/{domain}` (`type`\*, `kind`\*, `domain`\*) | `status_code` (+ `result` if the body is non-empty) |
| `lists` | `GET /api/lists` (`type` filter) | `result` + `items` (hoisted from `lists`) |
| `groups` | `GET /api/groups` | `result` + `items` (hoisted from `groups`) |
| `clients` | `GET /api/clients` | `result` + `items` (hoisted from `clients`) |
| `gravity_update` | `POST /api/action/gravity` | `status_code` (+ `result`, an object or plain string, if the body is non-empty) |
| `api` | `method` + `path` (under `/api`) + `query` + `body` — escape hatch for anything without a first-class verb | `result` (object response) or `items` (array response) |

`domain_add`/`domain_remove`'s `type` must be `allow` or `deny`; `kind` must be
`exact` or `regex` — both are validated locally before the request is sent.

## Capabilities & security

Declares empty egress — Pi-hole is self-hosted, so there is no fixed public
host to declare. Set `network: ["<your-host>:443"]` on the connector instance
to narrow it to your own server. `insecure_skip_verify` is available for
self-signed certificates but disables TLS certificate verification for that
connector instance; prefer importing the box's real certificate when
possible.
