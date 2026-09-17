# `unifi-protect` connector

UniFi Protect (NVR/cameras) as a connector: list/inspect cameras, PTZ control,
snapshots, NVR details, viewers, lights, sensors, and chimes over the modern
Protect **Integration API**, plus a raw `api` escape hatch. Built on the
standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/unifi-protect/main.go`](../../connectors/unifi-protect/main.go)
- **Provides:** `unifi-protect`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own console)

```yaml
connectors:
  protect:
    use: unifi-protect
    base_url: https://192.168.1.1
    api_key: ${PROTECT_API_KEY}
    network: ["192.168.1.1:443"]   # narrow the declared egress to your console
```

## Setup

Produces a Protect Integration API key the connector sends as `X-API-KEY`.

**Prerequisites:** a running UniFi OS console (UDM/UDM Pro/UDM SE) with
Protect installed, and admin/owner access to the console.

1. Log into the console's local UI and open **Settings > Control Plane >
   Integrations** (path varies by UniFi OS version; older releases expose it
   directly at `https://<console>/protect/settings/control-plane/integrations`).
2. Under **Your API Keys** (or **API Key**), click **Create API Key**.
3. Give it a name and click **Create** — the key is shown once, so copy it
   immediately.
4. If the console doesn't expose this page, sign into
   [unifi.ui.com](https://unifi.ui.com) (UniFi Site Manager) and generate the
   key under **Settings > API Keys** instead.

```yaml
connectors:
  protect:
    use: unifi-protect
    base_url: https://192.168.1.1
    api_key: ${PROTECT_API_KEY}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | UniFi OS console root, e.g. `https://192.168.1.1` (no trailing path) |
| `api_key` | string | Protect Integration API key, sent as `X-API-KEY` |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) |

## Auth: a static API key, not a cookie session

Unlike the sibling [`unifi`](./unifi.md) Network connector — which logs in
with a username/password and keeps a session cookie — UniFi Protect's modern
**Integration API** (UniFi OS 4.x / Protect 5.x) authenticates every request
with a static API key, sent as the `X-API-KEY` header. There is no login
call, no cookie jar, and nothing to re-authenticate on expiry: the key is
valid until revoked in the console's UI.

Every verb's endpoint is relative to a fixed base:

```
{base_url}/proxy/protect/integration/v1
```

### `insecure_skip_verify`: read this before enabling

UDMs and other UniFi OS consoles commonly run behind a self-signed
certificate on a LAN. `insecure_skip_verify: true` builds a
**per-connection** `tls.Config` with certificate verification disabled — it
never changes Go's process-wide TLS defaults, so it cannot affect any other
connector or outbound call in the same conductor process. Even so, enabling
it disables all protection against a man-in-the-middle on the path to the
console: anyone who can intercept the connection can read your API key and
forge responses. Only enable it for a console reached over a trusted network
(the same LAN, or a VPN), and prefer installing a real certificate when
possible.

## Response shaping

Protect's list endpoints (`cameras`, `viewers`, `lights`, `sensors`,
`chimes`) return a **bare JSON array**, hoisted into `items` (even an empty
list is returned as `items: []`, never omitted). Singular/object endpoints
(`meta`, `camera_get`, `nvrs`) return `result`. Every verb's outputs also
include `status_code`. A non-2xx response is returned as an error carrying
the status code and response body — nothing is swallowed.

`camera_snapshot` is the one exception: its response is a JPEG image, not
JSON. The raw bytes are base64-encoded into `image_base64`, alongside
`content_type` and `status_code` — there is no `result`/`items` for this verb.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema.

| verb | endpoint | outputs |
|------|----------|---------|
| `meta` | `GET /meta/info` | `result` |
| `cameras` | `GET /cameras` | `items` |
| `camera_get` | `GET /cameras/{camera_id}` | `result` |
| `camera_snapshot` | `GET /cameras/{camera_id}/snapshot` (`high_quality`) | `image_base64`, `content_type` |
| `camera_ptz` | `POST /cameras/{camera_id}/ptz/goto/{slot}` \| `/ptz/patrol/start/{slot}` \| `/ptz/patrol/stop` (`action`\*, `slot`) | `result` |
| `nvrs` | `GET /nvrs` | `result` |
| `viewers` | `GET /viewers` | `items` |
| `lights` | `GET /lights` | `items` |
| `sensors` | `GET /sensors` | `items` |
| `chimes` | `GET /chimes` | `items` |
| `api` | `method` + `path` (under the integration v1 base) + `query` + `body` — escape hatch for anything without a first-class verb | `result` (object response) or `items` (array response) |

`camera_snapshot` sets `?highQuality=true` only when `high_quality: true` is
passed; otherwise the console's default resolution is used.

`camera_ptz`'s `action` selects which of the three confirmed PTZ endpoints to
call: `goto` and `patrol_start` move to a saved preset `slot` (both require
`slot`); `patrol_stop` halts an active patrol and takes no `slot`.

`nvrs` has no path parameter and returns a single object — one UniFi OS
console has exactly one NVR, so the Integration API exposes it as a
singleton rather than a list.

There is no first-class verb for looking up a single viewer, light, sensor,
or chime by ID (the API supports `GET /{viewers,lights,sensors,chimes}/{id}`,
but v1 keeps the connector's surface to what's specified); reach one with the
`api` escape hatch, e.g. `path: /viewers/abc123`.

## No source (yet): live detection events need a WebSocket client

The Integration API's smart-detection events (person, vehicle, motion, and
so on) are delivered over a **WebSocket subscription**, not a webhook or a
pollable REST endpoint. A stdlib-only plugin has no `net/http` WebSocket
client to speak that protocol with, so this connector is **verb-only for
v1**: it declares no `Events` and does not implement `StartSource`.

A detection-event source is a planned follow-up — it needs a real WebSocket
client (either a new stdlib-adjacent dependency or a small hand-rolled
RFC 6455 client), which is out of scope for this first pass.

## Capabilities & security

Declares empty egress — a UniFi Protect console is self-hosted, so there is
no fixed public host to declare. Set `network: ["<your-console>:443"]` on the
connector instance to narrow it to your own console. Scope the API key to the
least privilege the workflow needs; unlike a controller login, a Protect API
key cannot be scoped narrower than "this console's Integration API" from the
console UI itself, so treat it as a credential worth rotating if a workflow
using it is ever compromised.
