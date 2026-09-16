# `ntfy` connector

[ntfy.sh](https://ntfy.sh) push notifications: a single **publish** verb, and a
**source** that subscribes to one or more topics over ntfy's JSON-stream
endpoint (a long-lived `GET .../json`, not a webhook). Built ONLY against the
public SDK (`pkg/plugin`) — no sourcekit, no conductor internals, no
third-party dependencies.

- **Kind:** connector (verb **and** source)
- **Source:** [`connectors/ntfy/main.go`](../../connectors/ntfy/main.go)
- **Provides:** `ntfy`
- **Capabilities:** egress to `ntfy.sh:443` only

```yaml
connectors:
  notify:
    use: ntfy
    token: ${NTFY_TOKEN}
    subscribe: [alerts, ci]
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `server` | string | ntfy server base URL (default `https://ntfy.sh`) |
| `token` | string | Bearer access token |
| `username` | string | Basic auth username (paired with `password`) |
| `password` | string | Basic auth password (paired with `username`) |
| `subscribe` | list | topics to subscribe to (StartSource only) |

`token` and `username`/`password` are alternatives — `token` wins if both are
set.

**Self-hosted servers widen the egress target.** The plugin declares
`ntfy.sh:443` as its capability. Pointing `server:` at a self-hosted instance
(e.g. `https://ntfy.example.com`) means the plugin now calls out to a
*different* host than the one it declared — narrow (or replace) the instance's
`network:` allowlist to match whatever server you actually configure, and
review the capability grant, since conductor's "can't exceed declaration"
enforcement is keyed off the declared host, not whatever `server` happens to
be set to at runtime.

## Source: `message`

Trigger with `on: <name>.message`. The plugin opens `GET
{server}/{topic1,topic2,...}/json` (the configured `subscribe` topics,
comma-joined) and reads the response as newline-delimited JSON, one object per
line. Each line's `event` field is `open` (stream established), `keepalive`
(periodic ping), or `message` (an actual publish) — **only `message` is ever
emitted.** The connection reconnects with exponential backoff (1s, doubling to
a 30s cap) on any stream error or unexpected end, until the daemon stops the
source.

| context key | type | |
|-------------|------|---|
| `topic` | string | the topic the message was published to |
| `message` | string | message body |
| `title` | string | message title |
| `priority` | integer | 1 (min) .. 5 (max) |
| `tags` | list | tags / emoji shortcodes |
| `click` | string | URL attached to the notification |
| `id` | string | ntfy message id (also the dedup key) |
| `time` | integer | unix timestamp |

Events are deduplicated on `id`, so a message ntfy redelivers after a dropped
connection fires only once.

Filters: `topics` (list), `priorities` (list), `tags` (list, matches against
the message's tag list), `topic` (scalar), `priority` (scalar).

```yaml
triggers:
  - on: notify.message
    filters: { topics: [alerts], priorities: [4, 5] }
    steps:
      - uses: pager.page
        options: { summary: "{{.title}}: {{.message}}" }
```

## Verbs

### `publish`

Publish a message to a topic. `topic` is the only required option; ntfy fills
in sane defaults for everything else.

| option | type | |
|--------|------|---|
| `topic` | string, required | the ntfy topic to publish to |
| `message` | string | message body |
| `title` | string | notification title |
| `priority` | any | `1`..`5`, or the name: `min`/`low`/`default`/`high`/`max` |
| `tags` | list | tags / emoji shortcodes |
| `click` | string | URL opened when the notification is tapped |
| `attach` | string | URL of a file to attach |
| `actions` | any | action buttons, passed through verbatim (list or map, per ntfy's action spec) |
| `email` | string | also forward the message to this e-mail address |
| `delay` | string | schedule delivery, e.g. `"30min"`, `"tomorrow, 9am"` |
| `markdown` | boolean | render the message as Markdown |

Sends a JSON `POST` to the server root (ntfy's JSON publish endpoint) with a
body built from the options above (`topic` plus whichever others were set),
carrying the configured Bearer or Basic auth.

Outputs:

| output | type | |
|--------|------|---|
| `status_code` | integer | the HTTP status ntfy returned |
| `result` | any | the parsed JSON response body |

```yaml
steps:
  - uses: notify.publish
    options:
      topic: alerts
      title: "disk is full"
      message: "/data is at 97%"
      priority: high
      tags: [warning, computer]
```

## Capabilities & security

Declares egress to `ntfy.sh:443` only. Narrow it per instance with `network:`
when pointed at the default server; when pointed at a self-hosted `server:`,
update the allowlist to match that host (see above) — the declaration does
not follow `server` automatically.
