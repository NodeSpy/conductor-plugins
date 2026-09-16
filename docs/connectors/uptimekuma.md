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

## Connection

| key | type | purpose |
|-----|------|---------|
| `listen` | string | HTTP listener address, e.g. `:9096` |
| `path` | string | listener path (default `/uptimekuma`) |
| `secret` | string | shared token compared to `X-Conductor-Token` / `?token=` |
| `allow_unsigned` | bool | accept unauthenticated POSTs when no `secret` is set |

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
