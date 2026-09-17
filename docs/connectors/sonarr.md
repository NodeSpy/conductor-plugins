# `sonarr` connector

Drive a self-hosted [Sonarr](https://sonarr.tv/) (TV series management)
instance over its REST API v3: list/add/delete series, search TheTVDB,
episodes, commands (search/refresh/rescan), the download queue, calendar,
wanted/missing episodes, quality profiles, root folders, health checks, and a
generic `api` escape hatch for any endpoint a first-class verb does not
cover. As a **source**, it receives Sonarr's own Webhook connection
deliveries (Grab, Download, SeriesAdd, SeriesDelete, EpisodeFileDelete,
Health, Test, etc. — whatever notifications you enable on the Webhook
connection in Sonarr) and streams a normalized event per delivery.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/sonarr/main.go`](../../connectors/sonarr/main.go)
- **Provides:** `sonarr`
- **Capabilities:** no declared egress (self-hosted; the operator narrows
  `network:` to their own instance)

```yaml
connectors:
  sr:
    use: sonarr
    base_url: http://sonarr:8989
    api_key: ${SONARR_API_KEY}
    webhook:
      listen: ":9096"
      secret: ${SONARR_WEBHOOK_TOKEN}
    network: ["sonarr:8989"]   # narrow the (empty) declared egress to your instance

triggers:
  - on: sr.event
    filters: { event_types: [Download] }
    steps:
      - uses: sr.episodes
        options: { series_id: "{{.tvdb_id}}" }
```

## Setup

You'll end up with a Sonarr API key for verb calls, and optionally a Webhook
connection pointed at conductor for the source.

**Prerequisites:** a running Sonarr instance, reachable from wherever
conductor runs, and admin access to its UI.

1. Open Sonarr and go to **Settings > General**, expand **Security**.
2. Copy the **API Key** field (Sonarr generates one automatically; use
   **Reset API Key** if you need a fresh one).
3. For the source: go to **Settings > Connect**, click the **+** button, and
   choose **Webhook** from the connection list.
4. Set **URL** to `http://<conductor-host>:9096/sonarr` (matching
   `webhook.listen`/`path` below), **Method** `POST`, and enable the
   notification triggers you want (On Grab, On Import, On Series Add, ...).
5. Give it a shared token: add an `X-Conductor-Token` header (or append
   `?token=...` to the URL) matching `webhook.secret` — see **Source: the
   Webhook connection** below for how it's verified.

```yaml
connectors:
  sr:
    use: sonarr
    base_url: http://sonarr:8989
    api_key: ${SONARR_API_KEY}
    webhook:
      listen: ":9096"
      secret: ${SONARR_WEBHOOK_TOKEN}
```

No public URL for conductor to receive on? Point the Webhook connection at a
smee.io channel instead and set `webhook.smee` in place of `listen` — see
**Source: the Webhook connection** below.

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | **required.** Base URL of the Sonarr instance, e.g. `http://sonarr:8989` (no trailing `/api/v3`) |
| `api_key` | string | **required.** Sonarr API key (Settings > General > Security) |
| `webhook` | map | source transport: `listen`, `path` (default `/sonarr`), `secret`, `allow_unsigned`, `smee` (smee.io-style SSE relay URL — receive forwarded deliveries when the endpoint has no public URL; the shared token is still checked) |

Every verb is a plain HTTP call to `{base_url}/api/v3/<resource>`,
authenticated with the `X-Api-Key` header. A non-2xx response is surfaced as
a plugin error carrying the status code and response body.

## Source: the Webhook connection

Sonarr has no signing of its own for outbound notifications — its **Webhook**
connection (Settings > Connect > Webhook) just `POST`s an `eventType`-tagged
JSON body to a URL whenever an event you enable fires. Because there is no
signature to verify, this source instead verifies an **optional shared
token**:

- Set `webhook.secret` to a token of your choosing.
- Configure Sonarr's Webhook connection to send it back, either as an
  `X-Conductor-Token` header (a custom header on the connection) or as a
  `?token=` query parameter on the webhook URL.
- The token is compared with a constant-time comparison (SHA-256 digest
  compare, so unequal lengths don't leak via early-exit timing).

Like the HMAC-verified sources, **this fails closed**: leaving `webhook.secret`
unset refuses to start unless you set `webhook.allow_unsigned: true` to say
explicitly that you're fronting the listener with something else that
authenticates.

Deliveries are deduplicated on `eventType` + the series' `tvdbId` + the
delivery's `downloadId` (falling back to the current second when no
`downloadId` is present, e.g. `Test`/`Health` events), so a redelivered
notification doesn't fire the same trigger twice.

### Event

| event | fires when |
|-------|-----------|
| `event` | a Sonarr webhook notification fired — the specific event (`Grab`, `Download`, `SeriesAdd`, `SeriesDelete`, `EpisodeFileDelete`, `Health`, `Test`, ...) depends entirely on which notifications you enable on the Webhook connection |

**Filters:** `event_types` (list-contains against `eventType`), `series`
(list-contains against the series title).

**Context:** `event_type`, `series_title`, `tvdb_id`, `episodes` (the
payload's `episodes[]`, verbatim), `quality` (the release/episode-file
quality name, when present), and `payload` (the full posted JSON, verbatim).

### Filtering

`filter:` matches an event's published fields (`event_types`, `series`, plus
anything under **Context** above). A key set to a value must match; a **list
matches any of** its values — `event_types: [Grab, Download]`. Prefix
**`not_`** to negate a field — `not_series: [Sandbox Show]` excludes. **`expr:`
/ `not_expr:`** take an expression over the fields. Keys within one filter
object are **AND**ed. A top-level **array** of filter objects is **OR**
across them.

### Example

Refresh episode state whenever a download completes:

```yaml
triggers:
  - on: sr.event
    filter:
      event_types: [Download]
    steps:
      - uses: sr.episodes
        options: { series_id: "{{.tvdb_id}}" }
```

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb is a plain HTTP call to `{base_url}/api/v3/<resource>`,
  authenticated with the `X-Api-Key` header.
- Every verb returns `status_code` (the HTTP status) alongside its listed
  outputs.
- **`result` vs `items`** — a verb whose response is naturally a list returns
  `items`; everything else returns `result`.

Required options are marked `*`.

### Series

- **`series`** — list all series known to Sonarr. → `items`.
- **`series_get`** — a single series by id. `id`*. → `result`.
- **`lookup`** — search for a series to add (TheTVDB search). `term`* (a
  title, or `tvdb:<id>`). → `items`.
- **`add_series`** — add a series to Sonarr. Either pass `tvdb_id` (TheTVDB
  id; required unless `series` is given), `quality_profile_id` (required
  unless `series` is given), `root_folder_path` (required unless `series` is
  given), `monitored` (boolean, default `true`), `season_folder` (boolean,
  per-season folders), `language_profile_id` (integer), `search_for_missing`
  (boolean → `addOptions.searchForMissingEpisodes`) — or pass the full
  `series` map (sent verbatim to `POST /series` instead of the individual
  fields; typically a `lookup` result, unmodified or edited). → `result`.
- **`delete_series`** — remove a series from Sonarr. `id`*, `delete_files`
  (boolean, also delete the series' files on disk), `add_import_exclusion`
  (boolean, add to the import list exclusion list). → `result`.

### Episodes

- **`episodes`** — list episodes for a series. `series_id`*. → `items`.
- **`episode_get`** — a single episode by id. `id`*. → `result`.

### Commands, queue & calendar

- **`command`** — run a Sonarr command. `name`* (e.g. `SeriesSearch`,
  `SeasonSearch`, `RefreshSeries`, `RescanSeries`), `series_id` (integer →
  `seriesId`), `season_number` (integer → `seasonNumber`), `episode_ids`
  (list → `episodeIds`), `params` (map, merged in verbatim for anything else
  the command needs). → `result`.
- **`queue`** — the current download queue. → `result`.
- **`calendar`** — episodes airing in a date range. `start`, `end` (ISO-8601
  dates). → `items`.
- **`wanted_missing`** — episodes Sonarr considers missing. → `result`.

### Reference data

- **`quality_profiles`** — configured quality profiles. → `items`.
- **`root_folders`** — configured root folders. → `items`.
- **`health`** — current health check results. → `items`.

### Escape hatch

- **`api`** — call any Sonarr API v3 endpoint not covered above. `method`
  (default `GET`), `path`* (relative to `/api/v3`, e.g. `/system/status`),
  `query` (map), `body` (any, marshaled to JSON). → `result` (+ `items` when
  the response decodes to a JSON array).
## Capabilities & security

Declares **no** egress — Sonarr is always self-hosted, so unlike a connector
with a fixed public hostname (e.g. `github`'s `api.github.com`), there is no
address this plugin can declare on the operator's behalf. Set `network:` on
the connector instance to your own Sonarr host (`host:port`) to scope its
egress; leaving it unset lets the daemon apply its own default policy for a
plugin with an empty manifest.

The webhook source listens on `webhook.listen` — bind it to an address only
your Sonarr instance (or something in front of it) can reach, and always set
`webhook.secret` in anything but a fully isolated network.
