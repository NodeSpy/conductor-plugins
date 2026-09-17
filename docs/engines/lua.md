# `lua` engine

Lua 5.1 code steps on [gopher-lua](https://github.com/yuin/gopher-lua) — a
pure-Go Lua VM, no cgo. Use it for `code:` steps that want small, fast,
table-oriented scripts.

- **Kind:** step engine (`use: lua` on a `steps:` entry — an engine has no
  top-level block of its own; `run:` is the same key for configs written
  before the rename)
- **Provides:** `lua`
- **Sandbox:** only the `base`, `table`, `string` and `math` libraries are
  opened (no `os`, `io`, `debug`, `package`), and the file/chunk loaders
  (`dofile`, `loadfile`, `load`, `loadstring`) are removed on top of that

## Use it

```yaml
steps:
  - id: greet
    use: lua
    code: |
      local name = ctx.name or "world"
      ctx.store("cache").set("greetings", name, "hello")
      return { greeting = "Hello, " .. name .. "!" }
```

## Contract

`ctx` is the step's inputs, converted into a Lua table (the `ctx` global) —
objects become tables with string keys, arrays become 1-based tables, and
scalars come across as themselves.

`code:` is the script body; the script's `return` value becomes the step's
outputs:

- a table with string keys — becomes the step's named outputs, verbatim
- an array-like table (1..n integer keys, no string keys) — becomes the
  step's single `value` output, as a list
- anything else (a number, a string, a boolean, no return at all) —
  either lands under `value:`, or (no return) the step gets no outputs

## Sandbox

The opened library set IS the sandbox: `base`, `table`, `string` and `math`
only, so there is no `os` (no env vars, no process control), no `io` (no
filesystem), no `debug`, and no `package` (no external module loading). The
base library's own loaders — `dofile`, `loadfile`, `load`, `loadstring` — are
then deleted, so a script can't reach the filesystem or assemble a new chunk
from a string at runtime. There is no heap cap (gopher-lua has none); the
step's `timeout:` is the available limit, enforced through context
cancellation checked in the VM's exec loop.

`ctx.store`, `ctx.sql` and `ctx.memory` keep their in-binary spelling and
reach conductor's data plane one host round trip per call, raising a Lua
error on failure:

- `ctx.store(name)` — a defined KV store: `get`, `set`, `setnx`, `merge`,
  `delete`, `incr`, `append`, `remove`, `contains`, `list`, `first`, `last`,
  `index`, `slice`, `len`, `pop`
- `ctx.sql(name)` — a defined SQL store: `query`, `exec`
- `ctx.memory` — the shared agent memory: `remember`, `recall`, `forget`,
  `list`

Every op is authorized by conductor against the step's own data-plane guard,
same as an in-binary `run: lua` step — a script written for one runs
unchanged on the other.

## Examples

Reading a nested field and returning a list:

```yaml
steps:
  - id: emails
    use: lua
    code: |
      local out = {}
      for _, u in ipairs(ctx.payload.users or {}) do
        if u.email then table.insert(out, u.email) end
      end
      return out
```

Querying a SQL store and shaping named outputs:

```yaml
steps:
  - id: recent
    use: lua
    code: |
      local rows = ctx.sql("analytics").query(
        "SELECT id, title FROM incidents WHERE severity = ?", { "high" })
      return { count = #rows, rows = rows }
```
