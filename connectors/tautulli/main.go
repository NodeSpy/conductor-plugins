// Command conductor-tautulli is the Tautulli connector as a standalone
// external conductor plugin (#59). It drives a self-hosted Tautulli
// instance's HTTP API as verbs (activity, history, libraries, users,
// metadata, recently added, server info, notify, terminate session, and a
// generic `api` escape hatch), and as a SOURCE it receives Tautulli's
// "Webhook" notification-agent POSTs (playback start/stop/pause, transcode
// decision, etc. — whatever the operator wires up in Tautulli's own
// notification triggers) and streams a normalized event per delivery. Built
// ONLY against the public SDK (pkg/plugin, pkg/sourcekit) — no conductor
// internals, no third-party dependencies.
//
// Tautulli's entire HTTP API is ONE endpoint: every call is a GET to
// {base_url}/api/v2 with apikey and cmd as query parameters, plus whatever
// parameters the command itself takes (see
// https://github.com/Tautulli/Tautulli/wiki/Tautulli-API-Reference). This
// plugin mirrors that shape directly rather than modeling per-verb REST
// paths. The response envelope is always
// {"response": {"result": "success"|"error", "message": ..., "data": ...}} —
// a "result": "error" is surfaced as a plugin error carrying `message`.
//
// Tautulli itself has no outbound webhook signing: its "Webhook" notification
// agent just POSTs an operator-defined JSON body (built from Tautulli's own
// notification template variables) to a URL. So the source verifies an
// OPTIONAL shared token instead of an HMAC signature — via the
// X-Conductor-Token header or a ?token= query parameter, compared against
// webhook.secret with a constant-time comparison — and fails closed exactly
// like the HMAC-verified sources do: a webhook with no secret configured
// refuses to start unless webhook.allow_unsigned: true says the operator
// means it.
//
// Connection:
//
//	base_url: "http://tautulli:8181"  # REQUIRED: base of the instance (no trailing /api/v2)
//	api_key:  "<api key>"             # REQUIRED: Settings > Web Interface > API
//	webhook:
//	  listen: ":9097"                  # HTTP listener address (StartSource only)
//	  path: "/tautulli"                # request path (default /tautulli)
//	  secret: "<shared token>"         # compared against X-Conductor-Token / ?token=
//	  allow_unsigned: false            # explicit opt-in to run with no shared token
//
// The Tautulli host is operator-specific and self-hosted, so this plugin
// declares NO egress in its capability manifest — the operator is expected to
// scope `network:` on the connector instance to their own Tautulli host (see
// docs/connectors/tautulli.md).
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type tautulliPlugin struct{}

func (tautulliPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "tautulli",
		Desc: "Tautulli: Plex monitoring/stats as verbs (activity, history, libraries, users, metadata, recently_added, server_info, notify, terminate_session, api escape hatch) over the single GET /api/v2 endpoint; playback events in via Tautulli's own Webhook notification agent (source).",
		Connection: plugin.Schema{
			"base_url": {Type: "string", Required: true, Desc: "base URL of the Tautulli instance, e.g. http://tautulli:8181 (no trailing /api/v2)"},
			"api_key":  {Type: "string", Required: true, Desc: "Tautulli API key (Settings > Web Interface > API)"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path (default /tautulli), secret, allow_unsigned"},
		},
		Events: []plugin.Event{tautulliEvent()},
		Verbs:  tautulliVerbs(),
		// Tautulli is always self-hosted — there is no fixed hostname this
		// plugin can declare the way api.github.com is fixed for the github
		// connector. An empty manifest is the honest declaration; the
		// operator MUST narrow `network:` on the connector instance to their
		// own instance's host themselves (documented in
		// docs/connectors/tautulli.md).
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func tautulliEvent() plugin.Event {
	return plugin.Event{
		Name: "event",
		Desc: "a Tautulli webhook notification fired (playback start/stop/pause, transcode decision, etc. — whatever notification triggers the operator wired to the Webhook agent in Tautulli)",
		Filters: plugin.Schema{
			"actions":     {Type: "list", Desc: "match payload.action against any of these (empty = any)"},
			"users":       {Type: "list", Desc: "match payload.user against any of these (empty = any)"},
			"media_types": {Type: "list", Desc: "match payload.media_type against any of these (empty = any)"},
			"action":      {Type: "string"},
			"user":        {Type: "string"},
			"media_type":  {Type: "string"},
		},
		Context: plugin.Schema{
			"action":     {Type: "string", Desc: "e.g. play, pause, resume, stop, transcode_decision"},
			"title":      {Type: "string"},
			"user":       {Type: "string"},
			"player":     {Type: "string"},
			"media_type": {Type: "string"},
			"rating_key": {Type: "string"},
			"payload":    {Type: "any", Desc: "the full posted JSON body, verbatim"},
		},
	}
}

func tautulliVerbs() []plugin.Verb {
	resultOut := plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}}
	itemsOut := plugin.Schema{"status_code": {Type: "integer"}, "items": {Type: "list"}}
	return []plugin.Verb{
		{
			Name:    "activity",
			Desc:    "current Plex activity (active streams, transcode sessions)",
			Outputs: resultOut,
		},
		{
			Name: "history",
			Desc: "playback history (data.data hoisted into items)",
			Options: plugin.Schema{
				"user":       {Type: "string", Desc: "filter to one user"},
				"section_id": {Type: "string", Desc: "filter to one library section"},
				"length":     {Type: "integer", Desc: "max rows to return"},
				"start":      {Type: "integer", Desc: "row offset"},
			},
			Outputs: itemsOut,
		},
		{
			Name:    "home_stats",
			Desc:    "the home page's \"most watched\"/\"recently added\" style stat blocks",
			Outputs: resultOut,
		},
		{
			Name:    "libraries",
			Desc:    "configured Plex libraries (hoisted into items)",
			Outputs: itemsOut,
		},
		{
			Name:    "users",
			Desc:    "known Plex users (hoisted into items)",
			Outputs: itemsOut,
		},
		{
			Name: "metadata",
			Desc: "metadata for a single Plex item",
			Options: plugin.Schema{
				"rating_key": {Type: "string", Required: true, Desc: "the Plex rating key of the item"},
			},
			Outputs: resultOut,
		},
		{
			Name: "recently_added",
			Desc: "recently added media (hoisted into items)",
			Options: plugin.Schema{
				"count":      {Type: "integer", Desc: "max rows to return"},
				"section_id": {Type: "string", Desc: "filter to one library section"},
			},
			Outputs: itemsOut,
		},
		{
			Name:    "server_info",
			Desc:    "the connected Plex Media Server's identity/version",
			Outputs: resultOut,
		},
		{
			Name: "notify",
			Desc: "send a notification through a configured Tautulli notifier",
			Options: plugin.Schema{
				"notifier_id": {Type: "integer", Required: true, Desc: "id of the Tautulli notifier agent to use"},
				"subject":     {Type: "string", Required: true},
				"body":        {Type: "string", Required: true},
			},
			Outputs: resultOut,
		},
		{
			Name: "terminate_session",
			Desc: "terminate an active Plex playback session",
			Options: plugin.Schema{
				"session_key": {Type: "string", Required: true, Desc: "the Plex session key to terminate"},
				"message":     {Type: "string", Desc: "message shown to the user"},
			},
			Outputs: resultOut,
		},
		{
			Name:  "api",
			Desc:  "call any Tautulli API command not covered by a first-class verb",
			Usage: "escape hatch: cmd + arbitrary query params",
			Options: plugin.Schema{
				"cmd":    {Type: "string", Required: true, Desc: "the Tautulli API command, e.g. get_plex_log"},
				"params": {Type: "map", Desc: "additional query parameters for the command"},
			},
			Outputs: resultOut,
		},
	}
}

// --- request building (pure, hermetically testable — no network) ----------

// reqBuild is what one verb call resolves to: the Tautulli cmd, its
// cmd-specific query parameters (apikey/cmd are added centrally by Invoke),
// and whether the response's data should be hoisted into outputs.items.
type reqBuild struct {
	cmd    string
	query  url.Values
	asList bool
}

func buildRequest(verb string, o map[string]any) (reqBuild, error) {
	switch verb {
	case "activity":
		return reqBuild{cmd: "get_activity", query: url.Values{}}, nil

	case "history":
		q := url.Values{}
		if v := str(o["user"]); v != "" {
			q.Set("user", v)
		}
		if v := str(o["section_id"]); v != "" {
			q.Set("section_id", v)
		}
		if v := intStr(o["length"]); v != "" {
			q.Set("length", v)
		}
		if v := intStr(o["start"]); v != "" {
			q.Set("start", v)
		}
		return reqBuild{cmd: "get_history", query: q, asList: true}, nil

	case "home_stats":
		return reqBuild{cmd: "get_home_stats", query: url.Values{}}, nil

	case "libraries":
		return reqBuild{cmd: "get_libraries", query: url.Values{}, asList: true}, nil

	case "users":
		return reqBuild{cmd: "get_users", query: url.Values{}, asList: true}, nil

	case "metadata":
		ratingKey, err := requiredStr(o, "rating_key")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{cmd: "get_metadata", query: url.Values{"rating_key": {ratingKey}}}, nil

	case "recently_added":
		q := url.Values{}
		if v := intStr(o["count"]); v != "" {
			q.Set("count", v)
		}
		if v := str(o["section_id"]); v != "" {
			q.Set("section_id", v)
		}
		return reqBuild{cmd: "get_recently_added", query: q, asList: true}, nil

	case "server_info":
		return reqBuild{cmd: "get_server_info", query: url.Values{}}, nil

	case "notify":
		notifierID, err := requiredStr(o, "notifier_id")
		if err != nil {
			return reqBuild{}, err
		}
		subject, err := requiredStr(o, "subject")
		if err != nil {
			return reqBuild{}, err
		}
		body, err := requiredStr(o, "body")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{cmd: "notify", query: url.Values{
			"notifier_id": {notifierID}, "subject": {subject}, "body": {body},
		}}, nil

	case "terminate_session":
		sessionKey, err := requiredStr(o, "session_key")
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{"session_key": {sessionKey}}
		if v := str(o["message"]); v != "" {
			q.Set("message", v)
		}
		return reqBuild{cmd: "terminate_session", query: q}, nil

	case "api":
		cmd, err := requiredStr(o, "cmd")
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{}
		if m, ok := o["params"].(map[string]any); ok {
			for k, v := range m {
				q.Set(k, fmt.Sprintf("%v", v))
			}
		}
		return reqBuild{cmd: cmd, query: q}, nil
	}
	return reqBuild{}, fmt.Errorf("unknown verb %q", verb)
}

// requiredStr reads o[key] as a non-empty string, accepting numeric option
// types too (notifier_id commonly arrives as a JSON number).
func requiredStr(o map[string]any, key string) (string, error) {
	v := o[key]
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case nil:
		s = ""
	default:
		s = intStr(v)
		if s == "" {
			s = fmt.Sprintf("%v", x)
		}
	}
	if s == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return s, nil
}

// --- Invoke -----------------------------------------------------------------

func (tautulliPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	if conn == nil {
		conn = map[string]any{}
	}
	baseURL := strings.TrimSpace(str(conn["base_url"]))
	if baseURL == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.base_url is required (e.g. http://tautulli:8181)")
	}
	apiKey := strings.TrimSpace(str(conn["api_key"]))
	if apiKey == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.api_key is required")
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	rb, err := buildRequest(req.Verb, o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}
	q := rb.query
	if q == nil {
		q = url.Values{}
	}
	q.Set("apikey", apiKey)
	q.Set("cmd", rb.cmd)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, raw, err := doRequest(ctx, baseURL, q)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	if status < 200 || status >= 300 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s: GET /api/v2: %d: %s", req.Verb, status, strings.TrimSpace(string(raw))))
	}

	data, apiErr := unwrapEnvelope(raw)
	if apiErr != "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+apiErr)
	}

	outputs := map[string]any{"status_code": status}
	if rb.asList {
		outputs["items"] = asItems(data)
	} else {
		outputs["result"] = data
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

// --- HTTP transport ----------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

// apiURL derives the API endpoint (base_url + /api/v2) — kept as its own
// function so a test can point it at an httptest.Server.
func apiURL(rawBase string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(rawBase), "/")
	if u == "" {
		return "", fmt.Errorf("connection.base_url is required (e.g. http://tautulli:8181)")
	}
	return u + "/api/v2", nil
}

func doRequest(ctx context.Context, baseURL string, query url.Values) (status int, raw []byte, err error) {
	base, err := apiURL(baseURL)
	if err != nil {
		return 0, nil, err
	}
	full := base + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, buf.Bytes(), nil
}

// envelope mirrors Tautulli's fixed response wrapper:
// {"response": {"result": "success"|"error", "message": ..., "data": ...}}.
type envelope struct {
	Response struct {
		Result  string `json:"result"`
		Message any    `json:"message"`
		Data    any    `json:"data"`
	} `json:"response"`
}

// unwrapEnvelope decodes the Tautulli response envelope. A non-empty apiErr
// return means response.result was "error" (or the body did not parse), and
// carries the message to surface to the caller.
func unwrapEnvelope(raw []byte) (data any, apiErr string) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, "invalid JSON response: " + err.Error()
	}
	if env.Response.Result != "success" {
		msg := fmt.Sprintf("%v", env.Response.Message)
		if env.Response.Message == nil || msg == "<nil>" || msg == "" {
			msg = "unknown error"
		}
		return nil, msg
	}
	return env.Response.Data, ""
}

// asItems normalizes a successful response's data into a list: data itself
// when it already is one, or the first well-known nested list key
// (get_history/get_recently_added wrap their rows one level down). Never nil
// — an empty list on anything unrecognized.
func asItems(data any) []any {
	switch v := data.(type) {
	case []any:
		return v
	case map[string]any:
		for _, key := range []string{"data", "recently_added", "items"} {
			if list, ok := v[key].([]any); ok {
				return list
			}
		}
	}
	return []any{}
}

// --- source: Tautulli Webhook notification agent ----------------------------

// requireToken refuses to start an unauthenticated webhook listener.
//
// Tautulli's Webhook agent has no signing of its own — it just POSTs
// operator-templated JSON. Omitting webhook.secret would silently accept ANY
// unsigned POST on the listen address as a real playback event, which is
// remote trigger injection with no signal that it happened. A missing secret
// is far more often a mistake than a choice, so it fails closed;
// `webhook.allow_unsigned: true` is the explicit, greppable way to say you
// meant it.
func requireToken(secret string, allowUnsigned bool) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintln(os.Stderr, "tautulli: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers")
		return nil
	}
	return fmt.Errorf("tautulli: no webhook.secret configured — an unauthenticated listener accepts any POST on the listen address as a real event. Set webhook.secret (and put it in Tautulli's Webhook agent as an X-Conductor-Token header or ?token= query param), or set webhook.allow_unsigned: true if you genuinely front this with something else that authenticates")
}

// tokenEqual is a constant-time comparison that also normalizes away the
// length signal a naive == or subtle.ConstantTimeCompare(unequal-length)
// would leak, by comparing SHA-256 digests instead of the raw strings.
func tokenEqual(got, want string) bool {
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

func (tautulliPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	wh, _ := cfg["webhook"].(map[string]any)
	if wh == nil {
		wh = map[string]any{}
	}
	addr := str(wh["listen"])
	path := strOr(wh["path"], "/tautulli")
	secret := str(wh["secret"])
	allowUnsigned := boolv(wh["allow_unsigned"])

	if addr == "" {
		return fmt.Errorf("tautulli: no webhook.listen address configured")
	}
	if err := requireToken(secret, allowUnsigned); err != nil {
		return err
	}

	dedup := sourcekit.NewDedup(2048)
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if secret != "" {
			tok := r.Header.Get("X-Conductor-Token")
			if tok == "" {
				tok = r.URL.Query().Get("token")
			}
			if !tokenEqual(tok, secret) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		handleWebhookBody(body, dedup, emit)
		w.WriteHeader(http.StatusAccepted)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	fmt.Fprintf(os.Stderr, "tautulli[%s]: listening on %s%s\n", req.Instance, addr, path)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// handleWebhookBody parses one Tautulli Webhook delivery and emits a
// normalized event. Deliveries missing both action and rating_key are
// dropped rather than deduped/emitted blind — there is nothing to build a
// sensible event or dedup key from.
func handleWebhookBody(body []byte, dedup *sourcekit.Dedup, emit func(any) error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return
	}
	action, _ := payload["action"].(string)
	title, _ := payload["title"].(string)
	user, _ := payload["user"].(string)
	player, _ := payload["player"].(string)
	mediaType, _ := payload["media_type"].(string)
	ratingKey := payloadStr(payload["rating_key"])

	if action == "" && ratingKey == "" {
		return
	}
	dk := ratingKey + "\x00" + action
	if ratingKey != "" && action != "" && !dedup.Add(dk) {
		return
	}

	ctxMap := map[string]any{}
	for k, v := range payload {
		ctxMap[k] = v
	}
	ctxMap["payload"] = payload
	ctxMap["actions"] = action
	ctxMap["users"] = user
	ctxMap["media_types"] = mediaType

	_ = emit(map[string]any{
		"event": "event",
		"kind":  nonEmpty(action, "event"),
		"title": fmt.Sprintf("tautulli %s: %s", nonEmpty(action, "event"), title),
		"dedup": dk,
		"context": mergeInto(ctxMap, map[string]any{
			"action": action, "title": title, "user": user,
			"player": player, "media_type": mediaType, "rating_key": ratingKey,
		}),
	})
}

// mergeInto layers override on top of base and returns base, so the
// well-known, typed fields win over whatever the operator's raw payload
// happened to name the same key.
func mergeInto(base, override map[string]any) map[string]any {
	for k, v := range override {
		base[k] = v
	}
	return base
}

func payloadStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
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

func main() {
	if err := plugin.Serve(tautulliPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-tautulli: %v\n", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) -------------------------------------------

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func boolv(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	}
	return false
}

// toInt64 accepts the numeric shapes JSON options arrive as.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	}
	return 0, false
}

// intStr renders an integer-ish option as a query-string value ("" if absent).
func intStr(v any) string {
	if v == nil {
		return ""
	}
	if n, ok := toInt64(v); ok {
		return strconv.FormatInt(n, 10)
	}
	return ""
}
