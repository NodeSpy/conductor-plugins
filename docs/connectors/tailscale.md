# `tailscale` connector

Tailscale as a connector: devices, auth keys, the tailnet ACL, and DNS
settings over the Tailscale API (`https://api.tailscale.com/api/v2`), plus a
raw `api` escape hatch. Built on the standard library's `net/http` only.

It also declares an **exposure connector** verb pair (conductor
docs/design/plugin-contract.md §2.3, the `exposes` semantic):
`funnel_open`/`funnel_close` bring a local address up on `tailscale serve`
(tailnet-private) or `tailscale funnel` (public internet) by shelling out to
the LOCAL tailscale CLI. See **Funnel/serve exposure** below.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/tailscale/main.go`](../../connectors/tailscale/main.go)
- **Provides:** `tailscale`
- **Capabilities:** egress `["api.tailscale.com:443"]`; spawns `tailscale` (for `funnel_open`/`funnel_close` only)

## Setup

Get credentials for one of the two auth paths below — an OAuth client for
conductor's managed OAuth2 (recommended: long-lived, scopable, no manual
rotation), or a short-lived API access token.

**Prerequisites:** admin access to the tailnet in the Tailscale admin console.

**Option A — OAuth client (managed, recommended):**

1. Admin console → **Settings → OAuth clients**
   (`https://login.tailscale.com/admin/settings/oauth`).
2. **Generate OAuth client**, pick the narrowest scopes this workflow needs
   (e.g. read-only `all:read`, or a specific resource like `devices:core`),
   and tag restrictions if you use them.
3. Copy the **Client ID** and **Client secret** — the secret is shown once.
4. Put them in the connector's `auth:` block (below), then run
   `conductor connector auth tailscale` to complete the exchange.

**Option B — API access token (fallback):**

1. Admin console → **Settings → Keys**
   (`https://login.tailscale.com/admin/settings/keys`).
2. Under **API access tokens**, **Generate access token**, set an expiry
   (1–90 days), and copy it (`tskey-api-...`) — shown once.

`tailnet` (either option) is your tailnet name, e.g. `example.com`, or `-`
for the default tailnet — see **Settings → General**.

**Configure (Option A):**

```yaml
connectors:
  tailscale:
    use: tailscale
    tailnet: example.com
    auth:
      grant: client_credentials
      client_id: ${TAILSCALE_OAUTH_CLIENT_ID}
      client_secret: ${TAILSCALE_OAUTH_CLIENT_SECRET}
      token_vault: tailscale
```

**Configure (Option B):**

```yaml
connectors:
  tailscale:
    use: tailscale
    tailnet: example.com
    api_key: ${TAILSCALE_API_KEY}
```

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
| `funnel_mode` | string | `serve` (tailnet-only) or `funnel` (public) for `funnel_open` (default `funnel`). |
| `binary` | string | Override the local `tailscale` CLI path, for `funnel_open`/`funnel_close` (default `tailscale`). |

`tailnet` is required by the connection schema even if you only use
`funnel_open`/`funnel_close` and never touch the API verbs — it's one
instance, one schema. You already know your tailnet name if you're using
Tailscale at all, so this is a one-line cost, not a real blocker.

The `auth:` block (grant, OAuth client credentials, token vault) is
daemon-managed configuration, not a connection field this plugin ever sees —
it is not part of `Describe().Connection`.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- **`device_id`** / **`key_id`** — required on every device-scoped or
  key-scoped verb respectively.
- Every verb's outputs include `status_code`; `devices`/`keys` return `items`
  (hoisted from the API's `devices`/`keys` envelope key); single-resource
  verbs return `result`. Omitted below for brevity.
- All requests send `Authorization: Bearer <token>` (managed OAuth2 or
  `api_key`, per above). A non-2xx response is returned as an error carrying
  the status code and response body — nothing is swallowed.

Required options are marked `*`.

### Devices

- **`devices`** — list devices in the tailnet. `GET /tailnet/{tailnet}/devices`. `fields` (pass `"all"` to include all device fields). → `items` (hoisted from `devices`).
- **`device_get`** — get one device. `GET /device/{id}`. `device_id`*. → `result`.
- **`device_delete`** — remove a device from the tailnet. `DELETE /device/{id}`. `device_id`*. → `status_code`.
- **`device_authorize`** — authorize (or deauthorize) a device. `POST /device/{id}/authorized`. `device_id`*, `authorized`* (boolean). → `status_code`.
- **`device_set_tags`** — set a device's ACL tags. `POST /device/{id}/tags`. `device_id`*, `tags`* (list, e.g. `["tag:server"]`). → `status_code`.
- **`device_routes`** — get a device's advertised/enabled subnet routes. `GET /device/{id}/routes`. `device_id`*. → `result`.
- **`device_set_routes`** — set a device's enabled subnet routes. `POST /device/{id}/routes`. `device_id`*, `routes`* (list, e.g. `["10.0.0.0/24"]`). → `result`.

### Auth keys

- **`keys`** — list the tailnet's auth keys. `GET /tailnet/{tailnet}/keys`. No options. → `items` (hoisted from `keys`).
- **`key_get`** — get one auth key. `GET /tailnet/{tailnet}/keys/{id}`. `key_id`*. → `result`.
- **`key_create`** — create a new auth key. `POST /tailnet/{tailnet}/keys`. `capabilities`* (any, the key's `capabilities` object per the Tailscale API), `expiry_seconds` (key lifetime in seconds), `description`. → `result`.
- **`key_delete`** — delete an auth key. `DELETE /tailnet/{tailnet}/keys/{id}`. `key_id`*. → `status_code`.

### ACL

- **`acl_get`** — get the tailnet's ACL. `GET /tailnet/{tailnet}/acl`. No options. → `result`.
- **`acl_set`** — replace the tailnet's ACL. `POST /tailnet/{tailnet}/acl`. `acl`* (any, the new ACL document, HuJSON/JSON). → `result`.

### DNS

- **`dns_nameservers`** — get the tailnet's DNS nameservers. `GET /tailnet/{tailnet}/dns/nameservers`. No options. → `result`.
- **`dns_set_nameservers`** — set the tailnet's DNS nameservers. `POST /tailnet/{tailnet}/dns/nameservers`. `dns`* (list of nameserver IPs). → `result`.
- **`dns_preferences`** — get the tailnet's DNS preferences (MagicDNS). `GET /tailnet/{tailnet}/dns/preferences`. No options. → `result`.

### Escape hatch

- **`api`** — raw escape hatch: any Tailscale API endpoint (enables writes). `method` (HTTP method, default `GET`), `path`* (path under `/api/v2`, e.g. `/tailnet/example.com/devices`), `query` (map of query string parameters), `body` (any, JSON request body). → `result` (object response) or `items` (array response).

## Funnel/serve exposure

Unlike every verb above, `funnel_open`/`funnel_close` talk to the **local**
`tailscale` CLI (already logged into this machine's tailnet), not the
Tailscale API — a separate credential concern from `tailnet`/`api_key`/
`auth:` above. This lives on the `tailscale` connector rather than a second
`tailscale-funnel` plugin because it's the same vendor and the same tailnet
concept: an operator who already configured `tailscale:` for the API
shouldn't need a second, confusingly-named connector just to expose a port
on the same tailnet.

**Prerequisites:** the `tailscale` CLI installed and logged in on the host
running conductor (`tailscale up`) — `tailscale funnel` additionally
requires [Funnel enabled for your tailnet](https://tailscale.com/kb/1223/funnel)
in the admin console (HTTPS + MagicDNS on by default for most plans; Funnel
itself may need an explicit ACL grant).

```yaml
connectors:
  tun: { use: tailscale, tailnet: example.com, funnel_mode: funnel }
  web: { use: web, expose: tun }
```

Use `funnel_mode: serve` instead for tailnet-private reachability (anyone on
your tailnet, not the public internet) — still useful as an `expose:`
target when "reachable from outside this machine" is all you need, not
"reachable from the internet".

### `funnel_open` — bring a local address up (`exposes`)

| option | type | |
|--------|------|---|
| `local_addr` * | string | `host:port` to expose |

Outputs `public_url`, `lease`. Reads the URL off `tailscale serve|funnel
--bg`'s own output; falls back to `tailscale status --json`'s
`Self.DNSName` when the command's own output carries none.

### `funnel_close` — end an exposure

`lease` * (the value `funnel_open` returned).

**Pre-existing mappings are never clobbered.** If a serve/funnel mapping on
port 443 already existed before `funnel_open` (someone else's, or a previous
manual `tailscale serve`), `funnel_close` leaves it alone and logs that fact
to stderr instead of running `tailscale serve --https=443 off` — it only
tears down a mapping it created itself. Tailscale supports one serve/funnel
mapping per port, so concurrent leases across instances contend at the OS
level, not this plugin's.

## Capabilities & security

Declares egress `api.tailscale.com:443` for the API verbs — the daemon, not
this plugin, talks to Tailscale's OAuth token endpoint for the managed
`client_credentials` exchange. Scope the OAuth client (or the API key) to the
least privilege the workflow needs; Tailscale OAuth clients can be scoped
read-only (`all:read`) or to specific resource types. Also declares `Commands:
["tailscale"]` and `Spawns: true`, used only by `funnel_open`/`funnel_close`.
