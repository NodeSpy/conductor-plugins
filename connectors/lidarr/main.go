// Command conductor-lidarr is the Lidarr connector as a standalone external
// conductor plugin. It drives a self-hosted Lidarr instance's REST API v1 as
// verbs (artist listing/lookup/add/delete, albums, command, queue, calendar,
// and a generic `api` escape hatch), and as a SOURCE it receives Lidarr's own
// Webhook notification POSTs (Grab, Download, Rename, Retag, Health,
// ApplicationUpdate, Test, etc. — whatever the operator wires up in Lidarr's
// own Connect settings) and streams a normalized event per delivery. Built
// ONLY against the public SDK (pkg/plugin, pkg/sourcekit) — no conductor
// internals, no third-party dependencies.
//
// Lidarr is a Servarr app almost identical to Sonarr, but for MUSIC: series
// become artists, episodes become albums, and TheTVDB search becomes a
// MusicBrainz search. Every verb is a plain net/http call to
// {base_url}/api/v1/<resource>, authenticated with the X-Api-Key header. A
// non-2xx response is surfaced as a plugin error carrying the status code
// and response body.
//
// Lidarr itself has no outbound webhook signing: its Connect > Webhook
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
//	base_url: "http://lidarr:8686" # REQUIRED: base of the instance (no trailing /api/v1)
//	api_key:  "<api key>"          # REQUIRED: Settings > General > Security
//	webhook:
//	  listen: ":9097"                # HTTP listener address (StartSource only)
//	  path: "/lidarr"                 # request path (default /lidarr)
//	  secret: "<shared token>"        # compared against X-Conductor-Token / ?token=
//	  allow_unsigned: false           # explicit opt-in to run with no shared token
//	  smee: "<smee.io channel URL>"   # optional relay for endpoints with no public URL
//
// The Lidarr host is operator-specific and self-hosted, so this plugin
// declares NO egress in its capability manifest — the operator is expected to
// scope `network:` on the connector instance to their own Lidarr host (see
// docs/connectors/lidarr.md).
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

type lidarrPlugin struct{}

func (lidarrPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "lidarr",
		Desc: "Lidarr: music library management as verbs (artists, artist lookup, add/delete artist, albums, command, queue, calendar, api escape hatch) over REST API v1; Grab/Download/artist lifecycle events in via Lidarr's own Webhook connection (source).",
		Connection: plugin.Schema{
			"base_url": {Type: "string", Required: true, Desc: "base URL of the Lidarr instance, e.g. http://lidarr:8686 (no trailing /api/v1)"},
			"api_key":  {Type: "string", Required: true, Desc: "Lidarr API key (Settings > General > Security)"},
			"webhook":  {Type: "map", Desc: "source transport: listen, path (default /lidarr), secret, allow_unsigned, smee"},
		},
		Events: []plugin.Event{lidarrEvent()},
		Verbs:  lidarrVerbs(),
		// Lidarr is always self-hosted — there is no fixed hostname this
		// plugin can declare the way api.github.com is fixed for the github
		// connector. An empty manifest is the honest declaration; the
		// operator MUST narrow `network:` on the connector instance to their
		// own instance's host themselves (documented in
		// docs/connectors/lidarr.md).
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func lidarrEvent() plugin.Event {
	return plugin.Event{
		Name: "event",
		Desc: "a Lidarr webhook notification fired (Grab, Download, Rename, Retag, ArtistAdd, ArtistDelete, AlbumDelete, Health, HealthRestored, ApplicationUpdate, Test, etc. — whatever notifications you enable on the Webhook connection in Lidarr)",
		Filters: plugin.Schema{
			"event_types": {Type: "list", Desc: "match payload.eventType against any of these (empty = any)"},
			"artists":     {Type: "list", Desc: "match the artist name against any of these (empty = any)"},
		},
		Context: plugin.Schema{
			"event_type":   {Type: "string", Desc: "e.g. Grab, Download, Rename, Retag, ArtistAdd, ArtistDelete, AlbumDelete, Health, ApplicationUpdate, Test"},
			"artist_title": {Type: "string", Desc: "the artist's name, when present"},
			"mbid":         {Type: "string", Desc: "the artist's MusicBrainz id (payload.artist.mbId), when present"},
			"albums":       {Type: "list", Desc: "the album(s) the payload carries, normalized to a list (Grab's albums[] as-is; Download/Rename/Retag's single album wrapped in a one-element list)"},
			"quality":      {Type: "string", Desc: "the release/track-file quality name, when present"},
			"payload":      {Type: "any", Desc: "the full posted JSON body, verbatim"},
		},
	}
}

func lidarrVerbs() []plugin.Verb {
	resultOut := plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}}
	itemsOut := plugin.Schema{"status_code": {Type: "integer"}, "items": {Type: "list"}}
	return []plugin.Verb{
		{
			Name:    "artists",
			Desc:    "list all artists known to Lidarr",
			Outputs: itemsOut,
		},
		{
			Name: "artist_get",
			Desc: "a single artist by id",
			Options: plugin.Schema{
				"id": {Type: "integer", Required: true},
			},
			Outputs: resultOut,
		},
		{
			Name: "lookup",
			Desc: "search for an artist to add (MusicBrainz search)",
			Options: plugin.Schema{
				"term": {Type: "string", Required: true, Desc: "search term, e.g. an artist name or mbid:<musicbrainz-id>"},
			},
			Outputs: itemsOut,
		},
		{
			Name: "add_artist", Desc: "add an artist to Lidarr",
			Usage: "either pass foreign_artist_id/quality_profile_id/root_folder_path (built into the request), or pass the full `artist` map for direct passthrough",
			Options: plugin.Schema{
				"foreign_artist_id":   {Type: "string", Desc: "MusicBrainz artist id (required unless `artist` is given)"},
				"quality_profile_id":  {Type: "integer", Desc: "required unless `artist` is given"},
				"root_folder_path":    {Type: "string", Desc: "required unless `artist` is given"},
				"monitored":           {Type: "boolean", Desc: "default true"},
				"metadata_profile_id": {Type: "integer"},
				"search_for_missing":  {Type: "boolean", Desc: "addOptions.searchForMissingAlbums"},
				"artist":              {Type: "map", Desc: "full Lidarr artist object, sent verbatim instead of the individual fields above"},
			},
			Outputs: resultOut,
		},
		{
			Name: "delete_artist", Desc: "remove an artist from Lidarr",
			Options: plugin.Schema{
				"id":                   {Type: "integer", Required: true},
				"delete_files":         {Type: "boolean", Desc: "also delete the artist's files on disk"},
				"add_import_exclusion": {Type: "boolean", Desc: "add the artist to the import list exclusion list"},
			},
			Outputs: resultOut,
		},
		{
			Name: "albums", Desc: "list albums for an artist",
			Options: plugin.Schema{
				"artist_id": {Type: "integer", Required: true},
			},
			Outputs: itemsOut,
		},
		{
			Name: "album_get", Desc: "a single album by id",
			Options: plugin.Schema{
				"id": {Type: "integer", Required: true},
			},
			Outputs: resultOut,
		},
		{
			Name: "command", Desc: "run a Lidarr command",
			Usage: "e.g. ArtistSearch, AlbumSearch, RefreshArtist, RescanFolders",
			Options: plugin.Schema{
				"name":      {Type: "string", Required: true, Desc: "command name, e.g. ArtistSearch, AlbumSearch, RefreshArtist, RescanFolders"},
				"artist_id": {Type: "integer"},
				"album_ids": {Type: "list", Desc: "list of album ids"},
				"params":    {Type: "map", Desc: "additional command-specific parameters, merged in verbatim"},
			},
			Outputs: resultOut,
		},
		{
			Name:    "queue",
			Desc:    "the current download queue",
			Outputs: resultOut,
		},
		{
			Name: "calendar", Desc: "albums releasing in a date range",
			Options: plugin.Schema{
				"start": {Type: "string", Desc: "ISO-8601 start date"},
				"end":   {Type: "string", Desc: "ISO-8601 end date"},
			},
			Outputs: itemsOut,
		},
		{
			Name: "api", Desc: "call any Lidarr API v1 endpoint not covered by a first-class verb",
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
	case "artists":
		return reqBuild{method: http.MethodGet, path: "/artist", query: url.Values{}, asList: true}, nil

	case "artist_get":
		id, err := requiredInt(o, "id")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/artist/" + strconv.FormatInt(id, 10), query: url.Values{}}, nil

	case "lookup":
		term, err := requiredStr(o, "term")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/artist/lookup", query: url.Values{"term": {term}}, asList: true}, nil

	case "add_artist":
		return buildAddArtist(o)

	case "delete_artist":
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
		return reqBuild{method: http.MethodDelete, path: "/artist/" + strconv.FormatInt(id, 10), query: q}, nil

	case "albums":
		artistID, err := requiredInt(o, "artist_id")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/album", query: url.Values{"artistId": {strconv.FormatInt(artistID, 10)}}, asList: true}, nil

	case "album_get":
		id, err := requiredInt(o, "id")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/album/" + strconv.FormatInt(id, 10), query: url.Values{}}, nil

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

	case "api":
		return buildAPI(o)
	}
	return reqBuild{}, fmt.Errorf("unknown verb %q", verb)
}

// buildAddArtist builds POST /artist. When options.artist is a map it is sent
// verbatim (full passthrough); otherwise the request body is built from the
// individual fields.
func buildAddArtist(o map[string]any) (reqBuild, error) {
	if artist, ok := o["artist"].(map[string]any); ok {
		return reqBuild{method: http.MethodPost, path: "/artist", query: url.Values{}, body: artist}, nil
	}
	foreignArtistID, err := requiredStr(o, "foreign_artist_id")
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
		"foreignArtistId":  foreignArtistID,
		"qualityProfileId": qualityProfileID,
		"rootFolderPath":   rootFolderPath,
		"monitored":        monitored,
		"addOptions": map[string]any{
			"searchForMissingAlbums": boolv(o["search_for_missing"]),
		},
	}
	if mp, ok := toInt64(o["metadata_profile_id"]); ok {
		body["metadataProfileId"] = mp
	}
	return reqBuild{method: http.MethodPost, path: "/artist", query: url.Values{}, body: body}, nil
}

// buildCommand builds POST /command from the required name plus the
// convenience fields (artist_id/album_ids), merged with any extra
// caller-supplied params.
func buildCommand(o map[string]any) (reqBuild, error) {
	name, err := requiredStr(o, "name")
	if err != nil {
		return reqBuild{}, err
	}
	body := map[string]any{"name": name}
	if v, ok := toInt64(o["artist_id"]); ok {
		body["artistId"] = v
	}
	if ids := o["album_ids"]; ids != nil {
		body["albumIds"] = ids
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

func (lidarrPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	if conn == nil {
		conn = map[string]any{}
	}
	baseURL := strings.TrimSpace(str(conn["base_url"]))
	if baseURL == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.base_url is required (e.g. http://lidarr:8686)")
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
		return "", fmt.Errorf("connection.base_url is required (e.g. http://lidarr:8686)")
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
// value; nil for an empty body (e.g. a 200/204 from delete_artist with
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

// --- source: Lidarr Webhook connection ---------------------------------------

// requireToken refuses to start an unauthenticated webhook listener.
//
// Lidarr's Webhook connection has no signing of its own — it just POSTs an
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
		fmt.Fprintln(os.Stderr, "lidarr: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers")
		return nil
	}
	return fmt.Errorf("lidarr: no webhook.secret configured — an unauthenticated listener accepts any POST on the listen address as a real event. Set webhook.secret (and put it in Lidarr's Webhook connection as an X-Conductor-Token header or ?token= query param), or set webhook.allow_unsigned: true if you genuinely front this with something else that authenticates")
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

func (lidarrPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	wh, _ := cfg["webhook"].(map[string]any)
	if wh == nil {
		wh = map[string]any{}
	}
	addr := str(wh["listen"])
	path := strOr(wh["path"], "/lidarr")
	secret := str(wh["secret"])
	allowUnsigned := boolv(wh["allow_unsigned"])
	smeeURL := str(wh["smee"])

	if addr == "" && smeeURL == "" {
		return fmt.Errorf("lidarr: no webhook.listen address or webhook.smee relay configured")
	}
	if err := requireToken(secret, allowUnsigned); err != nil {
		return err
	}

	dedup := sourcekit.NewDedup(2048)
	if addr != "" {
		fmt.Fprintf(os.Stderr, "lidarr[%s]: listening on %s%s\n", req.Instance, addr, path)
	}
	if smeeURL != "" {
		fmt.Fprintf(os.Stderr, "lidarr[%s]: relaying via smee channel %s\n", req.Instance, smeeURL)
	}
	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smeeURL}
	return ln.ServeReq(ctx, func(rq *sourcekit.Request) {
		if secret != "" && !verifyToken(secret, rq) {
			return
		}
		handleWebhookBody(rq.Body, dedup, emit)
	})
}

// lidarrArtist is the subset of Lidarr's `artist` object this plugin reads
// out of a webhook payload.
type lidarrArtist struct {
	Name string `json:"name"`
	MBID string `json:"mbId"`
}

// handleWebhookBody parses one Lidarr Webhook delivery and emits a
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

	var artist lidarrArtist
	if a, ok := payload["artist"].(map[string]any); ok {
		if b, err := json.Marshal(a); err == nil {
			_ = json.Unmarshal(b, &artist)
		}
	}

	albums := extractAlbums(payload)
	quality := extractQuality(payload)

	downloadID, _ := payload["downloadId"].(string)
	key := dedupKey(eventType, artist.MBID, downloadID)
	if !dedup.Add(key) {
		return
	}

	title := fmt.Sprintf("lidarr %s", eventType)
	if artist.Name != "" {
		title = fmt.Sprintf("lidarr %s: %s", eventType, artist.Name)
	}

	_ = emit(map[string]any{
		"event": "event",
		"kind":  eventType,
		"title": title,
		"dedup": key,
		"context": map[string]any{
			"event_type":   eventType,
			"artist_title": artist.Name,
			"mbid":         artist.MBID,
			"albums":       albums,
			"quality":      quality,
			"payload":      payload,
			// Filter-key aliases for the daemon's generic list-contains
			// filter evaluator (filters: {event_types/artists: [...]}).
			"event_types": eventType,
			"artists":     artist.Name,
		},
	})
}

// extractAlbums normalizes the payload's album data into a list: Grab
// deliveries carry `albums` (a list) while Download/Rename/Retag deliveries
// carry a single `album` object — wrapped here into a one-element list so
// callers always get a consistent shape. Returns nil when neither is present
// (e.g. Health/ApplicationUpdate/Test deliveries, which carry no albums).
func extractAlbums(payload map[string]any) []any {
	if albums, ok := payload["albums"].([]any); ok {
		return albums
	}
	if album, ok := payload["album"].(map[string]any); ok {
		return []any{album}
	}
	return nil
}

// extractQuality reads the release/track-file quality name Lidarr's Grab and
// Download webhooks carry. Unlike Sonarr, Lidarr's webhook payloads already
// flatten quality down to a plain string (`release.quality` /
// `trackFiles[].quality`), so no nested quality.quality.name unwrapping is
// needed.
func extractQuality(payload map[string]any) string {
	if release, ok := payload["release"].(map[string]any); ok {
		if q, ok := release["quality"].(string); ok && q != "" {
			return q
		}
	}
	if trackFiles, ok := payload["trackFiles"].([]any); ok && len(trackFiles) > 0 {
		if first, ok := trackFiles[0].(map[string]any); ok {
			if q, ok := first["quality"].(string); ok {
				return q
			}
		}
	}
	return ""
}

// dedupKey builds the dedup key: eventType + the artist's MusicBrainz id +
// (downloadId, when present, else a per-second timestamp so redeliveries of
// the SAME download within the same wall-clock second still collapse, but
// distinct deliveries without a downloadId are not incorrectly merged across
// time).
func dedupKey(eventType, mbid, downloadID string) string {
	disambiguator := downloadID
	if disambiguator == "" {
		disambiguator = strconv.FormatInt(time.Now().Unix(), 10)
	}
	return fmt.Sprintf("%s\x00%s\x00%s", eventType, mbid, disambiguator)
}

func main() {
	if err := plugin.Serve(lidarrPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-lidarr: %v\n", err)
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
