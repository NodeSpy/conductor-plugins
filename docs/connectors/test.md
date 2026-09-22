# `test` connector

A diagnostic connector for exercising conductor itself — **no external service,
no dependencies**. It gives you a synthetic **source** (a `tick` event on an
interval) and a set of action **verbs** (`ping`, `echo`, `sleep`, `fail`,
`counter`, `random`, `now`) so you can smoke-test triggers, filters, grouping,
step wiring, hooks, and gates without standing up a real integration.

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/test/main.go`](../../connectors/test/main.go)
- **Provides:** `test`
- **Capabilities:** none — purely synthetic (no network, no filesystem, no processes).

```yaml
connectors:
  t: { use: test, interval: 2s, labels: [a, b] }

triggers:
  - on: t.tick
    filter: { label: a }        # fires on every other tick
    steps:
      - uses: t.ping
```

## Setup

Nothing to set up — no credentials, no service. Just add the connector. The
config fields below only feed the source.

## Connection

| key | type | purpose |
|-----|------|---------|
| `interval` | duration/number | time between ticks — `"500ms"`, `"2s"`, or a number of seconds (default `2s`, floored at 100ms) — **source only** |
| `count` | integer | stop after N ticks (`0` = forever) — **source only** |
| `message` | string | a static string carried in every tick — **source only** |
| `labels` | list | labels rotated across ticks so a `filter:` has something to match — **source only** |

## Source events

| event | fires when | context fields (filter / template) |
|-------|-----------|-------------------------------------|
| `tick` | every `interval` | `n` (tick number from 1), `label` (rotating), `message`, `time` (unix) |

### Filtering

The usual grammar (value match; list = any-of; `not_` to negate; `expr:`; array
= OR). The rotating `label` and incrementing `n` are there to give filters
something real to exercise:

```yaml
triggers:
  - on: t.tick
    filter:
      labels: [a]               # only ticks whose label is "a"
      not_expr: "n > 100"       # ... and stop matching after n=100
    steps:
      - uses: t.echo
        options: { data: "{{.n}}:{{.label}}" }
```

## Verbs

- **`ping`** — return pong (the simplest liveness check). `message` (echoed back). → `pong` (true), `message`, `instance`, `time`.
- **`echo`** — return the options back verbatim (handy for templating tests). `data` (any). → `data`, `received` (every option passed).
- **`sleep`** — block for a while (to test timeouts / long steps); **capped at 60s**. `seconds` (a number, or `"500ms"`/`"2s"`). → `slept_seconds`.
- **`fail`** — **always returns an error** (to exercise error handling, gates, retries). `message` (default `"test: forced failure"`), `code` (`invalid_params` | `internal`, default internal).
- **`counter`** — increment and return a per-instance counter. `reset` (reset to 0 first, returns 0). → `count`.
- **`random`** — return random values. `max` (int upper bound, exclusive; default 1000000). → `uuid`, `hex`, `int`.
- **`now`** — return the current time. → `unix`, `unix_ms`, `iso` (RFC3339).

```yaml
steps:
  - uses: t.fail
    options: { message: "pretend the deploy failed", code: internal }   # exercise a gate / on-fail hook
```

The `counter` is per plugin process (per connector instance the daemon keeps
alive), so it advances across calls within a run of the daemon and resets when
the daemon restarts — useful for asserting a step ran N times.

## Capabilities & security

Declares nothing — no egress, no spawns, no filesystem. It only ever manipulates
in-memory values and emits synthetic events, so it is safe to leave configured
in any environment.
