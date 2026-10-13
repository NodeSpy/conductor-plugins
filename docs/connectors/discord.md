# `discord` connector

Discord as a connector: post messages, present hand-off questions, and
capture replies over a bot's gateway connection (or post-only via an
incoming webhook). Built against the public SDK (`pkg/plugin`) plus
`github.com/gorilla/websocket` for the gateway client — no other third-party
dependency.

- **Kind:** connector (verbs + source)
- **Source:** [`connectors/discord/main.go`](../../connectors/discord/main.go)
- **Provides:** `discord`
- **Capabilities:** egress to `discord.com:443` and `gateway.discord.gg:443`; spawns nothing.

```yaml
connectors:
  bot:
    use: discord
    bot_token: ${DISCORD_BOT_TOKEN}
triggers:
  - on: deploy.failed
    steps:
      - uses: bot.post
        options: { channel: "1234567890", text: "deploy failed: {{.error}}" }
```

## Setup

**Prerequisites:** a Discord application with a bot user (for `post`/`ask`
with full functionality and reply capture), or just an incoming webhook (for
post-only use with no replies).

**Bot token** (recommended — needed for DMs, threads, and `ask`):

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications)
   and create (or open) an application.
2. Open **Bot** in the sidebar, and click **Reset Token** (or **Add Bot** if
   there isn't one yet) to get a bot token. Copy it (this is `bot_token`).
3. Under **Privileged Gateway Intents**, enable **Message Content Intent** —
   without it, every message the gateway receives arrives with its content
   empty, so replies can never be read.
4. Use the **OAuth2 URL Generator** (scope `bot`, permissions `Send Messages`,
   `Read Message History`, and `Create Private Threads` if you plan to use
   threads) to generate an invite link, and add the bot to your server.

**Incoming webhook** (post-only alternative — no bot, no replies, no `ask`):

1. In a Discord channel's settings, go to **Integrations → Webhooks → New
   Webhook**.
2. Copy the webhook URL (this is `webhook_url`).

**Configure:**

```yaml
connectors:
  bot:
    use: discord
    bot_token: ${DISCORD_BOT_TOKEN}
```

or, post-only:

```yaml
connectors:
  bot:
    use: discord
    webhook_url: ${DISCORD_WEBHOOK_URL}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `bot_token` | string | Discord bot token (from the developer portal) |
| `webhook_url` | string | an incoming-webhook URL — post-only alternative to a bot token |
| `api_base` | string | override the Discord REST API base URL (tests, or a private gateway); default `https://discord.com/api/v10` |
| `gateway_url` | string | override the gateway URL this plugin dials to capture replies (tests only); when set, the normal `GET /gateway/bot` bootstrap call is skipped |

Set `bot_token` **or** `webhook_url`. `webhook_url`-only connections can only
`post` (no DMs, no `ask`, no reply capture — there is nothing for the gateway
to authenticate as). `plugin.validate` refuses a connection with neither set.

## Source: `reply`

Whenever `bot_token` is set, this connector runs a persistent Discord gateway
connection (reconnecting with exponential backoff, 1s doubling to a 30s cap,
on any drop) that fires a `reply` event for **every** inbound message in a
channel, thread, or DM the bot can see — except its own messages and other
bots' messages, which are always filtered out so the gateway can never
resolve its own posted question.

```yaml
triggers:
  - on: bot.reply
    filters: { channel: "1234567890" }
    steps:
      - uses: some.step
        options: { text: "{{.text}}" }
```

| context key | type | |
|-------------|------|---|
| `channel` | string | the channel (or DM channel) id the message landed in |
| `author` | string | the Discord user id of the message's author |
| `author_bot` | boolean | true if the author is a bot account |
| `text` | string | the message content |
| `message_id` | string | the Discord message id (also the dedup key) |

Filters: `channel` (scalar), `author` (scalar).

**This is also how `ask` gets its answer.** The `ask` verb (below) posts a
question and returns immediately with `ref` — the channel id the question
landed in. It does **not** wait inside this plugin. Instead, every reply that
arrives on that channel is emitted as a `reply` event carrying the
`conversation_reply` semantic (`id: channel, author, text`); the host matches
it against the pending ask by `(instance, channel)` and resolves it there —
enforcing `approvers` against the reply's author when the ask set any. A
reply that does not match a pending ask falls through as an ordinary `reply`
event, available to any trigger `on: <name>.reply`.

## Verbs

### `post`

Post a message to a channel or DM.

| option | type | |
|--------|------|---|
| `channel` | string, scope `channel` | channel id (or set `user:` for a DM) |
| `user` | string, scope `user` | user id to DM |
| `text` | string, required | |

With a `webhook_url`-only connection, `post` sends `{"content": text}` to the
incoming webhook and ignores `channel`/`user` (`id`/`channel` come back
empty). With `bot_token`, set `channel` **or** `user`; `user` opens (or
reuses) a DM first.

Outputs: `id` (the posted message id), `channel` (the channel it landed in).

Declares `conversation_post`: a flow replying to an event's author through
this verb is skipped automatically when that event's author is a bot and
`reply_to_bots` is off.

```yaml
steps:
  - uses: bot.post
    options: { channel: "1234567890", text: "build is green" }
```

### `ask`

Present a question (optionally with an editable draft) and open a
conversation for the reply.

| option | type | |
|--------|------|---|
| `to` | string, required | `dm` or `thread` |
| `user` | string, scope `user` | user id (`to: dm`) |
| `channel` | string, scope `channel` | channel id (`to: thread`) |
| `approvers` | list | `to: thread` — only these user ids may resolve the ask (default: anyone in the channel) |
| `prompt` | string, required | the question to present |
| `draft` | string | editable draft text presented with the question |
| `title` | string | presentation title (default: the prompt's first line) |
| `timeout` | duration | how long the host waits for an answer (default 1h) |

Outputs: `ref` (the channel/DM-channel id the question was posted to — this
is also the `reply` event's `channel` fact that resolves this ask), plus
`action`/`text` once a reply resolves it (see [Source: `reply`](#source-reply)
above — this verb's own `Invoke` only ever returns `ref`; the eventual
`action`/`text` are assembled by the host from the matching `reply` event,
not by this plugin).

`to: dm` needs `bot_token` and `options.user`; `to: thread` needs
`options.channel`. Needs `bot_token` either way — `ask` is unavailable on a
`webhook_url`-only connection.

```yaml
steps:
  - uses: bot.ask
    options:
      to: thread
      channel: "1234567890"
      prompt: "ship this release?"
      approvers: ["555444333"]
```

## Capabilities & security

Declares egress to `discord.com:443` (REST) and `gateway.discord.gg:443`
(the gateway's usual host; the actual gateway URL is resolved per-session via
`GET /gateway/bot` and may point elsewhere). Narrow with `network:` if your
deployment constrains egress further.

## Gap from the old bundled connector

Conductor's former bundled `discord` connector blocked inside its own `ask`
verb, waiting on an in-process inbox until a reply arrived or the timeout
elapsed, and returned `action`/`text`/`ref` all at once. The plugin contract
moves that wait to the host (`opens_conversation` / `conversation_reply`,
docs/design/plugin-contract.md §2.2–§2.3): this plugin's `ask` verb posts the
question and returns only `ref`, declaring `action`/`text` in its output
schema for documentation, but it is the host — not this `Invoke` call — that
fills them in once a matching `reply` event arrives. Everything else (every
connection key, both verbs' options, the webhook-only post path, and the
exact reply-filtering behavior) is byte-for-byte parity.
