package main

// This engine's happy path needs an actual compiled WASI module — there is
// no toolchain available in-agent to produce one from source (no TinyGo, no
// Rust wasm32-wasip1 target). Rather than depend on wazero's own bundled
// example/testdata binaries (which live under cmd/wazero and
// imports/wasi_snapshot_preview1/example — the "cat" ones there read a file
// from a mounted filesystem, not stdin, so they don't exercise this engine's
// stdin/stdout contract; and vendoring one via go:embed would mean adding a
// third file, which the task deliberately limits to two), this file
// hand-assembles the SMALLEST valid WASM modules needed for each case, byte
// section by byte section, using only encoding/binary-style LEB128 helpers
// below. Every test module is a real WASI command module wazero actually
// compiles and runs — nothing here is mocked.
//
// wasmHello is the happy-path module: it imports wasi_snapshot_preview1's
// fd_write, and its _start writes one FIXED JSON string to fd 1 (stdout) via
// a data segment holding both the iovec and the string bytes. This exercises
// the full path end to end: stdin is fed (and ignored, as this guest never
// reads it — a real echo-style guest would, but proving stdin was delivered
// doesn't require the guest to consume it) and stdout is captured, parsed as
// JSON, and wrapped exactly as execWASM documents.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// ---- minimal WASM byte assembler -----------------------------------------
//
// Just enough of the binary format (https://webassembly.github.io/spec/core/binary/)
// to build tiny WASI command modules: LEB128 integers, length-prefixed
// vectors and sections, and the handful of section/instruction encodings the
// builders below use.

func uleb128(v uint32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}

func sleb128(v int32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		signBitSet := b&0x40 != 0
		if (v == 0 && !signBitSet) || (v == -1 && signBitSet) {
			out = append(out, b)
			return out
		}
		out = append(out, b|0x80)
	}
}

// name encodes a WASM "name": a byte vector holding UTF-8 text.
func wname(s string) []byte {
	return append(uleb128(uint32(len(s))), []byte(s)...)
}

// vec length-prefixes a sequence of already-self-delimited blobs (functypes,
// code entries, data segments, ...) — the general "vec(B)" production.
func wvec(items ...[]byte) []byte {
	out := uleb128(uint32(len(items)))
	for _, it := range items {
		out = append(out, it...)
	}
	return out
}

// wsection wraps content in a section: id byte + byte-length + content.
func wsection(id byte, content []byte) []byte {
	out := []byte{id}
	out = append(out, uleb128(uint32(len(content)))...)
	return append(out, content...)
}

// functype encodes a func type: 0x60, params (each already one valtype
// byte), results (same).
func functype(params, results []byte) []byte {
	b := []byte{0x60}
	b = append(b, uleb128(uint32(len(params)))...)
	b = append(b, params...)
	b = append(b, uleb128(uint32(len(results)))...)
	b = append(b, results...)
	return b
}

const valI32 = 0x7f

var wasmHeader = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00} // \0asm, version 1

// wasmEmpty is the smallest possible valid module: the header and no
// sections at all — no types, no functions, no exports, in particular no
// "_start". Used to prove a module missing an entry point is handled
// cleanly (no error, no output) rather than rejected.
func wasmEmpty() []byte {
	return append([]byte{}, wasmHeader...)
}

// wasmHello builds a module that imports wasi_snapshot_preview1.fd_write and
// whose _start writes the fixed string jsonOut to fd 1 then returns.
//
// Memory layout (one page, everything from a single data segment):
//
//	offset 0:  4-byte scratch for fd_write's nwritten out-param (unused)
//	offset 8:  the iovec: iov_base=16 (i32 LE), iov_len=len(jsonOut) (i32 LE)
//	offset 16: jsonOut's bytes
func wasmHello(jsonOut string) []byte {
	const iovsPtr = 8
	const strPtr = 16
	const nwrittenPtr = 0

	types := wsection(1, wvec(
		functype([]byte{valI32, valI32, valI32, valI32}, []byte{valI32}), // 0: fd_write(i32,i32,i32,i32)->i32
		functype(nil, nil), // 1: _start()->()
	))
	imports := wsection(2, wvec(
		append(append(wname("wasi_snapshot_preview1"), wname("fd_write")...), 0x00, 0x00), // func import, typeidx 0
	))
	functions := wsection(3, wvec(uleb128(1))) // _start uses type 1
	memories := wsection(5, wvec(append([]byte{0x00}, uleb128(1)...)))
	exports := wsection(7, wvec(
		append(wname("memory"), 0x02, 0x00),
		append(wname("_start"), 0x00, 0x01), // funcidx 1 (0 is the fd_write import)
	))

	var body []byte
	body = append(body, 0x00) // 0 local decl groups
	body = append(body, 0x41)
	body = append(body, sleb128(1)...) // i32.const 1 (fd = stdout)
	body = append(body, 0x41)
	body = append(body, sleb128(iovsPtr)...) // i32.const iovsPtr
	body = append(body, 0x41)
	body = append(body, sleb128(1)...) // i32.const 1 (iovs_len)
	body = append(body, 0x41)
	body = append(body, sleb128(nwrittenPtr)...) // i32.const nwrittenPtr
	body = append(body, 0x10, 0x00)              // call 0 (fd_write)
	body = append(body, 0x1a)                    // drop the errno result
	body = append(body, 0x0b)                    // end
	code := wsection(10, wvec(append(uleb128(uint32(len(body))), body...)))

	iovec := make([]byte, 8)
	binary.LittleEndian.PutUint32(iovec[0:4], strPtr)
	binary.LittleEndian.PutUint32(iovec[4:8], uint32(len(jsonOut)))
	dataBlob := append(iovec, []byte(jsonOut)...)
	offsetExpr := append([]byte{0x41}, append(sleb128(iovsPtr), 0x0b)...)
	dataSeg := append(uleb128(0), offsetExpr...) // flag 0: active, memory 0
	dataSeg = append(dataSeg, append(uleb128(uint32(len(dataBlob))), dataBlob...)...)
	data := wsection(11, wvec(dataSeg))

	var m []byte
	m = append(m, wasmHeader...)
	m = append(m, types...)
	m = append(m, imports...)
	m = append(m, functions...)
	m = append(m, memories...)
	m = append(m, exports...)
	m = append(m, code...)
	m = append(m, data...)
	return m
}

// wasmProcExit builds a module that imports wasi_snapshot_preview1.proc_exit
// and whose _start calls it with the given exit code, then returns — used
// to exercise execWASM's non-zero-exit-is-an-error path.
func wasmProcExit(code int32) []byte {
	types := wsection(1, wvec(
		functype([]byte{valI32}, nil), // 0: proc_exit(i32)->()
		functype(nil, nil),            // 1: _start()->()
	))
	imports := wsection(2, wvec(
		append(append(wname("wasi_snapshot_preview1"), wname("proc_exit")...), 0x00, 0x00),
	))
	functions := wsection(3, wvec(uleb128(1)))
	exports := wsection(7, wvec(
		append(wname("_start"), 0x00, 0x01),
	))
	var body []byte
	body = append(body, 0x00) // 0 locals
	body = append(body, 0x41)
	body = append(body, sleb128(code)...)
	body = append(body, 0x10, 0x00) // call 0 (proc_exit)
	body = append(body, 0x0b)       // end
	code_ := wsection(10, wvec(append(uleb128(uint32(len(body))), body...)))

	var m []byte
	m = append(m, wasmHeader...)
	m = append(m, types...)
	m = append(m, imports...)
	m = append(m, functions...)
	m = append(m, exports...)
	m = append(m, code_...)
	return m
}

// wasmInfiniteLoop builds a module with no imports whose _start is an
// unconditional infinite loop — used to prove ctx cancellation actually
// halts a running guest instead of wedging the plugin.
func wasmInfiniteLoop() []byte {
	types := wsection(1, wvec(functype(nil, nil))) // 0: _start()->()
	functions := wsection(3, wvec(uleb128(0)))
	exports := wsection(7, wvec(append(wname("_start"), 0x00, 0x00)))
	body := []byte{
		0x00,       // 0 locals
		0x03, 0x40, // loop (blocktype: empty)
		0x0c, 0x00, // br 0 (back to the loop)
		0x0b, // end (loop)
		0x0b, // end (function)
	}
	code := wsection(10, wvec(append(uleb128(uint32(len(body))), body...)))

	var m []byte
	m = append(m, wasmHeader...)
	m = append(m, types...)
	m = append(m, functions...)
	m = append(m, exports...)
	m = append(m, code...)
	return m
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ---- tests -----------------------------------------------------------

func TestDescribe(t *testing.T) {
	d := describe()
	if d.Kind != plugin.KindStep {
		t.Fatalf("Kind = %v, want %v", d.Kind, plugin.KindStep)
	}
	if d.ABI != plugin.EngineABI {
		t.Fatalf("ABI = %d, want %d", d.ABI, plugin.EngineABI)
	}
	if d.Type != "wasm" {
		t.Fatalf("Type = %q, want %q", d.Type, "wasm")
	}
	if d.Desc == "" {
		t.Fatal("Desc is empty")
	}
	if !reflect.DeepEqual(d.Capabilities, plugin.Capabilities{}) {
		t.Fatalf("Capabilities = %+v, want empty (no egress/fs/spawns)", d.Capabilities)
	}
}

func TestExecWASM_InvalidBase64(t *testing.T) {
	_, err := execWASM(context.Background(), plugin.RunRequest{Code: "not-valid-base64!!!"})
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("err = %v, want a base64 decode error", err)
	}
}

func TestExecWASM_NotAWasmModule(t *testing.T) {
	req := plugin.RunRequest{Code: b64([]byte("this is plain text, not a wasm module"))}
	_, err := execWASM(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "not a valid WASM module") {
		t.Fatalf("err = %v, want a compile error", err)
	}
}

func TestExecWASM_MissingStart(t *testing.T) {
	req := plugin.RunRequest{Code: b64(wasmEmpty()), Inputs: map[string]any{"a": 1}}
	out, err := execWASM(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v, want nil (a module with no _start just never runs)", err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %v, want {} (nothing ran, nothing was printed)", out)
	}
}

func TestExecWASM_HappyPath(t *testing.T) {
	req := plugin.RunRequest{
		Code:   b64(wasmHello(`{"ok":true,"n":2}`)),
		Inputs: map[string]any{"a": 1}, // fed to stdin; this guest ignores it and prints a fixed reply
	}
	out, err := execWASM(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := map[string]any{"ok": true, "n": float64(2)}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("out = %#v, want %#v", out, want)
	}
}

func TestExecWASM_HappyPath_ScalarIsValueOutput(t *testing.T) {
	req := plugin.RunRequest{Code: b64(wasmHello(`42`))}
	out, err := execWASM(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := map[string]any{"value": float64(42)}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("out = %#v, want %#v", out, want)
	}
}

func TestExecWASM_NonJSONStdoutIsEmptyOutputs(t *testing.T) {
	req := plugin.RunRequest{Code: b64(wasmHello(`not json at all`))}
	out, err := execWASM(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v, want nil: non-JSON stdout from a zero exit is documented as {} not an error", err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %v, want {}", out)
	}
}

func TestExecWASM_BlankStdoutIsEmptyOutputs(t *testing.T) {
	req := plugin.RunRequest{Code: b64(wasmHello(""))}
	out, err := execWASM(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %v, want {}", out)
	}
}

func TestExecWASM_NonZeroExitIsAnError(t *testing.T) {
	req := plugin.RunRequest{Code: b64(wasmProcExit(7))}
	_, err := execWASM(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "exited with code 7") {
		t.Fatalf("err = %v, want a non-zero exit error naming code 7", err)
	}
}

func TestExecWASM_ZeroExitIsNotAnError(t *testing.T) {
	req := plugin.RunRequest{Code: b64(wasmProcExit(0))}
	out, err := execWASM(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v, want nil: proc_exit(0) is a normal successful exit", err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %v, want {} (nothing printed)", out)
	}
}

// TestExecWASM_ContextCancellationHaltsGuest proves the step's `timeout:`
// actually cuts a runaway module: an unconditional infinite loop is run
// under a short-lived ctx, and execWASM must return (reporting the ctx
// error) well within the test's own deadline instead of hanging.
func TestExecWASM_ContextCancellationHaltsGuest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := execWASM(ctx, plugin.RunRequest{Code: b64(wasmInfiniteLoop())})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("err = nil, want a context/runtime-fault error from the halted loop")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execWASM did not return after ctx expired — CloseOnContextDone did not halt the guest")
	}
}

func TestExecWASM_ArgsAndEnvDoNotError(t *testing.T) {
	// wasmHello ignores argv/env entirely; this just proves passing them
	// through ModuleConfig doesn't itself break instantiation.
	req := plugin.RunRequest{
		Code: b64(wasmHello(`{"ok":true}`)),
		Args: []string{"--flag", "value"},
		Env:  map[string]string{"FOO": "bar"},
	}
	out, err := execWASM(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(out, map[string]any{"ok": true}) {
		t.Fatalf("out = %#v", out)
	}
}

// --- module loading: base64 (default) OR a .wasm file path ---

func TestExecWASM_FromFilePath(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/mod.wasm"
	if err := os.WriteFile(path, wasmHello(`{"from":"file"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// a bare existing path is read as the module
	out, err := execWASM(context.Background(), plugin.RunRequest{Code: path})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, map[string]any{"from": "file"}) {
		t.Fatalf("bare path: out = %#v", out)
	}
	// the file: prefix form works too
	out, err = execWASM(context.Background(), plugin.RunRequest{Code: "file:" + path})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, map[string]any{"from": "file"}) {
		t.Fatalf("file: prefix: out = %#v", out)
	}
}

func TestLoadModule(t *testing.T) {
	// base64 still works
	if _, err := loadModule(b64(wasmEmpty())); err != nil {
		t.Fatalf("base64: %v", err)
	}
	// a missing file: path is a clear read error, not a base64 error
	if _, err := loadModule("file:/no/such/module.wasm"); err == nil || !strings.Contains(err.Error(), "read module file") {
		t.Fatalf("missing file: err = %v", err)
	}
	// empty is rejected
	if _, err := loadModule("   "); err == nil {
		t.Fatal("empty code should error")
	}
}
