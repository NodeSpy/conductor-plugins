// Command conductor-truenas is a conductor connector (#59) for TrueNAS SCALE:
// a verb-rich HTTP connector over the SCALE middleware REST API
// (base_url + "/api/v2.0") plus a POLL SOURCE that watches /alert/list and
// emits an `alert` event for every active (not dismissed) alert. Built ONLY
// against the public SDK (pkg/plugin) + connector-kit (pkg/sourcekit) — no
// other dependency.
//
// Connection:
//
//	base_url:             "https://truenas.example.com"  # required; SCALE web UI root
//	api_key:               "<api key>"                    # required; sent as Authorization: Bearer <api_key>
//	insecure_skip_verify:  false                           # optional; skip TLS verification for self-signed certs
//	poll_interval:         1m                              # optional; the alert-source poll period
//
// Every request goes to base_url + "/api/v2.0" + <endpoint>, with the API key
// as a Bearer credential. A non-2xx response is returned as a
// CodeInternalError carrying the status code and response body — nothing is
// swallowed.
//
// insecure_skip_verify exists because self-signed certificates are the norm
// on a home-lab TrueNAS box; setting it disables TLS certificate verification
// for that connector instance, which means the connection is no longer
// protected against a man-in-the-middle. Prefer importing the box's real CA
// certificate (or the SCALE UI's Let's Encrypt cert) instead when possible.
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

type truenasPlugin struct {
	client         *http.Client // default: TLS verified
	insecureClient *http.Client // insecure_skip_verify: true
}

func newTruenasPlugin() *truenasPlugin {
	return &truenasPlugin{
		client: &http.Client{Timeout: 30 * time.Second},
		insecureClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // opt-in per connection, for self-signed home-lab certs
		},
	}
}

// httpClient picks the TLS-verified or TLS-skipping client for one connection,
// so InsecureSkipVerify is scoped to the connector instance that asked for it
// rather than to the whole process.
func (p *truenasPlugin) httpClient(conn truenasConn) *http.Client {
	if conn.insecureSkipVerify {
		return p.insecureClient
	}
	return p.client
}

func (p *truenasPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "truenas",
		Desc: "TrueNAS SCALE: system info, pools, datasets, snapshots, replication, apps, alerts, and services over the SCALE middleware REST API (/api/v2.0), plus a raw `api` escape hatch and a poll source that emits an alert event for every active alert. Self-hosted; declares no egress (narrow with network: per instance).",
		Connection: plugin.Schema{
			"base_url":             {Type: "string", Required: true, Desc: "TrueNAS SCALE root, e.g. https://truenas.example.com"},
			"api_key":              {Type: "string", Required: true, Desc: "TrueNAS API key, sent as Authorization: Bearer <api_key>"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification (default false) — common with self-signed certs, but disables protection against a man-in-the-middle"},
			"poll_interval":        {Type: "duration", Desc: "alert-source poll period (default 1m)"},
		},
		Verbs: truenasVerbs(),
		Events: []plugin.Event{
			{
				Name: "alert", Desc: "an ACTIVE (not dismissed) TrueNAS alert",
				Filters: plugin.Schema{
					"levels":  {Type: "list", Desc: "match if level is one of these (INFO/WARNING/CRITICAL/...)"},
					"klasses": {Type: "list", Desc: "match if klass is one of these"},
				},
				Context: plugin.Schema{
					"id":        {Type: "string", Desc: "the alert's id/uuid"},
					"uuid":      {Type: "string", Desc: "same as id"},
					"level":     {Type: "string", Desc: "INFO/WARNING/CRITICAL/..."},
					"klass":     {Type: "string", Desc: "the alert class"},
					"formatted": {Type: "string", Desc: "the formatted alert message"},
					"dismissed": {Type: "boolean", Desc: "always false (the event only fires while active)"},
					"datetime":  {Type: "string", Desc: "when the alert was raised"},
					"node":      {Type: "string", Desc: "the cluster node that raised it (HA systems)"},
				},
			},
		},
		// TrueNAS is self-hosted: there is no fixed public host to declare. The
		// operator narrows egress to their own instance with
		// `network: ["truenas.example.com:443"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func truenasVerbs() []plugin.Verb {
	return []plugin.Verb{
		{
			Name: "system_info", Desc: "system identity/version/hardware info",
			Usage:   "GET /system/info",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "pools", Desc: "list storage pools",
			Usage:   "GET /pool",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "pool_get", Desc: "get one pool's details",
			Usage: "GET /pool/id/{id}",
			Options: plugin.Schema{
				"pool_id": {Type: "string", Required: true, Scope: "pool"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "datasets", Desc: "list ZFS datasets",
			Usage:   "GET /pool/dataset",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "dataset_get", Desc: "get one dataset's details",
			Usage: "GET /pool/dataset/id/{id} (id is URL-encoded, e.g. tank/data -> tank%2Fdata)",
			Options: plugin.Schema{
				"dataset_id": {Type: "string", Required: true, Scope: "dataset", Desc: "e.g. tank/data"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "snapshots", Desc: "list ZFS snapshots",
			Usage:   "GET /zfs/snapshot",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "snapshot_create", Desc: "create a ZFS snapshot",
			Usage: "POST /zfs/snapshot",
			Options: plugin.Schema{
				"dataset": {Type: "string", Required: true, Scope: "dataset", Desc: "dataset to snapshot, e.g. tank/data"},
				"name":    {Type: "string", Required: true, Desc: "snapshot name"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "replication", Desc: "list replication tasks",
			Usage:   "GET /replication",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "apps", Desc: "list SCALE apps",
			Usage:   "GET /app",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "alerts", Desc: "list current alerts",
			Usage:   "GET /alert/list",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "alert_dismiss", Desc: "dismiss an alert",
			Usage: "POST /alert/dismiss (body: the alert uuid as a JSON string)",
			Options: plugin.Schema{
				"uuid": {Type: "string", Required: true, Scope: "alert", Desc: "the alert's uuid"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "services", Desc: "list services and their running state",
			Usage:   "GET /service",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "service_control", Desc: "start or stop a service",
			Usage: "POST /service/start or /service/stop",
			Options: plugin.Schema{
				"service": {Type: "string", Required: true, Scope: "service", Desc: "service name, e.g. cifs, ssh, nfs"},
				"action":  {Type: "string", Required: true, Enum: []string{"start", "stop"}, Desc: "start or stop"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "api", Desc: "raw escape hatch: any TrueNAS API endpoint",
			Usage: "method + path under /api/v2.0, for anything without a first-class verb",
			Options: plugin.Schema{
				"method": {Type: "string", Desc: "HTTP method (default GET)"},
				"path":   {Type: "string", Required: true, Desc: "path under /api/v2.0, e.g. /pool/dataset"},
				"query":  {Type: "map", Desc: "query string parameters"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
	}
}

func (p *truenasPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	switch req.Verb {
	case "system_info":
		return p.systemInfo(conn)
	case "pools":
		return p.pools(conn)
	case "pool_get":
		return p.poolGet(conn, o)
	case "datasets":
		return p.datasets(conn)
	case "dataset_get":
		return p.datasetGet(conn, o)
	case "snapshots":
		return p.snapshots(conn)
	case "snapshot_create":
		return p.snapshotCreate(conn, o)
	case "replication":
		return p.replication(conn)
	case "apps":
		return p.apps(conn)
	case "alerts":
		return p.alerts(conn)
	case "alert_dismiss":
		return p.alertDismiss(conn, o)
	case "services":
		return p.services(conn)
	case "service_control":
		return p.serviceControl(conn, o)
	case "api":
		return p.api(conn, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// --- connection ---

type truenasConn struct {
	baseURL            string
	apiKey             string
	insecureSkipVerify bool
	pollInterval       time.Duration
}

func parseConn(m map[string]any) (truenasConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return truenasConn{}, fmt.Errorf("base_url is required")
	}
	apiKey := str(m["api_key"])
	if apiKey == "" {
		return truenasConn{}, fmt.Errorf("api_key is required")
	}
	c := truenasConn{
		baseURL:            strings.TrimRight(base, "/"),
		apiKey:             apiKey,
		insecureSkipVerify: boolv(m["insecure_skip_verify"]),
		pollInterval:       time.Minute,
	}
	if d, err := toDuration(m["poll_interval"]); err != nil {
		return truenasConn{}, fmt.Errorf("poll_interval: %w", err)
	} else if d > 0 {
		c.pollInterval = d
	}
	return c, nil
}

// apiBase is conn.base_url + "/api/v2.0" — every first-class verb's endpoint
// is relative to this. Overridable simply by pointing base_url at a
// httptest.Server in tests.
func (c truenasConn) apiBase() string { return c.baseURL + "/api/v2.0" }

// --- verb implementations ---

func (p *truenasPlugin) systemInfo(conn truenasConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/system/info", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *truenasPlugin) pools(conn truenasConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/pool", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *truenasPlugin) poolGet(conn truenasConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["pool_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "pool_id is required")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/pool/id/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *truenasPlugin) datasets(conn truenasConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/pool/dataset", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

// datasetGet URL-encodes the dataset id: TrueNAS dataset ids are the ZFS
// path (e.g. "tank/data"), and the "/" must be escaped to %2F for the
// middleware's /pool/dataset/id/{id} route.
func (p *truenasPlugin) datasetGet(conn truenasConn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["dataset_id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "dataset_id is required")
	}
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/pool/dataset/id/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": decoded, "status_code": status}}, nil
}

func (p *truenasPlugin) snapshots(conn truenasConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/zfs/snapshot", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *truenasPlugin) snapshotCreate(conn truenasConn, o map[string]any) (plugin.InvokeResult, error) {
	dataset := str(o["dataset"])
	if dataset == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "dataset is required")
	}
	name := str(o["name"])
	if name == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "name is required")
	}
	payload := map[string]any{"dataset": dataset, "name": name}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/zfs/snapshot", nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *truenasPlugin) replication(conn truenasConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/replication", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *truenasPlugin) apps(conn truenasConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/app", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *truenasPlugin) alerts(conn truenasConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/alert/list", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

// alertDismiss posts the alert's uuid as a bare JSON string body (not an
// object) — that is what /alert/dismiss expects.
func (p *truenasPlugin) alertDismiss(conn truenasConn, o map[string]any) (plugin.InvokeResult, error) {
	uuid := str(o["uuid"])
	if uuid == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "uuid is required")
	}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/alert/dismiss", nil, uuid)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *truenasPlugin) services(conn truenasConn) (plugin.InvokeResult, error) {
	status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/service", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": hoist(decoded), "status_code": status}}, nil
}

func (p *truenasPlugin) serviceControl(conn truenasConn, o map[string]any) (plugin.InvokeResult, error) {
	service := str(o["service"])
	if service == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "service is required")
	}
	action := str(o["action"])
	if action != "start" && action != "stop" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "action must be start or stop")
	}
	payload := map[string]any{"service": service}
	status, body, err := p.do(conn, http.MethodPost, conn.apiBase()+"/service/"+action, nil, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	if decoded, derr := decodeJSON(body); derr == nil && decoded != nil {
		out["result"] = decoded
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func (p *truenasPlugin) api(conn truenasConn, o map[string]any) (plugin.InvokeResult, error) {
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
	status, body, err := p.do(conn, method, conn.apiBase()+path, q, o["body"])
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	out := map[string]any{"status_code": status}
	decoded, derr := decodeJSON(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
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

// --- HTTP plumbing ---

// do performs one HTTP request against the TrueNAS API, attaching the Bearer
// API key, and returns the status code and raw response body. A non-2xx
// status is translated into a CodeInternalError carrying the status and
// body — the caller never has to check status codes itself.
func (p *truenasPlugin) do(conn truenasConn, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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
	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+conn.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.httpClient(conn).Do(req)
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

// hoist pulls a list out of a decoded JSON value: if it is already a list,
// return it as-is; if it is an object, return the first of the given keys
// that holds a list. Otherwise, an empty list — never nil, so callers get a
// consistent [] rather than null on the wire. Most TrueNAS list endpoints
// return a bare JSON array, so hoist() with no keys is the common case.
func hoist(v any, keys ...string) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		for _, k := range keys {
			if list, ok := x[k].([]any); ok {
				return list
			}
		}
	}
	return []any{}
}

// --- source: poll /alert/list, emit one event per active alert ---

// StartSource polls GET /alert/list every poll_interval and emits an `alert`
// event for each alert that is NOT dismissed. Dedup is on the alert's
// uuid/id, so an alert that stays active for days emits once, not once per
// cycle.
func (p *truenasPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	conn, err := parseConn(req.Config)
	if err != nil {
		return fmt.Errorf("truenas: %w", err)
	}
	dedup := sourcekit.NewDedup(1024)
	fmt.Fprintf(os.Stderr, "truenas[%s]: polling alerts every %s\n", req.Instance, conn.pollInterval)
	for {
		if ctx.Err() != nil {
			return nil
		}
		status, body, err := p.do(conn, http.MethodGet, conn.apiBase()+"/alert/list", nil, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "truenas[%s]: alert/list: %v (status %d)\n", req.Instance, err, status)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			continue
		}
		decoded, derr := decodeJSON(body)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "truenas[%s]: alert/list: %v\n", req.Instance, derr)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			continue
		}
		for _, item := range hoist(decoded) {
			a, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ev, ok := alertEvent(a)
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

// backoff is how long the source waits after a failed poll before retrying,
// so an unreachable TrueNAS box or a revoked API key doesn't become a hot
// loop.
const backoff = 30 * time.Second

// alertEvent is the source's WHOLE decision, kept pure so it is testable
// without a server: given one decoded alert object, it reports whether that
// alert should be emitted (active, i.e. not dismissed, and identifiable) and,
// if so, builds the event.
func alertEvent(a map[string]any) (map[string]any, bool) {
	if boolv(a["dismissed"]) {
		return nil, false
	}
	id := alertID(a)
	if id == "" {
		// No stable identity to dedup or reference by — skip rather than emit
		// something the operator can never dismiss/filter reliably.
		return nil, false
	}
	level := gs(a, "level")
	klass := gs(a, "klass")
	formatted := gs(a, "formatted")
	node := gs(a, "node")
	datetime := alertDatetime(a)

	title := "truenas: " + level + " alert"
	if formatted != "" {
		title += " — " + formatted
	}
	return map[string]any{
		"event": "alert",
		"kind":  "alert",
		"title": title,
		"dedup": id,
		"context": map[string]any{
			"id": id, "uuid": id,
			"level": level, "klass": klass,
			"formatted": formatted, "dismissed": false,
			"datetime": datetime, "node": node,
			// Plural aliases so the documented filter vocabulary
			// (filters: {levels/klasses: [...]}) matches against the daemon's
			// generic list-contains filter evaluator — the same convention the
			// smart connector uses for devices/models.
			"levels": level, "klasses": klass,
		},
	}, true
}

// alertID prefers "uuid" (TrueNAS's stable alert identity) and falls back to
// "id" for API variants/mocks that only carry the latter.
func alertID(a map[string]any) string {
	if u := gs(a, "uuid"); u != "" {
		return u
	}
	switch v := a["id"].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

// alertDatetime renders the alert's datetime field as a string: TrueNAS's
// middleware often wraps timestamps as {"$date": <epoch_ms>}; a plain string
// (or anything else) is passed through/stringified.
func alertDatetime(a map[string]any) string {
	v, ok := a["datetime"]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if d, ok := x["$date"]; ok {
			return fmt.Sprintf("%v", d)
		}
	}
	return fmt.Sprintf("%v", v)
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
	if err := plugin.Serve(newTruenasPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-truenas:", err)
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

func gs(m map[string]any, k string) string { s, _ := m[k].(string); return s }

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
