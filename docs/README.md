# conductor-plugins documentation

Reference docs for the plugins in this repo, kept next to the code they
describe. For the quickstart and the at-a-glance plugin table, see the
[top-level README](../README.md); this directory is the deeper reference.

## The plugin model

A conductor plugin is an **out-of-process binary** the daemon spawns and talks
to over **newline-delimited JSON-RPC 2.0 on stdin/stdout**. stdout is the RPC
transport; everything a plugin logs goes to stderr. The daemon verifies the
binary against the release checksums, records its declared permissions, sandboxes
it, and runs it.

Each plugin is built **only** against conductor's public SDK — `pkg/plugin`,
plus `pkg/sourcekit` for webhook sources and `pkg/githubkit` for the GitHub
client — and imports **no** conductor internals. That is enforced at release
time (`go list -deps ./... | grep NodeSpy/conductor/internal` must print
nothing), not merely asserted.

### Layout and `use:` resolution

Plugin source lives at `<kind>/<name>/`. That layout is load-bearing:
conductor's `use:` resolver turns a bare name in a config block into
`<kind>/<name>` in **this** repo.

```yaml
connectors:
  gh:   { use: github }     # → connectors/github
runtimes:
  gpu:  { use: paseo }      # → runtimes/paseo
```

A connector and a runtime may share a name without colliding, because the config
block (`connectors:` vs `runtimes:`) selects the kind.

### Kinds

| Kind | Declared as | Provides |
|------|-------------|----------|
| **connector** | `plugin.KindConnector` | **verbs** (`uses: <name>.<verb>` actions), and/or **source events** (`on: <name>.<event>` triggers) |
| **runtime** | `plugin.KindRuntime` | the operations conductor's `internal/dispatch.Backend` drives, over the plugin RPC |

A connector can be verbs-only (docker, the verb half of github), source-only
(sentry, pagerduty), or both (github).

## Installing a plugin

Name it in `use:` and run `conductor init`. The reference *is* the declaration —
there is no separate install step.

```yaml
connectors:
  d: { use: docker }
```

`conductor init` resolves the highest compatible release tag
(`connectors/<name>/vX.Y.Z`), downloads the per-platform asset
`conductor-<name>_<os>_<arch>`, verifies it against the release `checksums.txt`,
and installs it under conductor's **state dir**. Boot is offline.

- **Pin** an exact build with `use: <name>@vX.Y.Z`; leave it off to stay current
  (every move is logged with the sha it moved from).
- **Trust:** this repo is in conductor's default `plugin_trust` allowlist, so
  installing from here needs no ceremony. A third-party repo needs an explicit
  `plugin_trust.allow` entry.

## Capabilities — the permission manifest

Every plugin declares, in its `Describe`, a **permission manifest**: the network
hosts it calls (`Egress`), the commands it spawns (`Commands` / `Spawns`), and
the filesystem paths it needs (`FS`). Conductor records it at install, **shows
it to you** when you add the plugin, and confines the subprocess to it.

A connector instance's `network:` may **narrow** the declared egress, never
widen it:

```yaml
connectors:
  gh: { use: github, network: ["api.github.com:443"] }
```

OS isolation is available but **not** required — opt-in hardening for a
locked-down box, not the default path:

```yaml
connectors:
  mysentry: { use: sentry, isolation: { mode: namespace } }
```

A manifest is a *visible, can't-exceed-declaration* contract, not an OS jail on
its own. See conductor's `docs/design/use-unification.md §D` for exactly what it
does and does not enforce.

## Authoring a plugin

A verb-only connector implements two methods:

```go
type Handler interface {
    Describe() plugin.Decl                          // self-description
    Invoke(plugin.InvokeRequest) (plugin.InvokeResult, error) // run one verb
}
```

A source connector also implements `StartSource(ctx, req, emit)` and streams
events by calling `emit`. `func main()` is just `plugin.Serve(handler{})`, which
runs the protocol until the daemon closes stdin.

- **Options and outputs** are `plugin.Schema` maps (`map[string]plugin.Field`).
  A non-zero subprocess exit is usually returned as `exit_code` **data**, not an
  RPC error — reserve errors for "could not run at all".
- **Credentials** arrive per-call in `InvokeRequest.Connection` (verbs) or
  `StartSourceRequest.Config` (sources), never via ambient env.
- **Protocol version** is stamped automatically; the daemon refuses a plugin
  whose major version it does not understand.

### Testing

`go test ./...` runs each plugin's unit tests plus the `e2e/` suite, which
drives the built binaries over the real wire (newline-delimited JSON-RPC via
`internal/rpctest`). Favor **hermetic** tests that assert argv/parse logic
without spawning the real tool.

### Release

Tag-driven — no CI matrix to edit. Tag shape is `<kind>/<name>/vX.Y.Z`:

```console
$ git tag connectors/docker/v0.1.0 && git push origin connectors/docker/v0.1.0
```

CI derives the kind/name/version from the tag, runs the internal-free gate and
tests, cross-builds `conductor-<name>_<os>_<arch>` for linux/amd64,
linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, and publishes the
binaries + `checksums.txt`.

## Plugin reference

| Plugin | Kind | Page |
|--------|------|------|
| `docker` | connector (verbs) | [connectors/docker.md](connectors/docker.md) |
| `github` | connector (verbs + source) | [connectors/github.md](connectors/github.md) |
| `sentry` | connector (source) | [connectors/sentry.md](connectors/sentry.md) |
| `pagerduty` | connector (source) | [connectors/pagerduty.md](connectors/pagerduty.md) |
| `paseo` | runtime | [runtimes/paseo.md](runtimes/paseo.md) |
