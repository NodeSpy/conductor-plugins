# `pushover` connector

Pushover as a connector: send push notifications (including emergency
priority with delivery receipts), validate users/groups, list sounds, post
glances, and a generic API escape hatch — over `net/http`, form-encoded
per the Pushover REST API. Pushover is outbound only, so this is a
**verb-only** connector: no events, no source.

- **Kind:** connector (verbs only)
- **Source:** [`connectors/pushover/main.go`](../../connectors/pushover/main.go)
- **Provides:** `pushover`
- **Capabilities:** egress to `api.pushover.net:443` only; spawns nothing.
- **Bundled in conductor?** No — never in core. Add it here.

```yaml
connectors:
  notify:
    use: pushover
    token: ${PUSHOVER_APP_TOKEN}
    user: ${PUSHOVER_USER_KEY}
triggers:
  - on: deploy.failed
    steps:
      - uses: notify.send
        options:
          message: "deploy failed: {{.error}}"
          title: "CI"
          priority: 1
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `token` | string | Pushover application API token (required) |
| `user` | string | Pushover user or group key (required) |
| `api_base` | string | override the Pushover API base URL (tests, or a private gateway) |

`token` is included on every request a first-class verb builds. `user` is
included only where the endpoint needs the connection's own user/group —
`send` and `glances`; `validate_user` takes its own `user` option instead
(so it can validate any user or group, not just the connection's own), and
`get_receipt`/`cancel_receipt`/`sounds` need neither.

## Verbs

| verb | purpose |
|------|---------|
| `send` | send a push notification (`message`, `title`, `priority`, `url`, `url_title`, `sound`, `device`, `html`, `monospace`, `timestamp`, `retry`, `expire`, `tags`) |
| `validate_user` | validate a user or group key (and optional `device`) |
| `get_receipt` | check the delivery status of an emergency-priority (priority 2) notification by `receipt` |
| `cancel_receipt` | cancel further retries of an emergency-priority notification by `receipt` |
| `sounds` | list the notification sound names Pushover supports |
| `glances` | post a glance update (`title`, `text`, `subtext`, `count`, `percent`, `device`) |
| `api` | escape hatch: `method` + `path` (relative to the API base), `params` (form-encoded for POST, query string for GET) — token/user are **not** auto-included here; add them via `params` if the endpoint needs them |

Every verb's HTTP outcome is decoded as-is into `result` (the raw parsed JSON
response) plus `status_code`. A non-2xx response from Pushover surfaces as a
plugin error carrying the HTTP status and response body, not partial output.

### Emergency priority (priority 2)

`priority: 2` requires both `retry` (seconds between retries) and `expire`
(seconds until Pushover stops retrying) — `send` validates this itself and
refuses the call before any request is made if either is missing. The
receipt used by `get_receipt`/`cancel_receipt` is already present in `send`'s
own `result` for a priority-2 message; no extra hoisting is needed.

## Capabilities & security

Declares egress to `api.pushover.net:443` only, and spawns nothing. Narrow it
per instance with `network:`; it can never be widened past the declaration.
