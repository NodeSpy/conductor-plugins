# `homeassistant` connector

Home Assistant as a connector: call services, read/set entity state, fire bus
events, render templates, and read config/history/logbook over the REST API,
plus a raw `api` escape hatch. As a source, it receives inbound webhook POSTs
from a Home Assistant automation.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/homeassistant/main.go`](../../connectors/homeassistant/main.go)
- **Provides:** `homeassistant`
- **Capabilities:** no declared egress — the Home Assistant instance's
  `base_url` is operator-specific; narrow it yourself with `network:`.

```yaml
connectors:
  ha:
    use: homeassistant
    base_url: http://homeassistant.local:8123
    token: ${HA_TOKEN}
    network: ["homeassistant.local:8123"]   # narrow the declared egress
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string (required) | Home Assistant base URL, e.g. `http://homeassistant.local:8123`; the REST API is served at `base_url + /api` |
| `token` | string (required) | long-lived access token, sent as `Authorization: Bearer <token>` |
| `webhook` | map | source transport: `listen`, `path` (default `/homeassistant`), `secret`, `allow_unsigned` |

A non-2xx REST response is surfaced as an internal error carrying the status
code and response body.

## Source: inbound webhook

Home Assistant has no outbound-webhook signing scheme of its own, so this
source authenticates with a **shared token** instead of an HMAC signature: an
automation's `rest_command`/webhook call must send the configured
`webhook.secret` either as an `X-Conductor-Token` header or a `?token=` query
parameter. The comparison is constant-time (`crypto/subtle`).

The listener **fails closed**: if `webhook.secret` is empty, `StartSource`
refuses to start unless `webhook.allow_unsigned: true` is set explicitly —
the greppable way to say you really do front the listener with something else
that authenticates.

```yaml
connectors:
  ha:
    use: homeassistant
    base_url: http://homeassistant.local:8123
    token: ${HA_TOKEN}
    webhook:
      listen: ":9097"
      path: /homeassistant
      secret: ${HA_WEBHOOK_TOKEN}

triggers:
  - on: ha.event
    filters: { event_types: [doorbell] }
    steps:
      - uses: ha.call_service
        options: { domain: light, service: turn_on, entity_id: light.porch }
```

A Home Assistant automation action posting to this source looks like:

```yaml
action:
  - service: rest_command.notify_conductor
    # rest_command configured with:
    #   url: "http://conductor-host:9097/homeassistant?token={{ states('input_text.ha_webhook_token') }}"
    #   method: POST
    #   payload: '{"event_type": "doorbell", "id": "{{ now().timestamp() }}"}'
```

### Event

| event | fires when |
|-------|-----------|
| `event` | an automation posted a webhook payload |

The event's context is every top-level key of the posted JSON body, plus a
`payload` key holding a copy of the whole body. When the body includes
`event_type`, it becomes the event's `kind` and is also exposed as
`event_types` for filtering; `id`, if present, is the delivery's dedup key
(redeliveries with the same `id` are only emitted once).

**Filters:** `event_types` (list) or the scalar `event_type` alias.

## Verbs

Selected by `uses: <name>.<verb>`. All verbs return `status_code`; most also
return `result` (a single value) or `items` (a JSON array).

| verb | calls | notes |
|------|-------|-------|
| `call_service` | `POST /api/services/{domain}/{service}` | `entity_id` (string or list) is merged into `data` as the request body |
| `get_state` | `GET /api/states/{entity_id}` | |
| `list_states` | `GET /api/states` | |
| `set_state` | `POST /api/states/{entity_id}` | body: `state`, `attributes` |
| `fire_event` | `POST /api/events/{event_type}` | body: `data` |
| `render_template` | `POST /api/template` | returns the rendered text in `text` (not JSON-decoded) |
| `get_services` | `GET /api/services` | |
| `get_config` | `GET /api/config` | |
| `history` | `GET /api/history/period[/{start}]?filter_entity_id=&end_time=` | `entity_id`, `start`, `end` are all optional |
| `logbook` | `GET /api/logbook[/{start}]?entity=&end_time=` | `entity_id`, `start`, `end` are all optional |
| `api` | any `{method} /api{path}` | escape hatch: `method` (default GET), `path`, `query`, `body` |

## Capabilities & security

Declares no egress by default, since `base_url` varies per Home Assistant
install; set `network:` on the connector instance to pin it down. Use a
long-lived access token scoped to what this connector instance actually
needs to call.
