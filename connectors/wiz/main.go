// Command conductor-wiz is the Wiz connector as a standalone external
// conductor plugin (#59). Wiz (wiz.io) is a cloud-security platform; this
// plugin drives its tenant GraphQL API for issues, findings, and projects,
// authenticating via OAuth2 client-credentials, and — as a source — receives
// operator-configured Wiz Integration webhooks for new/updated issues. Built
// ONLY against the public SDK (pkg/plugin) and the connector-kit
// (pkg/sourcekit) — no other dependency, no conductor internals.
//
// Connection (used for both Invoke and StartSource):
//
//	client_id:     "<oauth client id>"                       # required
//	client_secret: "<oauth client secret>"                   # required
//	api_url:       "https://api.us1.app.wiz.io/graphql"      # required; tenant GraphQL endpoint
//	auth_url:      "https://auth.app.wiz.io/oauth/token"      # default; override for tests
//	audience:      "wiz-api"                                  # default
//	webhook:
//	  listen:         ":9097"           # HTTP listener address (StartSource only)
//	  path:           "/wiz"            # request path (default /wiz)
//	  secret:         "<shared token>"  # compared to X-Conductor-Token (or ?token=)
//	  header:         "X-Conductor-Token" # override the token header name
//	  allow_unsigned: false             # explicit opt-out of the fail-closed default
//	  smee:           "https://smee.io/xyz" # optional smee.io-style SSE relay,
//	                                     # for when the listener has no public URL
//
// The shared sourcekit.Listener.ServeReq hands the callback the full request
// (headers, query, body), so the ?token= query case is checked directly — and
// the same Listener transparently accepts deliveries relayed over
// webhook.smee. See checkToken below.
//
// Wiz's tenant `api_url` host varies per tenant/region (e.g.
// api.us1.app.wiz.io, api.eu1.app.wiz.io, ...) — narrow `network:` to your
// tenant's actual host; the declared Egress capability documents the pattern
// (`api.*.app.wiz.io:443`), not a single fixed host.
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
	"strings"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// defaultAuthURL is Wiz's OAuth2 client-credentials token endpoint.
// Overridable via connection `auth_url` so tests point this at an
// httptest.Server instead.
const defaultAuthURL = "https://auth.app.wiz.io/oauth/token"

// defaultAudience is the OAuth2 audience Wiz expects for API tokens.
const defaultAudience = "wiz-api"

type wizPlugin struct {
	mu      sync.Mutex
	tokens  map[string]*cachedToken // keyed by client_id+auth_url, so distinct instances never share a cache slot
	httpCli *http.Client
}

type cachedToken struct {
	value   string
	expires time.Time
}

func newWizPlugin() *wizPlugin {
	return &wizPlugin{tokens: map[string]*cachedToken{}, httpCli: &http.Client{Timeout: 30 * time.Second}}
}

func (w *wizPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "wiz",
		Desc: "Wiz cloud-security platform: issues and vulnerability findings over its tenant GraphQL API (OAuth2 client-credentials); issue-alert webhooks in as a source.",
		Connection: plugin.Schema{
			"client_id":     {Type: "string", Required: true, Desc: "OAuth2 client_credentials client_id"},
			"client_secret": {Type: "string", Required: true, Desc: "OAuth2 client_credentials client_secret"},
			"api_url":       {Type: "string", Required: true, Desc: "tenant GraphQL endpoint, e.g. https://api.us1.app.wiz.io/graphql"},
			"auth_url":      {Type: "string", Desc: "OAuth2 token endpoint (default " + defaultAuthURL + ")"},
			"audience":      {Type: "string", Desc: "OAuth2 audience (default " + defaultAudience + ")"},
			"webhook":       {Type: "map", Desc: "source transport: listen, path, secret, header, allow_unsigned, smee"},
		},
		Events: []plugin.Event{
			{
				Name: "issue",
				Desc: "a Wiz Integration webhook fired for a new or updated issue",
				Context: plugin.Schema{
					"issue_id": {Type: "string"}, "title": {Type: "string"},
					"severity": {Type: "string"}, "status": {Type: "string"},
					"entity": {Type: "string"}, "project": {Type: "string"}, "url": {Type: "string"},
				},
				Filters: plugin.Schema{
					"severities": {Type: "list", Desc: "e.g. CRITICAL, HIGH (empty = any)"},
					"statuses":   {Type: "list", Desc: "e.g. OPEN, IN_PROGRESS (empty = any)"},
					"projects":   {Type: "list", Desc: "project name (empty = any)"},
					"severity":   {Type: "string"}, "status": {Type: "string"}, "project": {Type: "string"},
				},
			},
		},
		Verbs: wizVerbs(),
		// Wiz's OAuth token endpoint is a single fixed host; the GraphQL API
		// host is tenant/region-specific (api.us1.app.wiz.io, api.eu1.app.wiz.io,
		// ...), so the manifest documents the pattern rather than one literal
		// host. Operators SHOULD narrow `network:` to their actual tenant host.
		Capabilities: plugin.Capabilities{Egress: []string{"auth.app.wiz.io:443", "api.*.app.wiz.io:443"}},
	}
}

func wizVerbs() []plugin.Verb {
	filterOpt := plugin.Field{Type: "map", Desc: `GraphQL filterBy, e.g. {"status":["OPEN","IN_PROGRESS"],"severity":["CRITICAL","HIGH"]}`}
	pageOutputs := plugin.Schema{"items": {Type: "list"}, "page_info": {Type: "any"}}
	return []plugin.Verb{
		{
			Name: "issues", Desc: "list issues matching a filter",
			Options: plugin.Schema{
				"filter": filterOpt,
				"first":  {Type: "integer", Desc: "page size (default 50)"},
				"after":  {Type: "string", Desc: "pagination cursor"},
			},
			Outputs: pageOutputs,
		},
		{
			Name: "issue_get", Desc: "fetch a single issue by id",
			Options: plugin.Schema{"id": {Type: "string", Required: true, Scope: "issue"}},
			Outputs: plugin.Schema{"result": {Type: "any"}},
		},
		{
			Name: "update_issue", Desc: "update an issue's status, note, or resolution reason",
			Options: plugin.Schema{
				"id":                {Type: "string", Required: true, Scope: "issue"},
				"status":            {Type: "string", Enum: []string{"OPEN", "IN_PROGRESS", "RESOLVED", "REJECTED"}},
				"note":              {Type: "string"},
				"resolution_reason": {Type: "string"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}},
		},
		{
			Name: "findings", Desc: "list vulnerability findings matching a filter",
			Usage: "alias: vulnerabilities",
			Options: plugin.Schema{
				"filter": filterOpt,
				"first":  {Type: "integer", Desc: "page size (default 50)"},
				"after":  {Type: "string", Desc: "pagination cursor"},
			},
			Outputs: pageOutputs,
		},
		{
			Name: "vulnerabilities", Desc: "alias of findings",
			Options: plugin.Schema{
				"filter": filterOpt,
				"first":  {Type: "integer", Desc: "page size (default 50)"},
				"after":  {Type: "string", Desc: "pagination cursor"},
			},
			Outputs: pageOutputs,
		},
		{
			Name: "projects", Desc: "list Wiz projects",
			Options: plugin.Schema{"first": {Type: "integer", Desc: "page size (default 50)"}},
			Outputs: plugin.Schema{"items": {Type: "list"}},
		},
		{
			Name: "graphql", Desc: "escape hatch: run any GraphQL query/mutation against the tenant API",
			Options: plugin.Schema{
				"query":     {Type: "string", Required: true},
				"variables": {Type: "map"},
			},
			Outputs: plugin.Schema{"result": {Type: "any"}},
		},
	}
}

// --- OAuth2 client-credentials, with in-memory token caching ---

type wizConn struct {
	clientID, clientSecret string
	apiURL, authURL        string
	audience               string
}

func parseConn(m map[string]any) (wizConn, error) {
	c := wizConn{
		clientID:     str(m["client_id"]),
		clientSecret: str(m["client_secret"]),
		apiURL:       str(m["api_url"]),
		authURL:      strOr(m["auth_url"], defaultAuthURL),
		audience:     strOr(m["audience"], defaultAudience),
	}
	if c.clientID == "" {
		return c, fmt.Errorf("client_id is required")
	}
	if c.clientSecret == "" {
		return c, fmt.Errorf("client_secret is required")
	}
	if c.apiURL == "" {
		return c, fmt.Errorf("api_url is required")
	}
	return c, nil
}

func (w *wizPlugin) tokenCacheKey(c wizConn) string {
	return c.authURL + "\x00" + c.clientID
}

// token returns a cached, still-valid access token, or fetches and caches a
// fresh one. Mutex-guarded so concurrent invocations against the same
// instance never race the cache, and a valid cache entry never triggers a
// second fetch.
func (w *wizPlugin) token(ctx context.Context, c wizConn) (string, error) {
	key := w.tokenCacheKey(c)

	w.mu.Lock()
	if t, ok := w.tokens[key]; ok && time.Now().Before(t.expires) {
		tok := t.value
		w.mu.Unlock()
		return tok, nil
	}
	w.mu.Unlock()

	tok, expiresIn, err := w.fetchToken(ctx, c)
	if err != nil {
		return "", err
	}
	// A small safety margin so a token doesn't expire mid-flight between the
	// cache check and the request that uses it.
	margin := 30 * time.Second
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	exp := time.Now().Add(time.Duration(expiresIn)*time.Second - margin)

	w.mu.Lock()
	w.tokens[key] = &cachedToken{value: tok, expires: exp}
	w.mu.Unlock()
	return tok, nil
}

// fetchToken POSTs the OAuth2 client-credentials grant to auth_url,
// form-encoded, and parses {access_token, expires_in} from the response.
func (w *wizPlugin) fetchToken(ctx context.Context, c wizConn) (token string, expiresIn int64, err error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"audience":      {c.audience},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := w.httpCli.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("wiz: auth request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("wiz: auth %s: %s", resp.Status, string(body))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", 0, fmt.Errorf("wiz: auth response decode: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("wiz: auth response had no access_token: %s", string(body))
	}
	return out.AccessToken, out.ExpiresIn, nil
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
	Data   json.RawMessage `json:"data"`
	Errors []graphqlError  `json:"errors"`
}

// doGraphQL POSTs query/variables to the tenant API with a bearer token,
// obtained (and cached/refreshed) via token(). A non-2xx response OR a
// GraphQL "errors" array is surfaced as CodeInternalError, message plus the
// raw response body so nothing about the failure is lost.
func (w *wizPlugin) doGraphQL(ctx context.Context, c wizConn, query string, variables map[string]any) (json.RawMessage, error) {
	tok, err := w.token(ctx, c)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}

	payload, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, "wiz: encode graphql request: "+err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := w.httpCli.Do(req)
	if err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, "wiz: graphql request failed: "+err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("wiz: graphql %s: %s", resp.Status, string(body)))
	}

	var gr graphqlResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, "wiz: graphql response decode: "+err.Error()+": "+string(body))
	}
	if len(gr.Errors) > 0 {
		msgs := make([]string, len(gr.Errors))
		for i, e := range gr.Errors {
			msgs[i] = e.Message
		}
		return nil, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("wiz: graphql errors: %s: %s", strings.Join(msgs, "; "), string(body)))
	}
	return gr.Data, nil
}

// --- Invoke ---

const (
	issuesQuery = `query Issues($filterBy: IssueFilters, $first: Int, $after: String) {
  issues(filterBy: $filterBy, first: $first, after: $after) {
    nodes {
      id
      sourceRule { name }
      status
      severity
      createdAt
      entitySnapshot { name type }
      projects { name }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

	issueGetQuery = `query Issue($id: ID!) {
  issue(id: $id) {
    id
    sourceRule { name }
    status
    severity
    createdAt
    entitySnapshot { name type }
    projects { name }
  }
}`

	updateIssueMutation = `mutation UpdateIssue($input: UpdateIssueInput!) {
  updateIssue(input: $input) {
    issue { id status }
  }
}`

	findingsQuery = `query VulnerabilityFindings($filterBy: VulnerabilityFindingFilters, $first: Int, $after: String) {
  vulnerabilityFindings(filterBy: $filterBy, first: $first, after: $after) {
    nodes {
      id
      name
      severity
      status
      vulnerableAsset { name type }
      detectedAt
      resolvedAt
    }
    pageInfo { hasNextPage endCursor }
  }
}`

	projectsQuery = `query Projects($first: Int) {
  projects(first: $first) {
    nodes { id name }
  }
}`
)

func (w *wizPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	c, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	ctx := context.Background()

	switch req.Verb {
	case "issues":
		return w.pagedQuery(ctx, c, issuesQuery, o, "issues")
	case "issue_get":
		id := str(o["id"])
		if id == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "id is required")
		}
		data, err := w.doGraphQL(ctx, c, issueGetQuery, map[string]any{"id": id})
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		var out struct {
			Issue any `json:"issue"`
		}
		_ = json.Unmarshal(data, &out)
		return plugin.InvokeResult{Outputs: map[string]any{"result": out.Issue}}, nil
	case "update_issue":
		id := str(o["id"])
		if id == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "id is required")
		}
		patch := map[string]any{}
		if s := str(o["status"]); s != "" {
			patch["status"] = s
		}
		if n := str(o["note"]); n != "" {
			patch["note"] = n
		}
		if r := str(o["resolution_reason"]); r != "" {
			patch["resolutionReason"] = r
		}
		variables := map[string]any{"input": map[string]any{"id": id, "patch": patch}}
		data, err := w.doGraphQL(ctx, c, updateIssueMutation, variables)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		var out struct {
			UpdateIssue struct {
				Issue any `json:"issue"`
			} `json:"updateIssue"`
		}
		_ = json.Unmarshal(data, &out)
		return plugin.InvokeResult{Outputs: map[string]any{"result": out.UpdateIssue.Issue}}, nil
	case "findings", "vulnerabilities":
		return w.pagedQuery(ctx, c, findingsQuery, o, "vulnerabilityFindings")
	case "projects":
		variables := map[string]any{"first": firstOr(o["first"], 50)}
		data, err := w.doGraphQL(ctx, c, projectsQuery, variables)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		var out struct {
			Projects struct {
				Nodes []any `json:"nodes"`
			} `json:"projects"`
		}
		_ = json.Unmarshal(data, &out)
		items := out.Projects.Nodes
		if items == nil {
			items = []any{}
		}
		return plugin.InvokeResult{Outputs: map[string]any{"items": items}}, nil
	case "graphql":
		query := str(o["query"])
		if query == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "query is required")
		}
		variables, _ := o["variables"].(map[string]any)
		data, err := w.doGraphQL(ctx, c, query, variables)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		var result any
		_ = json.Unmarshal(data, &result)
		return plugin.InvokeResult{Outputs: map[string]any{"result": result}}, nil
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
}

// pagedQuery runs a query shaped like {<field>{nodes{...} pageInfo{...}}} and
// normalizes it to items/page_info, shared by issues/findings/vulnerabilities.
func (w *wizPlugin) pagedQuery(ctx context.Context, c wizConn, query string, o map[string]any, field string) (plugin.InvokeResult, error) {
	filter, _ := o["filter"].(map[string]any)
	variables := map[string]any{
		"filterBy": filter,
		"first":    firstOr(o["first"], 50),
	}
	if after := str(o["after"]); after != "" {
		variables["after"] = after
	}
	data, err := w.doGraphQL(ctx, c, query, variables)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "wiz: decode "+field+" response: "+err.Error())
	}
	var page struct {
		Nodes    []any `json:"nodes"`
		PageInfo any   `json:"pageInfo"`
	}
	if fr, ok := raw[field]; ok {
		_ = json.Unmarshal(fr, &page)
	}
	items := page.Nodes
	if items == nil {
		items = []any{}
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "page_info": page.PageInfo}}, nil
}

func firstOr(v any, d int) int {
	switch x := v.(type) {
	case nil:
		return d
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return d
}

// --- Source (webhook) ---

// defaultTokenHeader is the header a Wiz Integration webhook is configured to
// send the shared verification token in, when the operator uses the
// documented recommended template. Overridable via webhook.header.
const defaultTokenHeader = "X-Conductor-Token"

// wizWebhookConfig is the resolved webhook.* sub-map.
type wizWebhookConfig struct {
	addr, path, secret, header, smeeURL string
	allowUnsigned                       bool
}

func parseWebhookConfig(cfg map[string]any) wizWebhookConfig {
	webhook, _ := cfg["webhook"].(map[string]any)
	wc := wizWebhookConfig{path: "/wiz", header: defaultTokenHeader}
	if webhook == nil {
		return wc
	}
	wc.addr = str(webhook["listen"])
	if p := str(webhook["path"]); p != "" {
		wc.path = p
	}
	wc.secret = str(webhook["secret"])
	if h := str(webhook["header"]); h != "" {
		wc.header = h
	}
	wc.allowUnsigned, _ = webhook["allow_unsigned"].(bool)
	wc.smeeURL = str(webhook["smee"])
	return wc
}

func (w *wizPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	wc := parseWebhookConfig(cfg)
	if wc.addr == "" && wc.smeeURL == "" {
		return fmt.Errorf("wiz: no webhook.listen address or webhook.smee relay configured")
	}
	if err := requireWebhookSecret("wiz", wc.secret, wc.allowUnsigned, "webhook.secret"); err != nil {
		return err
	}
	dedup := sourcekit.NewDedup(2048)
	if wc.addr != "" {
		fmt.Fprintf(os.Stderr, "wiz[%s]: listening on %s%s\n", req.Instance, wc.addr, wc.path)
	}
	if wc.smeeURL != "" {
		fmt.Fprintf(os.Stderr, "wiz[%s]: relaying via smee channel %s\n", req.Instance, wc.smeeURL)
	}
	ln := sourcekit.Listener{Addr: wc.addr, Path: wc.path, Relay: wc.smeeURL}
	return ln.ServeReq(ctx, func(rq *sourcekit.Request) {
		if wc.secret != "" && !checkToken(wc, rq) {
			return
		}
		iss := parseIssuePayload(rq.Body)
		if iss.IssueID == "" {
			return
		}
		dk := iss.IssueID + "\x00" + iss.Status
		if !dedup.Add(dk) {
			return
		}
		_ = emit(map[string]any{
			"event": "issue",
			"kind":  "issue",
			"title": fmt.Sprintf("wiz %s: %s", nonEmpty(iss.Severity, "issue"), iss.Title),
			"dedup": dk,
			"context": map[string]any{
				"issue_id": iss.IssueID, "title": iss.Title,
				"severity": iss.Severity, "status": iss.Status,
				"entity": iss.Entity, "project": iss.Project, "url": iss.URL,
				// Plural aliases so the documented filter vocabulary
				// (filters: {severities/statuses/projects: [...]}) matches
				// against the daemon's generic list-contains filter evaluator.
				"severities": iss.Severity, "statuses": iss.Status, "projects": iss.Project,
			},
		})
	})
}

// issuePayload is the recommended webhook body shape (see docs/connectors/wiz.md):
// Wiz Integrations let the operator configure an arbitrary JSON template for
// the outbound webhook, so this is what conductor asks operators to send —
// not a fixed shape Wiz itself mandates.
type issuePayload struct {
	IssueID  string `json:"issue_id"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	Entity   string `json:"entity"`
	Project  string `json:"project"`
	URL      string `json:"url"`
}

func parseIssuePayload(body []byte) issuePayload {
	var p issuePayload
	_ = json.Unmarshal(body, &p)
	return p
}

// checkToken compares the shared token from the configured header — or, if
// absent, the `token` query parameter — against wc.secret in constant time.
func checkToken(wc wizWebhookConfig, rq *sourcekit.Request) bool {
	got := rq.Header.Get(wc.header)
	if got == "" {
		got = rq.Query.Get("token")
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(wc.secret)) == 1
}

func main() {
	if err := plugin.Serve(newWizPlugin()); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-wiz: %v\n", err)
		os.Exit(1)
	}
}

// --- helpers ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// requireWebhookSecret refuses to start an unauthenticated webhook listener.
//
// A missing token secret is far more often a mistake than a choice, so it
// fails closed; `webhook.allow_unsigned: true` is the explicit, greppable way
// to say you meant it — e.g. because something else in front of the listener
// already authenticates the request.
func requireWebhookSecret(who, secret string, allowUnsigned bool, key string) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintf(os.Stderr, "%s: webhook.allow_unsigned is set — accepting UNVERIFIED webhooks; anyone who can reach the listen address can fire triggers\n", who)
		return nil
	}
	return fmt.Errorf("%s: no webhook token configured (%s) — an unverified listener accepts any POST on the listen address as a real event. Set it, or set `webhook.allow_unsigned: true` if you genuinely front this with something else that authenticates", who, key)
}
