# `netdata` connector

Netdata as a connector: agent `info`, `charts`/`chart` definitions,
time-series `data`, configured `alarms`, the `alarm_log`, the newer v2
`alerts` API, `contexts`, and a raw `api` escape hatch over the Netdata Agent
REST API — plus a **poll source** that watches `/api/v1/alarms` and emits an
`alarm` event for every alarm currently raised (`WARNING` or `CRITICAL`).
Built on the standard library's `net/http` only.

- **Kind:** connector (verbs + source)
- **Source:** [`connectors/netdata/main.go`](../../connectors/netdata/main.go)
- **Provides:** `netdata`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own instance)

```yaml
connectors:
  monitoring:
    use: netdata
    base_url: http://netdata.example.com:19999
    network: ["netdata.example.com:19999"]   # narrow the declared egress to your instance
triggers:
  - on: monitoring.alarm
    filters: { statuses: [CRITICAL] }
    steps:
      - id: page
        uses: pd.trigger
        options: { summary: "{{.title}}" }
```

## Setup

Produces (optionally) a bearer token for a Netdata Agent behind auth — most
home agents need no credential at all.

**Prerequisites:** a running Netdata Agent (the default `http://<host>:19999`
dashboard) and, if it's exposed beyond your LAN, admin access to whatever
fronts it.

1. If the agent is reachable only on your trusted LAN, skip credentials
   entirely — a stock agent has no login, and `api_key` can be left unset.
2. If the agent sits behind a reverse proxy you control, configure that
   proxy to require a bearer token and use the same value as `api_key` here.
3. If instead you're relaying through Netdata Cloud, generate a token from
   the Cloud UI: profile menu > **Settings** > **API Tokens** > **+**, and
   use that as `api_key`.

```yaml
connectors:
  monitoring:
    use: netdata
    base_url: http://netdata.example.com:19999
```

See **Source — the `alarm` event** below for the poll source that watches
`/api/v1/alarms`.

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | Netdata agent root, e.g. `http://netdata.example.com:19999` (required) |
| `api_key` | string | bearer token, sent as `Authorization: Bearer <api_key>`; **optional** |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) |
| `poll_interval` | duration | **source** poll period (default `1m`) |

Every verb calls `base_url + <path>`, where each verb's path already carries
its full `/api/v1/...` or `/api/v2/...` segment. A non-2xx response is
returned as an error carrying the status code and response body — nothing is
swallowed.

### `api_key` is optional — most self-hosted agents are open on the LAN

A stock Netdata agent listens on its own LAN with no authentication at all,
so `api_key` has no default and no requirement: when it is empty, **no**
`Authorization` header is sent. Set it when the agent sits behind a reverse
proxy that enforces a bearer token, or when pointing at a protected/relayed
endpoint (e.g. Netdata Cloud). Whenever it is set, every request carries it
identically, whichever verb you call.

### `insecure_skip_verify`: read this before enabling

Self-hosted Netdata agents commonly run behind a self-signed certificate on
a LAN. Setting this to `true` accepts that certificate — but it also accepts
**any** certificate, disabling all protection against a man-in-the-middle on
the path to your instance. Only enable it for instances reached over a
trusted network, and prefer installing a real certificate (e.g. via a
reverse proxy with ACME/Let's Encrypt) when possible.

## Verbs

Selected by `uses: <name>.<verb>`. Every verb's outputs include
`status_code`; the rest return either `result` (a single object) or `items`
(a bare JSON array, hoisted automatically). Required options are marked `*`.

### Agent info

- **`info`** — agent identity and build info. No options. → `result`,
  `status_code`.
- **`contexts`** — list every metric context the agent tracks. No options.
  → `result`, `status_code`.

### Charts & data

- **`charts`** — list every chart the agent collects. No options. →
  `result`, `status_code`.
- **`chart`** — get one chart's definition. `chart`* (chart id, e.g.
  `system.cpu`). → `result`, `status_code`.
- **`data`** — time-series data for one chart. `chart`* (chart id, e.g.
  `system.cpu`), `after` (integer, start time: unix seconds or a negative
  relative offset), `before` (integer, end time: unix seconds or a negative
  relative offset), `points` (integer, number of points to return),
  `dimensions` (list, restrict to these dimensions — joined into a single
  comma-separated `?dimensions=` value), `format` (enum `json` | `json2` |
  `csv` | `tsv` | `ssv` | `datatable` | `datasource` | `array` | `html`,
  default `json`). → `result`, `status_code`.

### Alarms

- **`alarms`** — currently configured alarms. `all` (boolean, return every
  configured alarm — `?all=true` — instead of only active/raised ones —
  `?active=true`, the default). → `result`, `status_code`.
- **`alarm_log`** — the alarm transition log; returns a bare JSON array. `after`
  (integer, unix timestamp; only entries after this time). → `items`,
  `status_code`.
- **`alerts`** — the newer v2 alerts API. No options. → `result`,
  `status_code`.

### Escape hatch

- **`api`** — raw escape hatch for any Netdata API endpoint without a
  first-class verb. `method` (HTTP method, default `GET`), `path`* (path
  under `base_url`, e.g. `/api/v1/info`), `query` (map, query string
  parameters), `body` (any, JSON request body). → `result` (object response)
  or `items` (array response), `status_code`.

## Source — the `alarm` event

Every `poll_interval` (default `1m`), the source calls
`GET /api/v1/alarms?active=true` — which returns
`{"alarms": {<name>: {status, value, ...}}}` — and emits **one `alarm` event
per alarm whose `status` is `WARNING` or `CRITICAL`**. An alarm that is
`CLEAR`, `UNDEFINED`, or any other status does not emit.

Events are **deduped on the alarm's name + status**, so an alarm that stays
raised emits once, not once per poll cycle — but a flap (a status change,
e.g. `WARNING` → `CRITICAL`) is a different dedup key and re-emits. A failed
poll backs off 30s rather than becoming a hot loop; the loop exits cleanly
when the daemon cancels it.

| context | type | notes |
|---------|------|-------|
| `name` | string | the alarm's short name (falls back to its `/api/v1/alarms` map key if absent) |
| `chart` | string | the chart the alarm is attached to, e.g. `system.cpu` |
| `family` | string | the chart's family, e.g. `cpu` |
| `status` | string | `WARNING` or `CRITICAL` (only these two ever emit) |
| `value` | number | the alarm's current calculated value |
| `units` | string | the value's units |
| `info` | string | the alarm's human-readable description |
| `last_status_change` | number | unix timestamp of the last status transition |
| `statuses` | string | alias of `status`, for the `statuses` filter |
| `charts` | string | alias of `chart`, for the `charts` filter |

| filter | type | matches |
|--------|------|---------|
| `statuses` | list | `status` is one of these (e.g. `WARNING`, `CRITICAL`) |
| `charts` | list | `chart` is one of these |

### Filtering

A trigger's `filter:`/`filters:` matches an event's published context fields
(the table above). The grammar:

- A key set to a value must match; a **list matches any of** its values —
  `statuses: [WARNING, CRITICAL]`.
- Prefix **`not_`** to negate a field — `not_charts: [system.cpu]` excludes.
- **`expr:` / `not_expr:`** take an expression over the fields —
  `expr: "status == 'CRITICAL'"`.
- Keys within one filter object are **AND**ed. A top-level **array** of
  filter objects is **OR** across them (one arm per rule).

```yaml
connectors:
  monitoring: { use: netdata, poll_interval: 30s }
triggers:
  - on: monitoring.alarm
    filters: { charts: [system.cpu, disk.space] }
    steps:
      - id: notify
        uses: ntfy.publish
        options:
          title: "{{.title}}"
          message: "{{.info}} ({{.status}}, value={{.value}}{{.units}})"
```

## Capabilities & security

Declares no `Egress` (Netdata is almost always self-hosted with no fixed
public host); narrow with `network:` on the connector instance to your own
agent host. Every request is a plain `net/http` call, optionally carrying a
Bearer token — there is no shelling out and no filesystem access.
