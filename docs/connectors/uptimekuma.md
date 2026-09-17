# `uptimekuma` connector

A **source-only** connector: it listens for Uptime Kuma's webhook
notification (fired on every monitor heartbeat status change), checks an
optional shared token, and streams a normalized `monitor` event to the
daemon.

- **Kind:** connector (source only — no verbs)
- **Source:** [`connectors/uptimekuma/main.go`](../../connectors/uptimekuma/main.go)
- **Provides:** `uptimekuma`
- **Capabilities:** empty manifest — it only listens, never dials out, and
  spawns nothing.
- **Why source-only:** Uptime Kuma has no stable REST API — its dashboard
  talks socket.io, which this plugin does not speak — so the webhook
  notification is the only integration surface available.

```yaml
connectors:
  myuptimekuma:
    use: uptimekuma
    listen: ":9096"
    secret: ${UPTIMEKUMA_TOKEN}
triggers:
  - on: myuptimekuma.monitor
    filters: { statuses: [down] }
    steps: [ ... ]
```

In Uptime Kuma, add a **Webhook** notification pointed at
`http://<listen>/uptimekuma` (or `?token=<secret>` appended to the URL if you
cannot set a custom header) and attach it to the monitors you want to watch.

## Setup

Uptime Kuma has no API token — its dashboard configures notifications
directly.

**Prerequisites:** admin access to the Uptime Kuma instance.

1. Go to **Settings → Notifications → Add New Notification**.
2. Set **Notification Type** to **Webhook**, set **Post URL** to
   `http://<host>:<port>/uptimekuma` (append `?token=<secret>` if you can't
   set a custom header, otherwise add `X-Conductor-Token: <secret>` under
   **Custom Headers**), and save.
3. On each monitor to forward, open **Edit → Notifications** and enable the
   new Webhook notification (or toggle **Default enabled** so new monitors
   pick it up automatically).

```yaml
connectors:
  myuptimekuma:
    use: uptimekuma
    listen: ":9096"
    secret: ${UPTIMEKUMA_TOKEN}
```

No public URL? Set `smee: https://smee.io/<channel>` instead of (or
alongside) `listen`. See **Events** below for the `monitor` payload and
status mapping.

## Connection

| key | type | purpose |
|-----|------|---------|
| `listen` | string | HTTP listener address, e.g. `:9096` |
| `path` | string | listener path (default `/uptimekuma`) |
| `secret` | string | shared token compared to `X-Conductor-Token` / `?token=` |
| `allow_unsigned` | bool | accept unauthenticated POSTs when no `secret` is set |
| `smee` | string | optional smee.io-style SSE relay URL — receive forwarded deliveries when the listener has no public URL; the shared token is still checked |

> **Unsigned listeners fail closed.** Uptime Kuma cannot sign its webhook
> notifications — there is no HMAC, only whatever token the operator embeds
> in the URL or a custom header. With no `secret`, the listener would accept
> any POST on the address as a real event. Set the secret, or set
> `allow_unsigned: true` to opt in explicitly when something else
> authenticates the endpoint.

## Events

| event | fires when |
|-------|-----------|
| `monitor` | an Uptime Kuma monitor heartbeat notification (up/down/pending/maintenance) |

**Status mapping** (`heartbeat.status` → `status`): `0` → `down`, `1` → `up`,
`2` → `pending`, `3` → `maintenance`.

**Context** (templated flat, e.g. `{{.status}}`, `{{.monitor_name}}`):
`status`, `monitor_name`, `monitor_url`, `monitor_type`, `monitor_id`, `msg`,
`important`, `time`.

`title` is the webhook's own `msg` when present, otherwise
`"<monitor_name> is <status>"`.

Uptime Kuma's default webhook body is `{heartbeat: {...}, monitor: {...},
msg: "..."}`; some custom notification templates only ever send a flat
`{"msg": "..."}` with no nested objects — this connector still emits an
event in that case, with the structured fields left empty.

**Filters** (evaluated by the daemon's generic list-contains evaluator):
`statuses`, `monitors`, `monitor_types` (list forms), and `status`,
`monitor`, `monitor_type` (scalar forms — `monitor` matches `monitor_name`).

**Dedup:** monitor ID + status + time, so a monitor re-firing the same status
later is treated as a new event, but a redelivered webhook for the same
heartbeat is not.
