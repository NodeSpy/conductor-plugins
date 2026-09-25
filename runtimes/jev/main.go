// Command conductor-jev is a DECISION runtime for conductor: it answers
// decide: steps with TypeSafe's Jev, a System One model that returns typed,
// calibrated answers (yes/no, choice, score) rather than text.
//
// It declares the system_one/v1 decision protocol and serves the two verbs a
// decision runtime needs, over the ordinary plugin.invoke:
//
//   - decide: the system_one/v1 request (state, model, questions) is sent to
//     POST {api_base}/v1/systemone and the response's answers are returned
//     unchanged. Conductor validates every answer against the questions it
//     asked, so this plugin passes the reply through rather than re-checking it.
//   - models: GET {api_base}/v1/models, as the roster conductor's fleets
//     resolve against (`light: ["jev-*", …]`).
//
// A decision runtime never runs an agent: conductor sends it decide: steps
// only. Built ONLY against the public SDK (pkg/plugin) and the standard
// library. stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// defaultAPIBase is TypeSafe's API origin. Overridable via connection
// `api_base` (tests, or a private gateway).
const defaultAPIBase = "https://api.typesafe.ai"

// defaultModel is the model a request names when conductor passes none — the
// alias TypeSafe's own SDK defaults to.
const defaultModel = "jev-latest"

// requestTimeout bounds one HTTP call. Jev answers in well under a second;
// conductor bounds the whole call too (and moves to the step's next
// candidate on failure), so this only has to be shorter than that.
const requestTimeout = 20 * time.Second

// maxErrorBody caps how much of an error response is quoted back — enough
// to diagnose, never a page of HTML.
const maxErrorBody = 200

var client = &http.Client{Timeout: requestTimeout}

type jevPlugin struct{}

func (jevPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind:      plugin.KindRuntime,
		Type:      "jev",
		Desc:      "TypeSafe Jev: a System One decision runtime — answers decide: steps with calibrated yes/no, choice and score answers.",
		Protocols: []string{plugin.ProtocolSystemOneV1},
		Connection: plugin.Schema{
			"api_key":  {Type: "string", Required: true, Desc: "TypeSafe API key"},
			"api_base": {Type: "string", Desc: "override the API origin (tests, or a private gateway); default " + defaultAPIBase},
			"model":    {Type: "string", Desc: "the model a request names when conductor passes none; default " + defaultModel},
		},
		Verbs: []plugin.Verb{
			{
				Name: plugin.VerbDecide, Desc: "answer one system_one/v1 request",
				Options: plugin.Schema{
					"protocol":  {Type: "string", Required: true, Enum: []string{plugin.ProtocolSystemOneV1}},
					"model":     {Type: "string", Desc: "the Jev model to answer with"},
					"state":     {Type: "any", Required: true, Desc: "the unstructured input the questions are asked about"},
					"questions": {Type: "map", Required: true, Desc: "named noul/choice/score questions"},
				},
				Outputs: plugin.Schema{
					"answers": {Type: "map", Desc: "the v1 answers, keyed by question name"},
					"model":   {Type: "string", Desc: "the model that answered"},
					"usage":   {Type: "map", Desc: "token usage, when reported"},
				},
			},
			{
				Name: plugin.VerbModels, Desc: "list the Jev models this key can use",
				Outputs: plugin.Schema{"models": {Type: "list", Desc: "[{id, name, released}]"}},
			},
		},
		Capabilities: plugin.Capabilities{Egress: []string{"api.typesafe.ai:443"}},
	}
}

// jevConn is the resolved connection for one call.
type jevConn struct {
	apiKey string
	base   string
	model  string
}

func parseConn(m map[string]any) (jevConn, error) {
	key := strings.TrimSpace(str(m["api_key"]))
	if key == "" {
		return jevConn{}, fmt.Errorf("connection.api_key is required")
	}
	return jevConn{
		apiKey: key,
		base:   strings.TrimRight(strOr(m["api_base"], defaultAPIBase), "/"),
		model:  strOr(m["model"], defaultModel),
	}, nil
}

func (jevPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	switch req.Verb {
	case plugin.VerbDecide:
		return conn.decide(req.Options)
	case plugin.VerbModels:
		return conn.models()
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, fmt.Sprintf("unknown verb %q", req.Verb))
}

// decideBody builds the /v1/systemone request body from a decide call's
// options. Pure, so the exact wire shape is testable without a server.
func (c jevConn) decideBody(o map[string]any) (map[string]any, error) {
	if p := str(o["protocol"]); p != plugin.ProtocolSystemOneV1 {
		return nil, fmt.Errorf("unsupported protocol %q (this runtime speaks %s)", p, plugin.ProtocolSystemOneV1)
	}
	if o["state"] == nil {
		return nil, fmt.Errorf("state is required")
	}
	qs, ok := o["questions"].(map[string]any)
	if !ok || len(qs) == 0 {
		return nil, fmt.Errorf("questions are required")
	}
	return map[string]any{
		"model":     strOr(o["model"], c.model),
		"state":     o["state"],
		"questions": qs,
	}, nil
}

func (c jevConn) decide(o map[string]any) (plugin.InvokeResult, error) {
	body, err := c.decideBody(o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "decide: "+err.Error())
	}
	var resp struct {
		Model   string         `json:"model"`
		Usage   map[string]any `json:"usage"`
		Answers map[string]any `json:"answers"`
	}
	if err := c.do(http.MethodPost, "/v1/systemone", body, &resp); err != nil {
		return plugin.InvokeResult{}, err
	}
	model := resp.Model
	if model == "" {
		model = str(body["model"])
	}
	return plugin.InvokeResult{Outputs: map[string]any{
		"answers": resp.Answers, "model": model, "usage": resp.Usage,
	}}, nil
}

func (c jevConn) models() (plugin.InvokeResult, error) {
	var resp struct {
		Models []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			ReleaseDate string `json:"release_date"`
		} `json:"models"`
	}
	if err := c.do(http.MethodGet, "/v1/models", nil, &resp); err != nil {
		return plugin.InvokeResult{}, err
	}
	out := make([]any, 0, len(resp.Models))
	for _, m := range resp.Models {
		if strings.TrimSpace(m.Name) == "" {
			continue
		}
		out = append(out, map[string]any{"id": m.Name, "name": m.Description, "released": m.ReleaseDate})
	}
	return plugin.InvokeResult{Outputs: map[string]any{"models": out}}, nil
}

// do sends one request and decodes a JSON response into out. A non-2xx
// response is an error carrying the status and a capped body; the API key
// never appears in an error.
func (c jevConn) do(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return plugin.Errorf(plugin.CodeInvalidParams, "encoding request: "+err.Error())
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "conductor-jev")
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "typesafe API: "+redact(err.Error(), c.apiKey))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "reading response: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > maxErrorBody {
			snippet = snippet[:maxErrorBody] + "…"
		}
		return plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("typesafe API %s %s: %s", method, resp.Status, redact(snippet, c.apiKey)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return plugin.Errorf(plugin.CodeInternalError, "decoding response: "+err.Error())
	}
	return nil
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

func main() {
	if err := plugin.Serve(jevPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-jev:", err)
		os.Exit(1)
	}
}

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return d
}
