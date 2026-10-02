# `ngrok` connector

An **exposure connector** (conductor docs/design/plugin-contract.md §2.3,
the `exposes` verb semantic): it spawns `ngrok http` to make a local address
reachable from outside. The public URL is read off ngrok's own **local API**
(`http://127.0.0.1:4040/api/tunnels` by default), not scanned from its TUI
output — ngrok's terminal UI isn't line-scannable the way a plain CLI's
output is.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/ngrok/main.go`](../../connectors/ngrok/main.go)
- **Provides:** `ngrok`
- **Capabilities:** spawns `ngrok`

```yaml
connectors:
  tun: { use: ngrok, authtoken: ${NGROK_AUTHTOKEN} }
  web: { use: web, expose: tun }
```

## Setup

**Prerequisites:** the `ngrok` binary on PATH (`brew install ngrok`, or see
[ngrok's download page](https://ngrok.com/download)).

1. Sign up at [ngrok.com](https://ngrok.com) and copy your authtoken from the
   dashboard (free tier works).
2. Either run `ngrok config add-authtoken <token>` once on the host, or pass
   `authtoken` in the connector config (below) — either way, no managed
   OAuth is involved; ngrok's own CLI owns this credential.

```yaml
connectors:
  tun: { use: ngrok, authtoken: ${NGROK_AUTHTOKEN} }
```

### A reserved domain (stable URL across leases)

A free ngrok tunnel gets a new random hostname every time. A paid plan's
reserved domain stays fixed:

```yaml
connectors:
  tun: { use: ngrok, authtoken: ${NGROK_AUTHTOKEN}, domain: myapp.ngrok.app }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `binary` | string | override the ngrok binary path (default `ngrok`) |
| `authtoken` | string | passed as `--authtoken`; otherwise ngrok uses its own configured default |
| `domain` | string | a reserved/static ngrok domain, passed as `--domain` |
| `api_addr` | string | ngrok's local API address (default `127.0.0.1:4040`); also passed to ngrok as `--web-addr`, so concurrent leases can each run their own ngrok agent on their own `api_addr` |
| `extra_args` | list | raw flags inserted before the target address |
| `start_timeout` | duration | how long to wait for ngrok's API to report the tunnel (default `30s`) |

## Verbs

### `open` — start a tunnel (`exposes`)

| option | type | |
|--------|------|---|
| `local_addr` * | string | `host:port` to expose |

Outputs `public_url`, `lease`.

### `close` — end a tunnel

`lease` * (the value `open` returned). Idempotent.

## Notes

- Every lease spawns its own `ngrok` process, each on its own `api_addr` (set
  `api_addr` explicitly if you want several leases to share one ngrok agent
  instead — not recommended, since `close` kills the whole process).
- `close` (or conductor stopping the instance) kills the process.
- `ngrok`'s free tier allows one agent session at a time per account; running
  several leases concurrently against the same authtoken will fail at
  ngrok's end, not this plugin's.
