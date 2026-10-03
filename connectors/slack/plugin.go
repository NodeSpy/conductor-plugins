package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// Plugin is the slack connector plugin. One process serves every instance of
// the type: Web API clients and running Socket Mode sources are keyed by
// instance name.
type Plugin struct {
	mu      sync.Mutex
	apis    map[string]*slackAPI
	sources map[string]*instanceSource
	cancels map[string]context.CancelFunc
}

// New returns the plugin handler.
func New() *Plugin {
	return &Plugin{
		apis:    map[string]*slackAPI{},
		sources: map[string]*instanceSource{},
		cancels: map[string]context.CancelFunc{},
	}
}

var (
	_ plugin.Handler         = (*Plugin)(nil)
	_ plugin.SourceHandler   = (*Plugin)(nil)
	_ plugin.ValidateHandler = (*Plugin)(nil)
	_ plugin.StopHandler     = (*Plugin)(nil)
)

// Describe returns the slack declaration.
func (p *Plugin) Describe() plugin.Decl { return Decl() }

// api returns (creating if needed) the Web API client for instance, from its
// connection. A running source's client (built at start_source, where the
// app_token is also available) is reused when one exists, so a post/react/
// ask call on an instance with events running never builds a second one.
func (p *Plugin) api(instance string, conn map[string]any) *slackAPI {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sources[instance]; ok {
		return s.api
	}
	if a, ok := p.apis[instance]; ok {
		return a
	}
	a := newSlackAPI(str(conn["bot_token"]), str(conn["app_token"]), str(conn["api_base"]))
	p.apis[instance] = a
	return a
}

// Invoke runs one verb with the instance's credentials.
func (p *Plugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	api := p.api(req.Instance, req.Connection)
	botToken := str(req.Connection["bot_token"])
	webhookURL := str(req.Connection["webhook_url"])
	ctx := context.Background()
	switch req.Verb {
	case "post":
		return p.postVerb(ctx, api, botToken, webhookURL, req.Options)
	case "react":
		return p.reactVerb(ctx, api, botToken, req.Options)
	case "feedback":
		return p.feedbackVerb(ctx, api, botToken, req.Options)
	case "ask":
		return p.askVerb(ctx, api, botToken, req.Options)
	case "thread":
		if botToken == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.thread needs a bot_token (the webhook_url connection is post-only)")
		}
		out, err := p.threadVerb(ctx, api, req.Options)
		if err != nil {
			var pe *plugin.Error
			if errors.As(err, &pe) {
				return plugin.InvokeResult{}, pe
			}
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		return plugin.InvokeResult{Outputs: out}, nil
	case "download":
		if botToken == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "slack.download needs a bot_token (the webhook_url connection is post-only)")
		}
		out, err := p.downloadVerb(ctx, api, req.Options, req.Staging)
		if err != nil {
			var pe *plugin.Error
			if errors.As(err, &pe) {
				return plugin.InvokeResult{}, pe
			}
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		return plugin.InvokeResult{Outputs: out}, nil
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeMethodNotFound, fmt.Sprintf("slack: unknown verb %q", req.Verb))
}
