# `js` engine

JavaScript code steps on QuickJS compiled to WASM and run under
[wazero](https://wazero.io) — pure Go, no cgo, no system QuickJS. Use it for
`code:` steps that want familiar JS syntax and JSON-shaped data manipulation
without shelling out to a real interpreter.

- **Kind:** step engine (`use: js` on a `steps:` entry — an engine has no
  top-level block of its own; `run:` is the same key for configs written
  before the rename)
- **Provides:** `js`
- **Sandbox:** no filesystem, no network from inside the VM — the only holes
  are three host bridges backing `ctx.store` / `ctx.sql` / `ctx.memory`, each
  one a request conductor answers or refuses

## Use it

```yaml
steps:
  - id: greet
    use: js
    code: |
      const name = ctx.name ?? "world";
      ctx.store("cache").set("greetings", name, "hello");
      return { greeting: `Hello, ${name}!` };
```

## Contract

`ctx` is the step's inputs: a plain JSON value (an object, in the common
case) round-tripped in as `globalThis.ctx` — no functions, no cycles, only
JSON-shaped data survives the trip.

`code:` is the BODY of an implicit IIFE, so a bare `return` works as you'd
expect: `return 5` yields `5`, and falling off the end with no `return`
yields `null` (not the string `"undefined"`).

The return value decides the step's outputs:

- no return / `null` — no outputs at all (`{}`)
- an object — its keys become the step's named outputs, verbatim
- anything else (a number, a string, an array) — becomes the step's single
  `value` output

## Sandbox

The VM is QuickJS-in-WASM: no filesystem and no network reachable from
inside it. The plugin's own capability manifest is empty — it dials nothing
and spawns nothing; everything it can reach, it reaches by asking conductor.

`ctx.store`, `ctx.sql` and `ctx.memory` are that asking: each call
serializes to JSON, crosses one host function, and comes back as `{v}` or
throws a JS `Error` built from `{err}`.

- `ctx.store(name)` — a defined KV store, with `get`, `set`, `setnx`,
  `merge`, `delete`, `incr`, `append`, `remove`, `contains`, `list`,
  `first`, `last`, `index`, `slice`, `len`, `pop`
- `ctx.sql(name)` — a defined SQL store, with `query` and `exec`
- `ctx.memory` — the shared agent memory, with `remember`, `recall`,
  `forget`, `list`

Every op is authorized by conductor against the step's own data-plane guard,
same as an in-binary `run: js` step — a snippet written for one runs
unchanged on the other.

## Examples

Reading a nested field and returning a list:

```yaml
steps:
  - id: emails
    use: js
    code: |
      const users = ctx.payload.users ?? [];
      return users.map(u => u.email).filter(Boolean);
```

Recalling shared memory and merging it into a KV entry:

```yaml
steps:
  - id: recap
    use: js
    code: |
      const facts = ctx.memory.recall({ tags: ["incident"], limit: 5 });
      const merged = ctx.store("cache").merge("incidents", ctx.incident_id, {
        recent_facts: facts.map(f => f.text),
      });
      return { merged };
```
