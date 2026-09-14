module github.com/NodeSpy/conductor-plugins

go 1.26.3

// The plugin sources live HERE, under connectors/<name>/ and runtimes/<name>/.
// They are SDK-only: they import github.com/NodeSpy/conductor/pkg/{plugin,
// sourcekit,githubkit} and no conductor internals (enforced by the
// internal-free gate in CONTRIBUTING/README). This repo is both the SOURCE and
// the DISTRIBUTION channel — its release CI builds a component and publishes
// the per-platform binaries + checksums that `conductor init` fetches.
require github.com/NodeSpy/conductor v0.9.0

require github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
