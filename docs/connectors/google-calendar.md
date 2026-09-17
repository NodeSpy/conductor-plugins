# `google-calendar` connector

Google Calendar as a connector: the user's calendar list, events (list, get,
create, update, delete), quick-add natural-language events, free/busy
queries, and a raw `api` escape hatch, over the Calendar API v3
(`https://www.googleapis.com/calendar/v3`). Built on the standard library's
`net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/google-calendar/main.go`](../../connectors/google-calendar/main.go)
- **Provides:** `google-calendar`
- **Capabilities:** egress `["www.googleapis.com:443"]`

## Managed OAuth2 — this plugin never talks to Google's OAuth2 endpoints

`google-calendar` authenticates via conductor's **managed OAuth2**, not its
own token exchange. `Describe().Auth` bakes in Google's OAuth2 endpoints; the
daemon runs the actual authorization-code flow against
`accounts.google.com` / `oauth2.googleapis.com` and injects a fresh,
auto-rotated bearer token into every verb call. The plugin's only egress is
`www.googleapis.com` — it never sees a client secret and never makes a
token-exchange request itself.

```yaml
connectors:
  google-calendar:
    use: google-calendar
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: google-calendar   # where the daemon persists the rotated token
```

After the connector is configured, run the one-time browser login:

```sh
conductor connector auth google-calendar
```

If no token has been configured/logged in yet, every verb fails fast with a
`CodeInvalidParams` error pointing back at this command, rather than sending
an unauthenticated request.

The connector's auth spec requests `access_type=offline` and
`prompt=consent`: without `access_type=offline` Google never returns a
refresh token, so the daemon would be unable to keep the connection alive
past the first access token's expiry.

## Setup

Managed OAuth2: register an app once in Google Cloud, then run
`conductor connector auth google-calendar` for the one-time browser login —
conductor stores and refreshes the token from there.

**Prerequisites:** a Google account and a Google Cloud project.

1. [console.cloud.google.com](https://console.cloud.google.com) → create or
   select a project.
2. **APIs & Services → Library** → enable the **Google Calendar API**.
3. **APIs & Services → OAuth consent screen** → **External** (unless on a
   Workspace org) → fill in app name/support email → add the
   `https://www.googleapis.com/auth/calendar` scope. While the app is in
   **Testing**, add your own account under **Test users**.
4. **APIs & Services → Credentials → Create credentials → OAuth client ID**
   → application type **Web application**.
5. Under **Authorized redirect URIs**, add conductor's local callback:
   `http://localhost:8400/callback` (override with the `auth:` block's
   `redirect_uri` if the daemon uses a different port).
6. Copy the **Client ID** and **Client secret**, put them in the `auth:`
   block below, then run `conductor connector auth google-calendar`.

```yaml
connectors:
  google-calendar:
    use: google-calendar
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: google-calendar
    calendar_id: primary   # optional — defaults to "primary"
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `calendar_id` | string | Default calendar for every verb. Optional — defaults to `"primary"`. A verb's own `calendar_id` option overrides it for that call. |
| `api_base` | string | Overrides `https://www.googleapis.com/calendar/v3` (tests only). |

The `auth:` block (grant, client credentials, token vault) is daemon-managed
configuration, not a connection field this plugin ever sees — it is not part
of `Describe().Connection`.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- **`calendar_id`** — every verb except `calendars` accepts a per-call
  `calendar_id` option that overrides the connection's default (`"primary"`).
- Every verb returns `status_code`; `calendars` and `events` hoist Google's
  `items` array into `items`; the rest return `result`.
- Every request sends `Authorization: Bearer <injected token>` and
  `Accept: application/json`. A non-2xx response is returned as an error
  carrying the status code and response body — nothing is swallowed.

Required options are marked `*`.

### Calendars & events

- **`calendars`** — list the calendars on the user's calendar list. (no options) → `items`.
- **`events`** — list events on a calendar. `calendar_id`, `timeMin` (RFC3339 lower bound, inclusive, on event end time), `timeMax` (RFC3339 upper bound, exclusive, on event start time), `q` (free text search terms), `maxResults` (integer; max events per page, Google default 250, max 2500), `singleEvents` (boolean; expand recurring events into single instances), `orderBy` (`startTime` (requires `singleEvents: true`) | `updated`). → `items`.
- **`event_get`** — get one event. `calendar_id`, `event_id`*. → `result`.
- **`event_create`** — create an event. `calendar_id`, `event` (map; a full Google Calendar Event resource body, overrides the convenience fields below when set), `summary` (convenience: event title), `description` (convenience: event description), `start` (convenience: RFC3339 dateTime string, or a `{dateTime|date, timeZone}` map), `end` (convenience, same shape as `start`), `attendees` (convenience: list of attendee email addresses). → `result`.
- **`event_update`** — patch an existing event. `calendar_id`, `event_id`*, `event`* (map; the fields to patch, as an Event resource fragment). → `result`.
- **`event_delete`** — delete an event. `calendar_id`, `event_id`*. → `status_code` only.
- **`quick_add`** — create an event from a natural-language description. `calendar_id`, `text`* (e.g. `"Lunch with Sam tomorrow 1pm"`). → `result`.

### Free/busy

- **`freebusy`** — query free/busy information for one or more calendars. `timeMin`* (RFC3339 start of the query interval), `timeMax`* (RFC3339 end of the query interval), `items` (list of calendar ids to query; default: the connection's `calendar_id`). → `result`.

### Escape hatch

- **`api`** — raw escape hatch: any Google Calendar API v3 endpoint (enables writes). `method` (HTTP method, default GET), `path`* (path under `https://www.googleapis.com/calendar/v3`, e.g. `/users/me/calendarList`), `query` (map of query string parameters), `body` (JSON request body). → `result` (object response) or `items` (array response).

## Capabilities & security

Declares egress `www.googleapis.com:443` only — the daemon, not this plugin,
talks to Google's OAuth2 endpoints for the token exchange. Scope the `auth:`
block's requested scopes to the least privilege the workflow needs; the
default scope, `https://www.googleapis.com/auth/calendar`, grants full
read/write access to the authenticated user's calendars.
