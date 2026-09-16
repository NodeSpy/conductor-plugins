# `telegram` connector

Telegram as a connector: send/edit/delete messages, photos, and documents,
answer inline-keyboard callback queries, and manage the webhook — over the
Telegram Bot API. As a **source**, it receives Telegram's webhook deliveries
(a bare `Update` object POSTed to the configured path) and streams a
normalized event per update for `message` and `callback_query`.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/telegram/main.go`](../../connectors/telegram/main.go)
- **Provides:** `telegram`
- **Capabilities:** egress to `api.telegram.org:443`

```yaml
connectors:
  tg:
    use: telegram
    token: ${TELEGRAM_BOT_TOKEN}
    network: ["api.telegram.org:443"]   # narrow the declared egress
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `token` | string | bot token from @BotFather (required for verbs) |
| `api_base` | string | override the Telegram Bot API base URL (tests) |
| `webhook` | map | source transport: `listen` (optional if `smee` is set), `path` (default `/telegram`), `secret`, `allow_unsigned`, `smee` (smee.io-style SSE relay URL, e.g. `https://smee.io/AbC123` — also (or instead) receive forwarded deliveries over SSE when the endpoint has no public URL) |

## Source events

Trigger with `on: <name>.<event>`. Telegram authenticates a webhook delivery
with a **plain shared-secret header**
(`X-Telegram-Bot-Api-Secret-Token`), not an HMAC signature — this source
compares it in constant time (`crypto/subtle`) rather than treating it as an
HMAC digest.

| event | fires when |
|-------|-----------|
| `message` | an incoming message |
| `callback_query` | an inline-keyboard callback query |

`message` context: `chat_id`, `chat_type`, `text`, `from` (username),
`from_id`, `message_id`, `command` (the word after `/`, with any `@botname`
suffix stripped, when the text is a bot command). `callback_query` context:
`callback_data`, `from`, `message_id`, `chat_id`.

Filters accept both list and scalar forms: `chat_ids`/`chat_id`,
`chat_types`/`chat_type`, `commands`/`command` (message only);
`chat_ids`/`chat_id` (callback_query). Updates are deduplicated by
`update_id`, so a Telegram redelivery does not fire twice.

```yaml
triggers:
  - on: tg.message
    filters: { commands: [deploy] }
    steps:
      - uses: tg.send_message
        options: { chat_id: "{{.chat_id}}", text: "deploying..." }
```

### Webhook security

No `webhook.secret` configured **and** no `webhook.allow_unsigned: true`
refuses to start the listener — a missing secret is far more often a mistake
than a choice. Set `webhook.secret` to the same secret token passed to
`set_webhook`'s `secret_token` option, or set `webhook.allow_unsigned: true`
if you genuinely front the listener with something else that authenticates
requests.

## Verbs

Selected by `uses: <name>.<verb>`. Every verb's outputs are uniformly
`result` (the API call's `result` field, unwrapped from Telegram's
`{ok, result, description}` envelope) + `ok`. A `ok: false` response is
returned as an invocation error carrying Telegram's `description`.

**Messaging**
`send_message(chat_id, text, parse_mode, reply_to_message_id, disable_web_page_preview, reply_markup)`,
`send_photo(chat_id, photo, caption)`,
`send_document(chat_id, document, caption)`,
`edit_message_text(chat_id, message_id, text, parse_mode, reply_markup)`,
`delete_message(chat_id, message_id)`.

**Callback queries**
`answer_callback_query(callback_query_id, text, show_alert)`.

**Bot & webhook management**
`get_updates(offset, limit, timeout)` (long-poll), `set_webhook(url,
secret_token, allowed_updates)`, `delete_webhook()`, `get_me()`.

**Escape hatch**
`api(method, params)` — call any Telegram Bot API method directly.

## Capabilities & security

Declares egress to `api.telegram.org:443` only. Narrow it further per
instance with `network:`; it can never be widened past the declaration.
Provide a bot token scoped to only the chats/channels this instance actually
needs to act on.
