# `sabnzbd` connector

Drive a self-hosted [SABnzbd](https://sabnzbd.org/) Usenet downloader over its
HTTP API: queue and history inspection, adding NZB URLs, job control
(pause/resume/delete), speed limiting, status/version, and a generic `api`
escape hatch for any mode a first-class verb does not cover.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/sabnzbd/main.go`](../../connectors/sabnzbd/main.go)
- **Provides:** `sabnzbd`
- **Capabilities:** no declared egress (self-hosted; the operator narrows
  `network:` to their own instance)

```yaml
connectors:
  sab:
    use: sabnzbd
    base_url: http://sab:8080
    api_key: ${SAB_API_KEY}
    network: ["sab:8080"]   # narrow the (empty) declared egress to your instance
triggers:
  - on: gh.release
    steps:
      - id: fetch
        uses: sab.add_url
        options: { url: "https://example.com/release.nzb", category: tv }
```

## Setup

You'll end up with a SABnzbd API key for verb calls.

**Prerequisites:** a running SABnzbd instance, reachable from wherever
conductor runs, and admin access to its UI.

1. Open SABnzbd and go to **Config > General**.
2. Scroll to the **Security** section.
3. Copy the **API Key** — not the **NZB Key** next to it, which only permits
   adding jobs to the queue — using **Generate new API Key** if none is set.

```yaml
connectors:
  sab:
    use: sabnzbd
    base_url: http://sab:8080
    api_key: ${SAB_API_KEY}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | **required.** Base URL of the SABnzbd instance, e.g. `http://sab:8080` (no trailing `/api`) |
| `api_key` | string | **required.** SABnzbd API key (Config > General > API Key) |

SABnzbd's entire HTTP API is **one endpoint**: every call is a `GET` to
`{base_url}/api` with `apikey`, `output=json`, and `mode` as query parameters,
plus whatever parameters the mode itself takes. This plugin mirrors that shape
directly instead of modeling per-verb REST paths — see SABnzbd's own
[API docs](https://sabnzbd.org/wiki/advanced/api) for the full parameter
reference per mode.

## Output shape

Every verb returns `status_code` plus a `result` — the **whole decoded JSON
response**, unmodified. Verbs whose response wraps an obvious list also hoist
it into `items`:

| verb | hoisted from | into |
|------|--------------|------|
| `queue` | `result.queue.slots` | `items` |
| `history` | `result.history.slots` | `items` |
| `categories` | `result.categories` | `items` |

A non-2xx HTTP response is returned as a plugin error carrying the status code
and response body — SABnzbd's own API-level errors (e.g. a bad `apikey`) come
back as `200 OK` with `{"error": "..."}` in the body, so check `result` for
that shape when a call "succeeds" but did nothing.

## Verbs

| verb | mode | notes |
|------|------|-------|
| `queue` | `queue` | options: `start`, `limit` |
| `history` | `history` | options: `start`, `limit`, `category`, `failed_only` |
| `add_url` | `addurl` | options: `url` (required, the .nzb URL), `name` (job display name → `nzbname`), `category` (→ `cat`), `priority`, `pp` |
| `pause` | `pause` | pause the entire queue |
| `resume` | `resume` | resume the entire queue |
| `pause_job` | `queue&name=pause` | options: `value` (required, `nzo_id`) |
| `resume_job` | `queue&name=resume` | options: `value` (required, `nzo_id`) |
| `delete_job` | `queue&name=delete` | options: `value` (required, `nzo_id` or `"all"`), `del_files` |
| `set_speedlimit` | `config&name=speedlimit` | options: `value` (required, e.g. `"50"` percent or `"1M"`) |
| `status` | `fullstatus` | full server status |
| `version` | `version` | SABnzbd version |
| `categories` | `get_cats` | configured categories, hoisted into `items` |
| `api` | *(caller-supplied)* | options: `mode` (required), `params` (map of additional query parameters) — escape hatch for any mode not covered above |

See `Describe()` in [`connectors/sabnzbd/main.go`](../../connectors/sabnzbd/main.go)
for each verb's full option schema.

## Capabilities & security

Declares **no** egress — SABnzbd is always self-hosted, so unlike a connector
with a fixed public hostname (e.g. `github`'s `api.github.com`), there is no
address this plugin can declare on the operator's behalf. Set `network:` on
the connector instance to your own SABnzbd host (`host:port`) to scope its
egress; leaving it unset lets the daemon apply its own default policy for a
plugin with an empty manifest.
