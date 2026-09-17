# `adguard` connector

AdGuard Home (a self-hosted network-wide DNS ad/tracker blocker) as a
connector: server status/stats, the DNS query log, protection toggle, DNS
filtering (status, allow-list/block-list subscriptions, custom rules), DNS
rewrites, clients, DNS info/config, and safe-browsing/parental-control
toggles over the AdGuard Home REST API, plus a raw `api` escape hatch. Built
on the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/adguard/main.go`](../../connectors/adguard/main.go)
- **Provides:** `adguard`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own instance)

```yaml
connectors:
  ag:
    use: adguard
    base_url: http://adguard.example.com
    username: ${ADGUARD_USERNAME}
    password: ${ADGUARD_PASSWORD}
    network: ["adguard.example.com:80"]   # narrow the declared egress to your instance
```

## Setup

Produces the AdGuard Home admin login the connector sends as HTTP Basic auth.

**Prerequisites:** a running AdGuard Home instance and admin access to it.

1. If you haven't completed AdGuard Home's first-run wizard, open
   `http://<host>:3000/install.html` and set an admin username/password there.
2. Otherwise, log into the existing web UI at `http://<host>` with the admin
   username/password you already created (or check `AdGuardHome.yaml`'s
   `users:` section on the host if you've forgotten it).
3. AdGuard Home has no separate API token — the same username/password used
   for the web UI is sent as HTTP Basic auth on every request.

```yaml
connectors:
  ag:
    use: adguard
    base_url: http://adguard.example.com
    username: ${ADGUARD_USERNAME}
    password: ${ADGUARD_PASSWORD}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | AdGuard Home instance root, e.g. `http://adguard.example.com` (no trailing `/control`) |
| `username` | string | AdGuard Home username, sent as the HTTP Basic auth **username** |
| `password` | string | AdGuard Home password, sent as the HTTP Basic auth **password** |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) |

Every verb calls `base_url + "/control" + <endpoint>`, authenticated with
HTTP Basic auth (`username`/`password` — the same credentials used to log
into the AdGuard Home web UI; there is no separate API token). A non-2xx
response is returned as an error carrying the status code and response body
— nothing is swallowed.

### `insecure_skip_verify`: read this before enabling

AdGuard Home instances commonly run behind a self-signed certificate on a
LAN, so `insecure_skip_verify: true` exists as an explicit, greppable
opt-out of TLS certificate verification. Enabling it also disables **all**
protection against a man-in-the-middle on the path to the instance — anyone
who can intercept the connection can read the username/password and forge
responses. Only enable it for instances reached over a trusted network
(e.g. the same LAN, or a VPN), and prefer installing a real certificate
instead when possible.

## No source: the API is request/response only

AdGuard Home's API has no webhook or event-stream mechanism to listen on —
every endpoint is a plain synchronous request/response. This connector is
verb-only: it declares no `Events` and does not implement `StartSource`.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb returns `status_code`; most also return `result` (the full
  decoded response), and list-shaped endpoints additionally (or instead)
  return `items`.
- `protection`'s current state (whether protection is active, and any pause
  duration) is surfaced by the `status` verb, not `protection` itself —
  `protection` is a fire-and-forget toggle.
- `safebrowsing_toggle` and `parental_toggle` pick the AdGuard Home
  `enable`/`disable` endpoint based on the boolean `enabled` option, since
  AdGuard Home models these as two separate action endpoints rather than a
  single settable field.

Required options are marked `*`.

### Status & stats

- **`status`** — server status: version, ports, protection state, running (`GET /control/status`). → `result`, `status_code`.
- **`stats`** — query statistics: counts, top domains/clients, processing time (`GET /control/stats`). → `result`, `status_code`.
- **`querylog`** — the DNS query log (`GET /control/querylog`). `older_than` (string — return entries older than this RFC3339 timestamp), `limit` (integer — max number of entries), `search` (string — search/filter term). → `result`, `items` (hoisted from `result.data`), `status_code`.

### Protection

- **`protection`** — enable/disable protection, optionally for a fixed duration (`POST /control/protection`). `enabled`* (boolean — true to enable protection, false to disable), `duration` (integer — pause duration in milliseconds; 0/omitted = indefinite). → `status_code` (+ `result` if the body is non-empty).
- **`safebrowsing_toggle`** — enable/disable the safe browsing filter (`POST /control/safebrowsing/enable` or `.../disable`). `enabled`* (boolean). → `status_code` (+ `result` if the body is non-empty).
- **`parental_toggle`** — enable/disable AdGuard parental control (`POST /control/parental/enable` or `.../disable`). `enabled`* (boolean). → `status_code` (+ `result` if the body is non-empty).

### Filtering

- **`filtering_status`** — filtering config: enabled, update interval, filter lists, custom rules (`GET /control/filtering/status`). → `result`, `status_code`.
- **`filtering_add_url`** — add a filter (block-list or allow-list) subscription (`POST /control/filtering/add_url`). `name`* — display name for the filter list, `url`* — filter list URL, `whitelist` (boolean — true to add as an allow-list instead of a block-list). → `status_code` (+ `result` if the body is non-empty).
- **`filtering_remove_url`** — remove a filter subscription (`POST /control/filtering/remove_url`). `url`* — filter list URL to remove, `whitelist` (boolean — true if the URL is an allow-list filter). → `status_code` (+ `result` if the body is non-empty).
- **`filtering_set_rules`** — replace the custom (user-defined) filtering rules (`POST /control/filtering/set_rules`). `rules`* (list) — full list of custom filtering rules, replacing the current set. → `status_code` (+ `result` if the body is non-empty).

### DNS rewrites

- **`rewrites`** — list DNS rewrites (`GET /control/rewrite/list`). → `items` (bare array response), `status_code`.
- **`rewrite_add`** — add a DNS rewrite (`POST /control/rewrite/add`). `domain`* — domain to rewrite, `answer`* — IP address or CNAME target to answer with. → `status_code` (+ `result` if the body is non-empty).
- **`rewrite_delete`** — delete a DNS rewrite (`POST /control/rewrite/delete`). `domain`* — domain of the rewrite to delete, `answer`* — answer of the rewrite to delete. → `status_code` (+ `result` if the body is non-empty).

### Clients & DNS config

- **`clients`** — list configured and auto-discovered clients (`GET /control/clients`). → `result`, `items` (hoisted from `result.clients`), `status_code`.
- **`dns_info`** — current DNS server configuration (`GET /control/dns_info`). → `result`, `status_code`.
- **`dns_config`** — update DNS server configuration (`POST /control/dns_config`). `config`* (map) — DNS config fields to update, e.g. `{upstream_dns, bootstrap_dns, ratelimit, blocking_mode, cache_enabled, dnssec_enabled, ...}`. → `status_code` (+ `result` if the body is non-empty).

### Raw access

- **`api`** — raw escape hatch: any AdGuard Home API endpoint under `/control`, for anything without a first-class verb. `method` (HTTP method, default `GET`), `path`* (path under `/control`, e.g. `/filtering/status`), `query` (map of query-string parameters), `body` (any — JSON request body). → `result` (object response), `items` (array response), `status_code`.

## Capabilities & security

Declares empty egress — AdGuard Home is self-hosted, so there is no fixed
public host to declare. Set `network: ["<your-host>:<port>"]` on the
connector instance to narrow it to your own instance. Provide a
username/password scoped to the least privilege the workflow needs (AdGuard
Home does not currently support scoped API tokens — the credentials are a
full admin login, so treat them with the same care as the web UI password).
