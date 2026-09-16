# `aws-sns` connector

A **source-only** connector: it runs an HTTP(S) subscription endpoint for an
AWS SNS topic (and/or relays through a smee.io channel), auto-confirms the
subscription, verifies the SNS message signature, and streams a normalized
`notification` event per delivery to the daemon.

- **Kind:** connector (source only — no verbs; publish to a topic via the
  `aws-cli` connector)
- **Source:** [`connectors/aws-sns/main.go`](../../connectors/aws-sns/main.go)
- **Provides:** `aws-sns`
- **Capabilities:** `egress: [smee.io:443, sns.*.amazonaws.com:443,
  *.amazonaws.com:443]` — it fetches SNS signing certs, GETs `SubscribeURL` to
  auto-confirm, and optionally dials smee.io. Spawns nothing.

```yaml
connectors:
  orders:
    use: aws-sns
    listen: ":9097"
triggers:
  - on: orders.notification
    filters: { topic_arns: ["arn:aws:sns:us-east-1:123456789012:orders"] }
    steps: [ ... ]
```

Behind smee.io, for an endpoint with no public URL:

```yaml
connectors:
  orders:
    use: aws-sns
    smee: https://smee.io/AbC123
```

Point the SNS subscription's endpoint at the smee.io channel URL; smee
forwards each delivery to this connector over Server-Sent Events, so
`listen`/a public URL is not required. `listen` and `smee` may both be set at
once — the connector runs whichever transports are configured.

## Connection

| key | type | purpose |
|-----|------|---------|
| `listen` | string | local HTTP listen address for the subscription endpoint (optional if `smee` is set) |
| `path` | string | listener path (default `/sns`) |
| `smee` | string | a smee.io channel URL, e.g. `https://smee.io/AbC123` |
| `auto_confirm` | boolean | on `SubscriptionConfirmation`, automatically GET the `SubscribeURL` (default `true`) |
| `verify_signature` | boolean | verify the SNS message signature (default `true`) |
| `allow_unsigned` | boolean | permit unverified messages, only when `verify_signature` is `false` (default `false`) |

At least one of `listen` or `smee` must be set.

> **Unverified auto-confirm fails closed.** Turning off `verify_signature`
> with no other guard means the endpoint would trust whatever body arrives —
> and, with `auto_confirm` on, GET whatever `SubscribeURL` a forged
> `SubscriptionConfirmation` names. That is both trigger injection and an open
> subscription-confirmation oracle. `StartSource` refuses to start with
> `verify_signature: false` unless `allow_unsigned: true` is set explicitly —
> the greppable way to say something else in front of this endpoint already
> authenticates it.

## Signature verification

Every `Notification` and `SubscriptionConfirmation`/`UnsubscribeConfirmation`
is verified (unless `verify_signature: false`) before it is acted on:

1. The canonical string-to-sign is built from the message's own fields, in
   AWS's documented key order (`Message`, `MessageId`, `Subject` if present,
   `Timestamp`, `TopicArn`, `Type` for a notification; `Message`, `MessageId`,
   `SubscribeURL`, `Timestamp`, `Token`, `TopicArn`, `Type` for a
   confirmation).
2. `SignatureVersion` selects the hash: `"1"` → SHA1, `"2"` → SHA256.
3. The signing cert is fetched from `SigningCertURL` — but only after
   confirming the URL is `https` **and** its host is `amazonaws.com` or a
   `*.amazonaws.com` subdomain. This is what makes fetching the cert safe: an
   attacker who forges a message can't point `SigningCertURL` at a cert they
   control. Certs are cached by URL.
4. `rsa.VerifyPKCS1v15` checks the signature against the cert's RSA public
   key.

`auto_confirm` only ever fires a `SubscribeURL` GET once the
`SubscriptionConfirmation` signature has verified (or verification was
explicitly disabled with `allow_unsigned: true`) — never on an unverified
message.

## smee.io transport

When `smee` is set, the connector rides `pkg/sourcekit`'s shared
`Listener{Relay: ...}` support: it opens the channel URL as a Server-Sent
Events stream (`Accept: text/event-stream`) and reconnects with backoff if the
stream drops. Each forwarded request arrives as one SSE `data:` payload — a
JSON object carrying the original request's headers at the top level (e.g.
`x-amz-sns-message-type`), a `body` field (the SNS JSON, delivered as a nested
object or as a string), plus `query`/`host`/`timestamp`. `sourcekit` extracts
the header and body from that payload and feeds them into the same core
handler the HTTP listener uses — smee is just an alternate transport, not a
different code path. When a relayed delivery carries no
`x-amz-sns-message-type` header, `handle()` falls back to the message body's
own top-level `Type` field, so relayed deliveries still classify correctly.
smee's own "ready"/keep-alive events (no `body`) are ignored.

## Events

| event | fires when |
|-------|-----------|
| `notification` | an SNS notification was delivered |

`SubscriptionConfirmation` and `UnsubscribeConfirmation` never emit an event —
they are transport plumbing (auto-confirm, and a log line), not something a
trigger fires on.

**Context** (templated flat, e.g. `{{.subject}}`, `{{.message}}`):
`topic_arn`, `subject`, `message` (the raw `Message` string), `message_id`,
`timestamp`, and — when `Message` parses as a JSON object —
`message_json` (the parsed object).

`title` is the notification's `Subject`, or `"sns notification"` if absent.

**Filters** (evaluated by the daemon's generic list-contains evaluator):
`topic_arns`, `subjects` (list forms), and `topic_arn`, `subject` (scalar
forms). Empty = any.
