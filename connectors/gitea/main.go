// Command conductor-gitea is the Gitea/Forgejo connector as an external
// conductor plugin (#59). It drives a self-hosted Gitea (or Forgejo — the two
// share the same v1 API surface) instance's REST API for verbs — issues, pull
// requests, releases, branches, and file contents — and, as a source, receives
// Gitea webhook deliveries (HMAC-SHA256 over the raw body, hex-encoded, in
// X-Gitea-Signature) and streams a normalized event per delivery for push,
// pull_request, issues, and issue_comment.
//
// Built ONLY against the public SDK (pkg/plugin) and the connector-kit
// (pkg/sourcekit) — no conductor internals, no third-party dependencies.
//
// Connection (used for both Invoke and StartSource):
//
//	url:   "https://gitea.example.com" # REQUIRED: base of the instance; API base is url + /api/v1
//	token: "<access token>"            # sent as Authorization: token <token>
//	webhook:
//	  listen: ":9097"                  # HTTP listener address (StartSource only)
//	  path:   "/gitea"                 # request path (default /gitea)
//	  secret: "<webhook secret>"       # HMAC secret configured on the webhook
//	  allow_unsigned: false            # explicitly accept unauthenticated deliveries when no secret is set
//
// Gitea signs webhook deliveries with HMAC-SHA256 over the raw body,
// hex-encoded, in X-Gitea-Signature — exactly the bare-hex shape
// sourcekit.VerifyHMAC already accepts (no v1=/sha256= prefix to strip), so
// this source hands verification straight to sourcekit.Listener rather than
// hand-rolling crypto/hmac here. It fails closed: no secret and no
// `webhook.allow_unsigned: true` refuses to start.
//
// The Gitea host is operator-specific and self-hosted, so this plugin
// declares NO egress in its capability manifest — the operator is expected to
// scope `network:` on the connector instance to their own Gitea/Forgejo host
// (see docs/connectors/gitea.md).
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type giteaPlugin struct{}

func (giteaPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "gitea",
		Desc: "Gitea (and Forgejo — same v1 API) self-hosted git forge: issues, pull requests, releases, branches, and file contents as verbs; push/PR/issue/comment events in via webhook.",
		Connection: plugin.Schema{
			"url":     {Type: "string", Required: true, Desc: "base URL of the Gitea/Forgejo instance, e.g. https://gitea.example.com (API base is url + /api/v1)"},
			"token":   {Type: "string", Desc: "access token; sent as Authorization: token <token>"},
			"webhook": {Type: "map", Desc: "source transport: listen, path (default /gitea), secret, allow_unsigned"},
		},
		Events: giteaEvents(),
		Verbs:  giteaVerbs(),
		// The Gitea/Forgejo host is operator-specific and self-hosted — there is
		// no fixed hostname this plugin can declare the way api.github.com is
		// fixed for the github connector. An empty manifest is the honest
		// declaration; the operator MUST narrow `network:` on the connector
		// instance to their own instance's host themselves (documented in
		// docs/connectors/gitea.md).
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

// --- events -----------------------------------------------------------

// giteaFilters/giteaContext are the shared filter and context schema
// fragments every event exposes: list-contains keys (actions/states/branches)
// alongside scalar-equality aliases (action/state/branch), mirroring the
// bundled connectors' plural-list + singular-scalar filter vocabulary.
func giteaFilters() plugin.Schema {
	return plugin.Schema{
		"actions":  {Type: "list", Desc: "webhook actions to match, e.g. opened, closed (empty = any)"},
		"action":   {Type: "string"},
		"states":   {Type: "list", Desc: "open, closed (empty = any)"},
		"state":    {Type: "string"},
		"branches": {Type: "list", Desc: "branch names to match (empty = any)"},
		"branch":   {Type: "string"},
	}
}

func giteaContext(extra plugin.Schema) plugin.Schema {
	c := plugin.Schema{
		"repo":   {Type: "string", Desc: "owner/name"},
		"action": {Type: "string"},
		"ref":    {Type: "string"},
		"branch": {Type: "string"},
		"sender": {Type: "string"},
		"url":    {Type: "string"},
	}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func giteaEvents() []plugin.Event {
	return []plugin.Event{
		{
			Name:    "push",
			Desc:    "commits were pushed to a branch",
			Filters: giteaFilters(),
			Context: giteaContext(nil),
		},
		{
			Name:    "pull_request",
			Desc:    "a pull request was opened, closed, or otherwise changed",
			Filters: giteaFilters(),
			Context: giteaContext(plugin.Schema{
				"pr_number": {Type: "integer"}, "title": {Type: "string"},
				"state": {Type: "string"}, "base": {Type: "string"}, "head": {Type: "string"},
				"merged": {Type: "boolean"},
			}),
		},
		{
			Name:    "issues",
			Desc:    "an issue was opened, closed, or otherwise changed",
			Filters: giteaFilters(),
			Context: giteaContext(plugin.Schema{
				"issue_number": {Type: "integer"}, "title": {Type: "string"}, "state": {Type: "string"},
			}),
		},
		{
			Name:    "issue_comment",
			Desc:    "a comment was added to an issue or pull request",
			Filters: giteaFilters(),
			Context: giteaContext(plugin.Schema{
				"comment_body": {Type: "string"}, "issue_number": {Type: "integer"},
			}),
		},
	}
}

func (giteaPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	if conn == nil {
		conn = map[string]any{}
	}
	if strings.TrimSpace(str(conn["url"])) == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.url is required (e.g. https://gitea.example.com)")
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	rb, err := buildRequest(req.Verb, o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}

	status, raw, err := doRequest(context.Background(), conn, rb.method, rb.path, rb.query, rb.body)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	if status < 200 || status >= 300 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError,
			fmt.Sprintf("%s: %s %s: %d: %s", req.Verb, rb.method, rb.path, status, strings.TrimSpace(string(raw))))
	}

	outputs := map[string]any{"status_code": status}
	if rb.list {
		outputs["items"] = parseJSON(raw)
	} else {
		outputs["result"] = parseJSON(raw)
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

// --- verb declarations --------------------------------------------------

func giteaVerbs() []plugin.Verb {
	objOut := plugin.Schema{"status_code": {Type: "integer"}, "result": {Type: "any"}}
	listOut := plugin.Schema{"status_code": {Type: "integer"}, "items": {Type: "list"}}
	repoOpt := plugin.Field{Type: "string", Required: true, Scope: "repo", Desc: "owner/name"}
	return []plugin.Verb{
		{
			Name: "comment_issue", Desc: "post a comment on an issue or pull request",
			Options: plugin.Schema{
				"repo": repoOpt, "index": {Type: "integer", Required: true, Desc: "issue or PR number — Gitea shares the numbering"},
				"body": {Type: "string", Required: true},
			},
			Outputs: objOut,
		},
		{
			Name: "create_issue", Desc: "open an issue",
			Options: plugin.Schema{
				"repo": repoOpt, "title": {Type: "string", Required: true},
				"body":      {Type: "string"},
				"labels":    {Type: "list", Desc: "label ids"},
				"assignees": {Type: "list", Desc: "logins to assign"},
			},
			Outputs: objOut,
		},
		{
			Name: "update_issue", Desc: "edit an issue: state (open|closed), title, body",
			Options: plugin.Schema{
				"repo": repoOpt, "index": {Type: "integer", Required: true},
				"state": {Type: "string", Enum: []string{"open", "closed"}},
				"title": {Type: "string"}, "body": {Type: "string"},
			},
			Outputs: objOut,
		},
		{
			Name: "get_issue", Desc: "read an issue or pull request by number",
			Options: plugin.Schema{"repo": repoOpt, "index": {Type: "integer", Required: true}},
			Outputs: objOut,
		},
		{
			Name: "list_issues", Desc: "list issues in a repo",
			Options: plugin.Schema{
				"repo":  repoOpt,
				"state": {Type: "string", Desc: "open|closed|all (default open)"},
				"type":  {Type: "string", Enum: []string{"issues", "pulls"}, Desc: "filter to issues or pulls"},
				"page":  {Type: "integer"}, "limit": {Type: "integer"},
			},
			Outputs: listOut,
		},
		{
			Name: "create_pr", Desc: "open a pull request",
			Options: plugin.Schema{
				"repo":  repoOpt,
				"head":  {Type: "string", Required: true, Desc: "the branch with your changes"},
				"base":  {Type: "string", Required: true, Desc: "the branch to merge into"},
				"title": {Type: "string", Required: true},
				"body":  {Type: "string"},
			},
			Outputs: objOut,
		},
		{
			Name: "merge_pr", Desc: "merge a pull request",
			Options: plugin.Schema{
				"repo": repoOpt, "index": {Type: "integer", Required: true},
				"do": {Type: "string", Enum: []string{"merge", "rebase", "squash"}, Desc: "merge strategy (default merge)"},
			},
			Outputs: objOut,
		},
		{
			Name: "get_pr", Desc: "read a pull request by number",
			Options: plugin.Schema{"repo": repoOpt, "index": {Type: "integer", Required: true}},
			Outputs: objOut,
		},
		{
			Name: "add_labels", Desc: "add labels to an issue or pull request",
			Options: plugin.Schema{
				"repo": repoOpt, "index": {Type: "integer", Required: true},
				"labels": {Type: "list", Required: true, Desc: "label ids"},
			},
			Outputs: objOut,
		},
		{
			Name: "create_release", Desc: "publish a release for a tag",
			Options: plugin.Schema{
				"repo": repoOpt, "tag_name": {Type: "string", Required: true},
				"name": {Type: "string"}, "body": {Type: "string"},
				"draft": {Type: "boolean"}, "prerelease": {Type: "boolean"},
			},
			Outputs: objOut,
		},
		{
			Name: "create_branch", Desc: "create a branch from another branch",
			Options: plugin.Schema{
				"repo": repoOpt, "new_branch_name": {Type: "string", Required: true},
				"old_branch_name": {Type: "string", Desc: "source branch (default: the repo's default branch)"},
			},
			Outputs: objOut,
		},
		{
			Name: "put_file", Desc: "create or update a file in one commit",
			Options: plugin.Schema{
				"repo": repoOpt, "path": {Type: "string", Required: true, Desc: "repo-relative file path"},
				"content": {Type: "string", Required: true, Desc: "file content — base64 already, or raw text (auto-encoded)"},
				"message": {Type: "string", Required: true, Desc: "commit message"},
				"branch":  {Type: "string", Desc: "branch to commit on (default: the repo's default branch)"},
			},
			Outputs: objOut,
		},
		{
			Name: "api", Desc: "call any Gitea v1 API endpoint not covered by a first-class verb",
			Options: plugin.Schema{
				"method": {Type: "string", Required: true, Enum: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
				"path":   {Type: "string", Required: true, Desc: "path under /api/v1, e.g. /repos/acme/app/topics"},
				"query":  {Type: "map"}, "body": {Type: "any"},
			},
			Outputs: objOut,
		},
	}
}

// --- request building (pure, hermetically testable — no network) -------

// reqBuild is what one verb call resolves to: an HTTP method, a path under
// the API base, optional query parameters, an optional JSON body, and
// whether the response is a list (→ outputs.items) or a single object (→
// outputs.result).
type reqBuild struct {
	method string
	path   string
	query  url.Values
	body   any
	list   bool
}

func buildRequest(verb string, o map[string]any) (reqBuild, error) {
	switch verb {
	case "comment_issue":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		index, err := requiredInt(o, "index")
		if err != nil {
			return reqBuild{}, err
		}
		body, err := requiredStr(o, "body")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{
			method: http.MethodPost,
			path:   fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, index),
			body:   map[string]any{"body": body},
		}, nil

	case "create_issue":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		title, err := requiredStr(o, "title")
		if err != nil {
			return reqBuild{}, err
		}
		b := map[string]any{"title": title}
		if v := str(o["body"]); v != "" {
			b["body"] = v
		}
		if ids := intList(o["labels"]); len(ids) > 0 {
			b["labels"] = ids
		}
		if logins := strList(o["assignees"]); len(logins) > 0 {
			b["assignees"] = logins
		}
		return reqBuild{method: http.MethodPost, path: fmt.Sprintf("/repos/%s/%s/issues", owner, repo), body: b}, nil

	case "update_issue":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		index, err := requiredInt(o, "index")
		if err != nil {
			return reqBuild{}, err
		}
		b := map[string]any{}
		if v := str(o["state"]); v != "" {
			b["state"] = v
		}
		if v := str(o["title"]); v != "" {
			b["title"] = v
		}
		if v, ok := o["body"]; ok {
			b["body"] = v
		}
		return reqBuild{method: http.MethodPatch, path: fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, index), body: b}, nil

	case "get_issue":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		index, err := requiredInt(o, "index")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, index)}, nil

	case "list_issues":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		q := url.Values{}
		if v := str(o["state"]); v != "" {
			q.Set("state", v)
		}
		if v := str(o["type"]); v != "" {
			q.Set("type", v)
		}
		if v := intStr(o["page"]); v != "" {
			q.Set("page", v)
		}
		if v := intStr(o["limit"]); v != "" {
			q.Set("limit", v)
		}
		return reqBuild{method: http.MethodGet, path: fmt.Sprintf("/repos/%s/%s/issues", owner, repo), query: q, list: true}, nil

	case "create_pr":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		head, err := requiredStr(o, "head")
		if err != nil {
			return reqBuild{}, err
		}
		base, err := requiredStr(o, "base")
		if err != nil {
			return reqBuild{}, err
		}
		title, err := requiredStr(o, "title")
		if err != nil {
			return reqBuild{}, err
		}
		b := map[string]any{"head": head, "base": base, "title": title}
		if v := str(o["body"]); v != "" {
			b["body"] = v
		}
		return reqBuild{method: http.MethodPost, path: fmt.Sprintf("/repos/%s/%s/pulls", owner, repo), body: b}, nil

	case "merge_pr":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		index, err := requiredInt(o, "index")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{
			method: http.MethodPost,
			path:   fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, index),
			body:   map[string]any{"do": strOr(o["do"], "merge")},
		}, nil

	case "get_pr":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		index, err := requiredInt(o, "index")
		if err != nil {
			return reqBuild{}, err
		}
		return reqBuild{method: http.MethodGet, path: fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, index)}, nil

	case "add_labels":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		index, err := requiredInt(o, "index")
		if err != nil {
			return reqBuild{}, err
		}
		ids := intList(o["labels"])
		if len(ids) == 0 {
			return reqBuild{}, fmt.Errorf("labels is required")
		}
		return reqBuild{
			method: http.MethodPost,
			path:   fmt.Sprintf("/repos/%s/%s/issues/%d/labels", owner, repo, index),
			body:   map[string]any{"labels": ids},
		}, nil

	case "create_release":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		tag, err := requiredStr(o, "tag_name")
		if err != nil {
			return reqBuild{}, err
		}
		b := map[string]any{"tag_name": tag}
		if v := str(o["name"]); v != "" {
			b["name"] = v
		}
		if v := str(o["body"]); v != "" {
			b["body"] = v
		}
		if boolv(o["draft"]) {
			b["draft"] = true
		}
		if boolv(o["prerelease"]) {
			b["prerelease"] = true
		}
		return reqBuild{method: http.MethodPost, path: fmt.Sprintf("/repos/%s/%s/releases", owner, repo), body: b}, nil

	case "create_branch":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		newBranch, err := requiredStr(o, "new_branch_name")
		if err != nil {
			return reqBuild{}, err
		}
		b := map[string]any{"new_branch_name": newBranch}
		if v := str(o["old_branch_name"]); v != "" {
			b["old_branch_name"] = v
		}
		return reqBuild{method: http.MethodPost, path: fmt.Sprintf("/repos/%s/%s/branches", owner, repo), body: b}, nil

	case "put_file":
		owner, repo, err := repoParts(o)
		if err != nil {
			return reqBuild{}, err
		}
		path, err := requiredStr(o, "path")
		if err != nil {
			return reqBuild{}, err
		}
		content, err := requiredStr(o, "content")
		if err != nil {
			return reqBuild{}, err
		}
		message, err := requiredStr(o, "message")
		if err != nil {
			return reqBuild{}, err
		}
		b := map[string]any{"content": toBase64Content(content), "message": message}
		if v := str(o["branch"]); v != "" {
			b["branch"] = v
		}
		return reqBuild{
			method: http.MethodPut,
			path:   fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, strings.TrimPrefix(path, "/")),
			body:   b,
		}, nil

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
		return reqBuild{
			method: strings.ToUpper(method),
			path:   "/" + strings.TrimPrefix(path, "/"),
			query:  q,
			body:   o["body"],
		}, nil
	}
	return reqBuild{}, fmt.Errorf("unknown verb %q", verb)
}

// --- HTTP transport ------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

// apiBase derives the API base (rawURL + /api/v1) from the connection's url —
// kept as its own function so a test can point it at an httptest.Server.
func apiBase(rawURL string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if u == "" {
		return "", fmt.Errorf("connection.url is required (e.g. https://gitea.example.com)")
	}
	return u + "/api/v1", nil
}

func doRequest(ctx context.Context, conn map[string]any, method, path string, query url.Values, body any) (status int, raw []byte, err error) {
	base, err := apiBase(str(conn["url"]))
	if err != nil {
		return 0, nil, err
	}
	full := base + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reader *bytes.Reader
	if body != nil {
		b, merr := json.Marshal(body)
		if merr != nil {
			return 0, nil, merr
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := str(conn["token"]); token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err = readAll(resp)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

func readAll(resp *http.Response) ([]byte, error) {
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseJSON(raw []byte) any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// --- source (webhooks) --------------------------------------------------

func (giteaPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	addr, path, secret, allowUnsigned := "", "/gitea", "", false
	if webhook != nil {
		addr = str(webhook["listen"])
		if p := str(webhook["path"]); p != "" {
			path = p
		}
		secret = str(webhook["secret"])
		allowUnsigned = boolv(webhook["allow_unsigned"])
	}
	// sourcekit.VerifyHMAC's bare-hex branch (no v1=/sha256= prefix present to
	// trim) is exactly Gitea's X-Gitea-Signature format: hex-encoded
	// HMAC-SHA256 over the raw body, nothing else — so the shared listener
	// verifies it with no custom crypto needed here.
	ln := sourcekit.Listener{Addr: addr, Path: path, Secret: secret, SigHeader: "X-Gitea-Signature"}
	if ln.Addr == "" {
		return fmt.Errorf("gitea: no webhook.listen address configured")
	}
	if err := requireWebhookSecret(ln.Secret, allowUnsigned); err != nil {
		return err
	}
	dedup := sourcekit.NewDedup(2048)
	fmt.Fprintf(os.Stderr, "gitea[%s]: listening on %s%s\n", req.Instance, ln.Addr, ln.Path)
	return ln.Serve(ctx, func(h http.Header, body []byte) {
		event := parseWebhook(h.Get("X-Gitea-Event"), body)
		if event == nil {
			return
		}
		if dk, _ := event["dedup"].(string); dk != "" && !dedup.Add(dk) {
			return
		}
		_ = emit(event)
	})
}

// giteaWire is the subset of Gitea's webhook payload shapes this plugin
// reads across push/pull_request/issues/issue_comment deliveries.
type giteaWire struct {
	Action     string `json:"action"`
	Ref        string `json:"ref"`
	Before     string `json:"before"`
	After      string `json:"after"`
	CompareURL string `json:"compare_url"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
	Sender struct {
		Login    string `json:"login"`
		UserName string `json:"username"`
	} `json:"sender"`
	Pusher *struct {
		Login    string `json:"login"`
		UserName string `json:"username"`
	} `json:"pusher"`
	PullRequest *struct {
		Number  int64  `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		Base    struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
	Issue *struct {
		Number  int64  `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
	} `json:"issue"`
	Comment *struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	} `json:"comment"`
}

func parseWebhook(kind string, body []byte) map[string]any {
	var w giteaWire
	if err := json.Unmarshal(body, &w); err != nil {
		return nil
	}
	repo := w.Repository.FullName
	sender := nonEmpty(w.Sender.Login, w.Sender.UserName)
	switch kind {
	case "push":
		return pushEvent(repo, sender, w)
	case "pull_request":
		return pullRequestEvent(repo, sender, w)
	case "issues":
		return issuesEvent(repo, sender, w)
	case "issue_comment":
		return issueCommentEvent(repo, sender, w)
	}
	return nil
}

// baseContext builds the fields every event shares, plus the
// list-contains/scalar filter aliases (see giteaFilters/giteaContext).
func baseContext(repo, action, ref, branch, sender, url string) map[string]any {
	return map[string]any{
		"repo": repo, "action": action, "ref": ref, "branch": branch, "sender": sender, "url": url,
		"actions": action, "states": "", "branches": branch,
	}
}

func pushEvent(repo, sender string, w giteaWire) map[string]any {
	branch := strings.TrimPrefix(w.Ref, "refs/heads/")
	pageURL := nonEmpty(w.CompareURL, w.Repository.HTMLURL)
	ctx := baseContext(repo, "", w.Ref, branch, sender, pageURL)
	dedup := fmt.Sprintf("push:%s:%s:%s", repo, w.Ref, w.After)
	return map[string]any{
		"event": "push", "kind": "push",
		"title":   fmt.Sprintf("push to %s on %s", branch, repo),
		"dedup":   dedup,
		"context": ctx,
	}
}

func pullRequestEvent(repo, sender string, w giteaWire) map[string]any {
	if w.PullRequest == nil {
		return nil
	}
	pr := w.PullRequest
	ctx := baseContext(repo, w.Action, "", pr.Head.Ref, sender, pr.HTMLURL)
	ctx["state"] = pr.State
	ctx["states"] = pr.State
	ctx["pr_number"] = pr.Number
	ctx["title"] = pr.Title
	ctx["base"] = pr.Base.Ref
	ctx["head"] = pr.Head.Ref
	ctx["merged"] = pr.Merged
	return map[string]any{
		"event": "pull_request", "kind": "pull_request",
		"title":   fmt.Sprintf("pull request %s: %s#%d %s", w.Action, repo, pr.Number, pr.Title),
		"dedup":   fmt.Sprintf("pr:%s:%d:%s", repo, pr.Number, w.Action),
		"context": ctx,
	}
}

func issuesEvent(repo, sender string, w giteaWire) map[string]any {
	if w.Issue == nil {
		return nil
	}
	iss := w.Issue
	ctx := baseContext(repo, w.Action, "", "", sender, iss.HTMLURL)
	ctx["state"] = iss.State
	ctx["states"] = iss.State
	ctx["issue_number"] = iss.Number
	ctx["title"] = iss.Title
	return map[string]any{
		"event": "issues", "kind": "issues",
		"title":   fmt.Sprintf("issue %s: %s#%d %s", w.Action, repo, iss.Number, iss.Title),
		"dedup":   fmt.Sprintf("issue:%s:%d:%s", repo, iss.Number, w.Action),
		"context": ctx,
	}
}

func issueCommentEvent(repo, sender string, w giteaWire) map[string]any {
	if w.Issue == nil || w.Comment == nil {
		return nil
	}
	iss, c := w.Issue, w.Comment
	ctx := baseContext(repo, w.Action, "", "", sender, nonEmpty(c.HTMLURL, iss.HTMLURL))
	ctx["issue_number"] = iss.Number
	ctx["comment_body"] = c.Body
	dedup := fmt.Sprintf("comment:%s:%d:%d", repo, iss.Number, c.ID)
	if c.ID == 0 {
		dedup = fmt.Sprintf("comment:%s:%d:%s:%s", repo, iss.Number, w.Action, c.Body)
	}
	return map[string]any{
		"event": "issue_comment", "kind": "issue_comment",
		"title":   fmt.Sprintf("new comment on %s#%d", repo, iss.Number),
		"dedup":   dedup,
		"context": ctx,
	}
}

func main() {
	if err := plugin.Serve(giteaPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-gitea: %v\n", err)
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

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
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
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

// intStr renders an integer-ish option as a query-string value ("" if absent).
func intStr(v any) string {
	if v == nil {
		return ""
	}
	if n := toInt64(v); n != 0 {
		return strconv.FormatInt(n, 10)
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
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

// intList accepts a list of numeric label ids in any of the JSON-decoded
// numeric shapes (or numeric strings) and returns them as int64s.
func intList(v any) []int64 {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]int64, 0, len(x))
		for _, e := range x {
			out = append(out, toInt64(e))
		}
		return out
	case []int64:
		return x
	}
	return nil
}

// repoParts splits the required "repo" option ("owner/name") into its parts.
func repoParts(o map[string]any) (owner, repo string, err error) {
	full := str(o["repo"])
	if full == "" {
		return "", "", fmt.Errorf("repo is required")
	}
	owner, repo, ok := strings.Cut(full, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", fmt.Errorf("repo must be owner/name, got %q", full)
	}
	return owner, repo, nil
}

func requiredStr(o map[string]any, key string) (string, error) {
	v := str(o[key])
	if v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}

func requiredInt(o map[string]any, key string) (int64, error) {
	v, ok := o[key]
	if !ok || v == nil {
		return 0, fmt.Errorf("%s is required", key)
	}
	n := toInt64(v)
	if n == 0 {
		return 0, fmt.Errorf("%s is required", key)
	}
	return n, nil
}

// toBase64Content accepts either already-base64-encoded content or raw text,
// and returns base64 either way. A valid base64 decode is treated as "already
// encoded" (the common case when a caller read a file with a base64 codec);
// anything else is encoded here.
func toBase64Content(s string) string {
	if s == "" {
		return ""
	}
	if _, err := base64.StdEncoding.DecodeString(s); err == nil {
		return s
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// requireWebhookSecret refuses to start an unauthenticated webhook
// listener.
//
// sourcekit.VerifyHMAC returns true when the secret is empty — it leaves
// the policy to the caller — and no caller had one. The result was that
// omitting the secret silently accepted ANY unsigned POST on the listen
// address as a real event from the provider, which is remote trigger
// injection with no signal that it happened. A missing secret is far more
// often a mistake than a choice, so it fails closed; `webhook.allow_unsigned:
// true` is the explicit, greppable way to say you meant it.
func requireWebhookSecret(secret string, allowUnsigned bool) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintf(os.Stderr, "gitea: allow_unsigned is set — accepting UNSIGNED webhooks; anyone who can reach the listen address can fire triggers\n")
		return nil
	}
	return fmt.Errorf("gitea: no webhook secret configured (webhook.secret) — an unsigned listener accepts any POST on the listen address as a real event. Set it, or set `webhook.allow_unsigned: true` if you genuinely front this with something else that authenticates")
}
