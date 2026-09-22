# `redis` connector

Redis / Valkey as a connector: key verbs (`get`, `set`, `del`, `incr`,
`expire`), `publish`, a raw `command` escape hatch, and a live **pub/sub**
source (`SUBSCRIBE` / `PSUBSCRIBE`) that emits one event per message. Built on
[redis/go-redis/v9](https://github.com/redis/go-redis) (pure Go), which speaks
the same protocol to Redis and to its **Valkey** fork — this connector works
with either.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/redis/main.go`](../../connectors/redis/main.go)
- **Provides:** `redis`
- **Capabilities:** no fixed egress — the server host is yours; narrow it with `network:`.

```yaml
connectors:
  cache:
    use: redis
    url: redis://:${REDIS_PASS}@redis.local:6379/0
    subscribe: ["invalidate"]
    network: ["redis.local:6379"]
```

## Setup

**Prerequisites:** a reachable Redis or Valkey server.

1. Build the connection: either a `url` (`redis://[user:pass@]host:port/db`, or
   `rediss://` for TLS), or the discrete `address` / `username` / `password` /
   `db` fields.
2. For pub/sub keyspace events, make sure whatever publishes uses `PUBLISH` (or
   enable Redis keyspace notifications and `psubscribe` `__keyevent@0__:*`).
3. For a TLS server with a self-signed cert, set `insecure_skip_verify: true`.

**Configure:**

```yaml
connectors:
  cache:
    use: redis
    url: redis://:${REDIS_PASS}@redis.local:6379/0
    subscribe: ["invalidate"]     # channels for the source
    psubscribe: ["cache:*"]       # patterns for the source
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `url` | string | `redis://[user:pass@]host:port/db` or `rediss://` for TLS (wins over the fields below) |
| `address` | string | `host:port` (when not using `url`; default `localhost:6379`) |
| `username` | string | ACL username (Redis 6+) |
| `password` | string | password / ACL secret |
| `db` | integer | database number (default `0`) |
| `insecure_skip_verify` | boolean | skip TLS verification for `rediss://` with a self-signed cert |
| `subscribe` | list | channels to `SUBSCRIBE` to (**StartSource only**) |
| `psubscribe` | list | glob patterns to `PSUBSCRIBE` to, e.g. `cache:*` (**StartSource only**) |

## Source events

Trigger with `on: <name>.<event>`. One event:

| event | fires when | context fields (filter / template) |
|-------|-----------|-------------------------------------|
| `message` | a message arrives on a subscribed channel or pattern | `channel`, `payload`, `pattern` |

`pattern` is the matching `PSUBSCRIBE` glob (empty for a plain `SUBSCRIBE`).

### Filtering

A value must match; a list matches any of its values; prefix `not_` to negate;
`expr:`/`not_expr:` take an expression; a top-level array of objects is OR.
`payload` is text — parse it in a step (e.g. the `jq` engine).

```yaml
triggers:
  - on: cache.message
    filter:
      channels: ["invalidate"]      # any of these exact channels
    steps:
      - use: jq
        code: '{key: .payload}'
```

## Verbs

- **`get`** — get a key's value. `key`*. → `value`, `found` (false when the key doesn't exist).
- **`set`** — set a key, optionally with a TTL. `key`*, `value`*, `ttl` (seconds; `0`/omitted = no expiry). → `ok`.
- **`del`** — delete one or more keys. `keys`* (a list; a single key string is accepted). → `deleted` (count removed).
- **`incr`** — atomically increment a key (creating it at 0 first). `key`*. → `value` (after incrementing).
- **`expire`** — set a key's TTL. `key`*, `ttl`* (seconds). → `ok` (false when the key doesn't exist).
- **`publish`** — publish a message to a pub/sub channel. `channel`*, `message`*. → `receivers` (clients that got it).
- **`command`** — run an arbitrary Redis command (escape hatch). `args`* (the command + arguments, e.g. `["LPUSH", "q", "job1"]`). → `result` (raw reply).

```yaml
steps:
  - uses: cache.set
    options: { key: "session:{{.user}}", value: "{{.token}}", ttl: 3600 }
```

## Capabilities & security

No fixed egress is declared (the server is operator-specific) — narrow it per
instance with `network:` (e.g. `["redis.local:6379"]`); it can never be widened
past the declaration. Spawns nothing. Use an ACL user scoped to the keys and
channels this instance needs.
