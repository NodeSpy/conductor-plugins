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

### Event

| event | fires when |
|-------|-----------|
| `playback` | a Plex webhook notification fired — the specific event (`media.play`, `media.pause`, `media.resume`, `media.stop`, `media.scrobble`, `media.rate`, …) depends entirely on which notification types are enabled for the webhook in Plex |

**Filters:** `events`, `media_types`, `accounts` (list-contains), or the
scalar `event`, `media_type`, `account`.

**Context:** `event`, `account` (`Account.title`), `player` (`Player.title`),
`server` (`Server.title`), `media_type` (`Metadata.type`), `title`
(`Metadata.title`), `library` (`Metadata.librarySectionTitle`), `rating_key`
(`Metadata.ratingKey`).

## Verbs

| verb | request | outputs | notes |
|------|---------|---------|-------|
| `sessions` | `GET /status/sessions` | `items` | current playback sessions |
| `library_sections` | `GET /library/sections` | `items` | configured library sections |
| `scan_library` | `GET /library/sections/{section_id}/refresh` | `result` | options: `section_id` (required) |
| `search` | `GET /search?query=...` | `items` | options: `query` (required) |
| `metadata` | `GET /library/metadata/{rating_key}` | `result` | options: `rating_key` (required) |
| `recently_added` | `GET /library/recentlyAdded` or `GET /library/sections/{section_id}/recentlyAdded` | `items` | options: `section_id` (optional — server-wide when omitted) |
| `mark_watched` | `GET /:/scrobble?identifier=com.plexapp.plugins.library&key={rating_key}` | `result` | options: `rating_key` (required) |
| `mark_unwatched` | `GET /:/unscrobble?identifier=com.plexapp.plugins.library&key={rating_key}` | `result` | options: `rating_key` (required) |
| `refresh_metadata` | `PUT /library/metadata/{rating_key}/refresh` | `result` | options: `rating_key` (required) |
| `identity` | `GET /identity` | `result` | server identity/version |
| `playlists` | `GET /playlists` | `items` | configured playlists |
| `api` | any method/path | `result`/`items` | escape hatch — options: `method` (default `GET`), `path` (required), `query` (map), `body` (JSON-encoded) |

Every verb also returns `status_code` (the HTTP status) and both `result`
(the unwrapped `MediaContainer` object) and `items` (its list child, or
empty) — the table above names the one that's meaningful for that verb, but
both are always populated so a trigger can reach either.

See `Describe()` in [`connectors/plex/main.go`](../../connectors/plex/main.go)
for each verb's full option schema.

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
