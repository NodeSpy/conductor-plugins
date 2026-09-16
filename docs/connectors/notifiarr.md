# `notifiarr` connector

Notifiarr as a connector: send Discord notifications through Notifiarr's
Passthrough integration, plus a generic API escape hatch. Verb-only, built
directly on `net/http` — no third-party client.

- **Kind:** connector (verbs only, no source)
- **Source:** [`connectors/notifiarr/main.go`](../../connectors/notifiarr/main.go)
- **Provides:** `notifiarr`
- **Capabilities:** egress to `notifiarr.com:443`

```yaml
connectors:
  notifiarr:
    use: notifiarr
    api_key: ${NOTIFIARR_API_KEY}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_key` | string | **required.** Notifiarr API key |
| `api_base` | string | override the API base URL (default `https://notifiarr.com/api/v1`; tests or a private gateway) |

## Verbs

### `passthrough`

Send a Discord notification via Notifiarr's Passthrough integration. Accepts
a flat, ergonomic set of options and assembles Notifiarr's documented nested
`notification`/`discord` request shape itself — callers never hand-build the
envelope.

| option | maps to | required |
|--------|---------|----------|
| `title` | `notification.name`, `discord.text.title` | yes |
| `message` | `discord.text.description` | yes |
| `channel_id` | `discord.ids.channel` | yes |
| `color` | `discord.color` (hex, no `#`) | |
| `event` | `notification.event` | |
| `ping_user` | `discord.ping.pingUser` | |
| `ping_role` | `discord.ping.pingRole` | |
| `icon` | `discord.text.icon` (URL) | |
| `image` | `discord.text.images.image` | |
| `thumbnail` | `discord.text.images.thumbnail` | |
| `footer` | `discord.text.footer` | |
| `fields` | `discord.text.fields`: a list of `{title, text, inline}` | |

Posts `POST /notification/passthrough/{api_key}` with the assembled body:

```json
{
  "notification": {"update": false, "name": "<title>", "event": "<event>"},
  "discord": {
    "color": "<color>",
    "ping": {"pingUser": "<ping_user>", "pingRole": "<ping_role>"},
    "images": {"thumbnail": "<thumbnail>", "image": "<image>"},
    "text": {
      "title": "<title>", "icon": "<icon>", "content": "",
      "description": "<message>", "fields": [...], "footer": "<footer>"
    },
    "ids": {"channel": "<channel_id>"}
  }
}
```

Outputs: `result` (the decoded JSON response) + `status_code`.

```yaml
steps:
  - uses: notifiarr.passthrough
    options:
      title: Backup finished
      message: the nightly backup completed successfully
      channel_id: "1234567890"
      color: "00ff00"
```

### `api`

Escape hatch for any Notifiarr endpoint not covered by `passthrough`.

| option | type | required | purpose |
|--------|------|----------|---------|
| `method` | string | yes | `GET`/`POST`/`PUT`/`PATCH`/`DELETE` |
| `path` | string | yes | path relative to `/api/v1` |
| `body` | any | | JSON request body, passed straight through |

Outputs: `result` + `status_code`.

## Capabilities & security

Declares egress to `notifiarr.com:443` only. Provide a scoped, least-
privileged Notifiarr API key.
