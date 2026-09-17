# `prowlarr` connector

Drive a self-hosted [Prowlarr](https://prowlarr.com/) (indexer manager)
instance over its REST API v1: list/get indexers, per-indexer statistics, the
applications Prowlarr syncs indexers into (Sonarr/Radarr/etc.), a release
search across indexers, commands, system status, tags, and a generic `api`
escape hatch for any endpoint a first-class verb does not cover. As a
**source**, it receives Prowlarr's own Webhook connection deliveries
(`HealthIssue`, `HealthRestored`, `ApplicationUpdate`, `Test` — Prowlarr has
far fewer notification events than Sonarr since it has no media of its own to
grab/download) and streams a normalized event per delivery.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/prowlarr/main.go`](../../connectors/prowlarr/main.go)
- **Provides:** `prowlarr`
- **Capabilities:** no declared egress (self-hosted; the operator narrows
  `network:` to their own instance)

```yaml
connectors:
  pr:
    use: prowlarr
    base_url: http://prowlarr:9696
    api_key: ${PROWLARR_API_KEY}
    webhook:
      listen: ":9097"
      secret: ${PROWLARR_WEBHOOK_TOKEN}
    network: ["prowlarr:9696"]   # narrow the (empty) declared egress to your instance

triggers:
  - on: pr.event
    filters: { event_types: [HealthIssue] }
    steps:
      - uses: pr.system_status
```

## Setup

You'll end up with a Prowlarr API key for verb calls, and optionally a
Webhook connection pointed at conductor for the source.

**Prerequisites:** a running Prowlarr instance, reachable from wherever
conductor runs, and admin access to its UI.

1. Open Prowlarr and go to **Settings > General**, expand **Security**.
2. Copy the **API Key** field (auto-generated; use **Reset API Key** for a
   fresh one).
3. For the source: go to **Settings > Connect**, click **+**, and choose
   **Webhook** from the connection list.
4. Set **URL** to `http://<conductor-host>:9097/prowlarr` (matching
   `webhook.listen`/`path` below), **Method** `POST`, and enable the
   notification triggers you want — Prowlarr only offers Health
   Issue/Restored and Application Update.
5. Give it a shared token: add an `X-Conductor-Token` header (or append
   `?token=...` to the URL) matching `webhook.secret` — see **Source: the
   Webhook connection** below for how it's verified.

```yaml
connectors:
  pr:
    use: prowlarr
    base_url: http://prowlarr:9696
    api_key: ${PROWLARR_API_KEY}
    webhook:
      listen: ":9097"
      secret: ${PROWLARR_WEBHOOK_TOKEN}
```

No public URL for conductor to receive on? Point the Webhook connection at a
smee.io channel instead and set `webhook.smee` in place of `listen` — see
**Source: the Webhook connection** below.

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | **required.** Base URL of the Prowlarr instance, e.g. `http://prowlarr:9696` (no trailing `/api/v1`) |
| `api_key` | string | **required.** Prowlarr API key (Settings > General > Security) |
| `webhook` | map | source transport: `listen`, `path` (default `/prowlarr`), `secret`, `allow_unsigned`, `smee` (smee.io-style SSE relay URL — receive forwarded deliveries when the endpoint has no public URL; the shared token is still checked) |

Every verb is a plain HTTP call to `{base_url}/api/v1/<resource>`,
authenticated with the `X-Api-Key` header. A non-2xx response is surfaced as
a plugin error carrying the status code and response body.

## Source: the Webhook connection

Prowlarr has no signing of its own for outbound notifications — its
**Webhook** connection (Settings > Connect > Webhook) just `POST`s an
`eventType`-tagged JSON body to a URL whenever an event you enable fires.
Because there is no signature to verify, this source instead verifies an
**optional shared token**:

- Set `webhook.secret` to a token of your choosing.
- Configure Prowlarr's Webhook connection to send it back, either as an
  `X-Conductor-Token` header (a custom header on the connection) or as a
  `?token=` query parameter on the webhook URL.
- The token is compared with a constant-time comparison (SHA-256 digest
  compare, so unequal lengths don't leak via early-exit timing).

Like the HMAC-verified sources, **this fails closed**: leaving `webhook.secret`
unset refuses to start unless you set `webhook.allow_unsigned: true` to say
explicitly that you're fronting the listener with something else that
authenticates.

Deliveries are deduplicated on `eventType` + (the delivery's `message`, or
its `type` field when `message` is absent, falling back to the current
second when neither is present, e.g. `Test` events), so a redelivered
notification doesn't fire the same trigger twice.

### Event

| event | fires when |
|-------|-----------|
| `event` | a Prowlarr webhook notification fired — the specific event (`HealthIssue`, `HealthRestored`, `ApplicationUpdate`, `Test`, ...) depends entirely on which notifications you enable on the Webhook connection |

**Filters:** `event_types` (list-contains against `eventType`).

**Context:** `event_type`, `level` (health check level — ok/warning/error —
when present), `message` (the human-readable message, when present),
`issue_type` (the health check's `type` field identifying which check fired,
when present), `wiki_url` (a link to the relevant Servarr wiki page, when
present), `previous_version` / `new_version` (`ApplicationUpdate`: the
version updated from/to, when present), and `payload` (the full posted JSON,
verbatim).

### Filtering

`filter:` matches an event's published fields (`event_types`, plus the
context fields above). A key set to a value must match; a **list matches any
of** its values — `event_types: [HealthIssue, HealthRestored]`. Prefix
**`not_`** to negate a field — `not_event_types: [Test]` excludes. **`expr:` /
`not_expr:`** take an expression over the fields. Keys within one filter
object are **AND**ed. A top-level **array** of filter objects is **OR** across
them.

### Example

Check system status whenever a health issue fires:

```yaml
triggers:
  - on: pr.event
    filter:
      event_types: [HealthIssue]
    steps:
      - uses: pr.system_status
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

### Indexers & applications

- **`indexers`** — list all indexers configured in Prowlarr. → `items`.
- **`indexer_get`** — a single indexer by id. `id`*. → `result`.
- **`indexer_stats`** — aggregated per-indexer statistics (query/grab
  counts, response times, failures). → `result`.
- **`applications`** — the applications Prowlarr syncs indexers into
  (Sonarr/Radarr/etc.). → `items`.

### Search

- **`search`** — search for a release across indexers, matching the query
  shape Prowlarr's own UI sends. `query` (string, search term; omit for an
  indexer's default/RSS query), `indexer_ids` (list, restricts the search to
  these indexer ids — joined into a comma-separated `indexerIds` parameter;
  empty = all enabled indexers), `categories` (list, restricted to these
  Newznab/Torznab category ids — sent as repeated `categories=` parameters),
  `type` (string, e.g. `search`, `tv-search`, `movie-search`). → `items`.

### Commands & system

- **`command`** — run a Prowlarr command. `name`* (e.g.
  `ApplicationIndexerSync`, `IndexerSync`, `CheckHealth`), `params` (map,
  merged in verbatim for anything else the command needs). → `result`.
- **`system_status`** — instance version and system information. → `result`.
- **`tags`** — configured tags. → `items`.

### Escape hatch

- **`api`** — call any Prowlarr API v1 endpoint not covered above. `method`
  (default `GET`), `path`* (relative to `/api/v1`, e.g. `/system/status`),
  `query` (map), `body` (any, marshaled to JSON). → `result` (+ `items` when
  the response decodes to a JSON array).
## Capabilities & security

Declares **no** egress — Prowlarr is always self-hosted, so unlike a
connector with a fixed public hostname (e.g. `github`'s `api.github.com`),
there is no address this plugin can declare on the operator's behalf. Set
`network:` on the connector instance to your own Prowlarr host (`host:port`)
to scope its egress; leaving it unset lets the daemon apply its own default
policy for a plugin with an empty manifest.

The webhook source listens on `webhook.listen` — bind it to an address only
your Prowlarr instance (or something in front of it) can reach, and always
set `webhook.secret` in anything but a fully isolated network.
