# `synology` connector

Drive a self-hosted Synology DSM NAS over its WebAPI: system info and
utilization, storage/volume info, File Station (list/getinfo/search),
Download Station tasks, and a raw `api` escape hatch for any endpoint a
first-class verb does not cover.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/synology/main.go`](../../connectors/synology/main.go)
- **Provides:** `synology`
- **Capabilities:** no declared egress — self-hosted software has no fixed
  public host; scope the instance's actual address with `network:`

```yaml
connectors:
  nas:
    use: synology
    base_url: https://nas.example.com:5001
    username: admin
    password: ${SYNOLOGY_PASSWORD}
    network: ["nas.example.com:5001"]   # narrow the declared egress to the real host
```

## Setup

You'll end up with a DSM username/password (and, if 2FA is on, an OTP code
path) this connector logs in with.

**Prerequisites:** a running Synology DSM NAS and admin access to it.

1. In DSM, go to **Control Panel → User & Group** and create a dedicated
   user for this connector (recommended over reusing your own admin
   account).
2. Go to **Control Panel → Application** (DSM 6) or **Control Panel →
   Application Privileges** (DSM 7), and grant that user access to the apps
   the connector calls (File Station, Download Station, etc).
3. If the account has 2-step verification enabled, a fresh OTP code
   (`otp_code`) must be supplied on login — it's checked only at login time,
   not cached with the session.
4. Note the NAS's base URL (e.g. `https://nas.example.com:5001`).

**Configure:**

```yaml
connectors:
  nas:
    use: synology
    base_url: https://nas.example.com:5001
    username: ${SYNOLOGY_USERNAME}
    password: ${SYNOLOGY_PASSWORD}
    network: ["nas.example.com:5001"]
```

```yaml
triggers:
  - on: schedule.daily
    steps:
      - uses: nas.storage
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string (required) | DSM base URL, e.g. `https://nas.example.com:5001` |
| `username` | string (required) | DSM account username |
| `password` | string (required) | DSM account password |
| `otp_code` | string | 2-step verification (OTP) code, sent on login only |
| `insecure_skip_verify` | boolean | skip TLS certificate verification for this connection (default `false`) |

## Not a REST API: the Synology WebAPI

Unlike most connectors in this repo, Synology's WebAPI is not resource-path
REST. Every call — including login — is a GET (or form-encoded POST) against
one of two fixed CGI entry points, with the actual "endpoint" named entirely
by query/form parameters:

- **Login:** `GET {base_url}/webapi/auth.cgi?api=SYNO.API.Auth&version=6&method=login&account=<user>&passwd=<pass>&session=Core&format=sid[&otp_code=...]`
- **Verbs:** `GET|POST {base_url}/webapi/entry.cgi?api=<SYNO.X>&version=<n>&method=<m>&_sid=<sid>&<params...>`

Every response — success **or** failure — comes back as HTTP 200 with a
JSON envelope:

```json
{"success": true,  "data": { ... }}
{"success": false, "error": {"code": 119}}
```

API-level failure is signaled inside that envelope, not in the HTTP status
line, so the connector always inspects `success`/`error.code` rather than
trusting a 200 status code alone.

## Auth: session (sid), not a bearer token

1. A successful login returns a session ID (`sid`) in `data.sid`. This
   connector caches the sid on the connector instance (keyed by `base_url` +
   `username` + `password` + `insecure_skip_verify`) and reuses it across
   every `Invoke` call rather than logging in from scratch each time.
2. Every `entry.cgi` call echoes the cached sid back as the `_sid` query/form
   parameter.
3. If a call's envelope comes back `success:false` with one of the
   well-known "session expired" error codes — **105** (the logged-in
   session lacks permission — commonly a stale/foreign sid), **106**
   (session timeout), or **119** (sid not found / invalid session) — the
   connector clears the cached sid, logs in again, and retries the call
   **once** before surfacing an error.
4. Any other `success:false` code (e.g. `102` — API does not exist, `400` —
   bad credentials) is surfaced immediately as an error; it is never treated
   as a reason to re-login.
5. `otp_code` (2-step verification) is sent only on the login call itself —
   it is not part of the cached session's identity, since it is a one-shot
   credential rather than an ongoing one.

### A few Synology error codes worth knowing

| code | meaning |
|------|---------|
| 102 | the requested API does not exist |
| 105 | the logged-in session does not have permission |
| 106 | session timeout |
| 107 | session interrupted by duplicate login |
| 119 | sid not found (invalid session) |
| 400 | no such account or incorrect password |
| 401 | account disabled |
| 402 | permission denied |
| 403 | 2-step verification code required |
| 404 | failed to authenticate 2-step verification code |

### `insecure_skip_verify`: read this before enabling

`insecure_skip_verify: true` builds a **per-connection** `tls.Config` with
certificate verification disabled — it never changes Go's process-wide TLS
defaults, so it cannot affect any other connector or outbound call in the
same conductor process. Even so, enabling it disables all protection
against a man-in-the-middle on the path to the NAS: anyone who can intercept
the connection can read your session id and forge responses. Only enable it
for a NAS reached over a trusted network (the same LAN, or a VPN), and
prefer installing a real certificate (Synology NASes can request one from
Let's Encrypt directly in DSM) when that is possible instead.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- `uri`, `id`, `path`, and `additional` all accept either a bare string or a
  list — lists are joined with the separator Synology's API expects (comma).
- Every verb returns `status_code` (200 for both a successful and a
  `success:false` API-level response — see [above](#not-a-rest-api-the-synology-webapi)),
  plus `result` (the decoded `data` object, or omitted for an empty body,
  e.g. `dl_create`'s typical response) and, for verbs whose `data` wraps a
  known list (`data.volumes`, `data.files`, `data.tasks`) or is itself a JSON
  array, `items`.
- A transport-level failure (a non-2xx HTTP status, or a `success:false`
  envelope after any session-expired retry) is a connector error, not a data
  output — inspect it via the invocation's error, not `status_code`.

Required options are marked `*`.

### System & storage

- **`system_info`** — DSM system information (`SYNO.Core.System.info`). → `result`.
- **`utilization`** — CPU/memory/network/disk utilization (`SYNO.Core.System.Utilization.get`). → `result`.
- **`storage`** — storage pools, volumes, and disks (`SYNO.Storage.CGI.Storage.load_info`). → `items` (hoisted from `data.volumes`), `result`.

### File Station

- **`fs_list`** — list files/folders under a path (`SYNO.FileStation.List.list`). `folder_path`* (e.g. `/volume1/photo`), `additional` (extra info fields to include — a value or list, e.g. `size,time`). → `items` (hoisted from `data.files`), `result`.
- **`fs_info`** — get info for one or more paths (`SYNO.FileStation.List.getinfo`). `path`* (a path, or a list of paths), `additional`. → `items` (hoisted from `data.files`), `result`.
- **`fs_search`** — start an asynchronous File Station search under a path (`SYNO.FileStation.Search.start`). `folder_path`*, `pattern` (filename search pattern, e.g. `*.mp4`), `recursive` (boolean — search subfolders, default true). → `result` (the started search's `taskid` — poll/stop it yourself via `api`, `method: list` / `method: stop`, same `taskid`).

### Download Station

- **`dl_tasks`** — list Download Station tasks (`SYNO.DownloadStation.Task.list`). `additional` (a value or list, e.g. `detail,transfer`). → `items` (hoisted from `data.tasks`), `result`.
- **`dl_create`** — create a Download Station task (`SYNO.DownloadStation.Task.create`, POST). `uri`* (a download URI — http/ftp/magnet — or a list of them), `destination` (destination shared-folder-relative path). → `result` (typically empty).
- **`dl_delete`** — delete one or more Download Station tasks (`SYNO.DownloadStation.Task.delete`, POST). `id`* (a task id, or a list of ids), `force_complete` (boolean — treat the task as complete instead of just removing it). → `items` (the per-task delete results), `result`.

### Escape hatch

- **`api`** — any `SYNO.*` API via `entry.cgi`. `api`* (the SYNO.* API namespace, e.g. `SYNO.FileStation.List`), `method`* (the API method, e.g. `list`), `version` (API version, default `1`), `params` (map of extra query/form parameters), `http_method` (`GET` (default) or `POST`). → `result` (or `items` when `data` is a bare array), `status_code`.
## Capabilities & security

Declares **no** egress — a Synology NAS is self-hosted with no fixed public
host, so an empty manifest is the strongest static claim this connector can
make. Set the connector instance's `network:` allowlist to the actual
`host:port` of your DSM (and nothing else) so the confinement is meaningful
in practice.
