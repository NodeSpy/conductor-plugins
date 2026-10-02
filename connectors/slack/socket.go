package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/coder/websocket"

	"github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// instanceSource is one running Socket Mode connection and its compiled
// triggers.
type instanceSource struct {
	instance string
	api      *slackAPI
	botToken string
	triggers []compiledTrigger
	dedup    *sourcekit.Dedup
	forms    formState
}

// envelope is the Socket Mode frame wrapper.
type envelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Reason     string          `json:"reason"`
	Payload    json.RawMessage `json:"payload"`
}

// Start runs the Socket Mode connection, reconnecting with capped backoff
// until ctx is cancelled (Slack recycles connections periodically via a
// `disconnect` frame).
func (s *instanceSource) Start(ctx context.Context, emit emitFunc) error {
	log.Printf("slack[%s]: connecting (Socket Mode)", s.instance)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := s.runOnce(ctx, emit)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("slack[%s]: connection ended (%v); reconnecting in %s", s.instance, err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// runOnce opens one Socket Mode session and pumps envelopes until it closes.
func (s *instanceSource) runOnce(ctx context.Context, emit emitFunc) error {
	wss, err := s.api.openSocket(ctx)
	if err != nil {
		return err
	}
	c, _, err := websocket.Dial(ctx, wss, nil)
	if err != nil {
		return err
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	c.SetReadLimit(1 << 20)

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return err
		}
		var env envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		// An interactive envelope is decided BEFORE its ACK: a modal
		// submission's validation errors travel in the ACK payload, and the
		// rest (views.open on a 3s trigger_id, emit) runs right after it.
		var ackPayload any
		var after func()
		if env.Type == "interactive" {
			ackPayload, after = s.handleInteractive(ctx, emit, env.Payload)
		}
		if env.EnvelopeID != "" {
			frame := map[string]any{"envelope_id": env.EnvelopeID}
			if ackPayload != nil {
				frame["payload"] = ackPayload
			}
			ack, _ := json.Marshal(frame)
			_ = c.Write(ctx, websocket.MessageText, ack)
		}
		if after != nil {
			after()
		}
		switch env.Type {
		case "hello":
			// connected
		case "disconnect":
			return fmt.Errorf("disconnect: %s", env.Reason)
		case "events_api":
			s.handleEvent(ctx, emit, env.Payload)
		case "slash_commands":
			s.handleSlash(ctx, emit, env.Payload)
		}
	}
}

// --- plugin wiring -----------------------------------------------------

func (p *Plugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	triggers, err := compileTriggers(req.Triggers)
	if err != nil {
		log.Printf("slack[%s]: not starting: %v", req.Instance, err)
		return err
	}
	appToken := str(req.Config["app_token"])
	botToken := str(req.Config["bot_token"])
	apiBase := str(req.Config["api_base"])
	if appToken == "" {
		err := fmt.Errorf("connector %q: app_token is required to receive slack events", req.Instance)
		log.Printf("slack[%s]: %v", req.Instance, err)
		return err
	}
	src := &instanceSource{
		instance: req.Instance, api: newSlackAPI(botToken, appToken, apiBase),
		botToken: botToken, triggers: triggers, dedup: sourcekit.NewDedup(4096),
	}
	// A derived, per-instance context: plugin.stop on ONE instance (reload,
	// removal) must not tear down every other instance this process serves,
	// but Serve hands every StartSource call the same shared ctx.
	iCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.sources[req.Instance] = src
	p.cancels[req.Instance] = cancel
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.sources[req.Instance] == src {
			delete(p.sources, req.Instance)
			delete(p.cancels, req.Instance)
		}
		p.mu.Unlock()
		cancel()
	}()
	err = src.Start(iCtx, func(se plugin.SourceEvent) error {
		se.Instance = req.Instance
		return emit(se)
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("slack[%s]: source stopped: %v", req.Instance, err)
	}
	return err
}

// Stop ends the instance's Socket Mode connection (its own derived context)
// without touching any other instance this process serves.
func (p *Plugin) Stop(_ context.Context, req plugin.StopRequest) error {
	p.mu.Lock()
	cancel := p.cancels[req.Instance]
	delete(p.sources, req.Instance)
	delete(p.cancels, req.Instance)
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}
