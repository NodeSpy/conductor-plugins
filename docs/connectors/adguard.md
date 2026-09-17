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

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; most also return
`result` (the full decoded response), and list-shaped endpoints
additionally (or instead) return `items`.

| verb | endpoint | outputs |
|------|----------|---------|
| `status` | `GET /control/status` | `result` |
| `stats` | `GET /control/stats` | `result` |
| `querylog` | `GET /control/querylog` (`older_than`, `limit`, `search`) | `result` + `items` (hoisted from `result.data`) |
| `protection` | `POST /control/protection` (`enabled`\*, `duration`) | `status_code` (+ `result` if the body is non-empty) |
| `filtering_status` | `GET /control/filtering/status` | `result` |
| `filtering_add_url` | `POST /control/filtering/add_url` (`name`\*, `url`\*, `whitelist`) | `status_code` (+ `result` if the body is non-empty) |
| `filtering_remove_url` | `POST /control/filtering/remove_url` (`url`\*, `whitelist`) | `status_code` (+ `result` if the body is non-empty) |
| `filtering_set_rules` | `POST /control/filtering/set_rules` (`rules`\*, replaces the full custom rule set) | `status_code` (+ `result` if the body is non-empty) |
| `rewrites` | `GET /control/rewrite/list` | `items` (bare array response) |
| `rewrite_add` | `POST /control/rewrite/add` (`domain`\*, `answer`\*) | `status_code` (+ `result` if the body is non-empty) |
| `rewrite_delete` | `POST /control/rewrite/delete` (`domain`\*, `answer`\*) | `status_code` (+ `result` if the body is non-empty) |
| `clients` | `GET /control/clients` | `result` + `items` (hoisted from `result.clients`) |
| `dns_info` | `GET /control/dns_info` | `result` |
| `dns_config` | `POST /control/dns_config` (`config`\*, a map of DNS config fields to update) | `status_code` (+ `result` if the body is non-empty) |
| `safebrowsing_toggle` | `POST /control/safebrowsing/enable` or `.../disable` (`enabled`\*) | `status_code` (+ `result` if the body is non-empty) |
| `parental_toggle` | `POST /control/parental/enable` or `.../disable` (`enabled`\*) | `status_code` (+ `result` if the body is non-empty) |
| `api` | `method` + `path` (under `/control`) + `query` + `body` — escape hatch for anything without a first-class verb | `result` (object response) or `items` (array response) |

`protection`'s current state (whether protection is active, and any pause
duration) is surfaced by the `status` verb, not `protection` itself —
`protection` is a fire-and-forget toggle. `safebrowsing_toggle` and
`parental_toggle` pick the AdGuard Home `enable`/`disable` endpoint based on
the boolean `enabled` option, since AdGuard Home models these as two
separate action endpoints rather than a single settable field.

## Capabilities & security

Declares empty egress — AdGuard Home is self-hosted, so there is no fixed
public host to declare. Set `network: ["<your-host>:<port>"]` on the
connector instance to narrow it to your own instance. Provide a
username/password scoped to the least privilege the workflow needs (AdGuard
Home does not currently support scoped API tokens — the credentials are a
full admin login, so treat them with the same care as the web UI password).
