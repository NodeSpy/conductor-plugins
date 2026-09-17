# `google-drive` connector

Google Drive as a connector: list/search files, get file metadata, download
and upload file content, create folders, update metadata, delete files, and
manage permissions over the Drive v3 API (`https://www.googleapis.com/drive/v3`),
plus a raw `api` escape hatch. Built on the standard library's `net/http`
only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/google-drive/main.go`](../../connectors/google-drive/main.go)
- **Provides:** `google-drive`
- **Capabilities:** egress `["www.googleapis.com:443"]`

## Managed OAuth2 — this plugin never talks to Google's OAuth2 endpoints

`google-drive` authenticates via conductor's **managed OAuth2**, not its own
token exchange. `Describe().Auth` bakes in Google's OAuth2 endpoints and the
`https://www.googleapis.com/auth/drive` scope; the daemon runs the actual
authorization-code flow against `accounts.google.com` /
`oauth2.googleapis.com` and injects a fresh, auto-rotated bearer token into
every verb call. The plugin's only egress is `www.googleapis.com` (both the
Drive v3 REST API and its upload host share this domain) — it never sees a
client secret and never makes a token-exchange request itself.

```yaml
connectors:
  google-drive:
    use: google-drive
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: google-drive   # where the daemon persists the rotated token
```

After the connector is configured, run the one-time login:

```sh
conductor connector auth google-drive
```

If no token has been configured/logged in yet, every verb fails fast with a
`CodeInvalidParams` error pointing back at this command, rather than sending
an unauthenticated request.

`Describe().Auth.AuthParams` bakes in `access_type=offline` and
`prompt=consent` on the consent URL, so Google's authorization-code exchange
returns a refresh token (without these, a repeat consent screen only returns
an access token).

## Setup

Managed OAuth2: register an app once in Google Cloud, then run
`conductor connector auth google-drive` for the one-time browser login —
conductor stores and refreshes the token from there.

**Prerequisites:** a Google account and a Google Cloud project.

1. [console.cloud.google.com](https://console.cloud.google.com) → create or
   select a project.
2. **APIs & Services → Library** → enable the **Google Drive API**.
3. **APIs & Services → OAuth consent screen** → **External** (unless on a
   Workspace org) → fill in app name/support email → add the
   `https://www.googleapis.com/auth/drive` scope. While the app is in
   **Testing**, add your own account under **Test users**.
4. **APIs & Services → Credentials → Create credentials → OAuth client ID**
   → application type **Web application**.
5. Under **Authorized redirect URIs**, add conductor's local callback:
   `http://localhost:8400/callback` (override with the `auth:` block's
   `redirect_uri` if the daemon uses a different port).
6. Copy the **Client ID** and **Client secret**, put them in the `auth:`
   block below, then run `conductor connector auth google-drive`.

```yaml
connectors:
  google-drive:
    use: google-drive
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: google-drive
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_base` | string | Overrides `https://www.googleapis.com/drive/v3` (tests only). |
| `upload_base` | string | Overrides `https://www.googleapis.com/upload/drive/v3` (tests only). |

The `auth:` block (grant, client credentials, token vault) is daemon-managed
configuration, not a connection field this plugin ever sees — it is not part
of `Describe().Connection`.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`. A non-2xx
response is returned as an error carrying the status code and response body
— nothing is swallowed.

| verb | endpoint | outputs |
|------|----------|---------|
| `files` | `GET /files` (`q`, `pageSize`, `fields`, `orderBy`, `spaces`, `pageToken`) | `items` (hoisted from `files`), `result` (full decoded response, so `nextPageToken` is reachable) |
| `file_get` | `GET /files/{file_id}` (`fields`) | `result` |
| `file_download` | `GET /files/{file_id}?alt=media` | `content_base64` (base64-encoded raw bytes), `content_type` (response `Content-Type`) |
| `file_upload` | `POST {upload_base}/files?uploadType=multipart` — reads a local file (`path`) and sends it as a `multipart/related` body: part 1 is JSON metadata (`name`, `parents`, `mimeType`), part 2 is the file's raw bytes | `result` |
| `file_create_folder` | `POST /files` (body `{name, mimeType: "application/vnd.google-apps.folder", parents}`) | `result` |
| `file_update_metadata` | `PATCH /files/{file_id}` (body is the `metadata` option, a map of Drive file fields) | `result` |
| `file_delete` | `DELETE /files/{file_id}` | — |
| `permissions` | `GET /files/{file_id}/permissions` | `items` (hoisted from `permissions`) |
| `permission_create` | `POST /files/{file_id}/permissions` (body `{role, type, emailAddress}`) | `result` |
| `api` | `method` + `path` (under `api_base`) + `query` + `body` — escape hatch for anything without a first-class verb, including writes | `result` (object response) or `items` (array response) |

Every request sends `Authorization: Bearer <injected token>`.

### `file_upload`'s multipart/related body

Drive's multipart upload protocol
(https://developers.google.com/drive/api/guides/manage-uploads#multipart) is
`multipart/related`, not the `multipart/form-data` shape used by e.g. the
`audiobookshelf` connector's `upload` verb: each part carries only a
`Content-Type` header, no `Content-Disposition`/form-field name. The
connector builds this with `mime/multipart`'s `Writer.CreatePart` (rather
than `CreateFormFile`, which is form-data-specific): part 1 is
`application/json; charset=UTF-8` containing `{name, parents, mimeType}`;
part 2 carries the file's own MIME type (from the `mime_type` option, or
guessed from the local path's extension, defaulting to
`application/octet-stream`) and streams the file's bytes read from the local
`path` option. The request's `Content-Type` header is set to
`multipart/related; boundary=<writer boundary>`.

## Capabilities & security

Declares egress `www.googleapis.com:443` only — the daemon, not this plugin,
talks to `accounts.google.com` / `oauth2.googleapis.com` for the OAuth2
exchange. Scope the `auth:` block's requested scopes to the least privilege
the workflow needs (e.g. `https://www.googleapis.com/auth/drive.file` instead
of the full `drive` scope, if the workflow only ever touches files it
created itself).
