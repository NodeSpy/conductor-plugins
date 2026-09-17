# `radarr` connector

Drive a self-hosted Radarr instance over its REST API v3. Movie
listing/lookup/add/delete, commands, queue, calendar, wanted/missing,
quality profiles, root folders, and health are exposed as verbs, plus an
`api` escape hatch for any endpoint a first-class verb does not cover. As a
**source**, it receives Radarr's own Webhook notification POSTs (Grab,
Download, MovieAdded, MovieDelete, MovieFileDelete, Health, Test, etc.) and
streams a normalized event per delivery.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/radarr/main.go`](../../connectors/radarr/main.go)
- **Provides:** `radarr`
- **Capabilities:** no declared egress — self-hosted software has no fixed
  public host; scope the instance's actual address with `network:`

```yaml
connectors:
  radarr:
    use: radarr
    base_url: http://radarr:7878
    api_key: ${RADARR_API_KEY}
    webhook:
      listen: ":9097"
      secret: ${RADARR_WEBHOOK_TOKEN}
    network: ["radarr:7878"]   # narrow the declared egress to the real host
triggers:
  - on: radarr.event
    filters: { event_types: [Download] }
    steps:
      - uses: radarr.movies
```

## Setup

You'll end up with a Radarr API key for verb calls, and optionally a Webhook
connection pointed at conductor for the source.

**Prerequisites:** a running Radarr instance, reachable from wherever
conductor runs, and admin access to its UI.

1. Open Radarr and go to **Settings > General**, expand **Security**.
2. Copy the **API Key** field (auto-generated; use **Reset API Key** for a
   fresh one).
3. For the source: go to **Settings > Connect**, click **+**, and choose
   **Webhook** from the connection list.
4. Set **URL** to `http://<conductor-host>:9097/radarr` (matching
   `webhook.listen`/`path` below), **Method** `POST`, and enable the
   notification triggers you want (On Grab, On Import, On Movie Added, ...).
5. Give it a shared token: add an `X-Conductor-Token` header (or append
   `?token=...` to the URL) matching `webhook.secret` — see **Source: Radarr
   Webhook connection** below for how it's verified.

```yaml
connectors:
  radarr:
    use: radarr
    base_url: http://radarr:7878
    api_key: ${RADARR_API_KEY}
    webhook:
      listen: ":9097"
      secret: ${RADARR_WEBHOOK_TOKEN}
```

No public URL for conductor to receive on? Point the Webhook connection at a
smee.io channel instead and set `webhook.smee` in place of `listen` — see
**Source: Radarr Webhook connection** below.

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string (required) | base URL of the Radarr instance, e.g. `http://radarr:7878` (no trailing `/api/v3`) |
| `api_key` | string (required) | Radarr API key (Settings > General > Security) |
| `webhook` | map | source transport: `listen`, `path` (default `/radarr`), `secret`, `allow_unsigned`, `smee` (smee.io-style SSE relay URL — receive forwarded deliveries when the endpoint has no public URL; the shared token is still checked) |

Every verb call sends the API key as the `X-Api-Key` header against
`{base_url}/api/v3/<resource>`. A non-2xx response is surfaced as a connector
error carrying the status code and response body — never as a data output.

## Source: Radarr Webhook connection

Radarr's own Connect > Webhook notification has no signing of its own — it
just POSTs an `eventType`-tagged JSON body to a URL. So this source verifies
an **optional shared token** instead of an HMAC signature: set
`webhook.secret`, then configure Radarr's Webhook connection to send it back
as either:

- an `X-Conductor-Token: <secret>` header (if Radarr's version supports
  custom headers on the Webhook connection), or
- a `?token=<secret>` query parameter appended to the webhook URL

The comparison is constant-time (SHA-256 digest comparison via
`crypto/subtle`). Like the HMAC-verified sources, this **fails closed**: a
`webhook.listen` configured with no `secret` refuses to start unless
`webhook.allow_unsigned: true` says the operator means it.

| event | fires when |
|-------|-----------|
| `event` | any Radarr webhook notification (Grab, Download, MovieAdded, MovieDelete, MovieFileDelete, Health, Test, …) — whatever notifications you enable on the Webhook connection in Radarr |

Filter context: `event_types` (matches `payload.eventType`), `movies`
(matches the movie title).

Context fields: `event_type`, `movie_title`, `year`, `tmdb_id`, `quality`
(the release/movie-file quality name, when present), `payload` (the full
posted JSON body, verbatim).

Deliveries are deduplicated on `eventType` + `tmdbId` + (`downloadId` when
present, else a per-second timestamp), so a redelivered webhook doesn't fire
the same trigger twice.

### Filtering

`filter:` matches an event's published fields (`event_types`, `movies`, plus
the context fields above). A key set to a value must match; a **list matches
any of** its values — `event_types: [Grab, Download]`. Prefix **`not_`** to
negate a field — `not_movies: [Sandbox Movie]` excludes. **`expr:` /
`not_expr:`** take an expression over the fields. Keys within one filter
object are **AND**ed. A top-level **array** of filter objects is **OR** across
them.

### Example

List the current queue whenever a download completes:

```yaml
triggers:
  - on: radarr.event
    filter:
      event_types: [Download]
    steps:
      - uses: radarr.queue
```

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb call sends the API key as the `X-Api-Key` header against
  `{base_url}/api/v3/<resource>`.
- Every verb returns `status_code` (the HTTP status) alongside its listed
  outputs. A non-2xx response is a connector error carrying the status and
  response body — never a data output.
- **`result` vs `items`** — a verb whose response is naturally a list returns
  `items`; everything else returns `result`.

Required options are marked `*`.

### Movies

- **`movies`** — list all movies known to Radarr. → `items`.
- **`movie_get`** — a single movie by id. `id`*. → `result`.
- **`lookup`** — search for a movie to add (TheMovieDB search). `term`* (a
  title, or `tmdb:<id>`). → `items`.
- **`add_movie`** — add a movie to Radarr. Either pass `tmdb_id` (TheMovieDB
  id; required unless `movie` is given), `quality_profile_id` (required
  unless `movie` is given), `root_folder_path` (required unless `movie` is
  given), `monitored` (boolean, default `true`), `minimum_availability`
  (string, default `released`), `search_for_movie` (boolean →
  `addOptions.searchForMovie`) — or pass the full `movie` map (sent verbatim
  to `POST /movie` instead of the individual fields; typically a `lookup`
  result, unmodified or edited). → `result`.
- **`delete_movie`** — remove a movie from Radarr. `id`*, `delete_files`
  (boolean, also delete the movie's files on disk), `add_import_exclusion`
  (boolean, add to the import list exclusion list). → `result`.

### Commands, queue & calendar

- **`command`** — run a Radarr command. `name`* (e.g. `MoviesSearch`,
  `MovieSearch`, `RefreshMovie`, `RescanMovie`), `movie_ids` (list →
  `movieIds`), `params` (map, merged in verbatim for anything else the
  command needs). → `result`.
- **`queue`** — the current download queue. → `result`.
- **`calendar`** — movies releasing in a date range. `start`, `end`
  (ISO-8601 dates). → `items`.
- **`wanted_missing`** — movies Radarr considers missing. → `result`.

### Reference data

- **`quality_profiles`** — configured quality profiles. → `items`.
- **`root_folders`** — configured root folders. → `items`.
- **`health`** — current health check results. → `items`.

### Escape hatch

- **`api`** — call any Radarr API v3 endpoint not covered above. `method`
  (default `GET`), `path`* (relative to `/api/v3`, e.g. `/system/status`),
  `query` (map), `body` (any, marshaled to JSON). → `result` (+ `items` when
  the response decodes to a JSON array).
## Capabilities & security

Declares **no** egress — Radarr is self-hosted with no fixed public host, so
an empty manifest is the strongest static claim this connector can make. Set
the connector instance's `network:` allowlist to the actual `host:port` of
your Radarr instance (and nothing else) so the confinement is meaningful in
practice.
