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

## Setup

You'll end up with a Lidarr API key for verb calls, and optionally a Webhook
connection pointed at conductor for the source.

**Prerequisites:** a running Lidarr instance, reachable from wherever
conductor runs, and admin access to its UI.

1. Open Lidarr and go to **Settings > General**, expand **Security**.
2. Copy the **API Key** field (auto-generated; use **Reset API Key** for a
   fresh one).
3. For the source: go to **Settings > Connect**, click **+**, and choose
   **Webhook** from the connection list.
4. Set **URL** to `http://<conductor-host>:9097/lidarr` (matching
   `webhook.listen`/`path` below), **Method** `POST`, and enable the
   notification triggers you want (On Grab, On Import, On Artist Add, ...).
5. Give it a shared token: add an `X-Conductor-Token` header (or append
   `?token=...` to the URL) matching `webhook.secret` — see **Source: the
   Webhook connection** below for how it's verified.

```yaml
connectors:
  lr:
    use: lidarr
    base_url: http://lidarr:8686
    api_key: ${LIDARR_API_KEY}
    webhook:
      listen: ":9097"
      secret: ${LIDARR_WEBHOOK_TOKEN}
```

No public URL for conductor to receive on? Point the Webhook connection at a
smee.io channel instead and set `webhook.smee` in place of `listen` — see
**Source: the Webhook connection** below.

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

### Filtering

`filter:` matches an event's published fields (`event_types`, `artists`, plus
the context fields above). A key set to a value must match; a **list matches
any of** its values — `event_types: [Grab, Download]`. Prefix **`not_`** to
negate a field — `not_artists: [Some Artist]` excludes. **`expr:` /
`not_expr:`** take an expression over the fields. Keys within one filter
object are **AND**ed. A top-level **array** of filter objects is **OR** across
them.

### Example

List an artist's albums whenever a download completes:

```yaml
triggers:
  - on: lr.event
    filter:
      event_types: [Download]
    steps:
      - uses: lr.albums
        options: { artist_id: "{{.mbid}}" }
```

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb is a plain HTTP call to `{base_url}/api/v1/<resource>`,
  authenticated with the `X-Api-Key` header.
- Every verb returns `status_code` (the HTTP status) alongside its listed
  outputs.
- **`result` vs `items`** — a verb whose response is naturally a list returns
  `items`; everything else returns `result`.

Required options are marked `*`.

### Artists

- **`artists`** — list all artists known to Lidarr. → `items`.
- **`artist_get`** — a single artist by id. `id`*. → `result`.
- **`lookup`** — search for an artist to add (MusicBrainz search). `term`*
  (an artist name, or `mbid:<musicbrainz-id>`). → `items`.
- **`add_artist`** — add an artist to Lidarr. Either pass `foreign_artist_id`
  (MusicBrainz artist id; required unless `artist` is given),
  `quality_profile_id` (required unless `artist` is given),
  `root_folder_path` (required unless `artist` is given), `monitored`
  (boolean, default `true`), `metadata_profile_id` (integer),
  `search_for_missing` (boolean → `addOptions.searchForMissingAlbums`) — or
  pass the full `artist` map (sent verbatim to `POST /artist` instead of the
  individual fields; typically a `lookup` result, unmodified or edited). →
  `result`.
- **`delete_artist`** — remove an artist from Lidarr. `id`*, `delete_files`
  (boolean, also delete the artist's files on disk), `add_import_exclusion`
  (boolean, add to the import list exclusion list). → `result`.

### Albums

- **`albums`** — list albums for an artist. `artist_id`*. → `items`.
- **`album_get`** — a single album by id. `id`*. → `result`.

### Commands, queue & calendar

- **`command`** — run a Lidarr command. `name`* (e.g. `ArtistSearch`,
  `AlbumSearch`, `RefreshArtist`, `RescanFolders`), `artist_id` (integer →
  `artistId`), `album_ids` (list → `albumIds`), `params` (map, merged in
  verbatim for anything else the command needs). → `result`.
- **`queue`** — the current download queue. → `result`.
- **`calendar`** — albums releasing in a date range. `start`, `end`
  (ISO-8601 dates). → `items`.

### Escape hatch

- **`api`** — call any Lidarr API v1 endpoint not covered above. `method`
  (default `GET`), `path`* (relative to `/api/v1`, e.g. `/system/status`),
  `query` (map), `body` (any, marshaled to JSON). → `result` (+ `items` when
  the response decodes to a JSON array).
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
