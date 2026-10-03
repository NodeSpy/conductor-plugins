# `localxpose` connector

An **exposure connector** (conductor docs/design/plugin-contract.md §2.3,
the `exposes` verb semantic): it runs `loclx tunnel http` to make a local
address reachable from outside at a `*.loclx.io` URL, read off the CLI's own
output.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/localxpose/main.go`](../../connectors/localxpose/main.go)
- **Provides:** `localxpose`
- **Capabilities:** spawns `loclx`

```yaml
connectors:
  tun: { use: localxpose }
  web: { use: web, expose: tun }
```

## Setup

**Prerequisites:** the `loclx` CLI on PATH — see
[LocalXpose's install docs](https://localxpose.io/docs/installation).

1. Sign up at [localxpose.io](https://localxpose.io) if you want a reserved
   subdomain, region pinning, or higher limits than the free tier.
2. Authenticate the CLI once on the host per LocalXpose's own docs (`loclx
   account login`), or pass whatever flag your plan needs through
   `extra_args` below.

```yaml
connectors:
  tun: { use: localxpose }
```

### Account-specific flags (reserved subdomain, region, access token)

LocalXpose's CLI flags for these vary by plan and change independently of
this plugin, so they're passed through rather than hard-coded:

```yaml
connectors:
  tun:
    use: localxpose
    extra_args: ["--subdomain", "myapp", "--region", "us"]
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `binary` | string | override the loclx binary path (default `loclx`) |
| `extra_args` | list | raw flags inserted before `--to`, e.g. a reserved subdomain, region, or access token |
| `start_timeout` | duration | how long to wait for the URL before failing (default `30s`) |

## Verbs

### `open` — start a tunnel (`exposes`)

| option | type | |
|--------|------|---|
| `local_addr` * | string | `host:port` to expose |

Outputs `public_url`, `lease`.

### `close` — end a tunnel

`lease` * (the value `open` returned). Idempotent.

## Notes

- Every lease spawns its own `loclx` process; `close` (or conductor stopping
  the instance) kills it.
- The public URL is matched with `https://[a-zA-Z0-9.-]+\.loclx\.io\S*`
  against the CLI's own output — the same pattern conductor's pre-contract
  tunnel code used.
