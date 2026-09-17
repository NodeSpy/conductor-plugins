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

## Setup

Managed OAuth2: register an app once in Google Cloud, then run
`conductor connector auth google-sheets` for the one-time browser login —
conductor stores and refreshes the token from there.

**Prerequisites:** a Google account and a Google Cloud project.

1. [console.cloud.google.com](https://console.cloud.google.com) → create or
   select a project.
2. **APIs & Services → Library** → enable the **Google Sheets API**.
3. **APIs & Services → OAuth consent screen** → **External** (unless on a
   Workspace org) → fill in app name/support email → add the
   `https://www.googleapis.com/auth/spreadsheets` scope. While the app is in
   **Testing**, add your own account under **Test users**.
4. **APIs & Services → Credentials → Create credentials → OAuth client ID**
   → application type **Web application**.
5. Under **Authorized redirect URIs**, add conductor's local callback:
   `http://localhost:8400/callback` (override with the `auth:` block's
   `redirect_uri` if the daemon uses a different port).
6. Copy the **Client ID** and **Client secret**, put them in the `auth:`
   block below, then run `conductor connector auth google-sheets`.

```yaml
connectors:
  google-sheets:
    use: google-sheets
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: google-sheets
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_base` | string | Overrides `https://sheets.googleapis.com/v4/spreadsheets` (tests only). |

The `auth:` block (grant, client credentials, token vault, requested scopes)
is daemon-managed configuration, not a connection field this plugin ever
sees — it is not part of `Describe().Connection`.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Sheets API v4 responses are JSON objects, not bare arrays, so every
  first-class verb hoists its decoded body straight into `result`. Every verb
  also returns `status_code`.
- `spreadsheet_id` is required on every verb except `create` (and the raw
  `api` escape hatch, which takes an arbitrary `path` instead).
- Every request sends `Authorization: Bearer <injected token>` and
  `Accept: application/json`. A non-2xx response is returned as an error
  carrying the status code and response body — nothing is swallowed.

Required options are marked `*`.

### Spreadsheets

- **`get`** — get a spreadsheet's metadata (and optionally grid data). `spreadsheet_id`*, `include_grid_data` (boolean; include cell data in the response), `ranges` (list of A1 ranges to limit the response to, e.g. `["Sheet1!A1:C10"]`). → `result`.
- **`create`** — create a new spreadsheet. `title` (spreadsheet title), `sheets` (list of Sheets API Sheet objects to seed the spreadsheet with). → `result`.

### Values

- **`values_get`** — get the values of a single A1 range. `spreadsheet_id`*, `range`* (A1 range, e.g. `Sheet1!A1:C10`), `value_render_option` (`FORMATTED_VALUE` (default) | `UNFORMATTED_VALUE` | `FORMULA`), `major_dimension` (`ROWS` (default) | `COLUMNS`). → `result` (has a `values` field: a 2D array of cell values).
- **`values_update`** — overwrite the values of a single A1 range. `spreadsheet_id`*, `range`*, `values`* (2D array of cell values, rows of columns). Sent as `PUT ...?valueInputOption=USER_ENTERED`. → `result`.
- **`values_append`** — append rows of values after the last row of a range. `spreadsheet_id`*, `range`* (A1 range to search for a table within, e.g. `Sheet1!A1:C10`), `values`* (2D array of cell values to append). Sent as `POST ...:append?valueInputOption=USER_ENTERED&insertDataOption=INSERT_ROWS`. → `result`.
- **`values_clear`** — clear the values of a single A1 range (formatting is untouched). `spreadsheet_id`*, `range`*. → `result`.
- **`values_batch_get`** — get the values of multiple A1 ranges in one call. `spreadsheet_id`*, `ranges` (list of A1 ranges, e.g. `["Sheet1!A1:C10", "Sheet2!A:A"]`). → `result`.

### Structural updates

- **`batch_update`** — apply one or more structural/formatting update requests atomically. `spreadsheet_id`*, `requests`* (list of Sheets API Request objects, e.g. `[{"addSheet": {...}}]`). → `result`.

### Escape hatch

- **`api`** — raw escape hatch: any Sheets API v4 endpoint (enables writes). `method` (HTTP method, default GET), `path`* (path under `https://sheets.googleapis.com/v4/spreadsheets`, e.g. `/{spreadsheetId}/values/A1:B2`), `query` (map of query string parameters), `body` (JSON request body). → `result` (object response) or `items` (array response).

## Capabilities & security

Declares egress `sheets.googleapis.com:443` only — the daemon, not this
plugin, talks to `accounts.google.com` / `oauth2.googleapis.com` for the
OAuth2 exchange. Scope the `auth:` block's grant and requested scopes to the
least privilege the workflow needs; the baked-in scope is
`https://www.googleapis.com/auth/spreadsheets` (full read/write access to
spreadsheets), and `access_type=offline` is set so refresh tokens keep
working without repeated interactive logins.
