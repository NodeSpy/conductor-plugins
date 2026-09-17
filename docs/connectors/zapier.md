# `zapier` connector

Zapier is a generic automation hub rather than a single API, so this connector
is a two-way bridge rather than a client for one product surface:

- **`send`** (verb) POSTs a JSON body to a Zapier **Catch Hook** webhook URL —
  the standard way an external system kicks off a Zap.
- **`event`** (source) receives the JSON a Zap's own **Webhooks by Zapier**
  action posts back to conductor, and streams it as a normalized event so a
  conductor trigger can react to whatever the Zap produced.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/zapier/main.go`](../../connectors/zapier/main.go)
- **Provides:** `zapier`
- **Capabilities:** egress to `hooks.zapier.com:443` only
- **Bundled in conductor?** No. This plugin is the only way to get a zapier
  connector.

```yaml
connectors:
  zap:
    use: zapier
    hook_url: ${ZAPIER_CATCH_HOOK_URL}
    webhook:
      listen: ":9096"
      secret: ${ZAPIER_WEBHOOK_TOKEN}
triggers:
  - on: zap.event
    filters: { events: [order.created] }
    steps:
      - uses: zap.send
        options:
          body: { message: "new order: {{.id}}" }
```

## Setup

**Prerequisites:** a Zapier account.

1. In the Zapier app, create a new Zap.
2. Set its trigger to **Webhooks by Zapier** → **Catch Hook**, and finish
   publishing the Zap.
3. On the trigger step, copy the **custom webhook URL** Zapier generates
   (`https://hooks.zapier.com/hooks/catch/...`) — this is `hook_url`.
4. To receive a Zap's own output back in conductor, add a **Webhooks by
   Zapier** → **POST** action at the end of the Zap, pointed at your
   `webhook` listener's public URL (see the source note below).

**Configure:**

```yaml
connectors:
  zap:
    use: zapier
    hook_url: ${ZAPIER_CATCH_HOOK_URL}
```

For the inbound source (a Zap posting its output back), see [Source
event](#source-event) below — it needs `webhook.secret` set and the Zap's
POST action configured to send that token back (header or query param).

## Connection

| key | type | purpose |
|-----|------|---------|
| `hook_url` | string | default Catch Hook URL used by `send` when the verb's own `hook_url` option is omitted; must be a `hooks.zapier.com` URL |
| `webhook` | map | source transport: `listen`, `path` (default `/zapier`), `secret`, `allow_unsigned`, `smee` (smee.io-style SSE relay URL — receive forwarded deliveries when the endpoint has no public URL; the shared token is still checked) |

## Verbs

| verb | purpose |
|------|---------|
| `send` | POST a JSON body to a Zapier Catch Hook webhook |

**Options:** `hook_url` (string, optional — overrides `connection.hook_url` for
this call; one of the two must be present), `body` (any — a map, a list, or a
bare string; the JSON payload posted).

**Outputs:** `status_code` (integer), `result` (any — the response body,
parsed as JSON when it was valid JSON).

`send` refuses to POST anywhere except `hooks.zapier.com`: the resolved
`hook_url`'s host is validated before any network call is made, so a
misconfigured or attacker-supplied `hook_url` can never turn this connector
into an open POST-to-anywhere relay. This is enforced in code, not just
declared — it is what makes the `hooks.zapier.com:443` capability meaningful.

## Source event

Zapier's "Webhooks by Zapier" action can only POST a JSON body — it cannot
compute an HMAC signature. So, like the `datadog` connector, authentication is
a shared token compared in constant time against either the
`X-Conductor-Token` header or a `?token=` query parameter (whichever the Zap's
action can attach), never an HMAC. `sourcekit.Listener.Secret` is left empty on
purpose; the shared `sourcekit.Listener.ServeReq` hands the callback the full
request (headers, query, body), so the `?token=` query case is checked
directly — and the same listener transparently accepts deliveries relayed
over `webhook.smee` for endpoints with no public URL.

> **Unauthenticated listeners fail closed.** With no `webhook.secret`, the
> listener would accept any POST on the address as a real event. Set it, or
> set `webhook.allow_unsigned: true` to opt in explicitly when something else
> authenticates the endpoint (e.g. a private network or reverse proxy).

| event | fires when |
|-------|-----------|
| `event` | a Zap's Webhooks-by-Zapier action posted its output back to conductor |

**Context:** every top-level key of the posted JSON object, flattened
directly (e.g. a posted `{"id": "1", "amount": 42}` yields `{{.id}}` and
`{{.amount}}`), plus the full object under `{{.payload}}`. A body that was not
a JSON object is carried instead as `{{.body}}` (the raw text).

**Filters** (list-contains, evaluated by the daemon's generic filter
evaluator): `events` (matches the posted payload's `events` key, if present),
`event` (exact match against the posted payload's `event` key, if present).
Since the payload shape is entirely operator-defined, the event is declared
`dynamic` — there is no fixed schema beyond these two conventional keys.

**Dedup:** keyed on the posted `id`, when present, so a redelivered webhook
doesn't emit twice. A payload with no `id` is always emitted.
