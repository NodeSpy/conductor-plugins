// Package ghplugin is the github connector as a conductor PLUGIN: a
// plugin.Handler that serves the github declaration, every verb (through
// pkg/githubkit), and the full event source (through pkg/githubkit/ghsource)
// over the public SDK's source extension. A plugin binary is one line:
//
//	func main() { _ = plugin.Serve(ghplugin.New()) }
//
// It is the same source conductor's bundled github connector runs — the
// bundled connector and this handler lower their configuration through the
// same two kit functions and evaluate every event with the same code — so the
// plugin is a drop-in for the builtin, not an approximation of it. What the
// daemon still owns (trust, dispatch identity, the engine's own dedup and
// high-water marks) is spelled out in docs/design/plugin-source-abi.md.
//
// It imports only pkg/: a third party can build exactly this.
package ghplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit"
	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghsource"
	"github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// Plugin is the github connector plugin. One process serves every instance
// of the type: verb clients and running sources are keyed by instance name.
type Plugin struct {
	mu      sync.Mutex
	clients map[string]*githubkit.Client
	sources map[string]*ghsource.Source
	// started are the start_source requests behind sources, kept so a dry
	// run can build a throwaway source with the same config and triggers
	// (a dry run against the live one would claim reviews it then never
	// dispatches).
	started map[string]plugin.StartSourceRequest
	// ownStatus holds status contexts an instance's set_status posted before
	// its source started, applied when it does (see noteOwnStatus).
	ownStatus map[string][]string
}

// New returns the plugin handler.
func New() *Plugin {
	return &Plugin{
		clients:   map[string]*githubkit.Client{},
		sources:   map[string]*ghsource.Source{},
		started:   map[string]plugin.StartSourceRequest{},
		ownStatus: map[string][]string{},
	}
}

var (
	_ plugin.Handler          = (*Plugin)(nil)
	_ plugin.SourceHandler    = (*Plugin)(nil)
	_ plugin.PollHandler      = (*Plugin)(nil)
	_ plugin.TranslateHandler = (*Plugin)(nil)
	_ plugin.AppTokenHandler  = (*Plugin)(nil)
)

// Describe returns the github declaration.
func (p *Plugin) Describe() plugin.Decl { return Decl() }

// Invoke runs one verb with the instance's credentials.
func (p *Plugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	if req.Verb == "sweep" {
		// The daemon answers the declared poll verb itself and never sends it
		// here; an older daemon does: nudge this process's own sources, which
		// is the most this side of the wire can reach.
		n := 0
		p.mu.Lock()
		for _, s := range p.sources {
			if s.SweepNow() {
				n++
			}
		}
		p.mu.Unlock()
		return plugin.InvokeResult{Outputs: map[string]any{"nudged": n}}, nil
	}
	kit, err := p.client(req.Instance, req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	out, err := kit.Invoke(context.Background(), req.Verb, req.Options)
	if err != nil {
		// A verb call that answered with one of the contract's own codes
		// (plugin-contract.md §1.11 — target_gone, invalid, rate_limited,
		// upstream, not_ready) must reach the daemon as that code, not as a
		// generic internal error: the engine's stop/retry/never-retry
		// behavior for each is keyed on it.
		var ce *plugin.Error
		if errors.As(err, &ce) {
			return plugin.InvokeResult{}, ce
		}
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if req.Verb == "set_status" {
		// Whatever context a status went out under is conductor's own from
		// now on: the source never reads it back as a CI signal, or a
		// `failure` a hook posted could dispatch the next fixer.
		if c, _ := out["context"].(string); c != "" {
			p.noteOwnStatus(req.Instance, c)
		}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *Plugin) client(instance string, conn map[string]any) (*githubkit.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[instance]; ok {
		return c, nil
	}
	pc, err := ParseConnection(conn)
	if err != nil {
		return nil, fmt.Errorf("connector %q: %w", instance, err)
	}
	c, err := githubkit.NewClient(pc.Client)
	if err != nil {
		return nil, err
	}
	p.clients[instance] = c
	return c, nil
}

func (p *Plugin) noteOwnStatus(instance, ctxName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.sources[instance]; s != nil {
		s.NoteOwnStatusContext(ctxName)
		return
	}
	p.ownStatus[instance] = append(p.ownStatus[instance], ctxName)
}

// BuildSource lowers a start_source request into the source it describes —
// the instance's connection plus its triggers, through the same kit functions
// the bundled connector calls. Exported for hosts that drive a source in
// process (and for the parity tests).
func BuildSource(instance string, config map[string]any, triggers []plugin.SourceTrigger) (*ghsource.Source, error) {
	pc, err := ParseConnection(config)
	if err != nil {
		return nil, fmt.Errorf("connector %q: %w", instance, err)
	}
	actions := map[string]ghsource.ActionSet{}
	for _, t := range triggers {
		var f *sourcekit.Filter
		if len(t.Filter) > 0 && string(t.Filter) != "null" {
			f = new(sourcekit.Filter)
			if err := json.Unmarshal(t.Filter, f); err != nil {
				return nil, fmt.Errorf("trigger %s: filter: %w", t.ID, err)
			}
		}
		act, err := ghsource.LowerTrigger(ghsource.TriggerSpec{
			Name: t.Name, Event: t.Event, Enabled: t.Enabled, Options: t.Options, Filter: f, Ext: t.ID,
		}, pc.Source.Repos)
		if err != nil {
			return nil, fmt.Errorf("trigger on %s.%s: %w", instance, t.Event, err)
		}
		actions[t.Event] = append(actions[t.Event], act)
	}
	src, err := ghsource.New(instance, pc.Source.SourceConfig(actions))
	if err != nil {
		return nil, err
	}
	if err := src.Validate(); err != nil {
		return nil, err
	}
	return src, nil
}

// StartSource runs the instance's event source until ctx ends, streaming
// every trigger it fires as a routed event.
func (p *Plugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	src, err := BuildSource(req.Instance, req.Config, req.Triggers)
	if err != nil {
		log.Printf("github[%s]: not starting: %v", req.Instance, err)
		return err
	}
	// The verb client too: a target-head read for a run this source started
	// may come before any verb call has handed over the credentials.
	if _, err := p.client(req.Instance, req.Config); err != nil {
		log.Printf("github[%s]: verb client: %v", req.Instance, err)
	}
	p.mu.Lock()
	p.sources[req.Instance] = src
	p.started[req.Instance] = req
	for _, c := range p.ownStatus[req.Instance] {
		src.NoteOwnStatusContext(c)
	}
	delete(p.ownStatus, req.Instance)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.sources[req.Instance] == src {
			delete(p.sources, req.Instance)
		}
		p.mu.Unlock()
	}()
	err = src.Start(ctx, func(_ context.Context, t ghsource.Trigger) {
		if werr := emit(Event(req.Instance, t)); werr != nil {
			log.Printf("github[%s]: emit %s: %v", req.Instance, t.Kind, werr)
		}
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("github[%s]: source stopped: %v", req.Instance, err)
	}
	return err
}

func wireTarget(t ghsource.Target) plugin.Target {
	return plugin.Target{Repo: t.Repo, Owner: t.Owner, Name: t.Name, PR: t.PR, Issue: t.Issue, Number: t.Number,
		HeadSHA: t.HeadSHA, BaseRef: t.BaseRef, HTMLURL: t.HTMLURL, Project: t.Project}
}

// Event is the wire form of one source trigger: the kind, the trigger it was
// evaluated for (its SourceTrigger.ID, carried in the Action's Ext), and the
// target and context exactly as the bundled source hands the engine.
func Event(instance string, t ghsource.Trigger) plugin.SourceEvent {
	ev := plugin.SourceEvent{
		Event: t.Kind, Kind: t.Kind, Title: t.Title,
		Target:  wireTarget(t.Target),
		Context: t.Context, Dedup: t.Dedup, Labels: t.Labels,
		Instance: instance, CatchUp: t.CatchUp, TargetTrusted: t.TargetTrusted,
	}
	if a, ok := t.Action.(ghsource.Action); ok {
		ev.Trigger, _ = a.Ext.(string)
	}
	return ev
}

func (p *Plugin) source(instance string) (*ghsource.Source, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sources[instance]
	if s == nil {
		return nil, plugin.Errorf(plugin.CodeInvalidParams, fmt.Sprintf("no running source for instance %q", instance))
	}
	return s, nil
}

// Poll serves plugin.poll: now runs the catch-up sweep (its events stream as
// usual); target returns one PR's or issue's events for req.Event (the host
// marks them forced); dry_run returns what a sweep would emit, from a
// throwaway source so the live one's review claims are untouched.
func (p *Plugin) Poll(ctx context.Context, req plugin.PollRequest) (plugin.PollResult, error) {
	switch req.Mode {
	case plugin.PollNow:
		s, err := p.source(req.Instance)
		if err != nil {
			return plugin.PollResult{}, nil // nothing running: nothing to poll
		}
		s.SweepNow()
		return plugin.PollResult{}, nil
	case plugin.PollTarget:
		s, err := p.source(req.Instance)
		if err != nil {
			return plugin.PollResult{}, err
		}
		repo, number, err := parseTargetRef(req.Target)
		if err != nil {
			return plugin.PollResult{}, plugin.Fail(plugin.CodeInvalid, err.Error(), nil)
		}
		if req.Event == "" {
			return plugin.PollResult{}, plugin.Fail(plugin.CodeInvalid, "github: a forced poll names the event to build", nil)
		}
		out, err := collect(ctx, req.Instance, func(emit ghsource.EmitFunc) error {
			_, err := s.Force(ctx, req.Event, repo, number, emit)
			return err
		})
		return plugin.PollResult{Events: out}, err
	case plugin.PollDryRun:
		p.mu.Lock()
		sreq, ok := p.started[req.Instance]
		p.mu.Unlock()
		if !ok {
			return plugin.PollResult{}, plugin.Fail(plugin.CodeInvalid, fmt.Sprintf("no running source for instance %q", req.Instance), nil)
		}
		dry, err := BuildSource(req.Instance, sreq.Config, sreq.Triggers)
		if err != nil {
			return plugin.PollResult{}, err
		}
		out, err := collect(ctx, req.Instance, func(emit ghsource.EmitFunc) error { return dry.SweepOnce(ctx, emit) })
		return plugin.PollResult{Events: out}, err
	}
	return plugin.PollResult{}, plugin.Fail(plugin.CodeInvalid, fmt.Sprintf("unknown poll mode %q", req.Mode), nil)
}

// Translate decodes one webhook delivery into routed events, from a source
// built for the request's config and triggers (replay and once run with no
// source started). The event name comes from req.Event, else the delivery's
// X-GitHub-Event header.
func (p *Plugin) Translate(ctx context.Context, req plugin.TranslateRequest) (plugin.TranslateResult, error) {
	event := req.Event
	if event == "" {
		for k, v := range req.Headers {
			if strings.EqualFold(k, "X-GitHub-Event") {
				event = v
			}
		}
	}
	if event == "" {
		return plugin.TranslateResult{}, plugin.Fail(plugin.CodeInvalid, "github: the delivery names no event (X-GitHub-Event)", nil)
	}
	src, err := BuildSource(req.Instance, req.Config, req.Triggers)
	if err != nil {
		return plugin.TranslateResult{}, plugin.Fail(plugin.CodeInvalid, err.Error(), nil)
	}
	var out []plugin.SourceEvent
	for _, t := range src.Translate(ctx, event, []byte(req.Body)) {
		out = append(out, Event(req.Instance, t))
	}
	return plugin.TranslateResult{Events: out}, nil
}

// collect runs fn with an emit that gathers wire events.
func collect(_ context.Context, instance string, fn func(ghsource.EmitFunc) error) ([]plugin.SourceEvent, error) {
	var out []plugin.SourceEvent
	var mu sync.Mutex
	err := fn(func(_ context.Context, t ghsource.Trigger) {
		mu.Lock()
		out = append(out, Event(instance, t))
		mu.Unlock()
	})
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	return out, nil
}

// parseTargetRef reads "owner/repo#n" (or "owner/repo").
func parseTargetRef(ref string) (string, int, error) {
	repo, num, ok := strings.Cut(ref, "#")
	if !strings.Contains(repo, "/") {
		return "", 0, fmt.Errorf("github: target %q is not owner/repo#number", ref)
	}
	if !ok {
		return repo, 0, nil
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("github: target %q is not owner/repo#number", ref)
	}
	return repo, n, nil
}

// AppToken mints a fresh App installation token for a resumed run.
func (p *Plugin) AppToken(req plugin.AppTokenRequest) (plugin.AppTokenResult, error) {
	s, err := p.source(req.Instance)
	if err != nil {
		return plugin.AppTokenResult{}, err
	}
	if req.InstallationID == 0 {
		return plugin.AppTokenResult{}, plugin.Errorf(plugin.CodeInvalidParams, "no installation_id")
	}
	tok, err := s.AppToken(context.Background(), req.InstallationID)
	if err != nil {
		return plugin.AppTokenResult{}, plugin.Errorf(plugin.CodeInternalError, strings.TrimSpace(err.Error()))
	}
	return plugin.AppTokenResult{Token: tok}, nil
}
