// Command conductor-pagerduty is the PagerDuty connector as an external
// conductor plugin (#59): a SOURCE of incident webhooks, plus incident VERBS
// over the REST API (get / list / acknowledge / resolve / add_note /
// escalate) for an agent or workflow that triages pages. It receives PagerDuty V3 webhook subscription events,
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
//	smee: "https://smee.io/AbC123"  # optional SSE relay URL, in place of or alongside listen
//
// Verbs (per invoke):
//
//	api_token: "<REST API key>"     # required for the verbs (or PAGERDUTY_TOKEN)
//	from: "you@example.com"         # the PagerDuty user writes are made as (required for writes)
//	api_base: "https://api.pagerduty.com"   # override (tests, EU service region)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/relay"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type pagerduty struct{}

func (pagerduty) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "pagerduty",
		Desc: "PagerDuty: incident webhooks (V3 subscriptions) as a source, plus incident verbs over the REST API — get, list, acknowledge, resolve, add_note, escalate.",
		Connection: plugin.Schema{
			"listen":         {Type: "string", Desc: "HTTP listener address, e.g. :9098 (optional if smee is set)"},
			"path":           {Type: "string", Desc: "listener path (default /pagerduty)"},
			"signing_secret": {Type: "string", Desc: "webhook subscription signing secret"},
			"smee":           {Type: "string", Desc: "smee.io-style SSE relay URL, e.g. https://smee.io/AbC123 — also (or instead) receive forwarded deliveries over SSE when the endpoint has no public URL"},
			"api_token":      {Type: "string", Desc: "REST API key for the verbs (or the PAGERDUTY_TOKEN env var)", Secret: true},
			"from":           {Type: "string", Desc: "email of the PagerDuty user writes are made as (required by PagerDuty for acknowledge/resolve/add_note/escalate)"},
			"api_base":       {Type: "string", Desc: "REST API base URL (default https://api.pagerduty.com)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "get_incident", Desc: "read one incident",
				Options: plugin.Schema{"id": {Type: "string", Required: true, Desc: "incident id (e.g. PT4KHLK)"}},
				Outputs: incidentOutputs,
			},
			{
				Name: "list_incidents", Desc: "list incidents (newest first)",
				Options: plugin.Schema{
					"statuses":    {Type: "list", Desc: "triggered | acknowledged | resolved (default triggered + acknowledged)"},
					"service_ids": {Type: "list"},
					"since":       {Type: "string", Desc: "RFC 3339 start"},
					"until":       {Type: "string", Desc: "RFC 3339 end"},
					"limit":       {Type: "integer", Desc: "max incidents (default 25, max 100)"},
				},
				Outputs: plugin.Schema{"incidents": {Type: "list"}, "count": {Type: "integer"}},
			},
			{
				Name: "acknowledge", Desc: "acknowledge an incident (stops its escalation; reversible — it re-triggers on timeout)",
				Options: plugin.Schema{"id": {Type: "string", Required: true}},
				Outputs: incidentOutputs,
			},
			{
				Name: "resolve", Desc: "resolve an incident",
				Options: plugin.Schema{"id": {Type: "string", Required: true}},
				Outputs: incidentOutputs,
			},
			{
				Name: "add_note", Desc: "add a note to an incident's timeline",
				Options: plugin.Schema{"id": {Type: "string", Required: true}, "content": {Type: "string", Required: true}},
				Outputs: plugin.Schema{"note_id": {Type: "string"}},
			},
			{
				Name: "escalate", Desc: "move an incident to an escalation level (re-notifies that level)",
				Options: plugin.Schema{"id": {Type: "string", Required: true}, "level": {Type: "integer", Required: true, Desc: "escalation level, 1-based"}},
				Outputs: incidentOutputs,
			},
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
		// The source only LISTENS; the verbs dial the REST API. Spawns nothing.
		Capabilities: plugin.Capabilities{Egress: []string{"api.pagerduty.com:443"}},
	}
}

var incidentOutputs = plugin.Schema{
	"id": {Type: "string"}, "number": {Type: "integer"}, "status": {Type: "string"}, "title": {Type: "string"},
	"urgency": {Type: "string"}, "service": {Type: "string"}, "url": {Type: "string"}, "escalation_level": {Type: "integer"},
}

func (pagerduty) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	c, o := req.Connection, req.Options
	if o == nil {
		o = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bad := func(msg string) (plugin.InvokeResult, error) {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+msg)
	}
	id := str(o["id"])
	needID := req.Verb != "list_incidents"
	if needID && (id == "" || strings.ContainsAny(id, "/?#")) {
		return bad("id is required (a PagerDuty incident id)")
	}
	write := map[string]bool{"acknowledge": true, "resolve": true, "add_note": true, "escalate": true}[req.Verb]
	if write && str(c["from"]) == "" {
		return bad("the connection's from: (a PagerDuty user email) is required for writes")
	}
	var (
		out map[string]any
		err error
	)
	switch req.Verb {
	case "get_incident":
		out, err = pdIncident(ctx, c, http.MethodGet, id, nil)
	case "acknowledge", "resolve":
		st := map[string]string{"acknowledge": "acknowledged", "resolve": "resolved"}[req.Verb]
		out, err = pdIncident(ctx, c, http.MethodPut, id, map[string]any{"incident": map[string]any{"type": "incident_reference", "status": st}})
	case "escalate":
		lvl, ok := o["level"].(float64)
		if !ok {
			if n, ok2 := o["level"].(int); ok2 {
				lvl, ok = float64(n), true
			}
		}
		if !ok || lvl < 1 || lvl != float64(int(lvl)) {
			return bad("level must be a positive integer")
		}
		out, err = pdIncident(ctx, c, http.MethodPut, id, map[string]any{"incident": map[string]any{"type": "incident_reference", "escalation_level": int(lvl)}})
	case "add_note":
		content := str(o["content"])
		if strings.TrimSpace(content) == "" {
			return bad("content is required")
		}
		var res map[string]any
		res, err = pdDo(ctx, c, http.MethodPost, "/incidents/"+url.PathEscape(id)+"/notes", map[string]any{"note": map[string]any{"content": content}})
		if err == nil {
			n, _ := res["note"].(map[string]any)
			out = map[string]any{"note_id": str(n["id"])}
		}
	case "list_incidents":
		q := url.Values{}
		sts := listOf(o["statuses"])
		if len(sts) == 0 {
			sts = []string{"triggered", "acknowledged"}
		}
		for _, s := range sts {
			q.Add("statuses[]", s)
		}
		for _, s := range listOf(o["service_ids"]) {
			q.Add("service_ids[]", s)
		}
		if v := str(o["since"]); v != "" {
			q.Set("since", v)
		}
		if v := str(o["until"]); v != "" {
			q.Set("until", v)
		}
		limit := 25
		if f, ok := o["limit"].(float64); ok && f > 0 {
			limit = int(f)
		}
		if limit > 100 {
			limit = 100
		}
		q.Set("limit", strconv.Itoa(limit))
		q.Set("sort_by", "created_at:desc")
		var res map[string]any
		res, err = pdDo(ctx, c, http.MethodGet, "/incidents?"+q.Encode(), nil)
		if err == nil {
			raw, _ := res["incidents"].([]any)
			list := make([]any, 0, len(raw))
			for _, r := range raw {
				m, _ := r.(map[string]any)
				list = append(list, incidentFacts(m))
			}
			out = map[string]any{"incidents": list, "count": len(list)}
		}
	default:
		return bad("unknown verb")
	}
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func pdIncident(ctx context.Context, c map[string]any, method, id string, body map[string]any) (map[string]any, error) {
	res, err := pdDo(ctx, c, method, "/incidents/"+url.PathEscape(id), body)
	if err != nil {
		return nil, err
	}
	m, _ := res["incident"].(map[string]any)
	return incidentFacts(m), nil
}

func incidentFacts(m map[string]any) map[string]any {
	out := map[string]any{"id": str(m["id"]), "status": str(m["status"]), "title": str(m["title"]),
		"urgency": str(m["urgency"]), "url": str(m["html_url"])}
	if n, ok := m["incident_number"].(float64); ok {
		out["number"] = int(n)
	}
	if n, ok := m["escalation_level"].(float64); ok {
		out["escalation_level"] = int(n)
	}
	if s, ok := m["service"].(map[string]any); ok {
		out["service"] = str(s["summary"])
	}
	return out
}

// pdDo is one REST API call.
func pdDo(ctx context.Context, c map[string]any, method, path string, body map[string]any) (map[string]any, error) {
	tok := str(c["api_token"])
	if tok == "" {
		tok = os.Getenv("PAGERDUTY_TOKEN")
	}
	if tok == "" {
		return nil, fmt.Errorf("no api_token (or PAGERDUTY_TOKEN) configured")
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(strOr(c["api_base"], "https://api.pagerduty.com"), "/")+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token token="+tok)
	req.Header.Set("Accept", "application/vnd.pagerduty+json;version=2")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if f := str(c["from"]); f != "" {
		req.Header.Set("From", f)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("%s %s: %s: %s", method, strings.SplitN(path, "?", 2)[0], resp.Status, msg)
	}
	var out map[string]any
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
	}
	return out, nil
}

func listOf(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return x
	case string:
		if x != "" {
			return []string{x}
		}
	}
	return nil
}

func (pagerduty) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	ln := relay.Listener{
		Addr:      str(cfg["listen"]),
		Path:      strOr(cfg["path"], "/pagerduty"),
		Secret:    str(cfg["signing_secret"]),
		SigHeader: "X-PagerDuty-Signature",
		Relay:     str(cfg["smee"]),
	}
	if ln.Addr == "" && ln.Relay == "" {
		return fmt.Errorf("pagerduty: no listen address or smee relay configured")
	}
	if err := requireWebhookSecret("pagerduty", ln.Secret, cfg, "signing_secret"); err != nil {
		return err
	}
	dedup := sourcekit.NewDedup(2048)
	if ln.Addr != "" {
		fmt.Fprintf(os.Stderr, "pagerduty[%s]: listening on %s%s\n", req.Instance, ln.Addr, ln.Path)
	}
	if ln.Relay != "" {
		fmt.Fprintf(os.Stderr, "pagerduty[%s]: relaying via smee channel %s\n", req.Instance, ln.Relay)
	}
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

// requireWebhookSecret refuses to start an unauthenticated webhook
// listener.
//
// sourcekit.VerifyHMAC returns true when the secret is empty — it leaves
// the policy to the caller — and no caller had one. The result was that
// omitting the secret silently accepted ANY unsigned POST on the listen
// address as a real event from the provider, which is remote trigger
// injection with no signal that it happened. A missing secret is far more
// often a mistake than a choice, so it fails closed; `allow_unsigned:
// true` is the explicit, greppable way to say you meant it.
func requireWebhookSecret(who, secret string, cfg map[string]any, allowKey string) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if b, _ := cfg["allow_unsigned"].(bool); b {
		fmt.Fprintf(os.Stderr, "%s: allow_unsigned is set — accepting UNSIGNED webhooks; anyone who can reach the listen address can fire triggers\n", who)
		return nil
	}
	return fmt.Errorf("%s: no webhook secret configured (%s) — an unsigned listener accepts any POST on the listen address as a real event. Set it, or set `allow_unsigned: true` if you genuinely front this with something else that authenticates", who, allowKey)
}
