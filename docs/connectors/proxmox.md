# `proxmox` connector

Drive a self-hosted [Proxmox VE](https://www.proxmox.com/en/proxmox-virtual-environment)
(homelab hypervisor) instance over its REST API: nodes, cluster-wide
resources, QEMU VMs (list/status/start/stop/shutdown/reboot/clone), LXC
containers (list/status/start/stop), storage, tasks, backups (`vzdump`),
snapshots, and a generic `api` escape hatch for any endpoint a first-class
verb does not cover. As a **poll source**, it watches `/cluster/tasks` and
emits a `task` event whenever a task finishes — a backup, migration, clone,
... — so a workflow can trigger on the outcome of one.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/proxmox/main.go`](../../connectors/proxmox/main.go)
- **Provides:** `proxmox`
- **Capabilities:** no declared egress (self-hosted; the operator narrows
  `network:` to their own instance)

```yaml
connectors:
  pve:
    use: proxmox
    base_url: https://pve.example.com:8006
    token_id: ${PVE_TOKEN_ID}       # "USER@REALM!TOKENID"
    token_secret: ${PVE_TOKEN_SECRET}
    insecure_skip_verify: true       # self-signed cert on the homelab box
    poll_interval: 1m
    network: ["pve.example.com:8006"]   # narrow the (empty) declared egress

triggers:
  - on: pve.task
    filters: { types: [vzdump], statuses: [OK] }
    steps:
      - uses: ntfy.publish
        options: { title: "backup finished", message: "{{.title}}" }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | **required.** Proxmox VE API root, e.g. `https://pve.example.com:8006` |
| `token_id` | string | **required.** API token ID, `USER@REALM!TOKENID` (Datacenter > Permissions > API Tokens) |
| `token_secret` | string | **required.** API token secret — the UUID shown once at token creation |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) — see below |
| `poll_interval` | duration | **source** poll period (default `30s`) |

Every verb is a plain HTTP call to `{base_url}/api2/json/<path>`,
authenticated with the
`Authorization: PVEAPIToken=<token_id>=<token_secret>` header. A non-2xx
response is surfaced as a plugin error carrying the status code and response
body. Proxmox wraps every response as `{"data": ...}`; this connector
unwraps `data` for you into `result` (a single object) or `items` (a list).

### `insecure_skip_verify` — read this before setting it

Proxmox VE ships with a **self-signed certificate** by default, which a
normal TLS client will reject. Setting `insecure_skip_verify: true` disables
certificate verification **entirely** — a network position able to
intercept traffic to `base_url` can impersonate your PVE host undetected,
with nothing tying the connection to your real server. Only set it when
`base_url` is reachable exclusively over a network you already trust (a LAN
or VPN, not the open internet), and prefer installing a real certificate
instead where you can — Proxmox has built-in ACME support (Datacenter >
ACME) that can get you a Let's Encrypt (or other ACME CA) certificate even
for a private hostname, via a DNS-01 challenge.

## Verbs

| verb | request | outputs | notes |
|------|---------|---------|-------|
| `nodes` | `GET /nodes` | `items` | cluster nodes |
| `node_status` | `GET /nodes/{node}/status` | `result` | options: `node` * |
| `cluster_resources` | `GET /cluster/resources` | `items` | options: `type` (`vm`\|`node`\|`storage`) |
| `qemu_list` | `GET /nodes/{node}/qemu` | `items` | options: `node` * |
| `qemu_status` | `GET /nodes/{node}/qemu/{vmid}/status/current` | `result` | options: `node` *, `vmid` * |
| `qemu_start` | `POST .../qemu/{vmid}/status/start` | `result` | options: `node` *, `vmid` * |
| `qemu_stop` | `POST .../qemu/{vmid}/status/stop` | `result` | options: `node` *, `vmid` * |
| `qemu_shutdown` | `POST .../qemu/{vmid}/status/shutdown` | `result` | options: `node` *, `vmid` * (graceful, via ACPI) |
| `qemu_reboot` | `POST .../qemu/{vmid}/status/reboot` | `result` | options: `node` *, `vmid` * |
| `qemu_clone` | `POST .../qemu/{vmid}/clone` | `result` | options: `node` *, `vmid` *, `newid` *, `name`, `full` |
| `lxc_list` | `GET /nodes/{node}/lxc` | `items` | options: `node` * |
| `lxc_status` | `GET /nodes/{node}/lxc/{vmid}/status/current` | `result` | options: `node` *, `vmid` * |
| `lxc_start` | `POST .../lxc/{vmid}/status/start` | `result` | options: `node` *, `vmid` * |
| `lxc_stop` | `POST .../lxc/{vmid}/status/stop` | `result` | options: `node` *, `vmid` * |
| `storage` | `GET /nodes/{node}/storage` | `items` | options: `node` * |
| `tasks` | `GET /nodes/{node}/tasks` | `items` | options: `node` * |
| `task_status` | `GET /nodes/{node}/tasks/{upid}/status` | `result` | options: `node` *, `upid` * |
| `backup` | `POST /nodes/{node}/vzdump` | `result` | options: `node` *, `vmid` *, `storage`, `mode` (`snapshot`\|`suspend`\|`stop`), `compress` |
| `snapshots` | `GET /nodes/{node}/qemu/{vmid}/snapshot` | `items` | options: `node` *, `vmid` * |
| `snapshot_create` | `POST .../qemu/{vmid}/snapshot` | `result` | options: `node` *, `vmid` *, `snapname` * |
| `api` | any `method`/`path`/`query`/`body` | `result` (+ `items` when the unwrapped `data` is a JSON array) | escape hatch for any endpoint not covered above |

`*` required. Every verb also returns `status_code` (the HTTP status).
`node`, `vmid`, and `upid` are **scoped** options (`node`, `vm`, `task`
respectively), so conductor gates which resource an agent-driven dispatch
may name.

Every `*_start`/`*_stop`/`*_shutdown`/`*_reboot`/`clone`/`backup`/
`snapshot_create` verb **starts** the underlying Proxmox task and returns
immediately with the task's UPID as `result` — Proxmox operations like a
backup or a clone run asynchronously. Poll `task_status` with that UPID (or
wait for the `task` source event, below) to see the outcome.

See `Describe()` in
[`connectors/proxmox/main.go`](../../connectors/proxmox/main.go) for each
verb's full option schema.

## Source — the `task` event

Every `poll_interval`, the source calls `GET /cluster/tasks` and emits **one
`task` event for every task that has STOPPED** since it was last polled: one
with a positive `endtime`, or a `status` other than `"running"` (Proxmox
omits `status` entirely while a task is still in flight). A task still
running does not emit.

- Events are **deduped on `upid`**, so a task already reported does not fire
  again on a later cycle.
- A task with no `upid` at all is dropped — there is nothing to identify or
  dedup it by.
- `exitstatus` is the task's outcome string (e.g. `OK`, or an error message);
  it falls back to the raw `status` field when Proxmox reports the outcome
  there instead.
- A failed poll (PVE unreachable, auth rejected, ...) backs off 10s rather
  than becoming a hot loop; the loop exits cleanly when the daemon cancels
  it.

| context | type | notes |
|---------|------|-------|
| `node` | string | the node the task ran on |
| `upid` | string | the task's UPID |
| `type` | string | task type, e.g. `vzdump`, `qmclone`, `qmigrate` |
| `id` | string | the task's `id` field (often the vmid, as a string) |
| `user` | string | the user/token that started the task |
| `status` | string | the raw `status` field Proxmox reported, if any |
| `exitstatus` | string | the outcome (e.g. `OK`); falls back to `status` |
| `starttime` | integer | unix timestamp |
| `endtime` | integer | unix timestamp |

| filter | type | matches |
|--------|------|---------|
| `nodes` | list | the task's node is one of these |
| `types` | list | the task's type is one of these |
| `statuses` | list | the task's `exitstatus` is one of these |

```yaml
connectors:
  pve: { use: proxmox, base_url: https://pve.example.com:8006, token_id: ..., token_secret: ..., poll_interval: 30s }
triggers:
  - on: pve.task
    filters: { types: [vzdump] }
    steps:
      - id: notify
        uses: ntfy.publish
        options:
          title: "{{.title}}"
          message: "{{.type}} on {{.node}} — {{.exitstatus}}"
```

## Capabilities & security

Declares **no** egress — Proxmox is always self-hosted, so unlike a
connector with a fixed public hostname, there is no address this plugin can
declare on the operator's behalf. Set `network:` on the connector instance
to your own PVE host (`host:port`) to scope its egress.

- Prefer a **dedicated API token** (Datacenter > Permissions > API Tokens)
  scoped to only the privileges this connector instance needs, over reusing
  a full-access user's token.
- Review `insecure_skip_verify` above before enabling it — it disables TLS
  verification entirely, not just hostname checking.
