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

## Connection

| key | type | purpose |
|-----|------|---------|
| `listen` | string | HTTP listener address, e.g. `:9098` |
| `path` | string | request path (default `/pagerduty`) |
| `signing_secret` | string | webhook subscription signing secret |

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
