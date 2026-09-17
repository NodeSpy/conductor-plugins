# `alertmanager` connector

A **source-only** connector: it listens for Prometheus Alertmanager (and
Grafana unified-alerting, which sends the identical payload shape) webhooks,
optionally checks a bearer token, and streams one normalized `alert` event per
alert in the batch to the daemon.

- **Kind:** connector (source only — no verbs)
- **Source:** [`connectors/alertmanager/main.go`](../../connectors/alertmanager/main.go)
- **Provides:** `alertmanager`
- **Capabilities:** empty manifest — it only listens, never dials out, and
  spawns nothing.
- **Bundled in conductor?** No. This plugin is the only way to get an
  alertmanager connector.

```yaml
connectors:
  am:
    use: alertmanager
    listen: ":9097"
    secret: ${ALERTMANAGER_BEARER_TOKEN}
triggers:
  - on: am.alert
    filters: { severities: [critical], statuses: [firing] }
    steps: [ ... ]
```

## Setup

Alertmanager has no webhook-registration UI — routing is entirely config-file
driven.

**Prerequisites:** write access to `alertmanager.yml` (or Grafana's unified
alerting config) and the ability to reload/restart Alertmanager.

1. Add a `webhook_configs` receiver pointing at the connector's endpoint:
   ```yaml
   receivers:
     - name: conductor
       webhook_configs:
         - url: http://<host>:<port>/alertmanager
           send_resolved: true
   ```
2. Wire the receiver into a `route:` (top-level or a matched sub-route) so
   the alerts you care about reach it.
3. Optional shared token: most versions support `authorization: {
   credentials: <token> }` on the `webhook_config` (or front the listener
   with a reverse proxy that injects `Authorization: Bearer <token>`) — set
   the same value as `secret` below. Grafana unified alerting: add the
   equivalent under **Alerting → Contact points → Webhook**.
4. Reload Alertmanager (`SIGHUP` or `POST /-/reload`) to pick up the change.

```yaml
connectors:
  am:
    use: alertmanager
    listen: ":9097"
    secret: ${ALERTMANAGER_BEARER_TOKEN}
```

No public URL? Set `smee: https://smee.io/<channel>` instead of (or
alongside) `listen`. See **Events** below for the `alert` payload.

## Connection

| key | type | purpose |
|-----|------|---------|
| `listen` | string | HTTP listener address, e.g. `:9097` (optional if `smee` is set) |
| `path` | string | listener path (default `/alertmanager`) |
| `secret` | string | bearer token compared to the `Authorization` header (`Bearer <secret>`) |
| `allow_unsigned` | bool | accept unauthenticated POSTs when no secret is set |
| `smee` | string | smee.io-style SSE relay URL, e.g. `https://smee.io/AbC123` — also (or instead) receive forwarded deliveries over SSE when the endpoint has no public URL |

> **Unauthenticated listeners fail closed.** Alertmanager and Grafana webhooks
> carry no HMAC signature at all — only an optional bearer token you configure
> on the receiver. With no `secret`, the listener would accept any POST on the
> address as a real alert. Set the secret, or set `allow_unsigned: true` to
> opt in explicitly when something else authenticates the endpoint.
>
> This is checked against the `Authorization` header directly by the
> connector, not via `sourcekit`'s HMAC path — `sourcekit.Listener.Secret` is
> left empty on purpose, since there is no signature to verify here.

## Events

| event | fires when |
|-------|-----------|
| `alert` | one Alertmanager/Grafana alert fired or resolved — emitted once per alert in the webhook's `alerts[]` |

**Context** (templated flat, e.g. `{{.severity}}`, `{{.summary}}`): `status`,
`alertname`, `severity`, `summary`, `description`, `instance`, `job`,
`runbook_url`, `generatorURL`, `externalURL`, `receiver`, plus every other
label on the alert flattened in by name (e.g. a `team` label becomes
`{{.team}}`) — flattening never overwrites the named keys above.

**Filters** (evaluated by the daemon's generic list-contains evaluator):
`alertnames`, `severities`, `statuses`, `receivers` (list forms), and
`alertname`, `severity`, `status`, `receiver` (scalar forms).

**Dedup:** keyed on the alert's `fingerprint` plus its `status`, so a
redelivered webhook doesn't emit twice, while the `firing` → `resolved`
transition for the same alert is a distinct, wanted event.
