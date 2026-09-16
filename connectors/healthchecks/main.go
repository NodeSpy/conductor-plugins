// Command conductor-healthchecks is the Healthchecks.io connector as a
// standalone external conductor plugin (#59). It has TWO faces of one
// service: the MANAGEMENT API (create/inspect/pause/delete checks, read ping
// history — authenticated with an API key) and PINGING (the lightweight,
// unauthenticated "I'm alive" beacon a monitored job calls). As a source, it
// receives Healthchecks' own webhook-integration deliveries and streams a
// normalized "check" event per delivery. Built ONLY against the public SDK
// (pkg/plugin) and the connector-kit (pkg/sourcekit) — no other internal
// daemon package.
//
// Connection (used for both Invoke and StartSource):
//
//	api_key:  "<key>"                    # Healthchecks API key (management verbs only)
//	api_base: "https://healthchecks.io"  # management API base (self-hosted, or tests)
//	ping_base: "https://hc-ping.com"     # pinging base (self-hosted, or tests)
//	webhook:
//	  listen:         ":9097"            # HTTP listener address (StartSource only)
//	  path:           "/healthchecks"    # request path (default /healthchecks)
//	  secret:         "<shared token>"   # compared against ?token= or X-Conductor-Token
//	  allow_unsigned: false              # explicitly accept an unauthenticated listener
//
// Self-hosted Healthchecks: point api_base/ping_base at your instance and
// narrow the connector's declared egress (network:) to that host — the
// default Capabilities only cover the hosted healthchecks.io / hc-ping.com.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type healthchecksPlugin struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

func (p *healthchecksPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "healthchecks",
		Desc: "Healthchecks.io: manage checks and read ping history via the management API, send liveness pings, and receive check-state webhook events (source).",
		Connection: plugin.Schema{
			"api_key":   {Type: "string", Desc: "Healthchecks API key (X-Api-Key; management verbs only, never sent for ping)"},
			"api_base":  {Type: "string", Desc: "management API base (default https://healthchecks.io; override for self-hosted or tests)"},
			"ping_base": {Type: "string", Desc: "pinging base (default https://hc-ping.com; override for self-hosted or tests)"},
			"webhook":   {Type: "map", Desc: "source transport: listen, path, secret, allow_unsigned"},
		},
		Events: []plugin.Event{
			{
				Name: "check", Desc: "a Healthchecks webhook-integration delivery for a check state change",
				Filters: plugin.Schema{
					"statuses": {Type: "list", Desc: "e.g. up, down, started (empty = any)"},
					"checks":   {Type: "list", Desc: "check uuid (empty = any)"},
					"tags":     {Type: "list", Desc: "check tags (empty = any)"},
				},
				Context: plugin.Schema{
					"name":   {Type: "string"},
					"status": {Type: "string"},
					"uuid":   {Type: "string"},
					"tags":   {Type: "list"},
				},
			},
		},
		Verbs: []plugin.Verb{
			{
				Name: "list_checks", Desc: "list checks, optionally filtered by tag",
				Options: plugin.Schema{
					"tag": {Type: "any", Desc: "tag, or list of tags (AND-filtered)"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}},
			},
			{
				Name: "get_check", Desc: "read one check by uuid",
				Options: plugin.Schema{"uuid": {Type: "string", Required: true}},
				Outputs: plugin.Schema{"result": {Type: "map"}},
			},
			{
				Name: "create_check", Desc: "create a new check",
				Options: plugin.Schema{
					"name":     {Type: "string"},
					"tags":     {Type: "any", Desc: "space-joined tag string, or list of tags"},
					"timeout":  {Type: "integer", Desc: "expected period, seconds"},
					"grace":    {Type: "integer", Desc: "grace period, seconds"},
					"schedule": {Type: "string", Desc: "cron expression (use instead of timeout for a cron-style check)"},
					"tz":       {Type: "string", Desc: "timezone for schedule"},
				},
				Outputs: plugin.Schema{"result": {Type: "map"}},
			},
			{
				Name: "update_check", Desc: "update an existing check (partial)",
				Options: plugin.Schema{
					"uuid":    {Type: "string", Required: true},
					"name":    {Type: "string"},
					"tags":    {Type: "any", Desc: "space-joined tag string, or list of tags"},
					"timeout": {Type: "integer"},
					"grace":   {Type: "integer"},
				},
				Outputs: plugin.Schema{"result": {Type: "map"}},
			},
			{
				Name: "pause_check", Desc: "pause monitoring for a check",
				Options: plugin.Schema{"uuid": {Type: "string", Required: true}},
				Outputs: plugin.Schema{"result": {Type: "map"}},
			},
			{
				Name: "delete_check", Desc: "permanently delete a check",
				Options: plugin.Schema{"uuid": {Type: "string", Required: true}},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "ping", Desc: "send a liveness ping (no api key — this is the unauthenticated pinging surface)",
				Options: plugin.Schema{
					"uuid":   {Type: "string", Required: true},
					"status": {Type: "string", Enum: []string{"success", "fail", "start", "log"}, Desc: "default success"},
					"body":   {Type: "string", Desc: "optional diagnostic payload; sent as the POST body when non-empty"},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "get_pings", Desc: "recent ping events for a check",
				Options: plugin.Schema{"uuid": {Type: "string", Required: true}},
				Outputs: plugin.Schema{"items": {Type: "list"}},
			},
			{
				Name: "api", Desc: "escape hatch: any management API call",
				Options: plugin.Schema{
					"method": {Type: "string", Required: true, Enum: []string{"GET", "POST", "DELETE"}},
					"path":   {Type: "string", Required: true, Desc: "path under api_base, e.g. /api/v3/checks/"},
					"body":   {Type: "map", Desc: "JSON request body (POST)"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}},
			},
		},
		// Egress covers the hosted service. Self-hosted operators override
		// api_base/ping_base and should narrow/replace this via the
		// connector instance's own network: declaration.
		Capabilities: plugin.Capabilities{Egress: []string{"healthchecks.io:443", "hc-ping.com:443"}},
	}
}

// conn is the resolved connection config for one Invoke call.
type conn struct {
	apiKey   string
	apiBase  string
	pingBase string
	client   *http.Client
}

func (p *healthchecksPlugin) clientFor(instance string) *http.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = map[string]*http.Client{}
	}
	if c, ok := p.clients[instance]; ok {
		return c
	}
	c := &http.Client{Timeout: 30 * time.Second}
	p.clients[instance] = c
	return c
}

func (p *healthchecksPlugin) connFor(req plugin.InvokeRequest) conn {
	m := req.Connection
	return conn{
		apiKey:   str(m["api_key"]),
		apiBase:  strOr(m["api_base"], "https://healthchecks.io"),
		pingBase: strOr(m["ping_base"], "https://hc-ping.com"),
		client:   p.clientFor(req.Instance),
	}
}

func (p *healthchecksPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	c := p.connFor(req)
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	ctx := context.Background()
	switch req.Verb {
	case "list_checks":
		return listChecks(ctx, c, o)
	case "get_check":
		return getCheck(ctx, c, o)
	case "create_check":
		return createCheck(ctx, c, o)
	case "update_check":
		return updateCheck(ctx, c, o)
	case "pause_check":
		return pauseCheck(ctx, c, o)
	case "delete_check":
		return deleteCheck(ctx, c, o)
	case "ping":
		return doPing(ctx, c, o)
	case "get_pings":
		return getPings(ctx, c, o)
	case "api":
		return apiVerb(ctx, c, o)
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
}

// --- management API plumbing ---

// mgmt calls the management API at path (already includes the leading /),
// sending X-Api-Key and, for a non-nil body, a JSON-encoded request body. It
// returns the raw response body and status; the caller decides how to parse
// success and turns a non-2xx into the mandated plugin.Errorf(CodeInternalError, ...).
func mgmt(ctx context.Context, c conn, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.apiBase, "/")+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func mgmtErr(status int, body []byte) error {
	return plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("%d: %s", status, string(body)))
}

func ok2xx(status int) bool { return status >= 200 && status < 300 }

func listChecks(ctx context.Context, c conn, o map[string]any) (plugin.InvokeResult, error) {
	path := "/api/v3/checks/"
	tags := strList(o["tag"])
	if len(tags) > 0 {
		q := url.Values{}
		for _, t := range tags {
			q.Add("tag", t)
		}
		path += "?" + q.Encode()
	}
	status, data, err := mgmt(ctx, c, http.MethodGet, path, nil)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if !ok2xx(status) {
		return plugin.InvokeResult{}, mgmtErr(status, data)
	}
	var parsed struct {
		Checks []any `json:"checks"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "decoding response: "+err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": parsed.Checks}}, nil
}

func getCheck(ctx context.Context, c conn, o map[string]any) (plugin.InvokeResult, error) {
	uuid := str(o["uuid"])
	if uuid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uuid is required")
	}
	status, data, err := mgmt(ctx, c, http.MethodGet, "/api/v3/checks/"+uuid, nil)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if !ok2xx(status) {
		return plugin.InvokeResult{}, mgmtErr(status, data)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": jsonAny(data)}}, nil
}

// checkBody builds the create/update JSON body from the shared subset of
// options both verbs accept, omitting anything unset.
func checkBody(o map[string]any, includeSchedule bool) map[string]any {
	body := map[string]any{}
	if n := str(o["name"]); n != "" {
		body["name"] = n
	}
	if tags := strList(o["tags"]); len(tags) > 0 {
		body["tags"] = strings.Join(tags, " ")
	}
	if t, ok := intOpt(o["timeout"]); ok {
		body["timeout"] = t
	}
	if g, ok := intOpt(o["grace"]); ok {
		body["grace"] = g
	}
	if includeSchedule {
		if s := str(o["schedule"]); s != "" {
			body["schedule"] = s
		}
		if tz := str(o["tz"]); tz != "" {
			body["tz"] = tz
		}
	}
	return body
}

func createCheck(ctx context.Context, c conn, o map[string]any) (plugin.InvokeResult, error) {
	status, data, err := mgmt(ctx, c, http.MethodPost, "/api/v3/checks/", checkBody(o, true))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if !ok2xx(status) {
		return plugin.InvokeResult{}, mgmtErr(status, data)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": jsonAny(data)}}, nil
}

func updateCheck(ctx context.Context, c conn, o map[string]any) (plugin.InvokeResult, error) {
	uuid := str(o["uuid"])
	if uuid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uuid is required")
	}
	status, data, err := mgmt(ctx, c, http.MethodPost, "/api/v3/checks/"+uuid, checkBody(o, false))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if !ok2xx(status) {
		return plugin.InvokeResult{}, mgmtErr(status, data)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": jsonAny(data)}}, nil
}

func pauseCheck(ctx context.Context, c conn, o map[string]any) (plugin.InvokeResult, error) {
	uuid := str(o["uuid"])
	if uuid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uuid is required")
	}
	status, data, err := mgmt(ctx, c, http.MethodPost, "/api/v3/checks/"+uuid+"/pause", map[string]any{})
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if !ok2xx(status) {
		return plugin.InvokeResult{}, mgmtErr(status, data)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": jsonAny(data)}}, nil
}

func deleteCheck(ctx context.Context, c conn, o map[string]any) (plugin.InvokeResult, error) {
	uuid := str(o["uuid"])
	if uuid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uuid is required")
	}
	status, data, err := mgmt(ctx, c, http.MethodDelete, "/api/v3/checks/"+uuid, nil)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if !ok2xx(status) {
		return plugin.InvokeResult{}, mgmtErr(status, data)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"ok": true}}, nil
}

func getPings(ctx context.Context, c conn, o map[string]any) (plugin.InvokeResult, error) {
	uuid := str(o["uuid"])
	if uuid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uuid is required")
	}
	status, data, err := mgmt(ctx, c, http.MethodGet, "/api/v3/checks/"+uuid+"/pings/", nil)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if !ok2xx(status) {
		return plugin.InvokeResult{}, mgmtErr(status, data)
	}
	var parsed struct {
		Pings []any `json:"pings"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "decoding response: "+err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": parsed.Pings}}, nil
}

func apiVerb(ctx context.Context, c conn, o map[string]any) (plugin.InvokeResult, error) {
	method := strings.ToUpper(str(o["method"]))
	if method == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "method is required")
	}
	path := str(o["path"])
	if path == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "path is required")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	var body any
	if b, ok := o["body"].(map[string]any); ok {
		body = b
	}
	status, data, err := mgmt(ctx, c, method, path, body)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if !ok2xx(status) {
		return plugin.InvokeResult{}, mgmtErr(status, data)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": jsonAny(data)}}, nil
}

// --- pinging (unauthenticated) ---

func doPing(ctx context.Context, c conn, o map[string]any) (plugin.InvokeResult, error) {
	uuid := str(o["uuid"])
	if uuid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uuid is required")
	}
	status := strOr(o["status"], "success")
	suffix := ""
	switch status {
	case "success":
		suffix = ""
	case "fail":
		suffix = "/fail"
	case "start":
		suffix = "/start"
	case "log":
		suffix = "/log"
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "status must be one of success, fail, start, log")
	}
	body := str(o["body"])
	pingURL := strings.TrimRight(c.pingBase, "/") + "/" + uuid + suffix

	method := http.MethodGet
	var rdr io.Reader
	if body != "" {
		method = http.MethodPost
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, pingURL, rdr)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	// NO X-Api-Key: pinging is deliberately unauthenticated — the uuid in
	// the URL is the credential.
	resp, err := c.client.Do(req)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if !ok2xx(resp.StatusCode) {
		return plugin.InvokeResult{}, mgmtErr(resp.StatusCode, data)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"ok": true, "status_code": resp.StatusCode}}, nil
}

// --- source: Healthchecks webhook-integration deliveries ---

// hcWebhookPayload is the shape of the OPERATOR-TEMPLATED body Healthchecks'
// webhook integration POSTs. Healthchecks lets the operator configure the
// exact JSON via $NAME/$STATUS/$CODE/$TAGS template variables; the
// recommended (and expected) body is:
//
//	{"check":"$NAME","status":"$STATUS","uuid":"$CODE","tags":"$TAGS"}
type hcWebhookPayload struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	UUID   string `json:"uuid"`
	Tags   string `json:"tags"`
}

func (p *healthchecksPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	addr, path, secret := "", "/healthchecks", ""
	allowUnsigned := false
	if webhook != nil {
		addr = str(webhook["listen"])
		if p := str(webhook["path"]); p != "" {
			path = p
		}
		secret = str(webhook["secret"])
		allowUnsigned = boolv(webhook["allow_unsigned"])
	}
	if addr == "" {
		return fmt.Errorf("healthchecks: no webhook.listen address configured")
	}
	if err := requireWebhookToken(secret, allowUnsigned); err != nil {
		return err
	}
	dedup := sourcekit.NewDedup(2048)
	fmt.Fprintf(os.Stderr, "healthchecks[%s]: listening on %s%s\n", req.Instance, addr, path)

	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if !verifyWebhookToken(secret, r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var p hcWebhookPayload
		if err := json.Unmarshal(body, &p); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if p.UUID == "" && p.Status == "" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		dk := p.UUID + "\x00" + p.Status
		if dedup.Add(dk) {
			tags := splitTags(p.Tags)
			_ = emit(map[string]any{
				"event": "check",
				"kind":  "check",
				"title": fmt.Sprintf("healthchecks %s: %s", p.Status, p.Check),
				"dedup": dk,
				"context": map[string]any{
					"name": p.Check, "status": p.Status, "uuid": p.UUID, "tags": tags,
					// Aliases so the documented filter vocabulary
					// (filters: {statuses/checks/tags: [...]}) matches
					// against the daemon's generic list-contains filter
					// evaluator.
					"statuses": p.Status, "checks": p.UUID,
				},
			})
		}
		w.WriteHeader(http.StatusAccepted)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// verifyWebhookToken checks the shared token, from either the ?token= query
// parameter or the X-Conductor-Token header, against secret using a
// constant-time comparison. An empty secret means allow_unsigned was set at
// startup (requireWebhookToken already enforced that), so every request is
// accepted.
func verifyWebhookToken(secret string, r *http.Request) bool {
	if secret == "" {
		return true
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		token = r.Header.Get("X-Conductor-Token")
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
}

// requireWebhookToken refuses to start an unauthenticated webhook listener.
//
// Healthchecks' webhook integration has no signature scheme of its own — the
// only authentication available is a shared token the operator embeds in the
// configured URL (?token=) or a custom header. Omitting webhook.secret would
// silently accept ANY POST on the listen address as a real check-state
// event, which is remote trigger injection with no signal that it happened.
// A missing secret is far more often a mistake than a choice, so this fails
// closed; `webhook.allow_unsigned: true` is the explicit, greppable way to
// say you meant it.
func requireWebhookToken(secret string, allowUnsigned bool) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintln(os.Stderr, "healthchecks: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers")
		return nil
	}
	return fmt.Errorf("healthchecks: no webhook.secret configured — an unauthenticated listener accepts any POST on the listen address as a real event. Set it, or set `webhook.allow_unsigned: true` if you genuinely front this with something else that authenticates")
}

func splitTags(s string) []string {
	fields := strings.Fields(s)
	if fields == nil {
		return []string{}
	}
	return fields
}

func main() {
	if err := plugin.Serve(&healthchecksPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-healthchecks:", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func boolv(v any) bool {
	b, _ := v.(bool)
	return b
}

// strList accepts a string, []string, or []any and returns a non-empty slice.
func strList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s := fmt.Sprintf("%v", e); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// intOpt extracts an integer-ish option, reporting whether it was present.
func intOpt(v any) (int, bool) {
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
		if x == "" {
			return 0, false
		}
		var n int
		if _, err := fmt.Sscanf(x, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// jsonAny parses a JSON document (object or array) into a generic value; nil
// on failure so a caller never surfaces an unparsable body as data.
func jsonAny(data []byte) any {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil
	}
	return v
}
