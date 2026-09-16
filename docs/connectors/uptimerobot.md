# `uptimerobot` connector

UptimeRobot as a connector: monitor CRUD (create/edit/pause/resume/delete),
account and alert-contact reads, and a generic API escape hatch, over the v2
REST API (`application/x-www-form-urlencoded` POSTs). As a **source**, it
receives UptimeRobot "Web-Hook" alert-contact deliveries and streams a
normalized `alert` event per monitor up/down transition.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/uptimerobot/main.go`](../../connectors/uptimerobot/main.go)
- **Provides:** `uptimerobot`
- **Capabilities:** egress to `api.uptimerobot.com:443` only; spawns nothing.
- **Bundled in conductor?** No — never in core. Add it here.

```yaml
connectors:
  uptime:
    use: uptimerobot
    api_key: ${UPTIMEROBOT_API_KEY}
    webhook:
      listen: ":9096"
      secret: ${UPTIMEROBOT_WEBHOOK_TOKEN}
triggers:
  - on: uptime.alert
    filters: { alert_types: [down] }
    steps:
      - uses: uptime.get_monitors
        options: { monitors: ["{{.monitor_id}}"] }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_key` | string | UptimeRobot API key — the main key, or a monitor-specific key (required) |
| `api_base` | string | override the API base URL (default `https://api.uptimerobot.com/v2`; tests or a private gateway) |
| `webhook` | map | source transport: `listen`, `path`, `secret`, `allow_unsigned`, `smee` |

### `webhook`

| key | type | purpose |
|-----|------|---------|
| `listen` | string | HTTP listener address, e.g. `:9096` |
| `path` | string | listener path (default `/uptimerobot`) |
| `secret` | string | shared token compared to `X-Conductor-Token` / `?token=` |
| `allow_unsigned` | boolean | accept unauthenticated POSTs when no `secret` is set |
| `smee` | string | optional smee.io-style SSE relay URL — receive forwarded deliveries when the listener has no public URL; the shared token is still checked |

## UptimeRobot v2 API conventions

Every verb POSTs `application/x-www-form-urlencoded` to `{api_base}/{method}`
with `api_key` and `format=json` always included. UptimeRobot always answers
with HTTP 200 and a JSON body carrying `"stat": "ok"` or `"stat": "fail"` — a
provider-level failure is signaled in the *body*, not the status line. This
connector treats **either** a non-2xx HTTP status **or** a 200 response with
`"stat":"fail"` as a plugin error carrying the failure message plus the raw
response body.

## Setting up the alert Web-Hook

UptimeRobot cannot sign its Web-Hook deliveries — the POST value template is
entirely operator-defined plain text. Add an **Alert Contact** of type
**Web-Hook** in the UptimeRobot dashboard, pointed at this listener, with a
POST value template built from UptimeRobot's variables:

```
monitorID=*monitorID*&monitorURL=*monitorURL*&monitorFriendlyName=*monitorFriendlyName*&alertType=*alertType*&alertDetails=*alertDetails*&alertDateTime=*alertDateTime*
```

The equivalent JSON body works too (set the alert contact's "POST value" to
JSON and the `Content-Type` accordingly, or simply send the same variables as
JSON fields) — this connector tries JSON first and falls back to
form-urlencoded parsing, so either shape is accepted without configuration.

Since there is no signature to verify, authenticate with a **shared token**:
paste it into the webhook URL's `?token=` query parameter (UptimeRobot's
webhook URL field accepts an arbitrary URL) or, if you front the listener with
something that lets you add a header, send it as `X-Conductor-Token`. Set
`webhook.secret` to the same value.

> **Unauthenticated listeners fail closed.** With no `webhook.secret`, the
> listener would accept any POST as a real alert. Set it, or set
> `webhook.allow_unsigned: true` to opt in explicitly (e.g. the listener sits
> behind something else that authenticates, such as a private network or a
> reverse proxy).

## Source events

Trigger with `on: <name>.alert`. `alertType` `1` maps to `down`, `2` to `up`;
any other value passes through as-is. Deduplicated on `monitorID` +
`alertType` + `alertDateTime`, so a redelivery is dropped but a genuine
down→up transition (or a later, distinct alert) is not.

| event | fires when |
|-------|-----------|
| `alert` | a monitor transitioned up or down (per the Web-Hook alert contact delivery) |

**Context:** `alert_type` (`up`/`down`), `monitor_id`, `monitor_url`,
`monitor_name`, `alert_details`, `datetime`.

**Filters** (evaluated by the daemon's generic list-contains evaluator):
`alert_types` (list), `monitors` (list — matches either `monitor_id` or
`monitor_name`), and the scalar forms `alert_type`, `monitor_name`.

## Verbs

| verb | purpose |
|------|---------|
| `get_monitors` | list monitors, optionally filtered by `monitors` (ids), `statuses`, `types`, `search`, paginated with `limit`/`offset`, optionally with `logs` → `items` (the `monitors` array) |
| `new_monitor` | create a monitor: `friendly_name`, `url`, `type` (1=http, 2=keyword, 3=ping, 4=port) required; `interval`, `sub_type`, `port`, `keyword_type`, `keyword_value` optional |
| `edit_monitor` | edit a monitor's `friendly_name`/`url`/`interval`/`status` by `id` |
| `delete_monitor` | permanently delete a monitor by `id` |
| `pause_monitor` | pause a monitor (`edit_monitor` with `status=0`) |
| `resume_monitor` | resume a paused monitor (`edit_monitor` with `status=1`) |
| `get_account_details` | read the account's limits/usage |
| `get_alert_contacts` | list the account's alert contacts → `items` |
| `api` | escape hatch: `method` (the API path segment, e.g. `getMSPUpdates`) + `params` (form-encoded) |

Every verb's response is decoded into `result` (the raw parsed JSON body) plus
`status_code`; `get_monitors` and `get_alert_contacts` additionally hoist
their respective arrays to a top-level `items` output. A non-2xx HTTP status
or a 200 with `"stat":"fail"` surfaces as a plugin error carrying the failure
message and the raw response body, not partial output.

## Capabilities & security

Declares egress to `api.uptimerobot.com:443` only, and spawns nothing. Narrow
it per instance with `network:`; it can never be widened past the
declaration. Use a monitor-specific API key rather than the main key when a
connector instance only needs to act on a subset of monitors.
