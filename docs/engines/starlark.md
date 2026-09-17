# `starlark` engine

Starlark (go.starlark.net) is Bazel's configuration language: a small,
deterministic, Python-like dialect with no recursion beyond what the
language itself allows and no standard-library face onto the filesystem,
network, or process. It is a pure-Go, hermetic code step for logic that
should produce exactly the same output on every run — computed fields,
validation, reshaping — where "deterministic and boring" is a feature.

- **Kind:** step engine (`use: starlark` on the step)
- **Provides:** `starlark`
- **Sandbox:** the predeclared environment is only `ctx` and `json` (encode/decode/indent) — Starlark's own spec has no `import os`, no `open`, no `exec`, and this engine adds no `time` module either, so there is nothing nondeterministic or I/O-capable to reach for.

Write `use: starlark`, not `run: starlark` — `run:` is a legacy alias kept
only for the four scripting engines conductor used to run in-process
(`js`, `lua`, `risor`, `go-embed`); for every other name, including this
one, `run:` falls back to its historic behavior of looking for a program
called `starlark` on the host's `PATH` instead of selecting this engine.

## Use it

```yaml
steps:
  - id: shape
    use: starlark
    code: |
      active = [i for i in ctx["items"] if i["active"]]
      output = {
          "count": len(active),
          "names": [i["name"] for i in active],
      }
```

## Contract

- `ctx` is the step's inputs, converted to a Starlark dict and predeclared
  as a global — `ctx["level"]`, `ctx["items"]`, etc.
- `code:` is a full Starlark **module** (statements, `if`/`for`, variable
  assignment), executed with `starlark.ExecFile` — not a single expression.
- **Output rule:** whatever the module's `output` global holds after it
  runs, if it set one, is the step's return value: a dict becomes the
  step's named outputs; anything else lands under `value:`. A script that
  never assigns `output` produces no outputs at all (not an error).
- A step's `timeout:` is honored: a goroutine watching the run's context
  calls `thread.Cancel` once it's done, so a runaway `for`/`while` loop is
  cut cleanly instead of wedging the run.

## Sandbox

The predeclared globals are exactly `ctx` and `json` — nothing else. There
is no `ctx.store`, `ctx.sql`, or `ctx.memory` here (those are only wired
into the in-process engines whose source this repo's other plugins
control); a starlark step's entire data plane is the `ctx` it was given.
The declared capability manifest is empty: no egress, no filesystem, no
spawned commands. Determinism is enforced by omission too — there is no
`time` module, so a script has no wall clock to read and can't make two
runs of itself disagree.

## Examples

Validate an input and report a decision as a scalar:

```yaml
steps:
  - id: check
    use: starlark
    code: |
      errors = []
      if ctx["age"] < 0:
          errors.append("age must be >= 0")
      if len(ctx["name"]) == 0:
          errors.append("name is required")
      output = "ok" if len(errors) == 0 else "invalid: " + ", ".join(errors)
```

Reshape a list of records using `json` for a nested field that already
arrived as a string:

```yaml
steps:
  - id: normalize
    use: starlark
    code: |
      extra = json.decode(ctx["raw_extra"])
      output = {
          "id": ctx["id"],
          "tags": sorted(extra.get("tags", [])),
      }
```
