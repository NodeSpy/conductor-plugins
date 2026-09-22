# `sqs` connector

Amazon SQS as a connector: a **source** that long-polls a queue and emits one
event per message (deleting it after hand-off by default), plus `send_message` /
`receive_message` / `delete_message` / `get_queue_attributes` verbs.

**No AWS SDK, no AWS CLI.** Every request is a plain HTTPS POST to the SQS JSON
endpoint, signed with hand-rolled AWS SigV4 ([`internal/awskit`](../../internal/awskit/sigv4.go),
verified against AWS's documented test vector).

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/sqs/main.go`](../../connectors/sqs/main.go)
- **Provides:** `sqs`
- **Capabilities:** no fixed egress — the endpoint is region-derived; narrow it with `network:`.

```yaml
connectors:
  jobs:
    use: sqs
    region: us-east-1
    queue_url: https://sqs.us-east-1.amazonaws.com/123456789012/jobs
    network: ["sqs.us-east-1.amazonaws.com:443"]
```

## Setup

**Prerequisites:** an AWS account with an SQS queue, and credentials that can
call it.

1. Create the queue (SQS console / IaC) and copy its **Queue URL**
   (`https://sqs.<region>.amazonaws.com/<account>/<name>`).
2. Get credentials for an IAM principal allowed `sqs:ReceiveMessage`,
   `sqs:DeleteMessage`, `sqs:SendMessage`, `sqs:GetQueueAttributes` on that
   queue. Provide them as `access_key_id` / `secret_access_key` (+
   `session_token` for temporary/role creds) **or** via the standard
   `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`
   environment variables.
3. Set `region` and `queue_url`.

> Credentials come from config or the `AWS_*` env vars. The EC2 instance
> metadata service (IMDS) and SSO are **not** consulted — export the
> credentials, or use static keys, for those environments.

**Configure:**

```yaml
connectors:
  jobs:
    use: sqs
    region: us-east-1
    queue_url: https://sqs.us-east-1.amazonaws.com/123456789012/jobs
    access_key_id: ${AWS_ACCESS_KEY_ID}
    secret_access_key: ${AWS_SECRET_ACCESS_KEY}
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `region` | string (required) | AWS region, e.g. `us-east-1` |
| `queue_url` | string | default queue URL (**required for the source**; a per-verb `queue_url` overrides it) |
| `access_key_id` | string | AWS access key id (or `AWS_ACCESS_KEY_ID`) |
| `secret_access_key` | string | AWS secret access key (or `AWS_SECRET_ACCESS_KEY`) |
| `session_token` | string | session token for temporary/role credentials (or `AWS_SESSION_TOKEN`) |
| `endpoint` | string | override the endpoint (LocalStack, a VPC endpoint, tests) |
| `wait_time` | integer | source long-poll seconds, 0..20 (default 20) |
| `max_messages` | integer | source messages per receive, 1..10 (default 10) |
| `visibility_timeout` | integer | seconds a received message stays hidden |
| `auto_delete` | boolean | delete a message after the source emits it (default `true`) |

## Source events

Trigger with `on: <name>.<event>`. One event:

| event | fires when | context fields (filter / template) |
|-------|-----------|-------------------------------------|
| `message` | a message was received from the queue | `message_id`, `body`, `receipt_handle`, `queue_url` |

The source **long-polls** (`wait_time`), emits each message, and — when
`auto_delete` is true (the default) — deletes it after a successful hand-off. A
message the daemon couldn't accept is left on the queue and redelivered after
its visibility timeout. Set `auto_delete: false` to ack manually with the
`delete_message` verb after your workflow finishes (using `receipt_handle`).

`body` is text — parse it in a step (e.g. the `jq` engine); SQS bodies carry no
filter keys of their own beyond the fields above.

```yaml
triggers:
  - on: jobs.message
    steps:
      - use: jq
        code: '{id: (.body | fromjson | .id)}'
```

## Verbs

Every verb targets the connection's `queue_url` unless a per-call `queue_url`
overrides it.

- **`send_message`** — send a message. `queue_url`, `body`*, `delay_seconds` (0..900), `group_id` (FIFO `MessageGroupId`), `dedup_id` (FIFO `MessageDeduplicationId`). → `message_id`, `md5`.
- **`receive_message`** — receive up to N messages (one-shot; the source is the streaming path). `queue_url`, `max_messages` (1..10, default 1), `wait_time` (0..20, default 0), `visibility_timeout`. → `messages` (`[{message_id, body, receipt_handle, md5}]`).
- **`delete_message`** — delete a message by receipt handle (ack). `queue_url`, `receipt_handle`*. → `ok`.
- **`get_queue_attributes`** — read a queue's attributes. `queue_url`, `attributes` (names, default `["All"]`). → `attributes` (a map).

```yaml
steps:
  - uses: jobs.send_message
    options: { body: '{"task":"resize","id":"{{.id}}"}' }
```

## Capabilities & security

No fixed egress is declared (the endpoint is region-derived / operator-overridable)
— narrow it per instance with `network:` (e.g. `["sqs.us-east-1.amazonaws.com:443"]`);
it can never be widened past the declaration. Spawns nothing. Scope the IAM
principal to just the queue and actions this instance uses.
