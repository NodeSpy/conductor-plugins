# `gmail` connector

Gmail as a connector: list/get messages, send mail, labels, drafts, threads,
label modification, trash/untrash, over the Gmail API v1
(`https://gmail.googleapis.com/gmail/v1/users/me`), plus a raw `api` escape
hatch. Built on the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/gmail/main.go`](../../connectors/gmail/main.go)
- **Provides:** `gmail`
- **Capabilities:** egress `["gmail.googleapis.com:443"]`

## Managed OAuth2 — this plugin never talks to oauth2.googleapis.com

`gmail` authenticates via conductor's **managed OAuth2**, not its own token
exchange. `Describe().Auth` bakes in Google's OAuth2 endpoints (plus
`access_type=offline` and `prompt=consent`, which Google requires to hand back
a refresh token on login); the daemon runs the actual authorization-code flow
against `accounts.google.com` / `oauth2.googleapis.com` and injects a fresh,
auto-rotated bearer token into every verb call. The plugin's only egress is
`gmail.googleapis.com` — it never sees a client secret and never makes a
token-exchange request itself.

```yaml
connectors:
  gmail:
    use: gmail
    auth:
      grant: authorization_code
      client_id: ${GMAIL_CLIENT_ID}
      client_secret: ${GMAIL_CLIENT_SECRET}
      token_vault: gmail   # where the daemon persists the rotated token
```

After the connector is configured, run the one-time login:

```sh
conductor connector auth gmail
```

If no token has been configured/logged in yet, every verb fails fast with a
`CodeInvalidParams` error pointing back at this command, rather than sending
an unauthenticated request.

## Setup

Managed OAuth2: register an app once in Google Cloud, then run
`conductor connector auth gmail` for the one-time browser login — conductor
stores and refreshes the token from there.

**Prerequisites:** a Google account and a Google Cloud project.

1. [console.cloud.google.com](https://console.cloud.google.com) → create or
   select a project.
2. **APIs & Services → Library** → enable the **Gmail API**.
3. **APIs & Services → OAuth consent screen** → **External** (unless on a
   Workspace org) → fill in app name/support email → add the
   `https://www.googleapis.com/auth/gmail.modify` scope. While the app is in
   **Testing**, add your own account under **Test users**.
4. **APIs & Services → Credentials → Create credentials → OAuth client ID**
   → application type **Web application**.
5. Under **Authorized redirect URIs**, add conductor's local callback:
   `http://localhost:8400/callback` (override with the `auth:` block's
   `redirect_uri` if the daemon uses a different port).
6. Copy the **Client ID** and **Client secret**, put them in the `auth:`
   block below, then run `conductor connector auth gmail`.

```yaml
connectors:
  gmail:
    use: gmail
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: gmail
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_base` | string | Overrides `https://gmail.googleapis.com/gmail/v1/users/me` (tests only). |

The `auth:` block (grant, client credentials, token vault) is daemon-managed
configuration, not a connection field this plugin ever sees — it is not part
of `Describe().Connection`.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb returns `status_code`. Collection verbs return `items` (hoisted
  from the Gmail API's named-list envelope, e.g. `{"messages": [...]}`);
  single-resource verbs return `result` (the decoded response body).
- Every request sends `Authorization: Bearer <injected token>` and
  `Accept: application/json`. A non-2xx response is returned as an error
  carrying the status code and response body — nothing is swallowed.

Required options are marked `*`.

### Messages

- **`messages`** — list messages. `q` (Gmail search query, e.g. `is:unread from:a@b.com`), `labelIds` (list; restrict to messages with all of these label ids), `maxResults` (integer; page size), `pageToken` (string; page token from a previous call). → `items`.
- **`message_get`** — get one message. `message_id`*, `format` (`full` (default) | `metadata` | `minimal`). → `result`.
- **`send`** — send an email. `to`* (list of recipient addresses), `cc` (list), `bcc` (list), `subject`*, `text` (plain-text body; multipart/alternative with `html` if both are set), `html` (HTML body; multipart/alternative with `text` if both are set), `from` (From header; defaults to the authenticated account). → `result`.
- **`modify`** — add/remove labels on a message. `message_id`*, `add_labels` (list of label ids to add), `remove_labels` (list of label ids to remove). → `result`.
- **`trash`** — move a message to trash. `message_id`*. → `result`.
- **`untrash`** — remove a message from trash. `message_id`*. → `result`.

### Labels

- **`labels`** — list labels. (no options) → `items`.
- **`label_create`** — create a label. `name`*, `labelListVisibility` (`labelShow` | `labelShowIfUnread` | `labelHide`), `messageListVisibility` (`show` | `hide`). → `result`.

### Drafts

- **`drafts`** — list drafts. (no options) → `items`.
- **`draft_create`** — create a draft. Same message options as `send`: `to`*, `cc`, `bcc`, `subject`*, `text`, `html`, `from`. Sent as `POST /drafts` with body `{message: {raw}}`. → `result`.

### Threads

- **`threads`** — list threads. `q` (Gmail search query). → `items`.
- **`thread_get`** — get one thread. `thread_id`*. → `result`.

### Escape hatch

- **`api`** — raw escape hatch: any Gmail API endpoint (enables writes). `method` (HTTP method, default GET), `path`* (path under the connection's `api_base`, e.g. `/messages`), `query` (map of query string parameters), `body` (JSON request body). → `result` (object response) or `items` (array response).

### Building the outgoing message (`send`, `draft_create`)

`send` and `draft_create` both take the same message options — `to` (list,
required), `cc`, `bcc`, `subject` (required), `text`, `html`, `from` — and
build a complete RFC 5322 message in-process: header lines (`From`, `To`,
`Cc`, `Bcc`, `Subject`, `MIME-Version`) followed by a `text/plain` or
`text/html` body, or a `multipart/alternative` body (both parts, each in its
own MIME section) when both `text` and `html` are set. The result is
base64url-encoded with `encoding/base64`'s `RawURLEncoding` (unpadded, per the
Gmail API's `raw` field requirement) and sent as `{"raw": "<encoded>"}` — for
`draft_create`, wrapped one level deeper as `{"message": {"raw": "<encoded>"}}`.
No third-party MIME library is used.

## Capabilities & security

Declares egress `gmail.googleapis.com:443` only — the daemon, not this
plugin, talks to `accounts.google.com` / `oauth2.googleapis.com` for the
OAuth2 exchange. Scope the `auth:` block's grant and the requested scopes to
the least privilege the workflow needs; this connector defaults to the
`gmail.modify` scope (read/write, including trash, but not permanent delete
or account settings).
