# `wasm` engine

The `wasm` engine runs an arbitrary, user-supplied WebAssembly module as a
step, sandboxed by wazero (pure Go, no cgo). Unlike this repo's other code
engines, it doesn't fix the language — `code:` is a precompiled module, so
it's the engine for logic already written (or best written) in a real
systems language: Rust, TinyGo, Zig, or anything else that targets
`wasm32-wasip1`, reused as a step without shelling out to a subprocess.

- **Kind:** step engine (`use: wasm` on the step)
- **Provides:** `wasm`
- **Sandbox:** a WASI preview 1 command module with nothing mounted — no `WithFS`, so no filesystem, and WASI preview 1 has no socket syscalls at all, so there is no network to withhold either. The only host surface is stdin/stdout/stderr/args/env.

Write `use: wasm`, not `run: wasm` — `run:` is a legacy alias kept only
for the four scripting engines conductor used to run in-process (`js`,
`lua`, `risor`, `go-embed`); for every other name, including this one,
`run:` falls back to looking for a program called `wasm` on the host's
`PATH` instead of selecting this engine.

## Use it

Compile your module for `wasm32-wasip1` (a WASI **command** module — one
with an exported `_start`, which `rustc`/TinyGo/Zig all produce by
default for a `fn main()`), then point `code:` at it — either as a **file
path** (the common case for a real module) or as a **base64-encoded**
binary inline:

```console
$ rustc --target wasm32-wasip1 -O -o transform.wasm transform.rs
```

```yaml
steps:
  - id: transform
    use: wasm
    args: ["--mode", "strict"]
    code: "file:/opt/conductor/modules/transform.wasm"   # a .wasm file on disk
```

`code:` accepts, in order: a `file:`-prefixed path; a bare path that exists on
disk; otherwise the base64 of the module (`base64 -w0 transform.wasm`), for
inlining a small module or shipping it in the config itself:

```yaml
    code: "AGFzbQEAAAABsAECYAJ/fwF/YAAAAwIBAQ==...<rest of the base64 module>..."
```

Reading the `.wasm` file is a **host-side load of the code to run** — it does
**not** give the guest filesystem access; the sandbox below is unchanged.

The module reads its JSON input from stdin and writes its JSON output to
stdout — for example, in Rust:

```rust
fn main() {
    let input: serde_json::Value =
        serde_json::from_reader(std::io::stdin()).unwrap();
    let output = serde_json::json!({ "doubled": input["n"].as_i64().unwrap() * 2 });
    serde_json::to_writer(std::io::stdout(), &output).unwrap();
}
```

## Contract

- `code:` is a **WASM module binary**, not source text — given as a `file:`
  path, a bare path that exists on disk, or base64. A read/decode failure, or
  bytes that don't compile as WASM, is a clear error.
- The step's inputs are `json.Marshal`'d onto the module's stdin (fd 0);
  the module parses that however its language does JSON.
- A step's `args:` become the module's `argv` (after a fixed `argv[0]`),
  and `env:` becomes its environment — the same two halves `use: sh`/`use:
  cli` give a real subprocess, minus the subprocess.
- **Output rule:** the module's stdout is read back after it exits and
  parsed as JSON, then passed through the same output rule every engine
  uses — an object becomes named outputs, any other JSON value becomes
  `value:`. Blank or non-JSON stdout from a module that exited zero is
  **not** an error — it's just no outputs, the same as a step with only
  side effects.
- A module that exits **non-zero** *is* an error; its stdout, if any, is
  discarded rather than trusted.
- A step's `timeout:` is honored via wazero's `WithCloseOnContextDone`,
  which halts the module (and any call into it) once the deadline passes.

## Sandbox

This engine never calls `ModuleConfig.WithFS`/`WithFSConfig`, so the
module is never given a filesystem, and WASI preview 1 has no socket
syscalls at all — a module cannot reach this host's filesystem or network
no matter what it imports, because nothing here answers those imports.
Unlike the source-level engines, there is no `ctx.store`/`ctx.sql`/
`ctx.memory` injection point either: an arbitrary precompiled module has
no source for this repo to inject a binding into, so its entire data
plane is its stdin/stdout. The declared capability manifest is empty: no
egress, no filesystem, no spawned commands.

## Examples

A module that only has side effects and prints nothing produces no
outputs — no error, just `{}`:

```yaml
steps:
  - id: validate_only
    use: wasm
    code: "AGFzbQEAAAABBQFgAAAD...<base64 module that exits 0 with empty stdout>..."
```

Passing extra argv and environment through to the module:

```yaml
steps:
  - id: render
    use: wasm
    args: ["--format", "svg"]
    env:
      LOG_LEVEL: "warn"
    code: "AGFzbQEAAAABjAGAgMAA...<base64 TinyGo module>..."
```
