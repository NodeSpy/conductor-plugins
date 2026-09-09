module github.com/NodeSpy/conductor-plugins

go 1.26.3

// The plugin sources live in the conductor module under plugins/ (SDK-only:
// they import github.com/NodeSpy/conductor/pkg/plugin + /pkg/sourcekit and no
// internals). This repo is the DISTRIBUTION channel — its release CI builds them
// and publishes the per-platform binaries + checksums that `conductor init`
// fetches. A sibling checkout of conductor resolves the replace below; the CI
// checks conductor out next to this repo.
require github.com/NodeSpy/conductor v0.9.0

replace github.com/NodeSpy/conductor => ../conductor
