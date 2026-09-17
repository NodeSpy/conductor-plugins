# `truenas` connector

TrueNAS SCALE as a connector: system info, storage pools, ZFS datasets and
snapshots, replication tasks, SCALE apps, alerts, and services over the SCALE
middleware REST API (`/api/v2.0`), plus a raw `api` escape hatch and a **poll
source** that emits an `alert` event for every active (not dismissed) alert.
Built on the standard library's `net/http` only.

- **Kind:** connector (verbs + source)
- **Source:** [`connectors/truenas/main.go`](../../connectors/truenas/main.go)
- **Provides:** `truenas`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your
  own instance)

```yaml
connectors:
  nas:
    use: truenas
    base_url: https://truenas.example.com
    api_key: ${TRUENAS_API_KEY}
    insecure_skip_verify: true   # common with a self-signed SCALE web UI cert
    poll_interval: 1m
    network: ["truenas.example.com:443"]   # narrow the declared egress to your instance
triggers:
  - on: nas.alert
    filters: { levels: [WARNING, CRITICAL] }
    steps:
      - id: notify
        uses: ntfy.publish
        options: { title: "{{.title}}", message: "{{.formatted}}" }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | TrueNAS SCALE root, e.g. `https://truenas.example.com` |
| `api_key` | string | TrueNAS API key, sent as `Authorization: Bearer <api_key>` |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) |
| `poll_interval` | duration | alert-source poll period (default `1m`) |

Every verb calls `base_url + "/api/v2.0" + <endpoint>`. A non-2xx response is
returned as an error carrying the status code and response body — nothing is
swallowed.

### `insecure_skip_verify`: read this before you set it

Self-signed certificates are the norm on a home-lab TrueNAS box, so
`insecure_skip_verify: true` is provided to unblock that case. Setting it
disables TLS certificate verification for **that connector instance** — the
connection is no longer protected against a man-in-the-middle on the network
path to your NAS. Prefer trusting the box's real CA certificate (or its
Let's Encrypt cert, if the SCALE UI is behind one) at the OS/daemon level
instead, when that is practical. Where it is not, this flag exists so a
self-signed lab box doesn't force you to disable TLS globally.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; most also return
either `result` (a single object) or `items` (a list — **many TrueNAS list
endpoints return a bare JSON array**, which is hoisted straight into `items`).

| verb | endpoint | outputs |
|------|----------|---------|
| `system_info` | `GET /system/info` | `result` |
| `pools` | `GET /pool` | `items` |
| `pool_get` | `GET /pool/id/{id}` | `result` |
| `datasets` | `GET /pool/dataset` | `items` |
| `dataset_get` | `GET /pool/dataset/id/{id}` | `result` |
| `snapshots` | `GET /zfs/snapshot` | `items` |
| `snapshot_create` | `POST /zfs/snapshot` | `result` |
| `replication` | `GET /replication` | `items` |
| `apps` | `GET /app` | `items` |
| `alerts` | `GET /alert/list` | `items` |
| `alert_dismiss` | `POST /alert/dismiss` | `result` |
| `services` | `GET /service` | `items` |
| `service_control` | `POST /service/start` or `/service/stop` | `result` |
| `api` | any `/api/v2.0` path | `result` or `items` |

### `pool_get` / `dataset_get` — resource ids

`pool_get` takes `pool_id` *. `dataset_get` takes `dataset_id` * — a ZFS
dataset path such as `tank/data` — and **URL-encodes it** before building the
path, so `tank/data` becomes `.../pool/dataset/id/tank%2Fdata` on the wire, as
the middleware's route requires.

```yaml
uses: nas.dataset_get
options: { dataset_id: tank/data }
```

### `snapshot_create` — take a ZFS snapshot

`dataset` * (e.g. `tank/data`), `name` * (the snapshot name).

```yaml
uses: nas.snapshot_create
options: { dataset: tank/data, name: pre-upgrade }
```

### `alert_dismiss` — dismiss one alert

`uuid` *. Unlike every other write verb here, the request body is **the bare
uuid as a JSON string** (`"a1b2c3"`), not a `{uuid: ...}` object — that is
what `/alert/dismiss` expects.

### `service_control` — start or stop a service

`service` * (e.g. `cifs`, `ssh`, `nfs`), `action` * (`start` or `stop`).

```yaml
uses: nas.service_control
options: { service: cifs, action: start }
# → POST /api/v2.0/service/start  {"service": "cifs"}
```

### `api` — raw escape hatch

`method` (default `GET`), `path` * (under `/api/v2.0`), `query` (map), `body`
(any JSON). A bare-array response is hoisted into `items`; anything else
lands in `result`.

```yaml
uses: nas.api
options: { path: /pool, query: { limit: "1" } }
```

## Source — the `alert` event

Every `poll_interval`, the source runs `GET /alert/list` and emits **one
`alert` event for each alert that is NOT dismissed**.

- Dedup is on the alert's `uuid` (falling back to `id`), so an alert that
  stays active for days emits once, not once per poll cycle.
- An alert object with no `uuid`/`id` at all is skipped — there is no stable
  identity to dedup or reference by.
- A failed poll (unreachable NAS, revoked API key) backs off 30s rather than
  becoming a hot loop; the loop exits cleanly when the daemon cancels it.

| context | type | notes |
|---------|------|-------|
| `id` | string | the alert's uuid/id |
| `uuid` | string | same as `id` |
| `level` | string | `INFO` / `WARNING` / `CRITICAL` / ... |
| `klass` | string | the alert class |
| `formatted` | string | the formatted alert message |
| `dismissed` | boolean | always `false` (the event only fires while active) |
| `datetime` | string | when the alert was raised |
| `node` | string | the cluster node that raised it (HA systems) |

| filter | type | matches |
|--------|------|---------|
| `levels` | list | `level` is one of these |
| `klasses` | list | `klass` is one of these |

```yaml
connectors:
  nas: { use: truenas, base_url: https://truenas.example.com, api_key: ${TRUENAS_API_KEY}, poll_interval: 2m }
triggers:
  - on: nas.alert
    filters: { levels: [CRITICAL] }
    steps:
      - id: page
        uses: pd.trigger
        options: { summary: "{{.title}}" }
```

## Capabilities & security

Declares egress `[]` (self-hosted; narrow with `network:` to your instance).

- The API key is sent as a Bearer credential on every request; it is never
  logged.
- `insecure_skip_verify` is opt-in per connector instance — see above. It
  never applies process-wide, only to the instance that set it.
- `pool_id` / `dataset_id` / `uuid` / `service` options are **scoped**
  (`scope: pool|dataset|alert|service`), so conductor gates which resource an
  agent-driven dispatch may name.
