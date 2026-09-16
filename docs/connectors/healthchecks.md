# `healthchecks` connector

Healthchecks.io as a connector: manage checks and read ping history through the
**management API**, send liveness beacons through the **pinging** endpoint, and
receive check-state changes as **source events** via Healthchecks' own webhook
integration. Built stdlib-only, on `pkg/plugin` + `pkg/sourcekit`.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/healthchecks/main.go`](../../connectors/healthchecks/main.go)
- **Provides:** `healthchecks`
- **Capabilities:** egress to `healthchecks.io:443` and `hc-ping.com:443`
  (self-hosted: override `api_base`/`ping_base` and narrow/replace the
  connector instance's `network:` to your own host)

```yaml
connectors:
  hc:
    use: healthchecks
    api_key: ${HEALTHCHECKS_API_KEY}
    webhook:
      listen: ":9097"
      secret: ${HEALTHCHECKS_WEBHOOK_TOKEN}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_key` | string | Healthchecks API key, sent as `X-Api-Key` on **management** verbs only |
| `api_base` | string | management API base (default `https://healthchecks.io`; override for self-hosted or tests) |
| `ping_base` | string | pinging base (default `https://hc-ping.com`; override for self-hosted or tests) |
| `webhook` | map | source transport: `listen`, `path` (default `/healthchecks`), `secret`, `allow_unsigned` |

Two surfaces, two trust models:

- **Management API** (`list_checks`, `get_check`, `create_check`,
  `update_check`, `pause_check`, `delete_check`, `get_pings`, `api`) sends
  `X-Api-Key: <api_key>` against `api_base`.
- **Pinging** (`ping`) sends **no** API key at all — the uuid embedded in the
  URL *is* the credential, exactly as Healthchecks designs it — against
  `ping_base`.

A non-2xx response from either surface fails the verb with a
`CodeInternalError` carrying the HTTP status and response body.

## Source events

Trigger with `on: <name>.check`. Healthchecks' webhook integration lets you
configure the exact JSON body it POSTs, using `$NAME`/`$STATUS`/`$CODE`/`$TAGS`
template variables. Configure it with this body:

```json
{"check":"$NAME","status":"$STATUS","uuid":"$CODE","tags":"$TAGS"}
```

and point it at `http://<listen>/healthchecks?token=<webhook.secret>` (or send
the token as an `X-Conductor-Token` header instead of the query parameter —
Healthchecks lets you add custom headers to a webhook integration).

Healthchecks has no signature scheme of its own for webhook integrations, so
this connector authenticates deliveries with a **shared token**: the value
must equal `webhook.secret`, compared in constant time
(`crypto/subtle.ConstantTimeCompare`). Startup **fails closed** if
`webhook.secret` is unset — set `webhook.allow_unsigned: true` if you
genuinely front the listener with something else that authenticates (e.g. a
reverse proxy).

| event | fires when |
|-------|-----------|
| `check` | a check-state change delivery arrives (up, down, started, …) |

Context: `name`, `status`, `uuid`, `tags` (a list). Deliveries are deduplicated
on `uuid + status`.

Filters (list or scalar match against the delivery):

| filter | matches against |
|--------|-----------------|
| `statuses` | the check's `status` |
| `checks` | the check's `uuid` |
| `tags` | the check's `tags` |

```yaml
triggers:
  - on: hc.check
    filters: { statuses: ["down"], tags: ["prod"] }
    steps:
      - uses: pagerduty.trigger
        options: { summary: "{{.name}} is down" }
```

## Verbs

Selected by `uses: <name>.<verb>`.

**Management**

| verb | options | does |
|------|---------|------|
| `list_checks` | `tag` (string or list) | list checks, optionally AND-filtered by tag → `items` |
| `get_check` | `uuid` | read one check → `result` |
| `create_check` | `name`, `tags`, `timeout`, `grace`, `schedule`, `tz` | create a check → `result` |
| `update_check` | `uuid`, `name`, `tags`, `timeout`, `grace` | partially update a check → `result` |
| `pause_check` | `uuid` | pause monitoring → `result` |
| `delete_check` | `uuid` | permanently delete → `ok` |
| `get_pings` | `uuid` | recent ping history → `items` |
| `api` | `method`, `path`, `body` | escape hatch for any management API call → `result` |

**Pinging** (no `api_key` sent)

| verb | options | does |
|------|---------|------|
| `ping` | `uuid`, `status` (`success`\|`fail`\|`start`\|`log`, default `success`), `body` | send a liveness beacon; a non-empty `body` switches GET → POST |

## Capabilities & security

Declares egress to `healthchecks.io:443` and `hc-ping.com:443` only. Narrow it
per instance with `network:`. Self-hosted Healthchecks: point `api_base` and
`ping_base` at your instance and replace the egress declaration with your own
host — it can never be widened past what's declared.
