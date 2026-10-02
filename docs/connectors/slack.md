# `slack` connector

Slack as a connector: mentions, reactions, slash commands, message
shortcuts and modal forms in over **Socket Mode** (an outbound WebSocket —
no public URL needed); messages, reactions, interactive hand-off asks,
thread reads and file downloads out over the Web API.

This is the same connector conductor used to ship compiled in, now out of
process: existing configs written for `use: slack` keep working unchanged.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/slack/`](../../connectors/slack/)
- **Provides:** `slack`
- **Capabilities:** egress to `slack.com:443` only

```yaml
connectors:
  bot:
    use: slack
    app_token: ${SLACK_APP_TOKEN}
    bot_token: ${SLACK_BOT_TOKEN}

triggers:
  - on: bot.app_mention
    steps:
      - use: agent
        with: { prompt: "{{.slack.text}}" }
```

## Setup

You need a Slack app with **Socket Mode** enabled (so the plugin connects
out to Slack — nothing needs to be reachable from the internet).

**Prerequisites:** permission to install an app into your Slack workspace
(a workspace admin, or an admin who approves your app).

1. Go to [api.slack.com/apps](https://api.slack.com/apps) → **Create New
   App** → **From scratch**. Name it (e.g. "conductor") and pick your
   workspace.
2. **Socket Mode**: in the app's settings, open **Socket Mode** and turn it
   on. Slack prompts you to generate an app-level token — give it the
   `connections:write` scope and copy it (starts `xapp-`). This is
   `app_token`.
3. **Bot token scopes**: open **OAuth & Permissions** → **Scopes** → **Bot
   Token Scopes**, and add at least:
   - `app_mentions:read`, `chat:write` — mentions in, messages out
   - `reactions:read`, `reactions:write` — reaction events and the `react`
     verb
   - `commands` — slash commands (if you use them)
   - `im:write` — DMs (the `ask` verb's `to: dm`, or `post` with `user:`)
   - `channels:history`, `groups:history` — the `thread` verb and reading
     thread replies for hand-off asks
   - `files:read` — the `download` verb
   - `users:read` — display names on the `thread` verb
4. **Install the app** to your workspace (**OAuth & Permissions** → **Install
   to Workspace**). Copy the **Bot User OAuth Token** (starts `xoxb-`) — this
   is `bot_token`.
5. **Event Subscriptions**: open **Event Subscriptions**, turn it on (Socket
   Mode delivers events over the websocket, so no Request URL is needed),
   and subscribe to the bot events you want: `app_mention`, `reaction_added`,
   `message.channels` / `message.im` (for thread replies and DMs — needed
   for hand-off asks and `on: <name>.reply`).
6. **Slash Commands** (optional): **Slash Commands** → **Create New Command**
   for each command the `slash_command` event should see.
7. **Interactivity & Shortcuts** (optional, for message shortcuts and
   `options.form`): open **Interactivity & Shortcuts**, turn it on (again, no
   Request URL — Socket Mode carries it), and under **Shortcuts** add a
   **Message Shortcut** for each one your triggers use (`message_shortcut`'s
   `callback_id` filter matches the shortcut's **Callback ID**).
8. Invite the bot to any channel it should post in or receive events from
   (`/invite @your-bot-name`).

**Configure:**

```yaml
connectors:
  bot:
    use: slack
    app_token: ${SLACK_APP_TOKEN}
    bot_token: ${SLACK_BOT_TOKEN}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `app_token` | string | Socket Mode app token (`xapp-…`) — needed for events and ask replies |
| `bot_token` | string | bot token (`xoxb-…`) for the Web API verbs |
| `webhook_url` | string | an incoming-webhook URL — post-only alternative to a bot token (no events, no `react`/`ask`/`thread`/`download`) |
| `api_base` | string | override the Slack Web API base URL — for tests against a fake server; leave unset in production |

Set `bot_token` **or** `webhook_url`; `app_token` is required only if any
trigger listens on this connector's events.

## Events

Trigger with `on: <name>.<event>`. Every event publishes its facts under
`.slack` (plus a sibling `.slack_bot_token`, see [Credentials and
secrecy](#credentials-and-secrecy) below).

| event | fires when |
|-------|-----------|
| `app_mention` | the bot was @-mentioned |
| `reaction_added` | a reaction was added to a message |
| `slash_command` | a slash command was invoked |
| `message_shortcut` | a message shortcut was used on a message |
| `reply` | a thread reply or a DM, as Socket Mode delivers them |

`.slack` context (present on every event; empty string/list/map where not
applicable): `channel`, `user`, `text`, `ts`, `thread_ts`, `reaction`,
`command`, `is_bot` (the acting user is a bot), `via` (`mention` \|
`shortcut`, for a form/shortcut trigger), `callback_id` (the shortcut's
callback id), `files` (`[{id, name, mimetype}]` on the triggering message),
`form` (submitted values by field name — see [Forms](#forms)).

Filters: `channel` (scalar), `users` (list of Slack user ids), and per-event:
`reaction_added` adds `reaction`; `slash_command` adds `command`;
`message_shortcut` adds `callback_id`.

```yaml
triggers:
  - on: bot.reaction_added
    filter: { reaction: eyes }
    steps:
      - use: agent
        with: { prompt: "Someone flagged: {{.slack.text}}" }
```

### Forms

`app_mention` and `message_shortcut` triggers accept `options.form`: a modal
collected **before** the trigger fires.

```yaml
triggers:
  - on: bot.message_shortcut
    filter: { callback_id: deploy_shortcut, users: [U0123ABCD] }
    options:
      form:
        title: Deploy
        fields:
          - { name: environment, label: "Environment", type: select, options: [staging, production] }
          - { name: notes, label: "Notes (optional)", type: textarea, optional: true }
    steps:
      - use: agent
        with: { prompt: "Deploy to {{.slack.form.environment}}: {{.slack.form.notes}}" }
```

- A **shortcut** opens the modal directly; a **mention** instead posts a
  private, in-thread button (visible only to the mentioning user) that opens
  it. Either way the trigger fires once, on submission.
- Field `type` is `select` (needs `options`), `text`, or `textarea`; a
  select's submitted value is re-checked against its configured `options`
  server-side (Slack only ever offers those, but it is never trusted
  blindly).
- **Safety rule:** a form trigger, or a bare `message_shortcut` trigger (no
  form), must restrict `users:` in its `filter:` to a non-empty list, or set
  `options.any_user: true` to explicitly allow anyone in the workspace. This
  is enforced both at load (`conductor validate`) and again by the connector
  itself before a modal opens or a submission fires.

## Verbs

### `post`

Post a message.

| option | type | |
|--------|------|---|
| `channel` | string | channel id (or set `user:` for a DM) |
| `user` | string | user id to DM |
| `text` | string, required | |
| `thread_ts` | string | post into this thread |
| `ephemeral` | boolean | visible only to `user:` (needs both `channel:` and `user:`) |

Outputs: `ts`, `channel`. With a `webhook_url`-only connection, `post` sends
`{"text": …}` to the incoming webhook and both outputs come back empty.

### `react`

Add a reaction. Options: `channel`, `ts`, `emoji` (all required; colons
optional). Outputs: `ok`.

### `ask`

Present a question/draft on Slack and wait for the reply. This is
conductor's hand-off/approval primitive for Slack: posting opens a
conversation the engine itself waits on (there is no plugin-side inbox or
reply hook — see [Hand-offs](#hand-offs-and-conversation-replies)).

| option | type | |
|--------|------|---|
| `prompt` | string, required | the question to present |
| `draft` | string | editable draft text presented with the question |
| `title` | string | presentation title (default: the prompt's first line) |
| `to` | string, required | `dm` or `thread` |
| `user` | string | user id (`to: dm`) |
| `channel` | string | channel id (`to: thread`) |
| `approvers` | list | `to: thread` — only these user ids may resolve the ask (default: anyone) |
| `timeout` | string | how long to wait (default 1h, enforced by conductor) |

A reply is parsed as `approve`/`lgtm`/`\U0001F44D` (approve), `discard`/`cancel`/`no`
(discard), or anything else as a revision (an optional leading `revise:` is
stripped) — the same keywords every hand-off channel understands.

### `thread`

Read a message's thread: ordered messages with resolved author display
names, file metadata, and a permalink.

| option | type | |
|--------|------|---|
| `channel` | string, required | |
| `ts` | string, required | the thread's root ts (or any message in it) |
| `limit` | integer | max messages (default 200, at most 1000) |

Outputs: `messages` (`[{user, user_name, text, ts, files}]`, oldest first),
`text` (the thread as plain text), `permalink`, `thread_ts`, `count`,
`truncated`.

### `download`

Download a message's (or its whole thread's) files to local disk, for an
agent step's `images:` or a file-reading tool.

| option | type | |
|--------|------|---|
| `channel` | string, required | |
| `ts` | string, required | the thread's root ts (or the message's own ts with `thread: false`) |
| `thread` | boolean | every file in the thread (default true); `false`: only the message at `ts` |
| `max_files` | integer | default 10, at most 50 |
| `max_file_bytes` | integer | default 20 MiB, at most 50 MiB |
| `max_total_bytes` | integer | default 50 MiB, at most 200 MiB |

Outputs: `dir`, `files` (`[{name, path, mimetype, size}]`), `paths`, `images`
(paths of the `image/*` files), `skipped` (`[{name, reason}]`), `count`.

Files are staged under a per-plugin cache directory, with a sanitized name
(no directory components, no template-unsafe characters), never a path
outside the staging directory, and a file URL off `slack.com` (or the
connection's own `api_base`, for tests) is refused outright. The bot token is
only ever sent to those hosts.

## Hand-offs and conversation replies

Unlike the old bundled connector, this plugin keeps **no reply inbox of its
own**. `ask` posts a message and returns a `conversation_id`
(`channel:thread_ts`, or `channel:` for a DM); every thread reply or DM
message then publishes as a `reply` event carrying the same
`channel`/`thread_ts` pair. Conductor's own engine — not this plugin —
matches the reply to the pending ask, enforces `approvers`, and parses the
decision. An unconsumed `reply` (nobody is waiting on it) is simply an
ordinary event, so `on: bot.reply` works too.

The practical effect: you can `handoff: bot` on any step exactly as with the
old connector, and a Slack reply still resolves it — the wiring just lives
in conductor's generic hand-off machinery instead of a Slack-specific inbox.

## Credentials and secrecy

`.slack_bot_token` (a sibling of `.slack`, not nested under it) carries the
connection's bot token, so an action can reply directly over the Web API
with `{{.slack_bot_token}}` without a separate credential. It, like every
Slack token, is never persisted and never rendered into an agent's prompt.

## Capabilities & security

Declares egress to `slack.com:443` only, and spawns nothing. Socket Mode
dials out; this connector never listens on a port.
