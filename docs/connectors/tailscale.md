# `tailscale` connector

Tailscale as a connector: devices, auth keys, the tailnet ACL, and DNS
settings over the Tailscale API (`https://api.tailscale.com/api/v2`), plus a
raw `api` escape hatch. Built on the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/tailscale/main.go`](../../connectors/tailscale/main.go)
- **Provides:** `tailscale`
- **Capabilities:** egress `["api.tailscale.com:443"]`

## Dual auth: managed OAuth2, or a plain API key

This connector is an **auth showcase**: it supports both of conductor's
credential paths, and prefers the managed one automatically — no connection
field selects between them.

1. **Managed OAuth2 (preferred).** `Describe().Auth` bakes in Tailscale's
   `client_credentials` token endpoint. The operator configures an OAuth
   client (created in the Tailscale admin console) in the connector's
   daemon-side `auth:` block; the daemon exchanges it for a bearer token —
   no browser step, since `client_credentials` is machine-to-machine — and
   injects the token into every verb call. The plugin never talks to
   Tailscale's OAuth endpoint itself.

   ```yaml
   connectors:
     tailscale:
       use: tailscale
       auth:
         grant: client_credentials
         client_id: ${TAILSCALE_OAUTH_CLIENT_ID}
         client_secret: ${TAILSCALE_OAUTH_CLIENT_SECRET}
         token_vault: tailscale
       tailnet: example.com
   ```

   ```sh
   conductor connector auth tailscale
   ```

2. **Plain API key (fallback).** If no managed token is configured, set
   `api_key` on the connection to a Tailscale API key minted in the admin
   console:

   ```yaml
   connectors:
     tailscale:
       use: tailscale
       tailnet: example.com
       api_key: ${TAILSCALE_API_KEY}
   ```

At invoke time the plugin resolves the bearer credential in that order:
conductor's managed token first, then `api_key`. If neither is present, every
verb fails fast with a `CodeInvalidParams` error pointing at `conductor
connector auth tailscale` / setting `api_key`, rather than sending an
unauthenticated request.

## Connection

| key | type | purpose |
|-----|------|---------|
| `tailnet` | string | Required. The tailnet name, e.g. `example.com`, or `-` for the default tailnet. |
| `api_key` | string | Optional. Fallback bearer credential when no managed `auth:` token is configured. |
| `api_base` | string | Overrides `https://api.tailscale.com` (tests only). |

The `auth:` block (grant, OAuth client credentials, token vault) is
daemon-managed configuration, not a connection field this plugin ever sees —
it is not part of `Describe().Connection`.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; `devices`/`keys`
return `items` (hoisted from the API's `devices`/`keys` envelope key);
single-resource verbs return `result`.

| verb | endpoint | outputs |
|------|----------|---------|
| `devices` | `GET /tailnet/{tailnet}/devices` (`fields=all`) | `items` (hoisted from `devices`) |
| `device_get` | `GET /device/{device_id}` | `result` |
| `device_delete` | `DELETE /device/{device_id}` | `status_code` |
| `device_authorize` | `POST /device/{device_id}/authorized` (`authorized`) | `status_code` |
| `device_set_tags` | `POST /device/{device_id}/tags` (`tags`) | `status_code` |
| `device_routes` | `GET /device/{device_id}/routes` | `result` |
| `device_set_routes` | `POST /device/{device_id}/routes` (`routes`) | `result` |
| `keys` | `GET /tailnet/{tailnet}/keys` | `items` (hoisted from `keys`) |
| `key_get` | `GET /tailnet/{tailnet}/keys/{key_id}` | `result` |
| `key_create` | `POST /tailnet/{tailnet}/keys` (`capabilities`, `expiry_seconds`, `description`) | `result` |
| `key_delete` | `DELETE /tailnet/{tailnet}/keys/{key_id}` | `status_code` |
| `acl_get` | `GET /tailnet/{tailnet}/acl` | `result` |
| `acl_set` | `POST /tailnet/{tailnet}/acl` (`acl`) | `result` |
| `dns_nameservers` | `GET /tailnet/{tailnet}/dns/nameservers` | `result` |
| `dns_set_nameservers` | `POST /tailnet/{tailnet}/dns/nameservers` (`dns`) | `result` |
| `dns_preferences` | `GET /tailnet/{tailnet}/dns/preferences` | `result` |
| `api` | `method` + `path` (under `/api/v2`) + `query` + `body` — escape hatch for anything without a first-class verb, including writes | `result` (object response) or `items` (array response) |

All requests send `Authorization: Bearer <token>` (managed or `api_key`, per
above). A non-2xx response is returned as an error carrying the status code
and response body — nothing is swallowed.

## Capabilities & security

Declares egress `api.tailscale.com:443` only — the daemon, not this plugin,
talks to Tailscale's OAuth token endpoint for the managed `client_credentials`
exchange. Scope the OAuth client (or the API key) to the least privilege the
workflow needs; Tailscale OAuth clients can be scoped read-only (`all:read`)
or to specific resource types.
