module github.com/NodeSpy/conductor-plugins

go 1.26.3

// The plugin sources live HERE, under connectors/<name>/, runtimes/<name>/ and
// engines/<name>/. They are SDK-only: they import
// github.com/NodeSpy/conductor/pkg/{plugin,sourcekit,githubkit} and no
// conductor internals (enforced by the internal-free gate in
// CONTRIBUTING/README). This repo is both the SOURCE and the DISTRIBUTION
// channel — its release CI builds a component and publishes the per-platform
// binaries + checksums that `conductor init` fetches.
//
// v0.11.0 is the SDK release that carries the STEP-ENGINE surface the
// engines/ plugins are built on: plugin.KindStep, Decl.ABI/EngineABI,
// plugin.EngineFunc, the plugin.run request/result, and the plugin.Host client
// for the host.kv/host.sql/host.memory callbacks.
require github.com/NodeSpy/conductor v0.17.0

// The interpreters the four step engines embed. These are the SAME versions
// conductor pinned while the engines lived in its binary, so the port is a
// move rather than an upgrade; each is pure Go (qjs is QuickJS compiled to
// WASM, run under wazero) so the zero-cgo cross-compiled release holds.
require (
	github.com/fastschema/qjs v0.0.6
	github.com/risor-io/risor v1.8.1
	github.com/traefik/yaegi v0.16.1
	github.com/yuin/gopher-lua v1.1.2
)

require (
	cel.dev/cel-go v0.32.0
	github.com/itchyny/gojq v0.12.19
	github.com/tetratelabs/wazero v1.12.0
	go.starlark.net v0.0.0-20260908191801-89a6a09411d5
)

require (
	cel.dev/expr v0.25.1 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/itchyny/timefmt-go v0.1.8 // indirect
	github.com/kr/text v0.2.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/exp v0.0.0-20240823005443-9b4947da3948 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
