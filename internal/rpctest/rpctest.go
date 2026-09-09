// Package rpctest is a minimal CLIENT for conductor's plugin protocol, for
// this repo's end-to-end tests.
//
// The daemon's own client lives in conductor's internal/plugin and is therefore
// not importable from here (that is the point — a plugin must not need
// conductor internals). So these tests drive the plugin binaries over the same
// wire the daemon uses: newline-delimited JSON-RPC 2.0 on the subprocess's
// stdin/stdout, with plugin.describe / plugin.invoke / plugin.start_source
// requests and plugin.event notifications, using the request/response types
// from the PUBLIC SDK (github.com/NodeSpy/conductor/pkg/plugin) so the schema
// stays a single source of truth.
//
// What this proves and what it does not: it proves each plugin speaks the
// protocol correctly and that its verb/source behaviour is right. It does NOT
// exercise the daemon's client, sandbox, or sha256 verify-before-execute —
// those are conductor's own tests (internal/plugin, against the in-repo
// test/plugins/acme-* reference plugins).
package rpctest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// Build compiles one plugin component of this repo into a temp dir and returns
// the binary path. Skips the test when there is no go toolchain.
func Build(t *testing.T, component string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	bin := filepath.Join(t.TempDir(), "conductor-"+component)
	cmd := exec.Command("go", "build", "-o", bin, "github.com/NodeSpy/conductor-plugins/cmd/conductor-"+component)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build conductor-%s: %v\n%s", component, err, out)
	}
	return bin
}

// wireMessage is the JSON-RPC 2.0 envelope the SDK's Serve loop reads/writes.
type wireMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *plugin.Error    `json:"error,omitempty"`
}

// Client is a live plugin subprocess plus the framing around it.
type Client struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	encM sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[string]chan wireMessage
	emit    func(json.RawMessage)

	closeOnce sync.Once
}

// Start spawns the plugin binary and begins reading its stdout. Stderr is
// forwarded to the test log by the caller's choice of os.Stderr (plugins log
// there; stdout is the transport and must stay clean).
func Start(t *testing.T, bin string) *Client {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	c := &Client{
		cmd: cmd, in: stdin, out: bufio.NewReader(stdout),
		pending: map[string]chan wireMessage{},
	}
	go c.read()
	t.Cleanup(c.Close)
	return c
}

// read demultiplexes the plugin's stdout: id'd responses go to the waiting
// caller, plugin.event notifications go to the emit sink.
func (c *Client) read() {
	dec := json.NewDecoder(c.out)
	for {
		var m wireMessage
		if err := dec.Decode(&m); err != nil {
			c.mu.Lock()
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		if m.Method == plugin.MethodEvent {
			c.mu.Lock()
			sink := c.emit
			c.mu.Unlock()
			if sink != nil {
				sink(m.Params)
			}
			continue
		}
		if m.ID == nil {
			continue
		}
		key := string(*m.ID)
		c.mu.Lock()
		ch, ok := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if ok {
			ch <- m
		}
	}
}

// call sends one request and waits for its response.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	idRaw := json.RawMessage(fmt.Sprintf("%d", id))
	ch := make(chan wireMessage, 1)
	c.pending[string(idRaw)] = ch
	c.mu.Unlock()

	msg := wireMessage{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: raw}
	line, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	c.encM.Lock()
	_, err = c.in.Write(append(line, '\n'))
	c.encM.Unlock()
	if err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: plugin exited before responding", method)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: plugin error %d: %s", method, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// Describe issues plugin.describe.
func (c *Client) Describe(ctx context.Context) (*plugin.Decl, error) {
	raw, err := c.call(ctx, plugin.MethodDescribe, struct{}{})
	if err != nil {
		return nil, err
	}
	var d plugin.Decl
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Invoke issues plugin.invoke and returns the verb's outputs.
func (c *Client) Invoke(ctx context.Context, req plugin.InvokeRequest) (map[string]any, error) {
	raw, err := c.call(ctx, plugin.MethodInvoke, req)
	if err != nil {
		return nil, err
	}
	var res plugin.InvokeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Outputs, nil
}

// StartSource issues plugin.start_source and routes the plugin's subsequent
// plugin.event notifications to emit.
func (c *Client) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(json.RawMessage)) error {
	c.mu.Lock()
	c.emit = emit
	c.mu.Unlock()
	_, err := c.call(ctx, plugin.MethodStartSource, req)
	return err
}

// Close shuts the plugin down the way the daemon does: closes its stdin (the
// SDK's Serve loop returns on EOF) and reaps the process.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		_ = c.in.Close()
		done := make(chan struct{})
		go func() { _, _ = c.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = c.cmd.Process.Kill()
		}
	})
}

// EventSink collects streamed source events and signals arrivals.
type EventSink struct {
	mu     sync.Mutex
	events []map[string]any
	ch     chan struct{}
}

// NewEventSink builds a sink usable as the StartSource emit callback.
func NewEventSink() *EventSink {
	return &EventSink{ch: make(chan struct{}, 64)}
}

// Emit is the StartSource callback.
func (s *EventSink) Emit(raw json.RawMessage) {
	var ev map[string]any
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// Wait blocks until at least one event has arrived or ctx is done.
func (s *EventSink) Wait(ctx context.Context) error {
	if s.Len() > 0 {
		return nil
	}
	select {
	case <-s.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Len is the number of events received so far.
func (s *EventSink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// At returns the i'th received event.
func (s *EventSink) At(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events[i]
}
