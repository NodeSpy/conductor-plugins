# `twilio` connector

Twilio as a connector: send/receive SMS and WhatsApp messages, place calls,
and drive the REST API generically, over `net/http` with HTTP Basic auth
(`account_sid:auth_token`). As a source, it receives inbound SMS/voice
webhooks and verifies Twilio's own `X-Twilio-Signature` scheme.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/twilio/main.go`](../../connectors/twilio/main.go)
- **Provides:** `twilio`
- **Capabilities:** egress to `api.twilio.com:443` only; spawns nothing.
- **Bundled in conductor?** No — never in core. Add it here.

```yaml
connectors:
  sms:
    use: twilio
    account_sid: ${TWILIO_ACCOUNT_SID}
    auth_token: ${TWILIO_AUTH_TOKEN}
    webhook:
      listen: ":9097"
      public_url: https://hooks.example.com/twilio
triggers:
  - on: sms.sms
    filters: { tos: ["+15551234567"] }
    steps:
      - uses: sms.send_sms
        options: { from: "+15551234567", to: "{{.from}}", body: "got it" }
```

## Setup

Grab your account credentials from the Twilio Console, and a phone number to
send/receive from.

**Prerequisites:** a Twilio account.

1. Log in to the [Twilio Console](https://console.twilio.com/).
2. On the Console dashboard (**Account Info** panel, or **Account** → **Account
   info**), copy the **Account SID** and **Auth Token** — the latter is
   `auth_token` above.
3. Buy or use an existing number under **Phone Numbers** → **Manage** →
   **Active numbers** (or provision a **Messaging Service** for higher
   throughput/sender pools).
4. For inbound SMS/voice, see [Source events](#source-events) below — point
   the number's **Messaging** webhook (or the Messaging Service's **Incoming
   messages** webhook) at `webhook.public_url` + `path`.

**Configure:**

```yaml
connectors:
  sms:
    use: twilio
    account_sid: ${TWILIO_ACCOUNT_SID}
    auth_token: ${TWILIO_AUTH_TOKEN}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `account_sid` | string | Twilio Account SID (required) |
| `auth_token` | string | Twilio Auth Token — used for REST auth **and** as the webhook signing secret (required) |
| `webhook` | map | source transport: `listen`, `path`, `validate`, `public_url`, `smee` |
| `api_base` | string | override the Twilio API base URL (tests, or a private gateway) |

### `webhook`

| key | type | purpose |
|-----|------|---------|
| `listen` | string | HTTP listener address, e.g. `:9097` (optional if `smee` is set) |
| `path` | string | listener path (default `/twilio`) |
| `validate` | boolean | verify `X-Twilio-Signature` (default `true`) |
| `public_url` | string | the externally-reachable base URL Twilio posts to (e.g. `https://hooks.example.com`) — required when `validate` is true, since it is part of the signed data |
| `smee` | string | a smee.io-style SSE relay URL, e.g. `https://smee.io/AbC123` — also (or instead) receive forwarded deliveries over SSE when the endpoint has no public URL |

At least one of `listen` or `smee` must be set.

> **Unverified listeners fail closed.** `validate: true` (the default) needs
> both `auth_token` and `webhook.public_url` — without either, signature
> verification is impossible and a plugin start fails rather than silently
> accepting unsigned deliveries. Set both, or set `webhook.validate: false` to
> opt in explicitly when something else authenticates the endpoint.

> **Signature verification behind `smee`.** `X-Twilio-Signature` is computed
> over the exact URL Twilio requested (`webhook.public_url` + `path`), not
> over whatever URL the delivery arrives at. When `smee` relays a request in,
> the request Twilio actually signed was the URL you gave *it* (typically the
> smee.io channel URL), which will not match `public_url` — so verification
> will fail even though the delivery is genuine. When using `smee`, either set
> `webhook.validate: false` (and rely on the relay channel's own secrecy), or
> set `webhook.public_url` to the exact URL Twilio was configured with so the
> signed URL matches.

## X-Twilio-Signature verification

Twilio's webhook signature is **not** the HMAC-SHA256-over-raw-body scheme
`pkg/sourcekit`'s `Listener` verifies for GitHub/Sentry/PagerDuty, so this
connector leaves `Listener.Secret` empty (which disables `sourcekit`'s own
check) and validates the Twilio way itself, once the body is parsed into form
params:

1. Take the exact URL Twilio requested: `webhook.public_url` + the listener
   path (scheme + host + path — no query string).
2. Append every POST parameter, **sorted by key**, as `key` + `value`
   concatenated directly (no separators).
3. HMAC-SHA1 the result, keyed by `auth_token`.
4. Base64-encode the MAC and compare it, constant-time, to the
   `X-Twilio-Signature` request header.

See [Twilio's own writeup](https://www.twilio.com/docs/usage/webhooks/webhooks-security)
for the reference algorithm.

## Source events

Trigger with `on: <name>.<event>`. Distinguished by the request shape:
`Body`/`MessageSid` (or the older `SmsSid`) means `sms`; `CallSid` alone means
`call`. Deduplicated on the message/call SID.

| event | fires when |
|-------|-----------|
| `sms` | an incoming SMS/MMS message |
| `call` | an incoming voice call |

**Context:**
- `sms`: `from`, `to`, `body`, `message_sid`, `num_media`
- `call`: `from`, `to`, `call_sid`, `call_status`

**Filters** (evaluated by the daemon's generic list-contains evaluator):
`froms`, `tos` (list forms), and `from`, `to` (scalar forms) — shared across
both events.

## Verbs

| verb | purpose |
|------|---------|
| `send_sms` | send an SMS message (`from`, `to`, `body`, `media_url`) |
| `send_whatsapp` | send a WhatsApp message; `from`/`to` are prefixed `whatsapp:` automatically if missing |
| `get_message` | read a message's status/body by SID |
| `list_messages` | list recent messages, optionally filtered by `to`/`from`/`date_sent` → `messages` |
| `make_call` | place an outbound call; exactly one of `url` (TwiML URL) or `twiml` (inline document) |
| `get_call` | read a call's status by SID |
| `api` | escape hatch: `method` + `path` (relative to `/Accounts/{account_sid}`), `params` (form-encoded for POST, query string for GET) |

Every verb's HTTP outcome is decoded into `result` (the raw parsed JSON
response) plus `status_code`; `list_messages` additionally hoists the
response's `messages` array to a top-level `messages` output. A non-2xx
response from Twilio surfaces as a plugin error carrying the HTTP status and
response body, not partial output.

## Capabilities & security

Declares egress to `api.twilio.com:443` only, and spawns nothing. Narrow it
per instance with `network:`; it can never be widened past the declaration.
