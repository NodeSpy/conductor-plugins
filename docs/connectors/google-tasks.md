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

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- **`tasklist`** — every verb that operates on a task list accepts this
  option; it defaults to `"@default"`, Google's alias for the authenticated
  user's default task list. Omitted below for brevity — every task/task-list
  verb but `tasklists` and `tasklist_create` accepts it.
- Every verb returns `status_code`; `tasklists` and `tasks` hoist Google's
  `items` array into `items`; the rest return `result` (except `task_delete`,
  which returns `status_code` only).
- Every request sends `Authorization: Bearer <injected token>` and
  `Accept: application/json`. A non-2xx response is returned as an error
  carrying the status code and response body — nothing is swallowed.

Required options are marked `*`.

### Task lists

- **`tasklists`** — list the user's task lists. (no options) → `items`.
- **`tasklist_get`** — get one task list. `tasklist` (default `"@default"`). → `result`.
- **`tasklist_create`** — create a task list. `title`* (task list title). → `result`.

### Tasks

- **`tasks`** — list tasks on a task list. `tasklist`, `showCompleted` (boolean; include completed tasks, Google default true), `showHidden` (boolean; include hidden — completed and no longer visible — tasks), `maxResults` (integer; max tasks per page), `dueMin` (RFC3339 lower bound, inclusive, on due date), `dueMax` (RFC3339 upper bound, exclusive, on due date). → `items`.
- **`task_get`** — get one task. `tasklist`, `task_id`*. → `result`.
- **`task_create`** — create a task. `tasklist`, `title`* (task title), `notes` (task notes/description), `due` (RFC3339 due date/time), `status` (`needsAction` | `completed`). → `result`.
- **`task_update`** — patch an existing task. `tasklist`, `task_id`*, `task` (map; a Task resource fragment, overrides the convenience fields below when set), `title` (convenience: task title), `notes` (convenience: task notes/description), `due` (convenience: RFC3339 due date/time), `status` (convenience: `needsAction` | `completed`). Requires either `task` or at least one convenience field; only the fields actually set are sent, so unset convenience fields never clobber existing values. → `result`.
- **`task_delete`** — delete a task. `tasklist`, `task_id`*. → `status_code` only.
- **`task_complete`** — mark a task completed. `tasklist`, `task_id`*. → `result`.
- **`task_move`** — move a task to a new position and/or parent within its list. `tasklist`, `task_id`*, `parent` (new parent task id; omit to move to the top level), `previous` (new previous-sibling task id; omit to move to the first position). → `result`.

### Escape hatch

- **`api`** — raw escape hatch: any Google Tasks API v1 endpoint (enables writes). `method` (HTTP method, default GET), `path`* (path under `https://tasks.googleapis.com/tasks/v1`, e.g. `/users/@me/lists`), `query` (map of query string parameters), `body` (JSON request body). → `result` (object response) or `items` (array response).

## Capabilities & security

Declares egress `tasks.googleapis.com:443` only — the daemon, not this
plugin, talks to Google's OAuth2 endpoints for the token exchange. Scope the
`auth:` block's requested scopes to the least privilege the workflow needs;
the default scope, `https://www.googleapis.com/auth/tasks`, grants full
read/write access to the authenticated user's tasks.
