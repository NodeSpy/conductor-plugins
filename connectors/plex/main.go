// Command conductor-plex is the Plex Media Server connector as a standalone
// external conductor plugin (#59). It drives a Plex Media Server's HTTP API as
// verbs (sessions, library sections, library scan, search, metadata,
// recently-added, mark watched/unwatched, refresh metadata, identity,
// playlists, and a generic `api` escape hatch), and as a SOURCE it receives
// Plex's own webhook notifications (playback start/pause/stop/resume/scrobble/
// rate) and streams a normalized "playback" event per delivery. Built ONLY
// against the public SDK (pkg/plugin, pkg/sourcekit) — no conductor internals,
// no third-party dependencies.
//
// Plex's JSON API wraps every response in a MediaContainer object; this
// plugin unwraps it uniformly: the MediaContainer itself becomes `result`,
// and whichever of its fields holds a JSON array (Video, Directory, Metadata,
// Playlist, …) becomes `items`. Every verb call carries the token as the
// X-Plex-Token header (never a query parameter) plus Accept: application/json
// so Plex returns JSON instead of XML.
//
// Connection:
//
//	base_url: "http://plex:32400"  # REQUIRED: base of the Plex Media Server (overridable in tests)
//	token:    "<X-Plex-Token>"     # REQUIRED: https://support.plex.tv/articles/204059436
//	webhook:
//	  listen: ":9096"               # HTTP listener address (StartSource only)
//	  path: "/plex"                 # request path (default /plex)
//	  secret: "<shared token>"      # compared against ?token= on the webhook URL
//	  allow_unsigned: false         # explicit opt-in to run with no shared token
//
// Plex has no signing of its own for outbound webhooks (a Plex Pass feature
// that just POSTs multipart/form-data to a URL you configure in Settings >
// Webhooks) — the `payload` form field is the JSON event body. So the source
// verifies an OPTIONAL shared token via a ?token= query parameter on the
// webhook URL itself, compared against webhook.secret with a constant-time
// comparison, and fails closed exactly like the HMAC-verified sources do: no
// secret configured refuses to start unless webhook.allow_unsigned: true says
// the operator means it.
//
// Plex is always self-hosted/LAN, so this plugin declares NO egress in its
// capability manifest — the operator is expected to scope `network:` on the
// connector instance to their own server (see docs/connectors/plex.md).
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
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type plexPlugin struct{}

func (plexPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "plex",
		Desc: "Plex Media Server: sessions, library sections, scan/refresh, search, metadata, recently-added, watched-state, identity, playlists, and an api escape hatch as verbs; source receives Plex's own webhook playback notifications.",
		Connection: plugin.Schema{
			"base_url": {Type: "string", Required: true, Desc: "base URL of the Plex Media Server, e.g. http://plex:32400"},
			"token":    {Type: "string", Required: true, Desc: "X-Plex-Token (Settings > Account, or see support.plex.tv/articles/204059436)"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path (default /plex), secret, allow_unsigned"},
		},
		Events: []plugin.Event{plexEvent()},
		Verbs:  plexVerbs(),
		// Plex is always self-hosted/LAN — there is no fixed hostname this
		// plugin can declare the way api.github.com is fixed for the github
		// connector. An empty manifest is the honest declaration; the
		// operator MUST narrow `network:` on the connector instance to their
		// own server's host themselves (documented in docs/connectors/plex.md).
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func plexEvent() plugin.Event {
	return plugin.Event{
		Name: "playback",
		Desc: "a Plex webhook playback notification fired — the specific event (media.play, media.pause, media.resume, media.stop, media.scrobble, media.rate, …) depends entirely on which notification types you enable in Plex's own Settings > Webhooks",
		Filters: plugin.Schema{
			"events":      {Type: "list", Desc: "match the event against any of these (empty = any)"},
			"media_types": {Type: "list", Desc: "match Metadata.type against any of these (empty = any)"},
			"accounts":    {Type: "list", Desc: "match Account.title against any of these (empty = any)"},
			"event":       {Type: "string"},
			"media_type":  {Type: "string"},
			"account":     {Type: "string"},
		},
		Context: plugin.Schema{
			"event":      {Type: "string", Desc: "e.g. media.play, media.pause, media.resume, media.stop, media.scrobble, media.rate"},
			"account":    {Type: "string", Desc: "Account.title — the Plex user"},
			"player":     {Type: "string", Desc: "Player.title — the playing client"},
			"server":     {Type: "string", Desc: "Server.title"},
			"media_type": {Type: "string", Desc: "Metadata.type — movie, episode, track, …"},
			"title":      {Type: "string", Desc: "Metadata.title"},
			"library":    {Type: "string", Desc: "Metadata.librarySectionTitle"},
			"rating_key": {Type: "string", Desc: "Metadata.ratingKey"},
		},
	}
}

func plexVerbs() []plugin.Verb {
	out := plugin.Schema{
		"status_code": {Type: "integer"},
		"result":      {Type: "any", Desc: "the unwrapped MediaContainer object"},
		"items":       {Type: "list", Desc: "the MediaContainer's list child (Video/Directory/Metadata/Playlist/…), or empty"},
	}
	return []plugin.Verb{
		{Name: "sessions", Desc: "current Plex playback sessions", Outputs: out},
		{Name: "library_sections", Desc: "configured library sections", Outputs: out},
		{
			Name: "scan_library", Desc: "trigger a library section scan/refresh",
			Options: plugin.Schema{"section_id": {Type: "string", Required: true, Scope: "section"}},
			Outputs: out,
		},
		{
			Name: "search", Desc: "search the server's libraries",
			Options: plugin.Schema{"query": {Type: "string", Required: true}},
			Outputs: out,
		},
		{
			Name: "metadata", Desc: "metadata for a single Plex item",
			Options: plugin.Schema{"rating_key": {Type: "string", Required: true, Scope: "item", Desc: "the Plex rating key of the item"}},
			Outputs: out,
		},
		{
			Name: "recently_added", Desc: "recently added media (server-wide, or one section)",
			Options: plugin.Schema{"section_id": {Type: "string", Scope: "section", Desc: "limit to one library section (default: server-wide)"}},
			Outputs: out,
		},
		{
			Name: "mark_watched", Desc: "mark an item watched (scrobble)",
			Options: plugin.Schema{"rating_key": {Type: "string", Required: true, Scope: "item"}},
			Outputs: out,
		},
		{
			Name: "mark_unwatched", Desc: "mark an item unwatched (unscrobble)",
			Options: plugin.Schema{"rating_key": {Type: "string", Required: true, Scope: "item"}},
			Outputs: out,
		},
		{
			Name: "refresh_metadata", Desc: "refresh a single item's metadata from its agent",
			Options: plugin.Schema{"rating_key": {Type: "string", Required: true, Scope: "item"}},
			Outputs: out,
		},
		{Name: "identity", Desc: "the connected Plex Media Server's identity/version", Outputs: out},
		{Name: "playlists", Desc: "configured playlists", Outputs: out},
		{
			Name:  "api",
			Desc:  "call any Plex Media Server endpoint not covered by a first-class verb",
			Usage: "escape hatch: method + path + query params/body",
			Options: plugin.Schema{
				"method": {Type: "string", Enum: []string{"GET", "POST", "PUT", "DELETE"}, Desc: "default GET"},
				"path":   {Type: "string", Required: true, Desc: "e.g. /library/sections/1/all"},
				"query":  {Type: "map", Desc: "additional query parameters"},
				"body":   {Type: "any", Desc: "JSON-encoded request body (POST/PUT)"},
			},
			Outputs: out,
		},
	}
}

// --- request building (pure, hermetically testable — no network) ----------

// reqBuild is what one verb call resolves to: the HTTP method, path, and
// query parameters against the Plex Media Server base_url, plus an optional
// JSON-encoded body (the `api` escape hatch only).
type reqBuild struct {
	method string
	path   string
	query  url.Values
	body   []byte
}

const scrobbleIdentifier = "com.plexapp.plugins.library"

func buildRequest(verb string, o map[string]any) (reqBuild, error) {
	switch verb {
	case "sessions":
		return reqBuild{method: http.MethodGet, path: "/status/sessions", query: url.Values{}}, nil

	case "library_sections":
		return reqBuild{method: http.MethodGet, path: "/library/sections", query: url.Values{}}, nil

	case "scan_library":
		id, err := requiredStr(o, "section_id")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/library/sections/" + url.PathEscape(id) + "/refresh", query: url.Values{}}, nil

	case "search":
		q, err := requiredStr(o, "query")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/search", query: url.Values{"query": {q}}}, nil

	case "metadata":
		rk, err := requiredStr(o, "rating_key")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/library/metadata/" + url.PathEscape(rk), query: url.Values{}}, nil

	case "recently_added":
		if sec := str(o["section_id"]); sec != "" {
			return reqBuild{method: http.MethodGet, path: "/library/sections/" + url.PathEscape(sec) + "/recentlyAdded", query: url.Values{}}, nil
		}
		return reqBuild{method: http.MethodGet, path: "/library/recentlyAdded", query: url.Values{}}, nil

	case "mark_watched":
		rk, err := requiredStr(o, "rating_key")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/:/scrobble", query: url.Values{"identifier": {scrobbleIdentifier}, "key": {rk}}}, nil

	case "mark_unwatched":
		rk, err := requiredStr(o, "rating_key")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/:/unscrobble", query: url.Values{"identifier": {scrobbleIdentifier}, "key": {rk}}}, nil

	case "refresh_metadata":
		rk, err := requiredStr(o, "rating_key")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodPut, path: "/library/metadata/" + url.PathEscape(rk) + "/refresh", query: url.Values{}}, nil

	case "identity":
		return reqBuild{method: http.MethodGet, path: "/identity", query: url.Values{}}, nil

	case "playlists":
		return reqBuild{method: http.MethodGet, path: "/playlists", query: url.Values{}}, nil

	case "api":
		p, err := requiredStr(o, "path")
		if err != nil {
			return reqBuild{}, err
		}
		method := strings.ToUpper(strOr(o["method"], http.MethodGet))
		q := url.Values{}
		if m, ok := o["query"].(map[string]any); ok {
			for k, v := range m {
				q.Set(k, fmt.Sprintf("%v", v))
			}
		}
		var body []byte
		if o["body"] != nil {
			b, err := json.Marshal(o["body"])
			if err != nil {
				return reqBuild{}, fmt.Errorf("body: %w", err)
			}
			body = b
		}
		return reqBuild{method: method, path: p, query: q, body: body}, nil
	}
	return reqBuild{}, fmt.Errorf("unknown verb %q", verb)
}

// requiredStr reads o[key] as a non-empty string.
func requiredStr(o map[string]any, key string) (string, error) {
	s := str(o[key])
	if s == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return s, nil
}

// --- Invoke -----------------------------------------------------------------

func (plexPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	if conn == nil {
		conn = map[string]any{}
	}
	baseURL := strings.TrimSpace(str(conn["base_url"]))
	if baseURL == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.base_url is required (e.g. http://plex:32400)")
	}
	token := strings.TrimSpace(str(conn["token"]))
	if token == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.token is required")
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
	status, raw, err := doRequest(ctx, baseURL, token, rb)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	if status < 200 || status >= 300 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s: %s %s: %d: %s", req.Verb, rb.method, rb.path, status, strings.TrimSpace(string(raw))))
	}

	result, items, err := unwrapContainer(raw)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{
		"status_code": status,
		"result":      result,
		"items":       items,
	}}, nil
}

// --- HTTP transport ----------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

// apiURL joins base_url + path (+ encoded query) — kept as its own function so
// a test can point it at an httptest.Server via base_url.
func apiURL(rawBase string, rb reqBuild) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(rawBase), "/")
	if base == "" {
		return "", fmt.Errorf("connection.base_url is required")
	}
	full := base + rb.path
	if len(rb.query) > 0 {
		full += "?" + rb.query.Encode()
	}
	return full, nil
}

// doRequest sends token as the X-Plex-Token header (never a query parameter,
// which would leak into access logs) alongside Accept: application/json, since
// Plex answers with XML by default.
func doRequest(ctx context.Context, baseURL, token string, rb reqBuild) (status int, raw []byte, err error) {
	full, err := apiURL(baseURL, rb)
	if err != nil {
		return 0, nil, err
	}
	var bodyReader *bytes.Reader
	if rb.body != nil {
		bodyReader = bytes.NewReader(rb.body)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	httpReq, err := http.NewRequestWithContext(ctx, rb.method, full, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	httpReq.Header.Set("X-Plex-Token", token)
	httpReq.Header.Set("Accept", "application/json")
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

// unwrapContainer unwraps Plex's fixed response envelope:
// {"MediaContainer": {...}}. The MediaContainer object itself becomes result;
// whichever of its fields is a JSON array (Video, Directory, Metadata,
// Playlist, …) becomes items — keys are scanned in sorted order so the choice
// is deterministic even though only one array field is ever present in
// practice. An empty body (some scrobble/unscrobble/refresh responses) yields
// empty result/items rather than an error.
func unwrapContainer(raw []byte) (result map[string]any, items []any, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return map[string]any{}, []any{}, nil
	}
	var env struct {
		MediaContainer map[string]any `json:"MediaContainer"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON response: %w", err)
	}
	result = env.MediaContainer
	if result == nil {
		result = map[string]any{}
	}
	items = []any{}
	keys := make([]string, 0, len(result))
	for k := range result {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if list, ok := result[k].([]any); ok {
			items = list
			break
		}
	}
	return result, items, nil
}

// --- source: Plex webhook notifications -------------------------------------

// nowFunc is time.Now, indirected so a test can pin the dedup timestamp.
var nowFunc = time.Now

// requireToken refuses to start an unauthenticated webhook listener.
//
// Plex's webhook feature has no signing of its own — it just POSTs a
// multipart/form-data body (the `payload` field is the JSON event) to
// whatever URL you configure in Settings > Webhooks. Omitting webhook.secret
// would silently accept ANY unsigned POST on the listen address as a real
// playback event, which is remote trigger injection with no signal that it
// happened. A missing secret is far more often a mistake than a choice, so it
// fails closed; `webhook.allow_unsigned: true` is the explicit, greppable way
// to say you meant it.
func requireToken(secret string, allowUnsigned bool) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintln(os.Stderr, "plex: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers")
		return nil
	}
	return fmt.Errorf("plex: no webhook.secret configured — an unauthenticated listener accepts any POST on the listen address as a real event. Set webhook.secret (and put it in Plex's webhook URL as a ?token= query parameter, e.g. http://host:port/plex?token=...), or set webhook.allow_unsigned: true if you genuinely front this with something else that authenticates")
}

// tokenEqual is a constant-time comparison that also normalizes away the
// length signal a naive == or subtle.ConstantTimeCompare(unequal-length)
// would leak, by comparing SHA-256 digests instead of the raw strings.
func tokenEqual(got, want string) bool {
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

func (plexPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	wh, _ := cfg["webhook"].(map[string]any)
	if wh == nil {
		wh = map[string]any{}
	}
	addr := str(wh["listen"])
	path := strOr(wh["path"], "/plex")
	secret := str(wh["secret"])
	allowUnsigned := boolv(wh["allow_unsigned"])

	if addr == "" {
		return fmt.Errorf("plex: no webhook.listen address configured")
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
		if secret != "" && !tokenEqual(r.URL.Query().Get("token"), secret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, "bad multipart form", http.StatusBadRequest)
			return
		}
		handleWebhookPayload([]byte(r.FormValue("payload")), dedup, emit)
		w.WriteHeader(http.StatusAccepted)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	fmt.Fprintf(os.Stderr, "plex[%s]: listening on %s%s\n", req.Instance, addr, path)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// webhookFacts is what one Plex webhook `payload` form field decodes to.
type webhookFacts struct {
	Event, Account, Player, Server string
	MediaType, Title, Library      string
	RatingKey                      string
}

type webhookWire struct {
	Event   string `json:"event"`
	Account struct {
		Title string `json:"title"`
	} `json:"Account"`
	Player struct {
		Title string `json:"title"`
	} `json:"Player"`
	Server struct {
		Title string `json:"title"`
	} `json:"Server"`
	Metadata struct {
		Type                string `json:"type"`
		Title               string `json:"title"`
		LibrarySectionTitle string `json:"librarySectionTitle"`
		RatingKey           string `json:"ratingKey"`
	} `json:"Metadata"`
}

func parseWebhook(payload []byte) (webhookFacts, error) {
	var w webhookWire
	if err := json.Unmarshal(payload, &w); err != nil {
		return webhookFacts{}, err
	}
	return webhookFacts{
		Event:     w.Event,
		Account:   w.Account.Title,
		Player:    w.Player.Title,
		Server:    w.Server.Title,
		MediaType: w.Metadata.Type,
		Title:     w.Metadata.Title,
		Library:   w.Metadata.LibrarySectionTitle,
		RatingKey: w.Metadata.RatingKey,
	}, nil
}

// handleWebhookPayload parses one Plex webhook `payload` field and emits a
// normalized "playback" event. A payload that fails to parse, or carries no
// event name, is dropped rather than emitted blind.
func handleWebhookPayload(payload []byte, dedup *sourcekit.Dedup, emit func(any) error) {
	f, err := parseWebhook(payload)
	if err != nil || f.Event == "" {
		return
	}
	dk := fmt.Sprintf("%s\x00%s\x00%d", f.Event, f.RatingKey, nowFunc().Unix())
	if !dedup.Add(dk) {
		return
	}
	_ = emit(map[string]any{
		"event": "playback",
		"kind":  f.Event,
		"title": fmt.Sprintf("plex %s: %s", f.Event, f.Title),
		"dedup": dk,
		"context": map[string]any{
			"event": f.Event, "account": f.Account, "player": f.Player, "server": f.Server,
			"media_type": f.MediaType, "title": f.Title, "library": f.Library, "rating_key": f.RatingKey,
			// Plural aliases so the documented filter vocabulary
			// (filters: {events/media_types/accounts: [...]}) matches
			// against the daemon's generic list-contains filter evaluator.
			"events": f.Event, "media_types": f.MediaType, "accounts": f.Account,
		},
	})
}

func main() {
	if err := plugin.Serve(plexPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-plex: %v\n", err)
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
