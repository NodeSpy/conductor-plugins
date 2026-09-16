# `terraform` connector

Drive a Terraform, OpenTofu, or Terragrunt workflow by shelling out to the
selected CLI. The standard workflow is exposed as verbs (`init`, `validate`,
`plan`, `apply`, `destroy`, `output`, `show`, `fmt`, `workspace`, `state`,
`import`, `refresh`, `providers`, `version`), plus a `cli` escape hatch for
any subcommand a first-class verb does not cover.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/terraform/main.go`](../../connectors/terraform/main.go)
- **Provides:** `terraform`
- **Capabilities:** spawns `terraform` (or `tofu`/`terragrunt`, depending on
  `engine`); no declared egress (talks to whatever backend/provider the
  configuration points at)

```yaml
connectors:
  tf: { use: terraform, chdir: infra/prod }
triggers:
  - on: gh.push
    steps:
      - id: plan
        uses: tf.plan
        options: { out: plan.tfplan }
      - id: apply
        uses: tf.apply
        options: { plan_file: plan.tfplan }
```

## Connection

Every field is optional. Credentials are read from the process environment
(`AWS_*`, provider-specific vars) — pass them through `env` if the connector's
own environment is otherwise clean.

| key | type | purpose |
|-----|------|---------|
| `engine` | string | which CLI to drive: `terraform` (default), `tofu`, or `terragrunt` |
| `chdir` | string | working directory — for `terraform`/`tofu`, emitted as the **global** `-chdir=<dir>` flag, which precedes the subcommand; for `terragrunt`, applied as the spawned process's working directory instead (see below) |
| `binary` | string | override the CLI binary path (default: the engine name — `terraform`, `tofu`, or `terragrunt`) |
| `env` | map | process environment for every invocation, e.g. `TF_VAR_*`, `AWS_*` |
| `timeout` | duration | default per-verb timeout (default `10m`); a verb's `timeout` option overrides it |

### Engines: Terraform, OpenTofu, Terragrunt

`engine` selects which CLI is spawned:

- **`terraform`** (default) — the standard `terraform` binary.
- **`tofu`** — OpenTofu, a 100% drop-in: identical argv to `terraform`,
  including `-chdir` placement, for every verb.
- **`terragrunt`** — Terragrunt understands every terraform/tofu subcommand,
  so the verbs behave the same way, but with two differences (below):
  `-chdir` and `run_all`.

```yaml
connectors:
  tf:   { use: terraform, chdir: infra/prod, binary: /usr/local/bin/terraform }
  tofu: { use: terraform, engine: tofu, chdir: infra/prod }
  tg:   { use: terraform, engine: terragrunt, chdir: infra/prod }
```

### `-chdir` placement

`-chdir` is a **global** terraform/OpenTofu flag — it must appear before the
subcommand (`terraform -chdir=DIR plan …`), unlike per-subcommand flags. For
`engine: terraform` and `engine: tofu`, the connector builds argv as
`[-chdir=<dir>] <subcommand> <subcommand flags…>`, so every verb — including
`cli` — runs against the configured directory.

**Terragrunt has no `-chdir` flag.** For `engine: terragrunt`, the connector
never emits `-chdir`; instead, when `chdir` is set, the terragrunt process is
spawned with that directory as its working directory (`cmd.Dir`), and argv is
just `<subcommand> <subcommand flags…>`.

### `run_all` (terragrunt only)

Terragrunt's multi-module orchestration is invoked via a `run-all` prefix
(`terragrunt run-all apply …`) rather than a flag. The `init`, `plan`,
`apply`, `destroy`, `refresh`, and `output` verbs accept a `run_all` boolean
option: when `true`, the subcommand is prefixed with `run-all`. It is a no-op
argv-wise for `terraform`/`tofu` connections (nothing stops you from setting
it, but only terragrunt gives it meaning). Anything terragrunt-specific
beyond that — `--terragrunt-*` flags on a verb without a matching option —
passes through the `cli` verb's `args` escape hatch.

```yaml
uses: tg.apply
options: { run_all: true }
# → terragrunt run-all apply -auto-approve -input=false   (cwd = chdir)
```

## Common output shape

Every verb returns the process result; a **non-zero exit is data, not an
error** — inspect `exit_code`. This matters most for `plan
detailed_exitcode: true` (2 = changes present) and `validate`/`plan -json`
failures — both are ordinary responses, not RPC errors.

| output | type | notes |
|--------|------|-------|
| `stdout` | string | captured stdout |
| `stderr` | string | captured stderr |
| `exit_code` | integer | process exit code (0 = success) |

`output` and `show` add a parsed structured output (below), populated only on
`exit_code == 0`.

## Verbs

### `init` — initialize a working directory

| option | type | flag |
|--------|------|------|
| `backend_config` | list | `-backend-config=<path-or-key=value>` each |
| `upgrade` | boolean | `-upgrade` |
| `reconfigure` | boolean | `-reconfigure` |
| `no_color` | boolean | `-no-color` |
| `run_all` | boolean | terragrunt only: prefix the subcommand with `run-all` |

### `validate` — validate the configuration

`json` (`-json`), `no_color` (`-no-color`).

### `plan` — compute an execution plan

Always runs with `-input=false` (non-interactive).

| option | type | flag |
|--------|------|------|
| `out` | string | `-out=<path>` |
| `var` | map | `-var 'k=v'` each (keys sorted) |
| `var_files` | list | `-var-file=<path>` each |
| `target` | list | `-target=<addr>` each |
| `destroy` | boolean | `-destroy` |
| `refresh_only` | boolean | `-refresh-only` |
| `detailed_exitcode` | boolean | `-detailed-exitcode` (0 no changes, 1 error, 2 changes present) |
| `json` | boolean | `-json` |
| `no_color` | boolean | `-no-color` |
| `run_all` | boolean | terragrunt only: prefix the subcommand with `run-all` |

```yaml
uses: tf.plan
options:
  out: plan.tfplan
  var: { region: us-east-1 }
  var_files: [prod.tfvars]
  detailed_exitcode: true
# → terraform [-chdir=…] plan -out=plan.tfplan -var region=us-east-1 -var-file=prod.tfvars -detailed-exitcode -input=false
```

### `apply` — apply a plan or the current configuration

`auto_approve` defaults to **true**; always runs with `-input=false`.

| option | type | flag |
|--------|------|------|
| `plan_file` | string | a saved plan file (positional, optional) |
| `auto_approve` | boolean | `-auto-approve` (default `true`) |
| `var` | map | `-var 'k=v'` each (keys sorted) |
| `var_files` | list | `-var-file=<path>` each |
| `target` | list | `-target=<addr>` each |
| `json` | boolean | `-json` |
| `no_color` | boolean | `-no-color` |
| `run_all` | boolean | terragrunt only: prefix the subcommand with `run-all` |

Set `auto_approve: false` to omit `-auto-approve` (terraform will then refuse
to run non-interactively unless a plan file with a matching approval is
supplied).

### `destroy` — destroy all managed resources

Same options as `apply` (minus `plan_file`): `auto_approve` (default `true`),
`var`, `var_files`, `target`, `json`, `no_color`, `run_all` (terragrunt only).
Always `-input=false`.

### `output` — read a state output (parsed into `outputs`)

| option | type | flag |
|--------|------|------|
| `name` | string | a single output name (positional, optional) |
| `json` | boolean | `-json` (default `true`) |
| `raw` | boolean | `-raw` — mutually exclusive with `json`; wins when both are set |
| `run_all` | boolean | terragrunt only: prefix the subcommand with `run-all` |

Extra output **`outputs`**: the parsed `-json` document, when `json` (and not
`raw`) is used and the command exits `0`.

### `show` — show a state or plan file (parsed into `result`)

`path` (positional, optional), `json` (default `true`). Extra output
**`result`**: the parsed `-json` document, when `json` is used.

### `fmt` — rewrite configuration to canonical style

`check` (`-check`), `diff` (`-diff`), `recursive` (`-recursive`), `write`
(boolean, default `true`; set `false` to add `-write=false`).

### `workspace` — manage workspaces

| option | type | notes |
|--------|------|-------|
| `subcommand` * | string | `list`, `select`, `new`, `delete`, `show` |
| `name` | string | positional; **required** for `select`/`new`/`delete` |

### `state` — advanced state management

`subcommand` * (`list`, `show`, `rm`, `mv`, `pull`, `push`), `args` (list,
appended as trailing positional arguments).

```yaml
uses: tf.state
options: { subcommand: show, args: [aws_instance.web] }
# → terraform [-chdir=…] state show aws_instance.web
```

### `import` — import an existing resource into state

`address` * (positional), `id` * (positional), `var` (map → `-var`),
`var_files` (list → `-var-file=`).

### `refresh` — reconcile state with real infrastructure

`var`, `var_files`, `target`, `no_color`, `run_all` (terragrunt only). Always
runs with `-input=false`.

### `providers` — print the provider dependency tree

No options.

### `version` — print terraform/provider versions

`json` (`-json`).

### `cli` — any terraform/tofu/terragrunt subcommand

`args` * (raw argv, appended after `-chdir` for terraform/tofu, or run as-is
in `chdir` for terragrunt). The escape hatch for anything the first-class
verbs don't model — including terragrunt-specific flags like
`--terragrunt-non-interactive`.

```yaml
uses: tf.cli
options: { args: [graph] }
# → terraform [-chdir=…] graph

uses: tg.cli
options: { args: [run-all, plan, --terragrunt-non-interactive] }
# → terragrunt run-all plan --terragrunt-non-interactive   (cwd = chdir)
```

## Capabilities & security

Declares `Commands: [terraform]` and `Spawns: true`; no `Egress`. (The
declared command name is `terraform` regardless of `engine` — connectors
declare capabilities statically, and `tofu`/`terragrunt` are the same class
of local process spawn.)

- Verb options become CLI argv **directly** (`exec.Command`, no shell), so
  there is no shell-quoting/injection surface in how options are assembled.
  The configuration and provider credentials you give terraform still carry
  the terraform provider's full power — scope the connector (and the
  trigger) accordingly.
- `apply`/`destroy` default to `-auto-approve` because these connectors run
  non-interactively; gate access to the `apply`/`destroy` verbs at the
  trigger level if that default is undesirable for a given workflow.
