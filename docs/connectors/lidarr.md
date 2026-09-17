# `lidarr` connector

Drive a self-hosted [Lidarr](https://lidarr.audio/) (music library management)
instance over its REST API v1: list/add/delete artists, search MusicBrainz,
albums, commands (search/refresh/rescan), the download queue, calendar, and a
generic `api` escape hatch for any endpoint a first-class verb does not
cover. As a **source**, it receives Lidarr's own Webhook connection
deliveries (Grab, Download, Rename, Retag, ArtistAdd, ArtistDelete,
AlbumDelete, Health, HealthRestored, ApplicationUpdate, Test, etc. —
whatever notifications you enable on the Webhook connection in Lidarr) and
streams a normalized event per delivery.

Lidarr is a Servarr app almost identical to [Sonarr](sonarr.md), but for
**MUSIC**: series become artists, episodes become albums, and TheTVDB search
becomes a MusicBrainz search.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/lidarr/main.go`](../../connectors/lidarr/main.go)
- **Provides:** `lidarr`
- **Capabilities:** no declared egress (self-hosted; the operator narrows
  `network:` to their own instance)

```yaml
connectors:
  lr:
    use: lidarr
    base_url: http://lidarr:8686
    api_key: ${LIDARR_API_KEY}
    webhook:
      listen: ":9097"
      secret: ${LIDARR_WEBHOOK_TOKEN}
    network: ["lidarr:8686"]   # narrow the (empty) declared egress to your instance

triggers:
  - on: lr.event
    filters: { event_types: [Download] }
    steps:
      - uses: lr.albums
        options: { artist_id: "{{.mbid}}" }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | **required.** Base URL of the Lidarr instance, e.g. `http://lidarr:8686` (no trailing `/api/v1`) |
| `api_key` | string | **required.** Lidarr API key (Settings > General > Security) |
| `webhook` | map | source transport: `listen`, `path` (default `/lidarr`), `secret`, `allow_unsigned`, `smee` (smee.io-style SSE relay URL — receive forwarded deliveries when the endpoint has no public URL; the shared token is still checked) |

Every verb is a plain HTTP call to `{base_url}/api/v1/<resource>`,
authenticated with the `X-Api-Key` header. A non-2xx response is surfaced as
a plugin error carrying the status code and response body.

## Source: the Webhook connection

Lidarr has no signing of its own for outbound notifications — its **Webhook**
connection (Settings > Connect > Webhook) just `POST`s an `eventType`-tagged
JSON body to a URL whenever an event you enable fires. Because there is no
signature to verify, this source instead verifies an **optional shared
token**:

- Set `webhook.secret` to a token of your choosing.
- Configure Lidarr's Webhook connection to send it back, either as an
  `X-Conductor-Token` header (a custom header on the connection) or as a
  `?token=` query parameter on the webhook URL.
- The token is compared with a constant-time comparison (SHA-256 digest
  compare, so unequal lengths don't leak via early-exit timing).

Like the HMAC-verified sources, **this fails closed**: leaving `webhook.secret`
unset refuses to start unless you set `webhook.allow_unsigned: true` to say
explicitly that you're fronting the listener with something else that
authenticates.

Deliveries are deduplicated on `eventType` + the artist's MusicBrainz id
(`mbId`) + the delivery's `downloadId` (falling back to the current second
when no `downloadId` is present, e.g. `Test`/`Health` events), so a
redelivered notification doesn't fire the same trigger twice.

### Event

| event | fires when |
|-------|-----------|
| `event` | a Lidarr webhook notification fired — the specific event (`Grab`, `Download`, `Rename`, `Retag`, `ArtistAdd`, `ArtistDelete`, `AlbumDelete`, `Health`, `ApplicationUpdate`, `Test`, ...) depends entirely on which notifications you enable on the Webhook connection |

**Filters:** `event_types` (list-contains against `eventType`), `artists`
(list-contains against the artist name).

**Context:** `event_type`, `artist_title`, `mbid` (the artist's MusicBrainz
id, `payload.artist.mbId`), `albums` (normalized to a list — Grab's
`albums[]` as-is, Download/Rename/Retag's single `album` wrapped in a
one-element list, `nil` when the payload carries neither), `quality` (the
release/track-file quality name, when present), and `payload` (the full
posted JSON, verbatim).

## Verbs

| verb | request | outputs | notes |
|------|---------|---------|-------|
| `artists` | `GET /artist` | `items` | all artists known to Lidarr |
| `artist_get` | `GET /artist/{id}` | `result` | options: `id` (required) |
| `lookup` | `GET /artist/lookup?term=` | `items` | search for an artist to add (MusicBrainz search) — options: `term` (required, e.g. an artist name or `mbid:<musicbrainz-id>`) |
| `add_artist` | `POST /artist` | `result` | see below |
| `delete_artist` | `DELETE /artist/{id}` | `result` | options: `id` (required), `delete_files`, `add_import_exclusion` |
| `albums` | `GET /album?artistId=` | `items` | options: `artist_id` (required) |
| `album_get` | `GET /album/{id}` | `result` | options: `id` (required) |
| `command` | `POST /command` | `result` | see below |
| `queue` | `GET /queue` | `result` | current download queue |
| `calendar` | `GET /calendar` | `items` | options: `start`, `end` (ISO-8601 dates) |
| `api` | any `method`/`path`/`query`/`body` | `result` (+ `items` when the response is a JSON array) | escape hatch for any endpoint not covered above |

Every verb also returns `status_code` (the HTTP status).

**`add_artist`** either builds the request from individual options —
`foreign_artist_id` (a MusicBrainz artist id), `quality_profile_id`,
`root_folder_path` (all required unless `artist` is given), `monitored`
(default `true`), `metadata_profile_id`, `search_for_missing` (becomes
`addOptions.searchForMissingAlbums`) — or, when `artist` (a map) is given,
sends that map to `POST /artist` verbatim, ignoring the individual fields.
Use the full passthrough when you already have an artist object from
`lookup` and want to add it unmodified (or with your own edits).

**`command`** posts `{name, ...}` to `POST /command`. `name` is required
(e.g. `ArtistSearch`, `AlbumSearch`, `RefreshArtist`, `RescanFolders`); the
convenience options `artist_id` → `artistId`, `album_ids` → `albumIds` are
merged in when present, and `params` (a map) is merged in last for anything
else a given command needs.

See `Describe()` in
[`connectors/lidarr/main.go`](../../connectors/lidarr/main.go) for each
verb's full option schema.

## Capabilities & security

Declares **no** egress — Lidarr is always self-hosted, so unlike a connector
with a fixed public hostname (e.g. `github`'s `api.github.com`), there is no
address this plugin can declare on the operator's behalf. Set `network:` on
the connector instance to your own Lidarr host (`host:port`) to scope its
egress; leaving it unset lets the daemon apply its own default policy for a
plugin with an empty manifest.

The webhook source listens on `webhook.listen` — bind it to an address only
your Lidarr instance (or something in front of it) can reach, and always set
`webhook.secret` in anything but a fully isolated network.
