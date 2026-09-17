# `risor` engine

Code steps on [risor](https://github.com/risor-io/risor) — a pure-Go
embeddable scripting language with Go-flavored syntax, no cgo. Use it for
`code:` steps that want a more expressive, typed-feeling script than Lua
while staying just as sandboxed.

- **Kind:** step engine (`use: risor` on a `steps:` entry — an engine has no
  top-level block of its own; `run:` is the same key for configs written
  before the rename)
- **Provides:** `risor`
- **Sandbox:** risor's default globals are dropped entirely
  (`WithoutDefaultGlobals`) and replaced with an explicit allowlist — no
  `os`, `exec`, `http`, `dns`, `net`, or `filepath`, which are all in risor's
  normal default set

## Use it

```yaml
steps:
  - id: greet
    use: risor
    code: |
      name := "world"
      if ctx["name"] != nil {
        name = ctx["name"]
      }
      store("cache").set("greetings", name, "hello")
      {"greeting": "Hello, " + name + "!"}
```

## Contract

`ctx` is the step's inputs, bound as the top-level `ctx` global — a risor
value built from the same JSON-shaped data every engine receives.

`code:` is the script body; its FINAL EXPRESSION (not a `return`) is the
step's result:

- a risor map — becomes the step's named outputs, verbatim
- anything else (a number, a string, a list, no result at all) — either
  lands under the step's single `value` output, or (nothing evaluated)
  the step gets no outputs

## Sandbox

risor ships with `os`, `exec`, `http`, `dns`, `net` and `filepath` in its
*default* global set — this engine opts out of all of it
(`risor.WithoutDefaultGlobals()`) and grants back only an explicit allowlist:
risor's core builtins (`len`, `keys`, `sprintf`, …) plus the data-shaping
modules `base64`, `bytes`, `errors`, `json`, `math`, `regexp`, `strconv`,
`strings` and `time`. Nothing that leaves the process is on the list.

`store`, `sql` and `memory` are top-level builtins (not fields on `ctx`) that
keep their in-binary spelling, reaching conductor's data plane one host
round trip per call:

- `store(name)` — a defined KV store: `get`, `set`, `setnx`, `merge`,
  `delete`, `incr`, `append`, `remove`, `contains`, `list`, `first`, `last`,
  `index`, `slice`, `len`, `pop`
- `sql(name)` — a defined SQL store: `query`, `exec`
- `memory` — the shared agent memory module: `remember`, `recall`,
  `forget`, `list`

Every op is authorized by conductor against the step's own data-plane guard,
same as an in-binary `run: risor` step — a script written for one runs
unchanged on the other.

## Examples

Reading a nested field and returning a list:

```yaml
steps:
  - id: emails
    use: risor
    code: |
      users := ctx["payload"]["users"]
      users.map(func(u) { u["email"] })
```

Recording a fact in memory and returning named outputs:

```yaml
steps:
  - id: recap
    use: risor
    code: |
      memory.remember(ctx["summary"], ["incident"], "repo:" + ctx["repo"])
      rows := sql("analytics").query(
        "SELECT id FROM incidents WHERE severity = $1", ["high"])
      {"recorded": true, "open_high_severity": len(rows)}
```
