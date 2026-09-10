// Command conductor-sentry is the Sentry SOURCE connector as an external
// conductor plugin (#59). It receives Sentry Integration-Platform webhooks
// (resources issue/error/event_alert), verifies the Sentry-Hook-Signature HMAC,
// and streams a normalized event per alert to the daemon, which matches it to
// the operator's triggers and resolves the action. It is built ONLY against the
// public SDK + connector-kit (no conductor internals) — it lives outside the
// conductor repo and the daemon no longer bundles a sentry connector at all.
//
// Config (delivered per start_source, from the connector instance):
//
//	listen: ":9099"                 # HTTP listener address
//	path: "/sentry"                 # request path (default /sentry)
//	client_secret: "<hmac secret>"  # Sentry-Hook-Signature key (empty = no verify)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type sentry struct{}

func (sentry) Describe() plugin.Decl {
	ctx := plugin.Schema{
		"resource": {Type: "string"}, "action": {Type: "string"},
		"title": {Type: "string"}, "level": {Type: "string"},
		"environment": {Type: "string"}, "culprit": {Type: "string"},
		"short_id": {Type: "string"}, "project": {Type: "string"}, "url": {Type: "string"},
	}
	ev := func(name, desc string) plugin.Event {
		return plugin.Event{Name: name, Desc: desc, Context: ctx, Filters: plugin.Schema{
			"projects": {Type: "list"}, "levels": {Type: "list"}, "environments": {Type: "list"},
			"level": {Type: "string"}, "project": {Type: "string"}, "environment": {Type: "string"},
		}}
	}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "sentry",
		Desc: "Sentry issue/error alerts via Integration-Platform webhooks (source only).",
		Connection: plugin.Schema{
			"listen":        {Type: "string", Desc: "HTTP listener address, e.g. :9099"},
			"path":          {Type: "string", Desc: "listener path (default /sentry)"},
			"client_secret": {Type: "string", Desc: "Sentry-Hook-Signature HMAC key"},
		},
		Events: []plugin.Event{
			ev("issue_alert", "a Sentry issue alert fired"),
			ev("error_alert", "a Sentry error alert fired"),
			ev("event_alert", "a Sentry metric/event alert fired"),
		},
		// Inbound only: it LISTENS for Sentry webhooks, never dials out, and
		// spawns nothing. An empty manifest is the strongest claim available.
		Capabilities: plugin.Capabilities{},
	}
}

func (sentry) Invoke(plugin.InvokeRequest) (plugin.InvokeResult, error) {
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "sentry is a source connector (no verbs)")
}

func (sentry) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	ln := sourcekit.Listener{
		Addr:      str(cfg["listen"]),
		Path:      strOr(cfg["path"], "/sentry"),
		Secret:    str(cfg["client_secret"]),
		SigHeader: "Sentry-Hook-Signature",
	}
	if ln.Addr == "" {
		return fmt.Errorf("sentry: no listen address configured")
	}
	dedup := sourcekit.NewDedup(2048)
	fmt.Fprintf(os.Stderr, "sentry[%s]: listening on %s%s\n", req.Instance, ln.Addr, ln.Path)
	return ln.Serve(ctx, func(h http.Header, body []byte) {
		f := parse(h.Get("Sentry-Hook-Resource"), body)
		if f.shortID == "" && f.title == "" {
			return
		}
		if !dedup.Add(f.shortID + "\x00" + f.action) {
			return
		}
		event := "issue_alert"
		switch f.resource {
		case "error":
			event = "error_alert"
		case "event_alert":
			event = "event_alert"
		}
		dk := f.shortID
		if dk == "" {
			dk = f.title
		}
		_ = emit(map[string]any{
			"event": event,
			"kind":  event,
			"title": fmt.Sprintf("sentry %s: %s", nonEmpty(f.level, "error"), f.title),
			"dedup": dk,
			"context": map[string]any{
				"resource": f.resource, "action": f.action, "title": f.title, "level": f.level,
				"environment": f.environment, "culprit": f.culprit, "short_id": f.shortID,
				"project": f.project, "url": f.url,
				// Plural aliases so the documented filter vocabulary
				// (filters: {levels/projects/environments: [...]}) matches
				// against the daemon's generic list-contains filter evaluator.
				"levels": f.level, "projects": f.project, "environments": f.environment,
			},
		})
	})
}

func main() {
	if err := plugin.Serve(sentry{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-sentry: %v\n", err)
		os.Exit(1)
	}
}

// --- Sentry payload parsing (replicates the daemon's internal parser) ---

type facts struct {
	resource, action, title, culprit, level string
	environment, shortID, url, project      string
}

type wire struct {
	Action string `json:"action"`
	Data   struct {
		Issue *struct {
			Title, Culprit, Level, ShortID, Permalink string
			Project                                   struct{ Slug string }
		} `json:"issue"`
		Error map[string]any `json:"error"`
		Event map[string]any `json:"event"`
	} `json:"data"`
}

func parse(resource string, body []byte) facts {
	var w wire
	_ = json.Unmarshal(body, &w)
	f := facts{resource: resource, action: w.Action}
	if w.Data.Issue != nil {
		i := w.Data.Issue
		f.title, f.culprit, f.level = i.Title, i.Culprit, i.Level
		f.shortID, f.url, f.project = i.ShortID, i.Permalink, i.Project.Slug
		return f
	}
	m := w.Data.Error
	if m == nil {
		m = w.Data.Event
	}
	if m != nil {
		f.title = nonEmpty(gs(m, "title"), gs(m, "message"))
		f.level = gs(m, "level")
		f.environment = gs(m, "environment")
		f.culprit = gs(m, "culprit")
		f.shortID = nonEmpty(gs(m, "issue_id"), gs(m, "event_id"))
		f.url = nonEmpty(gs(m, "web_url"), gs(m, "url"), gs(m, "issue_url"))
		f.project = projectSlug(m)
	}
	return f
}

func gs(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func projectSlug(m map[string]any) string {
	if p, ok := m["project"].(map[string]any); ok {
		return gs(p, "slug")
	}
	return gs(m, "project")
}

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func str(v any) string { s, _ := v.(string); return s }
func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}
