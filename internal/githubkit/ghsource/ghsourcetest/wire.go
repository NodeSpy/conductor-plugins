package ghsourcetest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// WireTriggers is a case's triggers as a plugin receives them on
// start_source — ids, events, options, and filters in the structural form —
// plus the id → name map a host routes events back by.
func WireTriggers(t *testing.T, ts []Trigger) ([]plugin.SourceTrigger, map[string]string) {
	t.Helper()
	out := make([]plugin.SourceTrigger, 0, len(ts))
	names := map[string]string{}
	for i, tr := range ts {
		id := fmt.Sprintf("%d:gh.%s", i, tr.On)
		st := plugin.SourceTrigger{ID: id, Name: tr.Name, Event: tr.On, Options: tr.Options}
		if tr.Filter != nil {
			f, err := sourcekit.ParseFilter(tr.Filter)
			if err != nil {
				t.Fatalf("trigger %s filter: %v", tr.Name, err)
			}
			b, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			st.Filter = b
		}
		out = append(out, st)
		names[id] = tr.Name
	}
	return out, names
}

// WireStarter drives a plugin BINARY over the bare SDK protocol, as a minimal
// daemon would: describe start_source with the
// case's connection and triggers, collect plugin.event notifications, answer
// a nudge with plugin.nudge. A routed event is mapped back to its trigger's
// name; its target-trust claim is taken at its word (there is no operator
// here to grant or withhold trust — conductor's own daemon-path driver tests
// that).
func WireStarter(bin string) Starter {
	return func(t *testing.T, c Case, env Env) Driver {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, bin)
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
			t.Fatalf("start plugin: %v", err)
		}
		w := &wire{in: stdin, pending: map[string]chan rpcMsg{}, cancel: cancel, cmd: cmd}
		triggers, names := WireTriggers(t, c.Triggers)
		w.names = names
		go w.read(stdout)

		var decl plugin.Decl
		if err := w.call("plugin.describe", struct{}{}, &decl); err != nil {
			t.Fatalf("describe: %v", err)
		}
		req := plugin.StartSourceRequest{Instance: "gh", Config: c.Connection(env), Triggers: triggers}
		if err := w.call(plugin.MethodStartSource, req, nil); err != nil {
			t.Fatalf("start_source: %v", err)
		}
		return w
	}
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *plugin.Error   `json:"error,omitempty"`
}

type wire struct {
	in     io.WriteCloser
	cancel context.CancelFunc
	cmd    *exec.Cmd
	names  map[string]string

	mu      sync.Mutex
	next    int
	pending map[string]chan rpcMsg
	events  []Got
}

func (w *wire) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method == plugin.MethodEvent {
			var ev plugin.SourceEvent
			if json.Unmarshal(m.Params, &ev) == nil {
				w.mu.Lock()
				w.events = append(w.events, Got{
					Kind: ev.Kind, Trigger: w.names[ev.Trigger], Repo: ev.Target.Repo, Number: ev.Target.Number,
					CatchUp: ev.CatchUp, TargetTrusted: ev.TargetTrusted, Context: ev.Context,
				})
				w.mu.Unlock()
			}
			continue
		}
		if len(m.ID) > 0 {
			w.mu.Lock()
			ch := w.pending[string(m.ID)]
			delete(w.pending, string(m.ID))
			w.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
}

func (w *wire) call(method string, params, result any) error {
	w.mu.Lock()
	w.next++
	id := strconv.Itoa(w.next)
	ch := make(chan rpcMsg, 1)
	w.pending[id] = ch
	w.mu.Unlock()
	raw, _ := json.Marshal(params)
	line, _ := json.Marshal(rpcMsg{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: raw})
	if _, err := w.in.Write(append(line, '\n')); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("%s: no response", method)
	}
}

func (w *wire) Nudge(t *testing.T) {
	t.Helper()
	var res plugin.PollResult
	if err := w.call(plugin.MethodPoll, plugin.PollRequest{Instance: "gh", Mode: plugin.PollNow}, &res); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

func (w *wire) Events() []Got {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Got(nil), w.events...)
}

func (w *wire) Close() {
	_ = w.in.Close()
	w.cancel()
	_ = w.cmd.Wait()
}
