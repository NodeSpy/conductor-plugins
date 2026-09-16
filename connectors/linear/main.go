// Command conductor-linear is the Linear connector as a standalone external
// conductor plugin (#59). It drives the Linear GraphQL API (verbs: create/
// update/archive issues, comment, get/search issues, and a raw graphql escape
// hatch) and, as a source, receives Linear webhook deliveries — HMAC-SHA256
// signed in the `Linear-Signature` header — and streams a normalized event
// per delivery (issue / comment / project, matching the webhook's `type`).
//
// Built ONLY against the public SDK (pkg/plugin) and the connector-kit
// (pkg/sourcekit) — no other internal daemon package, no third-party
// dependency.
//
// Connection (used for both Invoke and StartSource):
//
//	api_key: "<linear API key>"          # sent RAW (no "Bearer ") in Authorization
//	api_base: "https://api.example.com/graphql" # override the GraphQL endpoint (tests)
//	webhook:
//	  listen: ":9100"                    # HTTP listener address (StartSource only; optional if smee is set)
//	  path: "/linear"                    # request path (default /linear)
//	  secret: "<webhook signing secret>" # HMAC-SHA256 hex secret
//	  allow_unsigned: false              # explicit opt-in to accept unsigned webhooks
//	  smee: "https://smee.io/AbC123"     # smee.io-style SSE relay URL (optional; also/instead of listen)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

const defaultAPIBase = "https://api.linear.app/graphql"

type linearPlugin struct {
	httpClient *http.Client
}

func newLinearPlugin() *linearPlugin {
	return &linearPlugin{httpClient: &http.Client{Timeout: 30 * time.Second}}
}

func (l *linearPlugin) Describe() plugin.Decl {
	filters := plugin.Schema{
		"actions":    {Type: "list", Desc: "webhook actions to match (create/update/remove)"},
		"types":      {Type: "list", Desc: "webhook types to match (Issue/Comment/Project)"},
		"states":     {Type: "list", Desc: "issue workflow state names"},
		"teams":      {Type: "list", Desc: "team keys/names"},
		"priorities": {Type: "list", Desc: "issue priority values"},
		"action":     {Type: "string"},
		"type":       {Type: "string"},
		"state":      {Type: "string"},
		"team":       {Type: "string"},
		"priority":   {Type: "string"},
	}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "linear",
		Desc: "Linear: issue/comment/project verbs over the GraphQL API; issue/comment/project webhook events in.",
		Connection: plugin.Schema{
			"api_key":  {Type: "string", Required: true, Desc: "Linear personal API key or app token, sent raw (no Bearer prefix) in Authorization"},
			"api_base": {Type: "string", Desc: "override the GraphQL endpoint URL (default https://api.linear.app/graphql; used for tests)"},
			"webhook":  {Type: "map", Desc: "source transport: listen (optional if smee is set), path, secret, allow_unsigned, smee (smee.io-style SSE relay URL, e.g. https://smee.io/AbC123 — also (or instead) receive forwarded deliveries over SSE when the endpoint has no public URL)"},
		},
		Events: []plugin.Event{
			{
				Name: "issue", Desc: "a Linear issue was created, updated, or removed",
				Filters: filters,
				Context: plugin.Schema{
					"id":         {Type: "string"},
					"identifier": {Type: "string"},
					"title":      {Type: "string"},
					"state":      {Type: "string"},
					"priority":   {Type: "integer"},
					"assignee":   {Type: "string"},
					"team":       {Type: "string"},
					"url":        {Type: "string"},
					"action":     {Type: "string"},
				},
			},
			{
				Name: "comment", Desc: "a Linear comment was created, updated, or removed",
				Filters: filters,
				Context: plugin.Schema{
					"id":       {Type: "string"},
					"body":     {Type: "string"},
					"issue_id": {Type: "string"},
					"user":     {Type: "string"},
					"action":   {Type: "string"},
				},
			},
			{
				Name: "project", Desc: "a Linear project was created, updated, or removed",
				Filters: filters,
				Context: plugin.Schema{
					"id":     {Type: "string"},
					"name":   {Type: "string"},
					"state":  {Type: "string"},
					"url":    {Type: "string"},
					"action": {Type: "string"},
				},
			},
		},
		Verbs: []plugin.Verb{
			{
				Name: "create_issue", Desc: "create an issue",
				Options: plugin.Schema{
					"title":       {Type: "string", Required: true},
					"team_id":     {Type: "string", Required: true},
					"description": {Type: "string"},
					"priority":    {Type: "integer"},
					"assignee_id": {Type: "string"},
					"state_id":    {Type: "string"},
					"labels":      {Type: "list", Desc: "label ids"},
				},
				Outputs: plugin.Schema{"issue_id": {Type: "string"}, "identifier": {Type: "string"}, "url": {Type: "string"}},
			},
			{
				Name: "update_issue", Desc: "update an issue",
				Options: plugin.Schema{
					"id":          {Type: "string", Required: true},
					"title":       {Type: "string"},
					"description": {Type: "string"},
					"priority":    {Type: "integer"},
					"state_id":    {Type: "string"},
					"assignee_id": {Type: "string"},
				},
				Outputs: plugin.Schema{"issue_id": {Type: "string"}, "identifier": {Type: "string"}, "url": {Type: "string"}},
			},
			{
				Name: "comment", Desc: "post a comment on an issue",
				Options: plugin.Schema{
					"issue_id": {Type: "string", Required: true},
					"body":     {Type: "string", Required: true},
				},
				Outputs: plugin.Schema{"id": {Type: "string"}, "url": {Type: "string"}},
			},
			{
				Name: "get_issue", Desc: "read an issue's current state",
				Options: plugin.Schema{
					"id": {Type: "string", Required: true},
				},
				Outputs: plugin.Schema{"result": {Type: "map"}},
			},
			{
				Name: "search_issues", Desc: "search issues by full-text query or a Linear IssueFilter",
				Options: plugin.Schema{
					"query":  {Type: "string", Desc: "full-text search query"},
					"filter": {Type: "map", Desc: "a Linear IssueFilter object (mutually exclusive with query)"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}},
			},
			{
				Name: "archive_issue", Desc: "archive an issue",
				Options: plugin.Schema{
					"id": {Type: "string", Required: true},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "graphql", Desc: "escape hatch: run any GraphQL query/mutation against the Linear API",
				Options: plugin.Schema{
					"query":     {Type: "string", Required: true},
					"variables": {Type: "map"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}},
			},
		},
		Capabilities: plugin.Capabilities{Egress: []string{"api.linear.app:443"}},
	}
}

// --- GraphQL transport ---

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphqlError struct {
	Message string `json:"message"`
}

type graphqlResponse struct {
	Data   map[string]any `json:"data"`
	Errors []graphqlError `json:"errors,omitempty"`
}

// doGraphQL POSTs one GraphQL operation to the Linear API (or the api_base
// override) with the connection's api_key sent RAW (no "Bearer " prefix) in
// Authorization, per Linear's API convention. A non-2xx response, OR a 2xx
// response carrying a GraphQL "errors" array, is reported as CodeInternalError
// with the response body attached so the operator can see exactly what Linear
// said.
func (l *linearPlugin) doGraphQL(ctx context.Context, conn map[string]any, query string, variables map[string]any) (map[string]any, error) {
	apiKey := str(conn["api_key"])
	if apiKey == "" {
		return nil, fmt.Errorf("linear: connection.api_key is required")
	}
	base := strOr(conn["api_base"], defaultAPIBase)

	reqBody, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, fmt.Errorf("linear: encoding request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("linear: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", apiKey)

	resp, err := l.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("linear: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("linear: reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("linear: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var gr graphqlResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("linear: decoding response: %w: %s", err, string(body))
	}
	if len(gr.Errors) > 0 {
		msgs := make([]string, 0, len(gr.Errors))
		for _, e := range gr.Errors {
			msgs = append(msgs, e.Message)
		}
		return nil, fmt.Errorf("linear: graphql error: %s: %s", strings.Join(msgs, "; "), string(body))
	}
	return gr.Data, nil
}

func (l *linearPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	ctx := context.Background()

	switch req.Verb {
	case "create_issue":
		return l.createIssue(ctx, req.Connection, o)
	case "update_issue":
		return l.updateIssue(ctx, req.Connection, o)
	case "comment":
		return l.postComment(ctx, req.Connection, o)
	case "get_issue":
		return l.getIssue(ctx, req.Connection, o)
	case "search_issues":
		return l.searchIssues(ctx, req.Connection, o)
	case "archive_issue":
		return l.archiveIssue(ctx, req.Connection, o)
	case "graphql":
		return l.rawGraphQL(ctx, req.Connection, o)
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
}

const issueCreateMutation = `mutation IssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue { id identifier url }
  }
}`

func (l *linearPlugin) createIssue(ctx context.Context, conn, o map[string]any) (plugin.InvokeResult, error) {
	title := str(o["title"])
	teamID := str(o["team_id"])
	if title == "" || teamID == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "create_issue: title and team_id are required")
	}
	input := map[string]any{"title": title, "teamId": teamID}
	setIfPresent(input, "description", o["description"])
	setIfPresent(input, "priority", o["priority"])
	setIfPresent(input, "assigneeId", o["assignee_id"])
	setIfPresent(input, "stateId", o["state_id"])
	if labels := strList(o["labels"]); len(labels) > 0 {
		input["labelIds"] = labels
	}
	data, err := l.doGraphQL(ctx, conn, issueCreateMutation, map[string]any{"input": input})
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	issue, _ := mapAt(data, "issueCreate", "issue")
	return plugin.InvokeResult{Outputs: map[string]any{
		"issue_id":   gs(issue, "id"),
		"identifier": gs(issue, "identifier"),
		"url":        gs(issue, "url"),
	}}, nil
}

const issueUpdateMutation = `mutation IssueUpdate($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) {
    success
    issue { id identifier url }
  }
}`

func (l *linearPlugin) updateIssue(ctx context.Context, conn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "update_issue: id is required")
	}
	input := map[string]any{}
	setIfPresent(input, "title", o["title"])
	setIfPresent(input, "description", o["description"])
	setIfPresent(input, "priority", o["priority"])
	setIfPresent(input, "stateId", o["state_id"])
	setIfPresent(input, "assigneeId", o["assignee_id"])
	data, err := l.doGraphQL(ctx, conn, issueUpdateMutation, map[string]any{"id": id, "input": input})
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	issue, _ := mapAt(data, "issueUpdate", "issue")
	return plugin.InvokeResult{Outputs: map[string]any{
		"issue_id":   gs(issue, "id"),
		"identifier": gs(issue, "identifier"),
		"url":        gs(issue, "url"),
	}}, nil
}

const commentCreateMutation = `mutation CommentCreate($input: CommentCreateInput!) {
  commentCreate(input: $input) {
    success
    comment { id url }
  }
}`

func (l *linearPlugin) postComment(ctx context.Context, conn, o map[string]any) (plugin.InvokeResult, error) {
	issueID := str(o["issue_id"])
	body := str(o["body"])
	if issueID == "" || body == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "comment: issue_id and body are required")
	}
	input := map[string]any{"issueId": issueID, "body": body}
	data, err := l.doGraphQL(ctx, conn, commentCreateMutation, map[string]any{"input": input})
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	comment, _ := mapAt(data, "commentCreate", "comment")
	return plugin.InvokeResult{Outputs: map[string]any{
		"id":  gs(comment, "id"),
		"url": gs(comment, "url"),
	}}, nil
}

const getIssueQuery = `query GetIssue($id: String!) {
  issue(id: $id) {
    id identifier title description priority url
    state { name }
    assignee { name }
    team { name key }
  }
}`

func (l *linearPlugin) getIssue(ctx context.Context, conn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "get_issue: id is required")
	}
	data, err := l.doGraphQL(ctx, conn, getIssueQuery, map[string]any{"id": id})
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	result, _ := data["issue"].(map[string]any)
	return plugin.InvokeResult{Outputs: map[string]any{"result": result}}, nil
}

const issueSearchQuery = `query IssueSearch($query: String!) {
  issueSearch(query: $query) {
    nodes { id identifier title url }
  }
}`

const issuesFilterQuery = `query Issues($filter: IssueFilter) {
  issues(filter: $filter) {
    nodes { id identifier title url }
  }
}`

func (l *linearPlugin) searchIssues(ctx context.Context, conn, o map[string]any) (plugin.InvokeResult, error) {
	var (
		data map[string]any
		err  error
		root string
	)
	switch {
	case str(o["query"]) != "":
		root = "issueSearch"
		data, err = l.doGraphQL(ctx, conn, issueSearchQuery, map[string]any{"query": str(o["query"])})
	case o["filter"] != nil:
		root = "issues"
		data, err = l.doGraphQL(ctx, conn, issuesFilterQuery, map[string]any{"filter": o["filter"]})
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "search_issues: query or filter is required")
	}
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	conn2, _ := data[root].(map[string]any)
	nodes, _ := conn2["nodes"].([]any)
	return plugin.InvokeResult{Outputs: map[string]any{"items": nodes}}, nil
}

const issueArchiveMutation = `mutation IssueArchive($id: String!) {
  issueArchive(id: $id) { success }
}`

func (l *linearPlugin) archiveIssue(ctx context.Context, conn, o map[string]any) (plugin.InvokeResult, error) {
	id := str(o["id"])
	if id == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "archive_issue: id is required")
	}
	data, err := l.doGraphQL(ctx, conn, issueArchiveMutation, map[string]any{"id": id})
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	archive, _ := data["issueArchive"].(map[string]any)
	ok, _ := archive["success"].(bool)
	return plugin.InvokeResult{Outputs: map[string]any{"ok": ok}}, nil
}

func (l *linearPlugin) rawGraphQL(ctx context.Context, conn, o map[string]any) (plugin.InvokeResult, error) {
	query := str(o["query"])
	if query == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "graphql: query is required")
	}
	variables, _ := o["variables"].(map[string]any)
	data, err := l.doGraphQL(ctx, conn, query, variables)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": data}}, nil
}

// --- source: Linear webhook ingest ---

// verifySignature reports whether sigHex is the hex-encoded HMAC-SHA256 of
// body under secret. Comparison is constant-time (crypto/subtle) to avoid a
// timing side-channel on the signature check.
func verifySignature(secret string, body []byte, sigHex string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	got := strings.TrimSpace(sigHex)
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func (l *linearPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)

	addr, path, secret, smee := "", "/linear", "", ""
	allowUnsigned := false
	if webhook != nil {
		addr = str(webhook["listen"])
		if p := str(webhook["path"]); p != "" {
			path = p
		}
		secret = str(webhook["secret"])
		allowUnsigned = boolv(webhook["allow_unsigned"])
		smee = str(webhook["smee"])
	}
	if addr == "" && smee == "" {
		return fmt.Errorf("linear: no webhook.listen address or smee relay configured")
	}
	if strings.TrimSpace(secret) == "" {
		if !allowUnsigned {
			return fmt.Errorf("linear: no webhook.secret configured — an unsigned listener accepts any POST on the listen address as a real event. Set it, or set webhook.allow_unsigned: true if you genuinely front this with something else that authenticates")
		}
		fmt.Fprintf(os.Stderr, "linear: webhook.allow_unsigned is set — accepting UNSIGNED webhooks; anyone who can reach the listen address can fire triggers\n")
	}

	// Signature verification is done by hand (below), not by
	// sourcekit.Listener's built-in check, so ln.Secret is left empty.
	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smee}
	dedup := sourcekit.NewDedup(4096)
	if ln.Addr != "" {
		fmt.Fprintf(os.Stderr, "linear[%s]: listening on %s%s\n", req.Instance, ln.Addr, ln.Path)
	}
	if ln.Relay != "" {
		fmt.Fprintf(os.Stderr, "linear[%s]: relaying via smee channel %s\n", req.Instance, ln.Relay)
	}
	return ln.Serve(ctx, func(h http.Header, body []byte) {
		if secret != "" && !verifySignature(secret, body, h.Get("Linear-Signature")) {
			fmt.Fprintf(os.Stderr, "linear[%s]: rejected webhook: bad signature\n", req.Instance)
			return
		}
		ev := parseWebhook(body)
		if ev == nil {
			return
		}
		if !dedup.Add(ev.Dedup) {
			return
		}
		_ = emit(ev)
	})
}

// wireEvent is the normalized event streamed to the daemon's plugin source
// adapter.
type wireEvent struct {
	Event   string         `json:"event"`
	Kind    string         `json:"kind,omitempty"`
	Title   string         `json:"title,omitempty"`
	Context map[string]any `json:"context,omitempty"`
	Dedup   string         `json:"dedup,omitempty"`
}

// linearWebhook is the subset of Linear's webhook delivery shape this plugin
// reads: {action, type, data: {...}}. data's shape depends on type.
type linearWebhook struct {
	Action string          `json:"action"`
	Type   string          `json:"type"`
	Data   json.RawMessage `json:"data"`
}

type issueWebhookData struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	Priority   *int   `json:"priority"`
	URL        string `json:"url"`
	State      *struct {
		Name string `json:"name"`
	} `json:"state"`
	Assignee *struct {
		Name string `json:"name"`
	} `json:"assignee"`
	Team *struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	} `json:"team"`
}

type commentWebhookData struct {
	ID      string `json:"id"`
	Body    string `json:"body"`
	IssueID string `json:"issueId"`
	User    *struct {
		Name string `json:"name"`
	} `json:"user"`
}

type projectWebhookData struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url"`
}

func parseWebhook(body []byte) *wireEvent {
	var w linearWebhook
	if err := json.Unmarshal(body, &w); err != nil {
		return nil
	}
	if w.Action == "" || w.Type == "" || len(w.Data) == 0 {
		return nil
	}
	switch strings.ToLower(w.Type) {
	case "issue":
		return issueEvent(w)
	case "comment":
		return commentEvent(w)
	case "project":
		return projectEvent(w)
	}
	return nil
}

func issueEvent(w linearWebhook) *wireEvent {
	var d issueWebhookData
	if err := json.Unmarshal(w.Data, &d); err != nil || d.ID == "" {
		return nil
	}
	state, assignee, team := "", "", ""
	if d.State != nil {
		state = d.State.Name
	}
	if d.Assignee != nil {
		assignee = d.Assignee.Name
	}
	if d.Team != nil {
		team = nonEmpty(d.Team.Key, d.Team.Name)
	}
	priority := 0
	if d.Priority != nil {
		priority = *d.Priority
	}
	return &wireEvent{
		Event: "issue", Kind: "issue",
		Title: fmt.Sprintf("linear issue %s %s: %s", d.Identifier, w.Action, d.Title),
		Dedup: d.ID + ":" + w.Action,
		Context: map[string]any{
			"id": d.ID, "identifier": d.Identifier, "title": d.Title,
			"state": state, "priority": priority, "assignee": assignee,
			"team": team, "url": d.URL, "action": w.Action,
			"type": "Issue", "types": "Issue", "actions": w.Action,
			"states": state, "teams": team, "priorities": priority,
		},
	}
}

func commentEvent(w linearWebhook) *wireEvent {
	var d commentWebhookData
	if err := json.Unmarshal(w.Data, &d); err != nil || d.ID == "" {
		return nil
	}
	user := ""
	if d.User != nil {
		user = d.User.Name
	}
	return &wireEvent{
		Event: "comment", Kind: "comment",
		Title: fmt.Sprintf("linear comment %s by %s", w.Action, user),
		Dedup: d.ID + ":" + w.Action,
		Context: map[string]any{
			"id": d.ID, "body": d.Body, "issue_id": d.IssueID, "user": user,
			"action": w.Action, "type": "Comment", "types": "Comment", "actions": w.Action,
		},
	}
}

func projectEvent(w linearWebhook) *wireEvent {
	var d projectWebhookData
	if err := json.Unmarshal(w.Data, &d); err != nil || d.ID == "" {
		return nil
	}
	return &wireEvent{
		Event: "project", Kind: "project",
		Title: fmt.Sprintf("linear project %s %s", d.Name, w.Action),
		Dedup: d.ID + ":" + w.Action,
		Context: map[string]any{
			"id": d.ID, "name": d.Name, "state": d.State, "url": d.URL,
			"action": w.Action, "type": "Project", "types": "Project", "actions": w.Action,
			"states": d.State,
		},
	}
}

func main() {
	if err := plugin.Serve(newLinearPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-linear:", err)
		os.Exit(1)
	}
}

// --- small option/JSON helpers (stdlib only) ---

func str(v any) string { s, _ := v.(string); return s }

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

// setIfPresent copies v into m under key, skipping a nil or empty-string v so
// optional GraphQL input fields are omitted rather than sent as zero values.
func setIfPresent(m map[string]any, key string, v any) {
	if v == nil {
		return
	}
	if s, ok := v.(string); ok && s == "" {
		return
	}
	m[key] = v
}

// mapAt walks a chain of nested map[string]any keys, e.g. mapAt(data,
// "issueCreate", "issue"), returning the final map and whether every step
// resolved.
func mapAt(m map[string]any, keys ...string) (map[string]any, bool) {
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// gs reads a string field from a (possibly nil) map.
func gs(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
