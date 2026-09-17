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

## Setup

Produces an OPNsense API key/secret pair the connector sends as HTTP Basic auth.

**Prerequisites:** a running OPNsense instance and admin access to its web UI.

1. Log into the OPNsense web UI and open **System > Access > Users**.
2. Either create a dedicated automation user (recommended — grant only the
   privileges the workflow needs) or edit an existing one.
3. On the user's edit page, scroll to **API keys** and click **+** to
   generate a new key.
4. OPNsense downloads a `.txt` file containing the `key` and `secret` — save
   both; the secret is not shown again.

```yaml
connectors:
  fw:
    use: opnsense
    base_url: https://opnsense.example.com
    api_key: ${OPNSENSE_API_KEY}
    api_secret: ${OPNSENSE_API_SECRET}
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

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb returns `status_code`; most also return `result` (the full
  decoded response). List-shaped endpoints additionally return `items` —
  many OPNsense "search" endpoints wrap their rows under a `rows` key, which
  is hoisted into `items` alongside the full `result` (for pagination
  metadata like `total`/`rowCount`); a bare JSON array response is used
  directly as `items`.
- `name` (`service_*`) and `uuid` (`alias_get`/`alias_toggle`) are path
  parameters, URL-escaped before use.

Required options are marked `*`.

### Firmware

- **`firmware_status`** — check for available firmware/package updates (`GET /api/core/firmware/status`). → `result`, `status_code`.
- **`firmware_upgrade`** — run a firmware upgrade (`POST /api/core/firmware/upgrade`). → `result`, `status_code`.

### Services

- **`services`** — list known services and their running state (`GET /api/core/service/search`). → `result`, `items` (from `result.rows`), `status_code`.
- **`service_restart`** — restart a service (`POST /api/core/service/restart/{name}`). `name`* — service name, e.g. `unbound`, `dpinger`. → `result`, `status_code`.
- **`service_start`** — start a service (`POST /api/core/service/start/{name}`). `name`* — service name. → `result`, `status_code`.
- **`service_stop`** — stop a service (`POST /api/core/service/stop/{name}`). `name`* — service name. → `result`, `status_code`.

### Firewall aliases

- **`firewall_aliases`** — search/list firewall aliases (`GET /api/firewall/alias/searchItem`). → `result`, `items` (from `result.rows`), `status_code`.
- **`alias_get`** — get one firewall alias's details (`GET /api/firewall/alias/getItem/{uuid}`). `uuid`* — alias UUID. → `result`, `status_code`.
- **`alias_add`** — create a firewall alias (`POST /api/firewall/alias/addItem`, posted as `{"alias": {...}}`). `alias`* (map) — alias fields, e.g. `{name, type, content, description, enabled}`. → `result`, `status_code`.
- **`alias_toggle`** — enable/disable (or toggle) a firewall alias (`POST /api/firewall/alias/toggleItem/{uuid}[/{enabled}]`). `uuid`* — alias UUID, `enabled` (boolean; `1` to enable, `0` to disable — omit to toggle the current state). → `result`, `status_code`.
- **`firewall_apply`** — apply pending alias changes (reconfigure filter/aliases) (`POST /api/firewall/alias/reconfigure`). → `result`, `status_code`.

### Network & DNS

- **`interfaces`** — interface overview info: physical name, description, enabled state, identifier (`GET /api/interfaces/overview/interfacesInfo`). → `result`, `status_code`.
- **`dhcp_leases`** — search DHCPv4 leases (`GET /api/dhcpv4/leases/searchLease`). → `result`, `items` (from `result.rows`), `status_code`.
- **`gateway_status`** — gateway monitoring status (`GET /api/routes/gateway/status`). → `result`, `status_code`.
- **`unbound_settings`** — current Unbound (DNS resolver) settings (`GET /api/unbound/settings/get`). → `result`, `status_code`.

### System

- **`system_reboot`** — reboot the firewall (`POST /api/core/system/reboot`). → `result`, `status_code`.
- **`system_status`** — system status: uptime, versions, health (`GET /api/core/system/status`). → `result`, `status_code`.

### Raw access

- **`api`** — raw escape hatch: any OPNsense API endpoint under `/api`, for anything without a first-class verb. `method` (HTTP method, default `GET`), `path`* (path under `/api`, e.g. `/core/firmware/status`), `query` (map of query-string parameters), `body` (any — JSON request body). → `result` (object response), `items` (array response), `status_code`.

## Capabilities & security

Declares empty egress — OPNsense is self-hosted, so there is no fixed
public host to declare. Set `network: ["<your-host>:443"]` on the connector
instance to narrow it to your own firewall. Provide an API key/secret pair
scoped to the least privilege the workflow needs (OPNsense API keys are tied
to a user account and inherit that user's privileges — create a dedicated
user for automation rather than reusing an admin's key).
