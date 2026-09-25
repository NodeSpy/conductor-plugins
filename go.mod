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
// for the host.kv/host.sql/host.memory callbacks. The decision-runtime
// surface runtimes/jev is built on — Decl.Protocols, ProtocolSystemOneV1,
// VerbDecide/VerbModels — arrived with conductor v0.54.0's decide: steps.
require github.com/NodeSpy/conductor v0.54.0

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
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/fsnotify/fsnotify v1.10.1
	github.com/itchyny/gojq v0.12.19
	github.com/jackc/pgx/v5 v5.11.0
	github.com/mikefarah/yq/v4 v4.53.6
	github.com/redis/go-redis/v9 v9.22.0
	github.com/tetratelabs/wazero v1.9.0
	go.starlark.net v0.0.0-20260908191801-89a6a09411d5
	go.yaml.in/yaml/v3 v3.0.4
)

require (
	cel.dev/expr v0.25.1 // indirect
	github.com/a8m/envsubst v1.4.3 // indirect
	github.com/agext/levenshtein v1.2.1 // indirect
	github.com/alecthomas/participle/v2 v2.1.4 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/apparentlymart/go-textseg/v17 v17.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dimchansky/utfbom v1.1.1 // indirect
	github.com/elliotchance/orderedmap v1.8.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/goccy/go-json v0.10.6 // indirect
	github.com/goccy/go-yaml v1.19.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/hashicorp/hcl/v2 v2.24.0 // indirect
	github.com/itchyny/timefmt-go v0.1.8 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jinzhu/copier v0.4.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/magiconair/properties v1.18.11 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/zclconf/go-cty v1.19.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.6 // indirect
	golang.org/x/exp v0.0.0-20240823005443-9b4947da3948 // indirect
	golang.org/x/mod v0.40.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
