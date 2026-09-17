# `jq` engine

jq, on gojq (a pure-Go jq implementation — no cgo, no libjq), runs a jq
program directly over the step's inputs. It's the engine to reach for when
the job is "reshape or filter this JSON" — pulling a field out, mapping
over a list, building a smaller object from a bigger one — using jq's own
terse filter syntax instead of a general-purpose scripting language.

- **Kind:** step engine (`use: jq` on the step)
- **Provides:** `jq`
- **Sandbox:** gojq's compiled program touches only the value it is handed — no filesystem, no network, and this engine never enables gojq's optional `env`/`input`/`$ENV` builtins, so a jq program cannot reach this process's environment or its stdin (the RPC transport).

Write `use: jq`, **not `run: jq`** — `run:` is a legacy alias kept only
for the four scripting engines conductor used to run in-process (`js`,
`lua`, `risor`, `go-embed`); every other name, including this one, falls
back under `run:` to its historic behavior of looking for a program on
the host's `PATH`. Since `jq` is also the name of the real command-line
tool, `run: jq` would silently shell out to whatever system `jq` binary
happens to be installed instead of running this engine — `use: jq` is the
only spelling that reliably selects it.

## Use it

```yaml
steps:
  - id: shape
    use: jq
    code: '{names: [.items[] | select(.active) | .name], count: (.items | length)}'
```

Note there is no `ctx.` prefix inside the filter — the step's inputs
**are** the jq input document, so a top-level field is just `.field`.

## Contract

- The step's inputs are the jq program's input document (`.` at the top
  level) — not a variable named `ctx`.
- `code:` is a jq program (`.items | map(.name)`, `{count: (.items |
  length)}`, `.a.b[0]`, …), parsed and compiled once per run.
- **Output rule** — a jq filter is a generator, so one input can yield
  zero, one, or many results:
  - **zero results** (jq's `empty`, or a `select` that never matches) →
    no outputs at all.
  - **exactly one result** → the shared output rule applies to it: an
    object becomes named outputs; a scalar or array becomes `value:`.
  - **more than one result** → all of them collect into a `value:` list —
    jq's own way of saying "many things came out of one input".
- A runtime failure inside the program (a type mismatch, `error(...)`, an
  out-of-range index) surfaces as a step error, not a silent empty result.
- A step's `timeout:` is honored (`RunWithContext`), so a runaway program
  (an infinite `repeat`, say) is cut instead of wedging the run.

## Sandbox

No filesystem, no network, no process — jq has no such builtins to begin
with, and this engine additionally never enables gojq's optional
`env`/`input`/`$ENV` builtins, so even reading this plugin process's own
environment is unavailable to a program. There is no `ctx.store`/`ctx.sql`/
`ctx.memory`: a jq step that needs conductor's data plane reaches it
through a neighboring step, since jq has no host-callback affordance to
bolt one onto. The declared capability manifest is empty.

## Examples

Many results collecting into a `value:` list — one entry per matching item:

```yaml
steps:
  - id: hostnames
    use: jq
    code: '.servers[] | select(.status == "up") | .hostname'
```

No results at all when nothing matches — the step produces no outputs:

```yaml
steps:
  - id: alert_if_over
    use: jq
    code: 'select(.usage_pct > 90) | {usage: .usage_pct}'
```
