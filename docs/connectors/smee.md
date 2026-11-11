# `smee` connector

An **exposure connector** (conductor docs/design/plugin-contract.md §2.3,
the `exposes` verb semantic) for [smee.io](https://smee.io)-style relay
channels.

Every other exposure plugin in this repo (cloudflared, ngrok, localxpose,
sshtunnel) forwards real inbound traffic to a local port. A smee channel
works the other way around: the **public** side (GitHub, GitLab, …) POSTs to
the channel URL, and **this machine** makes an outbound, long-lived SSE
connection to receive each forwarded delivery. So `open` here means: create
(or reuse) a channel, and start relaying every delivery it receives as an
HTTP POST to the local address. From a consumer's point of view (the generic
`listeners` semantic, §2.4) this is indistinguishable from a real tunnel —
traffic aimed at the returned `public_url` arrives at `local_addr`.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/smee/main.go`](../../connectors/smee/main.go)
- **Provides:** `smee`
- **Capabilities:** egress to `smee.io:443` plus the hosts of the configured
  `smee_base` and `channel` (a pinned channel on a self-hosted smee server);
  `expose_local` — under conductor's plugin jail, it may POST to the
  listeners conductor wires to it with `expose:`, and to nothing else on
  the machine (never one of conductor's own ports, so not the bundled
  `webhook` connector's listener unless this connector is `trust: full`);
  spawns nothing (no subprocess — the relay is an in-process HTTP client)

```yaml
connectors:
  relay: { use: smee }
  web:    { use: web, expose: relay }
```

This is the same `expose:` the web hand-off page uses with any other
exposure connector — smee works as a drop-in alternative to cloudflared/
ngrok/etc. there, with the tradeoffs under **Known limitations** below.

**For a webhook SOURCE plugin** (github, gitlab, …), the intended path is
the generic `listeners` semantic (§2.4): the plugin declares
`{listen: <field>, expose: <field>, url_to: <field>, path: <field>}`, and the
engine opens the exposure for it automatically, resolving the listener's own
HTTP path and handing it straight to this plugin's `open` call as the `path`
option (below) — no vendor-specific code, and no separately configured path,
in the source plugin at all. The github plugin (`internal/githubkit/ghplugin`
in this repo) declares `listeners` this way, naming `webhook.path`, and so
does conductor's bundled `webhook` connector; check a plugin's own docs for
whether it has adopted it, and its exact field names, before relying on this
path for others. Several webhook-consuming plugins here still have their own
ad hoc `webhook.smee` config field (see e.g. [`gitea`](gitea.md)) as a
pre-`listeners` alternative that talks to smee.io directly, in-process, with
no exposure connector involved.

```yaml
connectors:
  gh:
    use: github
    webhook: { expose: gh-smee, secret: ${GH_WEBHOOK_SECRET} }   # listen/path optional: conductor picks them
  gh-smee: { use: smee, channel: ${GH_SMEE_URL} }
```

**Coming from `webhook.smee_url`.** The github plugin's own `smee_url` and
the bundled `webhook` connector's `smee_url` are retired in favour of this
connector. Conductor rewrites them at boot (or with
`conductor config migrate`) into the shape above: a `smee` connector pinned
to the **same** channel, so the sender keeps posting where it always did,
and the receiver's `expose:` pointing at it. It installs this plugin before
writing anything.

**Path precedence.** This plugin's `open` verb declares `exposes.path`
(conductor docs/design/plugin-contract.md §2.3): when a `listeners`-declaring
consumer's resolved path arrives as the `path` OPTION on an `open` call, it
always wins. The connection's own `path` field (below) is consulted only
when no `path` option is given at all — an operator using smee as a plain
web exposure (`expose:` on the web hand-off page, say) with no listener
involved. There is never a case where both are set and the connection field
wins: an operator migrating a consumer onto `listeners` can simply delete
their old `path:` entry here, rather than having to keep the two in sync.

## Setup

No account or binary needed — smee.io is a free public relay service with no
signup. If you'd rather not depend on it, point `smee_base` at a self-hosted
[smee.io server](https://github.com/probot/smee.io) instead:

```yaml
connectors:
  relay: { use: smee, smee_base: https://smee.internal.example.com }
```

### A stable channel (recommended for anything you register a webhook against)

By default, `open` creates a **fresh** channel every time it's called with no
`channel` configured, and remembers it (via `host.state`) so a plugin restart
reuses the same one rather than minting a new one — but the FIRST channel a
fresh install creates is still random. If you're about to paste the
resulting URL into GitHub's (or another vendor's) webhook settings, create
the channel yourself once and pin it, so the URL is known ahead of time and
survives a full reinstall, not just a restart:

1. Visit [smee.io](https://smee.io) and click **Start a new channel** (or
   `curl -i https://smee.io/new` and read the `Location` header).
2. Pin it:

```yaml
connectors:
  relay: { use: smee, channel: https://smee.io/AbC123dEf }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `channel` | string | a pinned smee channel URL, e.g. `https://smee.io/AbC123` (default: create a fresh one) |
| `smee_base` | string | override `https://smee.io` (a self-hosted smee server, or tests) |
| `path` | string | local HTTP path deliveries are replayed to (default `/`) — **fallback default only**; see **Path precedence** above and **Known limitations** below |
| `persist` | boolean | remember a *created* channel across restarts via `host.state` (default `true`; irrelevant when `channel` is pinned) |

## Verbs

### `open` — create or reuse a channel and start relaying (`exposes`, `exposes.path: path`)

| option | type | |
|--------|------|---|
| `local_addr` * | string | `host:port` deliveries are replayed to |
| `path` | string | HTTP path to replay deliveries to — set by the engine from a consumer's `listeners` semantic; falls back to the connection's own `path` field when absent |

Outputs `public_url` (the channel URL, unchanged — see **Path precedence**), `lease`.

### `close` — stop relaying

`lease` * (the value `open` returned). Idempotent.

## Relay behaviour

The channel is one long-lived SSE stream. TCP keep-alive probes it (every
15s after 20s idle, four misses), so a connection a NAT or proxy dropped
silently is noticed in about a minute and redialed; the retry backoff
starts over once a connection is up again. A delivery is POSTed to
`local_addr` + path with the original headers; the relay does not retry a
local POST that fails.

## Known limitations

These are documented precisely rather than worked around silently:

- **A consumer that does not declare `listeners.path` still needs the
  connection's `path` field set by hand.** The `listeners` semantic's `path`
  is optional, and the engine's own default when a listener names no path
  field (or the field is unset) is `/` — never whatever THIS plugin's own
  `path` connection field says, and never whatever the consuming plugin's
  own internal default is. A consumer not yet declaring `listeners.path` (or
  one whose own webhook path defaults to something other than `/`, with the
  operator relying on that internal default instead of setting the field
  explicitly) still needs this plugin's `path` field set to match by hand,
  exactly as before this feature.
- **smee.io re-serializes JSON bodies.** An HMAC computed over the relayed
  bytes may not byte-match one computed over the sender's original bytes.
  This is a pre-existing smee.io characteristic (not introduced by this
  plugin) — several of this repo's other connectors already document the
  same caveat for their own built-in `webhook.smee` relay field. Use a
  direct tunnel (cloudflared, ngrok, …) instead when byte-exact signature
  verification matters.
- **One relay per lease.** Two leases opened against the same pinned
  `channel` both receive every delivery — smee.io has no server-side
  fan-out control, so avoid pointing more than one `open` at one channel
  unless duplicate delivery is actually what you want.
