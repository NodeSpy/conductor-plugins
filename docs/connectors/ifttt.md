# `ifttt` connector

IFTTT [Maker Webhooks](https://ifttt.com/maker_webhooks) as a connector: two
**verbs** to fire an applet (the classic `value1`/`value2`/`value3` shape, or
an arbitrary JSON body), plus a **source** that receives inbound webhook POSTs
from an applet's "Make a web request" action.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/ifttt/main.go`](../../connectors/ifttt/main.go)
- **Provides:** `ifttt`
- **Capabilities:** egress to `maker.ifttt.com:443` only
- **Bundled in conductor?** No. This plugin is the only way to get an ifttt
  connector.

```yaml
connectors:
  ifttt:
    use: ifttt
    key: ${IFTTT_MAKER_KEY}
    webhook:
      listen: ":9100"
      secret: ${IFTTT_WEBHOOK_TOKEN}
triggers:
  - on: ifttt.event
    filters: { events: [door_opened] }
    steps:
      - uses: ifttt.trigger
        options: { event: notify_me, value1: "front door opened" }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `key` | string (required) | your IFTTT Maker Webhooks key |
| `base_url` | string | override the Maker Webhooks base URL (default `https://maker.ifttt.com`; for tests) |
| `webhook` | map | source transport: `listen`, `path` (default `/ifttt`), `secret`, `allow_unsigned` |

## Verbs

| verb | options | outputs | does |
|------|---------|---------|------|
| `trigger` | `event*`, `value1`, `value2`, `value3` | `result`, `status_code` | `POST /trigger/{event}/with/key/{key}` with `{value1,value2,value3}` (empty values omitted) |
| `trigger_json` | `event*`, `data*` (map) | `result`, `status_code` | `POST /trigger/{event}/json/with/key/{key}` with `data` as the request body verbatim |

A non-2xx response from IFTTT fails the verb with the status code and
response body in the error message.

## Source

`webhook.listen` runs an HTTP listener that receives an applet's "Make a web
request" POST and streams one normalized `event` event per delivery.

Maker Webhooks has no signing story — there's no HMAC to verify — so
authentication is a **shared token** you pick and configure the applet to
send back, checked in constant time against `webhook.secret`:

- as the `X-Conductor-Token` header (preferred: configure it under the
  applet's request "Additional Headers"), or
- as a `?token=` query parameter (when the applet only lets you set a URL).

> **Unsigned listeners fail closed.** With no `webhook.secret`, the listener
> refuses to start — any POST reaching the address would otherwise be
> accepted as a real event. Set it, or set `webhook.allow_unsigned: true` to
> opt in explicitly (e.g. when something else in front of the listener
> already authenticates the request).

### Event

| event | fires when |
|-------|-----------|
| `event` | an IFTTT applet posted a webhook event |

**Context:** every top-level key of the posted JSON body, spread in directly,
plus `payload` (the whole parsed object) and `event` (when the JSON carries a
top-level `event` field). A non-JSON body is carried instead as a single
`body` string key. A delivery whose JSON carries a top-level `id` is
deduplicated on it, so a retried delivery doesn't fire twice.

**Filters** (list-contains): `events` (event names to match; empty = any).
`event` is also accepted as a scalar alias.

```yaml
triggers:
  - on: ifttt.event
    filters: { events: [door_opened, motion_detected] }
    steps: [ ... ]
```

## Capabilities & security

Declares egress to `maker.ifttt.com:443` only. Narrow it further per instance
with `network:` if desired; it can never be widened past the declaration. The
Maker key is a bearer credential for your IFTTT account's applets — treat it
like any other secret.
