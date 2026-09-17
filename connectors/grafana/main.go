// Command conductor-grafana is a conductor connector (#59) for Grafana, the
// open-source observability platform. It drives the Grafana HTTP API over
// net/http: health, dashboard search/get/create/delete, datasources,
// folders, provisioned alert rules, annotations, org info, and a raw `api`
// escape hatch for anything a first-class verb does not cover. It also
// implements a POLL SOURCE that watches the Grafana-managed Alertmanager for
// firing alerts and emits one `alert` event per alert that is currently
// active. Built ONLY against the public SDK (pkg/plugin, pkg/sourcekit) — no
// other dependency.
//
// Connection (used for both Invoke and StartSource):
//
//	base_url:             "https://grafana.example.com"  # required; no trailing /api
//	api_key:               "<service-account token>"     # required; sent as Authorization: Bearer <api_key>
//	insecure_skip_verify:  false                          # optional; see risk note below
//	poll_interval:         1m                             # optional; source poll period (default 1m)
//
// Every request goes to base_url + <path>, where the path already carries
// its own leading /api (or /api/v1) segment — Grafana's HTTP API is not
// uniformly rooted at one prefix the way some connectors' APIs are (compare
// audiobookshelf, whose every endpoint really does live under one /api).
// Authentication is a Bearer token: a Grafana service-account token or
// legacy API key, both sent identically. A non-2xx response is returned as a
// CodeInternalError carrying the status code and response body — nothing is
// swallowed.
//
// insecure_skip_verify disables TLS certificate verification. Self-hosted
// Grafana instances commonly run behind a self-signed certificate on a LAN,
// so this exists as an explicit, greppable opt-out — but it also disables
// all protection against a man-in-the-middle on the path to the instance.
// Only enable it for instances reached over a trusted network, and prefer
// installing a real certificate when possible.
//
// The source polls GET /api/alertmanager/grafana/api/v2/alerts — the
// Grafana-managed Alertmanager's v2 alerts endpoint — every poll_interval,
// and emits an `alert` event for each alert whose status.state is "active"
// (i.e. currently firing). Dedup is on the alert's fingerprint, so a
// persistently-firing alert emits once, not once per poll cycle.
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
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type grafanaPlugin struct{}

func newGrafanaPlugin() *grafanaPlugin { return &grafanaPlugin{} }

func (p *grafanaPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "grafana",
		Desc: "Grafana: health, dashboard search/get/create/delete, datasources, folders, provisioned alert rules, annotations, org info, and a raw api escape hatch over the Grafana HTTP API — plus a poll source that emits an alert event for each currently-firing alert in the Grafana-managed Alertmanager. Self-hosted; declares no egress (narrow with network: per instance).",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "Grafana instance root, e.g. https://grafana.example.com (no trailing /api)"},
			"api_key":              {Type: "string", Required: true, Desc: "Grafana service-account token or API key, sent as Authorization: Bearer <api_key>"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification (default false); self-signed certs are common on LAN deployments, but this disables protection against MITM — only enable for trusted networks"},
			"poll_interval":        {Type: "duration", Desc: "source poll period (default 1m)"},
		},
		Verbs: grafanaVerbs(),
		Events: []plugin.Event{
			{
				Name: "alert", Desc: "a Grafana-managed alert is currently firing",
				Filters: plugin.Schema{
					"severities": {Type: "list", Desc: "match if labels.severity is one of these"},
					"alertnames": {Type: "list", Desc: "match if labels.alertname is one of these"},
				},
				Context: plugin.Schema{
					"fingerprint": {Type: "string"},
					"alertname":   {Type: "string"},
					"severity":    {Type: "string"},
					"status":      {Type: "any", Desc: "{state, silencedBy, inhibitedBy}"},
					"startsAt":    {Type: "string"},
					"summary":     {Type: "string"},
				},
			},
		},
		// Grafana is commonly self-hosted: there is no fixed public host to
		// declare. The operator narrows egress to their own instance with
		// `network: ["grafana.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func grafanaVerbs() []plugin.Verb {
	return []plugin.Verb{
		{
			Name: "health", Desc: "check API health",
			Usage:   "GET /api/health",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "search", Desc: "search dashboards/folders",
			Usage: "GET /api/search",
			Options: plugin.Schema{
				"query": {Type: "string", Desc: "search text"},
				"type":  {Type: "string", Enum: []string{"dash-db", "dash-folder"}, Desc: "restrict to dashboards or folders"},
				"tag":   {Type: "list", Desc: "restrict to results carrying all of these tags"},
			},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "dashboard_get", Desc: "get one dashboard by uid",
			Usage: "GET /api/dashboards/uid/{uid}",
			Options: plugin.Schema{
				"uid": {Type: "string", Required: true, Scope: "dashboard"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "dashboard_create", Desc: "create or update a dashboard",
			Usage: "POST /api/dashboards/db",
			Options: plugin.Schema{
				"dashboard":  {Type: "any", Required: true, Desc: "the dashboard JSON model"},
				"folder_uid": {Type: "string", Desc: "destination folder uid; empty = General"},
				"overwrite":  {Type: "boolean", Desc: "overwrite an existing dashboard with the same uid/version conflict"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "dashboard_delete", Desc: "delete a dashboard by uid",
			Usage: "DELETE /api/dashboards/uid/{uid}",
			Options: plugin.Schema{
				"uid": {Type: "string", Required: true, Scope: "dashboard"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "datasources", Desc: "list datasources",
			Usage:   "GET /api/datasources",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "datasource_get", Desc: "get one datasource by uid",
			Usage: "GET /api/datasources/uid/{uid}",
			Options: plugin.Schema{
				"uid": {Type: "string", Required: true, Scope: "datasource"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "folders", Desc: "list folders",
			Usage:   "GET /api/folders",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "folder_create", Desc: "create a folder",
			Usage: "POST /api/folders",
			Options: plugin.Schema{
				"title": {Type: "string", Required: true},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "alert_rules", Desc: "list provisioned alert rules",
			Usage:   "GET /api/v1/provisioning/alert-rules",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "annotations", Desc: "search annotations",
			Usage: "GET /api/annotations",
			Options: plugin.Schema{
				"from": {Type: "integer", Desc: "epoch millis, range start"},
				"to":   {Type: "integer", Desc: "epoch millis, range end"},
				"tags": {Type: "list", Desc: "restrict to annotations carrying all of these tags"},
			},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "annotation_create", Desc: "create an annotation",
			Usage: "POST /api/annotations",
			Options: plugin.Schema{
				"dashboard_uid": {Type: "string", Scope: "dashboard", Desc: "attach to this dashboard"},
				"panel_id":      {Type: "integer", Desc: "attach to this panel within the dashboard"},
				"time":          {Type: "integer", Desc: "epoch millis; defaults to now on the server"},
				"time_end":      {Type: "integer", Desc: "epoch millis; makes the annotation a region"},
				"tags":          {Type: "list"},
				"text":          {Type: "string", Required: true},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "org", Desc: "the authenticated org",
			Usage:   "GET /api/org",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "api", Desc: "raw escape hatch: any Grafana API endpoint",
			Usage: "method + path under base_url, for anything without a first-class verb",
			Options: plugin.Schema{
				"method": {Type: "string", Desc: "HTTP method (default GET)"},
				"path":   {Type: "string", Required: true, Desc: "path under base_url, e.g. /api/dashboards/uid/abc123"},
				"query":  {Type: "map", Desc: "query string parameters"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
	}
}

func (p *grafanaPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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
	case "health":
		return p.get(ctx, conn, "/api/health", nil)
	case "search":
		return p.search(ctx, conn, o)
	case "dashboard_get":
		return p.dashboardGet(ctx, conn, o)
	case "dashboard_create":
		return p.dashboardCreate(ctx, conn, o)
	case "dashboard_delete":
		return p.dashboardDelete(ctx, conn, o)
	case "datasources":
		return p.get(ctx, conn, "/api/datasources", nil)
	case "datasource_get":
		return p.datasourceGet(ctx, conn, o)
	case "folders":
		return p.get(ctx, conn, "/api/folders", nil)
	case "folder_create":
		return p.folderCreate(ctx, conn, o)
	case "alert_rules":
		return p.get(ctx, conn, "/api/v1/provisioning/alert-rules", nil)
	case "annotations":
		return p.annotations(ctx, conn, o)
	case "annotation_create":
		return p.annotationCreate(ctx, conn, o)
	case "org":
		return p.get(ctx, conn, "/api/org", nil)
	case "api":
		return p.api(ctx, conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type grafanaConn struct {
	baseURL            string
	apiKey             string
	insecureSkipVerify bool
	pollInterval       time.Duration
}

func parseConn(m map[string]any) (grafanaConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return grafanaConn{}, fmt.Errorf("base_url is required")
	}
	key := str(m["api_key"])
	if key == "" {
		return grafanaConn{}, fmt.Errorf("api_key is required")
	}
	c := grafanaConn{
		baseURL:            strings.TrimRight(base, "/"),
		apiKey:             key,
		insecureSkipVerify: boolv(m["insecure_skip_verify"]),
		pollInterval:       time.Minute,
	}
	d, err := toDuration(m["poll_interval"])
	if err != nil {
		return grafanaConn{}, fmt.Errorf("poll_interval: %w", err)
	}
	if d > 0 {
		c.pollInterval = d
	}
	return c, nil
}

// --- verb implementations ---

// get is the shape most GET verbs share: no body, shape the response as
// result or items.
func (p *grafanaPlugin) get(ctx context.Context, conn grafanaConn, path string, query url.Values) (plugin.InvokeResult, error) {
	return p.doAndShape(ctx, conn, http.MethodGet, path, query, nil)
}

func (p *grafanaPlugin) search(ctx context.Context, conn grafanaConn, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := str(o["query"]); v != "" {
		q.Set("query", v)
	}
	if v := str(o["type"]); v != "" {
		q.Set("type", v)
	}
	addAll(q, "tag", strList(o["tag"]))
	return p.doAndShape(ctx, conn, http.MethodGet, "/api/search", q, nil)
}

func (p *grafanaPlugin) dashboardGet(ctx context.Context, conn grafanaConn, o map[string]any) (plugin.InvokeResult, error) {
	uid := str(o["uid"])
	if uid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uid is required")
	}
	return p.doAndShape(ctx, conn, http.MethodGet, "/api/dashboards/uid/"+url.PathEscape(uid), nil, nil)
}

func (p *grafanaPlugin) dashboardCreate(ctx context.Context, conn grafanaConn, o map[string]any) (plugin.InvokeResult, error) {
	dashboard, ok := o["dashboard"]
	if !ok || dashboard == nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "dashboard is required")
	}
	body := map[string]any{"dashboard": dashboard}
	if v := str(o["folder_uid"]); v != "" {
		body["folderUid"] = v
	}
	if v, ok := o["overwrite"]; ok {
		body["overwrite"] = boolv(v)
	}
	return p.doAndShape(ctx, conn, http.MethodPost, "/api/dashboards/db", nil, body)
}

func (p *grafanaPlugin) dashboardDelete(ctx context.Context, conn grafanaConn, o map[string]any) (plugin.InvokeResult, error) {
	uid := str(o["uid"])
	if uid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uid is required")
	}
	return p.doAndShape(ctx, conn, http.MethodDelete, "/api/dashboards/uid/"+url.PathEscape(uid), nil, nil)
}

func (p *grafanaPlugin) datasourceGet(ctx context.Context, conn grafanaConn, o map[string]any) (plugin.InvokeResult, error) {
	uid := str(o["uid"])
	if uid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uid is required")
	}
	return p.doAndShape(ctx, conn, http.MethodGet, "/api/datasources/uid/"+url.PathEscape(uid), nil, nil)
}

func (p *grafanaPlugin) folderCreate(ctx context.Context, conn grafanaConn, o map[string]any) (plugin.InvokeResult, error) {
	title := str(o["title"])
	if title == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "title is required")
	}
	return p.doAndShape(ctx, conn, http.MethodPost, "/api/folders", nil, map[string]any{"title": title})
}

func (p *grafanaPlugin) annotations(ctx context.Context, conn grafanaConn, o map[string]any) (plugin.InvokeResult, error) {
	q := url.Values{}
	if v := intStr(o["from"]); v != "" {
		q.Set("from", v)
	}
	if v := intStr(o["to"]); v != "" {
		q.Set("to", v)
	}
	addAll(q, "tags", strList(o["tags"]))
	return p.doAndShape(ctx, conn, http.MethodGet, "/api/annotations", q, nil)
}

func (p *grafanaPlugin) annotationCreate(ctx context.Context, conn grafanaConn, o map[string]any) (plugin.InvokeResult, error) {
	text := str(o["text"])
	if text == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "text is required")
	}
	body := map[string]any{"text": text}
	if v := str(o["dashboard_uid"]); v != "" {
		body["dashboardUID"] = v
	}
	if v, ok := o["panel_id"]; ok {
		body["panelId"] = v
	}
	if v, ok := o["time"]; ok {
		body["time"] = v
	}
	if v, ok := o["time_end"]; ok {
		body["timeEnd"] = v
	}
	if tags := strList(o["tags"]); len(tags) > 0 {
		body["tags"] = tags
	}
	return p.doAndShape(ctx, conn, http.MethodPost, "/api/annotations", nil, body)
}

func (p *grafanaPlugin) api(ctx context.Context, conn grafanaConn, o map[string]any) (plugin.InvokeResult, error) {
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
func (p *grafanaPlugin) doAndShape(ctx context.Context, conn grafanaConn, method, path string, query url.Values, body any) (plugin.InvokeResult, error) {
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

// do performs one HTTP request against the Grafana API, attaching the Bearer
// token, and returns the status code and raw response body. A non-2xx status
// is translated into a CodeInternalError carrying the status and body — the
// caller never has to check status codes itself.
func (p *grafanaPlugin) do(ctx context.Context, conn grafanaConn, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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
	req.Header.Set("Authorization", "Bearer "+conn.apiKey)
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
func (p *grafanaPlugin) clientFor(conn grafanaConn) *http.Client {
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

// --- source: poll the Grafana-managed Alertmanager for firing alerts ---

// alertsPath is the Grafana-managed Alertmanager's v2 alerts endpoint —
// Grafana proxies the embedded Alertmanager under /api/alertmanager/grafana.
const alertsPath = "/api/alertmanager/grafana/api/v2/alerts"

// backoff is how long the source waits after a failed poll before retrying,
// so a misconfigured token or an unreachable instance doesn't become a hot
// loop.
const backoff = 30 * time.Second

// grafanaAlert is one element of the v2 alerts response — trimmed to the
// fields the source reads.
type grafanaAlert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
	Status       struct {
		State       string   `json:"state"`
		SilencedBy  []string `json:"silencedBy"`
		InhibitedBy []string `json:"inhibitedBy"`
	} `json:"status"`
}

// StartSource polls GET /api/alertmanager/grafana/api/v2/alerts every
// poll_interval and emits an `alert` event for each alert that is currently
// active (firing). Dedup is on the alert's fingerprint, so a persistently
// firing alert emits once, not once per cycle.
func (p *grafanaPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	conn, err := parseConn(req.Config)
	if err != nil {
		return fmt.Errorf("grafana: %w", err)
	}
	dedup := sourcekit.NewDedup(2048)
	fmt.Fprintf(os.Stderr, "grafana[%s]: polling firing alerts every %s\n", req.Instance, conn.pollInterval)
	for {
		if ctx.Err() != nil {
			return nil
		}
		alerts, err := p.pollAlerts(ctx, conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "grafana[%s]: poll: %v\n", req.Instance, err)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			continue
		}
		for _, a := range alerts {
			ev, ok := alertEvent(a)
			if !ok {
				continue
			}
			if !dedup.Add(a.Fingerprint) {
				continue
			}
			_ = emit(ev)
		}
		if !sleepCtx(ctx, conn.pollInterval) {
			return nil
		}
	}
}

// pollAlerts fetches and decodes the current alert list. Only a transport or
// decode failure is an error — an empty list is normal.
func (p *grafanaPlugin) pollAlerts(ctx context.Context, conn grafanaConn) ([]grafanaAlert, error) {
	status, body, err := p.do(ctx, conn, http.MethodGet, conn.baseURL+alertsPath, nil, nil)
	if err != nil {
		return nil, err
	}
	var alerts []grafanaAlert
	if err := json.Unmarshal(body, &alerts); err != nil {
		return nil, fmt.Errorf("decode v2 alerts (status %d): %w", status, err)
	}
	return alerts, nil
}

// alertEvent is the source's WHOLE decision, kept pure so it is testable
// without a live Grafana instance: given one alert from the v2 alerts
// response, it reports whether that alert is currently firing and, if so,
// builds the event.
//
// Only status.state == "active" emits — a resolved alert, or one that is
// merely silenced/suppressed, is not a firing alert.
func alertEvent(a grafanaAlert) (map[string]any, bool) {
	if a.Status.State != "active" {
		return nil, false
	}
	alertname := a.Labels["alertname"]
	severity := a.Labels["severity"]
	title := "grafana: " + nonEmpty(alertname, a.Annotations["summary"], "alert") + " firing"
	return map[string]any{
		"event": "alert",
		"kind":  "alert",
		"title": title,
		"dedup": a.Fingerprint,
		"context": map[string]any{
			"fingerprint": a.Fingerprint,
			"alertname":   alertname,
			"severity":    severity,
			"status": map[string]any{
				"state":       a.Status.State,
				"silencedBy":  a.Status.SilencedBy,
				"inhibitedBy": a.Status.InhibitedBy,
			},
			"startsAt": a.StartsAt,
			"summary":  a.Annotations["summary"],
			// Plural aliases so the documented filter vocabulary (filters:
			// {severities/alertnames: [...]}) matches against the daemon's
			// generic list-contains filter evaluator — the same convention
			// the smart and alertmanager connectors use.
			"severities": severity,
			"alertnames": alertname,
		},
	}, true
}

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
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
	if err := plugin.Serve(newGrafanaPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-grafana:", err)
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

// addAll sets one query parameter per value in vals, all under the same key —
// Grafana's search/annotations endpoints take a repeatable ?tag=/?tags=.
func addAll(q url.Values, key string, vals []string) {
	for _, v := range vals {
		q.Add(key, v)
	}
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
