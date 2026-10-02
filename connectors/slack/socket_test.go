package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	emit, got := collect()
	err := s.runOnce(context.Background(), emit)
	if err == nil || !strings.Contains(err.Error(), "disconnect: refresh_requested") {
		t.Fatalf("session should end on the disconnect frame, got %v", err)
	}
	if len(*got) != 1 || (*got)[0].Event != "app_mention" {
		t.Fatalf("mention not emitted: %+v", *got)
	}
	if acked.Load() != 1 {
		t.Fatal("envelope was not ACKed")
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
