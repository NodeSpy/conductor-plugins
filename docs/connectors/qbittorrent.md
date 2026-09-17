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

## Setup

You'll end up with WebUI credentials the connector uses for its cookie-based
login.

**Prerequisites:** a running qBittorrent instance, reachable from wherever
conductor runs, and access to its desktop/GUI to enable the Web UI once.

1. In the qBittorrent app, go to **Tools > Options > Web UI**.
2. Check **Enable the Web User Interface (Remote control)**.
3. Set/confirm the **IP address** and **Port** (default `8080`).
4. Under **Authentication**, set a **Username** and **Password** — change
   these from the `admin`/`adminadmin` default before exposing the port
   anywhere reachable.
5. Click **OK** to save; the WebUI is now reachable at `http://<host>:<port>`.

```yaml
connectors:
  qbit:
    use: qbittorrent
    base_url: http://qbit:8080
    username: admin
    password: ${QBIT_PASSWORD}
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

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb is a plain HTTP call to `{base_url}/api/v2/<path>`, over the
  cookie-authenticated session described above.
- **`hashes`**, **`tags`**, and **`urls`** all accept either a bare string or
  a list — lists are joined with the separator qBittorrent's API expects
  (`|` for hashes, `,` for tags, newline for `urls`).
- Every verb returns `status_code` (the HTTP status) and `result` — the
  decoded JSON body (object or array), or the raw trimmed text for
  qBittorrent's plain-text responses (`Ok.`, a bare version string, …).
  `items` is set alongside `result` when the response is a JSON array (e.g.
  `torrents`). A non-2xx response is a connector error carrying the status
  and response body, not a data output.

Required options are marked `*`.

### Torrents

- **`torrents`** — list torrents. `filter` (string, one of
  `all`|`downloading`|`seeding`|`completed`|`paused`|`active`|`inactive`|`resumed`|`stalled`|`stalled_uploading`|`stalled_downloading`|`errored`),
  `category` (string), `tag` (string), `sort` (string, e.g. `added_on`,
  `name`, `size`), `hashes` (a hash or list of hashes, restricts the
  result). → `result`, `items`.
- **`torrent_properties`** — a single torrent's detailed properties. `hash`*.
  → `result`.
- **`add`** — add torrents by magnet link or URL. Magnet/URL adds only —
  uploading a `.torrent` file's raw bytes is out of scope for this verb.
  `urls`* (a magnet/URL, or a list of them, sent newline-joined), `category`
  (string), `tags` (a tag or list of tags), `paused` (boolean, add without
  starting), `savepath` (string, download destination path), `rename`
  (string, rename the added torrent — single-URL adds only). → `result`.
- **`delete`** — delete torrents. `hashes`* (a hash, list of hashes, or
  `"all"`), `delete_files` (boolean, also delete the downloaded files). →
  `result`.
- **`pause`** — pause torrents. `hashes`*. → `result`.
- **`resume`** — resume torrents. `hashes`*. → `result`.
- **`recheck`** — force-recheck torrents. `hashes`*. → `result`.
- **`set_category`** — set torrents' category. `hashes`*, `category`*. →
  `result`.
- **`add_tags`** — add tags to torrents. `hashes`*, `tags`* (a tag or list
  of tags). → `result`.
- **`remove_tags`** — remove tags from torrents. `hashes`*, `tags`* (a tag
  or list of tags). → `result`.

### Transfer & speed limits

- **`set_speed_limits`** — set the global download and/or upload speed
  limit; at least one of `download`/`upload` is required, and each provided
  limit is applied with its own API call (`setDownloadLimit` /
  `setUploadLimit`). `download` (integer, bytes/sec, `0` = unlimited),
  `upload` (integer, bytes/sec, `0` = unlimited). → `result`.
- **`transfer_info`** — global transfer statistics and current speed
  limits. → `result`.
- **`app_version`** — the qBittorrent application version. → `result`.

### Escape hatch

- **`api`** — call any qBittorrent WebUI API v2 endpoint not covered above.
  `method`* (`GET` | `POST`), `path`* (relative to `/api/v2`, e.g.
  `sync/maindata`), `params` (map, GET query parameters or POST form
  fields). → `result`.
## Capabilities & security

Declares **no** egress — qBittorrent is self-hosted with no fixed public
host, so an empty manifest is the strongest static claim this connector can
make. Set the connector instance's `network:` allowlist to the actual
`host:port` of your qBittorrent WebUI (and nothing else) so the confinement
is meaningful in practice.
