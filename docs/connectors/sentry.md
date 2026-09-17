# `sentry` connector

A **source-only** connector: it listens for Sentry Integration-Platform
webhooks, verifies the `Sentry-Hook-Signature` HMAC, and streams a normalized
event per alert to the daemon.

- **Kind:** connector (source only — no verbs)
- **Source:** [`connectors/sentry/main.go`](../../connectors/sentry/main.go)
- **Provides:** `sentry`
- **Capabilities:** empty manifest — it only listens, never dials out, and
  spawns nothing.
- **Bundled in conductor?** No — removed from core. This plugin is the only way
  to get a sentry connector, and it is **not** a drop-in for the old bundled one
  (see below).

```yaml
connectors:
  mysentry:
    use: sentry
    listen: ":9099"
    client_secret: ${SENTRY_CLIENT_SECRET}
triggers:
  - on: mysentry.issue_alert
    filters: { levels: [error, fatal] }
    steps: [ ... ]
```

## Setup

Sentry webhooks are configured per **Internal Integration**, not per-project.

**Prerequisites:** an Organization Owner/Manager in the Sentry org.

1. In Sentry, go to **Settings → Developer Settings → Internal Integrations**
   (`/settings/<org>/developer-settings/`) and click **New Internal
   Integration**.
2. Give it a name, enable **Webhooks**, and set the **Webhook URL** to
   `http://<host>:<port>/sentry` (the connector's `listen` + `path`).
3. Save — Sentry generates a **Client Secret** once; copy it. It signs every
   delivery via `Sentry-Hook-Signature`.
4. Wire the integration's alert action into the Issue Alerts / Metric Alerts
   you want forwarded, or install it org-wide.

```yaml
connectors:
  mysentry:
    use: sentry
    listen: ":9099"
    client_secret: ${SENTRY_CLIENT_SECRET}
```

No public URL yet? Set `smee: https://smee.io/<channel>` instead of (or
alongside) `listen`. See **Events** below for the alert payloads this emits.

## Connection

| key | type | purpose |
|-----|------|---------|
| `listen` | string | HTTP listener address, e.g. `:9099` (optional if `smee` is set) |
| `path` | string | listener path (default `/sentry`) |
| `client_secret` | string | `Sentry-Hook-Signature` HMAC key |
| `smee` | string | smee.io-style SSE relay URL, e.g. `https://smee.io/AbC123` — also (or instead) receive forwarded deliveries over SSE when the endpoint has no public URL. HMAC verification still applies to relayed bodies. |

> **Unsigned listeners fail closed.** With no `client_secret`, the listener
> would accept any POST on the address as a real event. Set the secret, or set
> `allow_unsigned: true` to opt in explicitly when something else authenticates
> the endpoint.

## Events

| event | fires when |
|-------|-----------|
| `issue_alert` | a Sentry issue alert fired |
| `error_alert` | a Sentry error alert fired |
| `event_alert` | a Sentry metric/event alert fired |

**Context** (templated flat, e.g. `{{.level}}`, `{{.short_id}}`): `resource`,
`action`, `title`, `level`, `environment`, `culprit`, `short_id`, `project`,
`url`.

**Filters** (evaluated by the daemon's generic list-contains evaluator):
`projects`, `levels`, `environments` (list forms), and `project`, `level`,
`environment` (scalar forms).

## Migrating from the old bundled connector

Not a drop-in replacement — `conductor config migrate` will refuse to
auto-convert:

- **Event names changed:** the bundled connector had one `alert` event; this
  plugin declares `issue_alert` / `error_alert` / `event_alert`.
- **Context is flat:** template `{{.level}}`, not the old nested `{{.sentry.level}}`.
- **No `exclude:`** — the legacy "first matching rule wins" precedence can't be
  expressed; write mutually exclusive filters instead.
