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

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string (required) | base URL of the Radarr instance, e.g. `http://radarr:7878` (no trailing `/api/v3`) |
| `api_key` | string (required) | Radarr API key (Settings > General > Security) |
| `webhook` | map | source transport: `listen`, `path` (default `/radarr`), `secret`, `allow_unsigned` |

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

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema.

| verb | maps to | notes |
|------|---------|-------|
| `movies` | `GET /movie` | outputs `items` |
| `movie_get` | `GET /movie/{id}` | `id` required |
| `lookup` | `GET /movie/lookup?term=` | `term` required (title or `tmdb:<id>`); outputs `items` |
| `add_movie` | `POST /movie` | either `tmdb_id`/`quality_profile_id`/`root_folder_path` (+ optional `monitored` default `true`, `minimum_availability` default `released`, `search_for_movie`), or a full `movie` map sent verbatim |
| `delete_movie` | `DELETE /movie/{id}` | `id` required; `delete_files`, `add_import_exclusion` |
| `command` | `POST /command` | `name` required (e.g. `MoviesSearch`, `MovieSearch`, `RefreshMovie`, `RescanMovie`); `movie_ids`; `params` merged in verbatim |
| `queue` | `GET /queue` | the current download queue |
| `calendar` | `GET /calendar` | `start`, `end` (ISO-8601); outputs `items` |
| `wanted_missing` | `GET /wanted/missing` | movies Radarr considers missing |
| `quality_profiles` | `GET /qualityprofile` | outputs `items` |
| `root_folders` | `GET /rootfolder` | outputs `items` |
| `health` | `GET /health` | outputs `items` |
| `api` | any `/api/v3/<path>` | escape hatch: `method` (default GET), `path` (relative to `/api/v3`), `query`, `body` |

### Outputs

Every verb returns:

- `result` — the decoded JSON body (object or array), for verbs that don't
  naturally produce a list
- `items` — the decoded JSON array, for verbs whose response is a list
  (`movies`, `lookup`, `calendar`, `quality_profiles`, `root_folders`,
  `health`); `api` sets `items` too, whenever its response happens to decode
  to an array
- `status_code` — the HTTP status Radarr returned

A non-2xx response is a connector error carrying the status and response
body, not a data output.

## Capabilities & security

Declares **no** egress — Radarr is self-hosted with no fixed public host, so
an empty manifest is the strongest static claim this connector can make. Set
the connector instance's `network:` allowlist to the actual `host:port` of
your Radarr instance (and nothing else) so the confinement is meaningful in
practice.
