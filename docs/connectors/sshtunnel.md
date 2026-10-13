# `sshtunnel` connector

An **exposure connector** (conductor docs/design/plugin-contract.md §2.3,
the `exposes` verb semantic): it opens an ssh remote-port-forward to a
tunnelling host — [localhost.run](https://localhost.run),
[serveo.net](https://serveo.net), [pinggy](https://pinggy.io), or any other
host offering the same "ssh in, get a public URL back" convention — and
reads the URL off the ssh session's own banner.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/sshtunnel/main.go`](../../connectors/sshtunnel/main.go)
- **Provides:** `sshtunnel`
- **Capabilities:** spawns `ssh`

```yaml
connectors:
  tun: { use: sshtunnel, host: localhost.run }
  web: { use: web, expose: tun }
```

## Setup

**Prerequisites:** the `ssh` client on PATH (installed on essentially every
Linux/macOS box already). No account is needed for localhost.run or
serveo.net; pinggy's free tier works the same way.

```yaml
connectors:
  tun: { use: sshtunnel, host: localhost.run }
```

Most of these services (localhost.run, serveo.net, and any host offering the
same convention) use a plain `-R 80:localhost:<port>` forward and print the
assigned URL once connected — that's the **generic** preset, used by
default. **Pinggy** needs its own port/remote-forward form
(`-p 443 -R 0:localhost:<port>`); it's auto-detected from the host name
(`pinggy` or anything under `.pinggy.io`) or pinned with `preset`:

```yaml
connectors:
  tun: { use: sshtunnel, host: a.pinggy.io }   # preset auto-detected as pinggy
```

### A custom key or tunnel host

```yaml
connectors:
  tun:
    use: sshtunnel
    host: tunnel.example.com
    user: tun
    identity_file: /home/me/.ssh/id_ed25519
    port: 2222
    remote_port: 8080
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `host` * | string | the ssh tunnel host, e.g. `localhost.run`, `serveo.net`, `a.pinggy.io` |
| `preset` | string | `generic` \| `pinggy` — overrides host-name auto-detection |
| `user` | string | ssh user, if the host wants one other than its default |
| `identity_file` | string | path to an ssh private key (`-i`) |
| `port` | string | ssh connection port (default `22`, or `443` for the pinggy preset) |
| `remote_port` | string | forwarded remote port for the generic preset (default `80`); ignored for pinggy |
| `extra_args` | list | raw ssh flags inserted before the host |
| `start_timeout` | duration | how long to wait for the URL before failing (default `30s`) |

## Verbs

### `open` — open a reverse tunnel (`exposes`)

| option | type | |
|--------|------|---|
| `local_addr` * | string | `host:port` to expose |

Outputs `public_url`, `lease`.

### `close` — end a tunnel

`lease` * (the value `open` returned). Idempotent — kills the ssh process,
tearing the forward down.

## Notes

- Every lease opens its own `ssh` process; `close` (or conductor stopping the
  instance) kills it.
- Connects with `-o StrictHostKeyChecking=accept-new`, matching conductor's
  pre-contract tunnel behavior: the host key is trusted on first use and
  pinned for the rest of the session, rather than prompted for or rejected
  outright.
- The URL is matched with a generic `https?://\S+` pattern against the ssh
  session's banner text — these services print it inline, in varying
  wording, so no service-specific regex is used (ported verbatim from
  conductor's pre-contract tunnel code).
