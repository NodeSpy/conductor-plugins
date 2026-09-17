# `email` connector

Email as a connector: send mail over **SMTP** (verbs `send` / `send_raw`), and
poll an **IMAP** mailbox for new messages (source event `message`). Your own
mail account is the transport in both directions — there is no vendor API or
webhook involved. Built ONLY against the public SDK and the standard library:
`net/smtp` for sending, and a minimal hand-rolled IMAP4rev1 client (`net` +
`crypto/tls`) for the poll loop, since the standard library has no IMAP
package.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/email/main.go`](../../connectors/email/main.go)
- **Provides:** `email`
- **Capabilities:** no declared egress — the SMTP/IMAP host is operator-specific
  (your own mail provider), so there is nothing generic to declare. Set
  `network:` on the instance to the exact `host:port` pair(s) you expect it to
  dial; that is the actual confinement.
- **Bundled in conductor?** No — external plugin only.

```yaml
connectors:
  mail:
    use: email
    smtp:
      host: smtp.example.com
      username: bot@example.com
      password: ${SMTP_PASSWORD}
      from: bot@example.com
    imap:
      host: imap.example.com
      username: bot@example.com
      password: ${IMAP_PASSWORD}
      mailbox: INBOX
    network: ["smtp.example.com:587", "imap.example.com:993"]

triggers:
  - on: mail.message
    filters: { subjects: ["[urgent]"] }
    steps:
      - uses: mail.send
        options:
          to: oncall@example.com
          subject: "Re: {{.subject}}"
          body: "Saw this from {{.from}}: {{.body}}"
```

## Setup

No vendor API — the credential is just your mail account's login (SMTP for
sending, IMAP for the inbox source).

**Prerequisites:** a mailbox on a provider that exposes SMTP and/or IMAP.

1. If the account has 2-step verification (the default for Gmail and
   Microsoft 365), the normal login password won't work over SMTP/IMAP —
   generate an **app password** instead: Gmail →
   https://myaccount.google.com/apppasswords → name it → **Create**;
   Microsoft 365 → https://myaccount.microsoft.com → **Security info** →
   **Add method** → **App password**. Copy the password shown once.
2. Note the provider's SMTP/IMAP hostnames and ports (Gmail:
   `smtp.gmail.com:587` STARTTLS, `imap.gmail.com:993`; Microsoft 365:
   `smtp.office365.com:587` STARTTLS, `outlook.office365.com:993`).
3. Store the app password as a secret, referenced via `${ENV}`.

Configure:

```yaml
connectors:
  mail:
    use: email
    smtp:
      host: smtp.gmail.com
      username: bot@example.com
      password: ${SMTP_PASSWORD}
      from: bot@example.com
    imap:
      host: imap.gmail.com
      username: bot@example.com
      password: ${SMTP_PASSWORD}
      mailbox: INBOX
    network: ["smtp.gmail.com:587", "imap.gmail.com:993"]
```

`smtp.password`/`imap.password` are usually the same app password. For the
inbox source, see **Source: `message`** below for polling behavior, context
fields, and filters.

## Connection

| key | type | purpose |
|-----|------|---------|
| `smtp.host` | string | SMTP server host (required for `send`/`send_raw`) |
| `smtp.port` | integer | SMTP port (default `587`) |
| `smtp.username` | string | SMTP auth username (omit for an unauthenticated relay) |
| `smtp.password` | string | SMTP auth password |
| `smtp.from` | string | default `From` / envelope sender when a verb omits `from` |
| `smtp.tls` | string | `starttls` (default, port 587) \| `tls` (implicit TLS, port 465) \| `none` (plaintext — local/test relays only) |
| `imap.host` | string | IMAP server host (required for the `message` source) |
| `imap.port` | integer | IMAP port (default `993`) |
| `imap.username` | string | IMAP login username |
| `imap.password` | string | IMAP login password |
| `imap.mailbox` | string | mailbox to poll (default `INBOX`) |
| `imap.tls` | boolean | implicit TLS on connect (default `true`); `false` connects plaintext then `STARTTLS`-upgrades |
| `imap.poll_interval` | duration | how often to re-run the search (default `60s`) |
| `imap.mark_seen` | boolean | `UID STORE +FLAGS (\Seen)` after emitting each message (default `true`) |
| `imap.search` | string | the `UID SEARCH` criteria polled each round (default `UNSEEN`) |

## Source: `message`

Fires once per message matched by `imap.search` on each poll. Deduplicated by
`Message-ID` (falling back to `uid:<uid>@<mailbox>` for a message with no
Message-ID header) across restarts, within the dedup window.

**Context:** `from`, `to`, `subject`, `date`, `message_id`, `body`, `uid`,
`mailbox`.

**Filters** (evaluated by the daemon's generic list-contains evaluator):
`froms`, `subjects` (list forms), and `from`, `subject` (scalar forms).

```yaml
triggers:
  - on: mail.message
    filters: { froms: ["alerts@vendor.com"] }
    steps: [ ... ]
```

## Verbs

| verb | purpose |
|------|---------|
| `send` | build and send an email: `to` (address or list, required), `cc`, `bcc`, `subject`, `body` (plain text), `html`, `from` (overrides `smtp.from`), `reply_to`, `headers` (map of extra header fields). `body` + `html` together produce a `multipart/alternative` message; either alone produces a single-part message. Outputs `sent`, `message_id`. |
| `send_raw` | send a pre-built RFC 5322 message verbatim: `raw` (the full message), `to` (envelope recipient(s), required), `from` (envelope sender, overrides `smtp.from`). Outputs `sent`. |

## Design notes

- **`buildMessage`** (message construction) is a pure function of its options
  — no network, and Date/Message-ID/boundary are all overridable — so the
  RFC 5322 shape (headers, multipart boundary, part ordering) is unit-tested
  directly with no server involved.
- **SMTP sending** picks its transport from `smtp.tls`: `starttls` uses
  `smtp.SendMail` (which opportunistically upgrades when the server offers
  STARTTLS); `tls` dials straight into implicit TLS via `tls.Dial` +
  `smtp.NewClient`; `none` is a plaintext `net.Dial` + `smtp.NewClient`, for
  local or test relays only.
- **The IMAP client is intentionally minimal**: dial (implicit TLS or
  plaintext + `STARTTLS`), `LOGIN`, `SELECT`, then a loop of `UID SEARCH`,
  `UID FETCH (BODY.PEEK[HEADER.FIELDS (FROM TO SUBJECT DATE MESSAGE-ID)]
  BODY.PEEK[TEXT])` per matched UID, and `UID STORE +FLAGS (\Seen)` once
  `mark_seen` is on. `BODY.PEEK` is used throughout so fetching never
  flips `\Seen` on its own.
- **IMAP literal parsing is the one genuinely tricky part**: a `{N}` marker
  means the next N bytes are read verbatim (they can contain embedded CRLFs),
  so the reader (`readIMAPLine`) tracks literal byte *ranges* within the
  accumulated line text rather than scanning for marker text after the fact —
  a message body containing something that looks like `BODY[TEXT] {5}` can
  never be misparsed as a second literal. The tagged-response reader, the
  `SEARCH` UID-list parser, and the `FETCH` header/body parser are all pure
  functions exercised in `main_test.go` against canned server strings, with no
  real socket involved.
- On any IMAP error the connection is dropped and reconnected (`LOGIN` +
  `SELECT` again) after a short backoff; `Message-ID` dedup means a message
  re-seen across a reconnect is not re-emitted.
