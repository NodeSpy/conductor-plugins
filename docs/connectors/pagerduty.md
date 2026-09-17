# `pagerduty` connector

A **source-only** connector: it receives PagerDuty V3 webhook-subscription
events, verifies the `X-PagerDuty-Signature` HMAC (which may carry several `v1=`
values during key rotation), and streams a normalized `incident` event to the
daemon.

- **Kind:** connector (source only — no verbs)
- **Source:** [`connectors/pagerduty/main.go`](../../connectors/pagerduty/main.go)
- **Provides:** `pagerduty`
- **Capabilities:** empty manifest — inbound only; never dials out, spawns
  nothing.
- **Bundled in conductor?** No — removed from core. This plugin is the only way
  to get a pagerduty connector.

```yaml
connectors:
  pd:
    use: pagerduty
    listen: ":9098"
    signing_secret: ${PAGERDUTY_SIGNING_SECRET}
triggers:
  - on: pd.incident
    filters: { urgencies: [high], event_types: [incident.triggered] }
    steps: [ ... ]
```

## Setup

PagerDuty V3 webhook subscriptions can be account-wide or scoped to a service.

**Prerequisites:** a PagerDuty admin, or a user with the Webhooks permission.

1. In the PagerDuty web app, go to **Integrations → Generic Webhooks (v3)**
   (or a service's **Integrations** tab → **New Integration** → **Generic
   Webhook**).
2. Click **+ New Webhook Subscription**, choose the event subscriptions you
   want (e.g. `incident.triggered`, `incident.acknowledged`), and set
   **Webhook URL** to `http://<host>:<port>/pagerduty`.
3. On creation, PagerDuty shows the subscription's **secret** exactly once —
   copy it now. It signs deliveries via `X-PagerDuty-Signature` (`v1=…`,
   possibly several during key rotation).

```yaml
connectors:
  pd:
    use: pagerduty
    listen: ":9098"
    signing_secret: ${PAGERDUTY_SIGNING_SECRET}
```

No public URL? Set `smee: https://smee.io/<channel>` instead of (or
alongside) `listen`. See **Events** below for the `incident` payload and
filters.

## Connection

| key | type | purpose |
|-----|------|---------|
| `listen` | string | HTTP listener address, e.g. `:9098` (optional if `smee` is set) |
| `path` | string | request path (default `/pagerduty`) |
| `signing_secret` | string | webhook subscription signing secret |
| `smee` | string | smee.io-style SSE relay URL, e.g. `https://smee.io/AbC123` — also (or instead) receive forwarded deliveries over SSE when the endpoint has no public URL. HMAC verification still applies to relayed bodies. |

> **Unsigned listeners fail closed.** With no `signing_secret`, the listener
> would accept any POST as a real event. Set it, or set `allow_unsigned: true`
> to opt in explicitly. Multiple signatures (`v1=…,v1=…`) are all checked, so
> key rotation works.

## Events

| event | fires when |
|-------|-----------|
| `incident` | a PagerDuty incident event |

**Context:** `pagerduty.event_type`, `pagerduty.status`, `pagerduty.title`,
`pagerduty.urgency`, `pagerduty.priority`, `pagerduty.service`,
`pagerduty.service_id`, `pagerduty.number`, `pagerduty.id`, `pagerduty.url`, plus
convenience `title` / `url`.

**Filters** (list-contains): `event_types` (e.g. `incident.triggered`,
`incident.escalated`), `services` (summary or id), `urgencies` (`high`/`low`),
`priorities` (`P1`, `P2`, …). Empty = any.
