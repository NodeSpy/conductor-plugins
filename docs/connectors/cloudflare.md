# `cloudflare` connector

Cloudflare as a connector: DNS records, zones, cache purges, and Workers
scripts as verbs over the Cloudflare API v4, plus a generic `api` escape
hatch for anything without a first-class verb. Verb-only — no source events.

- **Kind:** connector (verbs only)
- **Source:** [`connectors/cloudflare/main.go`](../../connectors/cloudflare/main.go)
- **Provides:** `cloudflare`
- **Capabilities:** egress to `api.cloudflare.com:443`

```yaml
connectors:
  cf:
    use: cloudflare
    api_token: ${CLOUDFLARE_API_TOKEN}
    zone_id: ${CLOUDFLARE_ZONE_ID}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_token` | string | API token; sent as `Authorization: Bearer <token>` (preferred over `api_key`/`email`) |
| `api_key` | string | legacy Global API Key; paired with `email`, sent as `X-Auth-Key` (fallback when `api_token` is unset) |
| `email` | string | account email; paired with `api_key`, sent as `X-Auth-Email` |
| `account_id` | string | default account id for account-scoped verbs (`worker_deploy`) |
| `zone_id` | string | default zone id for zone-scoped verbs (`dns_*`, `cache_purge`, `zone_get`, …) |
| `api_base` | string | override the API base URL (tests only; default `https://api.cloudflare.com/client/v4`) |

`api_token` wins when both an API token and the legacy key/email pair are
configured. Every zone-scoped verb accepts a `zone` option that falls back to
`connection.zone_id`, and `worker_deploy` accepts `account` falling back to
`connection.account_id` — pin one zone/account per connector instance and
omit it on every call, or override per-call.

## Response handling

Every verb calls the standard Cloudflare v4 envelope
(`{success, errors, result}`). A non-2xx HTTP response, or a 2xx response with
`success: false`, becomes an error naming the Cloudflare error(s) (`code` +
`message`) and the raw response body. Otherwise the envelope's `result` is
returned as `outputs.result`, alongside `outputs.status_code`.

## Verbs

Selected by `uses: <name>.<verb>`; see `Describe()` for each verb's full
option schema.

**DNS records**

| verb | purpose |
|------|---------|
| `dns_list` | list DNS records in a zone (`type`, `name` filters) |
| `dns_create` | create a DNS record (`type`, `name`, `content` required; `ttl`, `proxied`, `priority`) |
| `dns_update` | update a DNS record by `record_id` |
| `dns_delete` | delete a DNS record by `record_id` |

**Cache**

| verb | purpose |
|------|---------|
| `cache_purge` | purge the zone's cache: `everything: true`, or `files`/`tags`/`hosts` |

**Zones**

| verb | purpose |
|------|---------|
| `zone_list` | list zones on the account (`name`, `status` filters) |
| `zone_get` | read a zone by id |

**Workers**

| verb | purpose |
|------|---------|
| `worker_deploy` | deploy (create or update) a Workers script; `main_module` set → ES module worker, unset → classic service-worker script |

**Rules**

| verb | purpose |
|------|---------|
| `ruleset_list` | list rulesets configured on a zone |
| `firewall_rules_list` | list legacy firewall rules configured on a zone |

**Escape hatch**

| verb | purpose |
|------|---------|
| `api` | call any Cloudflare v4 endpoint: `method`, `path` (under `/client/v4`), `query`, `body` |

```yaml
steps:
  - uses: cf.dns_create
    options: { type: A, name: app.example.com, content: "203.0.113.10", proxied: true }
  - uses: cf.cache_purge
    options: { everything: true }
```

## Capabilities & security

Declares egress to `api.cloudflare.com:443` only. Narrow it further per
instance with `network:`; it can never be widened past the declaration.
Provide the least-privileged API token (scoped to the zones/accounts you
actually act on) rather than the legacy Global API Key where possible.
