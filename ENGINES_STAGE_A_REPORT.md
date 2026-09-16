# Engines — Stage A report

Porting conductor's four in-binary code-step interpreters to official ENGINE
PLUGINS in this repo. Stage A is **additive**: conductor still ships its
in-binary copies, and these four are the fetchable replacements it will point
at. Stage B (removing the in-binary copies) is not in this branch.

Branch: `feat/engines`. Nothing pushed, tagged, or PR'd.

Commit: **`de2588c04501de32a5dd28a747ddd0e71327fa33`** — *feat(engines): port
js, go-embed, risor and lua step engines to SDK plugins* (21 files, +3335/−41).
This report lands in the commit immediately after it, so that it can cite that
sha.

---

## 1. What landed

| Path | Binary | Engine | Interpreter |
|------|--------|--------|-------------|
| `engines/js/` | `conductor-js` | `js` | `github.com/fastschema/qjs` v0.0.6 — QuickJS compiled to WASM, run under wazero |
| `engines/go-embed/` | `conductor-go-embed` | `go-embed` | `github.com/traefik/yaegi` v0.16.1 |
| `engines/risor/` | `conductor-risor` | `risor` | `github.com/risor-io/risor` v1.8.1 |
| `engines/lua/` | `conductor-lua` | `lua` | `github.com/yuin/gopher-lua` v1.1.2 |

Supporting code:

| Path | What |
|------|------|
| `internal/enginekit/` | the shared ctx data plane (`KV`/`SQL`/`Memory`, the three op lists, `WrapValue`) — the out-of-process twin of conductor's `internal/code/{kvbind,sqlbind,membind}.go` dispatchers |
| `internal/enginekit/hosttest/` | the recording test double for the conductor side of the data plane |
| `internal/rpctest/` | extended: `BuildEngine`, `Client.Run` (`plugin.run`), and `Client.SetHost` — the client now ANSWERS the plugin→daemon `host.*` requests, which no connector ever issues |
| `e2e/engines_test.go` | all four driven over the real wire, with host callbacks answered mid-run |

Each engine declares `Kind: plugin.KindStep`, `ABI: plugin.EngineABI`, and an
**empty** `Capabilities{}` — no egress, no commands, no fs, no spawns. All four
are served with `plugin.Serve(plugin.EngineFunc(describe, run))`, modelled on
`conductor/test/plugins/acme-engine/main.go`.

---

## 2. Verification — real output

### gofmt

```console
$ gofmt -l .
connectors/twilio/main.go
connectors/twilio/main_test.go
```

Both are **pre-existing** (commit `c5cdb6a`, untouched by this branch). Every
file this branch adds or edits is clean:

```console
$ gofmt -l engines/ internal/enginekit/ internal/rpctest/ e2e/
(no output)
```

### build + vet + the internal-free gate

```console
$ CGO_ENABLED=0 go build ./engines/...
OK
$ go vet ./engines/...
OK
$ go list -deps ./... | grep 'NodeSpy/conductor/internal'
(nothing — clean)
```

### Per-engine `run()` tests against a fake Host

```console
$ go test ./engines/... ./internal/enginekit/... -count=1 -v
--- PASS: TestDescribe (0.00s)
--- PASS: TestRunInputsRoundTripAndOutputs (0.00s)
--- PASS: TestCtxStoreRoutesToHost (0.01s)
--- PASS: TestCtxSQLAndMemoryRouteToHost (0.00s)
--- PASS: TestAllowlistRejectsNonAllowedImports (0.02s)
--- PASS: TestAllowlistAdmitsDataShapingPackages (0.05s)
--- PASS: TestAllowlistDoesNotLeakNestedVariants (0.00s)
--- PASS: TestRunContractIsChecked (0.01s)
--- PASS: TestOutputContract (0.00s)
--- PASS: TestRunErrorFailsTheStep (0.00s)
ok  	github.com/NodeSpy/conductor-plugins/engines/go-embed	0.116s
--- PASS: TestDescribe (0.00s)
--- PASS: TestRunInputsRoundTripAndOutputs (0.80s)
--- PASS: TestCtxStoreRoutesToHost (0.01s)
--- PASS: TestCtxSQLAndMemoryRouteToHost (0.01s)
--- PASS: TestOutputContract (0.02s)
--- PASS: TestHostErrorThrowsInScript (0.01s)
--- PASS: TestNoAmbientHostAccess (0.01s)
ok  	github.com/NodeSpy/conductor-plugins/engines/js	0.850s
--- PASS: TestDescribe (0.00s)
--- PASS: TestRunInputsRoundTripAndOutputs (0.00s)
--- PASS: TestCtxStoreRoutesToHost (0.00s)
--- PASS: TestCtxSQLAndMemoryRouteToHost (0.00s)
--- PASS: TestOutputContract (0.00s)
--- PASS: TestSandboxWithholdsEscapeLibraries (0.00s)
--- PASS: TestHostErrorRaisesInScript (0.00s)
ok  	github.com/NodeSpy/conductor-plugins/engines/lua	0.006s
--- PASS: TestDescribe (0.00s)
--- PASS: TestRunInputsRoundTripAndOutputs (0.00s)
--- PASS: TestStoreRoutesToHost (0.00s)
--- PASS: TestSQLAndMemoryRouteToHost (0.00s)
--- PASS: TestOutputContract (0.00s)
--- PASS: TestSandboxWithholdsEscapeModules (0.00s)
--- PASS: TestHostErrorFailsTheStep (0.00s)
ok  	github.com/NodeSpy/conductor-plugins/engines/risor	0.007s
--- PASS: TestArgsCrossVerbatim (0.00s)
--- PASS: TestUnknownOpNeverLeaves (0.00s)
--- PASS: TestHostErrorPropagates (0.00s)
--- PASS: TestNoDataPlane (0.00s)
--- PASS: TestWrapValue (0.00s)
ok  	github.com/NodeSpy/conductor-plugins/internal/enginekit	0.004s
```

These cover, per engine: the interpreter **actually executing a snippet**;
inputs round-tripping into the engine's `ctx`; a `ctx.store` set+get routing to
the fake Host with the right kind/op/store/args; `ctx.sql` and `ctx.memory`
likewise; the output contract; the sandbox boundary; and error/refusal
propagation back into the script. `TestAllowlistRejectsNonAllowedImports`
rejects `os`, `os/exec`, `net/http`, `net`, `io`, `reflect`, `unsafe` and
`path/filepath`; its twin proves every allowlisted package still resolves, so
the sandbox is narrow rather than empty.

### Cross-build smoke — the real binaries, raw stdio

Cross-built exactly as `release.yml` does (`CGO_ENABLED=0 GOOS=linux
GOARCH=amd64 go build -trimpath -ldflags "-s -w"`), then driven by a small
Python harness over stdin/stdout: a `plugin.describe`, a `plugin.run`, and the
harness answering the `host.kv` requests the engine issues **while that run is
still in flight**.

```
--- conductor-js (cross-built binary, raw stdio) ---
  describe: {"protocol_version": 1, "kind": "engine", "abi": 1, "type": "js"} capabilities: {}
  host.kv  set(cache) args=["run", "attempts", 1]
  host.kv  get(cache) args=["run", "attempts"]
  run outputs: {"attempts": 1, "repo": "acme/app"}
--- conductor-go-embed (cross-built binary, raw stdio) ---
  describe: {"protocol_version": 1, "kind": "engine", "abi": 1, "type": "go-embed"} capabilities: {}
  host.kv  set(cache) args=["run", "attempts", 1]
  host.kv  get(cache) args=["run", "attempts"]
  run outputs: {"attempts": 1, "repo": "acme/app"}
--- conductor-risor (cross-built binary, raw stdio) ---
  describe: {"protocol_version": 1, "kind": "engine", "abi": 1, "type": "risor"} capabilities: {}
  host.kv  set(cache) args=["run", "attempts", 1]
  host.kv  get(cache) args=["run", "attempts"]
  run outputs: {"attempts": 1, "repo": "acme/app"}
--- conductor-lua (cross-built binary, raw stdio) ---
  describe: {"protocol_version": 1, "kind": "engine", "abi": 1, "type": "lua"} capabilities: {}
  host.kv  set(cache) args=["run", "attempts", 1]
  host.kv  get(cache) args=["run", "attempts"]
  run outputs: {"attempts": 1, "repo": "acme/app"}
```

Identical inputs, identical outputs, identical host traffic across four
languages — which is the point of one shared code-step ABI. Binary sizes:
`js` 6.6 MB, `go-embed` 19.8 MB, `risor` 10.4 MB, `lua` 3.3 MB.

The same path is asserted in-repo by `e2e/engines_test.go`, which builds and
spawns each binary and uses `rpctest`:

```console
$ go test ./e2e/ -run TestEngines -v
--- PASS: TestEnginesRunOverTheWire (9.44s)
    --- PASS: TestEnginesRunOverTheWire/js (3.90s)
    --- PASS: TestEnginesRunOverTheWire/go-embed (2.86s)
    --- PASS: TestEnginesRunOverTheWire/risor (1.82s)
    --- PASS: TestEnginesRunOverTheWire/lua (0.87s)
--- PASS: TestEnginesHaveNoVerbs (5.58s)
    --- PASS: TestEnginesHaveNoVerbs/js (1.14s)
    --- PASS: TestEnginesHaveNoVerbs/go-embed (2.15s)
    --- PASS: TestEnginesHaveNoVerbs/risor (1.64s)
    --- PASS: TestEnginesHaveNoVerbs/lua (0.65s)
ok  	github.com/NodeSpy/conductor-plugins/e2e	15.030s
```

It also asserts the `run_id` crossed on every host request (the per-run
capability token) and that `plugin.invoke` is refused — an engine has no verbs.

### Whole suite

```console
$ go test ./...
…
ok  	github.com/NodeSpy/conductor-plugins/e2e	22.449s
ok  	github.com/NodeSpy/conductor-plugins/engines/go-embed	0.433s
ok  	github.com/NodeSpy/conductor-plugins/engines/js	1.958s
ok  	github.com/NodeSpy/conductor-plugins/engines/lua	0.019s
ok  	github.com/NodeSpy/conductor-plugins/engines/risor	0.016s
ok  	github.com/NodeSpy/conductor-plugins/internal/enginekit	0.011s
ok  	github.com/NodeSpy/conductor-plugins/runtimes/paseo	0.111s
```

Green — no `FAIL` lines anywhere in the output, all 49 packages `ok` or
`[no test files]`.

---

## 3. How each engine bridges ctx to the SDK Host

### The shared mechanism (`internal/enginekit`)

All three faces funnel into one function that does exactly one thing: forward
the op to conductor and hand back what comes out.

```go
KV(ctx, host, store, op, args)     → host.Call(ctx, "kv",     op, store, args...)
SQL(ctx, host, store, op, args)    → host.Call(ctx, "sql",    op, store, args...)
Memory(ctx, host, op, args)        → host.Call(ctx, "memory", op, "",    args...)
```

**Enforcement stays host-side, and that is why this is thin.** Conductor
answers a `host.*` call by running `internal/code`'s `CtxHandler.Invoke`, which
calls the very same `kvInvoke`/`sqlInvoke`/`memInvoke` an in-process engine
calls, carrying the very same `Spec.DataGuard`. Arity checks, arg coercion,
absent-read-folds-to-nil, the per-store capability gates (`kv.CheckCapability`,
`CheckCodeAccess`), `memory.CheckOp`, and the plan write barrier all still
happen — once, on conductor's side. Re-implementing any of them in the plugin
would be a second enforcement point that can only drift.

**Args cross verbatim**, which is what makes that work. `HostRequest.Args`
follows the positional convention of the in-process bindings, and a snippet's
call site produces exactly that list. Consequences, all asserted in
`TestArgsCrossVerbatim`:

- `ctx.store("c").set("ns")` sends two args, so conductor answers with its own
  `kv.set: want 3 args, got 2` rather than a plugin-invented message.
- `pop(ns, key, "front")` keeps its third argument. The typed `HostKV.Pop` has
  no parameter for it.
- `incr(ns, key)` stays two-arg, so conductor applies its `by=1` default rather
  than the plugin inventing a `0`.
- `sql.query(sql, [binds])` carries the bind list as ONE positional argument.

Ops outside `KVOps`/`SQLOps`/`MemOps` never reach the wire (`kv: no operation
%q`, matching `kvInvoke`'s tail error). A run with no data plane gets a sentence
rather than a panic.

### Per engine

| Engine | ctx faces, as the snippet sees them | Mechanism |
|--------|--------------------------------------|-----------|
| **js** | `ctx.store(name)`, `ctx.sql(name)`, `ctx.memory` | The three JS shims are **byte-identical** to conductor's `jsKVShim`/`jsSQLShim`/`jsMemShim` (op lists built from the same slices, `r.v ?? null`, `throw new Error(r.err)`). Each calls one `__conductor_*` host function taking/returning a `{store, op, args}` → `{v}`/`{err}` JSON string, so values cross the WASM boundary as strings with no per-type Value plumbing. The three JSON bridges are this repo's `kvInvokeJSON`/`sqlInvokeJSON`/`memInvokeJSON`, same payload shape as conductor's, answering from `enginekit` instead of the daemon's stores. |
| **go-embed** | `import "conductor/store"` → `store.Use(name)` → `KVHandle`; `"conductor/sql"` → `SQLHandle`; `"conductor/memory"` → `Remember`/`Recall`/`Forget`/`List` | The `interp.Exports` keys (`conductor/store/store`, etc.), the handle types and their **full method sets** are conductor's. Each method body is the original with `kvInvoke(h.guard, …)` replaced by `enginekit.KV(h.ctx, h.host, …)`. |
| **risor** | top-level `store(name)`, `sql(name)` builtins returning a `NewBuiltinsModule` of the op set; top-level `memory` module | Conductor's `kvRisorStoreFn`/`sqlRisorFn`/`memRisorFn`, with the same `object.Interface()` arg conversion, `object.FromGoType` result conversion, `object.Nil` for nil, `object.NewError` for errors, and the same module names (`store:<name>`, `sql:<name>`, `memory`). |
| **lua** | `ctx.store(name)`, `ctx.sql(name)` returning op tables; `ctx.memory` table | Conductor's `luaStoreFn`/`luaSQLFn`/`luaMemFn`, with the same `luaToGo` args / `goToLua` results / `L.RaiseError` on failure — so a refusal is catchable with `pcall`, asserted in `TestHostErrorRaisesInScript`. The three op-table builders are factored into one `opTable` helper; the per-op behaviour is unchanged. |

---

## 4. Deviations from the brief, and why

These follow the **actual** `pkg/plugin/host.go` surface, per the brief's
instruction to do so and note it.

### 4a. The JS interpreter is QuickJS, not goja

The brief said `js.go` (goja). Conductor's `internal/code/js.go` does not use
goja — it uses `github.com/fastschema/qjs` v0.0.6 (QuickJS compiled to WASM,
run under wazero, no cgo), with a 256 MiB `MemoryLimit` and
`CloseOnContextDone`. I mirrored the actual source. Nothing in conductor's
`go.mod` references goja.

### 4b. The bridge calls `Host.Call`, not `Host.KV().Get(…)`

`plugin.Host`'s typed faces (`KV()`, `SQL()`, `Memory()`) are fixed-arity
wrappers that each build exactly one `Host.Call` with a fixed arg tuple —
**identical wire bytes**. The engines call `Host.Call` directly for two reasons:

1. **Fidelity.** A snippet's `(op, args…)` is dynamic. Routing it through a
   fixed-arity typed method would reshape it: pad a short `set` to three args,
   drop `pop`'s `from`, substitute a `0` for `incr`'s absent `by`, invent a
   `to` for a two-arg `slice`. Each of those turns an error conductor would
   have reported into a different op actually executing. See §3 and
   `TestArgsCrossVerbatim`.
2. **Testability.** `plugin.Host` is a concrete struct whose `calls *callTable`
   field is unexported, and `callTable` is unexported too — a working one can
   only be built by `Serve` with a live daemon on the other end. So no test
   double can implement "the SDK Host interface"; there isn't one. The engines
   take `enginekit.Host`, a one-method interface over `Call` that
   `*plugin.Host` satisfies (asserted at compile time by
   `var _ Host = (*plugin.Host)(nil)`), and `hosttest.Host` stands in behind it.

The typed client is still exercised end-to-end: the e2e test and the raw-stdio
smoke drive the real `*plugin.Host` through `Serve`, and the recorded
`host.kv set(cache) args=["run","attempts",1]` is the same request
`host.KV().Set(ctx, "cache", "run", "attempts", 1)` would produce.

### 4c. `ParseOutputs` is not used — `wrapValue` is

`ParseOutputs` is conductor's **stdout** contract: `grep` shows its only callers
are `cli.go`, `gorun.go` and `hostinterp.go` (the `use: cli` / `run: go` / host
interpreter paths), never `js.go`/`goembed.go`/`risor.go`/`lua.go`. The four
in-process engines return a typed Go value and go through `wrapValue`, which is
ported verbatim as `enginekit.WrapValue` and tested (`TestWrapValue`, plus a
`TestOutputContract` per engine). Adding `ParseOutputs`' two extra cases (blank
text, non-JSON text) would have been a behaviour change: a JS snippet whose
result fails to decode currently errors, and under `ParseOutputs` it would
silently become `{"text": …}`.

### 4d. `Use(name)` no longer validates the store eagerly

In-binary, `store.Use("nope")` / `store("nope")` / `ctx.store("nope")` resolved
the name through `kv.Use` / `sqlstore.Use` and failed at construction. A plugin
has no store registry, and the only way to ask would be to perform a real op —
which the step's guard may refuse, turning a name check into a policy event. So
the constructors always return a handle and an undefined store surfaces on the
**first op**, with conductor's own "no such store" message. The error text is
the daemon's either way; only its timing moved. Documented at the top of each
engine's `ctx.go`.

### 4e. go-embed's count-returning handles tolerate JSON numbers

`KVHandle.Incr`/`Len` in-binary did `r.(int64)` / `r.(int)` because the value
came straight out of the store as a Go int. Across JSON it arrives as
`float64`, so the bare assertion would panic on a perfectly good answer. Those
two now go through an `asInt64` helper accepting `int`/`int64`/`float64`. Same
for the other handles' `.(map[string]any)` / `.([]any)` assertions, which became
comma-ok — a malformed reply is a zero value, not a panic in an engine process.

---

## 5. go.mod / replace situation

**No `replace` directive is needed, and none was added.**
`go get github.com/NodeSpy/conductor@v0.11.0` resolved straight from the public
module proxy:

```console
$ go get github.com/NodeSpy/conductor@v0.11.0
go: downloading github.com/NodeSpy/conductor v0.11.0
go: upgraded github.com/NodeSpy/conductor v0.9.0 => v0.11.0
```

`go.mod` now requires:

```
github.com/NodeSpy/conductor v0.11.0     // the step-engine SDK
github.com/fastschema/qjs     v0.0.6     // js
github.com/risor-io/risor     v1.8.1     // risor
github.com/traefik/yaegi      v0.16.1    // go-embed
github.com/yuin/gopher-lua    v1.1.2     // lua
github.com/golang-jwt/jwt/v5  v5.3.1     // indirect
github.com/tetratelabs/wazero v1.9.0     // indirect (qjs's WASM runtime)
```

The four interpreter versions are the **same ones conductor pinned** while the
engines lived in its binary, so this is a move rather than an upgrade. All are
pure Go (qjs is QuickJS-as-WASM), so the zero-cgo cross-compiled release holds —
confirmed by the `CGO_ENABLED=0` cross-builds above.

**Nothing must change before release on this front.** The one thing to watch:
adding four interpreters makes `go.sum` and the dependency surface of this
module considerably larger for *every* plugin in it, including connectors that
don't want them. That is a repo-shape question (one module vs. a submodule per
kind), not a blocker, and it does not affect the shipped binaries — each is
built from its own package and links only what it imports.

---

## 6. Release wiring + README

### `.github/workflows/release.yml`

The workflow is tag-driven rather than a hardcoded matrix, so "adding the four
packages to the build/release list" means teaching it the new kind:

- Tag trigger gained `"engines/*/v*"` alongside `connectors/*/v*` and
  `runtimes/*/v*`.
- The kind `case` accepts `engines` and now also emits `noun` ("step engine")
  and `block` ("engines") outputs.
- The release notes stopped using a two-way ternary
  (`kind == 'runtimes' && 'runtime' || 'connector'`, which would have called an
  engine a connector) and use `noun`/`block` instead — so an engine release
  reads *"conductor js step engine plugin v1.0.0 … Install: add `engines: { js:
  { use: js } }`"*.

Everything else already generalized: `pkg="./${kind}/$comp"`, the release title,
the internal-free gate, `go test ./...`, and the five-platform cross-build loop
producing `conductor-<name>_<os>_<arch>` + `checksums.txt`. So `git tag
engines/js/v1.0.0 && git push --tags` publishes a fetchable `conductor-js`.

### `README.md`

- Intro: the kind list is now `connectors/`, `runtimes/` **or** `engines/`, with
  the `use: js` → `engines/js` resolution spelled out.
- The `go list -deps` block gained the four engine packages, plus a note that
  the interpreters are ordinary module deps while the conductor surface is still
  `pkg/plugin` alone.
- **Status**: `v0.9.0` → `v0.11.0`, naming the engine SDK symbols that release
  carries.
- **New "Step engines" section** under Available plugins: what a step engine is,
  the ask-don't-hold data-plane model (and why the manifest is empty), and a
  four-row table matching the existing table's columns.
- **Install**: an `engines: { js: { use: js } }` block plus a step showing
  `use: js` with a `code:` body.
- **Tests**: describes `e2e/engines_test.go` and the host-callback direction,
  and the per-engine `hosttest` doubles.
- **Releasing**: tag shape gained `engines/js/v1.0.0`; SDK version updated.

---

## 7. Per-interpreter fidelity notes — what I mirrored

Everything below was copied from conductor's `internal/code/` and kept as-is
unless listed in §4.

**js** (`internal/code/js.go`) — `jsMemoryLimit = 256 << 20`; the exact source
construction `globalThis.ctx = <JSON>` + the three shims +
`JSON.stringify((function(){ <code> })() ?? null)`; the `null`→`{}` empty-ctx
fallback so `ctx.store` still attaches; `qjs.Option{Context, CloseOnContextDone,
MemoryLimit}`; the recover-wrapped teardown (qjs panics on a ctx-halted module,
including on `Close`) and the ctx-error-vs-runtime-fault split inside it; the
`JSON.stringify`→`json.Unmarshal`→`wrapValue` result path including the
`decode result %q` error. The three shims are character-identical to
conductor's, including the ops arrays being `json.Marshal`'d from the shared op
lists. The `{v}`/`{err}` encoder mirrors `kvInvokeJSON`'s `enc`, down to the
`{"err":"kv: unencodable result"}` fallback.

**go-embed** (`internal/code/goembed.go`) — `goEmbedAllowlist` copied
**verbatim**, all 16 entries; `goEmbedExports`'s last-slash split (so
`math/rand` being allowed does not admit `math/rand/v2`, asserted by
`TestAllowlistDoesNotLeakNestedVariants`); `GoPath:
"/nonexistent-conductor-goembed"`; the four `i.Use(…)` registrations in order;
the `conductor/__step/__step` harness with `Ctx`/`Return` and the mutex around
the captured result; evaluating user code with `EvalWithContext` (so top-level
initializers are cancellable) and calling `run()` **inside** a second
`EvalWithContext` (yaegi's cancellation only fires for context-evaluated code);
the one-return vs two-return harness variants; `checkGoEmbedSignature` and
`goEmbedContractMsg` verbatim; every `code: go-embed: …` error prefix.

**risor** (`internal/code/risor.go`) — `risorGlobals`'s exact global set:
`builtins.Builtins()` plus base64/bytes/errors/json/math/regexp/strconv/strings/
time, and nothing else (no os/exec/http/dns/net/filepath, asserted by
`TestSandboxWithholdsEscapeModules`); `risor.WithoutDefaultGlobals()` +
`WithGlobals`; the `result == nil` → `{}` case; `risor: %w` error wrapping. The
module identity note (`risor-io` vs the moved `deepnoodle-ai` repo) is carried
into the package doc.

**lua** (`internal/code/lua.go`) — `lua.Options{SkipOpenLibs: true}`;
`L.SetContext(ctx)` for cancellation; the four libraries opened in order
(base/table/string/math) via the push-name-call idiom; the
`dofile`/`loadfile`/`load`/`loadstring` removal **after** opening base; the
`ctxTbl.(*lua.LTable)` guard before attaching the three faces; the `GetTop()==0`
→ `{}` case; `goToLua` and `luaToGo` copied verbatim, including the
map-vs-array-vs-mixed table rule and the int64-when-integral number rule.

---

## 8. What I could not fully verify

1. **Against the real conductor daemon.** These were driven by
   `internal/rpctest` and by a raw Python harness, both of which implement the
   daemon's side of the wire from the public SDK types. Neither is conductor's
   actual `internal/plugin` client, and neither exercises verify-before-execute,
   manifest confinement, or the spawn sandbox. Nor did I run a real flow with
   `engines: { js: { use: js } }` — this repo has no daemon. The daemon-side
   half is conductor's own test surface, against its `test/plugins/acme-*`
   plugins.

2. **The DataGuard / refusal path end-to-end.** The engines' handling of a
   refusal is tested (it surfaces as a thrown Error in JS, a raise in Lua, an
   error value in risor, an `error` return in go-embed), but the refusals came
   from a test double. Whether conductor's `plan write barrier` and
   agent-authored resource allowlist produce the refusals these engines relay is
   conductor's assertion to make. Note that the engines do **not** currently
   distinguish `plugin.IsRefused(err)` from an ordinary failure — they relay the
   error text, which already reads `refused by conductor: kv.get: …` because the
   SDK's `Refusal.Error()` says so. A snippet cannot branch on it
   programmatically. That matches the in-binary engines (which had no such
   distinction either), but it is a real gap worth a follow-up if the
   refused/failed split should reach snippet code.

3. **Timeout/cancellation behaviour under load.** The ctx wiring is ported
   faithfully (wazero `CloseOnContextDone`, yaegi's context-evaluated
   cancellation, gopher-lua's `SetContext`, risor's ctx-aware `Eval`), but I
   did not add tests that hang a `while(true)` and assert it gets cut — those
   are slow and conductor has them in-binary. The ported code paths are
   line-for-line the ones those tests cover.

4. **`e2e/manifest_test.go`** was left alone; the equivalent assertions for the
   four engines (kind, ABI, type, empty manifest) live in
   `e2e/engines_test.go` instead, since they need the engine-specific
   `Kind`/`ABI` check the existing table-driven test has no column for.

5. **The two pre-existing `gofmt` offenders** in `connectors/twilio/` are
   untouched. Fixing them would put unrelated churn in this branch; flagging
   them here instead.
