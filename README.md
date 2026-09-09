# conductor-plugins

Distributable **plugins** for [conductor](https://github.com/NodeSpy/conductor) —
external connectors and runtimes the daemon fetches at `conductor init` instead
of bundling into the core binary. Each plugin is an out-of-process binary that
speaks conductor's plugin protocol; the daemon verifies, sandboxes, and runs it.

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

## Available plugins

| Plugin | Kind | Provides | Notes |
|--------|------|----------|-------|
| `sentry` | connector (source) | `sentry` | Sentry Integration-Platform webhooks → `issue_alert`/`error_alert`/`event_alert` events. HMAC-verified. |
| `pagerduty` | connector (source) | `pagerduty` | PagerDuty V3 incident webhooks → `incident` events. Multi-signature (`v1=…`) verified. |

More are extracted from conductor's core over time (github, the paseo runtime, …).

## How releases are built

The plugin **source** lives in the conductor repo under
[`plugins/`](https://github.com/NodeSpy/conductor/tree/main/plugins) — each is
built **only** against the public SDK (`github.com/NodeSpy/conductor/pkg/plugin`)
and connector-kit (`.../pkg/sourcekit`), with **no** conductor internals, exactly
as a third-party plugin would be. This repo's [release workflow](.github/workflows/release.yml)
cross-builds a component for all platforms and publishes the assets + checksums
when a `<component>/vX.Y.Z` tag is pushed.

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
under `<component>/vX.Y.Z` tags, and point `source:` at your repo. Source plugins
(webhook/poll) additionally use `pkg/sourcekit` (HMAC verify, webhook listener,
dedup) and implement `plugin.SourceHandler`.
