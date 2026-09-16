// Command conductor-notion is a verb-only conductor connector (#59) that
// drives the Notion API over net/http. It exposes pages, databases, blocks,
// search, comments, and users as verbs, plus an `api` escape hatch for any
// endpoint a first-class verb does not cover. Built ONLY against the public
// SDK (pkg/plugin) and the standard library — no third-party client.
//
// Notion's payload shapes (page properties, block children, database
// property schemas, filters) are rich, nested JSON that this connector does
// not attempt to model in Go structs. Verbs that accept them (properties,
// children, filter, sorts, ...) pass the caller's map/list straight through
// as the request body, so the full Notion API surface stays reachable
// without this connector chasing every property-type shape. A handful of
// verbs additionally accept an ergonomic shortcut (parent: database_id/
// page_id string, title/rich_text: plain string) for the common case.
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

// defaultAPIBase is Notion's REST API origin. Overridable via connection
// `api_base` so tests (and any private Notion-compatible gateway) can point
// this at an httptest.Server instead.
const defaultAPIBase = "https://api.notion.com/v1"

// defaultNotionVersion is the Notion-Version header Notion requires on every
// request, pinned to the latest stable revision as of writing.
const defaultNotionVersion = "2022-06-28"

type notionPlugin struct{}

func (notionPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "notion",
		Desc: "Notion: pages, databases, blocks, search, comments, and users as verbs. Structured properties/children/filter maps pass straight through; ergonomic shortcuts cover the common cases (parent: database_id/page_id, rich_text: plain string).",
		Connection: plugin.Schema{
			"token":    {Type: "string", Required: true, Desc: "Notion integration token, sent as Authorization: Bearer <token>"},
			"version":  {Type: "string", Desc: "Notion-Version header (default " + defaultNotionVersion + ")"},
			"api_base": {Type: "string", Desc: "override the Notion API base URL (tests, or a private gateway)"},
		},
		Verbs:        notionVerbs(),
		Capabilities: plugin.Capabilities{Egress: []string{"api.notion.com:443"}},
	}
}

func notionVerbs() []plugin.Verb {
	resultOnly := plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}}
	resultsList := plugin.Schema{"results": {Type: "list"}, "result": {Type: "any"}, "status_code": {Type: "integer"}}
	return []plugin.Verb{
		{
			Name: "create_page", Desc: "create a page in a database or under a parent page",
			Usage: "parent may be the full Notion object ({\"database_id\":...} / {\"page_id\":...}) or the database_id/page_id shortcut",
			Options: plugin.Schema{
				"parent":      {Type: "any", Required: true, Desc: "{database_id: ...} or {page_id: ...}, or a bare database_id/page_id string"},
				"database_id": {Type: "string", Desc: "shortcut: wraps into parent {\"database_id\": ...}"},
				"page_id":     {Type: "string", Desc: "shortcut: wraps into parent {\"page_id\": ...}"},
				"properties":  {Type: "map", Required: true, Desc: "Notion page property values, passed straight through"},
				"children":    {Type: "list", Desc: "block objects to seed the page with"},
				"icon":        {Type: "any", Desc: "page icon object"},
				"cover":       {Type: "any", Desc: "page cover object"},
			},
			Outputs: resultOnly,
		},
		{
			Name: "update_page", Desc: "edit a page's properties or archive it",
			Options: plugin.Schema{
				"page_id":    {Type: "string", Required: true, Scope: "page"},
				"properties": {Type: "map", Desc: "property values to change"},
				"archived":   {Type: "boolean", Desc: "true to move the page to trash, false to restore"},
			},
			Outputs: resultOnly,
		},
		{
			Name: "get_page", Desc: "read a page's properties",
			Options: plugin.Schema{"page_id": {Type: "string", Required: true, Scope: "page"}},
			Outputs: resultOnly,
		},
		{
			Name: "get_database", Desc: "read a database's schema (properties, title, parent)",
			Options: plugin.Schema{"database_id": {Type: "string", Required: true, Scope: "database"}},
			Outputs: resultOnly,
		},
		{
			Name: "query_database", Desc: "query a database's rows (pages), optionally filtered/sorted",
			Options: plugin.Schema{
				"database_id":  {Type: "string", Required: true, Scope: "database"},
				"filter":       {Type: "map", Desc: "Notion filter object, passed straight through"},
				"sorts":        {Type: "list", Desc: "Notion sort objects"},
				"page_size":    {Type: "integer"},
				"start_cursor": {Type: "string"},
			},
			Outputs: resultsList,
		},
		{
			Name: "create_database", Desc: "create a database under a parent page",
			Options: plugin.Schema{
				"parent":     {Type: "any", Required: true, Desc: "{page_id: ...}, or a bare page_id string"},
				"title":      {Type: "any", Desc: "a plain string, or a Notion rich_text list"},
				"properties": {Type: "map", Required: true, Desc: "the database's property schema"},
			},
			Outputs: resultOnly,
		},
		{
			Name: "update_database", Desc: "edit a database's title and/or property schema",
			Options: plugin.Schema{
				"database_id": {Type: "string", Required: true, Scope: "database"},
				"title":       {Type: "any", Desc: "a plain string, or a Notion rich_text list"},
				"properties":  {Type: "map", Desc: "property schema changes"},
			},
			Outputs: resultOnly,
		},
		{
			Name: "append_blocks", Desc: "append child blocks to a page or block",
			Options: plugin.Schema{
				"block_id": {Type: "string", Required: true, Scope: "block", Desc: "a page id or block id — both accept children"},
				"children": {Type: "list", Required: true, Desc: "block objects to append"},
			},
			Outputs: resultOnly,
		},
		{
			Name: "get_block_children", Desc: "list a block's (or page's) direct children",
			Options: plugin.Schema{
				"block_id":     {Type: "string", Required: true, Scope: "block"},
				"page_size":    {Type: "integer"},
				"start_cursor": {Type: "string"},
			},
			Outputs: resultsList,
		},
		{
			Name: "delete_block", Desc: "delete (archive) a block",
			Options: plugin.Schema{"block_id": {Type: "string", Required: true, Scope: "block"}},
			Outputs: resultOnly,
		},
		{
			Name: "search", Desc: "search pages and databases shared with the integration",
			Options: plugin.Schema{
				"query":     {Type: "string", Desc: "text to search for; empty returns everything shared with the integration"},
				"filter":    {Type: "map", Desc: "{property: \"object\", value: \"page\"|\"database\"}"},
				"sort":      {Type: "map", Desc: "Notion search sort object"},
				"page_size": {Type: "integer"},
			},
			Outputs: resultsList,
		},
		{
			Name: "create_comment", Desc: "add a comment to a page, or reply in an existing discussion",
			Usage: "set parent (page_id) to start a new discussion, or discussion_id to reply to one",
			Options: plugin.Schema{
				"parent":        {Type: "any", Desc: "{page_id: ...}, or a bare page_id string"},
				"discussion_id": {Type: "string", Desc: "reply to an existing discussion instead of a page"},
				"rich_text": {Type: "any", Required: true,
					Desc: "a plain string (wrapped into [{\"text\":{\"content\":...}}]), or a Notion rich_text list"},
			},
			Outputs: resultOnly,
		},
		{
			Name:    "get_user",
			Desc:    "read one user by id",
			Options: plugin.Schema{"user_id": {Type: "string", Required: true, Scope: "user"}},
			Outputs: resultOnly,
		},
		{
			Name:    "list_users",
			Desc:    "list workspace users",
			Options: plugin.Schema{"page_size": {Type: "integer"}, "start_cursor": {Type: "string"}},
			Outputs: resultsList,
		},
		{
			Name: "api", Desc: "call any Notion API endpoint not covered by a first-class verb",
			Usage: "escape hatch: method + path (relative to /v1), optional query and body",
			Options: plugin.Schema{
				"method": {Type: "string", Required: true, Enum: []string{"GET", "POST", "PATCH", "DELETE"}},
				"path":   {Type: "string", Required: true, Desc: "path relative to /v1, e.g. \"pages/123\""},
				"query":  {Type: "map", Desc: "query string parameters"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: resultOnly,
		},
	}
}

// notionConn is the resolved connection config for one invocation.
type notionConn struct {
	token   string
	version string
	base    string
}

func parseConn(m map[string]any) (notionConn, error) {
	token := str(m["token"])
	if token == "" {
		return notionConn{}, fmt.Errorf("connection.token is required")
	}
	return notionConn{
		token:   token,
		version: strOr(m["version"], defaultNotionVersion),
		base:    strOr(m["api_base"], defaultAPIBase),
	}, nil
}

func (notionPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	call, err := verbCall(req.Verb, o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}

	outputs, err := conn.do(call.method, call.path, call.query, call.body)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

// apiCall is the resolved HTTP request one verb builds, before it is sent.
type apiCall struct {
	method string
	path   string
	query  map[string]string
	body   any
}

// verbCall builds the HTTP call for one verb. Pure and hermetically
// testable — no request is sent here.
func verbCall(verb string, o map[string]any) (apiCall, error) {
	switch verb {
	case "create_page":
		return createPageCall(o)
	case "update_page":
		id := str(o["page_id"])
		if id == "" {
			return apiCall{}, fmt.Errorf("page_id is required")
		}
		body := map[string]any{}
		if p, ok := o["properties"].(map[string]any); ok {
			body["properties"] = p
		}
		if v, ok := o["archived"]; ok {
			body["archived"] = v
		}
		return apiCall{method: http.MethodPatch, path: "pages/" + id, body: body}, nil
	case "get_page":
		id := str(o["page_id"])
		if id == "" {
			return apiCall{}, fmt.Errorf("page_id is required")
		}
		return apiCall{method: http.MethodGet, path: "pages/" + id}, nil
	case "get_database":
		id := str(o["database_id"])
		if id == "" {
			return apiCall{}, fmt.Errorf("database_id is required")
		}
		return apiCall{method: http.MethodGet, path: "databases/" + id}, nil
	case "query_database":
		return queryDatabaseCall(o)
	case "create_database":
		return createDatabaseCall(o)
	case "update_database":
		id := str(o["database_id"])
		if id == "" {
			return apiCall{}, fmt.Errorf("database_id is required")
		}
		body := map[string]any{}
		if t, ok := o["title"]; ok {
			body["title"] = titleValue(t)
		}
		if p, ok := o["properties"].(map[string]any); ok {
			body["properties"] = p
		}
		return apiCall{method: http.MethodPatch, path: "databases/" + id, body: body}, nil
	case "append_blocks":
		id := str(o["block_id"])
		if id == "" {
			return apiCall{}, fmt.Errorf("block_id is required")
		}
		children, _ := o["children"].([]any)
		if len(children) == 0 {
			return apiCall{}, fmt.Errorf("children is required")
		}
		return apiCall{method: http.MethodPatch, path: "blocks/" + id + "/children", body: map[string]any{"children": children}}, nil
	case "get_block_children":
		id := str(o["block_id"])
		if id == "" {
			return apiCall{}, fmt.Errorf("block_id is required")
		}
		q := map[string]string{}
		if v := intStr(o["page_size"]); v != "" {
			q["page_size"] = v
		}
		if v := str(o["start_cursor"]); v != "" {
			q["start_cursor"] = v
		}
		return apiCall{method: http.MethodGet, path: "blocks/" + id + "/children", query: q}, nil
	case "delete_block":
		id := str(o["block_id"])
		if id == "" {
			return apiCall{}, fmt.Errorf("block_id is required")
		}
		return apiCall{method: http.MethodDelete, path: "blocks/" + id}, nil
	case "search":
		body := map[string]any{}
		if v := str(o["query"]); v != "" {
			body["query"] = v
		}
		if f, ok := o["filter"].(map[string]any); ok {
			body["filter"] = f
		}
		if s, ok := o["sort"].(map[string]any); ok {
			body["sort"] = s
		}
		if v, ok := o["page_size"]; ok {
			body["page_size"] = v
		}
		return apiCall{method: http.MethodPost, path: "search", body: body}, nil
	case "create_comment":
		return createCommentCall(o)
	case "get_user":
		id := str(o["user_id"])
		if id == "" {
			return apiCall{}, fmt.Errorf("user_id is required")
		}
		return apiCall{method: http.MethodGet, path: "users/" + id}, nil
	case "list_users":
		q := map[string]string{}
		if v := intStr(o["page_size"]); v != "" {
			q["page_size"] = v
		}
		if v := str(o["start_cursor"]); v != "" {
			q["start_cursor"] = v
		}
		return apiCall{method: http.MethodGet, path: "users", query: q}, nil
	case "api":
		return apiVerbCall(o)
	}
	return apiCall{}, fmt.Errorf("unknown verb")
}

// createPageCall resolves the parent (full object, or database_id/page_id
// shortcut) and required properties.
func createPageCall(o map[string]any) (apiCall, error) {
	parent, err := parentValue(o)
	if err != nil {
		return apiCall{}, err
	}
	properties, _ := o["properties"].(map[string]any)
	if properties == nil {
		return apiCall{}, fmt.Errorf("properties is required")
	}
	body := map[string]any{"parent": parent, "properties": properties}
	if c, ok := o["children"].([]any); ok && len(c) > 0 {
		body["children"] = c
	}
	if icon, ok := o["icon"]; ok {
		body["icon"] = icon
	}
	if cover, ok := o["cover"]; ok {
		body["cover"] = cover
	}
	return apiCall{method: http.MethodPost, path: "pages", body: body}, nil
}

// parentValue resolves the parent option: a full Notion parent object
// ({"database_id": ...} / {"page_id": ...}), or the database_id/page_id
// shortcuts (either the option named "parent" holding a bare string, or the
// dedicated database_id/page_id options).
func parentValue(o map[string]any) (map[string]any, error) {
	if dbID := str(o["database_id"]); dbID != "" {
		return map[string]any{"database_id": dbID}, nil
	}
	if pageID := str(o["page_id"]); pageID != "" {
		return map[string]any{"page_id": pageID}, nil
	}
	switch p := o["parent"].(type) {
	case map[string]any:
		return p, nil
	case string:
		if p == "" {
			break
		}
		// A bare parent string is ambiguous between database_id and page_id;
		// Notion's own page ids and database ids are both UUIDs, so there is
		// no way to disambiguate from the string alone. Treat it as a
		// database_id, the more common case for create_page, and document
		// the page_id shortcut for the other one.
		return map[string]any{"database_id": p}, nil
	}
	return nil, fmt.Errorf("parent (or database_id/page_id) is required")
}

func queryDatabaseCall(o map[string]any) (apiCall, error) {
	id := str(o["database_id"])
	if id == "" {
		return apiCall{}, fmt.Errorf("database_id is required")
	}
	body := map[string]any{}
	if f, ok := o["filter"].(map[string]any); ok {
		body["filter"] = f
	}
	if s, ok := o["sorts"].([]any); ok {
		body["sorts"] = s
	}
	if v, ok := o["page_size"]; ok {
		body["page_size"] = v
	}
	if v := str(o["start_cursor"]); v != "" {
		body["start_cursor"] = v
	}
	return apiCall{method: http.MethodPost, path: "databases/" + id + "/query", body: body}, nil
}

func createDatabaseCall(o map[string]any) (apiCall, error) {
	var parent map[string]any
	switch p := o["parent"].(type) {
	case map[string]any:
		parent = p
	case string:
		if p == "" {
			return apiCall{}, fmt.Errorf("parent is required")
		}
		parent = map[string]any{"page_id": p}
	default:
		return apiCall{}, fmt.Errorf("parent is required")
	}
	properties, _ := o["properties"].(map[string]any)
	if properties == nil {
		return apiCall{}, fmt.Errorf("properties is required")
	}
	body := map[string]any{"parent": parent, "properties": properties}
	if t, ok := o["title"]; ok {
		body["title"] = titleValue(t)
	}
	return apiCall{method: http.MethodPost, path: "databases", body: body}, nil
}

// titleValue wraps a plain string into Notion's rich_text title shape; a
// list is assumed to already be one and passed through unchanged.
func titleValue(v any) any {
	switch t := v.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": map[string]any{"content": t}}}
	default:
		return v
	}
}

func createCommentCall(o map[string]any) (apiCall, error) {
	body := map[string]any{}
	switch {
	case str(o["discussion_id"]) != "":
		body["discussion_id"] = str(o["discussion_id"])
	default:
		var parent map[string]any
		switch p := o["parent"].(type) {
		case map[string]any:
			parent = p
		case string:
			if p != "" {
				parent = map[string]any{"page_id": p}
			}
		}
		if parent == nil {
			if pid := str(o["page_id"]); pid != "" {
				parent = map[string]any{"page_id": pid}
			}
		}
		if parent == nil {
			return apiCall{}, fmt.Errorf("parent (page_id) or discussion_id is required")
		}
		body["parent"] = parent
	}
	rt, ok := o["rich_text"]
	if !ok {
		return apiCall{}, fmt.Errorf("rich_text is required")
	}
	body["rich_text"] = richTextValue(rt)
	return apiCall{method: http.MethodPost, path: "comments", body: body}, nil
}

// richTextValue wraps a plain string into Notion's rich_text list shape; a
// list is assumed to already be one and passed through unchanged.
func richTextValue(v any) any {
	switch t := v.(type) {
	case string:
		return []any{map[string]any{"text": map[string]any{"content": t}}}
	default:
		return v
	}
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
	q := map[string]string{}
	if m, ok := o["query"].(map[string]any); ok {
		for k, v := range m {
			q[k] = fmt.Sprintf("%v", v)
		}
	}
	return apiCall{method: method, path: path, query: q, body: o["body"]}, nil
}

// do sends one HTTP request to the Notion API, attaching the required
// headers, and decodes the JSON response into outputs. A non-2xx response is
// a plugin.CodeInternalError carrying the status and response body; verb
// callers never see a raw *http.Response.
func (c notionConn) do(method, path string, query map[string]string, body any) (map[string]any, error) {
	url := strings.TrimRight(c.base, "/") + "/" + path
	if len(query) > 0 {
		q := make([]string, 0, len(query))
		for k, v := range query {
			q = append(q, k+"="+v)
		}
		url += "?" + strings.Join(q, "&")
	}

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
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Notion-Version", c.version)
	httpReq.Header.Set("Content-Type", "application/json")

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
		return nil, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("notion API %s: %s", resp.Status, strings.TrimSpace(string(raw))))
	}

	var result any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, plugin.Errorf(plugin.CodeInternalError, "decoding response: "+err.Error())
		}
	}

	outputs := map[string]any{"result": result, "status_code": resp.StatusCode}
	if m, ok := result.(map[string]any); ok {
		if results, ok := m["results"]; ok {
			outputs["results"] = results
		}
	}
	return outputs, nil
}

func main() {
	if err := plugin.Serve(notionPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-notion:", err)
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

func intStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return fmt.Sprintf("%d", int64(x))
	case string:
		return x
	}
	return ""
}
