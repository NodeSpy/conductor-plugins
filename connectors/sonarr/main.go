// Command conductor-sonarr is the Sonarr connector as a standalone external
// conductor plugin (#59). It drives a self-hosted Sonarr instance's REST API
// v3 as verbs (series listing/lookup/add/delete, episodes, commands, queue,
// calendar, wanted/missing, quality profiles, root folders, health, and a
// generic `api` escape hatch), and as a SOURCE it receives Sonarr's own
// Webhook notification POSTs (Grab, Download, SeriesAdd, SeriesDelete,
// EpisodeFileDelete, Health, etc. — whatever the operator wires up in
// Sonarr's own Connect settings) and streams a normalized event per
// delivery. Built ONLY against the public SDK (pkg/plugin, pkg/sourcekit) —
// no conductor internals, no third-party dependencies.
//
// Every verb is a plain net/http call to {base_url}/api/v3/<resource>,
// authenticated with the X-Api-Key header. A non-2xx response is surfaced as
// a plugin error carrying the status code and response body.
//
// Sonarr itself has no outbound webhook signing: its Connect > Webhook
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
//	base_url: "http://sonarr:8989" # REQUIRED: base of the instance (no trailing /api/v3)
//	api_key:  "<api key>"          # REQUIRED: Settings > General > Security
//	webhook:
//	  listen: ":9096"                # HTTP listener address (StartSource only)
//	  path: "/sonarr"                 # request path (default /sonarr)
//	  secret: "<shared token>"        # compared against X-Conductor-Token / ?token=
//	  allow_unsigned: false           # explicit opt-in to run with no shared token
//	  smee: "<smee.io channel URL>"   # optional relay for endpoints with no public URL
//
// The Sonarr host is operator-specific and self-hosted, so this plugin
// declares NO egress in its capability manifest — the operator is expected to
// scope `network:` on the connector instance to their own Sonarr host (see
// docs/connectors/sonarr.md).
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

type sonarrPlugin struct{}

func (sonarrPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "sonarr",
		Desc: "Sonarr: TV series management as verbs (series, lookup, add/delete series, episodes, command, queue, calendar, wanted_missing, quality_profiles, root_folders, health, api escape hatch) over REST API v3; Grab/Download/series lifecycle events in via Sonarr's own Webhook connection (source).",
		Connection: plugin.Schema{
			"base_url": {Type: "string", Required: true, Desc: "base URL of the Sonarr instance, e.g. http://sonarr:8989 (no trailing /api/v3)"},
			"api_key":  {Type: "string", Required: true, Desc: "Sonarr API key (Settings > General > Security)"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path (default /sonarr), secret, allow_unsigned, smee"},
		},
		Events: []plugin.Event{sonarrEvent()},
		Verbs:  sonarrVerbs(),
		// Sonarr is always self-hosted — there is no fixed hostname this
		// plugin can declare the way api.github.com is fixed for the github
		// connector. An empty manifest is the honest declaration; the
		// operator MUST narrow `network:` on the connector instance to their
		// own instance's host themselves (documented in
		// docs/connectors/sonarr.md).
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func sonarrEvent() plugin.Event {
	return plugin.Event{
		Name: "event",
		Desc: "a Sonarr webhook notification fired (Grab, Download, SeriesAdd, SeriesDelete, EpisodeFileDelete, Health, Test, etc. — whatever notifications you enable on the Webhook connection in Sonarr)",
		Filters: plugin.Schema{
			"event_types": {Type: "list", Desc: "match payload.eventType against any of these (empty = any)"},
			"series":      {Type: "list", Desc: "match the series title against any of these (empty = any)"},
		},
		Context: plugin.Schema{
			"event_type":   {Type: "string", Desc: "e.g. Grab, Download, SeriesAdd, SeriesDelete, EpisodeFileDelete, Health, Test"},
			"series_title": {Type: "string"},
			"tvdb_id":      {Type: "integer"},
			"episodes":     {Type: "list", Desc: "the episodes[] array from the payload, verbatim"},
			"quality":      {Type: "string", Desc: "the release/episode file quality name, when present"},
			"payload":      {Type: "any", Desc: "the full posted JSON body, verbatim"},
		},
	}
}

func sonarrVerbs() []plugin.Verb {
	resultOut := plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}}
	itemsOut := plugin.Schema{"status_code": {Type: "integer"}, "items": {Type: "list"}}
	return []plugin.Verb{
		{
			Name:    "series",
			Desc:    "list all series known to Sonarr",
			Outputs: itemsOut,
		},
		{
			Name: "series_get",
			Desc: "a single series by id",
			Options: plugin.Schema{
				"id": {Type: "integer", Required: true},
			},
			Outputs: resultOut,
		},
		{
			Name: "lookup",
			Desc: "search for a series to add (TheTVDB search)",
			Options: plugin.Schema{
				"term": {Type: "string", Required: true, Desc: "search term, e.g. a title or tvdb:12345"},
			},
			Outputs: itemsOut,
		},
		{
			Name: "add_series", Desc: "add a series to Sonarr",
			Usage: "either pass tvdb_id/quality_profile_id/root_folder_path (built into the request), or pass the full `series` map for direct passthrough",
			Options: plugin.Schema{
				"tvdb_id":             {Type: "integer", Desc: "TheTVDB id (required unless `series` is given)"},
				"quality_profile_id":  {Type: "integer", Desc: "required unless `series` is given"},
				"root_folder_path":    {Type: "string", Desc: "required unless `series` is given"},
				"monitored":           {Type: "boolean", Desc: "default true"},
				"season_folder":       {Type: "boolean", Desc: "use per-season folders"},
				"language_profile_id": {Type: "integer"},
				"search_for_missing":  {Type: "boolean", Desc: "addOptions.searchForMissingEpisodes"},
				"series":              {Type: "map", Desc: "full Sonarr series object, sent verbatim instead of the individual fields above"},
			},
			Outputs: resultOut,
		},
		{
			Name: "delete_series", Desc: "remove a series from Sonarr",
			Options: plugin.Schema{
				"id":                   {Type: "integer", Required: true},
				"delete_files":         {Type: "boolean", Desc: "also delete the series' files on disk"},
				"add_import_exclusion": {Type: "boolean", Desc: "add the series to the import list exclusion list"},
			},
			Outputs: resultOut,
		},
		{
			Name: "episodes", Desc: "list episodes for a series",
			Options: plugin.Schema{
				"series_id": {Type: "integer", Required: true},
			},
			Outputs: itemsOut,
		},
		{
			Name: "episode_get", Desc: "a single episode by id",
			Options: plugin.Schema{
				"id": {Type: "integer", Required: true},
			},
			Outputs: resultOut,
		},
		{
			Name: "command", Desc: "run a Sonarr command",
			Usage: "e.g. SeriesSearch, SeasonSearch, RefreshSeries, RescanSeries",
			Options: plugin.Schema{
				"name":          {Type: "string", Required: true, Desc: "command name, e.g. SeriesSearch, SeasonSearch, RefreshSeries, RescanSeries"},
				"series_id":     {Type: "integer"},
				"season_number": {Type: "integer"},
				"episode_ids":   {Type: "list", Desc: "list of episode ids"},
				"params":        {Type: "map", Desc: "additional command-specific parameters, merged in verbatim"},
			},
			Outputs: resultOut,
		},
		{
			Name:    "queue",
			Desc:    "the current download queue",
			Outputs: resultOut,
		},
		{
			Name: "calendar", Desc: "episodes airing in a date range",
			Options: plugin.Schema{
				"start": {Type: "string", Desc: "ISO-8601 start date"},
				"end":   {Type: "string", Desc: "ISO-8601 end date"},
			},
			Outputs: itemsOut,
		},
		{
			Name:    "wanted_missing",
			Desc:    "episodes Sonarr considers missing",
			Outputs: resultOut,
		},
		{
			Name:    "quality_profiles",
			Desc:    "configured quality profiles",
			Outputs: itemsOut,
		},
		{
			Name:    "root_folders",
			Desc:    "configured root folders",
			Outputs: itemsOut,
		},
		{
			Name:    "health",
			Desc:    "current health check results",
			Outputs: itemsOut,
		},
		{
			Name: "api", Desc: "call any Sonarr API v3 endpoint not covered by a first-class verb",
			Usage: "escape hatch: method + path (relative to /api/v3) + query + body",
			Options: plugin.Schema{
				"method": {Type: "string", Desc: "default GET"},
				"path":   {Type: "string", Required: true, Desc: "path relative to /api/v3, e.g. /system/status"},
				"query":  {Type: "map", Desc: "query parameters"},
				"body":   {Type: "any", Desc: "request body, marshaled to JSON"},
			},
			Outputs: plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}, "items": {Type: "list"}},
		},
	}
}

// --- request building (pure, hermetically testable — no network) ----------

// reqBuild is what one verb call resolves to: the HTTP method, the path
// relative to /api/v3, its query parameters, an optional JSON body, and
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
	case "series":
		return reqBuild{method: http.MethodGet, path: "/series", query: url.Values{}, asList: true}, nil

	case "series_get":
		id, err := requiredInt(o, "id")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/series/" + strconv.FormatInt(id, 10), query: url.Values{}}, nil

	case "lookup":
		term, err := requiredStr(o, "term")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/series/lookup", query: url.Values{"term": {term}}, asList: true}, nil

	case "add_series":
		return buildAddSeries(o)

	case "delete_series":
		id, err := requiredInt(o, "id")
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{}
		if boolv(o["delete_files"]) {
			q.Set("deleteFiles", "true")
		}
		if boolv(o["add_import_exclusion"]) {
			q.Set("addImportListExclusion", "true")
		}
		return reqBuild{method: http.MethodDelete, path: "/series/" + strconv.FormatInt(id, 10), query: q}, nil

	case "episodes":
		seriesID, err := requiredInt(o, "series_id")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/episode", query: url.Values{"seriesId": {strconv.FormatInt(seriesID, 10)}}, asList: true}, nil

	case "episode_get":
		id, err := requiredInt(o, "id")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/episode/" + strconv.FormatInt(id, 10), query: url.Values{}}, nil

	case "command":
		return buildCommand(o)

	case "queue":
		return reqBuild{method: http.MethodGet, path: "/queue", query: url.Values{}}, nil

	case "calendar":
		q := url.Values{}
		if v := str(o["start"]); v != "" {
			q.Set("start", v)
		}
		if v := str(o["end"]); v != "" {
			q.Set("end", v)
		}
		return reqBuild{method: http.MethodGet, path: "/calendar", query: q, asList: true}, nil

	case "wanted_missing":
		return reqBuild{method: http.MethodGet, path: "/wanted/missing", query: url.Values{}}, nil

	case "quality_profiles":
		return reqBuild{method: http.MethodGet, path: "/qualityprofile", query: url.Values{}, asList: true}, nil

	case "root_folders":
		return reqBuild{method: http.MethodGet, path: "/rootfolder", query: url.Values{}, asList: true}, nil

	case "health":
		return reqBuild{method: http.MethodGet, path: "/health", query: url.Values{}, asList: true}, nil

	case "api":
		return buildAPI(o)
	}
	return reqBuild{}, fmt.Errorf("unknown verb %q", verb)
}

// buildAddSeries builds POST /series. When options.series is a map it is sent
// verbatim (full passthrough); otherwise the request body is built from the
// individual fields.
func buildAddSeries(o map[string]any) (reqBuild, error) {
	if series, ok := o["series"].(map[string]any); ok {
		return reqBuild{method: http.MethodPost, path: "/series", query: url.Values{}, body: series}, nil
	}
	tvdbID, err := requiredInt(o, "tvdb_id")
	if err != nil {
		return reqBuild{}, err
	}
	qualityProfileID, err := requiredInt(o, "quality_profile_id")
	if err != nil {
		return reqBuild{}, err
	}
	rootFolderPath, err := requiredStr(o, "root_folder_path")
	if err != nil {
		return reqBuild{}, err
	}
	monitored := true
	if v, ok := o["monitored"]; ok {
		monitored = boolv(v)
	}
	body := map[string]any{
		"tvdbId":           tvdbID,
		"qualityProfileId": qualityProfileID,
		"rootFolderPath":   rootFolderPath,
		"monitored":        monitored,
		"addOptions": map[string]any{
			"searchForMissingEpisodes": boolv(o["search_for_missing"]),
		},
	}
	if v, ok := o["season_folder"]; ok {
		body["seasonFolder"] = boolv(v)
	}
	if lp, ok := toInt64(o["language_profile_id"]); ok {
		body["languageProfileId"] = lp
	}
	return reqBuild{method: http.MethodPost, path: "/series", query: url.Values{}, body: body}, nil
}

// buildCommand builds POST /command from the required name plus the
// convenience fields (series_id/season_number/episode_ids), merged with any
// extra caller-supplied params.
func buildCommand(o map[string]any) (reqBuild, error) {
	name, err := requiredStr(o, "name")
	if err != nil {
		return reqBuild{}, err
	}
	body := map[string]any{"name": name}
	if v, ok := toInt64(o["series_id"]); ok {
		body["seriesId"] = v
	}
	if v, ok := toInt64(o["season_number"]); ok {
		body["seasonNumber"] = v
	}
	if ids := o["episode_ids"]; ids != nil {
		body["episodeIds"] = ids
	}
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

func (sonarrPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	if conn == nil {
		conn = map[string]any{}
	}
	baseURL := strings.TrimSpace(str(conn["base_url"]))
	if baseURL == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.base_url is required (e.g. http://sonarr:8989)")
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

// apiURL derives the API base (rawBase + /api/v3) — kept as its own function
// so a test can point it at an httptest.Server.
func apiURL(rawBase string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(rawBase), "/")
	if u == "" {
		return "", fmt.Errorf("connection.base_url is required (e.g. http://sonarr:8989)")
	}
	return u + "/api/v3", nil
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
// value; nil for an empty body (e.g. a 200/204 from delete_series with
// nothing to report).
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

// --- source: Sonarr Webhook connection ---------------------------------------

// requireToken refuses to start an unauthenticated webhook listener.
//
// Sonarr's Webhook connection has no signing of its own — it just POSTs an
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
		fmt.Fprintln(os.Stderr, "sonarr: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers")
		return nil
	}
	return fmt.Errorf("sonarr: no webhook.secret configured — an unauthenticated listener accepts any POST on the listen address as a real event. Set webhook.secret (and put it in Sonarr's Webhook connection as an X-Conductor-Token header or ?token= query param), or set webhook.allow_unsigned: true if you genuinely front this with something else that authenticates")
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

func (sonarrPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	wh, _ := cfg["webhook"].(map[string]any)
	if wh == nil {
		wh = map[string]any{}
	}
	addr := str(wh["listen"])
	path := strOr(wh["path"], "/sonarr")
	secret := str(wh["secret"])
	allowUnsigned := boolv(wh["allow_unsigned"])
	smeeURL := str(wh["smee"])

	if addr == "" && smeeURL == "" {
		return fmt.Errorf("sonarr: no webhook.listen address or webhook.smee relay configured")
	}
	if err := requireToken(secret, allowUnsigned); err != nil {
		return err
	}

	dedup := sourcekit.NewDedup(2048)
	if addr != "" {
		fmt.Fprintf(os.Stderr, "sonarr[%s]: listening on %s%s\n", req.Instance, addr, path)
	}
	if smeeURL != "" {
		fmt.Fprintf(os.Stderr, "sonarr[%s]: relaying via smee channel %s\n", req.Instance, smeeURL)
	}
	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smeeURL}
	return ln.ServeReq(ctx, func(rq *sourcekit.Request) {
		if secret != "" && !verifyToken(secret, rq) {
			return
		}
		handleWebhookBody(rq.Body, dedup, emit)
	})
}

// sonarrSeries is the subset of Sonarr's `series` object this plugin reads
// out of a webhook payload.
type sonarrSeries struct {
	Title  string `json:"title"`
	TvdbID int64  `json:"tvdbId"`
}

// sonarrEpisodeFile is the subset of Sonarr's `episodeFile`/`release` object
// this plugin reads for a quality name.
type sonarrQualityWrapper struct {
	Quality *struct {
		Quality *struct {
			Name string `json:"name"`
		} `json:"quality"`
	} `json:"quality"`
}

// handleWebhookBody parses one Sonarr Webhook delivery and emits a
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

	var series sonarrSeries
	if s, ok := payload["series"].(map[string]any); ok {
		if b, err := json.Marshal(s); err == nil {
			_ = json.Unmarshal(b, &series)
		}
	}

	var episodes []any
	if eps, ok := payload["episodes"].([]any); ok {
		episodes = eps
	}

	quality := extractQuality(payload)

	downloadID, _ := payload["downloadId"].(string)
	dedupKey := dedupKey(eventType, series.TvdbID, downloadID)
	if !dedup.Add(dedupKey) {
		return
	}

	title := fmt.Sprintf("sonarr %s", eventType)
	if series.Title != "" {
		title = fmt.Sprintf("sonarr %s: %s", eventType, series.Title)
	}

	_ = emit(map[string]any{
		"event": "event",
		"kind":  eventType,
		"title": title,
		"dedup": dedupKey,
		"context": map[string]any{
			"event_type":   eventType,
			"series_title": series.Title,
			"tvdb_id":      series.TvdbID,
			"episodes":     episodes,
			"quality":      quality,
			"payload":      payload,
			// Filter-key aliases for the daemon's generic list-contains
			// filter evaluator (filters: {event_types/series: [...]}).
			"event_types": eventType,
			"series":      series.Title,
		},
	})
}

// extractQuality reads the release/episode-file quality name Sonarr's Grab
// and Download webhooks carry, from whichever of `release` or `episodeFile`
// is present.
func extractQuality(payload map[string]any) string {
	for _, key := range []string{"release", "episodeFile"} {
		v, ok := payload[key].(map[string]any)
		if !ok {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		var w sonarrQualityWrapper
		if err := json.Unmarshal(b, &w); err != nil {
			continue
		}
		if w.Quality != nil && w.Quality.Quality != nil && w.Quality.Quality.Name != "" {
			return w.Quality.Quality.Name
		}
	}
	return ""
}

// dedupKey builds the dedup key: eventType + tvdbId + (downloadId, when
// present, else a per-second timestamp so redeliveries of the SAME download
// within the same wall-clock second still collapse, but distinct deliveries
// without a downloadId are not incorrectly merged across time).
func dedupKey(eventType string, tvdbID int64, downloadID string) string {
	disambiguator := downloadID
	if disambiguator == "" {
		disambiguator = strconv.FormatInt(time.Now().Unix(), 10)
	}
	return fmt.Sprintf("%s\x00%d\x00%s", eventType, tvdbID, disambiguator)
}

func main() {
	if err := plugin.Serve(sonarrPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-sonarr: %v\n", err)
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
