# `unifi` connector

A UniFi Network controller as a connector: devices, clients, WLANs,
networks, port forwards, firewall rules, health, alarms, and events over the
UniFi Network API, plus a raw `api` escape hatch. Built on the standard
library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/unifi/main.go`](../../connectors/unifi/main.go)
- **Provides:** `unifi`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own controller)

```yaml
connectors:
  unifi:
    use: unifi
    base_url: https://unifi.example.com
    username: ${UNIFI_USERNAME}
    password: ${UNIFI_PASSWORD}
    site: default
    network: ["unifi.example.com:443"]   # narrow the declared egress to your controller
```

## Setup

Produces a local UniFi Network username/password the connector logs in with.

**Prerequisites:** a running UniFi Network controller (UDM/UDM Pro/Cloud Key,
or a self-hosted controller) and admin access to it.

1. Log into the controller's UI (`https://<controller>/network/default/settings/admins`
   on UniFi OS, or **Settings > Admins** on a standalone controller).
2. Click **Add Admin** > **Add New**, give it a name/email, and grant it
   **Local Access Only** with the least role needed (e.g. Limited Admin) —
   avoid reusing your primary owner login.
3. Set a password for the new local admin and confirm it.
4. Note whether the controller is a UDM/UDM Pro/Cloud Key gen2+
   (`unifi_os: true`) or a legacy standalone controller (`unifi_os: false`) —
   check the login URL's shape if unsure.
5. Note the `site` name you want to target (`default` unless you've renamed
   or added sites).

```yaml
connectors:
  unifi:
    use: unifi
    base_url: https://unifi.example.com
    username: ${UNIFI_USERNAME}
    password: ${UNIFI_PASSWORD}
    site: default
    unifi_os: true
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | controller root, e.g. `https://unifi.example.com` or `https://10.0.0.1` (no trailing path) |
| `username` | string | local controller username |
| `password` | string | local controller password |
| `site` | string | site name (default `"default"`) |
| `unifi_os` | boolean | `true` (default) for UniFi OS controllers (UDM/UDM Pro/Cloud Key gen2+); `false` for a legacy standalone controller |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) |

## Auth: cookie session, not a bearer token

Unlike most connectors in this repo, the UniFi Network API does not use a
static API token. Authentication is a **login call that sets session
cookies**:

- The connector POSTs `{username, password}` as JSON to the login endpoint
  and keeps whatever cookies the controller sets in an `http.CookieJar`
  (`net/http/cookiejar`), scoped to one session.
- A UniFi OS controller also returns an `X-CSRF-Token` response header at
  login. That token is cached and echoed back as a request header on every
  subsequent mutating request (`POST`/`PUT`/`DELETE`/`PATCH`) — UniFi OS's
  proxy rejects state-changing requests without it.
- The session (cookie jar + CSRF token) is cached on the plugin process,
  keyed by `base_url` + `username` + `password` + `insecure_skip_verify`, and
  reused across every `Invoke` call rather than logging in from scratch each
  time.
- If a request comes back `401` (the controller's session expired
  server-side), the connector transparently re-logs-in **once** and retries
  the request before surfacing an error.

### `unifi_os`: two different controller shapes

| | `unifi_os: true` (default) | `unifi_os: false` (legacy) |
|---|---|---|
| Hardware | UDM, UDM Pro, UDM SE, Cloud Key Gen2+, UniFi OS Console | Standalone `unifi` controller software (older Cloud Key, self-hosted Java controller) |
| Login | `POST /api/auth/login` | `POST /api/login` |
| API prefix | `/proxy/network` (the network application is proxied behind the console's front door) | *(none)* |
| CSRF | `X-CSRF-Token` required on writes | not used |

Every verb's path is `base_url + <prefix> + /api/s/{site}/...` (or
`/api/self/sites`, which is not site-scoped). Get `unifi_os` wrong and every
request 404s against the wrong path shape — check your controller's login
URL in a browser if you're unsure which one you have.

### `insecure_skip_verify`: read this before enabling

Home UniFi controllers commonly run behind a self-signed certificate on a
LAN. `insecure_skip_verify: true` builds a **per-connection** `tls.Config`
with certificate verification disabled — it never changes Go's process-wide
TLS defaults, so it cannot affect any other connector or outbound call in
the same conductor process. Even so, enabling it disables all protection
against a man-in-the-middle on the path to the controller: anyone who can
intercept the connection can read your session cookie/CSRF token and forge
responses. Only enable it for a controller reached over a trusted network
(the same LAN, or a VPN), and prefer installing a real certificate when
possible.

## No source: the API is request/response only

The UniFi Network API has no webhook or event-stream mechanism to listen
on — every endpoint (including `events`, which merely lists recent events on
demand) is a plain synchronous request/response. This connector is
verb-only: it declares no `Events` and does not implement `StartSource`.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`.

UniFi wraps every response as `{"meta": {...}, "data": [...]}`. This
connector hoists `data` into `items` when it is a list (the common case —
even an empty list is returned as `items: []`, never omitted), or into
`result` when it is present but not a list. A response with no `data`
envelope at all falls back to `result`.

| verb | endpoint | outputs |
|------|----------|---------|
| `sites` | `GET {prefix}/api/self/sites` | `items` |
| `devices` | `GET {prefix}/api/s/{site}/stat/device` | `items` |
| `device_restart` | `POST {prefix}/api/s/{site}/cmd/devmgr` (`mac`\*) — `{cmd: "restart", mac}` | `items` |
| `clients` | `GET {prefix}/api/s/{site}/stat/sta` | `items` |
| `client_block` | `POST {prefix}/api/s/{site}/cmd/stamgr` (`mac`\*) — `{cmd: "block-sta", mac}` | `items` |
| `client_unblock` | `POST {prefix}/api/s/{site}/cmd/stamgr` (`mac`\*) — `{cmd: "unblock-sta", mac}` | `items` |
| `client_reconnect` | `POST {prefix}/api/s/{site}/cmd/stamgr` (`mac`\*) — `{cmd: "kick-sta", mac}` | `items` |
| `wlans` | `GET {prefix}/api/s/{site}/rest/wlanconf` | `items` |
| `networks` | `GET {prefix}/api/s/{site}/rest/networkconf` | `items` |
| `port_forwards` | `GET {prefix}/api/s/{site}/rest/portforward` | `items` |
| `firewall_rules` | `GET {prefix}/api/s/{site}/rest/firewallrule` | `items` |
| `health` | `GET {prefix}/api/s/{site}/stat/health` | `items` |
| `alarms` | `GET {prefix}/api/s/{site}/list/alarm` | `items` |
| `events` | `GET {prefix}/api/s/{site}/stat/event` | `items` |
| `api` | `method` + `path` (joined after `{prefix}`) + `query` + `body` — escape hatch for anything without a first-class verb | `items` (list `data`) or `result` (non-list `data`/no envelope) |

`device_restart`, `client_block`, `client_unblock`, and `client_reconnect`
all take a required `mac` option (the target device or client's MAC
address) and issue a controller "command" — UniFi's `cmd/devmgr` and
`cmd/stamgr` endpoints, which take a JSON body naming the action (`cmd`)
rather than exposing one endpoint per action.

`site` is a **connection**-level setting, not a per-verb option: every verb
targets the one site the connector instance is configured for. Point a
second connector instance at a different `site` if you manage more than one.

## Capabilities & security

Declares empty egress — a UniFi Network controller is self-hosted, so there
is no fixed public host to declare. Set `network: ["<your-host>:443"]` on
the connector instance to narrow it to your own controller. The
username/password are a full controller login (the same credentials as the
web UI); scope a dedicated local account to the least privilege the
workflow needs rather than reusing your primary admin login.
