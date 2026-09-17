# `grafana` connector

Grafana as a connector: health, dashboard search/get/create/delete,
datasources, folders, provisioned alert rules, annotations, org info, and a
raw `api` escape hatch over the Grafana HTTP API — plus a **poll source**
that watches the Grafana-managed Alertmanager and emits an `alert` event for
every currently-firing alert. Built on the standard library's `net/http`
only.

- **Kind:** connector (verbs + source)
- **Source:** [`connectors/grafana/main.go`](../../connectors/grafana/main.go)
- **Provides:** `grafana`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your own instance)

```yaml
connectors:
  monitoring:
    use: grafana
    base_url: https://grafana.example.com
    api_key: ${GRAFANA_API_KEY}
    network: ["grafana.example.com:443"]   # narrow the declared egress to your instance
triggers:
  - on: monitoring.alert
    filters: { severities: [critical] }
    steps:
      - id: page
        uses: pd.trigger
        options: { summary: "{{.title}}" }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | Grafana instance root, e.g. `https://grafana.example.com` (no trailing `/api`) |
| `api_key` | string | service-account token or legacy API key, sent as `Authorization: Bearer <api_key>` |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) |
| `poll_interval` | duration | **source** poll period (default `1m`) |

Every verb calls `base_url + <path>` — unlike some connectors, Grafana's
paths are not uniformly rooted under one prefix (`/api` for most endpoints,
`/api/v1` for provisioning), so each verb's own path already carries its
full leading segment. Authentication is always the same Bearer token,
whether it is a modern service-account token or a legacy API key. A non-2xx
response is returned as an error carrying the status code and response
body — nothing is swallowed.

### `insecure_skip_verify`: read this before enabling

Self-hosted Grafana instances commonly run behind a self-signed certificate
on a LAN. Setting this to `true` accepts that certificate — but it also
accepts **any** certificate, disabling all protection against a
man-in-the-middle on the path to your instance. Only enable it for instances
reached over a trusted network, and prefer installing a real certificate
(e.g. via a reverse proxy with ACME/Let's Encrypt) when possible.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; the rest return
either `result` (a single object) or `items` (a bare JSON array, hoisted
automatically).

| verb | endpoint | outputs |
|------|----------|---------|
| `health` | `GET /api/health` | `result` |
| `search` | `GET /api/search` (`query`, `type`: `dash-db`\|`dash-folder`, `tag`) | `items` |
| `dashboard_get` | `GET /api/dashboards/uid/{uid}` | `result` |
| `dashboard_create` | `POST /api/dashboards/db` (`dashboard`\*, `folder_uid`, `overwrite`) | `result` |
| `dashboard_delete` | `DELETE /api/dashboards/uid/{uid}` | `result` |
| `datasources` | `GET /api/datasources` | `items` |
| `datasource_get` | `GET /api/datasources/uid/{uid}` | `result` |
| `folders` | `GET /api/folders` | `items` |
| `folder_create` | `POST /api/folders` (`title`\*) | `result` |
| `alert_rules` | `GET /api/v1/provisioning/alert-rules` | `items` |
| `annotations` | `GET /api/annotations` (`from`, `to`, `tags`) | `items` |
| `annotation_create` | `POST /api/annotations` (`dashboard_uid`, `panel_id`, `time`, `time_end`, `tags`, `text`\*) | `result` |
| `org` | `GET /api/org` | `result` |
| `api` | `method` + `path` (under `base_url`) + `query` + `body` — escape hatch for anything without a first-class verb | `result` (object response) or `items` (array response) |

Notes:

- `search`'s `tag` and `annotations`' `tags` are lists — each value is sent
  as its own repeated query parameter (`?tag=a&tag=b`), matching how Grafana
  expects multiple tag filters.
- `dashboard_create`'s `dashboard` option is the full dashboard JSON model,
  passed through verbatim as the request body's `dashboard` field;
  `folder_uid` and `overwrite` map to Grafana's `folderUid`/`overwrite` body
  fields.
- `annotation_create`'s snake_case options map to Grafana's camelCase body
  fields (`dashboard_uid` → `dashboardUID`, `panel_id` → `panelId`,
  `time_end` → `timeEnd`).

## Source — the `alert` event

Every `poll_interval` (default `1m`), the source calls
`GET /api/alertmanager/grafana/api/v2/alerts` — the Grafana-managed
Alertmanager's v2 alerts endpoint — and emits **one `alert` event per alert
whose `status.state` is `"active"`** (currently firing). An alert that is
resolved, silenced, or otherwise not active does not emit.

Events are **deduped on the alert's `fingerprint`**, so an alert that stays
firing for hours emits once, not once per poll cycle. A failed poll backs
off 30s rather than becoming a hot loop; the loop exits cleanly when the
daemon cancels it.

| context | type | notes |
|---------|------|-------|
| `fingerprint` | string | the alert's stable identity, also the dedup key |
| `alertname` | string | `labels.alertname` |
| `severity` | string | `labels.severity` |
| `status` | any | `{state, silencedBy, inhibitedBy}` (state is always `"active"` here) |
| `startsAt` | string | RFC3339 |
| `summary` | string | `annotations.summary` |

| filter | type | matches |
|--------|------|---------|
| `severities` | list | `labels.severity` is one of these |
| `alertnames` | list | `labels.alertname` is one of these |

```yaml
connectors:
  monitoring: { use: grafana, poll_interval: 30s }
triggers:
  - on: monitoring.alert
    filters: { alertnames: [HighCPU, DiskFull] }
    steps:
      - id: notify
        uses: ntfy.publish
        options:
          title: "{{.title}}"
          message: "{{.summary}} (severity: {{.severity}})"
```

## Capabilities & security

Declares no `Egress` (Grafana is commonly self-hosted with no fixed public
host); narrow with `network:` on the connector instance to your own Grafana
host. Every request is a plain `net/http` call carrying a Bearer token —
there is no shelling out and no filesystem access.
