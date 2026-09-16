# `kubernetes` connector

Drive a Kubernetes cluster by shelling out to the `kubectl` CLI. The manifest
lifecycle is exposed as verbs (`apply`, `delete`, `get`, `describe`, `logs`,
`exec`, `rollout`, `scale`, `patch`, `create`, `label`, `annotate`, `wait`,
`top`, `cordon`, `uncordon`, `drain`, `cp`), plus a `cli` escape hatch for any
subcommand a first-class verb does not cover.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/kubernetes/main.go`](../../connectors/kubernetes/main.go)
- **Provides:** `kubernetes`
- **Capabilities:** spawns `kubectl`; no declared egress (talks to whatever
  cluster the resolved kubeconfig/context points at)

```yaml
connectors:
  k: { use: kubernetes, context: prod, namespace: web }
triggers:
  - on: gh.release
    steps:
      - id: rollout
        uses: k.rollout
        options: { subcommand: restart, resource: deployment/web }
```

## Connection

Every field is optional. Credentials/targets are read per-invocation from the
connector instance.

| key | type | purpose |
|-----|------|---------|
| `kubeconfig` | string | `--kubeconfig` path |
| `context` | string | `--context <name>` |
| `namespace` | string | `-n` default namespace, applied when a verb doesn't set its own |
| `binary` | string | override the CLI binary path (default `kubectl`) |
| `env` | map | default process environment for every invocation |
| `timeout` | duration | default per-verb timeout (default `10m`); a verb's `timeout` option overrides it |

Connection-level global flags (`--kubeconfig`, `--context`, `-n`) always
precede the subcommand:

```yaml
connectors:
  prod: { use: kubernetes, kubeconfig: /etc/kube/prod.yaml, context: prod-cluster, namespace: web }
# → kubectl --kubeconfig /etc/kube/prod.yaml --context prod-cluster -n web <subcommand>...
```

A verb's own `namespace` option (where present) overrides the connection
default for that call only.

## Common output shape

Every verb returns the process result; a **non-zero exit is data, not an
error** — inspect `exit_code`.

| output | type | notes |
|--------|------|-------|
| `stdout` | string | captured stdout |
| `stderr` | string | captured stderr |
| `exit_code` | integer | process exit code (0 = success) |

`get` and `apply` add a structured **`result`** output, populated only on
`exit_code == 0` when the effective output format is `json`.

## Verbs

### `apply` — apply one or more manifests

| option | type | flag |
|--------|------|------|
| `filename` | list | `-f` each (files, dirs, or URLs) |
| `manifest` | string | inline YAML/JSON, piped to stdin with `-f -` |
| `recursive` | boolean | `-R` |
| `prune` | boolean | `--prune` |
| `server_side` | boolean | `--server-side` |
| `force` | boolean | `--force` |
| `namespace` | string | `-n` override for this call |
| `output` | string | `json`/`yaml`; `json` is parsed into `result` |

One of `filename` or `manifest` is required.

```yaml
uses: k.apply
options:
  manifest: |
    apiVersion: v1
    kind: ConfigMap
    metadata: { name: cfg }
  server_side: true
```

### `delete` — delete resources

`resource` + `name`, or `filename` (`-f`), `selector` (`-l`), `all` (`--all`),
`grace_period` (`--grace-period`), `force`, `ignore_not_found`. Either
`resource` or `filename` is required.

### `get` — list/read resources

`resource` * (e.g. `pods`), `name`, `selector` (`-l`), `all_namespaces` (`-A`),
`field_selector` (`--field-selector`), `output` (`json`/`yaml`/`wide`/`name`,
default `json`). With the default/explicit `json` output, the parsed object is
exposed as the **`result`** output.

```yaml
uses: k.get
options: { resource: pods, selector: "app=web" }
# result: <parsed JSON of the list/object kubectl printed>
```

### `describe` — human-readable details

`resource` *, `name`, `selector` (`-l`).

### `logs` — fetch a pod's logs

`pod` *, `container` (`-c`), `tail` (`--tail`), `since` (`--since`), `previous`
(`-p`), `all_containers` (`--all-containers`).

### `exec` — run a command in a running pod

`pod` *, `container` (`-c`), `command` * (argv after `--`), `stdin` (`-i`),
`tty` (`-t`).

### `rollout` — manage a rollout

`subcommand` * (`status`/`restart`/`undo`/`pause`/`resume`/`history`),
`resource` * (e.g. `deployment/web`).

```yaml
uses: k.rollout
options: { subcommand: status, resource: deployment/web }
```

### `scale` — scale a resource's replica count

`resource` *, `replicas` * (`--replicas`).

### `patch` — patch a resource

`resource` *, `name` *, `patch` * (the patch body), `type` (`--type`:
`strategic`/`merge`/`json`).

### `create` — create a resource

`filename` (`-f`) or `manifest` (stdin), or a raw pass-through via
`extra_args` for `kubectl create <args>`. One of the three is required.

```yaml
uses: k.create
options: { extra_args: [configmap, cfg, "--from-literal=k=v"] }
```

### `label` / `annotate`

`resource` *, `name` *, `pairs` * (map → `key=value` positional args, keys
sorted), `overwrite` (`--overwrite`).

### `wait` — wait on a condition

`resource` *, `name`/`selector`, `for` * (`--for`, e.g. `condition=Ready`),
`timeout` (`--timeout`).

### `top` — resource usage

`subcommand` * (`pods`/`nodes`), `name`, `selector` (`-l`).

### `cordon` / `uncordon` / `drain`

- `cordon` / `uncordon`: `node` *.
- `drain`: `node` *, `ignore_daemonsets` (`--ignore-daemonsets`),
  `delete_emptydir_data` (`--delete-emptydir-data`), `force` (`--force`).

### `cp` — copy files to/from a pod

`src` *, `dst` * (`kubectl cp src dst`), `container` (`-c`).

### `cli` — any kubectl subcommand

`args` * (raw argv appended after the binary). The escape hatch for anything
the first-class verbs don't model.

```yaml
uses: k.cli
options: { args: [api-resources] }
```

## Capabilities & security

Declares `Commands: [kubectl]` and `Spawns: true`; no `Egress`.

- The cluster reached is whatever `kubeconfig`/`context` resolve to — scope the
  connector instance (and the trigger) to the namespace(s) it should touch.
- Verb options become CLI argv **directly** (`exec.Command`, no shell), so
  there is no shell-quoting/injection surface in how options are assembled.
  The commands you let `kubectl` run are still the cluster's full power under
  whatever RBAC the kubeconfig grants — scope accordingly.
