# `aws-cli` connector

Drive the AWS CLI by shelling out to it. AWS's surface is unbounded, so the
design is **generic-first + first-class conveniences**: `run` covers any
`aws <service> <operation>` pair, while `s3`, `lambda_invoke`, and
`sts_identity` give ergonomic shapes to the operations most triggers actually
need, plus a `cli` escape hatch for anything else.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/aws-cli/main.go`](../../connectors/aws-cli/main.go)
- **Provides:** `aws-cli`
- **Capabilities:** spawns `aws`; no declared egress (AWS API calls happen
  inside the CLI subprocess, not this connector)

```yaml
connectors:
  a: { use: aws-cli, profile: prod, region: us-east-1 }
triggers:
  - on: gh.release
    steps:
      - id: identity
        uses: a.sts_identity
      - id: sync
        uses: a.s3
        options: { subcommand: sync, src: ./dist, dst: "s3://my-bucket/release", delete: true }
```

## Connection

Every field is optional. **Credentials are never carried by this connector** —
`profile`/`region`/`env` only select which AMBIENT AWS credentials to use (the
standard aws CLI credential chain: env vars, `~/.aws/credentials`, an instance
role, an SSO cache, …). Put access keys in `env` only if you must; prefer a
profile or the instance's own role.

| key | type | purpose |
|-----|------|---------|
| `profile` | string | `--profile` (selects an ambient credential profile) |
| `region` | string | `--region` |
| `output` | string | `--output` (default `json`); ignored by `s3` and `cli` |
| `endpoint_url` | string | `--endpoint-url` (e.g. LocalStack or a VPC endpoint) |
| `binary` | string | override the CLI binary path (default `aws`) |
| `env` | map | default process environment for every invocation, e.g. `AWS_PROFILE`/`AWS_ACCESS_KEY_ID` |
| `timeout` | duration | default per-verb timeout (default `10m`); a verb's `timeout` option overrides it |

```yaml
connectors:
  prod:      { use: aws-cli, profile: prod-deploy, region: us-east-1 }
  localstack: { use: aws-cli, endpoint_url: "http://localhost:4566", env: { AWS_ACCESS_KEY_ID: test, AWS_SECRET_ACCESS_KEY: test } }
```

## Common output shape

Every verb returns the process result; a **non-zero exit is data, not an
error** — inspect `exit_code`.

| output | type | notes |
|--------|------|-------|
| `stdout` | string | captured stdout |
| `stderr` | string | captured stderr |
| `exit_code` | integer | process exit code (0 = success) |

Some verbs add structured outputs (below), populated only on `exit_code == 0`
so an error stream is never mistaken for JSON.

## Verbs

### `run` — the generic verb: `aws <service> <operation> [params…]`

The one verb that covers ANY AWS CLI operation, for when no first-class verb
below fits.

| option | type | flag |
|--------|------|------|
| `service` * | string | e.g. `s3api`, `ec2`, `dynamodb` (positional) |
| `operation` * | string | e.g. `list-buckets`, `describe-instances` (positional) |
| `params` | map | for each key: `--<key> <value>`; `true` emits a bare `--<key>`; `false` is skipped; a list value repeats `--<key>` per element |
| `args` | list | raw trailing argv, appended after `params` |

Always appends `--output json`. Extra output **`result`** is the parsed JSON
stdout.

```yaml
uses: a.run
options:
  service: s3api
  operation: list-objects-v2
  params: { bucket: my-bucket, max-items: 50 }
# → aws s3api list-objects-v2 --bucket my-bucket --max-items 50 --output json
```

### `s3` — `aws s3 <subcommand>`

The high-level S3 CLI (`aws s3`, not `s3api`) for object/bucket operations.
Does **not** append `--output` — the `s3` CLI has no such flag.

| option | type | flag |
|--------|------|------|
| `subcommand` * | string | `cp` / `sync` / `mv` / `rm` / `ls` / `mb` / `rb` |
| `src` | string | source path or `s3://` uri (positional) |
| `dst` | string | destination path or `s3://` uri (positional, cp/sync/mv) |
| `recursive` | boolean | `--recursive` |
| `delete` | boolean | `--delete` (sync) |
| `exclude` | list | `--exclude` each |
| `include` | list | `--include` each |
| `dryrun` | boolean | `--dryrun` |
| `extra_args` | list | raw flags inserted before `src`/`dst` |

```yaml
uses: a.s3
options: { subcommand: sync, src: ./dist, dst: "s3://bucket/site", delete: true, exclude: ["*.map"] }
```

### `lambda_invoke` — invoke a function and capture its response

| option | type | flag |
|--------|------|------|
| `function` * | string | `--function-name` |
| `payload` | any | `--payload`: a JSON string, or an object marshaled to one |
| `invocation_type` | string | `--invocation-type` (`RequestResponse`/`Event`/`DryRun`) |
| `log_type` | string | `--log-type` (`None`/`Tail`) |
| `qualifier` | string | `--qualifier` (version or alias) |

The AWS CLI writes an invocation's response BODY to an **outfile positional
argument** rather than stdout; this connector passes `/dev/stdout` as that
outfile so the body still comes back on the captured stdout stream:

```
aws lambda invoke --function-name myFn --payload '{"k":"v"}' --output json /dev/stdout
```

Extra output **`response`** is the parsed body.

### `sts_identity` — `aws sts get-caller-identity`

No options. Extra output **`identity`** (the parsed `Account`/`Arn`/`UserId`
document) — useful to confirm which credentials a connector instance actually
resolves to before running anything destructive.

### `cli` — any aws subcommand

`args` * (raw argv appended after the binary and connection flags). The
escape hatch for anything the first-class verbs don't model.

```yaml
uses: a.cli
options: { args: [configure, list] }
```

## Capabilities & security

Declares `Commands: [aws]` and `Spawns: true`; no `Egress`.

- **This connector never holds credentials.** `profile`/`region`/`env` only
  select which ambient credential source the `aws` CLI resolves — long-lived
  keys, if used at all, live in the instance's own environment/profile, not in
  connector config.
- Verb options become CLI argv **directly** (`exec.Command`, no shell), so
  there is no shell-quoting/injection surface in how options are assembled.
  The API calls those credentials are allowed to make are still whatever IAM
  grants the resolved identity — scope the connector (and the trigger)
  accordingly.
