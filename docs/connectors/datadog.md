# `datadog` connector

Datadog as a connector: a small **verb** surface over the Datadog API (post
events, mute/unmute monitors, read/list monitors, submit metrics, and a
generic API escape hatch), plus a **source** that receives Datadog monitor
webhooks and streams a normalized `alert` event per delivery. Built ONLY on
the standard library and the public SDK (`pkg/plugin`, `pkg/sourcekit`).

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/datadog/main.go`](../../connectors/datadog/main.go)
- **Provides:** `datadog`
- **Capabilities:** egress to `api.datadoghq.com:443` (default site only — see
  below)
- **Bundled in conductor?** No — this plugin is the only way to get a datadog
  connector.

```yaml
connectors:
  dd:
    use: datadog
    api_key: ${DATADOG_API_KEY}
    app_key: ${DATADOG_APP_KEY}
    webhook:
      listen: ":9097"
      secret: ${DATADOG_WEBHOOK_TOKEN}
triggers:
  - on: dd.alert
    filters: { alert_types: [error], priorities: [P1, P2] }
    steps:
      - uses: dd.mute_monitor
        options: { monitor_id: "{{.alert_id}}" }
```

## Setup

Two separate credentials: API keys for verbs, a webhook for the source.

**Prerequisites:** a Datadog org admin (to mint keys and add an integration).

1. **API key:** **Organization Settings → API Keys → New Key**; copy it for
   `api_key`.
2. **Application key:** **Organization Settings → Application Keys → New
   Key** (required for monitor/metric verbs); copy it for `app_key`.
3. **Webhook:** **Integrations → Webhooks → New** (or the tile's
   **Configuration** tab if already installed). Set a **Name** (referenced
   from monitors), **URL** to `http://<host>:<port>/datadog?token=<secret>`
   (or omit the query token and send `X-Conductor-Token` instead), and
   **Payload** to the exact JSON template from **Source: `alert` event**
   below — Datadog has no fixed shape, so paste it verbatim.
4. In each monitor's notification message, add `@webhook-<name>` to fire it.

```yaml
connectors:
  dd:
    use: datadog
    api_key: ${DATADOG_API_KEY}
    app_key: ${DATADOG_APP_KEY}
    webhook:
      listen: ":9097"
      secret: ${DATADOG_WEBHOOK_TOKEN}
```

No public URL? Set `webhook.smee: https://smee.io/<channel>` instead of (or
alongside) `webhook.listen`. Non-default `site`? Widen `network:` per the
note below.

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_key` | string | `DD-API-KEY` (required) |
| `app_key` | string | `DD-APPLICATION-KEY` (required for monitor/metric verbs) |
| `site` | string | Datadog site: `datadoghq.com` (default), `datadoghq.eu`, `us5.datadoghq.com`, … |
| `api_base` | string | override the full API base URL (tests only; overrides `site`) |
| `webhook` | map | source transport: `listen`, `path` (default `/datadog`), `secret`, `smee` (smee.io-style SSE relay URL — receive forwarded deliveries when the endpoint has no public URL; the shared token is still checked) |

### Non-default sites and egress

`Capabilities.Egress` declares `api.datadoghq.com:443` only, because the
manifest is fixed at Describe time and cannot vary per-instance. If your org
uses a different `site` (`datadoghq.eu`, `us3.datadoghq.com`, …), you MUST
widen `network:` on the connector instance to that host, or the daemon's
capability check will refuse the call — Capabilities can be narrowed by an
instance's `network:`, never widened, so the default manifest cannot cover
every site up front.

## Verbs

| verb | options | calls |
|------|---------|-------|
| `post_event` | `title`, `text`, `tags`, `alert_type` (error\|warning\|success\|info), `priority` (normal\|low), `aggregation_key` | `POST /api/v1/events` |
| `mute_monitor` | `monitor_id` (scope: monitor), `scope`, `end` (unix ts) | `POST /api/v1/monitor/{id}/mute` |
| `unmute_monitor` | `monitor_id` (scope: monitor), `scope` | `POST /api/v1/monitor/{id}/unmute` |
| `get_monitor` | `monitor_id` (scope: monitor) | `GET /api/v1/monitor/{id}` → `result` |
| `list_monitors` | `name`, `tags`, `monitor_tags` | `GET /api/v1/monitor` → `items` |
| `mute_all` | — | `POST /api/v1/monitor/mute_all` |
| `unmute_all` | — | `POST /api/v1/monitor/unmute_all` |
| `submit_metric` | `metric`, `points` (`[timestamp, value]` pairs or bare values), `type` (count\|gauge\|rate, default gauge), `tags` | `POST /api/v2/series` |
| `api` | `method`, `path`, `query`, `body` | generic escape hatch for any Datadog API path |

Every verb's outputs include `status_code`; verbs that return a body expose it
as `result` (`list_monitors` exposes it as `items` instead, since the response
is already a list). A non-2xx response is a `CodeInternalError` whose message
carries the HTTP status and response body verbatim.

## Source: `alert` event

Datadog webhooks are **operator-templated**: Datadog has no fixed webhook
payload shape — you type the JSON body yourself in the monitor's notification
message, using `$`-variables Datadog substitutes at delivery time. This plugin
understands exactly **one** template; paste it verbatim into the monitor's
webhook payload field:

```json
{"alert_id":"$ALERT_ID","alert_type":"$ALERT_TYPE","title":"$EVENT_TITLE","body":"$EVENT_MSG","priority":"$PRIORITY","tags":"$TAGS","url":"$LINK","event_id":"$ID","org_name":"$ORG_NAME","hostname":"$HOSTNAME","scope":"$ALERT_SCOPE","transition":"$ALERT_TRANSITION"}
```

Any other payload shape fails to parse and is silently dropped (no event is
emitted for a delivery this plugin can't understand).

**Context:** `alert_id`, `alert_type` (`error`\|`warning`\|`success`\|`info`\|`recovery`),
`title`, `body`, `priority`, `tags` (list — split from Datadog's
comma/space-separated `$TAGS`), `url`, `event_id`, `org_name`, `hostname`,
`scope`, `transition`.

**Filters** (list-contains, empty = any): `alert_types`, `priorities`, `tags`,
`scopes`. Scalar variants (`alert_type`, `priority`, `scope`) are also
declared for a single-value match.

**Dedup:** `alert_id` + `alert_type`, falling back to `event_id` then `title`
when `alert_id` is empty (Datadog omits `$ALERT_ID` for some monitor types).

### Webhook authentication: shared token, not HMAC

Datadog **cannot sign** its webhook payloads — there is no HMAC signature
header to verify, unlike GitHub/Sentry/PagerDuty. The only authentication
available is a shared token the operator pastes into the webhook URL
(`https://host:port/datadog?token=<secret>`) or a custom header
(`X-Conductor-Token: <secret>`), which this plugin compares to
`webhook.secret` in constant time (`crypto/subtle`). The header wins when
both are present.

> **Unauthenticated listeners fail closed.** With no `webhook.secret`, the
> listener would accept any POST as a real event from anyone who can reach the
> listen address. Set it, or set `allow_unsigned: true` on the connector
> instance to opt in explicitly (e.g. the listener sits behind something else
> that authenticates, such as a private network or a reverse proxy).

## Capabilities & security

Declares egress to `api.datadoghq.com:443` only (see the non-default-site note
above). Provide an API key + application key scoped to what the connector
instance actually needs to do.
