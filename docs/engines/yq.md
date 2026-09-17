# `yq` engine

yq, on [mikefarah/yq](https://github.com/mikefarah/yq)'s `yqlib` (pure Go, no
cgo), runs a yq expression over the step's inputs as a YAML document. It's the
YAML counterpart to the [`jq`](jq.md) engine — reach for it when the job is
"reshape, query, or edit YAML," especially when you want to **produce YAML
text** back out (a Kubernetes manifest, a `docker-compose.yml`, a CI config)
with comments, anchors, and key order preserved.

- **Kind:** step engine (`use: yq` on the step)
- **Provides:** `yq`
- **Sandbox:** yq's file/env/shell operators (`load`, `load_str`, `env`, `strenv`, `system`) are all disabled, so an expression cannot read the host filesystem, this process's environment, or run a command. Empty capability manifest — no filesystem, network, or spawns.

Write `use: yq`, **not `run: yq`** — `run:` is a legacy alias kept only for the
four scripting engines conductor used to run in-process (`js`, `lua`, `risor`,
`go-embed`); every other name, including this one, falls back under `run:` to
looking for a program on the host's `PATH`. Since `yq` is also the name of the
real command-line tool, `run: yq` would silently shell out to whatever system
`yq` binary is installed instead of running this engine — `use: yq` is the only
spelling that reliably selects it.

## Use it

```yaml
steps:
  - id: shape
    use: yq
    code: '{"names": [.items[] | select(.active) | .name], "count": (.items | length)}'
```

The step's inputs **are** the YAML document, so a top-level field is just
`.field` — there is no `ctx.` prefix inside the expression.

## Contract

- The step's inputs are the yq input document (`.` at the top level), fed in as
  YAML.
- `code:` is a yq expression (`.items | map(.name)`, `.metadata.labels.env = "prod"`,
  `(.. | select(tag == "!!str")) |= sub("foo", "bar")`, …).
- **Output rule** — a yq expression can match zero, one, or many nodes:
  - **zero results** → no outputs at all.
  - **exactly one result** → the shared output rule applies: a map becomes named
    outputs; a scalar or list becomes `value:`.
  - **more than one result** → all of them collect into a `value:` list.
- **Plus a `yaml` output.** Whenever the expression produces at least one result,
  the step also gets a `yaml` output: the result rendered back to YAML **text**
  exactly as the `yq` CLI would print it — comments, anchors, quoting style, and
  key order intact. This is yq's edge over jq: write the transformed document to
  a file, or hand it to another step, without losing formatting. (When the top
  result is itself a map, `yaml` sits alongside its named outputs; if a result
  key is literally `yaml`, the rendered text wins.)
- A parse or evaluation error surfaces as a step error, not a silent empty
  result. A step's `timeout:` is honored — a runaway expression is cut.

## Sandbox

yqlib exposes operators that reach outside the document — `load`/`load_str`
(read files), `env`/`strenv` (read environment), and `system` (run a command).
This engine turns all of them **off** (`DisableFileOps`, `DisableEnvOps`, and
`system` left at yqlib's disabled default), so an expression is confined to the
data it is handed. There is no `ctx.store`/`ctx.sql`/`ctx.memory`: a yq step that
needs conductor's data plane reaches it through a neighboring step. The declared
capability manifest is empty.

## Examples

Edit a value and keep the document as YAML text to write out — `yaml` carries the
result, comments and all:

```yaml
steps:
  - id: bump_replicas
    use: yq
    code: '.spec.replicas = 3'
    # → outputs.yaml is the full document re-rendered with replicas: 3
```

Pull many values into a `value:` list:

```yaml
steps:
  - id: images
    use: yq
    code: '.spec.template.spec.containers[].image'
```

Build a small object from a larger document (named outputs):

```yaml
steps:
  - id: summary
    use: yq
    code: '{"name": .metadata.name, "ns": .metadata.namespace}'
```
