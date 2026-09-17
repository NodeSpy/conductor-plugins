# `google-sheets` connector

Google Sheets as a connector: spreadsheet metadata, reading/writing/appending/
clearing cell ranges, batch value reads, batch structural updates
(`batchUpdate`), creating new spreadsheets, and a raw `api` escape hatch over
the Sheets API v4 (`https://sheets.googleapis.com/v4/spreadsheets`). Built on
the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/google-sheets/main.go`](../../connectors/google-sheets/main.go)
- **Provides:** `google-sheets`
- **Capabilities:** egress `["sheets.googleapis.com:443"]`

## Managed OAuth2 — this plugin never talks to Google's OAuth endpoints

`google-sheets` authenticates via conductor's **managed OAuth2**, not its own
token exchange. `Describe().Auth` bakes in Google's OAuth2 endpoints (and the
`access_type=offline` / `prompt=consent` params needed to get a refresh
token back); the daemon runs the actual authorization-code flow against
`accounts.google.com` / `oauth2.googleapis.com` and injects a fresh,
auto-rotated bearer token into every verb call. The plugin's only egress is
`sheets.googleapis.com` — it never sees a client secret and never makes a
token-exchange request itself.

```yaml
connectors:
  google-sheets:
    use: google-sheets
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: google-sheets   # where the daemon persists the rotated token
```

After the connector is configured, run the one-time login:

```sh
conductor connector auth google-sheets
```

If no token has been configured/logged in yet, every verb fails fast with a
`CodeInvalidParams` error pointing back at this command, rather than sending
an unauthenticated request.

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_base` | string | Overrides `https://sheets.googleapis.com/v4/spreadsheets` (tests only). |

The `auth:` block (grant, client credentials, token vault, requested scopes)
is daemon-managed configuration, not a connection field this plugin ever
sees — it is not part of `Describe().Connection`.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Sheets API v4 responses are JSON objects, not bare arrays, so
every first-class verb hoists its decoded body straight into `result`; every
verb also returns `status_code`.

| verb | endpoint | outputs |
|------|----------|---------|
| `get` | `GET /{spreadsheet_id}` (`include_grid_data`, `ranges`) | `result` |
| `values_get` | `GET /{spreadsheet_id}/values/{range}` (`value_render_option`, `major_dimension`) | `result` (has a `values` field) |
| `values_update` | `PUT /{spreadsheet_id}/values/{range}?valueInputOption=USER_ENTERED` (body `{values}` from the `values` option) | `result` |
| `values_append` | `POST /{spreadsheet_id}/values/{range}:append?valueInputOption=USER_ENTERED&insertDataOption=INSERT_ROWS` (body `{values}`) | `result` |
| `values_clear` | `POST /{spreadsheet_id}/values/{range}:clear` | `result` |
| `values_batch_get` | `GET /{spreadsheet_id}/values:batchGet` (`ranges`, repeated) | `result` |
| `batch_update` | `POST /{spreadsheet_id}:batchUpdate` (body `{requests}` from the `requests` option) | `result` |
| `create` | `POST /` (body `{properties: {title}, sheets}` from the `title`/`sheets` options) | `result` |
| `api` | `method` + `path` (under the spreadsheets resource root) + `query` + `body` — escape hatch for anything without a first-class verb, including writes | `result` (object response) or `items` (array response) |

Required options are validated locally before any request is made:
`spreadsheet_id` for every verb except `create` (and the raw `api` escape
hatch, which takes an arbitrary `path` instead); `range` for the `values_get`,
`values_update`, `values_append`, and `values_clear` verbs; `values` for
`values_update` and `values_append`; `requests` for `batch_update`.

Every request sends `Authorization: Bearer <injected token>` and
`Accept: application/json`. A non-2xx response is returned as an error
carrying the status code and response body — nothing is swallowed.

## Capabilities & security

Declares egress `sheets.googleapis.com:443` only — the daemon, not this
plugin, talks to `accounts.google.com` / `oauth2.googleapis.com` for the
OAuth2 exchange. Scope the `auth:` block's grant and requested scopes to the
least privilege the workflow needs; the baked-in scope is
`https://www.googleapis.com/auth/spreadsheets` (full read/write access to
spreadsheets), and `access_type=offline` is set so refresh tokens keep
working without repeated interactive logins.
