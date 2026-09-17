# `google-tasks` connector

Google Tasks as a connector: task lists (list, get, create), tasks (list,
get, create, update, delete, complete, move), and a raw `api` escape hatch,
over the Tasks API v1 (`https://tasks.googleapis.com/tasks/v1`). Built on the
standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/google-tasks/main.go`](../../connectors/google-tasks/main.go)
- **Provides:** `google-tasks`
- **Capabilities:** egress `["tasks.googleapis.com:443"]`

## Managed OAuth2 — this plugin never talks to Google's OAuth2 endpoints

`google-tasks` authenticates via conductor's **managed OAuth2**, not its own
token exchange. `Describe().Auth` bakes in Google's OAuth2 endpoints; the
daemon runs the actual authorization-code flow against
`accounts.google.com` / `oauth2.googleapis.com` and injects a fresh,
auto-rotated bearer token into every verb call. The plugin's only egress is
`tasks.googleapis.com` — it never sees a client secret and never makes a
token-exchange request itself.

```yaml
connectors:
  google-tasks:
    use: google-tasks
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: google-tasks   # where the daemon persists the rotated token
```

After the connector is configured, run the one-time browser login:

```sh
conductor connector auth google-tasks
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
`conductor connector auth google-tasks` for the one-time browser login —
conductor stores and refreshes the token from there.

**Prerequisites:** a Google account and a Google Cloud project.

1. [console.cloud.google.com](https://console.cloud.google.com) → create or
   select a project.
2. **APIs & Services → Library** → enable the **Google Tasks API**.
3. **APIs & Services → OAuth consent screen** → **External** (unless on a
   Workspace org) → fill in app name/support email → add the
   `https://www.googleapis.com/auth/tasks` scope. While the app is in
   **Testing**, add your own account under **Test users**.
4. **APIs & Services → Credentials → Create credentials → OAuth client ID**
   → application type **Web application**.
5. Under **Authorized redirect URIs**, add conductor's local callback:
   `http://localhost:8400/callback` (override with the `auth:` block's
   `redirect_uri` if the daemon uses a different port).
6. Copy the **Client ID** and **Client secret**, put them in the `auth:`
   block below, then run `conductor connector auth google-tasks`.

```yaml
connectors:
  google-tasks:
    use: google-tasks
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: google-tasks
```

`tasklist` is not a connector-level field — it's a per-verb option
(defaults to `"@default"`) passed on each `uses: google-tasks.<verb>` call.

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_base` | string | Overrides `https://tasks.googleapis.com/tasks/v1` (tests only). |

The `auth:` block (grant, client credentials, token vault) is daemon-managed
configuration, not a connection field this plugin ever sees — it is not part
of `Describe().Connection`.

Every verb that needs a task list accepts a `tasklist` option; when omitted
it defaults to `"@default"`, Google's alias for the authenticated user's
default task list.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; `tasklists` and
`tasks` hoist Google's `items` array into `items`; the rest return `result`
(except `task_delete`, which returns `status_code` only).

| verb | endpoint | outputs |
|------|----------|---------|
| `tasklists` | `GET /users/@me/lists` | `items` |
| `tasklist_get` | `GET /users/@me/lists/{tasklist}` | `result` |
| `tasklist_create` | `POST /users/@me/lists` — `title` (required) | `result` |
| `tasks` | `GET /lists/{tasklist}/tasks` (`showCompleted`, `showHidden`, `maxResults`, `dueMin`, `dueMax`) | `items` |
| `task_get` | `GET /lists/{tasklist}/tasks/{task_id}` | `result` |
| `task_create` | `POST /lists/{tasklist}/tasks` — `title` (required), `notes`, `due`, `status` | `result` |
| `task_update` | `PATCH /lists/{tasklist}/tasks/{task_id}` — body from a `task` map option, or convenience `title`/`notes`/`due`/`status` fields | `result` |
| `task_delete` | `DELETE /lists/{tasklist}/tasks/{task_id}` | `status_code` only |
| `task_complete` | `PATCH /lists/{tasklist}/tasks/{task_id}` with `{"status": "completed"}` | `result` |
| `task_move` | `POST /lists/{tasklist}/tasks/{task_id}/move` (`parent`, `previous` query params) | `result` |
| `api` | raw escape hatch: `method` + `path` (under `/tasks/v1`) + `query` + `body`, for anything without a first-class verb | `result` (object response) or `items` (array response) |

`task_update` requires either a `task` map (a Task resource fragment, used
verbatim as the PATCH body) or at least one of the convenience fields
(`title`, `notes`, `due`, `status`) — only the fields actually set are sent,
so unset convenience fields never clobber existing values. Every request
sends `Authorization: Bearer <injected token>` and `Accept: application/json`.
A non-2xx response is returned as an error carrying the status code and
response body — nothing is swallowed.

## Capabilities & security

Declares egress `tasks.googleapis.com:443` only — the daemon, not this
plugin, talks to Google's OAuth2 endpoints for the token exchange. Scope the
`auth:` block's requested scopes to the least privilege the workflow needs;
the default scope, `https://www.googleapis.com/auth/tasks`, grants full
read/write access to the authenticated user's tasks.
