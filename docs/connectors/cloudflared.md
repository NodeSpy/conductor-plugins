# `cloudflared` connector

An **exposure connector** (conductor docs/design/plugin-contract.md §2.3,
the `exposes` verb semantic): it spawns `cloudflared tunnel` to make a local
address reachable from outside, and reads the public URL off its output.
It is the named-vendor counterpart to conductor's vendor-neutral `tunnel`
builtin (which runs any tunnelling command) — this plugin bakes in
cloudflared's own argv shapes, URL pattern and named-tunnel wiring.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/cloudflared/main.go`](../../connectors/cloudflared/main.go)
- **Provides:** `cloudflared`
- **Capabilities:** spawns `cloudflared`

```yaml
connectors:
  tun: { use: cloudflared }
  web: { use: web, expose: tun }
```

Any connector declaring a `listeners` semantic (a webhook source with
`expose:`) or the web hand-off connector can name `tun` in its own `expose:`
field — the engine calls `open`/`close` on your behalf; you never invoke
`cloudflared.open` directly in a flow.

## Setup

**Prerequisites:** the `cloudflared` binary on PATH — see
[Cloudflare's install docs](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/).
No Cloudflare account is required for the default **quick** mode.

### Quick mode (default) — zero setup

```yaml
connectors:
  tun: { use: cloudflared }
```

Each lease gets a free, ephemeral `*.trycloudflare.com` hostname. It changes
every time a new lease opens (every tunnel run gets a fresh one), which is
fine for the web hand-off's per-draft links but not for anything that needs
a stable address.

### Named mode — a persistent, DNS-routed tunnel

1. `cloudflared tunnel login` (one-time browser auth to your Cloudflare account).
2. `cloudflared tunnel create my-tunnel` — note the tunnel ID/name and the
   credentials JSON path it prints.
3. `cloudflared tunnel route dns my-tunnel app.example.com` — points the
   hostname at your tunnel in Cloudflare DNS.

```yaml
connectors:
  tun:
    use: cloudflared
    mode: named
    tunnel: my-tunnel
    hostname: app.example.com
    credentials_file: /home/me/.cloudflared/abc-123.json
```

The public URL is always `https://app.example.com` here — it's fixed by the
DNS route, not parsed from cloudflared's output (a named tunnel's own log
lines don't carry it). The plugin instead waits for cloudflared to report the
connection registered before returning.

## Connection

| key | type | purpose |
|-----|------|---------|
| `mode` | string | `quick` (default) or `named` |
| `binary` | string | override the cloudflared binary path (default `cloudflared`) |
| `tunnel` | string | **named mode:** the tunnel's name or UUID |
| `hostname` | string | **named mode:** the DNS-routed public hostname |
| `credentials_file` | string | **named mode:** path to the tunnel's credentials JSON (default: cloudflared's own search path) |
| `config` | string | path to a `cloudflared` config.yml |
| `extra_args` | list | raw flags inserted after the `tunnel` subcommand |
| `start_timeout` | duration | how long to wait for the tunnel to come up (default `30s`) |

## Verbs

### `open` — start a tunnel (`exposes`)

| option | type | |
|--------|------|---|
| `local_addr` * | string | `host:port` to expose |

Outputs `public_url`, `lease`. This is what the engine calls when a consumer
names this connector in `expose:`; you don't normally call it from a flow.

### `close` — end a tunnel

`lease` * (the value `open` returned). Idempotent.

## Notes

- Every lease spawns its own `cloudflared` process; `close` (or conductor
  stopping the instance) kills it.
- Quick-mode URLs are matched with
  `https://[a-zA-Z0-9.-]+\.trycloudflare\.com\S*` against cloudflared's own
  banner — the same pattern conductor's pre-contract tunnel code used.
