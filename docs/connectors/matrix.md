# `matrix` connector

Matrix as a connector: send messages/events and manage rooms over the
Client-Server API, plus a **source** that streams new room messages (and
invites) via a `/sync` long-poll loop. Built only on `net/http` and the
standard library — no third-party Matrix SDK.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/matrix/main.go`](../../connectors/matrix/main.go)
- **Provides:** `matrix`
- **Capabilities:** none declared (the homeserver host is operator-configured
  per instance, documented in `Connection.homeserver` instead of a fixed
  egress list; see below)

```yaml
connectors:
  chat:
    use: matrix
    homeserver: https://matrix.example.org
    access_token: ${MATRIX_ACCESS_TOKEN}
    user_id: "@bot:example.org"
```

Unlike this repo's webhook-based connectors (`github`, `sentry`), a Matrix
account has no webhook push model of its own — the source here polls
`/sync` on the homeserver instead of listening for inbound HTTP. It carries
no listener, so it needs no `webhook:`/`listen:` block and no HMAC secret.

## Setup

Get an access token for a Matrix account — ideally a dedicated bot user, not
your personal login.

**Prerequisites:** a Matrix account on some homeserver (e.g. `matrix.org`, or
your own).

1. Register a dedicated bot user on your homeserver (recommended), or use an
   existing account.
2. Easiest path: in Element, log in as that user, then go to **Settings** →
   **Help & About** → scroll to **Advanced** → **Access Token** and copy it.
3. Alternative (scriptable): `POST /_matrix/client/v3/login` on your
   homeserver with the user's credentials, e.g.:
   `curl -XPOST -d '{"type":"m.login.password","user":"bot","password":"..."}' https://matrix.example.org/_matrix/client/v3/login`
   and read `access_token` from the response.
4. Note the homeserver base URL and the bot's full user id
   (`@bot:example.org`).

**Configure:**

```yaml
connectors:
  chat:
    use: matrix
    homeserver: https://matrix.example.org
    access_token: ${MATRIX_ACCESS_TOKEN}
    user_id: "@bot:example.org"
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `homeserver` | string | **required.** Homeserver base URL, e.g. `https://matrix.example.org` |
| `access_token` | string | **required.** The bot/account access token |
| `user_id` | string | the bot's own Matrix user id, e.g. `@bot:example.org` (informational; not required by the API itself) |
| `api_base` | string | override the Client-Server API base URL (default: `homeserver` + `/_matrix/client/v3`); for tests or non-standard deployments |

Every request carries `Authorization: Bearer <access_token>`. A non-2xx
response from the homeserver is surfaced as an invocation error carrying the
HTTP status and response body.

## Source: `/sync` long-poll

`StartSource` runs `GET /sync?timeout=30000[&since=<token>]` in a loop,
tracking the returned `next_batch` as the next call's `since`. On the very
first sync (no prior `since`), the token is recorded but **no backlog events
fire** — only messages/invites that arrive after the source starts. A sync
error backs off ~5s before retrying; the loop exits promptly when the
instance is stopped.

| event | fires when |
|-------|-----------|
| `message` | a new `m.room.message` event arrives in a joined room |
| `invite` | the connected account is invited to a room (`m.room.member`, `membership: invite`) |

Filter context for `message`: `room_id`, `sender`, `body`, `msgtype`,
`event_id`, `formatted_body` (plus plural `room_ids`/`senders`/`msgtypes`
aliases carrying the same value, for the daemon's list-contains filter
evaluator — the same convention the `sentry` connector uses for
`levels`/`projects`/`environments`). Filter context for `invite`: `room_id`,
`sender`, `user_id`, `event_id` (plus `room_ids`/`senders` aliases). Events
are deduped by `event_id`.

```yaml
triggers:
  - on: chat.message
    filters: { msgtypes: ["m.text"] }
    steps:
      - uses: chat.send_message
        options: { room_id: "{{.room_id}}", body: "got it" }
```

## Verbs

Selected by `uses: <name>.<verb>`. Every verb returns the raw decoded API
response under `result`; verbs that create an event also return its
`event_id` directly for convenience. Transaction ids (for `send_message`,
`send_notice`, `send_event`, `redact`) are minted from
`time.Now().UnixNano()`.

| verb | options | Client-Server call |
|------|---------|---------------------|
| `send_message` | `room_id`, `body`, `msgtype` (default `m.text`), `formatted_body`, `format` | `PUT /rooms/{roomId}/send/m.room.message/{txn}` |
| `send_notice` | `room_id`, `body` | `PUT /rooms/{roomId}/send/m.room.message/{txn}` (`msgtype: m.notice`) |
| `send_event` | `room_id`, `event_type`, `content` | `PUT /rooms/{roomId}/send/{event_type}/{txn}` |
| `join_room` | `room_id_or_alias` | `POST /join/{id}` |
| `leave_room` | `room_id` | `POST /rooms/{roomId}/leave` |
| `invite` | `room_id`, `user_id` | `POST /rooms/{roomId}/invite` |
| `redact` | `room_id`, `event_id`, `reason` | `PUT /rooms/{roomId}/redact/{eventId}/{txn}` |
| `set_name` | `room_id`, `value` | `PUT /rooms/{roomId}/state/m.room.name` |
| `set_topic` | `room_id`, `value` | `PUT /rooms/{roomId}/state/m.room.topic` |
| `get_messages` | `room_id`, `limit` (default 10), `from` | `GET /rooms/{roomId}/messages` (`dir=b`) |
| `whoami` | — | `GET /account/whoami` |
| `api` | `method`, `path`, `query`, `body` | escape hatch: any endpoint under the client/v3 base |

## Capabilities & security

Declares an empty capability manifest: the homeserver host is supplied per
connection instance rather than being a fixed, known-in-advance API host (the
way `api.github.com` is for the `github` connector), so there is nothing
static to declare. Narrow reachability per instance with `network:` naming
the actual homeserver host, and provide an access token scoped to only the
rooms/actions this instance actually needs.
