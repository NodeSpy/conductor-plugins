# `google-contacts` connector

Google Contacts as a connector: list/search the authenticated user's
contacts (connections), get/create/update/delete a single contact, list
"other contacts" (auto-saved from interactions), and a raw `api` escape
hatch, over the People API v1 (`https://people.googleapis.com/v1`). Built on
the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/google-contacts/main.go`](../../connectors/google-contacts/main.go)
- **Provides:** `google-contacts`
- **Capabilities:** egress `["people.googleapis.com:443"]`

## Managed OAuth2 — this plugin never talks to Google's OAuth2 endpoints

`google-contacts` authenticates via conductor's **managed OAuth2**, not its
own token exchange. `Describe().Auth` bakes in Google's OAuth2 endpoints; the
daemon runs the actual authorization-code flow against
`accounts.google.com` / `oauth2.googleapis.com` and injects a fresh,
auto-rotated bearer token into every verb call. The plugin's only egress is
`people.googleapis.com` — it never sees a client secret and never makes a
token-exchange request itself.

```yaml
connectors:
  google-contacts:
    use: google-contacts
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: google-contacts   # where the daemon persists the rotated token
```

After the connector is configured, run the one-time browser login:

```sh
conductor connector auth google-contacts
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
`conductor connector auth google-contacts` for the one-time browser login —
conductor stores and refreshes the token from there.

**Prerequisites:** a Google account and a Google Cloud project.

1. [console.cloud.google.com](https://console.cloud.google.com) → create or
   select a project.
2. **APIs & Services → Library** → enable the **People API**.
3. **APIs & Services → OAuth consent screen** → **External** (unless on a
   Workspace org) → fill in app name/support email → add the
   `https://www.googleapis.com/auth/contacts` scope. While the app is in
   **Testing**, add your own account under **Test users**.
4. **APIs & Services → Credentials → Create credentials → OAuth client ID**
   → application type **Web application**.
5. Under **Authorized redirect URIs**, add conductor's local callback:
   `http://localhost:8400/callback` (override with the `auth:` block's
   `redirect_uri` if the daemon uses a different port).
6. Copy the **Client ID** and **Client secret**, put them in the `auth:`
   block below, then run `conductor connector auth google-contacts`.

```yaml
connectors:
  google-contacts:
    use: google-contacts
    auth:
      grant: authorization_code
      client_id: ${GOOGLE_CLIENT_ID}
      client_secret: ${GOOGLE_CLIENT_SECRET}
      token_vault: google-contacts
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_base` | string | Overrides `https://people.googleapis.com/v1` (tests only). |

The `auth:` block (grant, client credentials, token vault) is daemon-managed
configuration, not a connection field this plugin ever sees — it is not part
of `Describe().Connection`.

## Field masks

The People API requires an explicit field mask on every request that reads
or writes a `Person` resource: `personFields` on reads, `updatePersonFields`
on `contact_update`, and `readMask` on `search`/`other_contacts`. Whenever a
verb's caller omits the relevant option, this plugin defaults it to
`"names,emailAddresses,phoneNumbers,organizations"` rather than sending an
invalid, mask-less request to Google.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- **`resource_name`** — identifies a single contact, e.g.
  `"people/c1234567890"` (exactly as returned in a prior verb's
  `resourceName` field); required on every single-contact verb.
- Field-mask options (`personFields`, `updatePersonFields`, `readMask`) all
  default to `"names,emailAddresses,phoneNumbers,organizations"` when
  omitted — see [Field masks](#field-masks).
- Every verb returns `status_code`; `connections`, `search`, and
  `other_contacts` hoist Google's list array into `items`; the rest return
  `result`.
- Every request sends `Authorization: Bearer <injected token>` and
  `Accept: application/json`. A non-2xx response is returned as an error
  carrying the status code and response body — nothing is swallowed.

Required options are marked `*`.

### Contacts

- **`connections`** — list the authenticated user's contacts. `personFields` (comma-separated Person fields to return), `pageSize` (integer), `pageToken` (string; page token from a previous response), `sortOrder` (`LAST_MODIFIED_ASCENDING` | `LAST_MODIFIED_DESCENDING` | `FIRST_NAME_ASCENDING` | `LAST_NAME_ASCENDING`). → `items`.
- **`contact_get`** — get one contact. `resource_name`*, `personFields`. → `result`.
- **`contact_create`** — create a contact. `person` (map; a full Person resource body, overrides the convenience fields below when set), `given_name` (convenience: contact given/first name), `family_name` (convenience: contact family/last name), `email` (convenience: contact email address), `phone` (convenience: contact phone number). → `result`.
- **`contact_update`** — patch an existing contact. `resource_name`*, `person`* (map; the fields to patch, as a Person resource fragment — must include `etag`), `updatePersonFields` (comma-separated Person fields being updated). → `result`.
- **`contact_delete`** — delete a contact. `resource_name`*. → `status_code` only.

### Search

- **`search`** — search the authenticated user's contacts. `query`* (search query text), `readMask` (comma-separated Person fields to return). → `items`.
- **`other_contacts`** — list "other contacts" (auto-saved from interactions, not in the user's contacts). `readMask` (comma-separated Person fields to return), `pageSize` (integer). → `items`.

### Escape hatch

- **`api`** — raw escape hatch: any Google People API v1 endpoint (enables writes). `method` (HTTP method, default GET), `path`* (path under `https://people.googleapis.com/v1`, e.g. `/people/me/connections`), `query` (map of query string parameters), `body` (JSON request body). → `result` (object response) or `items` (array response).

## Capabilities & security

Declares egress `people.googleapis.com:443` only — the daemon, not this
plugin, talks to Google's OAuth2 endpoints for the token exchange. Scope the
`auth:` block's requested scopes to the least privilege the workflow needs;
the default scope, `https://www.googleapis.com/auth/contacts`, grants full
read/write access to the authenticated user's contacts.
