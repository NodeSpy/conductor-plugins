// Command conductor-netdata is a conductor connector (#59) for Netdata, the
// open-source real-time monitoring agent. It drives the Netdata Agent REST
// API over net/http: info, charts, chart, data, alarms, alarm_log, the newer
// v2 alerts, contexts, and a raw `api` escape hatch for anything a
// first-class verb does not cover. It also implements a POLL SOURCE that
// watches /api/v1/alarms for raised alarms and emits an `alarm` event for
// every alarm currently in WARNING or CRITICAL. Built ONLY against the
// public SDK (pkg/plugin, pkg/sourcekit) — no other dependency.
//
// Connection (used for both Invoke and StartSource):
//
//	base_url:             "http://netdata.example.com:19999"  # required
//	api_key:               "<bearer token>"                   # optional; sent as Authorization: Bearer <api_key>
//	insecure_skip_verify:  false                                # optional; see risk note below
//	poll_interval:         1m                                   # optional; source poll period (default 1m)
//
// A self-hosted Netdata agent is very often unauthenticated on its own LAN,
// so api_key is optional: when it is empty, no Authorization header is sent
// at all — a protected agent (behind a reverse proxy) or Netdata Cloud
// relay can still be reached by setting it. A non-2xx response is returned
// as a CodeInternalError carrying the status code and response body —
// nothing is swallowed.
//
// insecure_skip_verify disables TLS certificate verification. Self-hosted
// Netdata agents commonly run behind a self-signed certificate on a LAN, so
// this exists as an explicit, greppable opt-out — but it also disables all
// protection against a man-in-the-middle on the path to the instance. Only
// enable it for instances reached over a trusted network, and prefer
// installing a real certificate when possible.
//
// The source polls GET /api/v1/alarms?active=true — which returns
// {"alarms": {<name>: {status, value, ...}}} — every poll_interval, and
// emits one `alarm` event per alarm whose status is WARNING or CRITICAL.
// Dedup is on the alarm's name + status, so a flapping alarm re-emits on
// every status change but a steady alarm emits once.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type netdataPlugin struct{}

func newNetdataPlugin() *netdataPlugin { return &netdataPlugin{} }

func (p *netdataPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "netdata",
		Desc: "Netdata: info, charts, chart, data, alarms, alarm_log, the newer v2 alerts, contexts, and a raw api escape hatch over the Netdata Agent REST API — plus a poll source that emits an alarm event for every alarm currently in WARNING or CRITICAL. Self-hosted; declares no egress (narrow with network: per instance).",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "Netdata agent root, e.g. http://netdata.example.com:19999"},
			"api_key":              {Type: "string", Desc: "bearer token, sent as Authorization: Bearer <api_key>; optional — a self-hosted agent is often open on the LAN and needs no auth header"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification (default false); self-signed certs are common on LAN deployments, but this disables protection against MITM — only enable for trusted networks"},
			"poll_interval":        {Type: "duration", Desc: "source poll period (default 1m)"},
		},
		Verbs: netdataVerbs(),
		Events: []plugin.Event{
			{
				Name: "alarm", Desc: "a Netdata alarm is currently raised (WARNING or CRITICAL)",
				Filters: plugin.Schema{
					"statuses": {Type: "list", Desc: "match if status is one of these (e.g. WARNING, CRITICAL)"},
					"charts":   {Type: "list", Desc: "match if chart is one of these"},
				},
				Context: plugin.Schema{
					"name":               {Type: "string"},
					"chart":              {Type: "string"},
					"family":             {Type: "string"},
					"status":             {Type: "string"},
					"value":              {Type: "number"},
					"units":              {Type: "string"},
					"info":               {Type: "string"},
					"last_status_change": {Type: "number"},
					"statuses":           {Type: "string", Desc: "alias of status, for the statuses filter"},
					"charts":             {Type: "string", Desc: "alias of chart, for the charts filter"},
				},
			},
		},
		// Netdata is commonly self-hosted: there is no fixed public host to
		// declare. The operator narrows egress to their own instance with
		// `network: ["netdata.example.com:19999"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func netdataVerbs() []plugin.Verb {
	return []plugin.Verb{
		{
			Name: "info", Desc: "agent identity and build info",
			Usage:   "GET /api/v1/info",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "charts", Desc: "list every chart the agent collects",
			Usage:   "GET /api/v1/charts",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "chart", Desc: "get one chart's definition",
			Usage: "GET /api/v1/chart",
			Options: plugin.Schema{
				"chart": {Type: "string", Required: true, Scope: "chart", Desc: "chart id, e.g. system.cpu"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "data", Desc: "time-series data for one chart",
			Usage: "GET /api/v1/data",
			Options: plugin.Schema{
				"chart":      {Type: "string", Required: true, Scope: "chart", Desc: "chart id, e.g. system.cpu"},
				"after":      {Type: "integer", Desc: "start time (unix seconds, or a negative relative offset)"},
				"before":     {Type: "integer", Desc: "end time (unix seconds, or a negative relative offset)"},
				"points":     {Type: "integer", Desc: "number of points to return"},
				"dimensions": {Type: "list", Desc: "restrict to these dimensions"},
				"format":     {Type: "string", Enum: []string{"json", "json2", "csv", "tsv", "ssv", "datatable", "datasource", "array", "html"}, Desc: "response format (default json)"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "alarms", Desc: "currently configured alarms",
			Usage: "GET /api/v1/alarms",
			Options: plugin.Schema{
				"all": {Type: "boolean", Desc: "return every configured alarm (?all=true) instead of only active/raised ones (?active=true, the default)"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "alarm_log", Desc: "the alarm transition log",
			Usage: "GET /api/v1/alarm_log",
			Options: plugin.Schema{
				"after": {Type: "integer", Desc: "unix timestamp; only entries after this time"},
			},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "alerts", Desc: "the newer v2 alerts API",
			Usage:   "GET /api/v2/alerts",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "contexts", Desc: "list every metric context the agent tracks",
			Usage:   "GET /api/v1/contexts",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "api", Desc: "raw escape hatch: any Netdata API endpoint",
			Usage: "method + path under base_url, for anything without a first-class verb",
			Options: plugin.Schema{
				"method": {Type: "string", Desc: "HTTP method (default GET)"},
				"path":   {Type: "string", Required: true, Desc: "path under base_url, e.g. /api/v1/info"},
				"query":  {Type: "map", Desc: "query string parameters"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
	}
}

func (p *netdataPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	ctx := context.Background()

	switch req.Verb {
	case "info":
		return p.get(ctx, conn, "/api/v1/info", nil)
	case "charts":
		return p.get(ctx, conn, "/api/v1/charts", nil)
	case "chart":
		return p.chart(ctx, conn, o)
	case "data":
		return p.data(ctx, conn, o)
	case "alarms":
		return p.alarms(ctx, conn, o)
	case "alarm_log":
		return p.alarmLog(ctx, conn, o)
	case "alerts":
		return p.get(ctx, conn, "/api/v2/alerts", nil)
	case "contexts":
		return p.get(ctx, conn, "/api/v1/contexts", nil)
	case "api":
		return p.api(ctx, conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type netdataConn struct {
	baseURL            string
	apiKey             string
	insecureSkipVerify bool
	pollInterval       time.Duration
}

func parseConn(m map[string]any) (netdataConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return netdataConn{}, fmt.Errorf("base_url is required")
	}
	c := netdataConn{
		baseURL:            strings.TrimRight(base, "/"),
		apiKey:             str(m["api_key"]),
		insecureSkipVerify: boolv(m["insecure_skip_verify"]),
		pollInterval:       time.Minute,
	}
	d, err := toDuration(m["poll_interval"])
	if err != nil {
		return netdataConn{}, fmt.Errorf("poll_interval: %w", err)
	}
	if d > 0 {
		c.pollInterval = d
	}
	return c, nil
}

// --- verb implementations ---

// get is the shape most GET verbs share: no body, shape the response as
// result or items.
func (p *netdataPlugin) get(ctx context.Context, conn netdataConn, path string, query url.Values) (plugin.InvokeResult, error) {
	return p.doAndShape(ctx, conn, http.MethodGet, path, query, nil)
}

func (p *netdataPlugin) chart(ctx context.Context, conn netdataConn, o map[string]any) (plugin.InvokeResult, error) {
	chart := str(o["chart"])
	if chart == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "chart is required")
	}
	q := url.Values{"chart": []string{chart}}
	return p.doAndShape(ctx, conn, http.MethodGet, "/api/v1/chart", q, nil)
}

func (p *netdataPlugin) data(ctx context.Context, conn netdataConn, o map[string]any) (plugin.InvokeResult, error) {
	chart := str(o["chart"])
	if chart == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "chart is required")
	}
	q := url.Values{"chart": []string{chart}}
	if v := intStr(o["after"]); v != "" {
		q.Set("after", v)
	}
	if v := intStr(o["before"]); v != "" {
		q.Set("before", v)
	}
	if v := intStr(o["points"]); v != "" {
		q.Set("points", v)
	}
	if dims := strList(o["dimensions"]); len(dims) > 0 {
		q.Set("dimensions", strings.Join(dims, ","))
	}
	if v := str(o["format"]); v != "" {
		q.Set("format", v)
	}
	return p.doAndShape(ctx, conn, http.MethodGet, "/api/v1/data", q, nil)
}

func (p *netdataPlugin) alarms(ctx context.Context, conn netdataConn, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if boolv(o["all"]) {
		q.Set("all", "true")
	} else {
		q.Set("active", "true")
	}
	return p.doAndShape(ctx, conn, http.MethodGet, "/api/v1/alarms", q, nil)
}

func (p *netdataPlugin) alarmLog(ctx context.Context, conn netdataConn, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := intStr(o["after"]); v != "" {
		q.Set("after", v)
	}
	return p.doAndShape(ctx, conn, http.MethodGet, "/api/v1/alarm_log", q, nil)
}

func (p *netdataPlugin) api(ctx context.Context, conn netdataConn, o map[string]any) (plugin.InvokeResult, error) {
	path := str(o["path"])
	if path == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "path is required")
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
	return p.doAndShape(ctx, conn, method, path, q, o["body"])
}

// --- HTTP plumbing ---

// doAndShape performs one request and shapes the decoded body: a bare JSON
// array becomes `items`, anything else non-nil becomes `result`. status_code
// is always set.
func (p *netdataPlugin) doAndShape(ctx context.Context, conn netdataConn, method, path string, query url.Values, body any) (plugin.InvokeResult, error) {
	status, respBody, err := p.do(ctx, conn, method, conn.baseURL+path, query, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(respBody)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	out := map[string]any{"status_code": status}
	switch v := decoded.(type) {
	case []any:
		out["items"] = v
	default:
		if decoded != nil {
			out["result"] = decoded
		}
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// do performs one HTTP request against the Netdata agent, attaching the
// Bearer token WHEN one is configured, and returns the status code and raw
// response body. A non-2xx status is translated into a CodeInternalError
// carrying the status and body — the caller never has to check status codes
// itself.
func (p *netdataPlugin) do(ctx context.Context, conn netdataConn, method, endpoint string, query url.Values, body any) (int, []byte, error) {
	full := endpoint
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, reader)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if conn.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+conn.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.clientFor(conn).Do(req)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, respBody, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s %s: %d %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(respBody))))
	}
	return resp.StatusCode, respBody, nil
}

// clientFor builds an *http.Client for one request. The transport is only
// customized (skip TLS verification) when the connection asks for it, so the
// common case pays no extra cost and gets normal certificate validation.
func (p *netdataPlugin) clientFor(conn netdataConn) *http.Client {
	client := &http.Client{Timeout: 30 * time.Second}
	if conn.insecureSkipVerify {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- explicit, documented opt-out for self-signed LAN instances
		}
	}
	return client
}

// decodeJSON decodes a JSON response body into a generic value. An empty
// body decodes to nil rather than an error (e.g. a 204 with no body).
func decodeJSON(body []byte) (any, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// --- source: poll for raised alarms ---

// backoff is how long the source waits after a failed poll before retrying,
// so a misconfigured host or an unreachable agent doesn't become a hot loop.
const backoff = 30 * time.Second

// StartSource polls GET /api/v1/alarms?active=true every poll_interval and
// emits an `alarm` event for each alarm whose status is WARNING or CRITICAL.
// Dedup is on the alarm's name + status, so an alarm that stays raised for
// hours emits once, not once per poll cycle, while a flap (status change)
// re-emits.
func (p *netdataPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	conn, err := parseConn(req.Config)
	if err != nil {
		return fmt.Errorf("netdata: %w", err)
	}
	dedup := sourcekit.NewDedup(2048)
	fmt.Fprintf(os.Stderr, "netdata[%s]: polling active alarms every %s\n", req.Instance, conn.pollInterval)
	for {
		if ctx.Err() != nil {
			return nil
		}
		alarms, err := p.pollAlarms(ctx, conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "netdata[%s]: poll: %v\n", req.Instance, err)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			continue
		}
		for _, key := range sortedKeys(alarms) {
			entry, ok := alarms[key].(map[string]any)
			if !ok {
				continue
			}
			ev, ok := alarmEvent(key, entry)
			if !ok {
				continue
			}
			if !dedup.Add(str(ev["dedup"])) {
				continue
			}
			_ = emit(ev)
		}
		if !sleepCtx(ctx, conn.pollInterval) {
			return nil
		}
	}
}

// pollAlarms fetches and decodes GET /api/v1/alarms?active=true, returning
// the {name: alarm} map. Only a transport or decode failure is an error — no
// active alarms is normal.
func (p *netdataPlugin) pollAlarms(ctx context.Context, conn netdataConn) (map[string]any, error) {
	status, body, err := p.do(ctx, conn, http.MethodGet, conn.baseURL+"/api/v1/alarms", url.Values{"active": []string{"true"}}, nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Alarms map[string]any `json:"alarms"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode alarms (status %d): %w", status, err)
	}
	return doc.Alarms, nil
}

// alarmEvent is the source's WHOLE decision, kept pure so it is testable
// without a live Netdata agent: given one alarm's map key (its identity in
// the /api/v1/alarms response) and its decoded fields, it reports whether
// that alarm is currently raised and, if so, builds the event.
//
// Only status WARNING or CRITICAL emits — CLEAR, UNDEFINED, and any other
// status is not a raised alarm.
func alarmEvent(key string, a map[string]any) (map[string]any, bool) {
	status := gs(a, "status")
	if status != "WARNING" && status != "CRITICAL" {
		return nil, false
	}
	name := gs(a, "name")
	if name == "" {
		name = key
	}
	chart := gs(a, "chart")
	family := gs(a, "family")
	title := "netdata: " + name + " is " + status
	if chart != "" {
		title += " (" + chart + ")"
	}
	return map[string]any{
		"event": "alarm",
		"kind":  "alarm",
		"title": title,
		"dedup": name + "\x00" + status,
		"context": map[string]any{
			"name":               name,
			"chart":              chart,
			"family":             family,
			"status":             status,
			"value":              a["value"],
			"units":              gs(a, "units"),
			"info":               gs(a, "info"),
			"last_status_change": a["last_status_change"],
			// Plural aliases so the documented filter vocabulary (filters:
			// {statuses/charts: [...]}) matches against the daemon's generic
			// list-contains filter evaluator — the same convention the smart
			// and grafana connectors use.
			"statuses": status,
			"charts":   chart,
		},
	}, true
}

// sortedKeys returns m's keys in sorted order, so the source's per-cycle
// iteration (and therefore emit order) is deterministic and testable.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sleepCtx sleeps d or returns early (false) if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func main() {
	if err := plugin.Serve(newNetdataPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-netdata:", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) ---

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strOr(v any, def string) string {
	if s := str(v); s != "" {
		return s
	}
	return def
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

// gs reads a string field, "" if absent or the wrong type.
func gs(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// strList reads a list-of-strings option: a []any of strings (the wire
// shape), a []string, or a single string. Empty entries are dropped.
func strList(v any) []string {
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
		if x == "" {
			return nil
		}
		return []string{x}
	}
	return nil
}

// intStr renders an integer-ish option as a string ("" if absent).
func intStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case string:
		return x
	}
	return fmt.Sprintf("%v", v)
}

// toDuration parses a duration option: a Go duration string ("1m"), or a
// number interpreted as seconds. Zero/absent -> 0 (use the default).
func toDuration(v any) (time.Duration, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case string:
		if x == "" {
			return 0, nil
		}
		return time.ParseDuration(x)
	case float64:
		return time.Duration(x * float64(time.Second)), nil
	case int:
		return time.Duration(x) * time.Second, nil
	case int64:
		return time.Duration(x) * time.Second, nil
	}
	return 0, fmt.Errorf("invalid duration %v", v)
}
