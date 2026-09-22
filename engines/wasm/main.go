// Command conductor-wasm is the `use: wasm` STEP ENGINE as an external
// conductor plugin: it runs an ARBITRARY, user-supplied WebAssembly module as
// a step, sandboxed by wazero (pure Go, no cgo — the same engine
// engines/js runs its bundled QuickJS-as-WASM under, here exposed directly
// instead of wrapping one fixed guest).
//
// CONTRACT (WASI stdin/stdout JSON — language-agnostic, unlike js/lua/risor's
// source-snippet contract, which controls the guest's source and can inject
// ctx bindings into it):
//
//	code     RunRequest.Code is the WASI COMMAND module (one compiled with an
//	         exported "_start" — e.g. TinyGo, Rust's wasm32-wasip1 target, or
//	         Zig), given EITHER as a BASE64-encoded binary OR as a PATH to a
//	         .wasm file on disk ("file:/opt/mods/x.wasm", or a bare path that
//	         exists). Reading the file is a host-side load of the code to run;
//	         it does not give the guest filesystem access. A read/decode
//	         failure, or bytes that don't compile as WASM, is a clear error.
//	inputs   RunRequest.Inputs, json.Marshal'd onto the module's stdin
//	         (fd 0). The module reads it however its language does that —
//	         conductor never parses the guest's argv or code, only feeds it.
//	outputs  the module's stdout (fd 1), read back after it returns and
//	         parsed as JSON, then passed through enginekit.WrapValue exactly
//	         like every other engine's return value: an object becomes the
//	         named outputs, any other JSON value becomes value:. Blank or
//	         non-JSON stdout from a module that exited zero is NOT an error —
//	         it is {} (no outputs), the same as a step that only had side
//	         effects. A module that exits NON-ZERO *is* an error; its stdout,
//	         if any, is discarded rather than trusted.
//	args/env RunRequest.Args become the module's argv (after a fixed
//	         argv[0]), RunRequest.Env becomes its environment — both via the
//	         WASI ModuleConfig, the same two halves `use: cli` gives a real
//	         subprocess, minus the subprocess.
//
// THE SANDBOX IS WHAT IS NEVER OPENED: this file never calls
// ModuleConfig.WithFS/WithFSConfig, so the module is never given a
// filesystem, and WASI preview 1 has no socket syscalls at all — the only
// host surface wired up is wasi_snapshot_preview1 plus stdin/stdout/stderr/
// args/env. A module cannot reach this host's filesystem or network no
// matter what it imports, because nothing here answers those imports.
//
// UNLIKE js/lua/risor, this engine does NOT expose ctx.store/ctx.sql/
// ctx.memory to the guest. Those are source-level shims this repo injects
// into a snippet whose source it controls; an arbitrary precompiled WASM
// module has no such injection point; its only announced ABI is WASI. So a
// `use: wasm` step's entire data plane is its stdin/stdout — nothing more.
// (host is still accepted, purely for parity with the other engines' Run
// signature and for the same stderr log line; execWASM never calls it.)
//
// ctx cancellation (the step's `timeout:`) is honored via
// RuntimeConfig.WithCloseOnContextDone: wazero halts the module (and any call
// into it) once ctx expires, so a runaway guest is cut instead of wedging the
// plugin — the same mechanism engines/js relies on for its qjs runtime.
//
// stdout is the RPC transport (plugin.Serve on os.Stdin/os.Stdout). The GUEST
// module's stdout is captured to a private buffer and never touches that
// transport; this plugin's own logs and the guest's stderr both go to this
// process's os.Stderr.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

func describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindStep,
		ABI:  plugin.EngineABI,
		Type: "wasm",
		Desc: "Runs an arbitrary WASI command module under wazero (pure Go, no cgo): code: is the module, either base64-encoded or a path to a .wasm file (file:/path or a bare path). Inputs are JSON on the module's stdin, its stdout parsed as JSON is the step's outputs. The guest is sandboxed to stdin/stdout/stderr/args/env only — no filesystem, no network.",
		// No egress, no fs, no spawns: the guest gets a WASI runtime with
		// nothing mounted and no socket syscalls, so the manifest matches
		// the sandbox exactly.
		Capabilities: plugin.Capabilities{},
	}
}

func run(ctx context.Context, req plugin.RunRequest, host *plugin.Host) (plugin.RunResult, error) {
	fmt.Fprintf(os.Stderr, "conductor-wasm: run instance=%s inputs=%d data-plane=%v\n",
		req.Instance, len(req.Inputs), host.Available())
	outputs, err := execWASM(ctx, req)
	if err != nil {
		return plugin.RunResult{}, err
	}
	return plugin.RunResult{Outputs: outputs}, nil
}

// execWASM decodes req.Code as a base64 WASI command module, runs it under a
// fresh wazero Runtime with req.Inputs as JSON on its stdin, and turns its
// captured stdout into the step's outputs. See the package doc for the full
// contract, including how a non-zero exit and non-JSON stdout are handled.
func execWASM(ctx context.Context, req plugin.RunRequest) (map[string]any, error) {
	wasmBytes, derr := loadModule(req.Code)
	if derr != nil {
		return nil, derr
	}

	stdinJSON, merr := json.Marshal(req.Inputs)
	if merr != nil {
		return nil, fmt.Errorf("wasm: marshal inputs: %w", merr)
	}

	// A fresh Runtime per run: simplest correct thing, and cheap next to
	// actually executing a guest module. (A compiled-module cache keyed by a
	// hash of req.Code would save the compile step across repeated runs of
	// the same code; left as a future optimization, not required for
	// correctness.)
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
	defer rt.Close(ctx)

	wasi, werr := wasi_snapshot_preview1.Instantiate(ctx, rt)
	if werr != nil {
		return nil, fmt.Errorf("wasm: instantiate WASI: %w", werr)
	}
	defer wasi.Close(ctx)

	compiled, cerr := rt.CompileModule(ctx, wasmBytes)
	if cerr != nil {
		return nil, fmt.Errorf("wasm: not a valid WASM module: %w", cerr)
	}
	defer compiled.Close(ctx)

	var stdout bytes.Buffer
	modCfg := wazero.NewModuleConfig().
		WithStdin(bytes.NewReader(stdinJSON)).
		WithStdout(&stdout).
		WithStderr(os.Stderr). // guest diagnostics, never the RPC transport
		WithArgs(append([]string{"step"}, req.Args...)...)
	// SANDBOX: no WithFS/WithFSConfig call — the module is never given a
	// filesystem. WASI preview 1 has no socket syscalls at all, so there is
	// no network call to withhold either; stdin/stdout/stderr/args/env is
	// the entire surface.
	for k, v := range req.Env {
		modCfg = modCfg.WithEnv(k, v)
	}

	// InstantiateModule runs the module's exported "_start" as part of
	// instantiation (WASI's convention for a command module) — there is no
	// separate "call the entry point" step.
	mod, ierr := rt.InstantiateModule(ctx, compiled, modCfg)
	if mod != nil {
		defer mod.Close(ctx)
	}
	if ierr != nil {
		// wazero reports an explicit proc_exit(0) as a nil error already, so
		// any *sys.ExitError reaching here is necessarily a failure: either
		// the guest itself exited non-zero (its stdout, if any, is
		// discarded rather than trusted), or CloseOnContextDone closed the
		// module out from under it, which wazero also surfaces as an
		// *sys.ExitError carrying one of two reserved sentinel codes — that
		// case is reported as the ctx error it actually is, not a made-up
		// exit code.
		var exitErr *sys.ExitError
		if errors.As(ierr, &exitErr) {
			switch exitErr.ExitCode() {
			case sys.ExitCodeContextCanceled, sys.ExitCodeDeadlineExceeded:
				return nil, fmt.Errorf("wasm: %w", ctx.Err())
			default:
				return nil, fmt.Errorf("wasm: module exited with code %d", exitErr.ExitCode())
			}
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("wasm: %w", ctx.Err())
		}
		return nil, fmt.Errorf("wasm: run module: %w", ierr)
	}

	// A module with no exported "_start" instantiates without error and
	// simply never runs (wazero's default start-function list is
	// ["_start"]; a missing export there is skipped, not a failure) — its
	// stdout is empty, so it lands in the same "no outputs" case just below
	// as a module that ran and printed nothing.
	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return map[string]any{}, nil
	}
	var v any
	if jerr := json.Unmarshal([]byte(text), &v); jerr != nil {
		// Documented behavior: non-JSON stdout from a module that exited
		// zero is not an error, just no outputs. A module need not speak
		// this JSON contract to be considered a successful run.
		return map[string]any{}, nil
	}
	return enginekit.WrapValue(v), nil
}

// loadModule turns the step's code: into the WASM module bytes. It accepts
// either the module inline (BASE64) or a PATH to a .wasm file on disk, so a
// module doesn't have to be base64-inlined into the config:
//
//   - a "file:" prefix ("file:/opt/mods/resize.wasm") is always a path;
//   - a bare value that stat()s as a regular file is read as that file;
//   - anything else is decoded as base64 (the original contract).
//
// Reading the file is a HOST-side load of the module to execute — it does NOT
// give the guest filesystem access; the sandbox below is unchanged (no
// WithFS). A leading "~/" is expanded to the user's home directory.
func loadModule(code string) ([]byte, error) {
	raw := strings.TrimSpace(code)
	if raw == "" {
		return nil, fmt.Errorf("wasm: code is empty (expected a base64 module or a .wasm file path)")
	}

	path, isPath := "", false
	if rest, ok := strings.CutPrefix(raw, "file:"); ok {
		path, isPath = strings.TrimSpace(rest), true
	} else if info, err := os.Stat(expandHome(raw)); err == nil && info.Mode().IsRegular() {
		path, isPath = raw, true
	}

	if isPath {
		b, err := os.ReadFile(expandHome(path))
		if err != nil {
			return nil, fmt.Errorf("wasm: read module file: %w", err)
		}
		return b, nil
	}

	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("wasm: code is neither an existing .wasm file path nor valid base64: %w", err)
	}
	return b, nil
}

// expandHome replaces a leading "~/" (or a bare "~") with the user's home dir.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + strings.TrimPrefix(p, "~")
		}
	}
	return p
}

func main() {
	if err := plugin.Serve(plugin.EngineFunc(describe, run)); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-wasm: serve: %v\n", err)
		os.Exit(1)
	}
}
