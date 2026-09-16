// Command conductor-sabnzbd is the SABnzbd connector as a standalone external
// conductor plugin (#59). It drives a self-hosted SABnzbd instance's HTTP API
// as verbs — queue and history inspection, adding NZB URLs, job control,
// speed limiting, and status/version — plus a generic `api` escape hatch for
// any mode the first-class verbs do not cover. Built ONLY against the public
// SDK (pkg/plugin) — no conductor internals, no third-party dependencies.
//
// SABnzbd's entire HTTP API is ONE endpoint: every call is a GET to
// {base_url}/api with apikey, output=json, and mode as query parameters, plus
// whatever parameters the mode itself takes (see
// https://sabnzbd.org/wiki/advanced/api). This plugin mirrors that shape
// directly rather than modeling per-verb REST paths.
//
// Connection:
//
//	base_url: "http://sab:8080"  # REQUIRED: base of the instance (no trailing /api)
//	api_key:  "<api key>"        # REQUIRED: SABnzbd Config > General > API Key
//
// The SABnzbd host is operator-specific and self-hosted, so this plugin
// declares NO egress in its capability manifest — the operator is expected to
// scope `network:` on the connector instance to their own SABnzbd host (see
// docs/connectors/sabnzbd.md).
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type sabnzbdPlugin struct{}

func (sabnzbdPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "sabnzbd",
		Desc: "SABnzbd Usenet downloader: queue/history inspection, adding NZB URLs, job control (pause/resume/delete), speed limiting, status/version, and a generic api escape hatch — over SABnzbd's single GET /api endpoint.",
		Connection: plugin.Schema{
			"base_url": {Type: "string", Required: true, Desc: "base URL of the SABnzbd instance, e.g. http://sab:8080 (no trailing /api)"},
			"api_key":  {Type: "string", Required: true, Desc: "SABnzbd API key (Config > General > API Key)"},
		},
		Verbs: sabnzbdVerbs(),
		// The SABnzbd host is operator-specific and self-hosted — there is no
		// fixed hostname this plugin can declare the way api.github.com is
		// fixed for the github connector. An empty manifest is the honest
		// declaration; the operator MUST narrow `network:` on the connector
		// instance to their own instance's host themselves (documented in
		// docs/connectors/sabnzbd.md).
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

// --- verb declarations ---------------------------------------------------

func sabnzbdVerbs() []plugin.Verb {
	resultOut := plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}}
	listOut := plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}, "items": {Type: "list"}}
	return []plugin.Verb{
		{
			Name: "queue", Desc: "read the download queue (queue.slots hoisted into items)",
			Options: plugin.Schema{
				"start": {Type: "integer", Desc: "offset into the queue"},
				"limit": {Type: "integer", Desc: "max slots to return"},
			},
			Outputs: listOut,
		},
		{
			Name: "history", Desc: "read the download history (history.slots hoisted into items)",
			Options: plugin.Schema{
				"start":       {Type: "integer", Desc: "offset into the history"},
				"limit":       {Type: "integer", Desc: "max slots to return"},
				"category":    {Type: "string", Desc: "filter to one category"},
				"failed_only": {Type: "boolean", Desc: "only failed jobs"},
			},
			Outputs: listOut,
		},
		{
			Name: "add_url", Desc: "add an NZB by URL to the queue",
			Options: plugin.Schema{
				"url":      {Type: "string", Required: true, Desc: "URL of the .nzb file to fetch"},
				"name":     {Type: "string", Desc: "custom job display name (nzbname)"},
				"category": {Type: "string", Desc: "SABnzbd category (cat)"},
				"priority": {Type: "string", Desc: "job priority, e.g. -2 (Paused) .. 2 (Force)"},
				"pp":       {Type: "string", Desc: "post-processing option, e.g. 0..3"},
			},
			Outputs: resultOut,
		},
		{
			Name: "pause", Desc: "pause the entire queue",
			Outputs: resultOut,
		},
		{
			Name: "resume", Desc: "resume the entire queue",
			Outputs: resultOut,
		},
		{
			Name: "pause_job", Desc: "pause a single queued job",
			Options: plugin.Schema{"value": {Type: "string", Required: true, Desc: "nzo_id of the job"}},
			Outputs: resultOut,
		},
		{
			Name: "resume_job", Desc: "resume a single queued job",
			Options: plugin.Schema{"value": {Type: "string", Required: true, Desc: "nzo_id of the job"}},
			Outputs: resultOut,
		},
		{
			Name: "delete_job", Desc: "remove a job from the queue",
			Options: plugin.Schema{
				"value":     {Type: "string", Required: true, Desc: "nzo_id of the job, or \"all\""},
				"del_files": {Type: "boolean", Desc: "also delete the downloaded files"},
			},
			Outputs: resultOut,
		},
		{
			Name: "set_speedlimit", Desc: "set the download speed limit",
			Options: plugin.Schema{"value": {Type: "string", Required: true, Desc: "e.g. \"50\" (percent) or \"1M\""}},
			Outputs: resultOut,
		},
		{
			Name: "status", Desc: "full server status (mode=fullstatus)",
			Outputs: resultOut,
		},
		{
			Name: "version", Desc: "SABnzbd version",
			Outputs: resultOut,
		},
		{
			Name: "categories", Desc: "list configured categories (hoisted into items)",
			Outputs: listOut,
		},
		{
			Name: "api", Desc: "call any SABnzbd API mode not covered by a first-class verb",
			Usage: "escape hatch: mode + arbitrary query params",
			Options: plugin.Schema{
				"mode":   {Type: "string", Required: true, Desc: "the SABnzbd API mode, e.g. get_config"},
				"params": {Type: "map", Desc: "additional query parameters for the mode"},
			},
			Outputs: resultOut,
		},
	}
}

// --- request building (pure, hermetically testable — no network) --------

// reqBuild is what one verb call resolves to: the SABnzbd mode, its
// mode-specific query parameters (apikey/output/mode are added centrally by
// Invoke), and — for verbs whose response wraps a natural list — the dotted
// path to hoist into outputs.items.
type reqBuild struct {
	mode  string
	query url.Values
	hoist []string
}

func buildRequest(verb string, o map[string]any) (reqBuild, error) {
	switch verb {
	case "queue":
		q := url.Values{}
		if v := intStr(o["start"]); v != "" {
			q.Set("start", v)
		}
		if v := intStr(o["limit"]); v != "" {
			q.Set("limit", v)
		}
		return reqBuild{mode: "queue", query: q, hoist: []string{"queue", "slots"}}, nil

	case "history":
		q := url.Values{}
		if v := intStr(o["start"]); v != "" {
			q.Set("start", v)
		}
		if v := intStr(o["limit"]); v != "" {
			q.Set("limit", v)
		}
		if v := str(o["category"]); v != "" {
			q.Set("category", v)
		}
		if boolv(o["failed_only"]) {
			q.Set("failed_only", "1")
		}
		return reqBuild{mode: "history", query: q, hoist: []string{"history", "slots"}}, nil

	case "add_url":
		nzbURL, err := requiredStr(o, "url")
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{}
		q.Set("name", nzbURL)
		if v := str(o["name"]); v != "" {
			q.Set("nzbname", v)
		}
		if v := str(o["category"]); v != "" {
			q.Set("cat", v)
		}
		if v := str(o["priority"]); v != "" {
			q.Set("priority", v)
		}
		if v := str(o["pp"]); v != "" {
			q.Set("pp", v)
		}
		return reqBuild{mode: "addurl", query: q}, nil

	case "pause":
		return reqBuild{mode: "pause", query: url.Values{}}, nil

	case "resume":
		return reqBuild{mode: "resume", query: url.Values{}}, nil

	case "pause_job":
		value, err := requiredStr(o, "value")
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{"name": {"pause"}, "value": {value}}
		return reqBuild{mode: "queue", query: q}, nil

	case "resume_job":
		value, err := requiredStr(o, "value")
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{"name": {"resume"}, "value": {value}}
		return reqBuild{mode: "queue", query: q}, nil

	case "delete_job":
		value, err := requiredStr(o, "value")
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{"name": {"delete"}, "value": {value}}
		if boolv(o["del_files"]) {
			q.Set("del_files", "1")
		}
		return reqBuild{mode: "queue", query: q}, nil

	case "set_speedlimit":
		value, err := requiredStr(o, "value")
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{"name": {"speedlimit"}, "value": {value}}
		return reqBuild{mode: "config", query: q}, nil

	case "status":
		return reqBuild{mode: "fullstatus", query: url.Values{}}, nil

	case "version":
		return reqBuild{mode: "version", query: url.Values{}}, nil

	case "categories":
		return reqBuild{mode: "get_cats", query: url.Values{}, hoist: []string{"categories"}}, nil

	case "api":
		mode, err := requiredStr(o, "mode")
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{}
		if m, ok := o["params"].(map[string]any); ok {
			for k, v := range m {
				q.Set(k, fmt.Sprintf("%v", v))
			}
		}
		return reqBuild{mode: mode, query: q}, nil
	}
	return reqBuild{}, fmt.Errorf("unknown verb %q", verb)
}

// --- Invoke ---------------------------------------------------------------

func (sabnzbdPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	if conn == nil {
		conn = map[string]any{}
	}
	baseURL := strings.TrimSpace(str(conn["base_url"]))
	if baseURL == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.base_url is required (e.g. http://sab:8080)")
	}
	apiKey := str(conn["api_key"])
	if strings.TrimSpace(apiKey) == "" {
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
	q.Set("output", "json")
	q.Set("mode", rb.mode)

	status, raw, err := doRequest(context.Background(), baseURL, q)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	if status < 200 || status >= 300 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s: GET /api: %d: %s", req.Verb, status, strings.TrimSpace(string(raw))))
	}

	decoded := parseJSON(raw)
	outputs := map[string]any{"status_code": status, "result": decoded}
	if rb.hoist != nil {
		items := hoist(decoded, rb.hoist...)
		if items == nil {
			items = []any{}
		}
		outputs["items"] = items
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

// --- HTTP transport --------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

// apiURL derives the API endpoint (base_url + /api) — kept as its own
// function so a test can point it at an httptest.Server.
func apiURL(rawBase string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(rawBase), "/")
	if u == "" {
		return "", fmt.Errorf("connection.base_url is required (e.g. http://sab:8080)")
	}
	return u + "/api", nil
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
	raw, err = readAll(resp)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

func readAll(resp *http.Response) ([]byte, error) {
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseJSON(raw []byte) any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// hoist walks a decoded JSON value through a sequence of map keys and returns
// the final value as a list, or nil if any step of the path is missing or
// not the shape expected.
func hoist(v any, path ...string) []any {
	cur := v
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[p]
		if !ok {
			return nil
		}
	}
	list, _ := cur.([]any)
	return list
}

func main() {
	if err := plugin.Serve(sabnzbdPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-sabnzbd: %v\n", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) -----------------------------------------

func str(v any) string { s, _ := v.(string); return s }

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

func requiredStr(o map[string]any, key string) (string, error) {
	v := str(o[key])
	if v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}
