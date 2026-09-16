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

## Connection

| key | type | purpose |
|-----|------|---------|
| `listen` | string | HTTP listener address, e.g. `:9097` |
| `path` | string | listener path (default `/alertmanager`) |
| `secret` | string | bearer token compared to the `Authorization` header (`Bearer <secret>`) |
| `allow_unsigned` | bool | accept unauthenticated POSTs when no secret is set |

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
