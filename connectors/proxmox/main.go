// Command conductor-proxmox is a conductor connector (#59) for Proxmox VE, a
// homelab hypervisor. It drives the PVE REST API as verbs — nodes, cluster
// resources, QEMU VMs (list/status/start/stop/shutdown/reboot/clone), LXC
// containers (list/status/start/stop), storage, tasks, backups (vzdump),
// snapshots, and a raw `api` escape hatch — and as a POLL SOURCE it watches
// /cluster/tasks and emits a `task` event whenever a task finishes (a
// backup, migration, clone, ...), so a workflow can trigger on the outcome
// of one. Built ONLY against the public SDK (pkg/plugin, pkg/sourcekit) — no
// conductor internals, no third-party dependencies.
//
// Every verb is a plain net/http call to {base_url}/api2/json/<path>,
// authenticated with the `Authorization: PVEAPIToken=<token_id>=<token_secret>`
// header (see https://pve.proxmox.com/wiki/Proxmox_VE_API#API_Tokens). Proxmox
// wraps every response as {"data": ...} — this connector unwraps `data`
// before handing it back as `result` (or `items`, for list endpoints).
//
// Connection:
//
//	base_url:             "https://pve.example.com:8006" # required; the PVE API root
//	token_id:              "USER@REALM!TOKENID"           # required
//	token_secret:          "<uuid>"                       # required
//	insecure_skip_verify:  false                          # skip TLS verification (self-signed certs)
//	poll_interval:         "30s"                           # source poll period (default 30s)
//
// Proxmox is always self-hosted, so this plugin declares NO egress — the
// operator narrows `network:` on the connector instance to their own PVE
// host (see docs/connectors/proxmox.md).
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

type proxmoxPlugin struct{}

func (proxmoxPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "proxmox",
		Desc: "Proxmox VE: nodes, cluster resources, QEMU VMs, LXC containers, storage, tasks, backups (vzdump), snapshots, and a raw api escape hatch over the PVE REST API — plus a poll source that emits a task event when a cluster task finishes. Self-hosted; declares no egress (narrow with network: per instance).",
		Connection: plugin.Schema{
			"base_url":     {Type: "string", Required: true, Desc: "Proxmox VE API root, e.g. https://pve.example.com:8006"},
			"token_id":     {Type: "string", Required: true, Desc: `API token ID, "USER@REALM!TOKENID" (Datacenter > Permissions > API Tokens)`},
			"token_secret": {Type: "string", Required: true, Desc: "API token secret — the UUID shown once at token creation"},
			"insecure_skip_verify": {
				Type: "boolean",
				Desc: "skip TLS certificate verification. Proxmox commonly uses self-signed certs, but this disables verification ENTIRELY (a man-in-the-middle can impersonate your PVE host undetected) — only set true when base_url is reachable exclusively over a network you trust (LAN/VPN), and prefer installing a real certificate (PVE has built-in ACME support) instead where possible",
			},
			"poll_interval": {Type: "duration", Desc: "source poll period (default 30s)"},
		},
		Verbs:  proxmoxVerbs(),
		Events: []plugin.Event{proxmoxTaskEvent()},
		// Proxmox is self-hosted: there is no fixed public host to declare.
		// The operator narrows egress to their own instance with
		// `network: ["pve.example.com:8006"]` on the connector instance.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

// --- verb schema ---

var (
	resultOut = plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}}
	itemsOut  = plugin.Schema{"status_code": {Type: "integer"}, "items": {Type: "list"}}

	nodeField = plugin.Field{Type: "string", Required: true, Scope: "node", Desc: "Proxmox node name, e.g. pve1"}
	vmidField = plugin.Field{Type: "integer", Required: true, Scope: "vm", Desc: "VM/container ID"}
	upidField = plugin.Field{Type: "string", Required: true, Scope: "task", Desc: "task UPID, e.g. from tasks or the *_start/backup outputs"}
)

// vmActionVerb is the shape every start/stop/shutdown/reboot verb shares:
// node + vmid in, the raw task result out (POST returns the started task's
// UPID as `data`).
func vmActionVerb(name, desc string) plugin.Verb {
	return plugin.Verb{
		Name:    name,
		Desc:    desc,
		Options: plugin.Schema{"node": nodeField, "vmid": vmidField},
		Outputs: resultOut,
	}
}

func proxmoxVerbs() []plugin.Verb {
	return []plugin.Verb{
		{
			Name: "nodes", Desc: "list cluster nodes",
			Usage:   "GET /nodes",
			Outputs: itemsOut,
		},
		{
			Name: "node_status", Desc: "a node's status (uptime, load, memory, ...)",
			Usage:   "GET /nodes/{node}/status",
			Options: plugin.Schema{"node": nodeField},
			Outputs: resultOut,
		},
		{
			Name: "cluster_resources", Desc: "cluster-wide resource list (VMs, nodes, storage)",
			Usage: "GET /cluster/resources",
			Options: plugin.Schema{
				"type": {Type: "string", Enum: []string{"vm", "node", "storage"}, Desc: "restrict to one resource type"},
			},
			Outputs: itemsOut,
		},
		{
			Name: "qemu_list", Desc: "list QEMU VMs on a node",
			Usage:   "GET /nodes/{node}/qemu",
			Options: plugin.Schema{"node": nodeField},
			Outputs: itemsOut,
		},
		{
			Name: "qemu_status", Desc: "a QEMU VM's current status",
			Usage:   "GET /nodes/{node}/qemu/{vmid}/status/current",
			Options: plugin.Schema{"node": nodeField, "vmid": vmidField},
			Outputs: resultOut,
		},
		vmActionVerb("qemu_start", "start a QEMU VM (POST /nodes/{node}/qemu/{vmid}/status/start)"),
		vmActionVerb("qemu_stop", "hard-stop a QEMU VM (POST /nodes/{node}/qemu/{vmid}/status/stop)"),
		vmActionVerb("qemu_shutdown", "gracefully shut down a QEMU VM via ACPI (POST /nodes/{node}/qemu/{vmid}/status/shutdown)"),
		vmActionVerb("qemu_reboot", "reboot a QEMU VM (POST /nodes/{node}/qemu/{vmid}/status/reboot)"),
		{
			Name: "qemu_clone", Desc: "clone a QEMU VM/template",
			Usage: "POST /nodes/{node}/qemu/{vmid}/clone",
			Options: plugin.Schema{
				"node":  nodeField,
				"vmid":  vmidField,
				"newid": {Type: "integer", Required: true, Desc: "VMID for the clone"},
				"name":  {Type: "string", Desc: "name for the clone"},
				"full":  {Type: "boolean", Desc: "full clone instead of a linked clone"},
			},
			Outputs: resultOut,
		},
		{
			Name: "lxc_list", Desc: "list LXC containers on a node",
			Usage:   "GET /nodes/{node}/lxc",
			Options: plugin.Schema{"node": nodeField},
			Outputs: itemsOut,
		},
		{
			Name: "lxc_status", Desc: "an LXC container's current status",
			Usage:   "GET /nodes/{node}/lxc/{vmid}/status/current",
			Options: plugin.Schema{"node": nodeField, "vmid": vmidField},
			Outputs: resultOut,
		},
		vmActionVerb("lxc_start", "start an LXC container (POST /nodes/{node}/lxc/{vmid}/status/start)"),
		vmActionVerb("lxc_stop", "hard-stop an LXC container (POST /nodes/{node}/lxc/{vmid}/status/stop)"),
		{
			Name: "storage", Desc: "storage configured on a node",
			Usage:   "GET /nodes/{node}/storage",
			Options: plugin.Schema{"node": nodeField},
			Outputs: itemsOut,
		},
		{
			Name: "tasks", Desc: "recent tasks on a node",
			Usage:   "GET /nodes/{node}/tasks",
			Options: plugin.Schema{"node": nodeField},
			Outputs: itemsOut,
		},
		{
			Name: "task_status", Desc: "a task's status by UPID",
			Usage:   "GET /nodes/{node}/tasks/{upid}/status",
			Options: plugin.Schema{"node": nodeField, "upid": upidField},
			Outputs: resultOut,
		},
		{
			Name: "backup", Desc: "start a vzdump backup job",
			Usage: "POST /nodes/{node}/vzdump",
			Options: plugin.Schema{
				"node":     nodeField,
				"vmid":     vmidField,
				"storage":  {Type: "string", Scope: "storage", Desc: "target storage ID"},
				"mode":     {Type: "string", Enum: []string{"snapshot", "suspend", "stop"}, Desc: "backup mode (default snapshot)"},
				"compress": {Type: "string", Desc: "compression: 0, 1, gzip, lzo, or zstd"},
			},
			Outputs: resultOut,
		},
		{
			Name: "snapshots", Desc: "list a QEMU VM's snapshots",
			Usage:   "GET /nodes/{node}/qemu/{vmid}/snapshot",
			Options: plugin.Schema{"node": nodeField, "vmid": vmidField},
			Outputs: itemsOut,
		},
		{
			Name: "snapshot_create", Desc: "create a QEMU VM snapshot",
			Usage: "POST /nodes/{node}/qemu/{vmid}/snapshot",
			Options: plugin.Schema{
				"node":     nodeField,
				"vmid":     vmidField,
				"snapname": {Type: "string", Required: true, Desc: "snapshot name"},
			},
			Outputs: resultOut,
		},
		{
			Name: "api", Desc: "raw escape hatch: any Proxmox API endpoint",
			Usage: "method + path under /api2/json, for anything without a first-class verb",
			Options: plugin.Schema{
				"method": {Type: "string", Desc: "HTTP method (default GET)"},
				"path":   {Type: "string", Required: true, Desc: "path under /api2/json, e.g. /nodes/pve1/qemu/100/config"},
				"query":  {Type: "map", Desc: "query string parameters"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "status_code": {Type: "integer"}},
		},
	}
}

func proxmoxTaskEvent() plugin.Event {
	return plugin.Event{
		Name: "task",
		Desc: "a Proxmox cluster task finished (stopped) — e.g. a backup, migration, or clone completed or failed",
		Filters: plugin.Schema{
			"nodes":    {Type: "list", Desc: "match if the task's node is one of these"},
			"types":    {Type: "list", Desc: "match if the task's type (e.g. vzdump, qmclone, qmigrate) is one of these"},
			"statuses": {Type: "list", Desc: "match if the task's exit status (e.g. OK) is one of these"},
		},
		Context: plugin.Schema{
			"node":       {Type: "string"},
			"upid":       {Type: "string"},
			"type":       {Type: "string"},
			"id":         {Type: "string"},
			"user":       {Type: "string"},
			"status":     {Type: "string"},
			"exitstatus": {Type: "string"},
			"starttime":  {Type: "integer"},
			"endtime":    {Type: "integer"},
		},
	}
}

// --- connection ---

type pveConn struct {
	baseURL            string
	tokenID            string
	tokenSecret        string
	insecureSkipVerify bool
	pollInterval       time.Duration
}

func parseConn(m map[string]any) (pveConn, error) {
	base := str(m["base_url"])
	if base == "" {
		return pveConn{}, fmt.Errorf("base_url is required")
	}
	tokenID := str(m["token_id"])
	if tokenID == "" {
		return pveConn{}, fmt.Errorf("token_id is required")
	}
	tokenSecret := str(m["token_secret"])
	if tokenSecret == "" {
		return pveConn{}, fmt.Errorf("token_secret is required")
	}
	c := pveConn{
		baseURL:            strings.TrimRight(base, "/"),
		tokenID:            tokenID,
		tokenSecret:        tokenSecret,
		insecureSkipVerify: boolv(m["insecure_skip_verify"]),
		pollInterval:       30 * time.Second,
	}
	if d, err := toDuration(m["poll_interval"]); err != nil {
		return c, fmt.Errorf("connection.poll_interval: %w", err)
	} else if d > 0 {
		c.pollInterval = d
	}
	return c, nil
}

// apiBase is conn.base_url + "/api2/json" — every first-class verb's endpoint
// is relative to this. Overridable simply by pointing base_url at an
// httptest.Server in tests.
func (c pveConn) apiBase() string { return c.baseURL + "/api2/json" }

// httpClient builds the client for one request. insecure_skip_verify is
// per-connection (a homelab operator's self-signed cert is common), so the
// TLS config cannot be a package-level singleton.
func (c pveConn) httpClient() *http.Client {
	client := &http.Client{Timeout: 30 * time.Second}
	if c.insecureSkipVerify {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return client
}

// authHeader is the PVE API token header, built once and reused by both the
// verb path and the source poller.
func (c pveConn) authHeader() string { return "PVEAPIToken=" + c.tokenID + "=" + c.tokenSecret }

// --- request building (pure, hermetically testable — no network) ---

// reqBuild is what one verb call resolves to: method, path (relative to
// /api2/json), query, an optional JSON body, and whether the unwrapped `data`
// should land in outputs.items (a list) rather than outputs.result.
type reqBuild struct {
	method string
	path   string
	query  url.Values
	body   any
	asList bool
}

func buildRequest(verb string, o map[string]any) (reqBuild, error) {
	switch verb {
	case "nodes":
		return reqBuild{method: http.MethodGet, path: "/nodes", query: url.Values{}, asList: true}, nil

	case "node_status":
		node, err := requiredStr(o, "node")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/nodes/" + esc(node) + "/status", query: url.Values{}}, nil

	case "cluster_resources":
		q := url.Values{}
		if t := str(o["type"]); t != "" {
			q.Set("type", t)
		}
		return reqBuild{method: http.MethodGet, path: "/cluster/resources", query: q, asList: true}, nil

	case "qemu_list":
		node, err := requiredStr(o, "node")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/nodes/" + esc(node) + "/qemu", query: url.Values{}, asList: true}, nil

	case "qemu_status":
		node, vmid, err := nodeVMID(o)
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/nodes/" + esc(node) + "/qemu/" + vmid + "/status/current", query: url.Values{}}, nil

	case "qemu_start", "qemu_stop", "qemu_shutdown", "qemu_reboot":
		node, vmid, err := nodeVMID(o)
		if err != nil {
			return reqBuild{}, err
		}
		action := strings.TrimPrefix(verb, "qemu_")
		return reqBuild{method: http.MethodPost, path: "/nodes/" + esc(node) + "/qemu/" + vmid + "/status/" + action, query: url.Values{}}, nil

	case "qemu_clone":
		node, vmid, err := nodeVMID(o)
		if err != nil {
			return reqBuild{}, err
		}
		newid, ok := numOrStr(o["newid"])
		if !ok {
			return reqBuild{}, fmt.Errorf("newid is required")
		}
		body := map[string]any{"newid": newid}
		if v := str(o["name"]); v != "" {
			body["name"] = v
		}
		if v, ok := o["full"]; ok {
			body["full"] = proxmoxBool(boolv(v))
		}
		return reqBuild{method: http.MethodPost, path: "/nodes/" + esc(node) + "/qemu/" + vmid + "/clone", query: url.Values{}, body: body}, nil

	case "lxc_list":
		node, err := requiredStr(o, "node")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/nodes/" + esc(node) + "/lxc", query: url.Values{}, asList: true}, nil

	case "lxc_status":
		node, vmid, err := nodeVMID(o)
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/nodes/" + esc(node) + "/lxc/" + vmid + "/status/current", query: url.Values{}}, nil

	case "lxc_start", "lxc_stop":
		node, vmid, err := nodeVMID(o)
		if err != nil {
			return reqBuild{}, err
		}
		action := strings.TrimPrefix(verb, "lxc_")
		return reqBuild{method: http.MethodPost, path: "/nodes/" + esc(node) + "/lxc/" + vmid + "/status/" + action, query: url.Values{}}, nil

	case "storage":
		node, err := requiredStr(o, "node")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/nodes/" + esc(node) + "/storage", query: url.Values{}, asList: true}, nil

	case "tasks":
		node, err := requiredStr(o, "node")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/nodes/" + esc(node) + "/tasks", query: url.Values{}, asList: true}, nil

	case "task_status":
		node, err := requiredStr(o, "node")
		if err != nil {
			return reqBuild{}, err
		}
		upid, err := requiredStr(o, "upid")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/nodes/" + esc(node) + "/tasks/" + esc(upid) + "/status", query: url.Values{}}, nil

	case "backup":
		node, vmid, err := nodeVMID(o)
		if err != nil {
			return reqBuild{}, err
		}
		body := map[string]any{"vmid": vmid}
		if v := str(o["storage"]); v != "" {
			body["storage"] = v
		}
		if v := str(o["mode"]); v != "" {
			body["mode"] = v
		}
		if v := str(o["compress"]); v != "" {
			body["compress"] = v
		}
		return reqBuild{method: http.MethodPost, path: "/nodes/" + esc(node) + "/vzdump", query: url.Values{}, body: body}, nil

	case "snapshots":
		node, vmid, err := nodeVMID(o)
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: "/nodes/" + esc(node) + "/qemu/" + vmid + "/snapshot", query: url.Values{}, asList: true}, nil

	case "snapshot_create":
		node, vmid, err := nodeVMID(o)
		if err != nil {
			return reqBuild{}, err
		}
		snapname, err := requiredStr(o, "snapname")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodPost, path: "/nodes/" + esc(node) + "/qemu/" + vmid + "/snapshot", query: url.Values{}, body: map[string]any{"snapname": snapname}}, nil
	}
	return reqBuild{}, fmt.Errorf("unknown verb %q", verb)
}

// nodeVMID reads the node+vmid pair every per-VM/container verb requires.
func nodeVMID(o map[string]any) (node, vmid string, err error) {
	node, err = requiredStr(o, "node")
	if err != nil {
		return "", "", err
	}
	v, ok := numOrStr(o["vmid"])
	if !ok {
		return "", "", fmt.Errorf("vmid is required")
	}
	return node, v, nil
}

// requiredStr reads o[key] as a non-empty string.
func requiredStr(o map[string]any, key string) (string, error) {
	s := str(o[key])
	if s == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return s, nil
}

// esc path-escapes one path segment (node name, UPID, ...).
func esc(s string) string { return url.PathEscape(s) }

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

// --- Invoke ---

func (p proxmoxPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	var rb reqBuild
	if req.Verb == "api" {
		rb, err = buildAPI(o)
	} else {
		rb, err = buildRequest(req.Verb, o)
	}
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, raw, err := p.do(ctx, conn, rb.method, conn.apiBase()+rb.path, rb.query, rb.body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	data, derr := decodeData(raw)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}

	out := map[string]any{"status_code": status}
	if req.Verb == "api" {
		switch v := data.(type) {
		case []any:
			out["items"] = v
		default:
			if data != nil {
				out["result"] = data
			}
		}
		return plugin.InvokeResult{Outputs: out}, nil
	}
	if rb.asList {
		out["items"] = asItems(data)
	} else if data != nil {
		out["result"] = data
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// --- HTTP plumbing ---

// do performs one HTTP request against the PVE API, attaching the
// PVEAPIToken header, and returns the status code and raw response body. A
// non-2xx status is translated into a CodeInternalError carrying the status
// and body — the caller never has to check status codes itself.
func (proxmoxPlugin) do(ctx context.Context, conn pveConn, method, endpoint string, query url.Values, body any) (int, []byte, error) {
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
	httpReq, err := http.NewRequestWithContext(ctx, method, full, reader)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	httpReq.Header.Set("Authorization", conn.authHeader())
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := conn.httpClient().Do(httpReq)
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
// body decodes to nil rather than an error (e.g. a 200 with an empty body).
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

// hoistData unwraps Proxmox's {"data": ...} envelope. A response that is not
// shaped that way (or has no body) is returned as-is.
func hoistData(v any) any {
	if m, ok := v.(map[string]any); ok {
		if d, ok := m["data"]; ok {
			return d
		}
	}
	return v
}

// decodeData is decodeJSON + hoistData, the shape every verb needs.
func decodeData(raw []byte) (any, error) {
	decoded, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	if decoded == nil {
		return nil, nil
	}
	return hoistData(decoded), nil
}

// asItems normalizes a decoded, unwrapped value into a list: itself when it
// already is one, an empty list otherwise. Never nil.
func asItems(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return []any{}
}

// --- source: poll /cluster/tasks, emit on every finished task ---

// backoff is how long the source waits after a failed poll before retrying,
// so an unreachable PVE host doesn't become a hot loop.
const backoff = 10 * time.Second

// StartSource polls GET /cluster/tasks once per poll_interval and emits a
// `task` event for every task that has STOPPED since the last cycle it was
// seen in (deduped on upid, so a long-running task's later completion is
// still reported exactly once, and a still-running task is never reported at
// all).
func (p proxmoxPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	conn, err := parseConn(req.Config)
	if err != nil {
		return fmt.Errorf("proxmox: %w", err)
	}
	dedup := sourcekit.NewDedup(2048)
	fmt.Fprintf(os.Stderr, "proxmox[%s]: polling /cluster/tasks every %s\n", req.Instance, conn.pollInterval)
	for {
		if ctx.Err() != nil {
			return nil
		}
		tasks, err := p.pollClusterTasks(ctx, conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "proxmox[%s]: poll: %v\n", req.Instance, err)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			continue
		}
		for _, t := range tasks {
			ev, ok := taskEvent(t)
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

// pollClusterTasks fetches and decodes GET /cluster/tasks into its list of
// raw task documents.
func (p proxmoxPlugin) pollClusterTasks(ctx context.Context, conn pveConn) ([]map[string]any, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, raw, err := p.do(cctx, conn, http.MethodGet, conn.apiBase()+"/cluster/tasks", nil, nil)
	if err != nil {
		return nil, err
	}
	data, derr := decodeData(raw)
	if derr != nil {
		return nil, fmt.Errorf("decode /cluster/tasks (status %d): %w", status, derr)
	}
	list, _ := data.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// taskEvent is the source's WHOLE decision, kept pure so it is testable
// without a network call: given one /cluster/tasks entry, it reports whether
// the task has STOPPED and, if so, builds the event.
//
// A task with neither an `endtime` nor a terminal `status` is still running
// and does not emit. Proxmox's task list entries carry the exit outcome
// (e.g. "OK", or an error message) in the `status` field once the task
// finishes; `exitstatus`, when the source also has it (e.g. from
// task_status), is preferred, falling back to `status` so callers always get
// something in both fields.
func taskEvent(doc map[string]any) (map[string]any, bool) {
	if !taskStopped(doc) {
		return nil, false
	}
	upid := gs(doc, "upid")
	if upid == "" {
		// Nothing to identify or dedup this task by — drop it rather than
		// emit an event no trigger could usefully filter or a workflow could
		// correlate.
		return nil, false
	}
	node, typ, id, user := gs(doc, "node"), gs(doc, "type"), gs(doc, "id"), gs(doc, "user")
	status := gs(doc, "status")
	exitstatus := gs(doc, "exitstatus")
	if exitstatus == "" {
		exitstatus = status
	}

	title := fmt.Sprintf("proxmox: %s task on %s finished", typ, node)
	if exitstatus != "" {
		title = fmt.Sprintf("proxmox: %s task on %s finished (%s)", typ, node, exitstatus)
	}

	return map[string]any{
		"event": "task",
		"kind":  "task",
		"title": title,
		"dedup": upid,
		"context": map[string]any{
			"node": node, "upid": upid, "type": typ, "id": id, "user": user,
			"status": status, "exitstatus": exitstatus,
			"starttime": gi(doc, "starttime"), "endtime": gi(doc, "endtime"),
			// Plural aliases so the documented filter vocabulary
			// (filters: {nodes/types/statuses: [...]}) matches against the
			// daemon's generic list-contains filter evaluator — the same
			// convention the smart and sonarr connectors use.
			"nodes": node, "types": typ, "statuses": exitstatus,
		},
	}, true
}

// taskStopped reports whether a /cluster/tasks entry describes a task that
// has finished: it has a positive `endtime`, or a `status` string other than
// "running" (Proxmox omits `status` entirely while a task is in flight).
func taskStopped(doc map[string]any) bool {
	if n, ok := toFloat(doc["endtime"]); ok && n > 0 {
		return true
	}
	if s, ok := doc["status"].(string); ok && s != "" && s != "running" {
		return true
	}
	return false
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
	if err := plugin.Serve(proxmoxPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-proxmox:", err)
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

// proxmoxBool renders a boolean as the 0/1 integer the PVE API expects for
// its boolean-ish parameters (e.g. clone's `full`).
func proxmoxBool(b bool) int {
	if b {
		return 1
	}
	return 0
}

// numOrStr formats an integer-ish option (vmid, newid — arriving as a JSON
// number, a Go int, or already a string) as a trimmed string; ok is false for
// anything empty or unrecognized.
func numOrStr(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return "", false
		}
		return x, true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return strconv.FormatInt(int64(x), 10), true
	}
	return "", false
}

// toFloat reads a JSON-numeric value as a float64, accepting the shapes a
// decoded document or an option can carry.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

// gs reads a string field from a decoded document ("" if absent/wrong type).
func gs(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// gi reads an integer-ish field from a decoded document (0 if absent).
func gi(m map[string]any, k string) int64 {
	f, _ := toFloat(m[k])
	return int64(f)
}

// toDuration parses a duration option: a Go duration string ("30s"), or a
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
