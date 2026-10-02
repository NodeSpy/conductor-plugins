package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// ---- parseSmeeFrame (ported shape from conductor's pre-contract-cutover
// internal/inbound/smee.go tests) ----

func TestParseSmeeFrameObjectBody(t *testing.T) {
	data := `{"host":"smee.io","content-type":"application/json","x-github-event":"push","body":{"a":1},"query":{}}`
	headers, body, ok := parseSmeeFrame(data)
	if !ok {
		t.Fatal("expected ok")
	}
	if headers["x-github-event"] != "push" || headers["content-type"] != "application/json" {
		t.Fatalf("headers: %v", headers)
	}
	if _, present := headers["query"]; present {
		t.Fatalf("query must not become a header: %v", headers)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil || decoded["a"] != float64(1) {
		t.Fatalf("body: %s (%v)", body, err)
	}
}

func TestParseSmeeFrameStringBody(t *testing.T) {
	data := `{"content-type":"text/plain","body":"hello world"}`
	_, body, ok := parseSmeeFrame(data)
	if !ok || string(body) != "hello world" {
		t.Fatalf("body: %q ok=%v", body, ok)
	}
}

func TestParseSmeeFrameNoBodyIsSkipped(t *testing.T) {
	// smee's own "ready"/ping control frames carry no body.
	if _, _, ok := parseSmeeFrame(`{"ready":true}`); ok {
		t.Fatal("a frame with no body must not be accepted")
	}
}

func TestParseSmeeFrameNotJSON(t *testing.T) {
	if _, _, ok := parseSmeeFrame(`not json`); ok {
		t.Fatal("expected ok=false for non-JSON data")
	}
}

// ---- createChannel ----

func TestCreateChannelRelativeLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/new" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Location", "/abc123")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	got, err := createChannel(&http.Client{Timeout: 5 * time.Second}, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got != srv.URL+"/abc123" {
		t.Fatalf("got %q, want %q", got, srv.URL+"/abc123")
	}
}

func TestCreateChannelAbsoluteLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://smee.io/xyz789")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	got, err := createChannel(&http.Client{Timeout: 5 * time.Second}, srv.URL)
	if err != nil || got != "https://smee.io/xyz789" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

func TestCreateChannelNoLocationErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	if _, err := createChannel(&http.Client{Timeout: 5 * time.Second}, srv.URL); err == nil {
		t.Fatal("expected error for a missing Location header")
	}
}

func TestCreateChannelBadStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := createChannel(&http.Client{Timeout: 5 * time.Second}, srv.URL); err == nil {
		t.Fatal("expected error for a non-redirect status")
	}
}

// ---- end-to-end: a fake smee.io (creates a channel, streams one SSE frame)
// and a fake local listener (captures the replayed POST) ----

func fakeSmeeServer(t *testing.T, frames <-chan string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/chan1", http.StatusFound)
	})
	mux.HandleFunc("/chan1", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseRecorder does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", f)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	return httptest.NewServer(mux)
}

func TestOpenRelaysDeliveriesAndCloseStopsIt(t *testing.T) {
	frames := make(chan string, 4)
	smeeSrv := fakeSmeeServer(t, frames)
	defer smeeSrv.Close()

	var mu sync.Mutex
	var gotHeader, gotBody string
	var count int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotHeader = r.Header.Get("X-Test-Event")
		gotBody = string(b)
		mu.Unlock()
		atomic.AddInt32(&count, 1)
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	p := newSmeePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Instance: "src", Verb: "open",
		Options:    map[string]any{"local_addr": targetAddr},
		Connection: map[string]any{"smee_base": smeeSrv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["public_url"] != smeeSrv.URL+"/chan1" {
		t.Fatalf("public_url: %v", res.Outputs["public_url"])
	}

	frames <- `{"x-test-event":"push","body":"hello"}`
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&count) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	h, b := gotHeader, gotBody
	mu.Unlock()
	if h != "push" || b != "hello" {
		t.Fatalf("relayed request: header=%q body=%q", h, b)
	}

	lease, _ := res.Outputs["lease"].(string)
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "close", Options: map[string]any{"lease": lease}}); err != nil {
		t.Fatal(err)
	}
	if p.leases.Len() != 0 {
		t.Fatalf("leases after close = %d, want 0", p.leases.Len())
	}

	// A frame sent after close must not be relayed.
	before := atomic.LoadInt32(&count)
	close(frames)
	time.Sleep(200 * time.Millisecond)
	if atomic.LoadInt32(&count) != before {
		t.Fatal("a delivery arrived after close")
	}
}

func TestOpenWithPinnedChannelSkipsCreation(t *testing.T) {
	var newHit int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/new" {
			atomic.AddInt32(&newHit, 1)
		}
		<-r.Context().Done() // the relay's GET just hangs; this test only checks /new wasn't hit
	}))
	defer srv.Close()

	p := newSmeePlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"},
		Connection: map[string]any{"channel": srv.URL + "/pinned"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.leases.Release(res.Outputs["lease"].(string))
	if res.Outputs["public_url"] != srv.URL+"/pinned" {
		t.Fatalf("public_url: %v", res.Outputs["public_url"])
	}
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&newHit) != 0 {
		t.Fatal("a pinned channel must not call /new")
	}
}

func TestOpenBadLocalAddrErrors(t *testing.T) {
	p := newSmeePlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "bad"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenCreateChannelFailureErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := newSmeePlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"smee_base": srv.URL}})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestUnknownVerbErrors(t *testing.T) {
	p := newSmeePlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "bogus"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestStopReleasesOnlyThatInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/new" {
			http.Redirect(w, r, "/c", http.StatusFound)
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	p := newSmeePlugin()
	defer p.Stop(context.Background(), plugin.StopRequest{Instance: "b"}) //nolint:errcheck
	for _, inst := range []string{"a", "b"} {
		if _, err := p.Invoke(plugin.InvokeRequest{Instance: inst, Verb: "open", Options: map[string]any{"local_addr": "127.0.0.1:8099"}, Connection: map[string]any{"smee_base": srv.URL}}); err != nil {
			t.Fatal(err)
		}
	}
	if p.leases.Len() != 2 {
		t.Fatalf("leases = %d, want 2", p.leases.Len())
	}
	if err := p.Stop(context.Background(), plugin.StopRequest{Instance: "a"}); err != nil {
		t.Fatal(err)
	}
	if p.leases.Len() != 1 {
		t.Fatalf("leases after stop a = %d, want 1", p.leases.Len())
	}
}

func TestDeclarationsAreValid(t *testing.T) {
	d := newSmeePlugin().Describe()
	d.ProtocolVersion = plugin.ProtocolVersion
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if p := plugin.CheckSemantics(raw); len(p) > 0 {
		t.Fatalf("CheckSemantics: %v", p)
	}
	if p := plugin.ValidateSemantics(d); len(p) > 0 {
		t.Fatalf("ValidateSemantics: %v", p)
	}
}

// ---- host.state persistence, over the real wire (plugin.ServeConn), with a
// minimal hand-rolled host side that actually stores values — proving a
// CREATED channel survives a plugin restart (a fresh smeePlugin with no
// in-memory state of its own) instead of minting a new one every time. ----

type wireMsg struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *plugin.Error    `json:"error,omitempty"`
}

// fakeHost drives a Handler over an in-process pipe, like plugintest does,
// but additionally answers host.state with a REAL in-memory store (keyed by
// instance+key) so persistence across two separate Handler instances (two
// "processes") can be proven.
type fakeHost struct {
	mu      sync.Mutex
	in      io.Writer
	nextID  int
	pending map[string]chan wireMsg
	store   map[string]any // shared across "restarts" in a test, like a real daemon's state
}

func newFakeHost(store map[string]any) *fakeHost {
	return &fakeHost{pending: map[string]chan wireMsg{}, store: store}
}

func (f *fakeHost) start(h plugin.Handler) (call func(method string, params any) (json.RawMessage, error), closeFn func()) {
	toPlugin, hostW := io.Pipe()
	hostR, fromPlugin := io.Pipe()
	f.in = hostW
	done := make(chan struct{})
	go func() { _ = plugin.ServeConn(toPlugin, fromPlugin, h); close(done) }()
	go f.read(hostR)
	call = func(method string, params any) (json.RawMessage, error) {
		return f.call(method, params)
	}
	closeFn = func() {
		_ = hostW.Close()
		<-done
	}
	return call, closeFn
}

func (f *fakeHost) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		var m wireMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch {
		case m.Method == plugin.MethodHostState && m.ID != nil:
			f.serveState(m)
		case m.ID != nil:
			key := strings.Trim(string(*m.ID), `"`)
			f.mu.Lock()
			ch := f.pending[key]
			f.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
}

func (f *fakeHost) serveState(m wireMsg) {
	var req plugin.HostStateRequest
	_ = json.Unmarshal(m.Params, &req)
	res := plugin.HostStateResult{OK: true}
	key := req.Instance + "\x00" + req.Key
	f.mu.Lock()
	switch req.Op {
	case "get":
		res.Value = f.store[key]
	case "put":
		f.store[key] = req.Value
	case "delete":
		delete(f.store, key)
	}
	f.mu.Unlock()
	b, _ := json.Marshal(res)
	reply := wireMsg{JSONRPC: "2.0", ID: m.ID, Result: b}
	line, _ := json.Marshal(reply)
	f.mu.Lock()
	_, _ = f.in.Write(append(line, '\n'))
	f.mu.Unlock()
}

func (f *fakeHost) call(method string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	f.mu.Lock()
	f.nextID++
	id := json.RawMessage(fmt.Sprintf("%d", f.nextID))
	ch := make(chan wireMsg, 1)
	f.pending[string(id)] = ch
	msg := wireMsg{JSONRPC: "2.0", ID: &id, Method: method, Params: raw}
	line, _ := json.Marshal(msg)
	w := f.in
	f.mu.Unlock()
	if _, err := w.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("%s: no response", method)
	}
}

func invokeOpen(t *testing.T, call func(string, any) (json.RawMessage, error), localAddr string, conn map[string]any) map[string]any {
	t.Helper()
	raw, err := call(plugin.MethodInvoke, plugin.InvokeRequest{
		Instance: "src", Verb: "open",
		Options:    map[string]any{"local_addr": localAddr},
		Connection: conn,
	})
	if err != nil {
		t.Fatal(err)
	}
	var res plugin.InvokeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res.Outputs
}

func TestChannelPersistsAcrossRestartViaHostState(t *testing.T) {
	var newCalls int32
	smeeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/new" {
			atomic.AddInt32(&newCalls, 1)
			http.Redirect(w, r, "/persisted-chan", http.StatusFound)
			return
		}
		// The relay's long-lived GET; never answered in this test, but must
		// unblock when the request's connection closes so Server.Close
		// (deferred below) doesn't hang waiting on it.
		<-r.Context().Done()
	}))
	defer smeeSrv.Close()

	store := map[string]any{}

	// "Process" 1: a fresh smeePlugin, no channel pinned — it must create
	// one and persist it.
	p1 := newSmeePlugin()
	h1 := newFakeHost(store)
	call1, close1 := h1.start(p1)
	out1 := invokeOpen(t, call1, "127.0.0.1:8099", map[string]any{"smee_base": smeeSrv.URL})
	if out1["public_url"] != smeeSrv.URL+"/persisted-chan" {
		t.Fatalf("public_url: %v", out1["public_url"])
	}
	p1.leases.StopInstance("src")
	close1()
	if atomic.LoadInt32(&newCalls) != 1 {
		t.Fatalf("new_calls after first open = %d, want 1", newCalls)
	}

	// "Process" 2: a BRAND NEW smeePlugin (simulating a restart — no
	// in-memory state carries over), talking to a NEW fakeHost wired to the
	// SAME store (simulating the daemon's own state surviving the plugin
	// restart). It must reuse the persisted channel, not mint a second one.
	p2 := newSmeePlugin()
	h2 := newFakeHost(store)
	call2, close2 := h2.start(p2)
	defer close2()
	out2 := invokeOpen(t, call2, "127.0.0.1:8099", map[string]any{"smee_base": smeeSrv.URL})
	defer p2.leases.StopInstance("src")
	if out2["public_url"] != smeeSrv.URL+"/persisted-chan" {
		t.Fatalf("public_url after restart: %v", out2["public_url"])
	}
	if atomic.LoadInt32(&newCalls) != 1 {
		t.Fatalf("new_calls after restart = %d, want still 1 (channel should have been reused)", newCalls)
	}
}
