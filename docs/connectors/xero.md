# `xero` connector

Xero as a connector: organisation details, invoices, contacts, accounts,
payments, bank transactions and inventory items over the Xero Accounting API
(`https://api.xero.com/api.xro/2.0`), plus a raw `api` escape hatch. Built on
the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/xero/main.go`](../../connectors/xero/main.go)
- **Provides:** `xero`
- **Capabilities:** egress `["api.xero.com:443"]`

## Managed OAuth2 — this plugin never talks to identity.xero.com

`xero` authenticates via conductor's **managed OAuth2**, not its own token
exchange. `Describe().Auth` bakes in Xero's OAuth2 endpoints; the daemon runs
the actual authorization-code / refresh flow against `identity.xero.com` and
injects a fresh, auto-rotated bearer token into every verb call. The plugin's
only egress is `api.xero.com` — it never sees a client secret and never makes
a token-exchange request itself.

```yaml
connectors:
  xero:
    use: xero
    auth:
      grant: authorization_code
      client_id: ${XERO_CLIENT_ID}
      client_secret: ${XERO_CLIENT_SECRET}
      token_vault: xero   # where the daemon persists the rotated token
```

After the connector is configured, run the one-time login:

```sh
conductor connector auth xero
```

If no token has been configured/logged in yet, every verb fails fast with a
`CodeInvalidParams` error pointing back at this command, rather than sending
an unauthenticated request.

## Setup

Managed OAuth2: register an app once at developer.xero.com, then run
`conductor connector auth xero` for the one-time browser login — conductor
stores and refreshes the token from there.

**Prerequisites:** a Xero developer account (developer.xero.com) with access
to the organisation(s) you want to connect.

1. [developer.xero.com/app/manage](https://developer.xero.com/app/manage) →
   **New app**.
2. Choose integration type **Web app**, give it a name, and set the company
   and privacy policy URLs (any reachable URL works for internal use).
3. Set **Redirect URI** to conductor's local callback:
   `http://localhost:8400/callback` (override with the `auth:` block's
   `redirect_uri` if the daemon uses a different port).
4. Create the app, then open its **Configuration** tab and copy the
   **Client ID**; click **Generate a secret** and copy the **Client secret**
   (shown once).
5. Put them in the `auth:` block below, then run `conductor connector auth
   xero` — the consent screen lets you pick which organisation(s) to
   authorize; `tenant_id` (or the first `GET /connections` result) selects
   among them.

```yaml
connectors:
  xero:
    use: xero
    auth:
      grant: authorization_code
      client_id: ${XERO_CLIENT_ID}
      client_secret: ${XERO_CLIENT_SECRET}
      token_vault: xero
```

This connector's baked-in scopes are `accounting.transactions`,
`accounting.contacts`, `accounting.settings`, and `offline_access` (the last
is required for refresh tokens) — no scope configuration is needed on the
app itself; Xero apps request scopes at authorization time, not at
registration time.

## Connection

| key | type | purpose |
|-----|------|---------|
| `tenant_id` | string | Xero organisation (tenant) id, sent as `Xero-tenant-id`. Optional — if omitted, the connector calls `GET /connections` and uses the first tenant, caching the result for the process. |
| `api_base` | string | Overrides `https://api.xero.com` (tests only). |

The `auth:` block (grant, client credentials, token vault) is daemon-managed
configuration, not a connection field this plugin ever sees — it is not part
of `Describe().Connection`.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb's outputs include `status_code`. Collection verbs return `items`
  (hoisted from the Accounting API's PascalCase envelope, e.g.
  `{"Invoices": [...]}`); single-resource verbs return `result` (the
  envelope's first element).
- All verbs except `connections` resolve a Xero **tenant** first: the
  connection's `tenant_id` if set, otherwise the first tenant from
  `GET /connections`, cached per process thereafter. Every request sends
  `Authorization: Bearer <injected token>`, `Accept: application/json`, and
  (except for `connections`) `Xero-tenant-id: <tenant>`.
- A non-2xx response is returned as an error carrying the status code and
  response body — nothing is swallowed.

Required options are marked `*`.

### Organisation & connections

- **`connections`** — list the tenants (organisations) this token is authorized for. `GET https://api.xero.com/connections` (Bearer only, no tenant header). No options. → `items`, `status_code`.
- **`organisation`** — get the connected organisation's details. `GET /Organisation`. No options. → `result`, `status_code`.

### Invoices

- **`invoices`** — list invoices. `GET /Invoices`. `where` (Xero where-clause filter expression), `order` (sort expression, e.g. `InvoiceNumber DESC`), `page` (1-based page number), `statuses` (list, filter by `Status`, e.g. `["AUTHORISED", "PAID"]`; sent as the comma-joined `Statuses` query parameter). → `items` (hoisted from `Invoices`), `status_code`.
- **`invoice_get`** — get one invoice. `GET /Invoices/{id}`. `invoice_id`*. → `result`, `status_code`.

### Contacts

- **`contacts`** — list contacts. `GET /Contacts`. No options. → `items` (hoisted from `Contacts`), `status_code`.
- **`contact_get`** — get one contact. `GET /Contacts/{id}`. `contact_id`*. → `result`, `status_code`.

### Accounts, payments, bank transactions & items

- **`accounts`** — list chart-of-accounts accounts. `GET /Accounts`. No options. → `items` (hoisted from `Accounts`), `status_code`.
- **`payments`** — list payments. `GET /Payments`. No options. → `items` (hoisted from `Payments`), `status_code`.
- **`bank_transactions`** — list bank transactions. `GET /BankTransactions`. No options. → `items` (hoisted from `BankTransactions`), `status_code`.
- **`items`** — list inventory items. `GET /Items`. No options. → `items` (hoisted from `Items`), `status_code`.

### Escape hatch

- **`api`** — raw escape hatch: any Xero Accounting API endpoint (enables writes). `method` (HTTP method, default `GET`), `path`* (path under `/api.xro/2.0`, e.g. `/Invoices`), `query` (map of query string parameters), `body` (any, JSON request body). → `result` (object response) or `items` (array response), `status_code`.

## Capabilities & security

Declares egress `api.xero.com:443` only — the daemon, not this plugin, talks
to `identity.xero.com` for the OAuth2 exchange. Scope the `auth:` block's
grant and the requested scopes to the least privilege the workflow needs;
`offline_access` is required for refresh tokens to keep working without
repeated interactive logins.
