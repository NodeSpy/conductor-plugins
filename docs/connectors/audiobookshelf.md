# `audiobookshelf` connector

Audiobookshelf (ABS) as a connector: libraries, items, search, scanning,
series/collections, the "me" user profile, and playback progress/listening
sessions over the ABS REST API, plus a raw `api` escape hatch. Built on the
standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/audiobookshelf/main.go`](../../connectors/audiobookshelf/main.go)
- **Provides:** `audiobookshelf`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own instance)

```yaml
connectors:
  abs:
    use: audiobookshelf
    base_url: https://abs.example.com
    token: ${ABS_TOKEN}
    network: ["abs.example.com:443"]   # narrow the declared egress to your instance
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | ABS server root, e.g. `https://abs.example.com` (no trailing `/api`) |
| `token` | string | ABS API token, sent as `Authorization: Bearer <token>` |

Every verb calls `base_url + "/api" + <endpoint>`. A non-2xx response is
returned as an error carrying the status code and response body — nothing is
swallowed.

## No source: live events are out of scope

Audiobookshelf's live-event stream is socket.io, not a plain webhook or SSE
feed. Socket.io is a stateful, bidirectional protocol with its own
handshake/upgrade framing — well outside "parse an HTTP request body"
territory. This connector deliberately does **not** implement a source; it
declares no `Events` and does not implement `StartSource`. If ABS live events
are needed, they belong in a dedicated plugin (or the daemon) with a real
socket.io client.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; most also return
either `result` (a single object) or `items` (a list).

| verb | endpoint | outputs |
|------|----------|---------|
| `libraries` | `GET /api/libraries` | `items` (hoisted from `libraries`) |
| `library_get` | `GET /api/libraries/{library_id}` | `result` |
| `library_items` | `GET /api/libraries/{library_id}/items` (`limit`, `page`, `sort`, `filter`) | `result` + `items` (hoisted from `result.results`) |
| `get_item` | `GET /api/items/{item_id}` (`?expanded=1`) | `result` |
| `search` | `GET /api/libraries/{library_id}/search?q=` | `result` |
| `scan` | `POST /api/libraries/{library_id}/scan` (`force`) | `status_code` (+ `result` if the body is non-empty) |
| `series` | `GET /api/libraries/{library_id}/series` | `items` |
| `collections` | `GET /api/libraries/{library_id}/collections` | `items` |
| `me` | `GET /api/me` | `result` |
| `get_progress` | `GET /api/me/progress/{item_id}` | `result` |
| `update_progress` | `PATCH /api/me/progress/{item_id}` (`progress`, `current_time`, `is_finished`) | `status_code` (+ `result` if the body is non-empty) |
| `playback_sessions` | `GET /api/me/listening-sessions` | `result` |
| `authorize` | `POST /api/authorize` | `result` |
| `api` | `method` + `path` (under `/api`) + `query` + `body` — escape hatch for anything without a first-class verb | `result` (object response) or `items` (array response) |

## Capabilities & security

Declares empty egress — Audiobookshelf is self-hosted, so there is no fixed
public host to declare. Set `network: ["<your-host>:443"]` on the connector
instance to narrow it to your own server. Provide a token scoped to the
least privilege the workflow needs.
