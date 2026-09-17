# `portainer` connector

Portainer (container management) as a connector: environments (endpoints),
stacks, and Docker containers/images proxied through Portainer's
docker-proxy, over the Portainer CE REST API, plus a raw `api` escape hatch.
Built on the standard library's `net/http` only.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/portainer/main.go`](../../connectors/portainer/main.go)
- **Provides:** `portainer`
- **Capabilities:** egress `[]` (self-hosted — narrow with `network:` to your
  own instance)

```yaml
connectors:
  pt:
    use: portainer
    base_url: https://portainer.example.com
    api_key: ${PORTAINER_API_KEY}
    insecure_skip_verify: true   # common with a self-signed Portainer UI cert
    network: ["portainer.example.com:443"]   # narrow the declared egress to your instance

steps:
  - id: containers
    uses: pt.containers
    with: { endpoint_id: "1", all: true }
  - id: restart_web
    uses: pt.container_action
    with: { endpoint_id: "1", container_id: "{{.web_container_id}}", action: restart }
```

## Setup

Produces a Portainer access token the connector sends as `X-API-Key`.

**Prerequisites:** a running Portainer instance and admin (or sufficiently
privileged) access to it.

1. Log into the Portainer UI and click your user icon (top right) > **My
   account**.
2. Open the **Access tokens** tab and click **Add access token**.
3. Give it a description and an optional expiry, then click **Add access
   token**.
4. Copy the generated token — it's shown once.

```yaml
connectors:
  pt:
    use: portainer
    base_url: https://portainer.example.com
    api_key: ${PORTAINER_API_KEY}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `base_url` | string | Portainer server root, e.g. `https://portainer.example.com` (no trailing `/api`) |
| `api_key` | string | Portainer API key, sent as `X-API-Key: <api_key>` |
| `insecure_skip_verify` | boolean | skip TLS certificate verification (default `false`) |

Every verb calls `base_url + "/api" + <endpoint>`. A non-2xx response is
returned as an error carrying the status code and response body — nothing is
swallowed.

### `insecure_skip_verify`: read this before you set it

Self-signed certificates are the norm on a home-lab Portainer box, so
`insecure_skip_verify: true` is provided to unblock that case. Setting it
disables TLS certificate verification for **that connector instance** — the
connection is no longer protected against a man-in-the-middle on the network
path to your Portainer server. Prefer trusting the box's real CA certificate
(or its Let's Encrypt cert, if it's behind one) at the OS/daemon level
instead, when that is practical. Where it is not, this flag exists so a
self-signed lab box doesn't force you to disable TLS globally.

## Verbs

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; most also return
either `result` (a single object), `items` (a list), or — for
`container_logs` — `logs` (a raw string).

| verb | endpoint | outputs |
|------|----------|---------|
| `status` | `GET /api/status` | `result` |
| `endpoints` | `GET /api/endpoints` | `items` |
| `endpoint_get` | `GET /api/endpoints/{endpoint_id}` | `result` |
| `stacks` | `GET /api/stacks` | `items` |
| `stack_get` | `GET /api/stacks/{stack_id}` | `result` |
| `stack_start` | `POST /api/stacks/{stack_id}/start` | `status_code` (+ `result` if the body is non-empty) |
| `stack_stop` | `POST /api/stacks/{stack_id}/stop` | `status_code` (+ `result` if the body is non-empty) |
| `stack_delete` | `DELETE /api/stacks/{stack_id}` (`?endpointId=`) | `status_code` (+ `result` if the body is non-empty) |
| `containers` | `GET /api/endpoints/{endpoint_id}/docker/containers/json` (`?all=1`) | `items` |
| `container_action` | `POST /api/endpoints/{endpoint_id}/docker/containers/{container_id}/{action}` (`action`: start\|stop\|restart\|kill\|pause\|unpause) | `status_code` (+ `result` if the body is non-empty) |
| `container_logs` | `GET /api/endpoints/{endpoint_id}/docker/containers/{container_id}/logs` (`?stdout=1&stderr=&tail=`) | `logs` (raw string) |
| `images` | `GET /api/endpoints/{endpoint_id}/docker/images/json` | `items` |
| `api` | `method` + `path` (under `/api`) + `query` + `body` — escape hatch for anything without a first-class verb | `result` (object response) or `items` (array response) |

`stack_delete`'s `endpoint_id` option maps to the `endpointId` query
parameter that Portainer requires for most stack types (Swarm/Compose
stacks tied to a specific environment).

`container_logs` defaults to `stdout: true` when `stdout` is omitted; set
`stdout: false` explicitly to fetch only `stderr`. The docker-proxy log
endpoint returns plain text (or, for containers created without a TTY, an
8-byte-framed multiplexed stdout/stderr stream) rather than JSON, so the
response body is returned verbatim as the `logs` string rather than
JSON-decoded — a workflow that needs stdout/stderr demultiplexed will need to
parse that framing itself.

## Capabilities & security

Declares empty egress — Portainer is self-hosted, so there is no fixed
public host to declare. Set `network: ["<your-host>:443"]` on the connector
instance to narrow it to your own server. Provide an API key scoped to the
least privilege the workflow needs (Portainer supports per-user/per-team
access control on environments and stacks).
