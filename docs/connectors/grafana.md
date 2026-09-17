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

## Setup

Produces a Grafana service account token the connector sends as a Bearer token.

**Prerequisites:** a running Grafana instance and admin access to it.

1. Log into Grafana and open **Administration > Users and access > Service
   accounts**.
2. Click **Add service account**, give it a name, and set its role (e.g.
   `Viewer` or `Editor`, scoped to what the workflow needs).
3. Open the new service account and click **Add service account token**.
4. Give the token a name (and optional expiry), click **Generate token**,
   and copy it — it's shown once. (Legacy **API keys** still work but are
   deprecated in favor of service account tokens.)

```yaml
connectors:
  monitoring:
    use: grafana
    base_url: https://grafana.example.com
    api_key: ${GRAFANA_API_KEY}
```

See **Source — the `alert` event** below for the poll source that watches
the Grafana-managed Alertmanager.

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

Selected by `uses: <name>.<verb>`. Every verb's outputs include
`status_code`; the rest return either `result` (a single object) or `items`
(a bare JSON array, hoisted automatically). Required options are marked `*`.

### Health & search

- **`health`** — check API health. No options. → `result`, `status_code`.
- **`search`** — search dashboards/folders. `query` (search text), `type`
  (enum `dash-db` | `dash-folder`, restrict to dashboards or folders), `tag`
  (list, restrict to results carrying **all** of these tags — each value is
  sent as its own repeated `?tag=` parameter). → `items`, `status_code`.

### Dashboards

- **`dashboard_get`** — get one dashboard by uid. `uid`*. → `result`,
  `status_code`.
- **`dashboard_create`** — create or update a dashboard. `dashboard`* (the
  full dashboard JSON model, passed through verbatim as the request body's
  `dashboard` field), `folder_uid` (destination folder uid; empty = General;
  maps to Grafana's `folderUid`), `overwrite` (boolean, overwrite an existing
  dashboard on a uid/version conflict). → `result`, `status_code`.
- **`dashboard_delete`** — delete a dashboard by uid. `uid`*. → `result`,
  `status_code`.

### Datasources & folders

- **`datasources`** — list datasources. No options. → `items`, `status_code`.
- **`datasource_get`** — get one datasource by uid. `uid`*. → `result`,
  `status_code`.
- **`folders`** — list folders. No options. → `items`, `status_code`.
- **`folder_create`** — create a folder. `title`*. → `result`, `status_code`.

### Alerting

- **`alert_rules`** — list provisioned alert rules. No options. → `items`,
  `status_code`.

### Annotations

- **`annotations`** — search annotations. `from` (integer, epoch millis
  range start), `to` (integer, epoch millis range end), `tags` (list,
  restrict to annotations carrying **all** of these tags — each value sent as
  its own repeated `?tags=` parameter). → `items`, `status_code`.
- **`annotation_create`** — create an annotation. `dashboard_uid` (attach to
  this dashboard; maps to `dashboardUID`), `panel_id` (integer, attach to
  this panel within the dashboard; maps to `panelId`), `time` (integer,
  epoch millis, defaults to now on the server), `time_end` (integer, epoch
  millis, makes the annotation a region; maps to `timeEnd`), `tags` (list),
  `text`*. → `result`, `status_code`.

### Org

- **`org`** — the authenticated org. No options. → `result`, `status_code`.

### Escape hatch

- **`api`** — raw escape hatch for any Grafana API endpoint without a
  first-class verb. `method` (HTTP method, default `GET`), `path`* (path
  under `base_url`, e.g. `/api/dashboards/uid/abc123`), `query` (map, query
  string parameters), `body` (any, JSON request body). → `result` (object
  response) or `items` (array response), `status_code`.

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

### Filtering

A trigger's `filter:`/`filters:` matches an event's published context fields
(the table above). The grammar:

- A key set to a value must match; a **list matches any of** its values —
  `severities: [critical, warning]`.
- Prefix **`not_`** to negate a field — `not_severities: [info]` excludes.
- **`expr:` / `not_expr:`** take an expression over the fields —
  `expr: "alertname == 'HighCPU'"`.
- Keys within one filter object are **AND**ed. A top-level **array** of
  filter objects is **OR** across them (one arm per rule).

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
