module github.com/NodeSpy/conductor-plugins

go 1.26.3

// The plugin sources live HERE, under cmd/conductor-<component>/. They are
// SDK-only: they import github.com/NodeSpy/conductor/pkg/{plugin,sourcekit,
// githubkit} and no conductor internals (enforced by the internal-free gate in
// CONTRIBUTING/README). This repo is both the SOURCE and the DISTRIBUTION
// channel — its release CI builds a component and publishes the per-platform
// binaries + checksums that `conductor init` fetches.
require github.com/NodeSpy/conductor v0.9.0

require github.com/golang-jwt/jwt/v5 v5.3.1 // indirect

// TEMPORARY. pkg/githubkit (and the pkg/plugin StartSource surface these
// plugins need) exist only on conductor's unmerged plugin-extraction branch —
// no tagged conductor release contains them yet, so the v0.9.0 require above
// cannot resolve them from the module proxy. This replace points at a LOCAL
// conductor checkout. Remove it, and bump the require to the real version, as
// soon as conductor tags a release that contains pkg/githubkit. Until then
// `go build ./...` only works with a conductor checkout at this path (CI
// rewrites the replace to its own sibling checkout — see
// .github/workflows/release.yml).
replace github.com/NodeSpy/conductor => /home/daniel/Projects/conductor
