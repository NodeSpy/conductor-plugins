// Command conductor-alertmanager is the Prometheus Alertmanager SOURCE
// connector as an external conductor plugin (#59). Grafana's unified alerting
// sends the identical webhook payload shape (it IS the Alertmanager v4
// format), so one connector covers both. It listens for the webhook, checks an
// optional bearer token, and streams one normalized "alert" event PER ALERT in
// the batch to the daemon, which matches it to the operator's triggers and
// resolves the action. It is built ONLY against the public SDK + connector-kit
// (no conductor internals).
//
// Config (delivered per start_source, from the connector instance):
//
//	listen: ":9097"          # HTTP listener address
//	path: "/alertmanager"    # request path (default /alertmanager)
//	secret: "<token>"        # bearer token checked against the Authorization
//	                         # header ("Bearer <secret>"); empty = no check
//	allow_unsigned: false    # true = accept unauthenticated POSTs
//
// Unlike sentry/pagerduty, Alertmanager and Grafana webhooks carry no HMAC
// signature — only, optionally, a bearer token configured on the receiver. So
// sourcekit.Listener.Secret is left EMPTY here (its HMAC check does not apply)
// and the bearer token is checked by hand inside the handler below.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type alertmanager struct{}

func (alertmanager) Describe() plugin.Decl {
	ctx := plugin.Schema{
		"status": {Type: "string"}, "alertname": {Type: "string"}, "severity": {Type: "string"},
		"summary": {Type: "string"}, "description": {Type: "string"}, "instance": {Type: "string"},
		"job": {Type: "string"}, "runbook_url": {Type: "string"}, "generatorURL": {Type: "string"},
		"externalURL": {Type: "string"}, "receiver": {Type: "string"},
	}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "alertmanager",
		Desc: "Prometheus Alertmanager and Grafana unified-alerting webhooks — they share the payload shape (source only).",
		Connection: plugin.Schema{
			"listen":         {Type: "string", Desc: "HTTP listener address, e.g. :9097 (optional if smee is set)"},
			"path":           {Type: "string", Desc: "listener path (default /alertmanager)"},
			"secret":         {Type: "string", Desc: "bearer token compared to the Authorization header (\"Bearer <secret>\")"},
			"allow_unsigned": {Type: "bool", Desc: "accept unauthenticated POSTs when no secret is set"},
			"smee":           {Type: "string", Desc: "smee.io-style SSE relay URL, e.g. https://smee.io/AbC123 — also (or instead) receive forwarded deliveries over SSE when the endpoint has no public URL"},
		},
		Events: []plugin.Event{{
			Name:    "alert",
			Desc:    "a Prometheus Alertmanager / Grafana alert fired or resolved (one event per alert in the batch)",
			Context: ctx,
			Filters: plugin.Schema{
				"alertnames": {Type: "list"}, "severities": {Type: "list"},
				"statuses": {Type: "list"}, "receivers": {Type: "list"},
				"alertname": {Type: "string"}, "severity": {Type: "string"},
				"status": {Type: "string"}, "receiver": {Type: "string"},
			},
		}},
		// Inbound only: it LISTENS for Alertmanager/Grafana webhooks, never
		// dials out, and spawns nothing. An empty manifest is the strongest
		// claim available.
		Capabilities: plugin.Capabilities{},
	}
}

func (alertmanager) Invoke(plugin.InvokeRequest) (plugin.InvokeResult, error) {
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "alertmanager is a source connector (no verbs)")
}

func (alertmanager) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	secret := str(cfg["secret"])
	ln := sourcekit.Listener{
		Addr: str(cfg["listen"]),
		Path: strOr(cfg["path"], "/alertmanager"),
		// Secret intentionally left empty — see the package comment. The
		// bearer token, when configured, is verified by hand below instead of
		// via sourcekit's HMAC path.
		Relay: str(cfg["smee"]),
	}
	if ln.Addr == "" && ln.Relay == "" {
		return fmt.Errorf("alertmanager: no listen address or smee relay configured")
	}
	if err := requireBearerSecret("alertmanager", secret, cfg); err != nil {
		return err
	}
	dedup := sourcekit.NewDedup(2048)
	if ln.Addr != "" {
		fmt.Fprintf(os.Stderr, "alertmanager[%s]: listening on %s%s\n", req.Instance, ln.Addr, ln.Path)
	}
	if ln.Relay != "" {
		fmt.Fprintf(os.Stderr, "alertmanager[%s]: relaying via smee channel %s\n", req.Instance, ln.Relay)
	}
	return ln.Serve(ctx, func(h http.Header, body []byte) {
		if secret != "" && !verifyBearer(secret, h.Get("Authorization")) {
			return
		}
		w, err := parse(body)
		if err != nil {
			return
		}
		for _, a := range w.Alerts {
			if a.Fingerprint == "" {
				continue
			}
			if !dedup.Add(dedupKey(a)) {
				continue
			}
			_ = emit(alertEvent(w, a))
		}
	})
}

func main() {
	if err := plugin.Serve(alertmanager{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-alertmanager: %v\n", err)
		os.Exit(1)
	}
}

// --- Alertmanager webhook (v4) payload parsing ---
//
// Grafana's unified alerting sends this exact shape, so one parser covers
// both providers.

type webhook struct {
	Version           string            `json:"version"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupKey          string            `json:"groupKey"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []alert           `json:"alerts"`
}

type alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

func parse(body []byte) (webhook, error) {
	var w webhook
	if err := json.Unmarshal(body, &w); err != nil {
		return webhook{}, err
	}
	return w, nil
}

// dedupKey identifies one alert delivery: the same fingerprint fires again
// when it transitions firing -> resolved, and that transition is a distinct,
// wanted event, so the status is part of the key.
func dedupKey(a alert) string {
	return a.Fingerprint + "\x00" + a.Status
}

// alertEvent builds the emitted event map for one alert out of a batch.
func alertEvent(w webhook, a alert) map[string]any {
	ctx := map[string]any{
		"status":       a.Status,
		"alertname":    a.Labels["alertname"],
		"severity":     a.Labels["severity"],
		"summary":      a.Annotations["summary"],
		"description":  a.Annotations["description"],
		"instance":     a.Labels["instance"],
		"job":          a.Labels["job"],
		"runbook_url":  a.Annotations["runbook_url"],
		"generatorURL": a.GeneratorURL,
		"externalURL":  w.ExternalURL,
		"receiver":     w.Receiver,
		// Plural aliases so the documented filter vocabulary (filters:
		// {alertnames/severities/statuses/receivers: [...]}) matches against
		// the daemon's generic list-contains filter evaluator.
		"alertnames": a.Labels["alertname"],
		"severities": a.Labels["severity"],
		"statuses":   a.Status,
		"receivers":  w.Receiver,
	}
	// Flatten every label into context (so `{{.<label>}}` works for anything
	// operator-defined, e.g. team/cluster/namespace) without clobbering the
	// named keys and filter aliases set above.
	for k, v := range a.Labels {
		if _, exists := ctx[k]; exists {
			continue
		}
		ctx[k] = v
	}
	return map[string]any{
		"event":   "alert",
		"kind":    a.Status,
		"title":   fmt.Sprintf("alertmanager %s: %s", a.Status, nonEmpty(a.Labels["alertname"], a.Annotations["summary"])),
		"dedup":   dedupKey(a),
		"context": ctx,
	}
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

// verifyBearer reports whether authHeader carries a bearer token matching
// secret. Comparison is constant-time to avoid leaking the secret via timing.
// An empty secret always passes — the caller (requireBearerSecret) is what
// decides whether that is acceptable.
func verifyBearer(secret, authHeader string) bool {
	if secret == "" {
		return true
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return false
	}
	token := strings.TrimPrefix(authHeader, prefix)
	return subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
}

// requireBearerSecret refuses to start an unauthenticated webhook listener.
//
// Alertmanager and Grafana webhooks carry no signature at all, only an
// optional bearer token — so unlike sentry/pagerduty's HMAC secret, there is
// nothing here that fails closed by default. Omitting `secret` is far more
// often a mistake than a choice, so it fails closed here too; `allow_unsigned:
// true` is the explicit, greppable way to say you meant it.
func requireBearerSecret(who, secret string, cfg map[string]any) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if b, _ := cfg["allow_unsigned"].(bool); b {
		fmt.Fprintf(os.Stderr, "%s: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers\n", who)
		return nil
	}
	return fmt.Errorf("%s: no bearer secret configured (secret) — an unauthenticated listener accepts any POST on the listen address as a real alert. Set it, or set `allow_unsigned: true` if you genuinely front this with something else that authenticates", who)
}
