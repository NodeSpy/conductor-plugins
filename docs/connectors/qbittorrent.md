# `qbittorrent` connector

Drive a self-hosted qBittorrent instance over its WebUI API v2. The torrent
lifecycle (add, delete, pause, resume, recheck), categories/tags, transfer
stats and global speed limits are exposed as verbs, plus an `api` escape
hatch for any endpoint a first-class verb does not cover.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/qbittorrent/main.go`](../../connectors/qbittorrent/main.go)
- **Provides:** `qbittorrent`
- **Capabilities:** no declared egress — self-hosted software has no fixed
  public host; scope the instance's actual address with `network:`

```yaml
connectors:
  qbit:
    use: qbittorrent
    base_url: http://qbit:8080
    username: admin
    password: ${QBIT_PASSWORD}
    network: ["qbit:8080"]   # narrow the declared egress to the real host
triggers:
  - on: schedule.daily
    steps:
      - uses: qbit.torrents
        options: { filter: completed }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string (required) | qBittorrent WebUI base URL, e.g. `http://qbit:8080` (no trailing path) |
| `username` | string | WebUI username (omit if the host has authentication disabled/bypassed for this client) |
| `password` | string | WebUI password |

## Cookie (SID) login flow

qBittorrent's WebUI API is cookie-authenticated, not token-authenticated:

1. If `username`/`password` are set, the connector's first request for a
   connector instance calls `POST {base_url}/api/v2/auth/login` with the
   credentials form-encoded.
2. qBittorrent answers with HTTP 200 and a plain-text body — `Ok.` on success,
   `Fails.` on bad credentials — and, on success, a `Set-Cookie: SID=...`
   header. Because the failure case is still a 200, the connector checks the
   body explicitly rather than trusting the status code.
3. The `SID` cookie is cached on the connector instance's client and sent as
   `Cookie: SID=...` on every subsequent verb call, so login happens once per
   instance rather than once per call.
4. If a later call gets back HTTP 403 (an expired session), the connector
   clears the cached SID, logs in again, and retries the call once.
5. Every request also sends `Referer`/`Origin` set to `base_url`, since
   qBittorrent's WebUI rejects requests whose Referer doesn't match its own
   host as a CSRF precaution.

If `username` is empty, the connector never calls `/auth/login` at all — for
hosts where WebUI authentication is disabled or bypassed for the connecting
client.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema.

| verb | maps to | notes |
|------|---------|-------|
| `torrents` | `GET /torrents/info` | `filter`, `category`, `tag`, `sort`, `hashes` (hash, list, or omit for all); outputs `items` |
| `torrent_properties` | `GET /torrents/properties` | `hash` required |
| `add` | `POST /torrents/add` | `urls` (magnet/URL, or list — sent newline-joined); `category`, `tags`, `paused`, `savepath`, `rename`. **Magnet/URL adds only** — uploading a `.torrent` file's raw bytes is out of scope for this verb |
| `delete` | `POST /torrents/delete` | `hashes` (hash, list, or `"all"`), `delete_files` |
| `pause` | `POST /torrents/pause` | `hashes` |
| `resume` | `POST /torrents/resume` | `hashes` |
| `recheck` | `POST /torrents/recheck` | `hashes` |
| `set_category` | `POST /torrents/setCategory` | `hashes`, `category` |
| `add_tags` | `POST /torrents/addTags` | `hashes`, `tags` |
| `remove_tags` | `POST /torrents/removeTags` | `hashes`, `tags` |
| `set_speed_limits` | `POST /transfer/setDownloadLimit` and/or `POST /transfer/setUploadLimit` | `download`/`upload` (bytes/sec); each provided limit is applied with its own API call |
| `transfer_info` | `GET /transfer/info` | global transfer stats and current limits |
| `app_version` | `GET /app/version` | plain-text version string, surfaced as `result` |
| `api` | any `/api/v2/<path>` | escape hatch: `method` (GET/POST), `path` (relative to `/api/v2`), `params` (query for GET, form fields for POST) |

`hashes`, `tags`, and `urls` all accept either a bare string or a list — lists
are joined with the separator qBittorrent's API expects (`|` for hashes, `,`
for tags, newline for `urls`).

### Outputs

Every verb returns:

- `result` — the decoded JSON body (object or array), or the raw trimmed text
  for qBittorrent's plain-text responses (`Ok.`, a bare version string, …)
- `items` — set alongside `result` when the response is a JSON array (e.g.
  `torrents`)
- `status_code` — the HTTP status qBittorrent returned

A non-2xx response is a connector error carrying the status and response
body, not a data output — inspect it via the invocation's error, not
`status_code`.

## Capabilities & security

Declares **no** egress — qBittorrent is self-hosted with no fixed public
host, so an empty manifest is the strongest static claim this connector can
make. Set the connector instance's `network:` allowlist to the actual
`host:port` of your qBittorrent WebUI (and nothing else) so the confinement
is meaningful in practice.
