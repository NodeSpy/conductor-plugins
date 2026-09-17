# `tautulli` connector

Drive a self-hosted [Tautulli](https://tautulli.com/) (Plex monitoring/stats)
instance over its HTTP API: activity, history, libraries, users, metadata,
recently-added, server info, sending a notification, terminating a session,
and a generic `api` escape hatch for any command a first-class verb does not
cover. As a **source**, it receives Tautulli's own "Webhook" notification
agent deliveries (playback start/stop/pause, transcode decision, etc. —
whatever notification triggers you wire up in Tautulli) and streams a
normalized event per delivery.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/tautulli/main.go`](../../connectors/tautulli/main.go)
- **Provides:** `tautulli`
- **Capabilities:** no declared egress (self-hosted; the operator narrows
  `network:` to their own instance)

```yaml
connectors:
  tt:
    use: tautulli
    base_url: http://tautulli:8181
    api_key: ${TAUTULLI_API_KEY}
    webhook:
      listen: ":9097"
      secret: ${TAUTULLI_WEBHOOK_TOKEN}
    network: ["tautulli:8181"]   # narrow the (empty) declared egress to your instance

triggers:
  - on: tt.event
    filters: { actions: [play] }
    steps:
      - uses: tt.history
        options: { user: "{{.user}}", length: 5 }
```

## Setup

You'll end up with a Tautulli API key for verb calls, and optionally a
Webhook notification agent pointed at conductor for the source.

**Prerequisites:** a running Tautulli instance, reachable from wherever
conductor runs, and admin access to its UI.

1. Open Tautulli and go to **Settings > Web Interface**.
2. Scroll to the **API** section, check **Enable API**, and copy the
   **API Key** (use **Regenerate API key** for a fresh one).
3. For the source: go to **Settings > Notification Agents**, click **Add a
   new notification agent**, and choose **Webhook**.
4. Set **Webhook URL** to `http://<conductor-host>:9097/tautulli` (matching
   `webhook.listen`/`path` below), **Webhook Method** `POST`, enable the
   triggers you want (Playback Start/Stop/Pause, ...), and fill in the
   **Data** field — see the suggested template under **Source: the Webhook
   notification agent** below.
5. Give it a shared token: add an `X-Conductor-Token` header (or append
   `?token=...` to the URL) matching `webhook.secret`.

```yaml
connectors:
  tt:
    use: tautulli
    base_url: http://tautulli:8181
    api_key: ${TAUTULLI_API_KEY}
    webhook:
      listen: ":9097"
      secret: ${TAUTULLI_WEBHOOK_TOKEN}
```

No public URL for conductor to receive on? Point the notification agent's
webhook at a smee.io channel instead and set `webhook.smee` in place of
`listen` — see **Source: the Webhook notification agent** below.

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | **required.** Base URL of the Tautulli instance, e.g. `http://tautulli:8181` (no trailing `/api/v2`) |
| `api_key` | string | **required.** Tautulli API key (Settings > Web Interface > API) |
| `webhook` | map | source transport: `listen`, `path` (default `/tautulli`), `secret`, `allow_unsigned`, `smee` (smee.io-style SSE relay URL — receive forwarded deliveries when the endpoint has no public URL; the shared token is still checked) |

Tautulli's entire HTTP API is **one endpoint**: every verb is a `GET` to
`{base_url}/api/v2` with `apikey` and `cmd` as query parameters, plus whatever
parameters the command itself takes. The response is always the envelope
`{"response": {"result": "success"|"error", "message": ..., "data": ...}}`; a
`result: "error"` is surfaced as a plugin error carrying `message`. See
Tautulli's own
[API reference](https://github.com/Tautulli/Tautulli/wiki/Tautulli-API-Reference)
for the full parameter set per command.

## Source: the Webhook notification agent

Tautulli has no signing of its own for outbound notifications — its
**Webhook** notification agent just `POST`s an operator-defined JSON body
(built from Tautulli's own notification-template variables) to a URL whenever
a notification you configure in Tautulli fires. Because there is no signature
to verify, this source instead verifies an **optional shared token**:

- Set `webhook.secret` to a token of your choosing.
- Configure Tautulli's Webhook agent to send it back, either as an
  `X-Conductor-Token` header or as a `?token=` query parameter on the webhook
  URL.
- The token is compared with a constant-time comparison (SHA-256 digest
  compare, so unequal lengths don't leak via early-exit timing).

Like the HMAC-verified sources, **this fails closed**: leaving `webhook.secret`
unset refuses to start unless you set `webhook.allow_unsigned: true` to say
explicitly that you're fronting the listener with something else that
authenticates.

Suggested Tautulli notification template (Notification Agents > Webhook >
Data):

```json
{
  "action": "{action}",
  "title": "{title}",
  "user": "{username}",
  "player": "{player}",
  "media_type": "{media_type}",
  "rating_key": "{rating_key}"
}
```

Deliveries are deduplicated on `rating_key` + `action` when both are present,
so a redelivered notification doesn't fire the same trigger twice.

### Event

| event | fires when |
|-------|-----------|
| `event` | a Tautulli webhook notification fired — the specific action (`play`, `pause`, `resume`, `stop`, `transcode_decision`, ...) depends entirely on which Tautulli notification triggers you wired to the Webhook agent |

**Filters:** `actions`, `users`, `media_types` (list-contains), or the scalar
`action`, `user`, `media_type`.

**Context:** `action`, `title`, `user`, `player`, `media_type`, `rating_key`,
plus every top-level key from the posted JSON body, and `payload` (the full
posted JSON, verbatim) for anything not lifted to a named field.

### Filtering

`filter:` matches an event's published fields (`actions`/`users`/
`media_types` as any-of lists, or the scalar `action`/`user`/`media_type`,
plus anything else your notification template posts). A key set to a value
must match; a **list matches any of** its values — `actions: [play, resume]`.
Prefix **`not_`** to negate a field — `not_users: [service_account]`
excludes. **`expr:` / `not_expr:`** take an expression over the fields. Keys
within one filter object are **AND**ed. A top-level **array** of filter
objects is **OR** across them.

### Example

Pull recent playback history whenever a stream starts:

```yaml
triggers:
  - on: tt.event
    filter:
      actions: [play]
    steps:
      - uses: tt.history
        options: { user: "{{.user}}", length: 5 }
```

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb is a `GET` to `{base_url}/api/v2` with `apikey` and `cmd` as
  query parameters, plus whatever parameters the command itself takes. The
  response is always the envelope `{"response": {"result":
  "success"|"error", "message": ..., "data": ...}}`; a `result: "error"` is
  surfaced as a plugin error carrying `message`.
- Every verb returns `status_code` (the HTTP status) alongside its listed
  outputs.
- **`result` vs `items`** — a verb whose response is naturally a list
  returns `items`, hoisted from `data` directly when it's already a list, or
  from a well-known nested key when Tautulli wraps the rows one level down
  (`get_history` → `data.data`, `get_recently_added` →
  `data.recently_added`); everything else returns `result`.

Required options are marked `*`.

### Activity & history

- **`activity`** — current Plex activity (active streams, transcode
  sessions). → `result`.
- **`history`** — playback history. `user` (string, filter to one user),
  `section_id` (string, filter to one library section), `length` (integer,
  max rows), `start` (integer, row offset). → `items`.
- **`home_stats`** — the home page's "most watched"/"recently added" style
  stat blocks. → `result`.

### Libraries, users & metadata

- **`libraries`** — configured Plex libraries. → `items`.
- **`users`** — known Plex users. → `items`.
- **`metadata`** — metadata for a single Plex item. `rating_key`* (the
  item's Plex rating key). → `result`.
- **`recently_added`** — recently added media. `count` (integer, max rows),
  `section_id` (string, filter to one library section). → `items`.
- **`server_info`** — the connected Plex Media Server's identity/version. →
  `result`.

### Notifications & sessions

- **`notify`** — send a notification through a configured Tautulli
  notifier. `notifier_id`* (integer, id of the notifier agent to use),
  `subject`*, `body`*. → `result`.
- **`terminate_session`** — terminate an active Plex playback session.
  `session_key`* (the Plex session key), `message` (string, shown to the
  user). → `result`.

### Escape hatch

- **`api`** — call any Tautulli API command not covered above. `cmd`* (the
  Tautulli command, e.g. `get_plex_log`), `params` (map, additional query
  parameters for the command). → `result`.
## Capabilities & security

Declares **no** egress — Tautulli is always self-hosted, so unlike a
connector with a fixed public hostname (e.g. `github`'s `api.github.com`),
there is no address this plugin can declare on the operator's behalf. Set
`network:` on the connector instance to your own Tautulli host (`host:port`)
to scope its egress; leaving it unset lets the daemon apply its own default
policy for a plugin with an empty manifest.

The webhook source listens on `webhook.listen` — bind it to an address only
your Tautulli instance (or something in front of it) can reach, and always
set `webhook.secret` in anything but a fully isolated network.
