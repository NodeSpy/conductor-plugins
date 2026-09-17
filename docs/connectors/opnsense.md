# `opnsense` connector

OPNsense (self-hosted firewall/router) as a connector: firmware status/
upgrade, service search/restart/start/stop, firewall alias search/get/add/
toggle/apply, interfaces, DHCPv4 leases, gateway status, Unbound settings,
and system reboot/status over the OPNsense REST API, plus a raw `api`
escape hatch. Built on the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/opnsense/main.go`](../../connectors/opnsense/main.go)
- **Provides:** `opnsense`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own instance)

```yaml
connectors:
  fw:
    use: opnsense
    base_url: https://opnsense.example.com
    api_key: ${OPNSENSE_API_KEY}
    api_secret: ${OPNSENSE_API_SECRET}
    network: ["opnsense.example.com:443"]   # narrow the declared egress to your instance
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | OPNsense instance root, e.g. `https://opnsense.example.com` (no trailing `/api`) |
| `api_key` | string | OPNsense API key, sent as the HTTP Basic auth **username** |
| `api_secret` | string | OPNsense API secret, sent as the HTTP Basic auth **password** |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) |

Every verb calls `base_url + "/api" + <endpoint>`, authenticated with HTTP
Basic auth (`api_key`/`api_secret` generated under System > Access > Users >
API keys — there is no bearer token). A non-2xx response is returned as an
error carrying the status code and response body — nothing is swallowed.

### `insecure_skip_verify`: read this before enabling

OPNsense instances commonly run behind a self-signed certificate on a LAN,
so `insecure_skip_verify: true` exists as an explicit, greppable opt-out of
TLS certificate verification. Enabling it also disables **all** protection
against a man-in-the-middle on the path to the firewall — anyone who can
intercept the connection can read the API key/secret and forge responses.
Only enable it for instances reached over a trusted network (e.g. the same
LAN, or a VPN), and prefer installing a real certificate (e.g. via
OPNsense's built-in ACME/Let's Encrypt plugin) instead when possible.

## No source: the API is request/response only

OPNsense's API has no webhook or event-stream mechanism to listen on — every
endpoint is a plain synchronous request/response. This connector is
verb-only: it declares no `Events` and does not implement `StartSource`.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; most also return
`result` (the full decoded response), and list-shaped endpoints additionally
return `items` — many OPNsense "search" endpoints wrap their rows under a
`rows` key, which is hoisted into `items` alongside the full `result` (for
pagination metadata like `total`/`rowCount`); a bare JSON array response is
used directly as `items`.

| verb | endpoint | outputs |
|------|----------|---------|
| `firmware_status` | `GET /api/core/firmware/status` | `result` |
| `firmware_upgrade` | `POST /api/core/firmware/upgrade` | `result` |
| `services` | `GET /api/core/service/search` | `result` + `items` (hoisted from `result.rows`) |
| `service_restart` | `POST /api/core/service/restart/{name}` | `result` |
| `service_start` | `POST /api/core/service/start/{name}` | `result` |
| `service_stop` | `POST /api/core/service/stop/{name}` | `result` |
| `firewall_aliases` | `GET /api/firewall/alias/searchItem` | `result` + `items` (hoisted from `result.rows`) |
| `alias_get` | `GET /api/firewall/alias/getItem/{uuid}` | `result` |
| `alias_add` | `POST /api/firewall/alias/addItem` (`alias`\*, a map of alias fields) | `result` |
| `alias_toggle` | `POST /api/firewall/alias/toggleItem/{uuid}[/{enabled}]` (`enabled` omitted toggles the current state) | `result` |
| `firewall_apply` | `POST /api/firewall/alias/reconfigure` | `result` |
| `interfaces` | `GET /api/interfaces/overview/interfacesInfo` | `result` |
| `dhcp_leases` | `GET /api/dhcpv4/leases/searchLease` | `result` + `items` (hoisted from `result.rows`) |
| `gateway_status` | `GET /api/routes/gateway/status` | `result` |
| `unbound_settings` | `GET /api/unbound/settings/get` | `result` |
| `system_reboot` | `POST /api/core/system/reboot` | `result` |
| `system_status` | `GET /api/core/system/status` | `result` |
| `api` | `method` + `path` (under `/api`) + `query` + `body` — escape hatch for anything without a first-class verb | `result` (object response) or `items` (array response) |

`name` (`service_*`) and `uuid` (`alias_get`/`alias_toggle`) are path
parameters, URL-escaped before use. `alias_add`'s `alias` option is posted
as `{"alias": {...}}`, matching OPNsense's `addItem` request shape (e.g.
`{name, type, content, description, enabled}`).

## Capabilities & security

Declares empty egress — OPNsense is self-hosted, so there is no fixed
public host to declare. Set `network: ["<your-host>:443"]` on the connector
instance to narrow it to your own firewall. Provide an API key/secret pair
scoped to the least privilege the workflow needs (OPNsense API keys are tied
to a user account and inherit that user's privileges — create a dedicated
user for automation rather than reusing an admin's key).
