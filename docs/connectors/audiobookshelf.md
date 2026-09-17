# `audiobookshelf` connector

Audiobookshelf (ABS) as a connector: libraries, items, search, scanning,
series/collections, the "me" user profile, playback progress/listening
sessions, and multipart file `upload` over the ABS REST API, plus a raw `api`
escape hatch. Built on the standard library's `net/http` only.

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

## Setup

You'll end up with an ABS API token and your server's base URL.

**Prerequisites:** a running Audiobookshelf instance and an admin account on
it.

1. Log into the Audiobookshelf web UI as an admin.
2. Go to **Settings → Users**.
3. Click the user whose token you want (yourself, or a dedicated automation
   user).
4. On that user's account page, copy the **API Token** shown there.
5. Note the server's root URL (no trailing `/api`) for `base_url`.

**Configure:**

```yaml
connectors:
  abs:
    use: audiobookshelf
    base_url: https://abs.example.com
    token: ${ABS_TOKEN}
    network: ["abs.example.com:443"]
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

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb's outputs include `status_code`; most also return either
  `result` (a single object) or `items` (a list).

Required options are marked `*`.

### Libraries & items

- **`libraries`** — list libraries (`GET /api/libraries`). → `items` (hoisted from `libraries`).
- **`library_get`** — get one library's details (`GET /api/libraries/{library_id}`). `library_id`*. → `result`.
- **`library_items`** — list items in a library, paginated (`GET /api/libraries/{library_id}/items`). `library_id`*, `limit` (page size), `page` (page number, 0-based), `sort` (sort field), `filter` (ABS filter expression). → `result`, `items` (hoisted from `result.results`).
- **`get_item`** — get one library item (`GET /api/items/{item_id}`). `item_id`*, `expanded` (boolean — include expanded media/library-file details). → `result`.
- **`search`** — search a library (`GET /api/libraries/{library_id}/search`). `library_id`*, `q`* (search query). → `result`.
- **`scan`** — trigger a library scan (`POST /api/libraries/{library_id}/scan`). `library_id`*, `force` (boolean — force a full re-scan). → `status_code` (+ `result` if the body is non-empty).

### Series & collections

- **`series`** — list a library's series (`GET /api/libraries/{library_id}/series`). `library_id`*. → `items`.
- **`collections`** — list a library's collections (`GET /api/libraries/{library_id}/collections`). `library_id`*. → `items`.

### Me & progress

- **`me`** — the authenticated user's profile (`GET /api/me`). → `result`.
- **`get_progress`** — get playback/reading progress for an item (`GET /api/me/progress/{item_id}`). `item_id`*. → `result`.
- **`update_progress`** — update playback/reading progress for an item (`PATCH /api/me/progress/{item_id}`). `item_id`*, `progress` (0..1 fraction complete), `current_time` (playback position in seconds), `is_finished` (boolean). → `status_code` (+ `result` if the body is non-empty).
- **`playback_sessions`** — the user's listening sessions (`GET /api/me/listening-sessions`). → `result`.
- **`authorize`** — validate the token / refresh identity (`POST /api/authorize`). → `result`.

### Upload

- **`upload`** — upload local audio/ebook/cover files into a library folder, as multipart (`POST /api/upload`). `library`* (target library ID), `folder`* (target folder ID within the library), `title`* (item title), `author`, `series`, `files`* (list of local file paths — `.m4b`/`.mp3`/`.flac`/`.opus`/`.epub`/`.pdf`/cover images and more; read from disk and sent as multipart parts, keyed on filename, not the form field name). → `status_code` (+ `result` if the body is non-empty).

### Escape hatch

- **`api`** — any ABS API endpoint not covered above. `method` (HTTP method, default `GET`), `path`* (path under `/api`, e.g. `/libraries/123/items`), `query` (map of query string parameters), `body` (JSON request body). → `result` (object response) or `items` (array response).
## Full Audible → Audiobookshelf pipeline

The `upload` verb is the import half of an end-to-end pipeline that mirrors
[`NodeSpy/audiobookshelf-import`](https://github.com/NodeSpy/audiobookshelf-import):
the [`libation`](./libation.md) connector downloads DRM-free books from Audible,
and this connector imports them into ABS. Composed in a conductor workflow:

```yaml
connectors:
  lib:
    use: libation
  abs:
    use: audiobookshelf
    base_url: https://abs.example.com
    token: ${ABS_TOKEN}
    network: ["abs.example.com:443"]

steps:
  # 1. pull any newly-available books out of Audible (DRM-free)
  - uses: lib.liberate
  # 2. import a finished download into an ABS library folder
  - uses: abs.upload
    with:
      library: ${ABS_LIBRARY_ID}
      folder: ${ABS_FOLDER_ID}
      title: "Dune"
      author: "Frank Herbert"
      files:
        - /downloads/libation/Dune.m4b
```

## Capabilities & security

Declares empty egress — Audiobookshelf is self-hosted, so there is no fixed
public host to declare. Set `network: ["<your-host>:443"]` on the connector
instance to narrow it to your own server. Provide a token scoped to the
least privilege the workflow needs.
