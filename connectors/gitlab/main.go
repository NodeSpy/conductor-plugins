// Command conductor-gitlab is the GitLab connector as a standalone external
// conductor plugin. It exposes GitLab REST v4 as verbs (merge requests,
// issues, notes, pipelines, branches, plus a raw `api` escape hatch) over a
// personal/project access token, and as a SOURCE it receives GitLab webhook
// deliveries (Push/Merge Request/Pipeline/Issue/Note hooks), verifies the
// `X-Gitlab-Token` header, and streams a normalized event per delivery.
//
// Built ONLY against the public SDK (pkg/plugin) and the connector-kit
// (pkg/sourcekit) — no conductor internals, no third-party dependency.
//
// Connection (used for both Invoke and StartSource):
//
//	url: "https://gitlab.com"      # GitLab instance base (self-managed: your own URL); API base = url + /api/v4
//	token: "<personal/project access token>"
//	webhook:
//	  listen: ":9097"               # HTTP listener address (StartSource only; optional if smee is set)
//	  path: "/gitlab"               # request path (default /gitlab)
//	  secret: "<secret token>"      # the webhook's Secret Token, compared to X-Gitlab-Token
//	  allow_unsigned: false         # explicitly accept unauthenticated deliveries when no secret is set
//	  smee: "https://smee.io/AbC123" # smee.io-style SSE relay URL (optional; also/instead of listen)
//
// GitLab webhooks authenticate with a plain shared-secret header
// (`X-Gitlab-Token`), NOT an HMAC signature — so this source compares it with
// a constant-time equality check (crypto/subtle) rather than
// sourcekit.VerifyHMAC, and leaves sourcekit.Listener.Secret empty so the
// listener's own (HMAC-shaped) verification is never invoked; verification
// happens in this file's handler instead. It fails closed: no secret and no
// `allow_unsigned: true` refuses to start.
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
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type gitlabPlugin struct{}

// httpClient is shared across invocations; GitLab connections are stateless
// (token in a header), so no per-instance caching is needed.
var httpClient = &http.Client{Timeout: 30 * time.Second}

func (gitlabPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "gitlab",
		Desc: "GitLab: merge requests, issues, notes, and pipelines over REST v4; push/merge_request/pipeline/issue/note webhook events in.",
		Connection: plugin.Schema{
			"url":     {Type: "string", Desc: "GitLab instance base URL (default https://gitlab.com); API base is url + /api/v4"},
			"token":   {Type: "string", Desc: "personal/project access token, sent as PRIVATE-TOKEN"},
			"webhook": {Type: "map", Desc: "source transport: listen (optional if smee is set), path, secret, allow_unsigned, smee (smee.io-style SSE relay URL, e.g. https://smee.io/AbC123 — also (or instead) receive forwarded deliveries over SSE when the endpoint has no public URL)"},
		},
		Events:       gitlabEvents(),
		Verbs:        gitlabVerbs(),
		Capabilities: plugin.Capabilities{Egress: []string{"gitlab.com:443"}}, // self-managed: narrow/replace via `network:` to your instance's host:443
	}
}

// --- events ---

func baseGitlabContext() plugin.Schema {
	return plugin.Schema{
		"project": {Type: "string", Desc: "path_with_namespace"},
		"user":    {Type: "string"},
		"url":     {Type: "string"},
		"action":  {Type: "string"},
		"ref":     {Type: "string"},
		"branch":  {Type: "string"},
	}
}

func baseGitlabFilters() plugin.Schema {
	return plugin.Schema{
		"actions":         {Type: "list"},
		"states":          {Type: "list"},
		"statuses":        {Type: "list"},
		"branches":        {Type: "list"},
		"target_branches": {Type: "list"},
		"action":          {Type: "string"},
		"state":           {Type: "string"},
		"status":          {Type: "string"},
		"branch":          {Type: "string"},
		"target_branch":   {Type: "string"},
	}
}

func gitlabEvent(name, desc string, contextExtra plugin.Schema) plugin.Event {
	c := baseGitlabContext()
	for k, v := range contextExtra {
		c[k] = v
	}
	return plugin.Event{Name: name, Desc: desc, Filters: baseGitlabFilters(), Context: c}
}

func gitlabEvents() []plugin.Event {
	return []plugin.Event{
		gitlabEvent("push", "a push to a branch", nil),
		gitlabEvent("merge_request", "a merge request was opened/updated/merged/closed", plugin.Schema{
			"mr_iid":        {Type: "integer"},
			"title":         {Type: "string"},
			"state":         {Type: "string"},
			"source_branch": {Type: "string"},
			"target_branch": {Type: "string"},
		}),
		gitlabEvent("pipeline", "a pipeline changed status", plugin.Schema{
			"status":      {Type: "string"},
			"pipeline_id": {Type: "integer"},
		}),
		gitlabEvent("issue", "an issue was opened/updated/closed", plugin.Schema{
			"issue_iid": {Type: "integer"},
			"title":     {Type: "string"},
			"state":     {Type: "string"},
		}),
		gitlabEvent("note", "a comment was posted on an MR, issue, commit, or snippet", nil),
	}
}

// --- verbs ---

// stdOutputs is the request/response shape every verb (except list_mrs)
// shares: the raw parsed JSON body plus the HTTP status.
func stdOutputs() plugin.Schema {
	return plugin.Schema{
		"result":      {Type: "any"},
		"status_code": {Type: "integer"},
	}
}

func listOutputs() plugin.Schema {
	return plugin.Schema{
		"items":       {Type: "list"},
		"status_code": {Type: "integer"},
	}
}

func projectOpt() plugin.Field {
	return plugin.Field{Type: "string", Required: true, Scope: "repo", Desc: "project path_with_namespace or numeric id"}
}

func gitlabVerbs() []plugin.Verb {
	return []plugin.Verb{
		{
			Name: "comment_mr", Desc: "post a comment (note) on a merge request",
			Options: plugin.Schema{
				"project": projectOpt(),
				"mr":      {Type: "integer", Required: true, Desc: "merge request iid"},
				"body":    {Type: "string", Required: true},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "comment_issue", Desc: "post a comment (note) on an issue",
			Options: plugin.Schema{
				"project": projectOpt(),
				"issue":   {Type: "integer", Required: true, Desc: "issue iid"},
				"body":    {Type: "string", Required: true},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "create_issue", Desc: "open an issue",
			Options: plugin.Schema{
				"project":     projectOpt(),
				"title":       {Type: "string", Required: true},
				"description": {Type: "string"},
				"labels":      {Type: "list"},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "update_issue", Desc: "edit an issue: state_event (close|reopen), title, description",
			Options: plugin.Schema{
				"project":     projectOpt(),
				"issue":       {Type: "integer", Required: true},
				"state_event": {Type: "string", Enum: []string{"close", "reopen"}},
				"title":       {Type: "string"},
				"description": {Type: "string"},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "create_mr", Desc: "open a merge request",
			Options: plugin.Schema{
				"project":       projectOpt(),
				"source_branch": {Type: "string", Required: true},
				"target_branch": {Type: "string", Required: true},
				"title":         {Type: "string", Required: true},
				"description":   {Type: "string"},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "update_mr", Desc: "edit a merge request: state_event (close|reopen), title, description, target_branch",
			Options: plugin.Schema{
				"project":       projectOpt(),
				"mr":            {Type: "integer", Required: true},
				"state_event":   {Type: "string", Enum: []string{"close", "reopen"}},
				"title":         {Type: "string"},
				"description":   {Type: "string"},
				"target_branch": {Type: "string"},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "merge_mr", Desc: "merge a merge request",
			Options: plugin.Schema{
				"project":                      projectOpt(),
				"mr":                           {Type: "integer", Required: true},
				"squash":                       {Type: "boolean"},
				"merge_when_pipeline_succeeds": {Type: "boolean"},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "get_mr", Desc: "read a merge request",
			Options: plugin.Schema{
				"project": projectOpt(),
				"mr":      {Type: "integer", Required: true},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "get_issue", Desc: "read an issue",
			Options: plugin.Schema{
				"project": projectOpt(),
				"issue":   {Type: "integer", Required: true},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "list_mrs", Desc: "list merge requests: outputs items[]",
			Options: plugin.Schema{
				"project":       projectOpt(),
				"state":         {Type: "string", Desc: "opened|closed|locked|merged"},
				"target_branch": {Type: "string"},
			},
			Outputs: listOutputs(),
		},
		{
			Name: "add_labels", Desc: "add labels to an issue",
			Options: plugin.Schema{
				"project": projectOpt(),
				"issue":   {Type: "integer", Required: true},
				"labels":  {Type: "list", Required: true},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "create_branch", Desc: "create a branch from a ref",
			Options: plugin.Schema{
				"project": projectOpt(),
				"branch":  {Type: "string", Required: true, Desc: "new branch name"},
				"ref":     {Type: "string", Required: true, Desc: "source branch/tag/sha"},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "trigger_pipeline", Desc: "trigger a pipeline on a ref",
			Options: plugin.Schema{
				"project": projectOpt(),
				"ref":     {Type: "string", Required: true},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "retry_pipeline", Desc: "retry a pipeline",
			Options: plugin.Schema{
				"project":  projectOpt(),
				"pipeline": {Type: "integer", Required: true},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "cancel_pipeline", Desc: "cancel a pipeline",
			Options: plugin.Schema{
				"project":  projectOpt(),
				"pipeline": {Type: "integer", Required: true},
			},
			Outputs: stdOutputs(),
		},
		{
			Name: "api", Desc: "raw GitLab REST v4 escape hatch: method + path (relative to /api/v4) + query + body",
			Options: plugin.Schema{
				"method": {Type: "string", Desc: "default GET"},
				"path":   {Type: "string", Required: true, Desc: "path relative to /api/v4, e.g. /projects/123/issues"},
				"query":  {Type: "map"},
				"body":   {Type: "any"},
			},
			Outputs: stdOutputs(),
		},
	}
}

func (gitlabPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	base := apiBase(req.Connection)
	token := str(req.Connection["token"])

	method, path, query, body, err := buildRequest(req.Verb, req.Options)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}

	status, result, err := doRequest(base, token, method, path, query, body)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}

	out := map[string]any{"status_code": status}
	if req.Verb == "list_mrs" {
		items, _ := result.([]any)
		out["items"] = items
	} else {
		out["result"] = result
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// apiBase resolves the REST v4 API base URL: conn `url` (default
// https://gitlab.com) + "/api/v4". Overridable in tests by pointing `url` at
// an httptest.Server.
func apiBase(conn map[string]any) string {
	u := strOr(conn["url"], "https://gitlab.com")
	return strings.TrimRight(u, "/") + "/api/v4"
}

// buildRequest turns one verb call into an HTTP method/path/query/body. Pure
// and hermetically testable — no request is sent here.
func buildRequest(verb string, o map[string]any) (method, path string, query url.Values, body map[string]any, err error) {
	project := str(o["project"])
	if verb != "api" && project == "" {
		return "", "", nil, nil, fmt.Errorf("project is required")
	}
	pp := projectPath(project)

	switch verb {
	case "comment_mr":
		mr := toInt(o["mr"])
		b := str(o["body"])
		if mr == 0 {
			return "", "", nil, nil, fmt.Errorf("mr is required")
		}
		if b == "" {
			return "", "", nil, nil, fmt.Errorf("body is required")
		}
		return "POST", fmt.Sprintf("/projects/%s/merge_requests/%d/notes", pp, mr), nil, map[string]any{"body": b}, nil

	case "comment_issue":
		issue := toInt(o["issue"])
		b := str(o["body"])
		if issue == 0 {
			return "", "", nil, nil, fmt.Errorf("issue is required")
		}
		if b == "" {
			return "", "", nil, nil, fmt.Errorf("body is required")
		}
		return "POST", fmt.Sprintf("/projects/%s/issues/%d/notes", pp, issue), nil, map[string]any{"body": b}, nil

	case "create_issue":
		title := str(o["title"])
		if title == "" {
			return "", "", nil, nil, fmt.Errorf("title is required")
		}
		b := map[string]any{"title": title}
		if d := str(o["description"]); d != "" {
			b["description"] = d
		}
		if labels := strList(o["labels"]); len(labels) > 0 {
			b["labels"] = strings.Join(labels, ",")
		}
		return "POST", fmt.Sprintf("/projects/%s/issues", pp), nil, b, nil

	case "update_issue":
		issue := toInt(o["issue"])
		if issue == 0 {
			return "", "", nil, nil, fmt.Errorf("issue is required")
		}
		b := map[string]any{}
		if s := str(o["state_event"]); s != "" {
			b["state_event"] = s
		}
		if t := str(o["title"]); t != "" {
			b["title"] = t
		}
		if d := str(o["description"]); d != "" {
			b["description"] = d
		}
		return "PUT", fmt.Sprintf("/projects/%s/issues/%d", pp, issue), nil, b, nil

	case "create_mr":
		src, tgt, title := str(o["source_branch"]), str(o["target_branch"]), str(o["title"])
		if src == "" {
			return "", "", nil, nil, fmt.Errorf("source_branch is required")
		}
		if tgt == "" {
			return "", "", nil, nil, fmt.Errorf("target_branch is required")
		}
		if title == "" {
			return "", "", nil, nil, fmt.Errorf("title is required")
		}
		b := map[string]any{"source_branch": src, "target_branch": tgt, "title": title}
		if d := str(o["description"]); d != "" {
			b["description"] = d
		}
		return "POST", fmt.Sprintf("/projects/%s/merge_requests", pp), nil, b, nil

	case "update_mr":
		mr := toInt(o["mr"])
		if mr == 0 {
			return "", "", nil, nil, fmt.Errorf("mr is required")
		}
		b := map[string]any{}
		if s := str(o["state_event"]); s != "" {
			b["state_event"] = s
		}
		if t := str(o["title"]); t != "" {
			b["title"] = t
		}
		if d := str(o["description"]); d != "" {
			b["description"] = d
		}
		if t := str(o["target_branch"]); t != "" {
			b["target_branch"] = t
		}
		return "PUT", fmt.Sprintf("/projects/%s/merge_requests/%d", pp, mr), nil, b, nil

	case "merge_mr":
		mr := toInt(o["mr"])
		if mr == 0 {
			return "", "", nil, nil, fmt.Errorf("mr is required")
		}
		b := map[string]any{}
		if v, ok := o["squash"]; ok {
			b["squash"] = boolv(v)
		}
		if v, ok := o["merge_when_pipeline_succeeds"]; ok {
			b["merge_when_pipeline_succeeds"] = boolv(v)
		}
		return "PUT", fmt.Sprintf("/projects/%s/merge_requests/%d/merge", pp, mr), nil, b, nil

	case "get_mr":
		mr := toInt(o["mr"])
		if mr == 0 {
			return "", "", nil, nil, fmt.Errorf("mr is required")
		}
		return "GET", fmt.Sprintf("/projects/%s/merge_requests/%d", pp, mr), nil, nil, nil

	case "get_issue":
		issue := toInt(o["issue"])
		if issue == 0 {
			return "", "", nil, nil, fmt.Errorf("issue is required")
		}
		return "GET", fmt.Sprintf("/projects/%s/issues/%d", pp, issue), nil, nil, nil

	case "list_mrs":
		q := url.Values{}
		if s := str(o["state"]); s != "" {
			q.Set("state", s)
		}
		if t := str(o["target_branch"]); t != "" {
			q.Set("target_branch", t)
		}
		return "GET", fmt.Sprintf("/projects/%s/merge_requests", pp), q, nil, nil

	case "add_labels":
		issue := toInt(o["issue"])
		labels := strList(o["labels"])
		if issue == 0 {
			return "", "", nil, nil, fmt.Errorf("issue is required")
		}
		if len(labels) == 0 {
			return "", "", nil, nil, fmt.Errorf("labels is required")
		}
		return "PUT", fmt.Sprintf("/projects/%s/issues/%d", pp, issue), nil, map[string]any{"add_labels": strings.Join(labels, ",")}, nil

	case "create_branch":
		branch, ref := str(o["branch"]), str(o["ref"])
		if branch == "" {
			return "", "", nil, nil, fmt.Errorf("branch is required")
		}
		if ref == "" {
			return "", "", nil, nil, fmt.Errorf("ref is required")
		}
		q := url.Values{"branch": {branch}, "ref": {ref}}
		return "POST", fmt.Sprintf("/projects/%s/repository/branches", pp), q, nil, nil

	case "trigger_pipeline":
		ref := str(o["ref"])
		if ref == "" {
			return "", "", nil, nil, fmt.Errorf("ref is required")
		}
		q := url.Values{"ref": {ref}}
		return "POST", fmt.Sprintf("/projects/%s/pipeline", pp), q, nil, nil

	case "retry_pipeline":
		pipe := toInt(o["pipeline"])
		if pipe == 0 {
			return "", "", nil, nil, fmt.Errorf("pipeline is required")
		}
		return "POST", fmt.Sprintf("/projects/%s/pipelines/%d/retry", pp, pipe), nil, nil, nil

	case "cancel_pipeline":
		pipe := toInt(o["pipeline"])
		if pipe == 0 {
			return "", "", nil, nil, fmt.Errorf("pipeline is required")
		}
		return "POST", fmt.Sprintf("/projects/%s/pipelines/%d/cancel", pp, pipe), nil, nil, nil

	case "api":
		p := str(o["path"])
		if p == "" {
			return "", "", nil, nil, fmt.Errorf("path is required")
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		m := strOr(o["method"], "GET")
		var q url.Values
		if qm, ok := o["query"].(map[string]any); ok && len(qm) > 0 {
			q = url.Values{}
			for k, v := range qm {
				q.Set(k, fmt.Sprintf("%v", v))
			}
		}
		var b map[string]any
		if bm, ok := o["body"].(map[string]any); ok {
			b = bm
		}
		return m, p, q, b, nil
	}
	return "", "", nil, nil, fmt.Errorf("unknown verb")
}

// projectPath percent-encodes a project path_with_namespace (or numeric id)
// for use as GitLab's :id path parameter, per GitLab API convention.
func projectPath(project string) string {
	return url.PathEscape(project)
}

// doRequest performs one GitLab REST v4 call. A non-2xx response is returned
// as an error carrying the status and response body verbatim; the caller
// wraps it as plugin.CodeInternalError.
func doRequest(base, token, method, path string, query url.Values, body map[string]any) (int, any, error) {
	full := base + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("gitlab: encode request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("gitlab: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("gitlab: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil, fmt.Errorf("gitlab: %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed any
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("gitlab: parse response: %w", err)
		}
	}
	return resp.StatusCode, parsed, nil
}

// --- source ---

func (gitlabPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	addr := str(webhook["listen"])
	path := strOr(webhook["path"], "/gitlab")
	secret := str(webhook["secret"])
	allowUnsigned := boolv(webhook["allow_unsigned"])
	smee := str(webhook["smee"])

	if addr == "" && smee == "" {
		return fmt.Errorf("gitlab: no webhook.listen address or smee relay configured")
	}
	if err := requireWebhookSecret(secret, allowUnsigned); err != nil {
		return err
	}

	// Left deliberately empty: GitLab's webhook auth is a plain shared-secret
	// header (X-Gitlab-Token), not an HMAC signature, so sourcekit.Listener's
	// own (HMAC-shaped) verification must not run here — this handler verifies
	// the header itself, by constant-time equality.
	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smee}

	dedup := sourcekit.NewDedup(4096)
	if addr != "" {
		fmt.Fprintf(os.Stderr, "gitlab[%s]: listening on %s%s\n", req.Instance, addr, path)
	}
	if smee != "" {
		fmt.Fprintf(os.Stderr, "gitlab[%s]: relaying via smee channel %s\n", req.Instance, smee)
	}
	return ln.Serve(ctx, func(h http.Header, body []byte) {
		if secret != "" && !constantTimeEqual(h.Get("X-Gitlab-Token"), secret) {
			return
		}
		for _, ev := range parseWebhook(body) {
			if ev.dedup != "" && !dedup.Add(ev.dedup) {
				continue
			}
			_ = emit(ev.toMap())
		}
	})
}

// requireWebhookSecret refuses to start an unauthenticated webhook listener.
//
// A missing secret is far more often a mistake than a choice, so it fails
// closed; `webhook.allow_unsigned: true` is the explicit, greppable way to
// say you meant it.
func requireWebhookSecret(secret string, allowUnsigned bool) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintf(os.Stderr, "gitlab: allow_unsigned is set — accepting UNSIGNED webhooks; anyone who can reach the listen address can fire triggers\n")
		return nil
	}
	return fmt.Errorf("gitlab: no webhook secret configured (webhook.secret) — an unsigned listener accepts any POST on the listen address as a real event. Set it, or set `webhook.allow_unsigned: true` if you genuinely front this with something else that authenticates")
}

// constantTimeEqual compares the X-Gitlab-Token header to the configured
// secret in constant time (crypto/subtle), per GitLab's plain shared-secret
// scheme (no HMAC is computed by GitLab for webhooks).
func constantTimeEqual(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// wireEvent is the normalized event streamed to the daemon.
type wireEvent struct {
	event, kind, title, dedup string
	context                   map[string]any
}

func (e wireEvent) toMap() map[string]any {
	return map[string]any{"event": e.event, "kind": e.kind, "title": e.title, "dedup": e.dedup, "context": e.context}
}

func parseWebhook(body []byte) []wireEvent {
	var probe struct {
		ObjectKind string `json:"object_kind"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil
	}
	switch probe.ObjectKind {
	case "push":
		return pushEvents(body)
	case "merge_request":
		return mergeRequestEvents(body)
	case "pipeline":
		return pipelineEvents(body)
	case "issue":
		return issueEvents(body)
	case "note":
		return noteEvents(body)
	}
	return nil
}

type glProject struct {
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
}

type glUser struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

func userOf(u glUser, fallbackName, fallbackUsername string) string {
	return nonEmpty(u.Username, u.Name, fallbackUsername, fallbackName)
}

type pushPayload struct {
	Ref          string    `json:"ref"`
	After        string    `json:"after"`
	UserName     string    `json:"user_name"`
	UserUsername string    `json:"user_username"`
	Project      glProject `json:"project"`
}

func pushEvents(body []byte) []wireEvent {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	repo := p.Project.PathWithNamespace
	branch := strings.TrimPrefix(p.Ref, "refs/heads/")
	user := nonEmpty(p.UserUsername, p.UserName)
	return []wireEvent{{
		event: "push", kind: "push",
		title: fmt.Sprintf("push to %s on %s", branch, repo),
		dedup: fmt.Sprintf("push:%s:%s:%s", repo, branch, p.After),
		context: map[string]any{
			"project": repo, "user": user, "url": p.Project.WebURL,
			"action": "push", "ref": p.Ref, "branch": branch,
			"actions": []string{"push"}, "branches": []string{branch},
		},
	}}
}

type mrPayload struct {
	User             glUser    `json:"user"`
	Project          glProject `json:"project"`
	ObjectAttributes struct {
		IID          int    `json:"iid"`
		Title        string `json:"title"`
		State        string `json:"state"`
		Action       string `json:"action"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		URL          string `json:"url"`
	} `json:"object_attributes"`
}

func mergeRequestEvents(body []byte) []wireEvent {
	var p mrPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	oa := p.ObjectAttributes
	repo := p.Project.PathWithNamespace
	user := userOf(p.User, "", "")
	return []wireEvent{{
		event: "merge_request", kind: "merge_request",
		title: fmt.Sprintf("MR !%d %s on %s", oa.IID, oa.Action, repo),
		dedup: fmt.Sprintf("mr:%s:%d:%s", repo, oa.IID, oa.Action),
		context: map[string]any{
			"project": repo, "user": user, "url": oa.URL, "action": oa.Action,
			"ref": oa.SourceBranch, "branch": oa.SourceBranch,
			"mr_iid": oa.IID, "title": oa.Title, "state": oa.State,
			"source_branch": oa.SourceBranch, "target_branch": oa.TargetBranch,
			"actions": []string{oa.Action}, "states": []string{oa.State},
			"branches": []string{oa.SourceBranch}, "target_branches": []string{oa.TargetBranch},
		},
	}}
}

type pipelinePayload struct {
	User             glUser    `json:"user"`
	Project          glProject `json:"project"`
	ObjectAttributes struct {
		ID     int    `json:"id"`
		Ref    string `json:"ref"`
		Status string `json:"status"`
	} `json:"object_attributes"`
}

func pipelineEvents(body []byte) []wireEvent {
	var p pipelinePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	oa := p.ObjectAttributes
	repo := p.Project.PathWithNamespace
	user := userOf(p.User, "", "")
	url := strings.TrimRight(p.Project.WebURL, "/") + fmt.Sprintf("/-/pipelines/%d", oa.ID)
	return []wireEvent{{
		event: "pipeline", kind: "pipeline",
		title: fmt.Sprintf("pipeline %s on %s (%s)", oa.Status, repo, oa.Ref),
		dedup: fmt.Sprintf("pipeline:%s:%d:%s", repo, oa.ID, oa.Status),
		context: map[string]any{
			"project": repo, "user": user, "url": url, "action": oa.Status,
			"ref": oa.Ref, "branch": oa.Ref,
			"status": oa.Status, "pipeline_id": oa.ID,
			"statuses": []string{oa.Status}, "branches": []string{oa.Ref},
		},
	}}
}

type issuePayload struct {
	User             glUser    `json:"user"`
	Project          glProject `json:"project"`
	ObjectAttributes struct {
		IID    int    `json:"iid"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Action string `json:"action"`
		URL    string `json:"url"`
	} `json:"object_attributes"`
}

func issueEvents(body []byte) []wireEvent {
	var p issuePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	oa := p.ObjectAttributes
	repo := p.Project.PathWithNamespace
	user := userOf(p.User, "", "")
	return []wireEvent{{
		event: "issue", kind: "issue",
		title: fmt.Sprintf("issue #%d %s on %s", oa.IID, oa.Action, repo),
		dedup: fmt.Sprintf("issue:%s:%d:%s", repo, oa.IID, oa.Action),
		context: map[string]any{
			"project": repo, "user": user, "url": oa.URL, "action": oa.Action,
			"issue_iid": oa.IID, "title": oa.Title, "state": oa.State,
			"actions": []string{oa.Action}, "states": []string{oa.State},
		},
	}}
}

type notePayload struct {
	User             glUser    `json:"user"`
	Project          glProject `json:"project"`
	ObjectAttributes struct {
		Note         string `json:"note"`
		URL          string `json:"url"`
		NoteableType string `json:"noteable_type"`
	} `json:"object_attributes"`
}

func noteEvents(body []byte) []wireEvent {
	var p notePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	oa := p.ObjectAttributes
	repo := p.Project.PathWithNamespace
	user := userOf(p.User, "", "")
	return []wireEvent{{
		event: "note", kind: "note",
		title: fmt.Sprintf("note by %s on %s", user, repo),
		dedup: fmt.Sprintf("note:%s:%s", repo, oa.URL),
		context: map[string]any{
			"project": repo, "user": user, "url": oa.URL, "action": "note",
			"actions": []string{"note"},
		},
	}}
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

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
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

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func main() {
	if err := plugin.Serve(gitlabPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-gitlab:", err)
		os.Exit(1)
	}
}
