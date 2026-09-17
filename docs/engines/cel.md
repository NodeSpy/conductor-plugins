# `cel` engine

CEL (Google's Common Expression Language, on cel-go) evaluates one
expression against the step's inputs — no statements, no assignment, no
function definitions, no loops. It's the smallest of this repo's engines,
and deliberately so: use it for computed fields and boolean conditions
where a single expression is the whole job, not a script.

- **Kind:** step engine (`use: cel` on the step)
- **Provides:** `cel`
- **Sandbox:** CEL has no I/O primitive at all in the language — no file, socket, process, or even `print` — so there is nothing to strip and no `ctx.store`/`ctx.sql`/`ctx.memory` face to wire in; an expression can only read the `ctx` it was given.

Write `use: cel`, not `run: cel` — `run:` is a legacy alias kept only for
the four scripting engines conductor used to run in-process (`js`, `lua`,
`risor`, `go-embed`); for every other name, including this one, `run:`
falls back to looking for a program called `cel` on the host's `PATH`
instead of selecting this engine.

## Use it

```yaml
steps:
  - id: names
    use: cel
    code: "ctx.items.filter(x, x.active).map(x, x.name)"
```

A map-producing expression becomes named outputs directly:

```yaml
steps:
  - id: shape
    use: cel
    code: "{'severity': ctx.level, 'count': size(ctx.items)}"
```

## Contract

- `ctx` is bound as CEL's single variable, typed `map(string, dyn)` — the
  step's rendered inputs.
- `code:` is exactly **one CEL expression** (`ctx.a + ctx.b`,
  `ctx.items.filter(...)`, a map or list literal, …). CEL is deliberately
  not Turing-complete: every well-typed expression it accepts is
  guaranteed to terminate.
- **Output rule:** the expression's result becomes the step's outputs the
  same way every engine's does — a map result becomes named outputs;
  anything else (a list, a number, a string, a bool) lands under `value:`.
- A step's `timeout:` still applies even though CEL can't infinite-loop:
  evaluation runs through CEL's `ContextEval`, and a cost limit
  (1,000,000) is a second, cheaper backstop against a pathologically
  expensive expression (e.g. a huge list comprehension).

## Sandbox

There is no `ctx.store`, `ctx.sql`, or `ctx.memory` binding — CEL has no
notion of a host callback, and the language has no I/O surface to
sandbox in the first place. The declared capability manifest is empty:
no egress, no filesystem, no spawned commands.

## Examples

A boolean condition, useful directly on a step's `if:`:

```yaml
steps:
  - id: should_page
    use: cel
    code: "ctx.severity == 'critical' && !ctx.silenced"
```

Filtering and re-shaping a list of records into named outputs:

```yaml
steps:
  - id: summarize
    use: cel
    code: >
      {'critical_count': ctx.alerts.filter(a, a.severity == 'critical').size(),
       'hosts': ctx.alerts.map(a, a.host)}
```
