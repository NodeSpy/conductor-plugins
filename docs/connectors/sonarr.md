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

## Verbs

| verb | request | outputs | notes |
|------|---------|---------|-------|
| `series` | `GET /series` | `items` | all series known to Sonarr |
| `series_get` | `GET /series/{id}` | `result` | options: `id` (required) |
| `lookup` | `GET /series/lookup?term=` | `items` | search for a series to add (TheTVDB search) — options: `term` (required) |
| `add_series` | `POST /series` | `result` | see below |
| `delete_series` | `DELETE /series/{id}` | `result` | options: `id` (required), `delete_files`, `add_import_exclusion` |
| `episodes` | `GET /episode?seriesId=` | `items` | options: `series_id` (required) |
| `episode_get` | `GET /episode/{id}` | `result` | options: `id` (required) |
| `command` | `POST /command` | `result` | see below |
| `queue` | `GET /queue` | `result` | current download queue |
| `calendar` | `GET /calendar` | `items` | options: `start`, `end` (ISO-8601 dates) |
| `wanted_missing` | `GET /wanted/missing` | `result` | episodes Sonarr considers missing |
| `quality_profiles` | `GET /qualityprofile` | `items` | configured quality profiles |
| `root_folders` | `GET /rootfolder` | `items` | configured root folders |
| `health` | `GET /health` | `items` | current health check results |
| `api` | any `method`/`path`/`query`/`body` | `result` (+ `items` when the response is a JSON array) | escape hatch for any endpoint not covered above |

Every verb also returns `status_code` (the HTTP status).

**`add_series`** either builds the request from individual options —
`tvdb_id`, `quality_profile_id`, `root_folder_path` (all required unless
`series` is given), `monitored` (default `true`), `season_folder`,
`language_profile_id`, `search_for_missing` (becomes
`addOptions.searchForMissingEpisodes`) — or, when `series` (a map) is given,
sends that map to `POST /series` verbatim, ignoring the individual fields.
Use the full passthrough when you already have a series object from
`lookup` and want to add it unmodified (or with your own edits).

**`command`** posts `{name, ...}` to `POST /command`. `name` is required
(e.g. `SeriesSearch`, `SeasonSearch`, `RefreshSeries`, `RescanSeries`); the
convenience options `series_id` → `seriesId`, `season_number` →
`seasonNumber`, `episode_ids` → `episodeIds` are merged in when present, and
`params` (a map) is merged in last for anything else a given command needs.

See `Describe()` in
[`connectors/sonarr/main.go`](../../connectors/sonarr/main.go) for each
verb's full option schema.

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
