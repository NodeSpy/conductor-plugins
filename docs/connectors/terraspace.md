# `terraspace` connector

Drive [Terraspace](https://terraspace.cloud) — which wraps Terraform/OpenTofu
with a per-**stack** command model — by shelling out to the `terraspace` CLI.
The standard workflow is exposed as verbs (`up`, `down`, `plan`, `all_up`,
`all_down`, `all_plan`, `output`, `logs`, `list`, `new`, `import`, `console`,
`state`, `build`, `clean`, `fmt`, `validate`, `test`, `info`), plus a `cli`
escape hatch for any subcommand a first-class verb does not cover.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/terraspace/main.go`](../../connectors/terraspace/main.go)
- **Provides:** `terraspace`
- **Capabilities:** spawns `terraspace`, `terraform`, `tofu` (Terraspace shells
  to Terraform/OpenTofu underneath); no declared egress (talks to whatever
  backend/provider the configuration points at)

```yaml
connectors:
  ts: { use: terraspace, dir: infra, ts_env: prod }
triggers:
  - on: gh.push
    steps:
      - id: plan
        uses: ts.plan
        options: { stack: vpc, out: vpc.tfplan }
      - id: up
        uses: ts.up
        options: { stack: vpc }
```

## Setup

Installs Ruby + the Terraspace gem, plus the Terraform/OpenTofu engine it drives underneath.

**Prerequisites:** `terraspace` on PATH (override with `binary`), plus `terraform` or `tofu` on PATH (Terraspace shells to whichever the project's config selects).

1. Install Ruby (2.7+) via your distro/`rbenv`/`asdf`, then `gem install terraspace` — see [Terraspace's install docs](https://terraspace.cloud/docs/install/).
2. Install Terraform or OpenTofu the normal way (see the `terraform` connector's Setup).
3. Configure provider credentials exactly as you would for bare Terraform — an AWS profile/SSO, `GOOGLE_APPLICATION_CREDENTIALS`, etc. — then pass them through `env`, since Terraspace's own environment is what reaches the underlying engine.
4. Select the target environment with `ts_env` (sets `TS_ENV`); confirm with `terraspace info` run by hand from the project root (`dir`).

```yaml
connectors:
  ts: { use: terraspace, dir: infra, ts_env: prod, env: { AWS_PROFILE: prod-deploy } }
```

## Connection

Every field is optional. Credentials are read from the process environment
(`AWS_*`, provider-specific vars) — pass them through `env` if the
connector's own environment is otherwise clean.

| key | type | purpose |
|-----|------|---------|
| `binary` | string | override the CLI binary path (default `terraspace`) |
| `dir` | string | working directory for the spawned process — a Terraspace project root; set as `cmd.Dir`, not a CLI flag |
| `ts_env` | string | sets `TS_ENV` for every invocation — selects the Terraspace environment |
| `env` | map | extra process environment for every invocation |
| `timeout` | duration | default per-verb timeout (default `10m`); a verb's `timeout` option overrides it |

### `TS_ENV` placement

Terraspace selects its target environment through the `TS_ENV` **process
environment variable** — there is no `--env` flag. The connector sets it by
exporting `TS_ENV=<ts_env>` into the spawned process's environment (alongside
the parent's own environment and anything in `env`); it never appears in
argv.

```yaml
connectors:
  ts: { use: terraspace, dir: infra, ts_env: staging }
```

### `dir` vs Terraspace's own `-c`/chdir

Terraspace has no global `-chdir`-style flag. Instead, `dir` is applied as the
spawned process's **working directory** (`cmd.Dir`) — every verb, including
`cli`, runs with that directory as the process's cwd, exactly as if you had
`cd`'d there before running `terraspace`.

## Common output shape

Every verb returns the process result; a **non-zero exit is data, not an
error** — inspect `exit_code`.

| output | type | notes |
|--------|------|-------|
| `stdout` | string | captured stdout |
| `stderr` | string | captured stderr |
| `exit_code` | integer | process exit code (0 = success) |

## Verbs

### `up` — provision a stack

`yes` defaults to **true** (non-interactive `-y`); `extra_args` are appended
last.

| option | type | notes |
|--------|------|-------|
| `stack` * | string | the stack to provision |
| `yes` | boolean | `-y` (default `true`) |
| `extra_args` | list | raw flags, appended after `-y` |

```yaml
uses: ts.up
options: { stack: vpc }
# → terraspace up vpc -y
```

### `down` — tear down a stack

Same shape as `up`: `stack` * , `yes` (default `true` → `-y`), `extra_args`
(appended after `-y`).

### `plan` — compute an execution plan for a stack

| option | type | notes |
|--------|------|-------|
| `stack` * | string | the stack to plan |
| `out` | string | `--out <path>` — save the plan |
| `extra_args` | list | raw flags, appended after `--out` |

```yaml
uses: ts.plan
options: { stack: vpc, out: vpc.tfplan }
# → terraspace plan vpc --out vpc.tfplan
```

### `all_up` — provision every stack

`yes` defaults to **true** (`-y`); `extra_args` appended last.

```yaml
uses: ts.all_up
# → terraspace all up -y
```

### `all_down` — tear down every stack

Same shape as `all_up`: `yes` (default `true` → `-y`), `extra_args`.

### `all_plan` — compute an execution plan for every stack

`extra_args` (list, appended last).

```yaml
uses: ts.all_plan
# → terraspace all plan
```

### `output` — read a stack's output value(s)

| option | type | notes |
|--------|------|-------|
| `stack` * | string | the stack to read from |
| `name` | string | a single output name (positional, optional) |

### `logs` — fetch a stack's (or every stack's) logs

| option | type | notes |
|--------|------|-------|
| `stack` | string | optional; omit for every stack's logs |
| `follow` | boolean | `-f` |

```yaml
uses: ts.logs
options: { stack: vpc, follow: true }
# → terraspace logs vpc -f
```

### `list` — list all stacks

No options.

### `new` — scaffold a new module, project, or stack

| option | type | notes |
|--------|------|-------|
| `subcommand` * | string | e.g. `module`, `project`, `stack` |
| `name` * | string | the new resource's name |
| `extra_args` | list | raw flags, appended last |

```yaml
uses: ts.new
options: { subcommand: stack, name: vpc }
# → terraspace new stack vpc
```

### `import` — import an existing resource into a stack's state

`stack` * , `address` * (positional), `id` * (positional).

```yaml
uses: ts.import
options: { stack: vpc, address: aws_vpc.this, id: vpc-0123456789 }
# → terraspace import vpc aws_vpc.this vpc-0123456789
```

### `console` — open an interactive console for a stack

`stack` * .

### `state` — advanced state management for a stack

`stack` * , `args` (list, appended as trailing positional arguments).

```yaml
uses: ts.state
options: { stack: vpc, args: [show, aws_vpc.this] }
# → terraspace state vpc show aws_vpc.this
```

### `build` — build the configuration for a stack (or all stacks) without applying

`stack` (optional; omit to build every stack).

### `clean` — remove generated build artifacts

`target` (enum `cache`/`all`/`logs`, optional).

### `fmt` — rewrite configuration files to canonical style

No options.

### `validate` — validate a stack's configuration

`stack` * .

### `test` — run the project's test suite

No options.

### `info` — print project/environment info

No options.

### `cli` — any terraspace subcommand

`args` * (raw argv). The escape hatch for anything the first-class verbs
don't model.

```yaml
uses: ts.cli
options: { args: [doctor] }
# → terraspace doctor
```

## Capabilities & security

Declares `Commands: [terraspace, terraform, tofu]` and `Spawns: true`; no
`Egress`.

- Verb options become CLI argv **directly** (`exec.Command`, no shell), so
  there is no shell-quoting/injection surface in how options are assembled.
  The configuration and provider credentials Terraspace hands to
  Terraform/OpenTofu still carry the provider's full power — scope the
  connector (and the trigger) accordingly.
- `up`/`down`/`all_up`/`all_down` default `yes` to `true` (`-y`) because these
  connectors run non-interactively; gate access to those verbs at the trigger
  level if that default is undesirable for a given workflow.
- `TS_ENV` (via `ts_env`) determines which environment's configuration and
  state Terraspace operates against — treat it as security-sensitive
  configuration, not a cosmetic label.
