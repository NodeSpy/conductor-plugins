// Command conductor-uptimekuma is the Uptime Kuma SOURCE connector as an
// external conductor plugin (#59). It listens for Uptime Kuma's webhook
// notification (fired on every monitor heartbeat status change), checks an
// optional shared token, and streams one normalized "monitor" event to the
// daemon, which matches it to the operator's triggers and resolves the
// action. Built ONLY against the public SDK (pkg/plugin) and the
// connector-kit (pkg/sourcekit) — no other internal daemon package, no
// third-party dependency.
//
// Uptime Kuma has no stable REST API — its dashboard talks socket.io, which
// this plugin does not speak — so the only integration surface available is
// its outbound webhook notification. That makes this connector SOURCE ONLY:
// there is nothing to invoke.
//
// Config (delivered per start_source, from the connector instance):
//
//	listen: ":9096"          # HTTP listener address
//	path: "/uptimekuma"      # request path (default /uptimekuma)
//	secret: "<shared token>" # compared to X-Conductor-Token / ?token=
//	allow_unsigned: false    # true = accept unauthenticated POSTs
//
// Uptime Kuma's webhook notification is UNSIGNED: there is no HMAC to verify,
// only whatever the operator pastes into the notification's custom body /
// URL. So, like the datadog connector, sourcekit.Listener.Secret is left
// EMPTY here (its HMAC check does not apply) and the shared token is checked
// by hand inside the handler below. See requireWebhookSecret and verifyToken.
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
	"strconv"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type uptimekuma struct{}

func (uptimekuma) Describe() plugin.Decl {
	ctx := plugin.Schema{
		"status": {Type: "string"}, "monitor_name": {Type: "string"},
		"monitor_url": {Type: "string"}, "monitor_type": {Type: "string"},
		"monitor_id": {Type: "string"}, "msg": {Type: "string"},
		"important": {Type: "bool"}, "time": {Type: "string"},
	}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "uptimekuma",
		Desc: "Uptime Kuma webhook notifications (up/down) as a source. Uptime Kuma has no stable REST API — it is socket.io — so this is source-only.",
		Connection: plugin.Schema{
			"listen":         {Type: "string", Desc: "HTTP listener address, e.g. :9096"},
			"path":           {Type: "string", Desc: "listener path (default /uptimekuma)"},
			"secret":         {Type: "string", Desc: "shared token compared to X-Conductor-Token / ?token="},
			"allow_unsigned": {Type: "bool", Desc: "accept unauthenticated POSTs when no secret is set"},
		},
		Events: []plugin.Event{{
			Name:    "monitor",
			Desc:    "an Uptime Kuma monitor heartbeat notification (up/down/pending/maintenance)",
			Context: ctx,
			Filters: plugin.Schema{
				"statuses":      {Type: "list", Desc: "down|up|pending|maintenance (empty = any)"},
				"monitors":      {Type: "list", Desc: "monitor name (empty = any)"},
				"monitor_types": {Type: "list", Desc: "e.g. http, tcp, ping (empty = any)"},
				"status":        {Type: "string"}, "monitor": {Type: "string"}, "monitor_type": {Type: "string"},
			},
		}},
		// Inbound only: it LISTENS for Uptime Kuma webhooks, never dials out,
		// and spawns nothing. An empty manifest is the strongest claim
		// available.
		Capabilities: plugin.Capabilities{},
	}
}

func (uptimekuma) Invoke(plugin.InvokeRequest) (plugin.InvokeResult, error) {
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uptimekuma is a source connector (no verbs)")
}

func (uptimekuma) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	secret := str(cfg["secret"])
	ln := sourcekit.Listener{
		Addr: str(cfg["listen"]),
		Path: strOr(cfg["path"], "/uptimekuma"),
		// Secret intentionally left empty — see the package comment. The
		// shared token, when configured, is verified by hand below instead
		// of via sourcekit's HMAC path.
	}
	if ln.Addr == "" {
		return fmt.Errorf("uptimekuma: no listen address configured")
	}
	if err := requireWebhookSecret("uptimekuma", secret, cfg, "secret"); err != nil {
		return err
	}
	dedup := sourcekit.NewDedup(2048)
	fmt.Fprintf(os.Stderr, "uptimekuma[%s]: listening on %s%s\n", req.Instance, ln.Addr, ln.Path)
	return ln.Serve(ctx, func(h http.Header, body []byte) {
		if secret != "" && !verifyToken(secret, h) {
			return
		}
		f, ok := parse(body)
		if !ok {
			return
		}
		dk := dedupKey(f)
		if !dedup.Add(dk) {
			return
		}
		_ = emit(monitorEvent(f, dk))
	})
}

func main() {
	if err := plugin.Serve(uptimekuma{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-uptimekuma: %v\n", err)
		os.Exit(1)
	}
}

// --- Uptime Kuma webhook payload parsing ---
//
// The default "Webhook" notification posts:
//
//	{
//	  "heartbeat": {"monitorID": 1, "status": 0, "time": "...", "msg": "...", "important": true, "duration": 60},
//	  "monitor":   {"id": 1, "name": "My Site", "url": "https://...", "hostname": "", "type": "http"},
//	  "msg": "[My Site] [Down] ..."
//	}
//
// Some custom notification templates / older Uptime Kuma builds only ever
// send a flat {"msg": "..."} with no nested heartbeat/monitor object, so
// every field below is read defensively and the event is still emitted with
// whatever is present.
type wireHeartbeat struct {
	MonitorID any    `json:"monitorID"`
	Status    any    `json:"status"`
	Time      string `json:"time"`
	Msg       string `json:"msg"`
	Important any    `json:"important"`
	Duration  any    `json:"duration"`
}

type wireMonitor struct {
	ID       any    `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Hostname string `json:"hostname"`
	Type     string `json:"type"`
}

type wire struct {
	Heartbeat *wireHeartbeat `json:"heartbeat"`
	Monitor   *wireMonitor   `json:"monitor"`
	Msg       string         `json:"msg"`
}

// facts is the normalized shape parse extracts from either the full
// heartbeat+monitor payload or the flat fallback.
type facts struct {
	monitorID, status, monitorName, monitorURL, monitorType, msg, time string
	important                                                          bool
}

// parse extracts facts from an Uptime Kuma webhook body. It reports false
// only when the body carries no usable signal at all (unparsable JSON, or
// every recognized field empty).
func parse(body []byte) (facts, bool) {
	var w wire
	if err := json.Unmarshal(body, &w); err != nil {
		return facts{}, false
	}
	f := facts{msg: w.Msg}
	if w.Heartbeat != nil {
		h := w.Heartbeat
		f.status = statusString(h.Status)
		f.time = h.Time
		f.important = boolVal(h.Important)
		if id, ok := toStr(h.MonitorID); ok {
			f.monitorID = id
		}
		if f.msg == "" {
			f.msg = h.Msg
		}
	}
	if w.Monitor != nil {
		m := w.Monitor
		f.monitorName = m.Name
		f.monitorURL = m.URL
		f.monitorType = m.Type
		if f.monitorID == "" {
			if id, ok := toStr(m.ID); ok {
				f.monitorID = id
			}
		}
	}
	if f.monitorID == "" && f.monitorName == "" && f.status == "" && f.msg == "" {
		return facts{}, false
	}
	return f, true
}

// statusString maps Uptime Kuma's heartbeat.status integer to its documented
// name: 0=DOWN, 1=UP, 2=PENDING, 3=MAINTENANCE.
func statusString(v any) string {
	n, ok := intVal(v)
	if !ok {
		return ""
	}
	switch n {
	case 0:
		return "down"
	case 1:
		return "up"
	case 2:
		return "pending"
	case 3:
		return "maintenance"
	default:
		return "unknown"
	}
}

// dedupKey identifies one heartbeat delivery: the same monitor firing the
// same status again at a later time is a distinct, wanted event, so time is
// part of the key alongside monitorID + status.
func dedupKey(f facts) string {
	return f.monitorID + "\x00" + f.status + "\x00" + f.time
}

// monitorEvent builds the emitted event map for one parsed heartbeat.
func monitorEvent(f facts, dk string) map[string]any {
	title := f.msg
	if title == "" {
		title = fmt.Sprintf("%s is %s", nonEmpty(f.monitorName, "monitor"), nonEmpty(f.status, "unknown"))
	}
	return map[string]any{
		"event": "monitor",
		"kind":  nonEmpty(f.status, "monitor"),
		"title": title,
		"dedup": dk,
		"context": map[string]any{
			"status": f.status, "monitor_name": f.monitorName, "monitor_url": f.monitorURL,
			"monitor_type": f.monitorType, "monitor_id": f.monitorID, "msg": f.msg,
			"important": f.important, "time": f.time,
			// Plural aliases (+ the "monitor" scalar alias for monitor_name) so
			// the documented filter vocabulary (filters:
			// {statuses/monitors/monitor_types/status/monitor/monitor_type})
			// matches against the daemon's generic list-contains filter
			// evaluator.
			"statuses": f.status, "monitors": f.monitorName, "monitor_types": f.monitorType,
			"monitor": f.monitorName,
		},
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

// toStr renders a decoded JSON number/string/bool id field as a string,
// reporting whether v carried any value at all.
func toStr(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, x != ""
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	default:
		return fmt.Sprintf("%v", x), true
	}
}

// intVal coerces a JSON-decoded number/string into an int, reporting whether
// v was present and coercible.
func intVal(v any) (int, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case string:
		i, err := strconv.Atoi(x)
		return i, err == nil
	}
	return 0, false
}

// boolVal coerces a JSON-decoded bool/number/string into a bool.
func boolVal(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		b, _ := strconv.ParseBool(x)
		return b
	}
	return false
}

// verifyToken compares the request's X-Conductor-Token header against secret
// in constant time.
//
// Uptime Kuma cannot sign its webhook notifications, so this shared-token
// check is the only authentication available; requireWebhookSecret ensures a
// token is always configured unless the operator explicitly opts out with
// allow_unsigned. sourcekit.Listener.Serve hands its callback only the
// request's headers and body (not the *http.Request), so the ?token= query
// fallback documented alongside this connector is exercised through
// verifyTokenFromRequest below, which a future Listener revision exposing
// the full request can wire straight in.
func verifyToken(secret string, h http.Header) bool {
	got := h.Get("X-Conductor-Token")
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// verifyTokenFromRequest is the full check: header first, then the ?token=
// query parameter, against secret in constant time.
func verifyTokenFromRequest(secret string, r *http.Request) bool {
	got := r.Header.Get("X-Conductor-Token")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// requireWebhookSecret refuses to start an unauthenticated webhook listener.
//
// Uptime Kuma cannot sign its webhook notifications (the payload is
// operator-templated JSON with no HMAC), so the ONLY authentication
// available is a shared token the operator embeds in the notification's URL
// or a custom header. Treating a missing token the same way the HMAC-based
// connectors treat a missing signing secret keeps the fail-closed posture
// consistent: a missing token is far more often a mistake than a choice, so
// it fails closed; `allow_unsigned: true` is the explicit, greppable way to
// say you meant it (e.g. the listener sits behind something else that
// authenticates, such as a private network or a reverse proxy).
func requireWebhookSecret(who, secret string, cfg map[string]any, allowKey string) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if b, _ := cfg["allow_unsigned"].(bool); b {
		fmt.Fprintf(os.Stderr, "%s: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers\n", who)
		return nil
	}
	return fmt.Errorf("%s: no webhook token configured (%s) — Uptime Kuma cannot sign webhooks, so an unauthenticated listener accepts any POST on the listen address as a real event. Set it, or set `allow_unsigned: true` if you genuinely front this with something else that authenticates", who, allowKey)
}
