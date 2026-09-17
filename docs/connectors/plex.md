# `plex` connector

Drive a [Plex Media Server](https://www.plex.tv/) over its HTTP API: sessions,
library sections, library scan/refresh, search, metadata, recently-added,
watched-state, identity, playlists, and a generic `api` escape hatch for any
endpoint a first-class verb does not cover. As a **source**, it receives
Plex's own webhook notifications (Settings > Webhooks — a Plex Pass feature)
for playback events and streams a normalized `playback` event per delivery.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/plex/main.go`](../../connectors/plex/main.go)
- **Provides:** `plex`
- **Capabilities:** no declared egress (LAN/self-hosted; the operator narrows
  `network:` to their own instance)

```yaml
connectors:
  plex:
    use: plex
    base_url: http://plex:32400
    token: ${PLEX_TOKEN}
    webhook:
      listen: ":9096"
      secret: ${PLEX_WEBHOOK_TOKEN}
    network: ["plex:32400"]   # narrow the (empty) declared egress to your instance

triggers:
  - on: plex.playback
    filters: { events: [media.play] }
    steps:
      - uses: plex.metadata
        options: { rating_key: "{{.rating_key}}" }
```

## Setup

You'll end up with an `X-Plex-Token` and your Plex Media Server's URL, enough
for this connector to call the API.

**Prerequisites:** a running Plex Media Server you have full/admin access to.

1. Sign in to the Plex Web App (`app.plex.tv`) with the account that
   administers your server.
2. Open any library item, click the **⋮** (more) menu, and choose **Get
   Info**.
3. In the info panel's lower-left corner, click **View XML** — it opens in a
   new tab.
4. Copy the `X-Plex-Token` query parameter value from that tab's URL.
5. Note your server's reachable address (`http://<host>:32400`) for
   `base_url`.

**Configure:**

```yaml
connectors:
  plex:
    use: plex
    base_url: http://plex:32400
    token: ${PLEX_TOKEN}
    network: ["plex:32400"]
```

For the playback-event webhook (a Plex Pass feature), see **Source: Plex
webhooks** below.

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | **required.** Base URL of the Plex Media Server, e.g. `http://plex:32400` |
| `token` | string | **required.** `X-Plex-Token` (Settings > Account, or see [support.plex.tv/articles/204059436](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/)) |
| `webhook` | map | source transport: `listen`, `path` (default `/plex`), `secret`, `allow_unsigned`, `smee` (smee.io-style SSE relay URL — receive forwarded deliveries when the endpoint has no public URL; the shared token is still checked) |

Every verb call sends `token` as the `X-Plex-Token` header (never a query
parameter, so it never leaks into an access log) plus `Accept:
application/json`, since Plex answers with XML by default. Plex wraps every
JSON response in a `MediaContainer` object; this plugin unwraps it uniformly:
the `MediaContainer` itself becomes `result`, and whichever of its fields
holds a JSON array (`Video`, `Directory`, `Metadata`, `Playlist`, …) becomes
`items`. A non-2xx response is surfaced as a plugin error carrying the status
and response body.

## Source: Plex webhooks

Plex's webhook feature (Plex Pass, Settings > Webhooks) has no signing of its
own — it just `POST`s `multipart/form-data` with a single `payload` form
field (the JSON event body) to a URL you configure. Because there is no
signature to verify, this source instead verifies an **optional shared
token**:

- Set `webhook.secret` to a token of your choosing.
- Configure the webhook URL in Plex with a `?token=` query parameter, e.g.
  `http://host:9096/plex?token=<secret>`.
- The token is compared with a constant-time comparison (SHA-256 digest
  compare, so unequal lengths don't leak via early-exit timing).

Like the HMAC-verified sources, **this fails closed**: leaving `webhook.secret`
unset refuses to start unless you set `webhook.allow_unsigned: true` to say
explicitly that you're fronting the listener with something else that
authenticates.

Deliveries are deduplicated on `event` + `rating_key` + the receiving second,
so a webhook redelivered in the same instant doesn't fire the same trigger
twice.

### The `playback` event

Fires once per Plex webhook delivery — the specific event (`media.play`,
`media.pause`, `media.resume`, `media.stop`, `media.scrobble`, `media.rate`,
…) depends entirely on which notification types are enabled for the webhook
in Plex's own Settings > Webhooks.

| context | notes |
|---------|-------|
| `event` | e.g. `media.play`, `media.pause`, `media.resume`, `media.stop`, `media.scrobble`, `media.rate` |
| `account` | `Account.title` — the Plex user |
| `player` | `Player.title` — the playing client |
| `server` | `Server.title` |
| `media_type` | `Metadata.type` — `movie`, `episode`, `track`, … |
| `title` | `Metadata.title` |
| `library` | `Metadata.librarySectionTitle` |
| `rating_key` | `Metadata.ratingKey` |

| filter | type | matches |
|--------|------|---------|
| `events` | list | `event` is one of these |
| `media_types` | list | `media_type` is one of these |
| `accounts` | list | `account` is one of these |
| `event` | string | `event` equals this |
| `media_type` | string | `media_type` equals this |
| `account` | string | `account` equals this |

**Filtering:** a list matches any of its values (OR); the scalar form
(`event`/`media_type`/`account`) matches a single exact value.

```yaml
connectors:
  plex: { use: plex, base_url: http://plex:32400, token: ${PLEX_TOKEN}, webhook: { listen: ":9096", secret: ${PLEX_WEBHOOK_TOKEN} } }
triggers:
  - on: plex.playback
    filters: { events: [media.play, media.resume] }
    steps:
      - uses: plex.metadata
        options: { rating_key: "{{.rating_key}}" }
```

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb returns `status_code` plus both `result` (the unwrapped
  `MediaContainer` object) and `items` (its list child — `Video`/`Directory`/
  `Metadata`/`Playlist`/… — or empty); the descriptions below name whichever
  is meaningful for that verb, but both are always populated so a trigger can
  reach either.

Required options are marked `*`.

### Sessions & libraries

- **`sessions`** — current Plex playback sessions (`GET /status/sessions`). → `items`.
- **`library_sections`** — configured library sections (`GET /library/sections`). → `items`.
- **`scan_library`** — trigger a library section scan/refresh (`GET /library/sections/{section_id}/refresh`). `section_id`*. → `result`.
- **`search`** — search the server's libraries (`GET /search?query=...`). `query`*. → `items`.
- **`recently_added`** — recently added media, server-wide or for one section (`GET /library/recentlyAdded` or `GET /library/sections/{section_id}/recentlyAdded`). `section_id` (limit to one library section; default server-wide). → `items`.
- **`playlists`** — configured playlists (`GET /playlists`). → `items`.
- **`identity`** — the connected Plex Media Server's identity/version (`GET /identity`). → `result`.

### Metadata & watch state

- **`metadata`** — metadata for a single Plex item (`GET /library/metadata/{rating_key}`). `rating_key`* (the item's Plex rating key). → `result`.
- **`mark_watched`** — mark an item watched / scrobble (`GET /:/scrobble?identifier=com.plexapp.plugins.library&key={rating_key}`). `rating_key`*. → `result`.
- **`mark_unwatched`** — mark an item unwatched / unscrobble (`GET /:/unscrobble?identifier=com.plexapp.plugins.library&key={rating_key}`). `rating_key`*. → `result`.
- **`refresh_metadata`** — refresh a single item's metadata from its agent (`PUT /library/metadata/{rating_key}/refresh`). `rating_key`*. → `result`.

### Escape hatch

- **`api`** — call any Plex Media Server endpoint not covered above. `method` (`GET` | `POST` | `PUT` | `DELETE`, default `GET`), `path`* (e.g. `/library/sections/1/all`), `query` (map of additional query parameters), `body` (JSON-encoded request body, for POST/PUT). → `result`, `items`.
## Capabilities & security

Declares **no** egress — Plex is always self-hosted/LAN, so unlike a
connector with a fixed public hostname (e.g. `github`'s `api.github.com`),
there is no address this plugin can declare on the operator's behalf. Set
`network:` on the connector instance to your own Plex Media Server host
(`host:port`) to scope its egress; leaving it unset lets the daemon apply its
own default policy for a plugin with an empty manifest.

The webhook source listens on `webhook.listen` — bind it to an address only
your Plex server (or something in front of it) can reach, and always set
`webhook.secret` in anything but a fully isolated network.
