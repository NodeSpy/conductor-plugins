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

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; `connections`,
`search`, and `other_contacts` hoist Google's list array into `items`; the
rest return `result`.

| verb | endpoint | outputs |
|------|----------|---------|
| `connections` | `GET /people/me/connections` (`personFields`, `pageSize`, `pageToken`, `sortOrder`) | `items` |
| `contact_get` | `GET /{resource_name}` (`personFields`) | `result` |
| `contact_create` | `POST /people:createContact` — body from a `person` map option, or convenience `given_name`/`family_name`/`email`/`phone` fields | `result` |
| `contact_update` | `PATCH /{resource_name}:updateContact` (`updatePersonFields`) — body is the `person` map option (required) | `result` |
| `contact_delete` | `DELETE /{resource_name}:deleteContact` | `status_code` only |
| `search` | `GET /people:searchContacts` (`query` required, `readMask`) | `items` |
| `other_contacts` | `GET /otherContacts` (`readMask`, `pageSize`) | `items` |
| `api` | raw escape hatch: `method` + `path` (under `/v1`) + `query` + `body`, for anything without a first-class verb | `result` (object response) or `items` (array response) |

`resource_name` identifies a single contact, e.g. `"people/c1234567890"`
(exactly as returned in a prior verb's `resourceName` field). Every request
sends `Authorization: Bearer <injected token>` and `Accept: application/json`.
A non-2xx response is returned as an error carrying the status code and
response body — nothing is swallowed.

## Capabilities & security

Declares egress `people.googleapis.com:443` only — the daemon, not this
plugin, talks to Google's OAuth2 endpoints for the token exchange. Scope the
`auth:` block's requested scopes to the least privilege the workflow needs;
the default scope, `https://www.googleapis.com/auth/contacts`, grants full
read/write access to the authenticated user's contacts.
