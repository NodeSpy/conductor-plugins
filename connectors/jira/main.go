// Command conductor-jira is the Jira Cloud connector as a standalone external
// conductor plugin (#59). It drives Jira Cloud's REST API v3 over net/http
// (issues, comments, transitions, search) and, as a source, receives Jira
// webhook deliveries and streams a normalized event per delivery for the
// issue-lifecycle events derivable from a single webhook payload:
// jira:issue_created, jira:issue_updated, comment_created. Built ONLY against
// the public SDK (pkg/plugin) and the connector-kit (pkg/sourcekit) — no other
// internal daemon package, no third-party client.
//
// Jira Cloud's webhooks (whether configured via a Connect/Forge app or a
// plain "WebHooks" admin entry) carry NO signature header — there is nothing
// analogous to GitHub's X-Hub-Signature-256 or PagerDuty's
// X-PagerDuty-Signature to verify. This connector instead authenticates
// deliveries against a shared secret the operator configures, checked
// constant-time against either the `secret` query parameter (the value Jira
// lets you template into the webhook URL) or an `X-Conductor-Token` header
// (for a delivery path that can set a custom header instead, e.g. a
// forwarding proxy). Omitting the secret fails closed: `webhook.allow_unsigned:
// true` is the explicit, greppable way to say you accept unauthenticated
// deliveries anyway. The shared sourcekit.Listener.ServeReq hands the
// callback the full request (headers, query, body), so the ?secret= query
// case is checked directly — and the same Listener transparently accepts
// deliveries over a smee.io-style relay (webhook.smee) for endpoints with no
// public URL.
//
// Connection (used for both Invoke and StartSource):
//
//	base_url: "https://acme.atlassian.net"  # REQUIRED — the Jira Cloud site
//	email:    "bot@acme.com"                # Basic-auth user
//	api_token: "<token>"                    # Basic-auth password (Atlassian API token)
//	webhook:
//	  listen: ":9097"                       # HTTP listener address (StartSource only)
//	  path: "/jira"                         # request path (default /jira)
//	  secret: "<shared secret>"             # checked against ?secret= or X-Conductor-Token
//	  allow_unsigned: false                 # accept deliveries with no secret configured
//	  smee: "https://smee.io/abc123"        # optional smee.io-style SSE relay channel
//
// Jira Cloud REST v3 is reached at base_url + "/rest/api/3"; tests point
// base_url at an httptest.Server to exercise the same code path with no
// network access.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type jiraPlugin struct{}

func (jiraPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "jira",
		Desc: "Jira Cloud: issues, comments, transitions, and search as verbs (REST API v3); issue-created/updated and comment-created events in (webhook-derived, shared-secret authenticated).",
		Connection: plugin.Schema{
			"base_url":  {Type: "string", Required: true, Desc: "Jira Cloud site base URL, e.g. https://acme.atlassian.net (self-hosted Jira Server/Data Center is not supported by the v3 REST verbs this connector calls)"},
			"email":     {Type: "string", Desc: "Atlassian account email for HTTP Basic auth"},
			"api_token": {Type: "string", Desc: "Atlassian API token for HTTP Basic auth"},
			"webhook":   {Type: "map", Desc: "source transport: listen, path (default /jira), secret, allow_unsigned, smee"},
		},
		Events:       jiraEvents(),
		Verbs:        jiraVerbs(),
		Capabilities: plugin.Capabilities{Egress: []string{"*.atlassian.net:443"}},
	}
}

// jiraEvents declares the source events this plugin streams. issueContext and
// commentContext each carry a scalar copy of every filterable field PLUS a
// plural alias (project/projects, status/statuses, ...) so the daemon's
// generic list-contains filter evaluator matches against the documented
// plural filter vocabulary regardless of which field a trigger filters on.
func jiraEvents() []plugin.Event {
	filters := plugin.Schema{
		"projects":   {Type: "list", Desc: "Jira project keys (empty = any)"},
		"statuses":   {Type: "list", Desc: "issue status names (empty = any)"},
		"issuetypes": {Type: "list", Desc: "issue type names (empty = any)"},
		"priorities": {Type: "list", Desc: "priority names (empty = any)"},
	}
	issueContext := plugin.Schema{
		"key":       {Type: "string"},
		"summary":   {Type: "string"},
		"status":    {Type: "string"},
		"issuetype": {Type: "string"},
		"project":   {Type: "string"},
		"assignee":  {Type: "string"},
		"reporter":  {Type: "string"},
		"priority":  {Type: "string"},
		"url":       {Type: "string"},
		"action":    {Type: "string"},
	}
	commentContext := plugin.Schema{
		"key":          {Type: "string"},
		"comment_body": {Type: "string"},
		"author":       {Type: "string"},
		"project":      {Type: "string"},
		"status":       {Type: "string"},
		"issuetype":    {Type: "string"},
		"url":          {Type: "string"},
		"action":       {Type: "string"},
	}
	return []plugin.Event{
		{Name: "issue_created", Desc: "a Jira issue was created", Context: issueContext, Filters: filters},
		{Name: "issue_updated", Desc: "a Jira issue was updated", Context: issueContext, Filters: filters},
		{Name: "comment_created", Desc: "a comment was added to a Jira issue", Context: commentContext, Filters: filters},
	}
}

func jiraVerbs() []plugin.Verb {
	statusOut := plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}}
	return []plugin.Verb{
		{
			Name: "create_issue", Desc: "create an issue",
			Options: plugin.Schema{
				"project":     {Type: "string", Required: true, Scope: "project", Desc: "project key"},
				"summary":     {Type: "string", Required: true},
				"issuetype":   {Type: "string", Required: true, Desc: "issue type name, e.g. Bug, Task, Story"},
				"description": {Type: "string", Desc: "plain text; wrapped into a minimal ADF document"},
				"labels":      {Type: "list"},
				"assignee_id": {Type: "string", Desc: "Atlassian account id"},
				"priority":    {Type: "string", Desc: "priority name, e.g. High"},
				"fields":      {Type: "map", Desc: "raw Jira fields, merged in last (overrides the shortcuts above)"},
			},
			Outputs: plugin.Schema{
				"key": {Type: "string"}, "id": {Type: "string"}, "url": {Type: "string"},
				"result": {Type: "any"}, "status_code": {Type: "integer"},
			},
		},
		{
			Name: "add_comment", Desc: "add a comment to an issue",
			Options: plugin.Schema{
				"key":  {Type: "string", Required: true, Scope: "issue"},
				"body": {Type: "string", Required: true, Desc: "plain text; wrapped into a minimal ADF document"},
			},
			Outputs: plugin.Schema{"id": {Type: "string"}, "result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "transition", Desc: "move an issue through a workflow transition",
			Usage: "pass transition_id if known, or transition_name to look it up via list_transitions first",
			Options: plugin.Schema{
				"key":             {Type: "string", Required: true, Scope: "issue"},
				"transition_id":   {Type: "string", Desc: "the transition id (see list_transitions)"},
				"transition_name": {Type: "string", Desc: "the transition's display name, looked up case-insensitively (alternative to transition_id)"},
			},
			Outputs: statusOut,
		},
		{
			Name: "list_transitions", Desc: "list the workflow transitions available on an issue",
			Options: plugin.Schema{"key": {Type: "string", Required: true, Scope: "issue"}},
			Outputs: plugin.Schema{"transitions": {Type: "list"}, "result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "get_issue", Desc: "read an issue",
			Options: plugin.Schema{
				"key":    {Type: "string", Required: true, Scope: "issue"},
				"fields": {Type: "list", Desc: "field names to return (default: Jira's navigable field set)"},
			},
			Outputs: statusOut,
		},
		{
			Name: "update_issue", Desc: "edit an issue's fields",
			Options: plugin.Schema{
				"key":         {Type: "string", Required: true, Scope: "issue"},
				"summary":     {Type: "string"},
				"description": {Type: "string", Desc: "plain text; wrapped into a minimal ADF document"},
				"fields":      {Type: "map", Desc: "raw Jira fields, merged in last (overrides the shortcuts above)"},
			},
			Outputs: statusOut,
		},
		{
			Name: "assign", Desc: "assign an issue",
			Options: plugin.Schema{
				"key":        {Type: "string", Required: true, Scope: "issue"},
				"account_id": {Type: "string", Required: true, Desc: "Atlassian account id (use \"-1\" for automatic, or unassigned via Jira's own convention)"},
			},
			Outputs: statusOut,
		},
		{
			Name: "add_labels", Desc: "add labels to an issue",
			Options: plugin.Schema{
				"key":    {Type: "string", Required: true, Scope: "issue"},
				"labels": {Type: "list", Required: true},
			},
			Outputs: statusOut,
		},
		{
			Name: "search", Desc: "search issues with JQL",
			Options: plugin.Schema{
				"jql":         {Type: "string", Required: true},
				"max_results": {Type: "integer"},
				"fields":      {Type: "list"},
			},
			Outputs: plugin.Schema{"issues": {Type: "list"}, "result": {Type: "any"}, "status_code": {Type: "integer"}},
		},
		{
			Name: "api", Desc: "call any Jira REST v3 endpoint not covered by a first-class verb",
			Usage: "escape hatch: method + path (relative to /rest/api/3), optional query and body",
			Options: plugin.Schema{
				"method": {Type: "string", Required: true, Enum: []string{"GET", "POST", "PUT", "DELETE"}},
				"path":   {Type: "string", Required: true, Desc: "path relative to /rest/api/3, e.g. \"issue/PROJ-1\""},
				"query":  {Type: "map", Desc: "query string parameters"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: statusOut,
		},
	}
}

// jiraConn is the resolved connection config for one invocation.
type jiraConn struct {
	rawBase string // the site base_url, trimmed (used for /browse/ links)
	base    string // rawBase + "/rest/api/3" (the REST API root)
	email   string
	token   string
}

func parseConn(m map[string]any) (jiraConn, error) {
	base := strings.TrimRight(str(m["base_url"]), "/")
	if base == "" {
		return jiraConn{}, fmt.Errorf("connection.base_url is required")
	}
	return jiraConn{
		rawBase: base,
		base:    base + "/rest/api/3",
		email:   str(m["email"]),
		token:   str(m["api_token"]),
	}, nil
}

func (c jiraConn) browseURL(key string) string {
	return c.rawBase + "/browse/" + key
}

// do sends one HTTP request to the Jira REST API, attaching HTTP Basic auth,
// and decodes the JSON response body. A non-2xx response is a
// plugin.CodeInternalError carrying the status and response body; verb
// callers never see a raw *http.Response.
func (c jiraConn) do(method, path string, query map[string]string, body any) (any, int, error) {
	u := strings.TrimRight(c.base, "/") + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		keys := make([]string, 0, len(query))
		for k := range query {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		v := url.Values{}
		for _, k := range keys {
			v.Set(k, query[k])
		}
		u += "?" + v.Encode()
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, plugin.Errorf(plugin.CodeInvalidParams, "encoding request body: "+err.Error())
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, u, reader)
	if err != nil {
		return nil, 0, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	if c.email != "" || c.token != "" {
		req.SetBasicAuth(c.email, c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, plugin.Errorf(plugin.CodeInternalError, "reading response: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("jira API %s: %s", resp.Status, strings.TrimSpace(string(raw))))
	}

	var result any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, resp.StatusCode, plugin.Errorf(plugin.CodeInternalError, "decoding response: "+err.Error())
		}
	}
	return result, resp.StatusCode, nil
}

// adf wraps plain text into the minimal Atlassian Document Format Jira's v3
// API requires for rich-text fields (description, comment body): a single
// paragraph containing one text node.
func adf(text string) map[string]any {
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": text},
				},
			},
		},
	}
}

func (jiraPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	switch req.Verb {
	case "create_issue":
		return createIssue(conn, o)
	case "add_comment":
		return addComment(conn, o)
	case "transition":
		return doTransition(conn, o)
	case "list_transitions":
		return listTransitions(conn, o)
	case "get_issue":
		return getIssue(conn, o)
	case "update_issue":
		return updateIssue(conn, o)
	case "assign":
		return doAssign(conn, o)
	case "add_labels":
		return addLabels(conn, o)
	case "search":
		return doSearch(conn, o)
	case "api":
		return doAPI(conn, o)
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
}

func createIssue(conn jiraConn, o map[string]any) (plugin.InvokeResult, error) {
	project := str(o["project"])
	summary := str(o["summary"])
	issuetype := str(o["issuetype"])
	if project == "" || summary == "" || issuetype == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "project, summary, and issuetype are required")
	}
	fields := map[string]any{
		"project":   map[string]any{"key": project},
		"summary":   summary,
		"issuetype": map[string]any{"name": issuetype},
	}
	if d := str(o["description"]); d != "" {
		fields["description"] = adf(d)
	}
	if labels := strList(o["labels"]); len(labels) > 0 {
		fields["labels"] = labels
	}
	if aid := str(o["assignee_id"]); aid != "" {
		fields["assignee"] = map[string]any{"id": aid}
	}
	if p := str(o["priority"]); p != "" {
		fields["priority"] = map[string]any{"name": p}
	}
	if extra, ok := o["fields"].(map[string]any); ok {
		for k, v := range extra {
			fields[k] = v
		}
	}
	result, status, err := conn.do(http.MethodPost, "issue", nil, map[string]any{"fields": fields})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	key, id := "", ""
	if m, ok := result.(map[string]any); ok {
		key, _ = m["key"].(string)
		id, _ = m["id"].(string)
	}
	outputs := map[string]any{"result": result, "status_code": status}
	if key != "" {
		outputs["key"] = key
		outputs["url"] = conn.browseURL(key)
	}
	if id != "" {
		outputs["id"] = id
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

func addComment(conn jiraConn, o map[string]any) (plugin.InvokeResult, error) {
	key := str(o["key"])
	body := str(o["body"])
	if key == "" || body == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "key and body are required")
	}
	result, status, err := conn.do(http.MethodPost, "issue/"+key+"/comment", nil, map[string]any{"body": adf(body)})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	outputs := map[string]any{"result": result, "status_code": status}
	if m, ok := result.(map[string]any); ok {
		if id, ok := m["id"].(string); ok {
			outputs["id"] = id
		}
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

func doTransition(conn jiraConn, o map[string]any) (plugin.InvokeResult, error) {
	key := str(o["key"])
	if key == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "key is required")
	}
	tid := str(o["transition_id"])
	if tid == "" {
		name := str(o["transition_name"])
		if name == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "transition_id or transition_name is required")
		}
		result, _, err := conn.do(http.MethodGet, "issue/"+key+"/transitions", nil, nil)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		found, err := findTransitionID(result, name)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
		}
		tid = found
	}
	result, status, err := conn.do(http.MethodPost, "issue/"+key+"/transitions", nil, map[string]any{"transition": map[string]any{"id": tid}})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": result, "status_code": status}}, nil
}

// findTransitionID looks a transition up by display name (case-insensitive)
// in the decoded /issue/{key}/transitions response.
func findTransitionID(result any, name string) (string, error) {
	m, _ := result.(map[string]any)
	list, _ := m["transitions"].([]any)
	for _, t := range list {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if strings.EqualFold(str(tm["name"]), name) {
			return str(tm["id"]), nil
		}
	}
	return "", fmt.Errorf("no transition named %q", name)
}

func listTransitions(conn jiraConn, o map[string]any) (plugin.InvokeResult, error) {
	key := str(o["key"])
	if key == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "key is required")
	}
	result, status, err := conn.do(http.MethodGet, "issue/"+key+"/transitions", nil, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	outputs := map[string]any{"result": result, "status_code": status}
	if m, ok := result.(map[string]any); ok {
		if transitions, ok := m["transitions"]; ok {
			outputs["transitions"] = transitions
		}
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

func getIssue(conn jiraConn, o map[string]any) (plugin.InvokeResult, error) {
	key := str(o["key"])
	if key == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "key is required")
	}
	q := map[string]string{}
	if fields := strList(o["fields"]); len(fields) > 0 {
		q["fields"] = strings.Join(fields, ",")
	}
	result, status, err := conn.do(http.MethodGet, "issue/"+key, q, nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": result, "status_code": status}}, nil
}

func updateIssue(conn jiraConn, o map[string]any) (plugin.InvokeResult, error) {
	key := str(o["key"])
	if key == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "key is required")
	}
	fields := map[string]any{}
	if f, ok := o["fields"].(map[string]any); ok {
		for k, v := range f {
			fields[k] = v
		}
	}
	if s := str(o["summary"]); s != "" {
		fields["summary"] = s
	}
	if d := str(o["description"]); d != "" {
		fields["description"] = adf(d)
	}
	if len(fields) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "fields, summary, or description is required")
	}
	result, status, err := conn.do(http.MethodPut, "issue/"+key, nil, map[string]any{"fields": fields})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": result, "status_code": status}}, nil
}

func doAssign(conn jiraConn, o map[string]any) (plugin.InvokeResult, error) {
	key := str(o["key"])
	accountID := str(o["account_id"])
	if key == "" || accountID == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "key and account_id are required")
	}
	result, status, err := conn.do(http.MethodPut, "issue/"+key+"/assignee", nil, map[string]any{"accountId": accountID})
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": result, "status_code": status}}, nil
}

func addLabels(conn jiraConn, o map[string]any) (plugin.InvokeResult, error) {
	key := str(o["key"])
	labels := strList(o["labels"])
	if key == "" || len(labels) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "key and labels are required")
	}
	adds := make([]any, 0, len(labels))
	for _, l := range labels {
		adds = append(adds, map[string]any{"add": l})
	}
	body := map[string]any{"update": map[string]any{"labels": adds}}
	result, status, err := conn.do(http.MethodPut, "issue/"+key, nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": result, "status_code": status}}, nil
}

func doSearch(conn jiraConn, o map[string]any) (plugin.InvokeResult, error) {
	jql := str(o["jql"])
	if jql == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "jql is required")
	}
	body := map[string]any{"jql": jql}
	if mr, ok := o["max_results"]; ok {
		body["maxResults"] = mr
	}
	if fields := strList(o["fields"]); len(fields) > 0 {
		body["fields"] = fields
	}
	result, status, err := conn.do(http.MethodPost, "search", nil, body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	outputs := map[string]any{"result": result, "status_code": status}
	if m, ok := result.(map[string]any); ok {
		if issues, ok := m["issues"]; ok {
			outputs["issues"] = issues
		}
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

func doAPI(conn jiraConn, o map[string]any) (plugin.InvokeResult, error) {
	method := strings.ToUpper(str(o["method"]))
	if method == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "method is required")
	}
	path := strings.TrimPrefix(str(o["path"]), "/")
	if path == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "path is required")
	}
	q := map[string]string{}
	if m, ok := o["query"].(map[string]any); ok {
		for k, v := range m {
			q[k] = fmt.Sprintf("%v", v)
		}
	}
	result, status, err := conn.do(method, path, q, o["body"])
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": result, "status_code": status}}, nil
}

// StartSource runs the webhook listener for one connector instance. Jira
// Cloud webhooks carry no signature, so authentication is a shared secret
// checked against the `secret` query parameter or an X-Conductor-Token
// header — never sourcekit.Listener's built-in HMAC verification (there is
// no HMAC to verify), hence Listener.Secret is left empty and
// verifyJiraSecret does the check itself against the sourcekit.Request the
// shared Listener hands the callback (headers, query, body — over the HTTP
// listener AND, when webhook.smee is set, a smee.io-style SSE relay).
func (jiraPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	conn, err := parseConn(cfg)
	if err != nil {
		return err
	}
	webhook, _ := cfg["webhook"].(map[string]any)
	addr := str(webhook["listen"])
	path := strOr(webhook["path"], "/jira")
	secret := str(webhook["secret"])
	allowUnsigned := boolv(webhook["allow_unsigned"])
	smeeURL := str(webhook["smee"])
	if addr == "" && smeeURL == "" {
		return fmt.Errorf("jira: no webhook.listen address or webhook.smee relay configured")
	}
	if err := requireWebhookSecret("jira", secret, allowUnsigned, "webhook.secret"); err != nil {
		return err
	}

	dedup := sourcekit.NewDedup(4096)
	if addr != "" {
		fmt.Fprintf(os.Stderr, "jira[%s]: listening on %s%s\n", req.Instance, addr, path)
	}
	if smeeURL != "" {
		fmt.Fprintf(os.Stderr, "jira[%s]: relaying via smee channel %s\n", req.Instance, smeeURL)
	}

	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smeeURL}
	return ln.ServeReq(ctx, func(rq *sourcekit.Request) {
		if !verifyJiraSecret(secret, rq) {
			return
		}
		for _, ev := range parseWebhook(conn.rawBase, rq.Body) {
			if ev.Dedup != "" && !dedup.Add(ev.Dedup) {
				continue
			}
			_ = emit(ev)
		}
	})
}

// verifyJiraSecret reports whether the request carries the configured shared
// secret, via the `secret` query parameter or an X-Conductor-Token header
// (query checked first). An empty configured secret always passes — the
// caller (requireWebhookSecret) already refused to start unless that was an
// explicit allow_unsigned opt-in. Comparison is constant-time.
func verifyJiraSecret(secret string, rq *sourcekit.Request) bool {
	if secret == "" {
		return true
	}
	got := rq.Query.Get("secret")
	if got == "" {
		got = rq.Header.Get("X-Conductor-Token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// requireWebhookSecret refuses to start an unauthenticated webhook listener.
//
// A missing secret is far more often a mistake than a choice, so it fails
// closed; `webhook.allow_unsigned: true` is the explicit, greppable way to
// say you meant it (e.g. a trusted network path, or a front door that
// authenticates some other way).
func requireWebhookSecret(who, secret string, allowUnsigned bool, allowKey string) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintf(os.Stderr, "%s: allow_unsigned is set — accepting UNAUTHENTICATED webhooks; anyone who can reach the listen address can fire triggers\n", who)
		return nil
	}
	return fmt.Errorf("%s: no webhook secret configured (%s) — an unauthenticated listener accepts any POST on the listen address as a real event. Set it, or set webhook.allow_unsigned: true if you genuinely front this with something else that authenticates", who, allowKey)
}

// wireEvent is the normalized event streamed to the daemon's plugin source
// adapter.
type wireEvent struct {
	Event   string         `json:"event"`
	Kind    string         `json:"kind,omitempty"`
	Title   string         `json:"title,omitempty"`
	Dedup   string         `json:"dedup,omitempty"`
	Context map[string]any `json:"context,omitempty"`
}

// jiraWebhookPayload is the subset of Jira's webhook payload shapes this
// plugin reads.
type jiraWebhookPayload struct {
	Timestamp    int64  `json:"timestamp"`
	WebhookEvent string `json:"webhookEvent"`
	Issue        *struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Status  struct {
				Name string `json:"name"`
			} `json:"status"`
			IssueType struct {
				Name string `json:"name"`
			} `json:"issuetype"`
			Project struct {
				Key string `json:"key"`
			} `json:"project"`
			Assignee *struct {
				DisplayName string `json:"displayName"`
			} `json:"assignee"`
			Reporter *struct {
				DisplayName string `json:"displayName"`
			} `json:"reporter"`
			Priority *struct {
				Name string `json:"name"`
			} `json:"priority"`
		} `json:"fields"`
	} `json:"issue"`
	Comment *struct {
		Body   string `json:"body"`
		Author struct {
			DisplayName string `json:"displayName"`
		} `json:"author"`
	} `json:"comment"`
}

// parseWebhook decodes one Jira webhook delivery into the normalized events
// this plugin streams. rawBase is the connector's base_url, used to build
// the issue's browse URL.
func parseWebhook(rawBase string, body []byte) []wireEvent {
	var p jiraWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	switch p.WebhookEvent {
	case "jira:issue_created":
		return issueEvent("issue_created", rawBase, p)
	case "jira:issue_updated":
		return issueEvent("issue_updated", rawBase, p)
	case "comment_created":
		return commentEvent(rawBase, p)
	}
	return nil
}

func issueEvent(name, rawBase string, p jiraWebhookPayload) []wireEvent {
	if p.Issue == nil {
		return nil
	}
	key := p.Issue.Key
	f := p.Issue.Fields
	assignee, reporter, priority := "", "", ""
	if f.Assignee != nil {
		assignee = f.Assignee.DisplayName
	}
	if f.Reporter != nil {
		reporter = f.Reporter.DisplayName
	}
	if f.Priority != nil {
		priority = f.Priority.Name
	}
	action := strings.TrimPrefix(p.WebhookEvent, "jira:")
	return []wireEvent{{
		Event: name, Kind: name,
		Title: fmt.Sprintf("%s: %s", key, f.Summary),
		Dedup: fmt.Sprintf("%s:%s:%d", key, p.WebhookEvent, p.Timestamp),
		Context: map[string]any{
			"key": key, "summary": f.Summary, "status": f.Status.Name, "issuetype": f.IssueType.Name,
			"project": f.Project.Key, "assignee": assignee, "reporter": reporter, "priority": priority,
			"url": rawBase + "/browse/" + key, "action": action,
			// Plural aliases for the daemon's generic list-contains filter
			// evaluator (filters: {projects/statuses/issuetypes/priorities: [...]}).
			"projects": f.Project.Key, "statuses": f.Status.Name, "issuetypes": f.IssueType.Name, "priorities": priority,
		},
	}}
}

func commentEvent(rawBase string, p jiraWebhookPayload) []wireEvent {
	if p.Issue == nil || p.Comment == nil {
		return nil
	}
	key := p.Issue.Key
	f := p.Issue.Fields
	return []wireEvent{{
		Event: "comment_created", Kind: "comment_created",
		Title: fmt.Sprintf("comment on %s by %s", key, p.Comment.Author.DisplayName),
		Dedup: fmt.Sprintf("%s:%s:%d", key, p.WebhookEvent, p.Timestamp),
		Context: map[string]any{
			"key": key, "comment_body": p.Comment.Body, "author": p.Comment.Author.DisplayName,
			"project": f.Project.Key, "status": f.Status.Name, "issuetype": f.IssueType.Name,
			"url": rawBase + "/browse/" + key, "action": "comment_created",
			"projects": f.Project.Key, "statuses": f.Status.Name, "issuetypes": f.IssueType.Name,
		},
	}}
}

func main() {
	if err := plugin.Serve(jiraPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-jira:", err)
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

func boolv(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	}
	return false
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
