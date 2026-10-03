package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/NodeSpy/conductor/pkg/plugintest"
)

// TestConformance drives the plugin's Handler over the REAL wire (the same
// transport a spawned binary speaks), using pkg/plugintest's host-side
// driver: plugin.describe (checked for must-understand semantics, exactly as
// a real host would refuse a bad declaration), plugin.start_source with a
// configured trigger, then a Socket Mode mention routed back as one
// plugin.event. Socket Mode has no webhook delivery for
// plugintest.RunCase's Step/Deliver model, so this drives the Starter
// directly instead of through Run/RunCase — the wire and Driver plumbing
// (describe, semantics checks, start_source, event collection) are exactly
// what a webhook-based suite reuses.
func TestConformance(t *testing.T) {
	api, wsURL := fakeSocketModeServer(t)

	suite := plugintest.Suite{Instance: "src"}
	env := plugintest.Env{Upstream: api.URL}
	c := plugintest.Case{
		Connection: func(e plugintest.Env) map[string]any {
			return map[string]any{"app_token": "xapp-1", "bot_token": "xoxb-1", "api_base": e.Upstream}
		},
		Triggers: []plugintest.Trigger{{On: "app_mention", Name: "mention"}},
	}
	start := plugintest.HandlerStarter(New())
	d := start(t, suite, c, env)
	defer d.Close()

	_ = wsURL
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(d.Events()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	got := d.Events()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(got), got)
	}
	if got[0].Event != "app_mention" {
		t.Fatalf("event = %q", got[0].Event)
	}
	if got[0].Trigger != "mention" {
		t.Fatalf("trigger = %q, want the routed trigger name \"mention\"", got[0].Trigger)
	}
	if got[0].Target != "slack:C1:1.1" {
		t.Fatalf("target = %q", got[0].Target)
	}
	if !got[0].Assigned {
		t.Fatal("a slack event's target must be assigned (Slack chose the channel/ts)")
	}
}

// fakeSocketModeServer returns an httptest server answering
// apps.connections.open with a websocket URL that, once dialed, sends hello
// then one app_mention envelope and holds the connection open.
func fakeSocketModeServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	var wsSrv *httptest.Server
	wsSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"hello"}`))
		payload := `{"event":{"type":"app_mention","text":"fix it","user":"U1","channel":"C1","ts":"1.1"}}`
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"events_api","envelope_id":"env-1","payload":`+payload+`}`))
		<-ctx.Done()
	}))
	t.Cleanup(wsSrv.Close)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps.connections.open" {
			w.Write([]byte(`{"ok":true,"url":"` + "ws" + wsSrv.URL[len("http"):] + `"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(api.Close)
	return api, wsSrv.URL
}
