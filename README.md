# conductor-plugins

Distributable **plugins** for [conductor](https://github.com/NodeSpy/conductor) —
external connectors the daemon fetches at `conductor init` instead of bundling
into the core binary. Each plugin is an out-of-process binary that speaks
conductor's plugin protocol over stdio; the daemon verifies, sandboxes, and runs
it.

Plugin **source** lives in this repo, under `<kind>/<name>/` — `connectors/` or
`runtimes/`. That layout is load-bearing: conductor's `use:` resolver turns a
bare name in a config block into `<kind>/<name>` in THIS repo, so a bare
`use: sentry` under `connectors:` resolves to `connectors/sentry` and nothing
else. A connector and a runtime may share a name without colliding. Each plugin
is built only against conductor's public SDK — `pkg/plugin`, plus
`pkg/sourcekit` for webhook sources and `pkg/githubkit` for the github client —
and imports **no** conductor internals. That is enforced, not asserted:

```console
$ go list -deps ./... | grep NodeSpy/conductor/internal   # must print nothing
$ go list -deps ./... | grep NodeSpy
github.com/NodeSpy/conductor/pkg/githubkit
github.com/NodeSpy/conductor/pkg/plugin
github.com/NodeSpy/conductor/pkg/sourcekit
github.com/NodeSpy/conductor-plugins/connectors/docker
github.com/NodeSpy/conductor-plugins/connectors/github
github.com/NodeSpy/conductor-plugins/connectors/helm
github.com/NodeSpy/conductor-plugins/connectors/kubernetes
github.com/NodeSpy/conductor-plugins/connectors/pagerduty
github.com/NodeSpy/conductor-plugins/connectors/sentry
github.com/NodeSpy/conductor-plugins/runtimes/paseo
```

**Documentation** lives in [`docs/`](docs/README.md) — the plugin model, install
and capability details, and a reference page per plugin (linked from the table
below).

## Status — read this first

`go.mod` requires `github.com/NodeSpy/conductor v0.9.0` — the tagged release
that carries `pkg/githubkit` and the `plugin.SourceHandler`/`start_source`
surface these plugins need — resolved straight from the public module proxy.
No `replace` directive.

## Available plugins

| Plugin | Kind | Provides | Bundled in conductor? | Notes |
|--------|------|----------|-----------------------|-------|
| [`sentry`](docs/connectors/sentry.md) | connector (source) | `sentry` | **No — removed from core.** This is the only way to get it. | Sentry Integration-Platform webhooks → `issue_alert` / `error_alert` / `event_alert`. HMAC-verified. |
| [`pagerduty`](docs/connectors/pagerduty.md) | connector (source) | `pagerduty` | **No — removed from core.** This is the only way to get it. | PagerDuty V3 incident webhooks → `incident`. Multi-signature (`v1=…,v1=…`) verified, so key rotation works. |
| [`github`](docs/connectors/github.md) | connector (verbs + source) | `github` | **Yes — still bundled.** This is additive. | Full verb surface (comment, submit_review, pr_diff, merge_pr, create_issue, checks, releases, gists, …) over token or GitHub-App auth, plus the webhook events derivable from a single delivery. Built on `pkg/githubkit`. |
| [`paseo`](docs/runtimes/paseo.md) | runtime | `paseo` | **Yes — still bundled.** This is additive, opt-in. | The paseo-daemon operations `internal/dispatch.Backend` needs, by shelling to the `paseo` CLI. Driven by conductor's `rpcBackend`. |
| [`docker`](docs/connectors/docker.md) | connector (verbs) | `docker` | **No — never in core.** Add it here. | The container-engine lifecycle as verbs (`run`, `exec`, `build`, `pull`, `push`, `ps`, `images`, `logs`, `stop`, `start`, `rm`, `inspect`, `compose`, `buildx`, `bake`, `cli`) by shelling to the `docker` (or `podman`) CLI. Local by default; `docker_host: ssh://…` / `context:` reach a remote engine. `buildx`/`bake` are docker-only. |
| [`helm`](docs/connectors/helm.md) | connector (verbs) | `helm` | **No — never in core.** Add it here. | Helm releases as verbs (`install`, `upgrade`, `uninstall`, `rollback`, `list`, `status`, `history`, `get_values`, `template`, `pull`, `repo_add`, `repo_update`, `test`, `lint`, `cli`) by shelling to `helm`. `list`/`status`/`history`/`get_values` parse JSON into structured outputs. |
| [`kubernetes`](docs/connectors/kubernetes.md) | connector (verbs) | `kubernetes` | **No — never in core.** Add it here. | The Kubernetes lifecycle as verbs (`apply`, `get`, `delete`, `describe`, `logs`, `exec`, `rollout`, `scale`, `patch`, `create`, `label`, `annotate`, `wait`, `top`, `cordon`/`uncordon`/`drain`, `cp`, `cli`) by shelling to `kubectl`. `kubeconfig`/`context`/`namespace` select the target; `get`/`apply` parse JSON into `result`. |

### What the source plugins do NOT replace

`sentry` and `pagerduty` genuinely replace connectors that no longer exist in
conductor. They are **not** drop-in equivalents of the old bundled ones, and
`conductor config migrate` will refuse to auto-convert a legacy config for
exactly this reason:

- Event names changed — the bundled sentry connector had one `alert` event;
  this plugin declares `issue_alert` / `error_alert` / `event_alert`.
- Context is flat. Steps template `{{.level}}`, `{{.short_id}}`, … — not the old
  nested `{{.sentry.level}}`.
- Filters use the plugin's own vocabulary (`levels`, `projects`,
  `environments`, `event_types`, `services`, `urgencies`, `priorities`) and are
  evaluated by the daemon's generic list-contains evaluator. There is **no
  `exclude:` support**, so the legacy "first matching rule wins" precedence
  cannot be expressed — write mutually exclusive filters instead.

`github` and `paseo` are additive: conductor still bundles both, and its own
autopilot depends on the bundled github (App-token minting, the catch-up sweep,
the poll-derived events). Use the plugin when you want github verbs running in a
sandboxed subprocess with a narrowed credential, not as a replacement. See
conductor's `docs/design/connector-extraction.md`.

## Install a plugin

Name it in `use:` and run `conductor init`. There is no separate declaration —
the reference *is* the declaration:

```yaml
connectors:
  mysentry:
    use: sentry                          # bare name → connectors/sentry, here
    listen: ":9099"
    client_secret: ${SENTRY_CLIENT_SECRET}
triggers:
  - on: mysentry.issue_alert
    filters: { levels: [error, fatal] }
    steps: [ ... ]

runtimes:
  gpu: { use: paseo }                    # runtimes: → runtimes/paseo, here
```

`conductor init` resolves the highest compatible release tag
(`connectors/sentry/vX.Y.Z`), downloads the per-platform asset
`conductor-sentry_<os>_<arch>`, verifies it against the release
`checksums.txt`, and installs it under conductor's **state dir** — install state
is local to the machine, not a committed lockfile. Boot is offline. See the
conductor [Plugins wiki](https://github.com/NodeSpy/conductor/wiki/Plugins).

Pin an exact build with `use: sentry@v1.2.3`; leave it off and the plugin stays
current, with every update logged along with the sha it moved from. This repo
is in conductor's DEFAULT `plugin_trust` allowlist, so installing from here
needs no ceremony; a third-party repo needs an explicit `plugin_trust.allow`
entry.

### What it can do, and what it may do

Each plugin declares its own **permission manifest** — the hosts it calls and
the commands it spawns — in its `Describe`. Conductor records that at install,
shows it to you when you add the plugin, and confines the subprocess to it. A
connector's optional `network:` may NARROW that declaration, never widen it:

```yaml
connectors:
  gh: { use: github, network: ["api.github.com:443"] }
```

OS isolation is available but **not** required: it is opt-in hardening for a
locked-down box, not the default path.

```yaml
connectors:
  mysentry: { use: sentry, isolation: { mode: namespace } }
```

## Tests

`e2e/` drives each built binary over the real wire: newline-delimited JSON-RPC
2.0 on the subprocess's stdin/stdout, using the public SDK's own
request/response types via `internal/rpctest`. It covers HMAC verification and
normalized event shape for the two source plugins, github's token-auth and
App-auth verb paths plus its webhook source, and paseo's verb set and CLI round
trip.

```console
$ go test ./...
```

These tests moved here from conductor's `internal/plugin` package when the
sources moved into this repo. What they do **not** cover is the daemon side —
its plugin client, the manifest confinement, and verify-before-execute. That
stays in conductor, tested against its in-repo `test/plugins/acme-*` reference
plugins, because the daemon's client is internal and deliberately unreachable
from a plugin module.

`e2e/manifest_test.go` additionally asserts that every plugin here declares its
own `Kind` and a permission manifest that matches what it actually does — a
plugin that declares nothing gives conductor nothing to enforce, and a plugin
that over-declares quietly widens what the operator is asked to accept.

## Releasing

Not automated on merge — pushing a tag publishes binaries, so it is a human
decision.

Tag shape is `<kind>/<name>/vX.Y.Z` — `connectors/sentry/v1.0.0`,
`runtimes/paseo/v1.0.0`. The kind prefix matches the source directory AND the
prefix conductor's resolver matches against when it resolves a bare `use:`, so
tags for a connector and a runtime of the same name never mix.

Pushing one runs [`.github/workflows/release.yml`](.github/workflows/release.yml),
which cross-builds `<kind>/<name>` for linux/darwin/windows on amd64 and arm64,
checksums the set, and publishes `conductor-<name>_<os>_<arch>` +
`checksums.txt` to the release. Note the ASSET name is flat — the kind lives in
the tag, not in the filename.

The workflow builds straight against `github.com/NodeSpy/conductor v0.9.0` from
the public module proxy — no sibling checkout, no `replace` rewrite.

## Build your own plugin

You don't need this repo. A plugin is a standalone Go binary:

```go
package main

import plugin "github.com/NodeSpy/conductor/pkg/plugin"

func main() {
	plugin.Serve(plugin.ConnectorFunc(
		func() plugin.Decl { return plugin.Decl{Type: "acme", Verbs: []plugin.Verb{{Name: "ping"}}} },
		func(r plugin.InvokeRequest) (plugin.InvokeResult, error) {
			return plugin.InvokeResult{Outputs: map[string]any{"pong": true}}, nil
		},
	))
}
```

Release it from **your own** repo as `conductor-<name>_<os>_<arch>` assets
under `<kind>/<name>/vX.Y.Z` tags, and point `use:` at your repo
(`use: your-org/your-repo/<name>`) — a third-party source needs an explicit
`plugin_trust.allow` entry in the operator's config. Declare `Kind` in your
`Describe`: conductor checks it against the block the reference appeared in, so
a connector can never be wired as a runtime. Source
plugins (webhook/poll) additionally use `pkg/sourcekit` (HMAC verify, webhook
listener, dedup) and implement `plugin.SourceHandler`.
