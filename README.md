# conductor-plugins

Distributable **plugins** for [conductor](https://github.com/NodeSpy/conductor) —
external connectors the daemon fetches at `conductor init` instead of bundling
into the core binary. Each plugin is an out-of-process binary that speaks
conductor's plugin protocol over stdio; the daemon verifies, sandboxes, and runs
it.

Plugin **source** lives in this repo, under `cmd/conductor-<component>/`. Each
one is built only against conductor's public SDK — `pkg/plugin`, plus
`pkg/sourcekit` for webhook sources and `pkg/githubkit` for the github client —
and imports **no** conductor internals. That is enforced, not asserted:

```console
$ go list -deps ./... | grep NodeSpy/conductor/internal   # must print nothing
$ go list -deps ./... | grep NodeSpy
github.com/NodeSpy/conductor/pkg/githubkit
github.com/NodeSpy/conductor/pkg/plugin
github.com/NodeSpy/conductor/pkg/sourcekit
github.com/NodeSpy/conductor-plugins/cmd/conductor-github
github.com/NodeSpy/conductor-plugins/cmd/conductor-pagerduty
github.com/NodeSpy/conductor-plugins/cmd/conductor-paseo
github.com/NodeSpy/conductor-plugins/cmd/conductor-sentry
```

## Status — read this first

**No release has been published yet, so `conductor init` cannot fetch any of
these.** The repo has zero tags. Tagging is a deliberate human step (see
[Releasing](#releasing)).

`go.mod` carries a **temporary** `replace github.com/NodeSpy/conductor => …`
pointing at a local conductor checkout. It is there because `pkg/githubkit` —
and the `plugin.SourceHandler`/`start_source` surface these plugins need — exist
only on conductor's unmerged plugin-extraction branch. No tagged conductor
release contains them, so the `require github.com/NodeSpy/conductor v0.9.0`
above the replace cannot resolve them from the module proxy.

**Remove the replace and bump the require as soon as conductor tags a release
containing `pkg/githubkit`.** Until then, building here needs a conductor
checkout at the replace path; CI rewrites the replace to its own sibling
checkout.

## Available plugins

| Plugin | Kind | Provides | Bundled in conductor? | Notes |
|--------|------|----------|-----------------------|-------|
| `sentry` | connector (source) | `sentry` | **No — removed from core.** This is the only way to get it. | Sentry Integration-Platform webhooks → `issue_alert` / `error_alert` / `event_alert`. HMAC-verified. |
| `pagerduty` | connector (source) | `pagerduty` | **No — removed from core.** This is the only way to get it. | PagerDuty V3 incident webhooks → `incident`. Multi-signature (`v1=…,v1=…`) verified, so key rotation works. |
| `github` | connector (verbs + source) | `github` | **Yes — still bundled.** This is additive. | Full verb surface (comment, submit_review, pr_diff, merge_pr, create_issue, checks, releases, gists, …) over token or GitHub-App auth, plus the webhook events derivable from a single delivery. Built on `pkg/githubkit`. |
| `paseo` | connector (verbs) | `paseo` | **Yes — still bundled.** This is additive, opt-in. | The paseo-daemon operations `internal/dispatch.Backend` needs, by shelling to the `paseo` CLI. Driven by conductor's `rpcBackend`. |

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

Add it to your config's `plugins:` block and run `conductor init`:

```yaml
plugins:
  sentry:
    source: github.com/NodeSpy/conductor-plugins//sentry
    kind: connector
    version: "~> 1.0"          # semver constraint → highest matching release
    isolation: { mode: namespace }
connectors:
  mysentry:
    type: sentry               # the type the plugin provides
    listen: ":9099"
    client_secret: ${SENTRY_CLIENT_SECRET}
triggers:
  - on: mysentry.issue_alert
    filters: { levels: [error, fatal] }
    steps: [ ... ]
```

`conductor init` resolves the constraint to a release tag (`sentry/vX.Y.Z`),
downloads the per-platform asset `conductor-sentry_<os>_<arch>`, verifies it
against the release `checksums.txt` (and any `sha256:` you pin), vendors it, and
records it in `conductor.lock.yaml`. Boot is offline. Gate sources with
`plugin_trust:`; freeze a version with `hold: true`. See the conductor
[Plugins wiki](https://github.com/NodeSpy/conductor/wiki/Plugins).

Because a plugin is code the daemon executes — and, for a connector, code that
*receives your credential* — an entry without an `isolation:` block is refused
unless you set `allow_unsandboxed: true`. Don't.

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
its plugin client, the sandbox, and sha256 verify-before-execute. That stays in
conductor, tested against its in-repo `test/plugins/acme-*` reference plugins,
because the daemon's client is internal and deliberately unreachable from a
plugin module.

## Releasing

Not automated on merge, and not done yet — pushing a tag publishes binaries, so
it is a human decision.

Tag shape is `<component>/vX.Y.Z` (e.g. `sentry/v1.0.0`) — the same shape
`conductor init` resolves for
`source: github.com/NodeSpy/conductor-plugins//<component>`. Pushing one runs
[`.github/workflows/release.yml`](.github/workflows/release.yml), which
cross-builds `cmd/conductor-<component>` for linux/darwin/windows on amd64 and
arm64, checksums the set, and publishes
`conductor-<component>_<os>_<arch>` + `checksums.txt` to the release.

The workflow checks conductor out as a sibling and rewrites the `replace` to
point at it, so it builds against a real conductor tree rather than the
developer's local path. Set the `CONDUCTOR_REF` repository variable to pin which
conductor ref that is; it defaults to `main`. **Until conductor's plugin
extraction lands on `main`, `CONDUCTOR_REF` must name the branch that carries
`pkg/githubkit`, or the build will fail.**

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

Release it from **your own** repo as `conductor-<component>_<os>_<arch>` assets
under `<component>/vX.Y.Z` tags, and point `source:` at your repo. Source
plugins (webhook/poll) additionally use `pkg/sourcekit` (HMAC verify, webhook
listener, dedup) and implement `plugin.SourceHandler`.
