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
```

## Setup

You'll end up with a Proxmox API token id + secret scoped to a dedicated
user.

**Prerequisites:** a running Proxmox VE instance and admin access to it.

1. In the Proxmox web UI, go to **Datacenter → Permissions → Users** and add
   a dedicated user, if you don't want to reuse an existing one.
2. Go to **Datacenter → Permissions → API Tokens → Add**.
3. Pick the user (e.g. `root@pam`), give the token an ID (e.g. `conductor`),
   and leave **Privilege Separation** checked so the token gets its own role
   rather than inheriting the user's full permissions.
4. Click **Add** — the **Secret** is shown once. Copy it now; Proxmox never
   shows it again.
5. Grant the token a role: **Datacenter → Permissions → Add**, targeting the
   `user@realm!tokenid` path with a role (e.g. `PVEAuditor`, or a custom
   least-privilege role).

**Configure:**

```yaml
connectors:
  pve:
    use: proxmox
    base_url: https://pve.example.com:8006
    token_id: ${PVE_TOKEN_ID}       # "USER@REALM!TOKENID"
    token_secret: ${PVE_TOKEN_SECRET}
    network: ["pve.example.com:8006"]
```

For the `task` poll source (backup/clone/migration completion events), see
**Source — the `task` event** below.

```yaml
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

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- **`node`** — Proxmox node name (e.g. `pve1`); required on every node-scoped verb.
- **`vmid`** — VM/container ID; required on every per-VM/container verb.
- Every verb returns `status_code` (the HTTP status) plus either `result` (a
  single object) or `items` (a list).
- `node`, `vmid`, and `upid` are **scoped** options (`node`, `vm`, `task`
  respectively), so conductor gates which resource an agent-driven dispatch
  may name.
- Every `qemu_start`/`qemu_stop`/`qemu_shutdown`/`qemu_reboot`/`qemu_clone`/
  `lxc_start`/`lxc_stop`/`backup`/`snapshot_create` verb **starts** the
  underlying Proxmox task and returns immediately with the task's UPID as
  `result` — these operations run asynchronously on the PVE side. Poll
  `task_status` with that UPID, or wait for the `task` source event below, to
  see the outcome.

Required options are marked `*`.

### Nodes & cluster

- **`nodes`** — list cluster nodes (`GET /nodes`). → `items`.
- **`node_status`** — a node's status: uptime, load, memory, ... (`GET /nodes/{node}/status`). `node`*. → `result`.
- **`cluster_resources`** — cluster-wide resource list (`GET /cluster/resources`). `type` (`vm` | `node` | `storage` — restrict to one resource type). → `items`.

### QEMU VMs

- **`qemu_list`** — list QEMU VMs on a node (`GET /nodes/{node}/qemu`). `node`*. → `items`.
- **`qemu_status`** — a QEMU VM's current status (`GET .../qemu/{vmid}/status/current`). `node`*, `vmid`*. → `result`.
- **`qemu_start`** — start a QEMU VM (`POST .../qemu/{vmid}/status/start`). `node`*, `vmid`*. → `result` (task UPID).
- **`qemu_stop`** — hard-stop a QEMU VM (`POST .../qemu/{vmid}/status/stop`). `node`*, `vmid`*. → `result` (task UPID).
- **`qemu_shutdown`** — gracefully shut down a QEMU VM via ACPI (`POST .../qemu/{vmid}/status/shutdown`). `node`*, `vmid`*. → `result` (task UPID).
- **`qemu_reboot`** — reboot a QEMU VM (`POST .../qemu/{vmid}/status/reboot`). `node`*, `vmid`*. → `result` (task UPID).
- **`qemu_clone`** — clone a QEMU VM/template (`POST .../qemu/{vmid}/clone`). `node`*, `vmid`*, `newid`* (VMID for the clone), `name` (name for the clone), `full` (boolean — full clone instead of a linked clone). → `result` (task UPID).

### LXC containers

- **`lxc_list`** — list LXC containers on a node (`GET /nodes/{node}/lxc`). `node`*. → `items`.
- **`lxc_status`** — an LXC container's current status (`GET .../lxc/{vmid}/status/current`). `node`*, `vmid`*. → `result`.
- **`lxc_start`** — start an LXC container (`POST .../lxc/{vmid}/status/start`). `node`*, `vmid`*. → `result` (task UPID).
- **`lxc_stop`** — hard-stop an LXC container (`POST .../lxc/{vmid}/status/stop`). `node`*, `vmid`*. → `result` (task UPID).

### Storage, tasks & backups

- **`storage`** — storage configured on a node (`GET /nodes/{node}/storage`). `node`*. → `items`.
- **`tasks`** — recent tasks on a node (`GET /nodes/{node}/tasks`). `node`*. → `items`.
- **`task_status`** — a task's status by UPID (`GET /nodes/{node}/tasks/{upid}/status`). `node`*, `upid`* (from `tasks`, or a `*_start`/`clone`/`backup`/`snapshot_create` output). → `result`.
- **`backup`** — start a vzdump backup job (`POST /nodes/{node}/vzdump`). `node`*, `vmid`*, `storage` (target storage ID), `mode` (`snapshot` | `suspend` | `stop`, default `snapshot`), `compress` (`0` | `1` | `gzip` | `lzo` | `zstd`). → `result` (task UPID).
- **`snapshots`** — list a QEMU VM's snapshots (`GET .../qemu/{vmid}/snapshot`). `node`*, `vmid`*. → `items`.
- **`snapshot_create`** — create a QEMU VM snapshot (`POST .../qemu/{vmid}/snapshot`). `node`*, `vmid`*, `snapname`* (snapshot name). → `result`.

### Escape hatch

- **`api`** — any Proxmox API endpoint not covered above. `method` (HTTP method, default `GET`), `path`* (path under `/api2/json`, e.g. `/nodes/pve1/qemu/100/config`), `query` (map of query string parameters), `body` (JSON request body). → `result` (or `items` when the unwrapped `data` is a JSON array), `status_code`.
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

**Filtering:** each filter is a list; a task matches when its corresponding
field is one of the listed values (OR within the list). Filters given
together are ANDed.

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
