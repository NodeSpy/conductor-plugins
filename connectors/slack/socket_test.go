package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

func TestOpenSocket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer xapp-1" {
			t.Errorf("auth header: %q", got)
		}
		fmt.Fprint(w, `{"ok":true,"url":"wss://socket.example/x"}`)
	}))
	defer srv.Close()
	api := newSlackAPI("", "xapp-1", srv.URL)

	url, err := api.openSocket(context.Background())
	if err != nil || url != "wss://socket.example/x" {
		t.Fatalf("openSocket: %q %v", url, err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	defer bad.Close()
	api2 := newSlackAPI("", "xapp-1", bad.URL)
	if _, err := api2.openSocket(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("api error: %v", err)
	}

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `nope`)
	}))
	defer junk.Close()
	api3 := newSlackAPI("", "xapp-1", junk.URL)
	if _, err := api3.openSocket(context.Background()); err == nil {
		t.Fatal("bad json should error")
	}
}

// socketServer runs a Socket-Mode-shaped websocket endpoint: hello, one
// app_mention events_api envelope (expecting the ACK back), then disconnect.
func socketServer(t *testing.T, acked *atomic.Int64) *httptest.Server {
	t.Helper()
	payload := `{"event":{"type":"app_mention","text":"<@U0> fix it","user":"U1","channel":"C1","ts":"1.2"}}`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"hello"}`))
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"events_api","envelope_id":"env-1","payload":`+payload+`}`))
		if _, data, err := c.Read(ctx); err == nil {
			var ack struct {
				EnvelopeID string `json:"envelope_id"`
			}
			if json.Unmarshal(data, &ack) == nil && ack.EnvelopeID == "env-1" {
				acked.Add(1)
			}
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"disconnect","reason":"refresh_requested"}`))
	}))
}

func TestRunOnceSocketSession(t *testing.T) {
	var acked atomic.Int64
	ws := socketServer(t, &acked)
	defer ws.Close()
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":true,"url":%q}`, ws.URL)
	}))
	defer open.Close()

	s := newSource(t, []plugin.SourceTrigger{testTrigger(t, "t1", "app_mention", nil, nil)})
	s.api = newSlackAPI("xoxb-1", "xapp-1", open.URL)
	// An events_api envelope's dispatch now runs on a goroutine, started
	// after its ACK (finding #9) — runOnce can return (on the "disconnect"
	// frame sent right after) before that goroutine's emit happens. A
	// channel is the synchronization point: collect() is a plain slice and
	// reading it right after runOnce returns, with no happens-before edge to
	// the emitting goroutine, is a data race, not just a timing risk.
	emitted := make(chan plugin.SourceEvent, 4)
	err := s.runOnce(context.Background(), func(se plugin.SourceEvent) error { emitted <- se; return nil })
	if err == nil || !strings.Contains(err.Error(), "disconnect: refresh_requested") {
		t.Fatalf("session should end on the disconnect frame, got %v", err)
	}
	select {
	case se := <-emitted:
		if se.Event != "app_mention" {
			t.Fatalf("mention not emitted: %+v", se)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mention event was never emitted — async dispatch did not run")
	}
	if acked.Load() != 1 {
		t.Fatal("envelope was not ACKed")
	}
}

// TestSlowHandlerDoesNotDelayTheNextEnvelopesACK is the regression test for
// finding #9. Before the fix, an events_api/interactive envelope's dispatch
// (which can make a blocking Web API call — offerForms' chat.postEphemeral
// here) ran INLINE in the read loop, so one slow call delayed reading, and so
// ACKing, every envelope behind it — including one racing Slack's 3s
// trigger_id window. The fix runs that dispatch on a goroutine after its own
// ACK, so a second envelope arriving right behind a slow one must still be
// ACKed promptly.
func TestSlowHandlerDoesNotDelayTheNextEnvelopesACK(t *testing.T) {
	const slowCall = 300 * time.Millisecond
	const ackBudget = 150 * time.Millisecond // comfortably under slowCall

	var wsURL string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apps.connections.open":
			fmt.Fprintf(w, `{"ok":true,"url":%q}`, wsURL)
		case "/chat.postEphemeral":
			time.Sleep(slowCall) // offerForms' blocking call, deliberately slow
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer api.Close()

	secondEnvelopeSentAt := make(chan time.Time, 1)
	secondEnvelopeACKed := make(chan time.Time, 1)
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"hello"}`))
		// Envelope #1: a form-carrying app_mention — its dispatch calls
		// offerForms, which hits the slow /chat.postEphemeral above.
		formMention := `{"event":{"type":"app_mention","text":"deploy it","user":"U1","channel":"C1","ts":"1.1"}}`
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"events_api","envelope_id":"env-1","payload":`+formMention+`}`))
		// Envelope #2, sent right behind it with no delay: a plain reaction,
		// whose own dispatch does nothing slow.
		reaction := `{"event":{"type":"reaction_added","reaction":"eyes","user":"U2","item":{"channel":"C1","ts":"2.2"}}}`
		secondEnvelopeSentAt <- time.Now()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"events_api","envelope_id":"env-2","payload":`+reaction+`}`))
		for i := 0; i < 2; i++ {
			if _, data, err := c.Read(ctx); err == nil {
				var ack struct {
					EnvelopeID string `json:"envelope_id"`
				}
				if json.Unmarshal(data, &ack) == nil && ack.EnvelopeID == "env-2" {
					secondEnvelopeACKed <- time.Now()
				}
			}
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"disconnect","reason":"done"}`))
	}))
	defer ws.Close()
	wsURL = ws.URL

	s := newSource(t, []plugin.SourceTrigger{
		testTrigger(t, "t1", "app_mention", map[string]any{"users": []any{"U1"}}, mentionForm()),
	})
	s.api = newSlackAPI("xoxb-1", "xapp-1", api.URL)
	emit, _ := collect()
	done := make(chan error, 1)
	go func() { done <- s.runOnce(context.Background(), emit) }()

	var sentAt, ackedAt time.Time
	select {
	case sentAt = <-secondEnvelopeSentAt:
	case <-time.After(2 * time.Second):
		t.Fatal("second envelope was never sent")
	}
	select {
	case ackedAt = <-secondEnvelopeACKed:
	case <-time.After(2 * time.Second):
		t.Fatal("second envelope was never ACKed")
	}
	if elapsed := ackedAt.Sub(sentAt); elapsed > ackBudget {
		t.Fatalf("second envelope's ACK took %s (budget %s) — the slow handler for the FIRST envelope blocked it", elapsed, ackBudget)
	}
	<-done
}

// TestAckWriteErrorIsLogged is the regression test for finding #12: a failed
// ACK write (the connection is already gone) must be logged, not silently
// swallowed — an operator debugging missed ACKs has nothing to go on
// otherwise.
// fakeWSConn is a deterministic wsConn: it hands pumpSocket one message per
// Read call from a fixed script, and Write always fails — unlike a real
// websocket, which can't reliably be made to fail a specific Write on
// command (its Close handshake, or even CloseNow, races the peer's next
// write rather than guaranteeing it fails).
type fakeWSConn struct {
	messages [][]byte
	n        int
	writeErr error
}

func (f *fakeWSConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	if f.n >= len(f.messages) {
		return 0, nil, errors.New("fakeWSConn: no more scripted messages")
	}
	m := f.messages[f.n]
	f.n++
	return websocket.MessageText, m, nil
}

func (f *fakeWSConn) Write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	return f.writeErr
}

// TestAckWriteErrorIsLogged is the regression test for finding #12: a failed
// ACK write must be logged, not silently swallowed — an operator debugging
// missed ACKs has nothing to go on otherwise.
func TestAckWriteErrorIsLogged(t *testing.T) {
	var logBuf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(orig)

	s := newSource(t, nil)
	payload := `{"event":{"type":"reaction_added","reaction":"eyes","user":"U1","item":{"channel":"C1","ts":"1.1"}}}`
	fake := &fakeWSConn{
		messages: [][]byte{[]byte(`{"type":"events_api","envelope_id":"env-1","payload":` + payload + `}`)},
		writeErr: errors.New("write: broken pipe"),
	}
	emit, _ := collect()
	_ = s.pumpSocket(context.Background(), fake, emit) // expected to error (script runs out); only the log matters here

	if !strings.Contains(logBuf.String(), "ack envelope env-1") || !strings.Contains(logBuf.String(), "broken pipe") {
		t.Fatalf("a failed ACK write was not logged: %q", logBuf.String())
	}
}

// TestStartReconnects: a failed connect backs off, reconnects, pumps a
// session, and cancel stops the loop.
func TestStartReconnects(t *testing.T) {
	var acked, opens atomic.Int64
	ws := socketServer(t, &acked)
	defer ws.Close()
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opens.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"url":%q}`, ws.URL)
	}))
	defer open.Close()

	s := newSource(t, []plugin.SourceTrigger{testTrigger(t, "t1", "app_mention", nil, nil)})
	s.api = newSlackAPI("xoxb-1", "xapp-1", open.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emitted := make(chan plugin.SourceEvent, 4)
	done := make(chan error, 1)
	go func() {
		done <- s.Start(ctx, func(se plugin.SourceEvent) error { emitted <- se; return nil })
	}()
	select {
	case <-emitted:
	case <-time.After(10 * time.Second):
		t.Fatal("no event after reconnect")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start should return ctx.Err(), got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop")
	}
	if opens.Load() < 2 {
		t.Fatalf("expected a reconnect, opens=%d", opens.Load())
	}
}

// TestStartSourceRejectsNoAppToken proves the plugin refuses to start a
// Socket Mode source with no app_token, the same guard the builtin had.
func TestStartSourceRejectsNoAppToken(t *testing.T) {
	p := New()
	err := p.StartSource(context.Background(), plugin.StartSourceRequest{
		Instance: "x", Config: map[string]any{"bot_token": "xoxb-1"},
		Triggers: []plugin.SourceTrigger{{ID: "t1", Event: "app_mention"}},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected an error with no app_token")
	}
}

// TestStopEndsOneInstanceOnly proves plugin.stop on one instance cancels only
// that instance's Socket Mode connection, not a sibling's.
func TestStopEndsOneInstanceOnly(t *testing.T) {
	// Each instance's socket must stay open for the duration of the test, so
	// (unlike socketServer, which serves one envelope then disconnects) this
	// fake holds the connection until the test tears it down.
	hold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello"}`))
		<-r.Context().Done()
	}))
	defer hold.Close()
	openHold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":true,"url":%q}`, hold.URL)
	}))
	defer openHold.Close()

	p := New()
	done := make(chan error, 2)
	ctx := context.Background()
	for _, inst := range []string{"a", "b"} {
		inst := inst
		go func() {
			done <- p.StartSource(ctx, plugin.StartSourceRequest{
				Instance: inst, Config: map[string]any{"app_token": "xapp-1", "bot_token": "xoxb-1", "api_base": openHold.URL},
			}, func(any) error { return nil })
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := len(p.sources)
		p.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.Stop(ctx, plugin.StopRequest{Instance: "a"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stopped instance should return context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not end instance a's source")
	}
	p.mu.Lock()
	_, bStillRunning := p.sources["b"]
	p.mu.Unlock()
	if !bStillRunning {
		t.Fatal("stopping instance a must not affect instance b")
	}
}
