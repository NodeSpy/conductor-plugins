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

## Setup

Produces the Pi-hole password the connector exchanges for a session.

**Prerequisites:** a running Pi-hole v6 instance and admin access to its web UI.

1. Log into the Pi-hole web UI and open **Settings > Web Interface / API**.
2. Switch the settings view from **Basic** to **Expert** (top of the page).
3. Under **Configure app password**, generate a new app password (or reuse
   the password you set at install, or via `pihole setpasswd`).
4. Copy the password — this is what the connector exchanges for a session
   via `POST /api/auth`.

```yaml
connectors:
  pihole:
    use: pihole
    base_url: https://pihole.example.com
    password: ${PIHOLE_PASSWORD}
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

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb returns `status_code`; most also return either `result` (a
  single object) or `items` (a list, alongside the full decoded body in
  `result`).
- `domain_add`/`domain_remove`'s `type` must be `allow` or `deny`; `kind`
  must be `exact` or `regex` — both are validated locally before the
  request is sent.

Required options are marked `*`.

### Stats & history

- **`summary`** — overall stats summary: queries, clients, gravity (`GET /api/stats/summary`). → `result`, `status_code`.
- **`history`** — query count history, bucketed over time (`GET /api/history`). `from` (integer — start of the range, unix seconds), `until` (integer — end of the range, unix seconds). → `result`, `items` (hoisted from `history`), `status_code`.
- **`queries`** — the raw query log, paginated (`GET /api/queries`). `from` (integer), `until` (integer), `length` (integer — page size), `cursor` (string — opaque pagination cursor from a previous call), `domain` (string — filter to this domain), `client` (string — filter to this client, IP/name), `upstream` (string — filter to this upstream), `type` (string — filter to this query type, e.g. `A`, `AAAA`), `status` (string — filter to this query status, e.g. `GRAVITY`, `FORWARDED`), `blocked` (boolean — filter to blocked, or if false permitted, queries). → `result`, `items` (hoisted from `queries`), `status_code`.
- **`top_domains`** — the most-queried domains (`GET /api/stats/top_domains`). `count` (integer — number of domains to return, default 10), `blocked` (boolean — top blocked domains instead of top permitted domains). → `result`, `items` (hoisted from `domains`), `status_code`.
- **`top_clients`** — the clients generating the most queries (`GET /api/stats/top_clients`). `count` (integer — number of clients to return, default 10), `blocked` (boolean — top clients by blocked queries instead of total queries). → `result`, `items` (hoisted from `clients`), `status_code`.
- **`upstreams`** — the configured upstream DNS servers and their usage (`GET /api/stats/upstreams`). → `result`, `items` (hoisted from `upstreams`), `status_code`.

### Blocking

- **`blocking`** — current blocking status (`GET /api/dns/blocking`). → `result`, `status_code`.
- **`set_blocking`** — enable or disable blocking, optionally for a limited time (`POST /api/dns/blocking`). `blocking`* (boolean — true to enable blocking, false to disable it), `timer` (integer — seconds until blocking automatically reverts; omit for no timer). → `result`, `status_code`.

### Allow/deny domains

- **`domains`** — list allow/deny domain rules (`GET /api/domains`). `type` (`allow` | `deny` — filter to this rule type), `kind` (`exact` | `regex` — filter to this rule kind). → `result`, `items` (hoisted from `domains`), `status_code`.
- **`domain_add`** — add an allow/deny domain rule (`POST /api/domains/{type}/{kind}`). `type`* (`allow` | `deny`), `kind`* (`exact` | `regex`), `domain`* (the domain, or regex, to add), `comment` (optional free-text comment), `enabled` (boolean — whether the rule is enabled, default true). → `status_code` (+ `result` if the body is non-empty).
- **`domain_remove`** — remove an allow/deny domain rule (`DELETE /api/domains/{type}/{kind}/{domain}`). `type`* (`allow` | `deny`), `kind`* (`exact` | `regex`), `domain`* (the domain, or regex, to remove). → `status_code` (+ `result` if the body is non-empty).

### Lists, groups & clients

- **`lists`** — list configured adlists (`GET /api/lists`). `type` (`allow` | `block` — filter to this list type). → `result`, `items` (hoisted from `lists`), `status_code`.
- **`groups`** — list configured groups (`GET /api/groups`). → `result`, `items` (hoisted from `groups`), `status_code`.
- **`clients`** — list known clients (`GET /api/clients`). → `result`, `items` (hoisted from `clients`), `status_code`.

### Gravity

- **`gravity_update`** — trigger a gravity (blocklist) update (`POST /api/action/gravity`). → `status_code` (+ `result` — an object or plain string — if the body is non-empty).

### Raw access

- **`api`** — raw escape hatch: any Pi-hole API endpoint under `/api`, for anything without a first-class verb. `method` (HTTP method, default `GET`), `path`* (path under `/api`, e.g. `/stats/summary`), `query` (map of query-string parameters), `body` (any — JSON request body). → `result` (object response), `items` (array response), `status_code`.

## Capabilities & security

Declares empty egress — Pi-hole is self-hosted, so there is no fixed public
host to declare. Set `network: ["<your-host>:443"]` on the connector instance
to narrow it to your own server. `insecure_skip_verify` is available for
self-signed certificates but disables TLS certificate verification for that
connector instance; prefer importing the box's real certificate when
possible.
