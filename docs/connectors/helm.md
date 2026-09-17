# `helm` connector

Drive Helm 3 releases against a Kubernetes cluster by shelling out to the
`helm` CLI. The release lifecycle is exposed as verbs (`install`, `upgrade`,
`uninstall`, `rollback`, `list`, `status`, `history`, `get_values`,
`template`, `pull`, `repo_add`, `repo_update`, `test`, `lint`), plus a `cli`
escape hatch for any subcommand a first-class verb does not cover.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/helm/main.go`](../../connectors/helm/main.go)
- **Provides:** `helm`
- **Capabilities:** spawns `helm`; no declared egress (talks to the cluster
  configured in kubeconfig)

```yaml
connectors:
  h: { use: helm, namespace: web }
triggers:
  - on: gh.release
    steps:
      - id: deploy
        uses: h.upgrade
        options: { name: web, chart: ./charts/web, install: true, wait: true }
```

## Setup

Installs `helm`, which shares kubeconfig/context with the `kubernetes` connector.

**Prerequisites:** `helm` (v3) on PATH; override with `binary`.

1. Install Helm — see [Installing Helm](https://helm.sh/docs/intro/install/) (`brew install helm`, or the install script).
2. Auth is inherited from the same kubeconfig the `kubernetes` connector uses — no separate Helm login. Set it up the same way (a cloud CLI's `get-credentials`, or a copied kubeconfig file); see the `kubernetes` connector's Setup section.
3. For a private chart repository, add and authenticate it once: `helm repo add <name> <url> --username <user> --password <pass>` (or use the `repo_add` verb's `username`/`password` options per-call).
4. Confirm with `helm --kube-context <ctx> list -A`.

```yaml
connectors:
  h: { use: helm, kube_context: prod, namespace: web }
```

## Connection

Every field is optional. Credentials/targets are read per-invocation from the
connector instance.

| key | type | purpose |
|-----|------|---------|
| `kubeconfig` | string | `--kubeconfig` path |
| `kube_context` | string | `--kube-context` name |
| `namespace` | string | `-n` namespace |
| `binary` | string | override the CLI binary path (default `helm`) |
| `env` | map | default process environment for every invocation |
| `timeout` | duration | default per-verb timeout (default `10m`); a verb's `timeout` option overrides it |

```yaml
connectors:
  local: { use: helm }                                               # default kubeconfig
  prod:  { use: helm, kube_context: prod, namespace: web }           # a named context, scoped namespace
  ci:    { use: helm, kubeconfig: /etc/ci/kubeconfig }               # explicit kubeconfig file
```

> **Note:** these connection flags precede the subcommand in the argv Helm
> sees, exactly as `helm --kubeconfig … --kube-context … -n … <verb> …`.

## Common output shape

Every verb returns the process result; a **non-zero exit is data, not an
error** — inspect `exit_code`.

| output | type | notes |
|--------|------|-------|
| `stdout` | string | captured stdout |
| `stderr` | string | captured stderr |
| `exit_code` | integer | process exit code (0 = success) |

Some verbs add structured outputs (below), populated only on `exit_code == 0`.

## Verbs

### `install` — install a chart as a new release

| option | type | flag |
|--------|------|------|
| `name` * | string | release name (positional) |
| `chart` * | string | chart reference (positional) |
| `version` | string | `--version` |
| `values` | list | `-f` each |
| `set` | map or list | `--set K=V` (map keys sorted, or raw `KEY=VALUE` list) |
| `set_string` | map or list | `--set-string K=V` (same shape as `set`) |
| `create_namespace` | boolean | `--create-namespace` |
| `wait` | boolean | `--wait` |
| `atomic` | boolean | `--atomic` |
| `dry_run` | boolean | `--dry-run` |
| `timeout` | duration | `--timeout` (Helm's own wait timeout, distinct from the connector's process timeout) |
| `repo` | string | `--repo` chart repository URL |

```yaml
uses: h.install
options:
  name: web
  chart: bitnami/nginx
  version: "15.4.0"
  values: [values.yaml]
  set: { replicaCount: "3" }
  wait: true
  atomic: true
```

### `upgrade` — upgrade a release, installing it first if it doesn't exist

Same options as `install`, plus:

| option | type | flag |
|--------|------|------|
| `install` | boolean | `--install` |
| `reuse_values` | boolean | `--reuse-values` |
| `force` | boolean | `--force` |

### `uninstall` — uninstall a release

`name` *, `keep_history` (`--keep-history`), `wait`, `timeout`.

### `rollback` — roll back a release to a previous revision

`name` *, `revision` (positional int; omit for the previous revision),
`wait`, `timeout`.

### `list` — list releases

`all` (`-a` include uninstalled/failed), `all_namespaces` (`-A`), `filter`
(`--filter` regex). Always runs with `-o json`; extra output **`releases`**
is the parsed list of objects.

### `status` — show a release's status

`name` *, `revision` (`--revision`). Always runs with `-o json`; extra output
**`status`** is the parsed object.

### `history` — show a release's revision history

`name` *. Always runs with `-o json`; extra output **`history`** is the
parsed list.

### `get_values` — fetch a release's values

`name` *, `all` (`-a` include computed defaults). Always runs with
`-o json`; extra output **`values`** is the parsed object.

### `template` — render chart templates locally without installing

`name` *, `chart` *, `values` (list → `-f` each), `set` (map or list →
`--set`), `version`, `show_only` (list → `-s` each).

### `pull` — download a chart to the local filesystem

`chart` *, `version`, `destination` (`-d`), `untar` (bool), `repo`.

### `repo_add` — add a chart repository

`name` *, `url` *, `username`, `password`, `force_update` (`--force-update`).

```yaml
uses: h.repo_add
options: { name: bitnami, url: https://charts.bitnami.com/bitnami }
```

### `repo_update` — update the local chart repository cache

`names` (list, optional positional — omit to update every repository).

### `test` — run a release's Helm tests

`name` *, `timeout`.

### `lint` — lint a chart directory for issues

`chart` * (path), `values` (list → `-f` each), `strict` (bool).

### `cli` — any helm subcommand

`args` * (raw argv appended after the binary). The escape hatch for anything
the first-class verbs don't model.

```yaml
uses: h.cli
options: { args: [version, --short] }
```

## Capabilities & security

Declares `Commands: [helm]` and `Spawns: true`; no `Egress`.

- The cluster Helm reaches is whatever `kubeconfig`/`kube_context` resolve
  to — under OS isolation, that network access is not narrowed by a
  connector's `network:` unless the cluster is reached over a socket the
  isolation permits.
- Verb options become CLI argv **directly** (`exec.Command`, no shell), so
  there is no shell-quoting/injection surface in how options are assembled.
  The releases you let Helm touch are still Helm's full power — scope the
  connector (and the trigger) accordingly, especially `namespace` and
  `kube_context`.
