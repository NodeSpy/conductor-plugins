# `pfsense` connector

pfSense (self-hosted firewall/router) as a connector: firewall rule
list/get/create/delete, firewall-change apply, firewall alias list/get,
interface status, service list/control, DHCP server leases, system status,
and gateway status over the **pfSense REST API v2** (the
[pfSense-pkg-RESTAPI](https://github.com/pfrest/pfSense-pkg-RESTAPI) package,
formerly `jaredhendrickson13/pfsense-api`), plus a raw `api` escape hatch.
Built on the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/pfsense/main.go`](../../connectors/pfsense/main.go)
- **Provides:** `pfsense`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own instance)

## Requires the pfSense REST API v2 package

pfSense does **not** ship a REST API out of the box. Before any verb here
will work, install the REST API v2 package on the target firewall (System >
Package Manager > Available Packages, search "RESTAPI", or see the
[pfSense-pkg-RESTAPI](https://pfrest.org/) install docs), then generate an
API key for a user under System > REST API > Access.

```yaml
connectors:
  fw:
    use: pfsense
    base_url: https://pfsense.example.com
    api_key: ${PFSENSE_API_KEY}
    network: ["pfsense.example.com:443"]   # narrow the declared egress to your instance
```

## Setup

Produces a pfSense REST API v2 key the connector sends as `X-API-Key`.

**Prerequisites:** a running pfSense instance, admin access to it, and the
pfSense REST API v2 package installed (see above).

1. Install the package first (**System > Package Manager > Available
   Packages**, search "RESTAPI") if you haven't already.
2. Log into the pfSense web UI and open **System > REST API > Access** (or
   **System > REST API > Users**, depending on package version).
3. Select the user the key should belong to (or create a dedicated one under
   **System > User Manager** first) and generate an API key for it.
4. Copy the generated key — it's shown once.

```yaml
connectors:
  fw:
    use: pfsense
    base_url: https://pfsense.example.com
    api_key: ${PFSENSE_API_KEY}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | pfSense instance root, e.g. `https://pfsense.example.com` (no trailing `/api/v2`) |
| `api_key` | string | pfSense REST API key, sent as the `X-API-Key` header |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) |

Every verb calls `base_url + "/api/v2" + <endpoint>`, authenticated with the
`X-API-Key` header. A non-2xx response is returned as an error carrying the
status code and the pfSense error message — nothing is swallowed.

### `insecure_skip_verify`: read this before enabling

pfSense instances commonly run behind a self-signed certificate on a LAN, so
`insecure_skip_verify: true` exists as an explicit, greppable opt-out of TLS
certificate verification. Enabling it also disables **all** protection
against a man-in-the-middle on the path to the firewall — anyone who can
intercept the connection can read the API key and forge responses. Only
enable it for instances reached over a trusted network (e.g. the same LAN,
or a VPN), and prefer installing a real certificate (e.g. via pfSense's
built-in ACME/Let's Encrypt package) instead when possible.

## The response envelope

The pfSense REST API v2 wraps every response in an envelope:

```json
{"code": 200, "status": "ok", "response_id": "SUCCESS", "message": "", "data": [...]}
```

This connector hoists the envelope's `data` field for you: a list-shaped
`data` becomes `items`, an object-shaped `data` becomes `result`. You never
have to unwrap the envelope yourself. `status_code` in every verb's outputs
is the HTTP status code (which normally agrees with the envelope's `code`).

## No source: the API is request/response only

Like the rest of the REST API v2 package, there is no webhook or
event-stream mechanism to listen on — every endpoint is a plain synchronous
request/response. This connector is verb-only: it declares no `Events` and
does not implement `StartSource`.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb returns `status_code`; list-shaped endpoints hoist the
  envelope's `data` into `items`, object-shaped endpoints hoist it into
  `result`.
- `id` (`rule_get`/`rule_delete`/`alias_get`) is sent as a query-string
  parameter, matching the REST API's convention for identifying a single
  object on its singular endpoints (`/firewall/rule`, `/firewall/alias`) —
  firewall rule and alias IDs are array indices.
- `interfaces`, `services`, and `service_control` live under
  `/api/v2/status/...` in the real API (`StatusInterfacesEndpoint`,
  `StatusServicesEndpoint`, `StatusServiceEndpoint`) rather than bare
  `/api/v2/interfaces` or `/api/v2/service/{action}` paths — this connector
  targets the endpoints the pfSense-pkg-RESTAPI package actually exposes.

Required options are marked `*`.

### Firewall rules

- **`firewall_rules`** — list all firewall rules (`GET /api/v2/firewall/rules`). → `items`, `status_code`.
- **`rule_get`** — get one firewall rule's details (`GET /api/v2/firewall/rule?id=`). `id`* — rule ID (array index). → `result`, `status_code`.
- **`rule_create`** — create a firewall rule (`POST /api/v2/firewall/rule`). `rule`* (map) — rule fields, e.g. `{type, interface, ipprotocol, protocol, source, destination, descr}` (posted as-is onto the `FirewallRule` model). → `result`, `status_code`.
- **`rule_delete`** — delete a firewall rule (`DELETE /api/v2/firewall/rule?id=`). `id`* — rule ID (array index). → `result`, `status_code`.
- **`firewall_apply`** — apply pending firewall changes (`POST /api/v2/firewall/apply`). → `result`, `status_code`.

### Firewall aliases

- **`aliases`** — list all firewall aliases (`GET /api/v2/firewall/aliases`). → `items`, `status_code`.
- **`alias_get`** — get one firewall alias's details (`GET /api/v2/firewall/alias?id=`). `id`* — alias ID (array index) or name. → `result`, `status_code`.

### Interfaces, services & status

- **`interfaces`** — interface status: link state, addresses, stats (`GET /api/v2/status/interfaces`). → `items`, `status_code`.
- **`services`** — list known services and their running state (`GET /api/v2/status/services`). → `items`, `status_code`.
- **`service_control`** — start, stop, or restart a service (`POST /api/v2/status/service`, mapping onto the `Service` model). `name`* — service name, e.g. `unbound`, `openvpn`; `action`* — one of `start`, `stop`, `restart`. → `result`, `status_code`.
- **`dhcp_leases`** — list DHCP server leases (`GET /api/v2/status/dhcp_server/leases`). → `items`, `status_code`.
- **`system_status`** — system status: versions, temp, load, memory, disk (`GET /api/v2/status/system`). → `result`, `status_code`.
- **`gateways`** — gateway monitoring status (`GET /api/v2/status/gateways`). → `items`, `status_code`.

### Raw access

- **`api`** — raw escape hatch: any pfSense REST API v2 endpoint under `/api/v2`, for anything without a first-class verb. `method` (HTTP method, default `GET`), `path`* (path under `/api/v2`, e.g. `/firewall/rules`), `query` (map of query-string parameters), `body` (any — JSON request body). → `result` (object `data`), `items` (list `data`), `status_code`.

## Capabilities & security

Declares empty egress — pfSense is self-hosted, so there is no fixed public
host to declare. Set `network: ["<your-host>:443"]` on the connector
instance to narrow it to your own firewall. Provide an API key scoped to
the least privilege the workflow needs (pfSense REST API keys are tied to a
user account and inherit that user's privileges — create a dedicated user
for automation rather than reusing an admin's key).
