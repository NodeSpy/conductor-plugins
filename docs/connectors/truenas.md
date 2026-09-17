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
```

## Setup

You'll end up with a TrueNAS SCALE API key and your instance's base URL.

**Prerequisites:** a running TrueNAS SCALE instance and admin access to it.

1. Log into the TrueNAS SCALE web UI.
2. Click the account icon in the top-right toolbar and choose **My API
   Keys** (or go to **Credentials → Users**, select your user, and click
   **View API Keys**).
3. Click **Add**, give the key a descriptive name, and click **Add** again
   to create it.
4. Copy the key from the confirmation dialog — it is shown only once.
5. Note your instance's base URL (e.g. `https://truenas.example.com`).

**Configure:**

```yaml
connectors:
  nas:
    use: truenas
    base_url: https://truenas.example.com
    api_key: ${TRUENAS_API_KEY}
    network: ["truenas.example.com:443"]
```

For the `alert` poll source, see **Source — the `alert` event** below.

```yaml
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

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb returns `status_code` (the HTTP status) plus either `result` (a
  single object) or `items` (a list) — **most TrueNAS list endpoints return a
  bare JSON array**, which is hoisted straight into `items`.
- `pool_id` / `dataset_id` / `uuid` / `service` options are **scoped**
  (`scope: pool|dataset|alert|service`), so conductor gates which resource an
  agent-driven dispatch may name.

Required options are marked `*`.

### System & storage

- **`system_info`** — system identity/version/hardware info (`GET /system/info`). → `result`.
- **`pools`** — list storage pools (`GET /pool`). → `items`.
- **`pool_get`** — get one pool's details (`GET /pool/id/{id}`). `pool_id`*. → `result`.
- **`datasets`** — list ZFS datasets (`GET /pool/dataset`). → `items`.
- **`dataset_get`** — get one dataset's details (`GET /pool/dataset/id/{id}`). `dataset_id`* (a ZFS dataset path, e.g. `tank/data` — **URL-encoded** on the wire, so `tank/data` becomes `.../pool/dataset/id/tank%2Fdata`). → `result`.

### Snapshots & replication

- **`snapshots`** — list ZFS snapshots (`GET /zfs/snapshot`). → `items`.
- **`snapshot_create`** — create a ZFS snapshot (`POST /zfs/snapshot`). `dataset`* (e.g. `tank/data`), `name`* (snapshot name). → `result`.
- **`replication`** — list replication tasks (`GET /replication`). → `items`.

### Apps, alerts & services

- **`apps`** — list SCALE apps (`GET /app`). → `items`.
- **`alerts`** — list current alerts (`GET /alert/list`). → `items`.
- **`alert_dismiss`** — dismiss an alert (`POST /alert/dismiss`). `uuid`*. → `result`. Unlike every other write verb here, the request body is **the bare uuid as a JSON string** (`"a1b2c3"`), not a `{uuid: ...}` object — that is what `/alert/dismiss` expects.
- **`services`** — list services and their running state (`GET /service`). → `items`.
- **`service_control`** — start or stop a service (`POST /service/start` or `/service/stop`). `service`* (e.g. `cifs`, `ssh`, `nfs`), `action`* (`start` | `stop`). → `result`.

### Escape hatch

- **`api`** — any TrueNAS API endpoint not covered above. `method` (HTTP method, default `GET`), `path`* (path under `/api/v2.0`, e.g. `/pool/dataset`), `query` (map of query string parameters), `body` (JSON request body). → `result` (or `items` when the response is a bare JSON array), `status_code`.

```yaml
uses: nas.dataset_get
options: { dataset_id: tank/data }
```

```yaml
uses: nas.service_control
options: { service: cifs, action: start }
# → POST /api/v2.0/service/start  {"service": "cifs"}
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

**Filtering:** each filter is a list; an alert matches when its corresponding
field is one of the listed values (OR within the list). Filters given
together are ANDed.

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
