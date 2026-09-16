// Command conductor-notifiarr is a verb-only conductor connector (#59) that
// drives the Notifiarr API over net/http. It exposes Notifiarr's Passthrough
// Discord-notification integration as an ergonomic `passthrough` verb, plus
// an `api` escape hatch for any endpoint a first-class verb does not cover.
// Built ONLY against the public SDK (pkg/plugin) and the standard library —
// no third-party client.
//
// `passthrough` accepts a flat set of ergonomic options (title, message,
// channel_id, color, ...) and assembles Notifiarr's documented nested
// notification/discord request shape itself, so callers never have to hand-
// build the {"notification":{...},"discord":{...}} envelope.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// defaultAPIBase is Notifiarr's REST API origin. Overridable via connection
// `api_base` so tests (and any private gateway) can point this at an
// httptest.Server instead.
const defaultAPIBase = "https://notifiarr.com/api/v1"

type notifiarrPlugin struct{}

func (notifiarrPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "notifiarr",
		Desc: "Notifiarr: send Discord notifications via Notifiarr's Passthrough integration, plus a generic API escape hatch.",
		Connection: plugin.Schema{
			"api_key":  {Type: "string", Required: true, Desc: "Notifiarr API key"},
			"api_base": {Type: "string", Desc: "override the Notifiarr API base URL (tests, or a private gateway)"},
		},
		Verbs:        notifiarrVerbs(),
		Capabilities: plugin.Capabilities{Egress: []string{"notifiarr.com:443"}},
	}
}

func notifiarrVerbs() []plugin.Verb {
	resultOnly := plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}}
	return []plugin.Verb{
		{
			Name: "passthrough", Desc: "send a Discord notification via Notifiarr's Passthrough integration",
			Usage: "post a message/embed to a Discord channel through Notifiarr",
			Options: plugin.Schema{
				"title":      {Type: "string", Required: true, Desc: "embed title / notification name"},
				"message":    {Type: "string", Required: true, Desc: "embed description text"},
				"channel_id": {Type: "string", Required: true, Scope: "channel", Desc: "Discord channel ID to post to"},
				"color":      {Type: "string", Desc: "embed color, hex without '#'"},
				"event":      {Type: "string", Desc: "notification event name"},
				"ping_user":  {Type: "string", Desc: "Discord user ID to ping"},
				"ping_role":  {Type: "string", Desc: "Discord role ID to ping"},
				"icon":       {Type: "string", Desc: "embed icon URL"},
				"image":      {Type: "string", Desc: "embed image URL"},
				"thumbnail":  {Type: "string", Desc: "embed thumbnail URL"},
				"footer":     {Type: "string", Desc: "embed footer text"},
				"fields":     {Type: "list", Desc: "embed fields: a list of {title, text, inline}"},
			},
			Outputs: resultOnly,
		},
		{
			Name: "api", Desc: "call any Notifiarr API endpoint not covered by a first-class verb",
			Usage: "escape hatch: method + path relative to /api/v1, body passed straight through as JSON",
			Options: plugin.Schema{
				"method": {Type: "string", Required: true, Enum: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
				"path":   {Type: "string", Required: true, Desc: "path relative to /api/v1"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: resultOnly,
		},
	}
}

// notifiarrConn is the resolved connection config for one invocation.
type notifiarrConn struct {
	apiKey string
	base   string
}

func parseConn(m map[string]any) (notifiarrConn, error) {
	key := str(m["api_key"])
	if key == "" {
		return notifiarrConn{}, fmt.Errorf("connection.api_key is required")
	}
	return notifiarrConn{
		apiKey: key,
		base:   strOr(m["api_base"], defaultAPIBase),
	}, nil
}

func (notifiarrPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	call, err := verbCall(req.Verb, o, conn.apiKey)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}

	outputs, err := conn.do(call.method, call.path, call.body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

// apiCall is the resolved HTTP request one verb builds, before it is sent.
type apiCall struct {
	method string
	path   string // relative to the API base, no leading slash
	body   any
}

// verbCall builds the HTTP call for one verb. Pure and hermetically
// testable — no request is sent here.
func verbCall(verb string, o map[string]any, apiKey string) (apiCall, error) {
	switch verb {
	case "passthrough":
		return passthroughCall(o, apiKey)
	case "api":
		return apiVerbCall(o)
	}
	return apiCall{}, fmt.Errorf("unknown verb")
}

// passthroughCall assembles Notifiarr's documented Passthrough Discord
// notification shape from the ergonomic flat options.
func passthroughCall(o map[string]any, apiKey string) (apiCall, error) {
	title := str(o["title"])
	if title == "" {
		return apiCall{}, fmt.Errorf("title is required")
	}
	message := str(o["message"])
	if message == "" {
		return apiCall{}, fmt.Errorf("message is required")
	}
	channelID := str(o["channel_id"])
	if channelID == "" {
		return apiCall{}, fmt.Errorf("channel_id is required")
	}

	body := map[string]any{
		"notification": map[string]any{
			"update": false,
			"name":   strOr(o["title"], "Conductor"),
			"event":  str(o["event"]),
		},
		"discord": map[string]any{
			"color": str(o["color"]),
			"ping": map[string]any{
				"pingUser": o["ping_user"],
				"pingRole": o["ping_role"],
			},
			"images": map[string]any{
				"thumbnail": o["thumbnail"],
				"image":     o["image"],
			},
			"text": map[string]any{
				"title":       title,
				"icon":        o["icon"],
				"content":     "",
				"description": message,
				"fields":      o["fields"],
				"footer":      o["footer"],
			},
			"ids": map[string]any{
				"channel": channelID,
			},
		},
	}
	return apiCall{method: http.MethodPost, path: "notification/passthrough/" + apiKey, body: body}, nil
}

func apiVerbCall(o map[string]any) (apiCall, error) {
	method := strings.ToUpper(str(o["method"]))
	if method == "" {
		return apiCall{}, fmt.Errorf("method is required")
	}
	path := strings.TrimPrefix(str(o["path"]), "/")
	if path == "" {
		return apiCall{}, fmt.Errorf("path is required")
	}
	return apiCall{method: method, path: path, body: o["body"]}, nil
}

// do sends one HTTP request to the Notifiarr API and decodes the JSON
// response into outputs. A non-2xx response is a plugin.CodeInternalError
// carrying the status and response body; verb callers never see a raw
// *http.Response.
func (c notifiarrConn) do(method, path string, body any) (map[string]any, error) {
	url := strings.TrimRight(c.base, "/") + "/" + path

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "encoding request body: "+err.Error())
		}
		reader = bytes.NewReader(raw)
	}

	httpReq, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if reader != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, "reading response: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("notifiarr API %s: %s", resp.Status, strings.TrimSpace(string(raw))))
	}

	var result any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, plugin.Errorf(plugin.CodeInternalError, "decoding response: "+err.Error())
		}
	}
	return map[string]any{"result": result, "status_code": resp.StatusCode}, nil
}

func main() {
	if err := plugin.Serve(notifiarrPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-notifiarr:", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}
