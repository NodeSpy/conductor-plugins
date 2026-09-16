// Command conductor-cloudflare is the Cloudflare connector as a standalone
// external conductor plugin (#59). It drives the Cloudflare API v4 for DNS
// records, zones, cache purges, and Workers scripts, plus a generic `api`
// escape hatch for any endpoint without a first-class verb. It is verb-only —
// no source events.
//
// Built ONLY against the public SDK (pkg/plugin) — no conductor internals, no
// third-party dependencies.
//
// Connection:
//
//	api_token:  "<token>"     # preferred: sent as Authorization: Bearer <token>
//	api_key:    "<key>"       # legacy fallback (paired with email): X-Auth-Key
//	email:      "<email>"     # legacy fallback (paired with api_key): X-Auth-Email
//	account_id: "<account>"   # default account for account-scoped verbs (worker_deploy)
//	zone_id:    "<zone>"      # default zone for zone-scoped verbs (dns_*, cache_purge, …)
//	api_base:   "https://..." # override the API base URL (tests only; default
//	                          # https://api.cloudflare.com/client/v4)
//
// api_token, when set, wins over api_key/email — see authHeaders.
//
// Every verb calls the standard Cloudflare v4 envelope
// ({success, errors, result}): a non-2xx response, or a 2xx response with
// success:false, becomes a CodeInternalError naming the Cloudflare error(s)
// and the raw response body; otherwise the envelope's result is returned as
// outputs.result.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// defaultAPIBase is the real Cloudflare API v4 base. Connection.api_base
// overrides it — the only reason to do so is to point doHTTP at an
// httptest.Server in a test.
const defaultAPIBase = "https://api.cloudflare.com/client/v4"

type cloudflarePlugin struct{}

func (cloudflarePlugin) Describe() plugin.Decl {
	objOut := plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}}
	zoneOpt := plugin.Field{Type: "string", Scope: "zone", Desc: "zone id (default: connection.zone_id)"}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "cloudflare",
		Desc: "Cloudflare API v4: DNS records, zones, cache purges, and Workers scripts as verbs, plus a generic api escape hatch.",
		Connection: plugin.Schema{
			"api_token":  {Type: "string", Desc: "API token; sent as Authorization: Bearer <token> (preferred over api_key/email)"},
			"api_key":    {Type: "string", Desc: "legacy Global API Key; paired with email, sent as X-Auth-Key (fallback when api_token is unset)"},
			"email":      {Type: "string", Desc: "account email; paired with api_key, sent as X-Auth-Email"},
			"account_id": {Type: "string", Desc: "default account id for account-scoped verbs (worker_deploy)"},
			"zone_id":    {Type: "string", Desc: "default zone id for zone-scoped verbs (dns_*, cache_purge, zone_get, …)"},
			"api_base":   {Type: "string", Desc: "override the Cloudflare API base URL (tests only; default " + defaultAPIBase + ")"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "dns_list", Desc: "list DNS records in a zone",
				Options: plugin.Schema{
					"zone": zoneOpt,
					"type": {Type: "string", Desc: "filter by record type, e.g. A, CNAME, TXT"},
					"name": {Type: "string", Desc: "filter by exact record name"},
				},
				Outputs: objOut,
			},
			{
				Name: "dns_create", Desc: "create a DNS record",
				Options: plugin.Schema{
					"zone":     zoneOpt,
					"type":     {Type: "string", Required: true, Desc: "record type, e.g. A, AAAA, CNAME, TXT, MX"},
					"name":     {Type: "string", Required: true},
					"content":  {Type: "string", Required: true},
					"ttl":      {Type: "integer", Desc: "TTL in seconds (1 = automatic)"},
					"proxied":  {Type: "boolean", Desc: "proxy through Cloudflare (orange-cloud)"},
					"priority": {Type: "integer", Desc: "MX/SRV priority"},
				},
				Outputs: objOut,
			},
			{
				Name: "dns_update", Desc: "update a DNS record",
				Options: plugin.Schema{
					"zone":      zoneOpt,
					"record_id": {Type: "string", Required: true},
					"type":      {Type: "string"},
					"name":      {Type: "string"},
					"content":   {Type: "string"},
					"ttl":       {Type: "integer"},
					"proxied":   {Type: "boolean"},
				},
				Outputs: objOut,
			},
			{
				Name: "dns_delete", Desc: "delete a DNS record",
				Options: plugin.Schema{
					"zone":      zoneOpt,
					"record_id": {Type: "string", Required: true},
				},
				Outputs: objOut,
			},
			{
				Name: "cache_purge", Desc: "purge the zone's cache: everything, or specific files/tags/hosts",
				Options: plugin.Schema{
					"zone":       zoneOpt,
					"everything": {Type: "boolean", Desc: "purge the entire zone cache (mutually exclusive with files/tags/hosts)"},
					"files":      {Type: "list", Desc: "exact URLs to purge"},
					"tags":       {Type: "list", Desc: "cache-tags to purge (Enterprise only)"},
					"hosts":      {Type: "list", Desc: "hostnames to purge"},
				},
				Outputs: objOut,
			},
			{
				Name: "zone_list", Desc: "list zones on the account",
				Options: plugin.Schema{
					"name":   {Type: "string", Desc: "filter by exact zone name"},
					"status": {Type: "string", Desc: "filter by zone status, e.g. active"},
				},
				Outputs: objOut,
			},
			{
				Name: "zone_get", Desc: "read a zone by id",
				Options: plugin.Schema{"zone": zoneOpt},
				Outputs: objOut,
			},
			{
				Name: "worker_deploy", Desc: "deploy (create or update) a Workers script",
				Options: plugin.Schema{
					"account":     {Type: "string", Scope: "account", Desc: "account id (default: connection.account_id)"},
					"name":        {Type: "string", Required: true, Desc: "script name"},
					"script":      {Type: "string", Required: true, Desc: "the worker's JS source"},
					"main_module": {Type: "string", Desc: "when set, deploys as an ES module worker (Content-Type application/javascript+module) instead of a classic service-worker script"},
				},
				Outputs: objOut,
			},
			{
				Name: "ruleset_list", Desc: "list rulesets configured on a zone",
				Options: plugin.Schema{"zone": zoneOpt},
				Outputs: objOut,
			},
			{
				Name: "firewall_rules_list", Desc: "list legacy firewall rules configured on a zone",
				Options: plugin.Schema{"zone": zoneOpt},
				Outputs: objOut,
			},
			{
				Name: "api", Desc: "call any Cloudflare v4 API endpoint not covered by a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Required: true, Enum: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
					"path":   {Type: "string", Required: true, Desc: "path under /client/v4, e.g. /zones/{zone}/dns_records"},
					"query":  {Type: "map"},
					"body":   {Type: "any"},
				},
				Outputs: objOut,
			},
		},
		// It only ever calls the Cloudflare API; it spawns nothing.
		Capabilities: plugin.Capabilities{Egress: []string{"api.cloudflare.com:443"}},
	}
}

func (cloudflarePlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	if conn == nil {
		conn = map[string]any{}
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	rb, err := buildRequest(req.Verb, o, conn)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}

	status, raw, err := doHTTP(context.Background(), conn, rb)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}

	result, success, isEnvelope, errMsg := parseCF(raw)
	if status < 200 || status >= 300 || (isEnvelope && !success) {
		msg := fmt.Sprintf("%s: %s %s: status %d", req.Verb, rb.method, rb.path, status)
		if errMsg != "" {
			msg += ": " + errMsg
		}
		if body := strings.TrimSpace(string(raw)); body != "" {
			msg += ": " + body
		}
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, msg)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"status_code": status, "result": result}}, nil
}

// --- request building (pure, hermetically testable — no network) -------

// reqBuild is what one verb call resolves to: an HTTP method, a path under
// the API base, optional query parameters, and either a JSON body or a raw
// body with its own content type (worker_deploy's script upload).
type reqBuild struct {
	method      string
	path        string
	query       url.Values
	body        any
	rawBody     []byte
	contentType string
}

func buildRequest(verb string, o, conn map[string]any) (reqBuild, error) {
	switch verb {
	case "dns_list":
		zone, err := resolveZone(o, conn)
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{}
		if t := str(o["type"]); t != "" {
			q.Set("type", t)
		}
		if n := str(o["name"]); n != "" {
			q.Set("name", n)
		}
		return reqBuild{method: http.MethodGet, path: fmt.Sprintf("/zones/%s/dns_records", zone), query: q}, nil

	case "dns_create":
		zone, err := resolveZone(o, conn)
		if err != nil {
			return reqBuild{}, err
		}
		typ, err := requiredStr(o, "type")
		if err != nil {
			return reqBuild{}, err
		}
		name, err := requiredStr(o, "name")
		if err != nil {
			return reqBuild{}, err
		}
		content, err := requiredStr(o, "content")
		if err != nil {
			return reqBuild{}, err
		}
		b := map[string]any{"type": typ, "name": name, "content": content}
		if v, ok := o["ttl"]; ok && v != nil {
			b["ttl"] = toInt64(v)
		}
		if _, ok := o["proxied"]; ok {
			b["proxied"] = boolv(o["proxied"])
		}
		if v, ok := o["priority"]; ok && v != nil {
			b["priority"] = toInt64(v)
		}
		return reqBuild{method: http.MethodPost, path: fmt.Sprintf("/zones/%s/dns_records", zone), body: b}, nil

	case "dns_update":
		zone, err := resolveZone(o, conn)
		if err != nil {
			return reqBuild{}, err
		}
		recordID, err := requiredStr(o, "record_id")
		if err != nil {
			return reqBuild{}, err
		}
		b := map[string]any{}
		if v := str(o["type"]); v != "" {
			b["type"] = v
		}
		if v := str(o["name"]); v != "" {
			b["name"] = v
		}
		if v := str(o["content"]); v != "" {
			b["content"] = v
		}
		if v, ok := o["ttl"]; ok && v != nil {
			b["ttl"] = toInt64(v)
		}
		if _, ok := o["proxied"]; ok {
			b["proxied"] = boolv(o["proxied"])
		}
		return reqBuild{method: http.MethodPut, path: fmt.Sprintf("/zones/%s/dns_records/%s", zone, recordID), body: b}, nil

	case "dns_delete":
		zone, err := resolveZone(o, conn)
		if err != nil {
			return reqBuild{}, err
		}
		recordID, err := requiredStr(o, "record_id")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodDelete, path: fmt.Sprintf("/zones/%s/dns_records/%s", zone, recordID)}, nil

	case "cache_purge":
		zone, err := resolveZone(o, conn)
		if err != nil {
			return reqBuild{}, err
		}
		b := map[string]any{}
		if boolv(o["everything"]) {
			b["purge_everything"] = true
		} else {
			if files := strList(o["files"]); len(files) > 0 {
				b["files"] = files
			}
			if tags := strList(o["tags"]); len(tags) > 0 {
				b["tags"] = tags
			}
			if hosts := strList(o["hosts"]); len(hosts) > 0 {
				b["hosts"] = hosts
			}
			if len(b) == 0 {
				return reqBuild{}, fmt.Errorf("one of everything, files, tags, or hosts is required")
			}
		}
		return reqBuild{method: http.MethodPost, path: fmt.Sprintf("/zones/%s/purge_cache", zone), body: b}, nil

	case "zone_list":
		q := url.Values{}
		if n := str(o["name"]); n != "" {
			q.Set("name", n)
		}
		if s := str(o["status"]); s != "" {
			q.Set("status", s)
		}
		return reqBuild{method: http.MethodGet, path: "/zones", query: q}, nil

	case "zone_get":
		zone, err := resolveZone(o, conn)
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: fmt.Sprintf("/zones/%s", zone)}, nil

	case "worker_deploy":
		account, err := resolveAccount(o, conn)
		if err != nil {
			return reqBuild{}, err
		}
		name, err := requiredStr(o, "name")
		if err != nil {
			return reqBuild{}, err
		}
		script, err := requiredStr(o, "script")
		if err != nil {
			return reqBuild{}, err
		}
		ct := "application/javascript"
		if str(o["main_module"]) != "" {
			ct = "application/javascript+module"
		}
		return reqBuild{
			method: http.MethodPut, path: fmt.Sprintf("/accounts/%s/workers/scripts/%s", account, name),
			rawBody: []byte(script), contentType: ct,
		}, nil

	case "ruleset_list":
		zone, err := resolveZone(o, conn)
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: fmt.Sprintf("/zones/%s/rulesets", zone)}, nil

	case "firewall_rules_list":
		zone, err := resolveZone(o, conn)
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: fmt.Sprintf("/zones/%s/firewall/rules", zone)}, nil

	case "api":
		method, err := requiredStr(o, "method")
		if err != nil {
			return reqBuild{}, err
		}
		path, err := requiredStr(o, "path")
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{}
		if m, ok := o["query"].(map[string]any); ok {
			for k, v := range m {
				q.Set(k, fmt.Sprintf("%v", v))
			}
		}
		return reqBuild{method: strings.ToUpper(method), path: "/" + strings.TrimPrefix(path, "/"), query: q, body: o["body"]}, nil
	}
	return reqBuild{}, fmt.Errorf("unknown verb %q", verb)
}

// resolveZone resolves the zone-scoped verbs' "zone" option, falling back to
// connection.zone_id so an operator who pins one zone per connector instance
// need not repeat it on every call.
func resolveZone(o, conn map[string]any) (string, error) {
	z := strOr(o["zone"], str(conn["zone_id"]))
	if z == "" {
		return "", fmt.Errorf("zone is required (option zone, or connection.zone_id)")
	}
	return z, nil
}

// resolveAccount is resolveZone's account-scoped counterpart, for
// worker_deploy.
func resolveAccount(o, conn map[string]any) (string, error) {
	a := strOr(o["account"], str(conn["account_id"]))
	if a == "" {
		return "", fmt.Errorf("account is required (option account, or connection.account_id)")
	}
	return a, nil
}

// --- HTTP transport ------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

// authHeaders picks the auth scheme: api_token (Bearer) when set, else the
// legacy api_key + email pair (X-Auth-Key / X-Auth-Email). api_token wins
// when both are configured.
func authHeaders(conn map[string]any) map[string]string {
	if token := str(conn["api_token"]); token != "" {
		return map[string]string{"Authorization": "Bearer " + token}
	}
	if key, email := str(conn["api_key"]), str(conn["email"]); key != "" && email != "" {
		return map[string]string{"X-Auth-Key": key, "X-Auth-Email": email}
	}
	return nil
}

// doHTTP sends one built request against the (possibly overridden) API base
// and returns the raw response body for parseCF to interpret.
func doHTTP(ctx context.Context, conn map[string]any, rb reqBuild) (status int, raw []byte, err error) {
	base := strOr(conn["api_base"], defaultAPIBase)
	full := strings.TrimRight(base, "/") + rb.path
	if len(rb.query) > 0 {
		full += "?" + rb.query.Encode()
	}

	var bodyReader io.Reader
	contentType := rb.contentType
	switch {
	case rb.rawBody != nil:
		bodyReader = bytes.NewReader(rb.rawBody)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	case rb.body != nil:
		b, merr := json.Marshal(rb.body)
		if merr != nil {
			return 0, nil, merr
		}
		bodyReader = bytes.NewReader(b)
		contentType = "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, rb.method, full, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range authHeaders(conn) {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

// parseCF interprets a Cloudflare API response body. When the body is a JSON
// object carrying a "success" key (the standard v4 envelope), it unwraps
// "result" and reports success/errors from the envelope. Otherwise (the `api`
// escape hatch hitting a non-standard response, or an empty/non-JSON body) it
// passes the parsed value straight through as the result, with isEnvelope
// false so the caller falls back to the HTTP status code alone.
func parseCF(raw []byte) (result any, success, isEnvelope bool, errMsg string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, true, false, ""
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return string(raw), true, false, ""
	}
	if m, ok := v.(map[string]any); ok {
		if s, has := m["success"]; has {
			ok, _ := s.(bool)
			return m["result"], ok, true, formatCFErrors(m["errors"])
		}
	}
	return v, true, false, ""
}

// formatCFErrors renders a Cloudflare envelope's "errors" array — each entry
// is {code, message} — into one readable string.
func formatCFErrors(v any) string {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	parts := make([]string, 0, len(arr))
	for _, e := range arr {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		msg, _ := em["message"].(string)
		parts = append(parts, fmt.Sprintf("[%v] %s", em["code"], msg))
	}
	return strings.Join(parts, "; ")
}

func main() {
	if err := plugin.Serve(cloudflarePlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-cloudflare:", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) --------------------------------------

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
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// strList accepts a string, []string, or []any and returns a non-empty slice.
func strList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s := fmt.Sprintf("%v", e); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func requiredStr(o map[string]any, key string) (string, error) {
	v := str(o[key])
	if v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}
