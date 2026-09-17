// Command conductor-prowlarr is the Prowlarr connector as a standalone
// external conductor plugin. Prowlarr is a Servarr app too, but it manages
// INDEXERS rather than media: it drives a self-hosted Prowlarr instance's
// REST API v1 as verbs (indexers, a single indexer, indexer stats, the
// applications Prowlarr syncs indexers into, a release search across
// indexers, commands, system status, tags, and a generic `api` escape
// hatch), and as a SOURCE it receives Prowlarr's own Webhook notification
// POSTs (HealthIssue, HealthRestored, ApplicationUpdate, Test — Prowlarr has
// far fewer notification events than Sonarr since it has no media of its
// own to grab/download) and streams a normalized event per delivery. Built
// ONLY against the public SDK (pkg/plugin, pkg/sourcekit) — no conductor
// internals, no third-party dependencies.
//
// Every verb is a plain net/http call to {base_url}/api/v1/<resource>,
// authenticated with the X-Api-Key header. A non-2xx response is surfaced as
// a plugin error carrying the status code and response body.
//
// Prowlarr itself has no outbound webhook signing: its Connect > Webhook
// notification just POSTs an eventType-tagged JSON body to a URL. So the
// source verifies an OPTIONAL shared token instead of an HMAC signature —
// via the X-Conductor-Token header or a ?token= query parameter, compared
// against webhook.secret with a constant-time comparison — and fails closed
// exactly like the HMAC-verified sources do: a webhook with no secret
// configured refuses to start unless webhook.allow_unsigned: true says the
// operator means it. The shared sourcekit.Listener.ServeReq hands the
// callback the full request (headers, query, body), so the ?token= query
// case is checked directly — and the same Listener transparently accepts
// deliveries over a smee.io-style relay (webhook.smee) for endpoints with no
// public URL.
//
// Connection:
//
//	base_url: "http://prowlarr:9696" # REQUIRED: base of the instance (no trailing /api/v1)
//	api_key:  "<api key>"            # REQUIRED: Settings > General > Security
//	webhook:
//	  listen: ":9097"                 # HTTP listener address (StartSource only)
//	  path: "/prowlarr"                # request path (default /prowlarr)
//	  secret: "<shared token>"        # compared against X-Conductor-Token / ?token=
//	  allow_unsigned: false           # explicit opt-in to run with no shared token
//	  smee: "<smee.io channel URL>"   # optional relay for endpoints with no public URL
//
// The Prowlarr host is operator-specific and self-hosted, so this plugin
// declares NO egress in its capability manifest — the operator is expected to
// scope `network:` on the connector instance to their own Prowlarr host (see
// docs/connectors/prowlarr.md).
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

type prowlarrPlugin struct{}

func (prowlarrPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "prowlarr",
		Desc: "Prowlarr: indexer manager as verbs (indexers, indexer_get, indexer_stats, applications, search, command, system_status, tags, api escape hatch) over REST API v1; HealthIssue/HealthRestored/ApplicationUpdate/Test events in via Prowlarr's own Webhook connection (source).",
		Connection: plugin.Schema{
			"base_url": {Type: "string", Required: true, Desc: "base URL of the Prowlarr instance, e.g. http://prowlarr:9696 (no trailing /api/v1)"},
			"api_key":  {Type: "string", Required: true, Desc: "Prowlarr API key (Settings > General > Security)"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path (default /prowlarr), secret, allow_unsigned, smee"},
		},
		Events: []plugin.Event{prowlarrEvent()},
		Verbs:  prowlarrVerbs(),
		// Prowlarr is always self-hosted — there is no fixed hostname this
		// plugin can declare the way api.github.com is fixed for the github
		// connector. An empty manifest is the honest declaration; the
		// operator MUST narrow `network:` on the connector instance to their
		// own instance's host themselves (documented in
		// docs/connectors/prowlarr.md).
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func prowlarrEvent() plugin.Event {
	return plugin.Event{
		Name: "event",
		Desc: "a Prowlarr webhook notification fired (HealthIssue, HealthRestored, ApplicationUpdate, Test — whatever notifications you enable on the Webhook connection in Prowlarr)",
		Filters: plugin.Schema{
			"event_types": {Type: "list", Desc: "match payload.eventType against any of these (empty = any)"},
		},
		Context: plugin.Schema{
			"event_type":       {Type: "string", Desc: "e.g. HealthIssue, HealthRestored, ApplicationUpdate, Test"},
			"level":            {Type: "string", Desc: "health check level (ok/warning/error), when present"},
			"message":          {Type: "string", Desc: "the human-readable message, when present"},
			"issue_type":       {Type: "string", Desc: "the health check's `type` field identifying which check fired, when present"},
			"wiki_url":         {Type: "string", Desc: "a link to the relevant Servarr wiki page, when present"},
			"previous_version": {Type: "string", Desc: "ApplicationUpdate: the version updated from, when present"},
			"new_version":      {Type: "string", Desc: "ApplicationUpdate: the version updated to, when present"},
			"payload":          {Type: "any", Desc: "the full posted JSON body, verbatim"},
		},
	}
}

func prowlarrVerbs() []plugin.Verb {
	resultOut := plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}}
	itemsOut := plugin.Schema{"status_code": {Type: "integer"}, "items": {Type: "list"}}
	return []plugin.Verb{
		{
			Name:    "indexers",
			Desc:    "list all indexers configured in Prowlarr",
			Outputs: itemsOut,
		},
		{
			Name: "indexer_get",
			Desc: "a single indexer by id",
			Options: plugin.Schema{
				"id": {Type: "integer", Required: true},
			},
			Outputs: resultOut,
		},
		{
			Name:    "indexer_stats",
			Desc:    "aggregated per-indexer statistics (query/grab counts, response times, failures)",
			Outputs: resultOut,
		},
		{
			Name:    "applications",
			Desc:    "list the applications Prowlarr syncs indexers into (Sonarr/Radarr/etc.)",
			Outputs: itemsOut,
		},
		{
			Name: "search", Desc: "search for a release across indexers",
			Options: plugin.Schema{
				"query":       {Type: "string", Desc: "search term; omit for an indexer's default/RSS query"},
				"indexer_ids": {Type: "list", Desc: "restrict the search to these indexer ids (empty = all enabled indexers)"},
				"categories":  {Type: "list", Desc: "restrict the search to these Newznab/Torznab category ids"},
				"type":        {Type: "string", Desc: "search type, e.g. search, tv-search, movie-search"},
			},
			Outputs: itemsOut,
		},
		{
			Name: "command", Desc: "run a Prowlarr command",
			Usage: "e.g. ApplicationIndexerSync, IndexerSync, CheckHealth",
			Options: plugin.Schema{
				"name":   {Type: "string", Required: true, Desc: "command name, e.g. ApplicationIndexerSync, IndexerSync, CheckHealth"},
				"params": {Type: "map", Desc: "additional command-specific parameters, merged in verbatim"},
			},
			Outputs: resultOut,
		},
		{
			Name:    "system_status",
			Desc:    "instance version and system information",
			Outputs: resultOut,
		},
		{
			Name:    "tags",
			Desc:    "configured tags",
			Outputs: itemsOut,
		},
		{
			Name: "api", Desc: "call any Prowlarr API v1 endpoint not covered by a first-class verb",
			Usage: "escape hatch: method + path (relative to /api/v1) + query + body",
			Options: plugin.Schema{
				"method": {Type: "string", Desc: "default GET"},
				"path":   {Type: "string", Required: true, Desc: "path relative to /api/v1, e.g. /system/status"},
				"query":  {Type: "map", Desc: "query parameters"},
				"body":   {Type: "any", Desc: "request body, marshaled to JSON"},
			},
			Outputs: plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}, "items": {Type: "list"}},
		},
	}
}

// --- request building (pure, hermetically testable — no network) ----------

// reqBuild is what one verb call resolves to: the HTTP method, the path
// relative to /api/v1, its query parameters, an optional JSON body, and
// whether the response should be hoisted into outputs.items (a list) rather
// than outputs.result.
type reqBuild struct {
	method string
	path   string
	query  url.Values
	body   any
	asList bool
}

func buildRequest(verb string, o map[string]any) (reqBuild, error) {
	switch verb {
	case "indexers":
		return reqBuild{method: http.MethodGet, path: "/indexer", query: url.Values{}, asList: true}, nil

	case "indexer_get":
		id, err := requiredInt(o, "id")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/indexer/" + strconv.FormatInt(id, 10), query: url.Values{}}, nil

	case "indexer_stats":
		return reqBuild{method: http.MethodGet, path: "/indexerstats", query: url.Values{}}, nil

	case "applications":
		return reqBuild{method: http.MethodGet, path: "/applications", query: url.Values{}, asList: true}, nil

	case "search":
		return buildSearch(o), nil

	case "command":
		return buildCommand(o)

	case "system_status":
		return reqBuild{method: http.MethodGet, path: "/system/status", query: url.Values{}}, nil

	case "tags":
		return reqBuild{method: http.MethodGet, path: "/tag", query: url.Values{}, asList: true}, nil

	case "api":
		return buildAPI(o)
	}
	return reqBuild{}, fmt.Errorf("unknown verb %q", verb)
}

// buildSearch builds GET /search. `indexer_ids` is joined into a single
// comma-separated `indexerIds` parameter; `categories` is repeated as
// multiple `categories=` parameters — both match the query shape Prowlarr's
// own UI sends.
func buildSearch(o map[string]any) reqBuild {
	q := url.Values{}
	if v := str(o["query"]); v != "" {
		q.Set("query", v)
	}
	if ids := toStringSlice(o["indexer_ids"]); len(ids) > 0 {
		q.Set("indexerIds", strings.Join(ids, ","))
	}
	for _, c := range toStringSlice(o["categories"]) {
		q.Add("categories", c)
	}
	if v := str(o["type"]); v != "" {
		q.Set("type", v)
	}
	return reqBuild{method: http.MethodGet, path: "/search", query: q, asList: true}
}

// buildCommand builds POST /command from the required name plus any
// caller-supplied params merged in verbatim.
func buildCommand(o map[string]any) (reqBuild, error) {
	name, err := requiredStr(o, "name")
	if err != nil {
		return reqBuild{}, err
	}
	body := map[string]any{"name": name}
	if params, ok := o["params"].(map[string]any); ok {
		for k, v := range params {
			body[k] = v
		}
	}
	return reqBuild{method: http.MethodPost, path: "/command", query: url.Values{}, body: body}, nil
}

// buildAPI builds the generic escape-hatch request.
func buildAPI(o map[string]any) (reqBuild, error) {
	path, err := requiredStr(o, "path")
	if err != nil {
		return reqBuild{}, err
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	method := strOr(o["method"], http.MethodGet)
	q := url.Values{}
	if m, ok := o["query"].(map[string]any); ok {
		for k, v := range m {
			q.Set(k, fmt.Sprintf("%v", v))
		}
	}
	return reqBuild{method: strings.ToUpper(method), path: path, query: q, body: o["body"]}, nil
}

// requiredStr reads o[key] as a non-empty string.
func requiredStr(o map[string]any, key string) (string, error) {
	s := str(o[key])
	if s == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return s, nil
}

// requiredInt reads o[key] as a non-zero integer, accepting the numeric
// shapes JSON options commonly arrive as.
func requiredInt(o map[string]any, key string) (int64, error) {
	v, ok := toInt64(o[key])
	if !ok {
		return 0, fmt.Errorf("%s is required", key)
	}
	return v, nil
}

// --- Invoke -----------------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

func (prowlarrPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	if conn == nil {
		conn = map[string]any{}
	}
	baseURL := strings.TrimSpace(str(conn["base_url"]))
	if baseURL == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.base_url is required (e.g. http://prowlarr:9696)")
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, raw, err := doRequest(ctx, baseURL, apiKey, rb)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	if status < 200 || status >= 300 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s: %s %s: %d: %s", req.Verb, rb.method, rb.path, status, strings.TrimSpace(string(raw))))
	}

	outputs := map[string]any{"status_code": status}
	data := decodeBody(raw)
	if req.Verb == "api" {
		outputs["result"] = data
		if list, ok := data.([]any); ok {
			outputs["items"] = list
		}
		return plugin.InvokeResult{Outputs: outputs}, nil
	}
	if rb.asList {
		outputs["items"] = asItems(data)
	} else {
		outputs["result"] = data
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

// apiURL derives the API base (rawBase + /api/v1) — kept as its own function
// so a test can point it at an httptest.Server.
func apiURL(rawBase string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(rawBase), "/")
	if u == "" {
		return "", fmt.Errorf("connection.base_url is required (e.g. http://prowlarr:9696)")
	}
	return u + "/api/v1", nil
}

func doRequest(ctx context.Context, baseURL, apiKey string, rb reqBuild) (status int, raw []byte, err error) {
	base, err := apiURL(baseURL)
	if err != nil {
		return 0, nil, err
	}
	full := base + rb.path
	if q := rb.query.Encode(); q != "" {
		full += "?" + q
	}
	var bodyReader io.Reader
	if rb.body != nil {
		encoded, mErr := json.Marshal(rb.body)
		if mErr != nil {
			return 0, nil, mErr
		}
		bodyReader = bytes.NewReader(encoded)
	}
	httpReq, err := http.NewRequestWithContext(ctx, rb.method, full, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	httpReq.Header.Set("X-Api-Key", apiKey)
	if rb.body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(httpReq)
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

// decodeBody parses a (possibly empty) JSON response body into a generic
// value; nil for an empty body.
func decodeBody(raw []byte) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return nil
	}
	return v
}

// asItems normalizes a decoded response into a list: the value itself when it
// already is one, or an empty list otherwise. Never nil.
func asItems(data any) []any {
	if list, ok := data.([]any); ok {
		return list
	}
	return []any{}
}

// --- source: Prowlarr Webhook connection ---------------------------------------

// requireToken refuses to start an unauthenticated webhook listener.
//
// Prowlarr's Webhook connection has no signing of its own — it just POSTs an
// eventType-tagged JSON body. Omitting webhook.secret would silently accept
// ANY unsigned POST on the listen address as a real event, which is remote
// trigger injection with no signal that it happened. A missing secret is far
// more often a mistake than a choice, so it fails closed; `webhook.allow_
// unsigned: true` is the explicit, greppable way to say you meant it.
func requireToken(secret string, allowUnsigned bool) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintln(os.Stderr, "prowlarr: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers")
		return nil
	}
	return fmt.Errorf("prowlarr: no webhook.secret configured — an unauthenticated listener accepts any POST on the listen address as a real event. Set webhook.secret (and put it in Prowlarr's Webhook connection as an X-Conductor-Token header or ?token= query param), or set webhook.allow_unsigned: true if you genuinely front this with something else that authenticates")
}

// tokenEqual is a constant-time comparison that also normalizes away the
// length signal a naive == or subtle.ConstantTimeCompare(unequal-length)
// would leak, by comparing SHA-256 digests instead of the raw strings.
func tokenEqual(got, want string) bool {
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

// verifyToken compares the request's X-Conductor-Token header, or (when the
// header is absent) its ?token= query parameter, against secret.
func verifyToken(secret string, rq *sourcekit.Request) bool {
	tok := rq.Header.Get("X-Conductor-Token")
	if tok == "" {
		tok = rq.Query.Get("token")
	}
	return tokenEqual(tok, secret)
}

func (prowlarrPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	wh, _ := cfg["webhook"].(map[string]any)
	if wh == nil {
		wh = map[string]any{}
	}
	addr := str(wh["listen"])
	path := strOr(wh["path"], "/prowlarr")
	secret := str(wh["secret"])
	allowUnsigned := boolv(wh["allow_unsigned"])
	smeeURL := str(wh["smee"])

	if addr == "" && smeeURL == "" {
		return fmt.Errorf("prowlarr: no webhook.listen address or webhook.smee relay configured")
	}
	if err := requireToken(secret, allowUnsigned); err != nil {
		return err
	}

	dedup := sourcekit.NewDedup(2048)
	if addr != "" {
		fmt.Fprintf(os.Stderr, "prowlarr[%s]: listening on %s%s\n", req.Instance, addr, path)
	}
	if smeeURL != "" {
		fmt.Fprintf(os.Stderr, "prowlarr[%s]: relaying via smee channel %s\n", req.Instance, smeeURL)
	}
	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smeeURL}
	return ln.ServeReq(ctx, func(rq *sourcekit.Request) {
		if secret != "" && !verifyToken(secret, rq) {
			return
		}
		handleWebhookBody(rq.Body, dedup, emit)
	})
}

// handleWebhookBody parses one Prowlarr Webhook delivery and emits a
// normalized event. Deliveries missing eventType are dropped rather than
// deduped/emitted blind — there is nothing to build a sensible event from.
func handleWebhookBody(body []byte, dedup *sourcekit.Dedup, emit func(any) error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return
	}
	eventType, _ := payload["eventType"].(string)
	if eventType == "" {
		return
	}

	level, _ := payload["level"].(string)
	message, _ := payload["message"].(string)
	issueType, _ := payload["type"].(string)
	wikiURL, _ := payload["wikiUrl"].(string)
	previousVersion, _ := payload["previousVersion"].(string)
	newVersion, _ := payload["newVersion"].(string)

	dedupKey := dedupKey(eventType, message, issueType)
	if !dedup.Add(dedupKey) {
		return
	}

	title := fmt.Sprintf("prowlarr %s", eventType)
	if message != "" {
		title = fmt.Sprintf("prowlarr %s: %s", eventType, message)
	}

	_ = emit(map[string]any{
		"event": "event",
		"kind":  eventType,
		"title": title,
		"dedup": dedupKey,
		"context": map[string]any{
			"event_type":       eventType,
			"level":            level,
			"message":          message,
			"issue_type":       issueType,
			"wiki_url":         wikiURL,
			"previous_version": previousVersion,
			"new_version":      newVersion,
			"payload":          payload,
			// Filter-key alias for the daemon's generic list-contains filter
			// evaluator (filters: {event_types: [...]}).
			"event_types": eventType,
		},
	})
}

// dedupKey builds the dedup key: eventType + (message or issueType, when
// present, else a per-second timestamp so redeliveries of the SAME event
// within the same wall-clock second still collapse, but distinct deliveries
// with no distinguishing field are not incorrectly merged across time).
func dedupKey(eventType, message, issueType string) string {
	disambiguator := message
	if disambiguator == "" {
		disambiguator = issueType
	}
	if disambiguator == "" {
		disambiguator = strconv.FormatInt(time.Now().Unix(), 10)
	}
	return fmt.Sprintf("%s\x00%s", eventType, disambiguator)
}

func main() {
	if err := plugin.Serve(prowlarrPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-prowlarr: %v\n", err)
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

// toStringSlice accepts the list shapes JSON options arrive as ([]any of
// strings/numbers, or []string), stringifying each element.
func toStringSlice(v any) []string {
	switch xs := v.(type) {
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			out = append(out, fmt.Sprintf("%v", x))
		}
		return out
	case []string:
		return xs
	}
	return nil
}
