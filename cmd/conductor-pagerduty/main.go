// Command conductor-pagerduty is the PagerDuty SOURCE connector as an external
// conductor plugin (#59). It receives PagerDuty V3 webhook subscription events,
// verifies the X-PagerDuty-Signature HMAC (which may carry several v1= values
// during key rotation), and streams a normalized "incident" event per event to
// the daemon, which matches it to the operator's triggers and resolves the
// action. Built ONLY against the public SDK + connector-kit.
//
// Config (per start_source):
//
//	listen: ":9098"                 # HTTP listener address
//	path: "/pagerduty"              # request path (default /pagerduty)
//	signing_secret: "<secret>"      # webhook subscription signing secret
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type pagerduty struct{}

func (pagerduty) Describe() plugin.Decl {
	return plugin.Decl{
		Type: "pagerduty",
		Desc: "PagerDuty incident webhooks (V3 subscriptions; source only).",
		Connection: plugin.Schema{
			"listen":         {Type: "string", Desc: "HTTP listener address, e.g. :9098"},
			"path":           {Type: "string", Desc: "listener path (default /pagerduty)"},
			"signing_secret": {Type: "string", Desc: "webhook subscription signing secret"},
		},
		Events: []plugin.Event{{
			Name: "incident",
			Desc: "a PagerDuty incident event",
			Filters: plugin.Schema{
				"event_types": {Type: "list", Desc: "e.g. incident.triggered, incident.escalated (empty = any)"},
				"services":    {Type: "list", Desc: "service summary or id (empty = any)"},
				"urgencies":   {Type: "list", Desc: "high|low (empty = any)"},
				"priorities":  {Type: "list", Desc: "P1, P2, … (empty = any)"},
			},
		}},
		Capabilities: plugin.Capabilities{},
	}
}

func (pagerduty) Invoke(plugin.InvokeRequest) (plugin.InvokeResult, error) {
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "pagerduty is a source connector (no verbs)")
}

func (pagerduty) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	ln := sourcekit.Listener{
		Addr:      str(cfg["listen"]),
		Path:      strOr(cfg["path"], "/pagerduty"),
		Secret:    str(cfg["signing_secret"]),
		SigHeader: "X-PagerDuty-Signature",
	}
	if ln.Addr == "" {
		return fmt.Errorf("pagerduty: no listen address configured")
	}
	dedup := sourcekit.NewDedup(2048)
	fmt.Fprintf(os.Stderr, "pagerduty[%s]: listening on %s%s\n", req.Instance, ln.Addr, ln.Path)
	return ln.Serve(ctx, func(h http.Header, body []byte) {
		f := parse(body)
		if f.EventType == "" {
			return
		}
		dk := fmt.Sprintf("%s\x00%s", f.ID, f.EventType)
		if !dedup.Add(dk) {
			return
		}
		_ = emit(map[string]any{
			"event": "incident",
			"kind":  f.EventType,
			"title": fmt.Sprintf("pagerduty %s: %s", f.EventType, f.Title),
			"dedup": dk,
			"context": map[string]any{
				"pagerduty.event_type": f.EventType,
				"pagerduty.status":     f.Status,
				"pagerduty.title":      f.Title,
				"pagerduty.urgency":    f.Urgency,
				"pagerduty.priority":   f.Priority,
				"pagerduty.service":    f.Service,
				"pagerduty.service_id": f.ServiceID,
				"pagerduty.number":     f.Number,
				"pagerduty.id":         f.ID,
				"pagerduty.url":        f.URL,
				"url":                  f.URL,
				"title":                f.Title,
				// Filter-key aliases for the daemon's generic list-contains
				// filter evaluator (filters: {event_types/services/urgencies/priorities}).
				"event_types": f.EventType,
				"services":    []any{f.Service, f.ServiceID},
				"urgencies":   f.Urgency,
				"priorities":  f.Priority,
			},
		})
	})
}

func main() {
	if err := plugin.Serve(pagerduty{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-pagerduty: %v\n", err)
		os.Exit(1)
	}
}

// --- PagerDuty V3 payload parsing (replicates the daemon's internal parser) ---

type facts struct {
	EventType, Status, Title, Urgency, Priority string
	Service, ServiceID, ID, URL                 string
	Number                                      int
}

type wire struct {
	Event struct {
		EventType string `json:"event_type"`
		Data      struct {
			ID       string `json:"id"`
			Number   int    `json:"number"`
			Status   string `json:"status"`
			Title    string `json:"title"`
			HTMLURL  string `json:"html_url"`
			Urgency  string `json:"urgency"`
			Priority *struct {
				Summary string `json:"summary"`
			} `json:"priority"`
			Service *struct {
				ID      string `json:"id"`
				Summary string `json:"summary"`
			} `json:"service"`
		} `json:"data"`
	} `json:"event"`
}

func parse(body []byte) facts {
	var w wire
	_ = json.Unmarshal(body, &w)
	d := w.Event.Data
	f := facts{
		EventType: w.Event.EventType, Status: d.Status, Title: d.Title,
		Urgency: d.Urgency, Number: d.Number, ID: d.ID, URL: d.HTMLURL,
	}
	if d.Priority != nil {
		f.Priority = d.Priority.Summary
	}
	if d.Service != nil {
		f.Service, f.ServiceID = d.Service.Summary, d.Service.ID
	}
	return f
}

func str(v any) string { s, _ := v.(string); return s }
func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}
