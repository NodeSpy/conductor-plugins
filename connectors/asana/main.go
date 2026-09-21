// Command conductor-asana is the Asana connector as a standalone external
// conductor plugin. It drives the Asana REST API (https://app.asana.com/api/1.0)
// over net/http — tasks, projects, sections, stories (comments), tags, users,
// workspaces, search, typeahead and webhook management, plus a raw `api`
// escape hatch — and, as a source, receives Asana webhook deliveries and
// streams one normalized event per Asana event (task / story / project, with
// a catch-all `event`).
//
// Built ONLY against the public SDK (pkg/plugin) and the connector-kit
// (pkg/sourcekit, for HMAC verification and delivery dedup) — no other
// dependency.
//
// Authentication, in priority order:
//
//  1. conductor's MANAGED OAuth2 (pkg/plugin v0.15.0+): Describe().Auth bakes
//     in Asana's OAuth2 endpoints; the operator adds an `auth:` block and runs
//     `conductor connector auth asana`; the daemon then injects a fresh bearer
//     token under plugin.AccessTokenKey. This plugin never talks to the token
//     endpoint itself, so its only egress is app.asana.com.
//  2. a plain `token` connection field — an Asana Personal Access Token, sent
//     as `Authorization: Bearer <token>`. The simplest path for automation.
//
// Webhooks. Asana webhooks are unlike GitHub's or Linear's: the signing secret
// is NOT chosen by the operator. When a webhook is created (POST /webhooks —
// see the create_webhook verb) Asana immediately POSTs a HANDSHAKE to the
// target URL carrying `X-Hook-Secret: <secret>`; the receiver must echo that
// header back with a 200 in the same request, and every later delivery is
// signed with `X-Hook-Signature: <hex HMAC-SHA256 of the body under that
// secret>`. Because the handshake needs a synchronous custom response header,
// this source runs its own small net/http listener rather than
// sourcekit.Listener (which always answers 202 and cannot echo headers) — and
// a smee.io-style relay cannot complete the handshake either, so there is no
// `smee` option here.
//
// Trust model:
//
//   - webhook.secret pins one or more previously-handshaken secrets, so the
//     source verifies deliveries from the moment it starts and survives
//     restarts. Pinning is what you want in steady state.
//   - With no pinned secret the source starts in BOOTSTRAP mode: it accepts
//     exactly ONE handshake, installs the secret it carries, prints it once to
//     stderr so the operator can pin it, and closes the window. Until that
//     handshake arrives, signed deliveries are rejected (fail closed) —
//     nothing unverifiable is emitted.
//   - Once a secret is known — pinned, or installed by the bootstrap
//     handshake — further handshakes are REFUSED (a stranger who can reach the
//     listener must not be able to install their own key) unless
//     `webhook.handshake: true` is set explicitly. Do that to register
//     additional webhooks, then pin the new secrets too (secret takes a list).
//   - `webhook.allow_unsigned: true` accepts deliveries regardless of
//     signature. Explicit, greppable, and loudly logged.
//
// Connection (used for both Invoke and StartSource):
//
//	token: "<personal access token>"   # fallback bearer credential when no managed auth: is configured
//	api_base: "https://..."            # override https://app.asana.com/api/1.0 (tests)
//	webhook:
//	  listen: ":9110"                  # HTTP listener address (StartSource only; required)
//	  path: "/asana"                   # request path (default /asana)
//	  secret: "<X-Hook-Secret>"        # pinned handshake secret(s): a string or a list
//	  handshake: true                  # always accept handshakes (unset: exactly one, and only while no secret is pinned; false: never)
//	  allow_unsigned: false            # accept deliveries with no/invalid signature
//	  enrich: true                     # GET the task/story/project each event names (default true when a token is available)
//	  opt_fields: "name,completed,..." # override the opt_fields used to enrich task events
//
// Asana's webhook events are COMPACT — {action, resource{gid,resource_type},
// parent, change{field,...}, user{gid}, created_at} — with no names. With a
// token available the source enriches each event with one GET (task: name,
// completed, assignee, projects, section, due, url; story: text, author;
// project: name, url) so triggers can filter on and template with them. It
// answers Asana 200 first and enriches afterwards, so a large batch never
// times the delivery out.
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
	"strconv"
	"strings"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// defaultAPIBase is Asana's production REST API root. api_base overrides it
// for tests.
const defaultAPIBase = "https://app.asana.com/api/1.0"

// maxPages bounds how many pages an `all: true` collection verb will follow
// (100 pages × 100 items = 10,000 records) so a runaway list cannot spin.
const maxPages = 100

// defaultTaskOptFields is the field list the source requests when enriching a
// task event. webhook.opt_fields overrides it.
const defaultTaskOptFields = "name,completed,assignee.name,assignee.gid,due_on,due_at,projects.name,memberships.project.name,memberships.section.name,permalink_url,resource_subtype,workspace.name,parent.gid,tags.name"

const storyOptFields = "text,resource_subtype,type,created_by.name,created_by.gid,target.gid,target.name"

const projectOptFields = "name,permalink_url,archived,owner.name,team.name,workspace.name"

type asanaPlugin struct {
	client *http.Client
}

func newAsanaPlugin() *asanaPlugin {
	return &asanaPlugin{client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *asanaPlugin) Describe() plugin.Decl {
	resultOut := plugin.Schema{"result": {Type: "any"}, "status_code": {Type: "integer"}}
	okOut := plugin.Schema{"ok": {Type: "boolean"}, "result": {Type: "any"}, "status_code": {Type: "integer"}}
	listOut := plugin.Schema{"items": {Type: "list"}, "next_offset": {Type: "string", Desc: "offset of the next page (empty when there is none)"}, "status_code": {Type: "integer"}}
	taskOut := plugin.Schema{
		"gid": {Type: "string"}, "name": {Type: "string"}, "url": {Type: "string", Desc: "the task's permalink_url"},
		"result": {Type: "any"}, "status_code": {Type: "integer"},
	}
	// paged adds the shared pagination/projection options to a collection verb.
	paged := func(s plugin.Schema) plugin.Schema {
		out := plugin.Schema{
			"limit":      {Type: "integer", Desc: "page size, 1-100 (Asana's default when omitted; 100 when all is set)"},
			"offset":     {Type: "string", Desc: "pagination offset from a previous next_offset"},
			"all":        {Type: "boolean", Desc: "follow next_page until exhausted (bounded to 100 pages)"},
			"opt_fields": {Type: "list", Desc: "fields to include on each compact record, e.g. [name, completed, assignee.name]; a comma-joined string also works"},
		}
		for k, v := range s {
			out[k] = v
		}
		return out
	}

	filters := plugin.Schema{
		"actions":        {Type: "list", Desc: "Asana actions to match: added, changed, removed, deleted, undeleted"},
		"fields":         {Type: "list", Desc: "changed field names (change.field), e.g. completed, assignee, due_on, name"},
		"subtypes":       {Type: "list", Desc: "resource_subtype values, e.g. default_task, milestone, comment_added"},
		"resource_types": {Type: "list", Desc: "resource_type values (task, story, project, section, tag, ...)"},
		"projects":       {Type: "list", Desc: "project names — matches the enriched task's primary (first) project"},
		"assignees":      {Type: "list", Desc: "assignee display names (enriched task events)"},
		"users":          {Type: "list", Desc: "gids of the user who caused the event"},
		"completed":      {Type: "boolean", Desc: "enriched task completion state"},
		"action":         {Type: "string"},
		"field":          {Type: "string"},
		"subtype":        {Type: "string"},
		"resource_type":  {Type: "string"},
		"project":        {Type: "string"},
		"assignee":       {Type: "string"},
		"user":           {Type: "string"},
	}
	baseContext := plugin.Schema{
		"gid":           {Type: "string"},
		"action":        {Type: "string", Desc: "added / changed / removed / deleted / undeleted"},
		"resource_type": {Type: "string"},
		"subtype":       {Type: "string", Desc: "resource_subtype"},
		"parent_gid":    {Type: "string"},
		"parent_type":   {Type: "string"},
		"field":         {Type: "string", Desc: "change.field when action is changed"},
		"change_action": {Type: "string", Desc: "change.action (changed / added / removed)"},
		"new_value":     {Type: "any", Desc: "change.new_value when Asana includes it"},
		"user":          {Type: "string", Desc: "gid of the acting user"},
		"created_at":    {Type: "string"},
	}
	taskContext := plugin.Schema{
		"name": {Type: "string"}, "completed": {Type: "boolean"}, "assignee": {Type: "string"}, "assignee_gid": {Type: "string"},
		"due_on": {Type: "string"}, "due_at": {Type: "string"}, "project": {Type: "string", Desc: "primary (first) project name"},
		"project_gid": {Type: "string"}, "project_names": {Type: "list"}, "project_gids": {Type: "list"},
		"section": {Type: "string", Desc: "section name in the primary project"}, "sections": {Type: "list"},
		"tags": {Type: "list"}, "workspace": {Type: "string"}, "url": {Type: "string"},
	}
	for k, v := range baseContext {
		taskContext[k] = v
	}
	storyContext := plugin.Schema{
		"text": {Type: "string"}, "type": {Type: "string", Desc: "comment / system"}, "author": {Type: "string"},
		"task_gid": {Type: "string"}, "task": {Type: "string", Desc: "the parent task's name"},
	}
	for k, v := range baseContext {
		storyContext[k] = v
	}
	projectContext := plugin.Schema{
		"name": {Type: "string"}, "url": {Type: "string"}, "archived": {Type: "boolean"},
		"owner": {Type: "string"}, "team": {Type: "string"}, "workspace": {Type: "string"},
	}
	for k, v := range baseContext {
		projectContext[k] = v
	}

	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "asana",
		Desc: "Asana: tasks, projects, sections, comments, tags, search and webhook management over the REST API, plus a raw `api` escape hatch; task/story/project webhook events in (handshake + HMAC verified). Authenticates with a personal access token (`token`) or conductor's managed OAuth2 (`auth:` block + `conductor connector auth asana`).",
		Connection: plugin.Schema{
			"token":    {Type: "string", Desc: "Asana personal access token, sent as Authorization: Bearer — the fallback credential when no managed auth: token is configured"},
			"api_base": {Type: "string", Desc: "override https://app.asana.com/api/1.0 (tests, or a proxy)"},
			"webhook":  {Type: "map", Desc: "source transport: listen (required), path (default /asana), secret (pinned X-Hook-Secret, string or list), handshake (true: always accept handshakes; false: never; unset: exactly one, only while no secret is pinned), allow_unsigned, enrich (default true), opt_fields (task enrichment fields)"},
		},
		Events: []plugin.Event{
			{
				Name: "task", Desc: "an Asana task was added, changed, removed (from its parent), deleted, or undeleted",
				Filters: filters, Context: taskContext,
			},
			{
				Name: "story", Desc: "a story (comment or system activity) was added to, changed on, or removed from a task",
				Filters: filters, Context: storyContext,
			},
			{
				Name: "project", Desc: "an Asana project was added, changed, removed, or deleted",
				Filters: filters, Context: projectContext,
			},
			{
				Name: "event", Desc: "catch-all: any other Asana webhook event (section, tag, attachment, workspace, ...)",
				Filters: filters, Context: baseContext,
			},
		},
		Verbs: []plugin.Verb{
			{
				Name: "me", Desc: "the authenticated user, including the workspaces the token can reach",
				Usage: "GET /users/me", Options: plugin.Schema{}, Outputs: resultOut,
			},
			{
				Name: "workspaces", Desc: "list workspaces and organizations visible to the token",
				Usage: "GET /workspaces", Options: paged(plugin.Schema{}), Outputs: listOut,
			},
			{
				Name: "users", Desc: "list users in a workspace or team",
				Usage: "GET /users",
				Options: paged(plugin.Schema{
					"workspace": {Type: "string", Scope: "workspace", Desc: "workspace gid to filter on"},
					"team":      {Type: "string", Desc: "team gid to filter on"},
				}),
				Outputs: listOut,
			},
			{
				Name: "projects", Desc: "list projects",
				Usage: "GET /projects",
				Options: paged(plugin.Schema{
					"workspace": {Type: "string", Scope: "workspace"},
					"team":      {Type: "string"},
					"archived":  {Type: "boolean", Desc: "only archived (true) or only active (false) projects"},
				}),
				Outputs: listOut,
			},
			{
				Name: "get_project", Desc: "read one project",
				Usage: "GET /projects/{project}",
				Options: plugin.Schema{
					"project":    {Type: "string", Required: true, Scope: "project"},
					"opt_fields": {Type: "list"},
				},
				Outputs: resultOut,
			},
			{
				Name: "sections", Desc: "list a project's sections (columns)",
				Usage: "GET /projects/{project}/sections",
				Options: paged(plugin.Schema{
					"project": {Type: "string", Required: true, Scope: "project"},
				}),
				Outputs: listOut,
			},
			{
				Name: "tasks", Desc: "list tasks in a project, section, tag, or user task list, or by assignee within a workspace",
				Usage: "GET /tasks (or /tags/{tag}/tasks, /user_task_lists/{list}/tasks)",
				Options: paged(plugin.Schema{
					"project":         {Type: "string", Scope: "project"},
					"section":         {Type: "string"},
					"tag":             {Type: "string"},
					"user_task_list":  {Type: "string"},
					"assignee":        {Type: "string", Desc: "user gid, email, or \"me\" — requires workspace"},
					"workspace":       {Type: "string", Scope: "workspace"},
					"completed_since": {Type: "string", Desc: "RFC3339 timestamp, or \"now\" for only-incomplete tasks"},
					"modified_since":  {Type: "string", Desc: "RFC3339 timestamp"},
				}),
				Outputs: listOut,
			},
			{
				Name: "get_task", Desc: "read one task",
				Usage: "GET /tasks/{task}",
				Options: plugin.Schema{
					"task":       {Type: "string", Required: true, Scope: "task"},
					"opt_fields": {Type: "list"},
				},
				Outputs: taskOut,
			},
			{
				Name: "create_task", Desc: "create a task (or a subtask when parent is set)",
				Usage: "POST /tasks — one of workspace, projects, or parent is required",
				Options: plugin.Schema{
					"name":          {Type: "string", Required: true},
					"workspace":     {Type: "string", Scope: "workspace"},
					"projects":      {Type: "list", Desc: "project gids to add the task to"},
					"section":       {Type: "string", Desc: "section gid within the first project (sent as a membership)"},
					"parent":        {Type: "string", Desc: "parent task gid — makes this a subtask"},
					"notes":         {Type: "string", Desc: "plain-text description"},
					"html_notes":    {Type: "string", Desc: "rich-text description (<body>...</body>)"},
					"assignee":      {Type: "string", Desc: "user gid, email, or \"me\""},
					"due_on":        {Type: "string", Desc: "YYYY-MM-DD"},
					"due_at":        {Type: "string", Desc: "RFC3339 timestamp (mutually exclusive with due_on)"},
					"start_on":      {Type: "string", Desc: "YYYY-MM-DD"},
					"completed":     {Type: "boolean"},
					"tags":          {Type: "list", Desc: "tag gids"},
					"followers":     {Type: "list", Desc: "user gids"},
					"custom_fields": {Type: "map", Desc: "{custom_field_gid: value}"},
					"fields":        {Type: "map", Desc: "raw Asana task fields, merged in last (overrides the shortcuts above)"},
				},
				Outputs: taskOut,
			},
			{
				Name: "update_task", Desc: "edit a task's fields",
				Usage: "PUT /tasks/{task}",
				Options: plugin.Schema{
					"task":          {Type: "string", Required: true, Scope: "task"},
					"name":          {Type: "string"},
					"notes":         {Type: "string"},
					"html_notes":    {Type: "string"},
					"assignee":      {Type: "string", Desc: "user gid, email, \"me\", or null-string \"\" is ignored — use fields: {assignee: null} to unassign"},
					"due_on":        {Type: "string"},
					"due_at":        {Type: "string"},
					"start_on":      {Type: "string"},
					"completed":     {Type: "boolean"},
					"custom_fields": {Type: "map"},
					"fields":        {Type: "map", Desc: "raw Asana task fields, merged in last"},
				},
				Outputs: taskOut,
			},
			{
				Name: "complete_task", Desc: "mark a task complete (or incomplete with completed: false)",
				Usage: "PUT /tasks/{task} {completed}",
				Options: plugin.Schema{
					"task":      {Type: "string", Required: true, Scope: "task"},
					"completed": {Type: "boolean", Desc: "default true"},
				},
				Outputs: taskOut,
			},
			{
				Name: "delete_task", Desc: "delete a task (recoverable from Asana's trash for 30 days)",
				Usage: "DELETE /tasks/{task}",
				Options: plugin.Schema{
					"task": {Type: "string", Required: true, Scope: "task"},
				},
				Outputs: okOut,
			},
			{
				Name: "add_comment", Desc: "post a comment (story) on a task",
				Usage: "POST /tasks/{task}/stories",
				Options: plugin.Schema{
					"task":      {Type: "string", Required: true, Scope: "task"},
					"text":      {Type: "string", Desc: "plain-text comment (required unless html_text is given)"},
					"html_text": {Type: "string", Desc: "rich-text comment (<body>...</body>)"},
					"is_pinned": {Type: "boolean"},
				},
				Outputs: plugin.Schema{"gid": {Type: "string"}, "result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "stories", Desc: "list a task's stories (comments and system activity)",
				Usage: "GET /tasks/{task}/stories",
				Options: paged(plugin.Schema{
					"task": {Type: "string", Required: true, Scope: "task"},
				}),
				Outputs: listOut,
			},
			{
				Name: "subtasks", Desc: "list a task's subtasks",
				Usage: "GET /tasks/{task}/subtasks",
				Options: paged(plugin.Schema{
					"task": {Type: "string", Required: true, Scope: "task"},
				}),
				Outputs: listOut,
			},
			{
				Name: "add_to_project", Desc: "add a task to a project, optionally into a section",
				Usage: "POST /tasks/{task}/addProject",
				Options: plugin.Schema{
					"task":          {Type: "string", Required: true, Scope: "task"},
					"project":       {Type: "string", Required: true, Scope: "project"},
					"section":       {Type: "string"},
					"insert_before": {Type: "string", Desc: "task gid to insert before"},
					"insert_after":  {Type: "string", Desc: "task gid to insert after"},
				},
				Outputs: okOut,
			},
			{
				Name: "remove_from_project", Desc: "remove a task from a project",
				Usage: "POST /tasks/{task}/removeProject",
				Options: plugin.Schema{
					"task":    {Type: "string", Required: true, Scope: "task"},
					"project": {Type: "string", Required: true, Scope: "project"},
				},
				Outputs: okOut,
			},
			{
				Name: "move_to_section", Desc: "move a task into a section (board column) of a project it is already in",
				Usage: "POST /sections/{section}/addTask",
				Options: plugin.Schema{
					"section":       {Type: "string", Required: true},
					"task":          {Type: "string", Required: true, Scope: "task"},
					"insert_before": {Type: "string"},
					"insert_after":  {Type: "string"},
				},
				Outputs: okOut,
			},
			{
				Name: "add_tag", Desc: "add a tag to a task",
				Usage: "POST /tasks/{task}/addTag",
				Options: plugin.Schema{
					"task": {Type: "string", Required: true, Scope: "task"},
					"tag":  {Type: "string", Required: true},
				},
				Outputs: okOut,
			},
			{
				Name: "remove_tag", Desc: "remove a tag from a task",
				Usage: "POST /tasks/{task}/removeTag",
				Options: plugin.Schema{
					"task": {Type: "string", Required: true, Scope: "task"},
					"tag":  {Type: "string", Required: true},
				},
				Outputs: okOut,
			},
			{
				Name: "tags", Desc: "list a workspace's tags",
				Usage: "GET /tags",
				Options: paged(plugin.Schema{
					"workspace": {Type: "string", Required: true, Scope: "workspace"},
				}),
				Outputs: listOut,
			},
			{
				Name: "search_tasks", Desc: "search tasks in a workspace (Asana premium workspaces only)",
				Usage: "GET /workspaces/{workspace}/tasks/search",
				Options: plugin.Schema{
					"workspace":      {Type: "string", Required: true, Scope: "workspace"},
					"text":           {Type: "string", Desc: "full-text query"},
					"completed":      {Type: "boolean"},
					"assignee":       {Type: "list", Desc: "user gids/emails/\"me\" → assignee.any"},
					"projects":       {Type: "list", Desc: "project gids → projects.any"},
					"sections":       {Type: "list", Desc: "section gids → sections.any"},
					"tags":           {Type: "list", Desc: "tag gids → tags.any"},
					"sort_by":        {Type: "string", Enum: []string{"due_date", "created_at", "completed_at", "likes", "modified_at"}},
					"sort_ascending": {Type: "boolean"},
					"limit":          {Type: "integer", Desc: "1-100"},
					"opt_fields":     {Type: "list"},
					"query":          {Type: "map", Desc: "any other search parameter, passed straight through (e.g. {\"due_on.before\": \"2026-10-01\", \"projects.not\": \"123\"})"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "typeahead", Desc: "resolve names to objects: quick search for tasks, users, projects, tags, teams, ... in a workspace",
				Usage: "GET /workspaces/{workspace}/typeahead",
				Options: plugin.Schema{
					"workspace":     {Type: "string", Required: true, Scope: "workspace"},
					"resource_type": {Type: "string", Required: true, Enum: []string{"task", "user", "project", "portfolio", "tag", "team", "custom_field", "project_template"}},
					"query":         {Type: "string", Desc: "the text to match (omit for the most relevant results)"},
					"count":         {Type: "integer", Desc: "1-100 (Asana default 20)"},
					"opt_fields":    {Type: "list"},
				},
				Outputs: plugin.Schema{"items": {Type: "list"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "webhooks", Desc: "list the webhooks registered by this token in a workspace",
				Usage: "GET /webhooks",
				Options: paged(plugin.Schema{
					"workspace": {Type: "string", Required: true, Scope: "workspace"},
					"resource":  {Type: "string", Desc: "only webhooks on this resource gid"},
				}),
				Outputs: listOut,
			},
			{
				Name: "create_webhook", Desc: "register a webhook — Asana handshakes the target during this call, so the source listener must already be up",
				Usage: "POST /webhooks",
				Options: plugin.Schema{
					"resource": {Type: "string", Required: true, Desc: "gid of the project, task, portfolio, team, or workspace to watch"},
					"target":   {Type: "string", Required: true, Desc: "public https URL of this connector's webhook listener, e.g. https://hooks.example.com/asana"},
					"filters":  {Type: "list", Desc: "[{resource_type, resource_subtype, action, fields}] — required for workspace/team/portfolio resources"},
				},
				Outputs: plugin.Schema{"gid": {Type: "string"}, "result": {Type: "any"}, "status_code": {Type: "integer"}},
			},
			{
				Name: "delete_webhook", Desc: "delete a webhook",
				Usage: "DELETE /webhooks/{webhook}",
				Options: plugin.Schema{
					"webhook": {Type: "string", Required: true},
				},
				Outputs: okOut,
			},
			{
				Name: "api", Desc: "call any Asana REST endpoint not covered by a first-class verb",
				Usage: "escape hatch: method + path (relative to /api/1.0), optional query and body (wrapped in {data: ...} unless it already is)",
				Options: plugin.Schema{
					"method": {Type: "string", Required: true, Enum: []string{"GET", "POST", "PUT", "DELETE"}},
					"path":   {Type: "string", Required: true, Desc: "e.g. \"tasks/123/attachments\""},
					"query":  {Type: "map", Desc: "query string parameters (lists are comma-joined)"},
					"body":   {Type: "any", Desc: "JSON request body"},
				},
				Outputs: plugin.Schema{"result": {Type: "any"}, "items": {Type: "list"}, "next_offset": {Type: "string"}, "status_code": {Type: "integer"}},
			},
		},
		// Conductor performs the OAuth2 exchange with app.asana.com/-/oauth_token
		// on this plugin's behalf; the plugin itself only ever calls the REST
		// API on the same host.
		Capabilities: plugin.Capabilities{Egress: []string{"app.asana.com:443"}},
		Auth: &plugin.AuthSpec{
			Grants:   []string{"authorization_code", "refresh_token"},
			TokenURL: "https://app.asana.com/-/oauth_token",
			AuthURL:  "https://app.asana.com/-/oauth_authorize",
			Scopes: []string{
				"tasks:read", "tasks:write", "tasks:delete",
				"projects:read", "stories:read", "stories:write", "tags:read",
				"users:read", "workspaces:read", "workspaces.typeahead:read",
				"webhooks:read", "webhooks:write", "webhooks:delete",
			},
		},
	}
}

// --- connection + HTTP transport ---

type asanaConn struct {
	apiBase string
}

// url joins a path (relative to the API root) and a query string.
func (c asanaConn) url(path string, q url.Values) string {
	u := c.apiBase + "/" + strings.TrimLeft(path, "/")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// parseConn reads the connection map, resolving the bearer credential in
// priority order: conductor's managed OAuth2 token first (plugin.AccessToken),
// then the plain `token` connection field. If neither is present no request
// is sent — the operator is pointed at how to configure either path.
func parseConn(m map[string]any) (asanaConn, string, error) {
	token := plugin.AccessToken(m)
	if token == "" {
		token = str(m["token"])
	}
	if token == "" {
		return asanaConn{}, "", fmt.Errorf("no credentials — set token (an Asana personal access token), or configure an auth: block and run `conductor connector auth asana`")
	}
	base := strings.TrimRight(strOr(str(m["api_base"]), defaultAPIBase), "/")
	return asanaConn{apiBase: base}, token, nil
}

// do sends one request to the Asana API with the bearer token attached and
// returns the status and raw body. payload, when non-nil, is JSON-encoded as
// the request body exactly as given — callers wrap resource data in Asana's
// {"data": ...} envelope with wrap(). A non-2xx response is a
// plugin.CodeInternalError carrying the status and body (Asana's own
// {"errors":[{"message":...}]}), so nothing is swallowed.
func (p *asanaPlugin) do(ctx context.Context, token, method, u string, payload any) (int, []byte, error) {
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, plugin.Errorf(plugin.CodeInvalidParams, "encoding request body: "+err.Error())
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, "asana: building request: "+err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, plugin.Errorf(plugin.CodeInternalError, "asana: request failed: "+err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, nil, plugin.Errorf(plugin.CodeInternalError, "asana: reading response: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, body, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("asana API %s: %s", resp.Status, strings.TrimSpace(string(body))))
	}
	return resp.StatusCode, body, nil
}

// wrap puts resource data in Asana's request envelope.
func wrap(data any) map[string]any { return map[string]any{"data": data} }

// apiEnvelope is the subset of Asana's response envelope this plugin reads.
type apiEnvelope struct {
	Data     json.RawMessage `json:"data"`
	NextPage *struct {
		Offset string `json:"offset"`
	} `json:"next_page"`
}

// decodeEnvelope unwraps {"data": ..., "next_page": {...}} into the decoded
// data value and the next page's offset (empty when there is none). An empty
// body decodes to nil data.
func decodeEnvelope(body []byte) (any, string, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, "", nil
	}
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, "", fmt.Errorf("asana: decoding response: %w: %s", err, strings.TrimSpace(string(body)))
	}
	var data any
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, "", fmt.Errorf("asana: decoding response data: %w", err)
		}
	}
	next := ""
	if env.NextPage != nil {
		next = env.NextPage.Offset
	}
	return data, next, nil
}

// call is do + decodeEnvelope for the single-resource case.
func (p *asanaPlugin) call(ctx context.Context, token, method, u string, payload any) (any, int, error) {
	status, body, err := p.do(ctx, token, method, u, payload)
	if err != nil {
		return nil, status, err
	}
	data, _, derr := decodeEnvelope(body)
	if derr != nil {
		return nil, status, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	return data, status, nil
}

// listVerb runs a paginated collection GET, honoring the shared limit /
// offset / all / opt_fields options, and returns items + next_offset.
func (p *asanaPlugin) listVerb(ctx context.Context, conn asanaConn, token, path string, q url.Values, o map[string]any) (plugin.InvokeResult, error) {
	if q == nil {
		q = url.Values{}
	}
	all := boolv(o["all"])
	if lim := intv(o["limit"]); lim > 0 {
		q.Set("limit", strconv.Itoa(lim))
	} else if all {
		q.Set("limit", "100")
	}
	if off := str(o["offset"]); off != "" {
		q.Set("offset", off)
	}
	addOptFields(q, o["opt_fields"])

	items := []any{}
	status, next := 0, ""
	for page := 0; ; page++ {
		st, body, err := p.do(ctx, token, http.MethodGet, conn.url(path, q), nil)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		status = st
		data, off, derr := decodeEnvelope(body)
		if derr != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
		}
		if list, ok := data.([]any); ok {
			items = append(items, list...)
		}
		next = off
		if !all || off == "" || page >= maxPages-1 {
			break
		}
		q.Set("offset", off)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "next_offset": next, "status_code": status}}, nil
}

func (p *asanaPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, token, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	ctx := context.Background()

	switch req.Verb {
	case "me":
		return p.single(ctx, conn, token, "users/me", nil)
	case "workspaces":
		return p.listVerb(ctx, conn, token, "workspaces", nil, o)
	case "users":
		q := url.Values{}
		setQ(q, "workspace", o["workspace"])
		setQ(q, "team", o["team"])
		return p.listVerb(ctx, conn, token, "users", q, o)
	case "projects":
		q := url.Values{}
		setQ(q, "workspace", o["workspace"])
		setQ(q, "team", o["team"])
		if o["archived"] != nil {
			q.Set("archived", strconv.FormatBool(boolv(o["archived"])))
		}
		return p.listVerb(ctx, conn, token, "projects", q, o)
	case "get_project":
		project := str(o["project"])
		if project == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "get_project: project is required")
		}
		return p.single(ctx, conn, token, "projects/"+url.PathEscape(project), o["opt_fields"])
	case "sections":
		project := str(o["project"])
		if project == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "sections: project is required")
		}
		return p.listVerb(ctx, conn, token, "projects/"+url.PathEscape(project)+"/sections", nil, o)
	case "tasks":
		return p.tasks(ctx, conn, token, o)
	case "get_task":
		task := str(o["task"])
		if task == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "get_task: task is required")
		}
		q := url.Values{}
		addOptFields(q, o["opt_fields"])
		data, status, err := p.call(ctx, token, http.MethodGet, conn.url("tasks/"+url.PathEscape(task), q), nil)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return taskResult(data, status), nil
	case "create_task":
		return p.createTask(ctx, conn, token, o)
	case "update_task":
		return p.updateTask(ctx, conn, token, o)
	case "complete_task":
		task := str(o["task"])
		if task == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "complete_task: task is required")
		}
		completed := true
		if o["completed"] != nil {
			completed = boolv(o["completed"])
		}
		data, status, err := p.call(ctx, token, http.MethodPut, conn.url("tasks/"+url.PathEscape(task), nil), wrap(map[string]any{"completed": completed}))
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return taskResult(data, status), nil
	case "delete_task":
		task := str(o["task"])
		if task == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "delete_task: task is required")
		}
		return p.action(ctx, token, http.MethodDelete, conn.url("tasks/"+url.PathEscape(task), nil), nil)
	case "add_comment":
		return p.addComment(ctx, conn, token, o)
	case "stories":
		task := str(o["task"])
		if task == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "stories: task is required")
		}
		return p.listVerb(ctx, conn, token, "tasks/"+url.PathEscape(task)+"/stories", nil, o)
	case "subtasks":
		task := str(o["task"])
		if task == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "subtasks: task is required")
		}
		return p.listVerb(ctx, conn, token, "tasks/"+url.PathEscape(task)+"/subtasks", nil, o)
	case "add_to_project":
		task, project := str(o["task"]), str(o["project"])
		if task == "" || project == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "add_to_project: task and project are required")
		}
		data := map[string]any{"project": project}
		setIfPresent(data, "section", o["section"])
		setIfPresent(data, "insert_before", o["insert_before"])
		setIfPresent(data, "insert_after", o["insert_after"])
		return p.action(ctx, token, http.MethodPost, conn.url("tasks/"+url.PathEscape(task)+"/addProject", nil), wrap(data))
	case "remove_from_project":
		task, project := str(o["task"]), str(o["project"])
		if task == "" || project == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "remove_from_project: task and project are required")
		}
		return p.action(ctx, token, http.MethodPost, conn.url("tasks/"+url.PathEscape(task)+"/removeProject", nil), wrap(map[string]any{"project": project}))
	case "move_to_section":
		section, task := str(o["section"]), str(o["task"])
		if section == "" || task == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "move_to_section: section and task are required")
		}
		data := map[string]any{"task": task}
		setIfPresent(data, "insert_before", o["insert_before"])
		setIfPresent(data, "insert_after", o["insert_after"])
		return p.action(ctx, token, http.MethodPost, conn.url("sections/"+url.PathEscape(section)+"/addTask", nil), wrap(data))
	case "add_tag", "remove_tag":
		task, tag := str(o["task"]), str(o["tag"])
		if task == "" || tag == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": task and tag are required")
		}
		op := "addTag"
		if req.Verb == "remove_tag" {
			op = "removeTag"
		}
		return p.action(ctx, token, http.MethodPost, conn.url("tasks/"+url.PathEscape(task)+"/"+op, nil), wrap(map[string]any{"tag": tag}))
	case "tags":
		workspace := str(o["workspace"])
		if workspace == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "tags: workspace is required")
		}
		q := url.Values{"workspace": {workspace}}
		return p.listVerb(ctx, conn, token, "tags", q, o)
	case "search_tasks":
		return p.searchTasks(ctx, conn, token, o)
	case "typeahead":
		return p.typeahead(ctx, conn, token, o)
	case "webhooks":
		workspace := str(o["workspace"])
		if workspace == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "webhooks: workspace is required")
		}
		q := url.Values{"workspace": {workspace}}
		setQ(q, "resource", o["resource"])
		return p.listVerb(ctx, conn, token, "webhooks", q, o)
	case "create_webhook":
		resource, target := str(o["resource"]), str(o["target"])
		if resource == "" || target == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "create_webhook: resource and target are required")
		}
		data := map[string]any{"resource": resource, "target": target}
		if f, ok := o["filters"].([]any); ok && len(f) > 0 {
			data["filters"] = f
		}
		result, status, err := p.call(ctx, token, http.MethodPost, conn.url("webhooks", nil), wrap(data))
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		m, _ := result.(map[string]any)
		return plugin.InvokeResult{Outputs: map[string]any{"gid": gs(m, "gid"), "result": result, "status_code": status}}, nil
	case "delete_webhook":
		webhook := str(o["webhook"])
		if webhook == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "delete_webhook: webhook is required")
		}
		return p.action(ctx, token, http.MethodDelete, conn.url("webhooks/"+url.PathEscape(webhook), nil), nil)
	case "api":
		return p.api(ctx, conn, token, o)
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
}

// single GETs one resource and returns {result, status_code}.
func (p *asanaPlugin) single(ctx context.Context, conn asanaConn, token, path string, optFields any) (plugin.InvokeResult, error) {
	q := url.Values{}
	addOptFields(q, optFields)
	data, status, err := p.call(ctx, token, http.MethodGet, conn.url(path, q), nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": data, "status_code": status}}, nil
}

// action runs a fire-and-forget mutation (addProject, addTag, DELETE ...) and
// returns {ok, result, status_code}.
func (p *asanaPlugin) action(ctx context.Context, token, method, u string, payload any) (plugin.InvokeResult, error) {
	data, status, err := p.call(ctx, token, method, u, payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: map[string]any{"ok": true, "result": data, "status_code": status}}, nil
}

// taskResult hoists a task record's gid, name and permalink_url next to the
// full result.
func taskResult(data any, status int) plugin.InvokeResult {
	m, _ := data.(map[string]any)
	return plugin.InvokeResult{Outputs: map[string]any{
		"gid": gs(m, "gid"), "name": gs(m, "name"), "url": gs(m, "permalink_url"),
		"result": data, "status_code": status,
	}}
}

func (p *asanaPlugin) tasks(ctx context.Context, conn asanaConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	project, section, tag := str(o["project"]), str(o["section"]), str(o["tag"])
	utl, assignee, workspace := str(o["user_task_list"]), str(o["assignee"]), str(o["workspace"])
	q := url.Values{}
	setQ(q, "completed_since", o["completed_since"])
	setQ(q, "modified_since", o["modified_since"])
	switch {
	case tag != "":
		return p.listVerb(ctx, conn, token, "tags/"+url.PathEscape(tag)+"/tasks", q, o)
	case utl != "":
		return p.listVerb(ctx, conn, token, "user_task_lists/"+url.PathEscape(utl)+"/tasks", q, o)
	case project != "" || section != "" || (assignee != "" && workspace != ""):
		setQ(q, "project", project)
		setQ(q, "section", section)
		setQ(q, "assignee", assignee)
		setQ(q, "workspace", workspace)
		return p.listVerb(ctx, conn, token, "tasks", q, o)
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "tasks: one of project, section, tag, user_task_list, or assignee+workspace is required")
}

// taskFields collects the task shortcut options shared by create_task and
// update_task into an Asana task body, with `fields` merged in last.
func taskFields(o map[string]any) map[string]any {
	data := map[string]any{}
	for _, k := range []string{"name", "notes", "html_notes", "assignee", "due_on", "due_at", "start_on"} {
		setIfPresent(data, k, o[k])
	}
	if o["completed"] != nil {
		data["completed"] = boolv(o["completed"])
	}
	if cf, ok := o["custom_fields"].(map[string]any); ok && len(cf) > 0 {
		data["custom_fields"] = cf
	}
	if extra, ok := o["fields"].(map[string]any); ok {
		for k, v := range extra {
			data[k] = v
		}
	}
	return data
}

func (p *asanaPlugin) createTask(ctx context.Context, conn asanaConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	if str(o["name"]) == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "create_task: name is required")
	}
	data := map[string]any{}
	setIfPresent(data, "workspace", o["workspace"])
	setIfPresent(data, "parent", o["parent"])
	projects := strList(o["projects"])
	if section := str(o["section"]); section != "" {
		if len(projects) == 0 {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "create_task: section requires projects (the section's project must be listed first)")
		}
		// Asana places a task in a section via a membership; the same project
		// must not also appear in `projects`, so the first one moves over.
		data["memberships"] = []any{map[string]any{"project": projects[0], "section": section}}
		projects = projects[1:]
	}
	if len(projects) > 0 {
		data["projects"] = projects
	}
	if tags := strList(o["tags"]); len(tags) > 0 {
		data["tags"] = tags
	}
	if followers := strList(o["followers"]); len(followers) > 0 {
		data["followers"] = followers
	}
	for k, v := range taskFields(o) {
		data[k] = v
	}
	if data["workspace"] == nil && data["projects"] == nil && data["memberships"] == nil && data["parent"] == nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "create_task: one of workspace, projects, or parent is required")
	}
	result, status, err := p.call(ctx, token, http.MethodPost, conn.url("tasks", nil), wrap(data))
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return taskResult(result, status), nil
}

func (p *asanaPlugin) updateTask(ctx context.Context, conn asanaConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	task := str(o["task"])
	if task == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "update_task: task is required")
	}
	data := taskFields(o)
	if len(data) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "update_task: nothing to update — pass at least one field")
	}
	result, status, err := p.call(ctx, token, http.MethodPut, conn.url("tasks/"+url.PathEscape(task), nil), wrap(data))
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return taskResult(result, status), nil
}

func (p *asanaPlugin) addComment(ctx context.Context, conn asanaConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	task := str(o["task"])
	text, htmlText := str(o["text"]), str(o["html_text"])
	if task == "" || (text == "" && htmlText == "") {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "add_comment: task and text (or html_text) are required")
	}
	data := map[string]any{}
	setIfPresent(data, "text", text)
	setIfPresent(data, "html_text", htmlText)
	if o["is_pinned"] != nil {
		data["is_pinned"] = boolv(o["is_pinned"])
	}
	result, status, err := p.call(ctx, token, http.MethodPost, conn.url("tasks/"+url.PathEscape(task)+"/stories", nil), wrap(data))
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	m, _ := result.(map[string]any)
	return plugin.InvokeResult{Outputs: map[string]any{"gid": gs(m, "gid"), "result": result, "status_code": status}}, nil
}

func (p *asanaPlugin) searchTasks(ctx context.Context, conn asanaConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	workspace := str(o["workspace"])
	if workspace == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "search_tasks: workspace is required")
	}
	q := url.Values{}
	if extra, ok := o["query"].(map[string]any); ok {
		for k, v := range extra {
			setQ(q, k, v)
		}
	}
	setQ(q, "text", o["text"])
	if o["completed"] != nil {
		q.Set("completed", strconv.FormatBool(boolv(o["completed"])))
	}
	setQ(q, "assignee.any", o["assignee"])
	setQ(q, "projects.any", o["projects"])
	setQ(q, "sections.any", o["sections"])
	setQ(q, "tags.any", o["tags"])
	setQ(q, "sort_by", o["sort_by"])
	if o["sort_ascending"] != nil {
		q.Set("sort_ascending", strconv.FormatBool(boolv(o["sort_ascending"])))
	}
	if lim := intv(o["limit"]); lim > 0 {
		q.Set("limit", strconv.Itoa(lim))
	}
	addOptFields(q, o["opt_fields"])
	data, status, err := p.call(ctx, token, http.MethodGet, conn.url("workspaces/"+url.PathEscape(workspace)+"/tasks/search", q), nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	items, _ := data.([]any)
	if items == nil {
		items = []any{}
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *asanaPlugin) typeahead(ctx context.Context, conn asanaConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	workspace, rt := str(o["workspace"]), str(o["resource_type"])
	if workspace == "" || rt == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "typeahead: workspace and resource_type are required")
	}
	q := url.Values{"resource_type": {rt}}
	setQ(q, "query", o["query"])
	if n := intv(o["count"]); n > 0 {
		q.Set("count", strconv.Itoa(n))
	}
	addOptFields(q, o["opt_fields"])
	data, status, err := p.call(ctx, token, http.MethodGet, conn.url("workspaces/"+url.PathEscape(workspace)+"/typeahead", q), nil)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	items, _ := data.([]any)
	if items == nil {
		items = []any{}
	}
	return plugin.InvokeResult{Outputs: map[string]any{"items": items, "status_code": status}}, nil
}

func (p *asanaPlugin) api(ctx context.Context, conn asanaConn, token string, o map[string]any) (plugin.InvokeResult, error) {
	method := strings.ToUpper(str(o["method"]))
	path := str(o["path"])
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete:
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "api: method must be GET, POST, PUT, or DELETE")
	}
	if path == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "api: path is required")
	}
	q := url.Values{}
	if extra, ok := o["query"].(map[string]any); ok {
		for k, v := range extra {
			setQ(q, k, v)
		}
	}
	var payload any
	if b := o["body"]; b != nil {
		if m, ok := b.(map[string]any); ok {
			if _, enveloped := m["data"]; enveloped {
				payload = m
			} else {
				payload = wrap(m)
			}
		} else {
			payload = wrap(b)
		}
	}
	status, body, err := p.do(ctx, token, method, conn.url(path, q), payload)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	data, next, derr := decodeEnvelope(body)
	if derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, derr.Error())
	}
	out := map[string]any{"result": data, "next_offset": next, "status_code": status}
	if list, ok := data.([]any); ok {
		out["items"] = list
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

// --- source: Asana webhook ingest ---

// source is one running webhook listener.
type source struct {
	instance      string
	plugin        *asanaPlugin
	conn          asanaConn
	token         string
	enrich        bool
	taskOptFields string
	allowUnsigned bool
	emit          func(any) error
	dedup         *sourcekit.Dedup

	mu            sync.Mutex
	secrets       []string
	handshake     bool // accept X-Hook-Secret handshakes right now
	handshakeOnce bool // handshake was defaulted (bootstrap): it closes after the first one
}

func (s *source) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "asana["+s.instance+"]: "+format+"\n", args...)
}

func (s *source) addSecret(secret string) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.secrets {
		if have == secret {
			return
		}
	}
	s.secrets = append(s.secrets, secret)
}

func (s *source) knownSecrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.secrets...)
}

// acceptHandshake reports whether a handshake may install a secret right
// now and, in bootstrap mode (webhook.handshake unset), closes the window so
// only the first one ever does.
func (s *source) acceptHandshake() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.handshake {
		return false
	}
	if s.handshakeOnce {
		s.handshake = false
	}
	return true
}

// verify reports whether sig (X-Hook-Signature, hex HMAC-SHA256) matches body
// under ANY known secret — pinned or handshaken. With no secret known nothing
// verifies.
func (s *source) verify(body []byte, sig string) bool {
	for _, secret := range s.knownSecrets() {
		if sourcekit.VerifyHMAC(secret, body, sig) {
			return true
		}
	}
	return false
}

func (p *asanaPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	if webhook == nil {
		webhook = map[string]any{}
	}
	addr := str(webhook["listen"])
	if addr == "" {
		return fmt.Errorf("asana: webhook.listen is required — Asana's handshake needs a synchronous X-Hook-Secret echo, which a relay cannot provide")
	}
	path := strOr(str(webhook["path"]), "/asana")

	s := &source{
		instance:      req.Instance,
		plugin:        p,
		allowUnsigned: boolv(webhook["allow_unsigned"]),
		emit:          emit,
		dedup:         sourcekit.NewDedup(4096),
	}
	for _, secret := range strList(webhook["secret"]) {
		s.addSecret(secret)
	}
	pinned := len(s.knownSecrets()) > 0
	s.handshake, s.handshakeOnce = !pinned, !pinned
	if v, ok := webhook["handshake"]; ok && v != nil {
		s.handshake, s.handshakeOnce = boolv(v), false
	}

	// Enrichment needs a credential; without one events carry gids only.
	s.enrich = true
	if v, ok := webhook["enrich"]; ok && v != nil {
		s.enrich = boolv(v)
	}
	s.taskOptFields = strOr(strings.Join(strList(webhook["opt_fields"]), ","), defaultTaskOptFields)
	if conn, token, err := parseConn(cfg); err == nil {
		s.conn, s.token = conn, token
	} else if s.enrich {
		s.enrich = false
		s.logf("no token or managed auth configured — events will carry gids only (enrichment off)")
	}

	switch {
	case s.allowUnsigned:
		s.logf("webhook.allow_unsigned is set — accepting UNSIGNED deliveries; anyone who can reach %s%s can fire triggers", addr, path)
	case s.handshake && s.handshakeOnce:
		s.logf("no webhook.secret pinned — bootstrap mode: the next Asana handshake installs its secret (printed once so you can pin it) and then the handshake window closes; signed deliveries are rejected until then")
	case s.handshake:
		s.logf("webhook.handshake is set — every handshake can install an additional signing secret")
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, s.handle)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	s.logf("listening on %s%s", addr, path)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// handle answers one HTTP request on the webhook path: the handshake (echo
// X-Hook-Secret), a heartbeat (empty events), or a signed delivery. Deliveries
// are acknowledged with 200 BEFORE enrichment so a large batch never times
// out on Asana's side.
func (s *source) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	if hs := strings.TrimSpace(r.Header.Get("X-Hook-Secret")); hs != "" {
		if !s.acceptHandshake() {
			s.logf("REFUSED a webhook handshake: a signing secret is already known and webhook.handshake is not set (set it to register another webhook)")
			http.Error(w, "handshake refused", http.StatusForbidden)
			return
		}
		s.addSecret(hs)
		s.logf("webhook handshake accepted. Pin this secret as webhook.secret so deliveries verify after a restart: %s", hs)
		w.Header().Set("X-Hook-Secret", hs)
		w.WriteHeader(http.StatusOK)
		return
	}

	if !s.verify(body, r.Header.Get("X-Hook-Signature")) {
		if !s.allowUnsigned {
			if len(s.knownSecrets()) == 0 {
				s.logf("rejected delivery: no signing secret known yet (waiting for a handshake, or pin webhook.secret)")
			} else {
				s.logf("rejected delivery: bad X-Hook-Signature")
			}
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
	}

	var d asanaDelivery
	if err := json.Unmarshal(body, &d); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	if len(d.Events) == 0 {
		return // heartbeat
	}
	go s.process(d.Events)
}

// process normalizes, enriches, dedups and emits one delivery's events, in
// order.
func (s *source) process(events []asanaEvent) {
	for _, e := range events {
		ev := s.toWire(e)
		if ev == nil {
			continue
		}
		if !s.dedup.Add(ev.Dedup) {
			continue
		}
		_ = s.emit(ev)
	}
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

// asanaDelivery is one webhook POST body: a batch of compact events. A
// heartbeat is a delivery with no events.
type asanaDelivery struct {
	Events []asanaEvent `json:"events"`
}

type asanaRef struct {
	GID             string `json:"gid"`
	ResourceType    string `json:"resource_type"`
	ResourceSubtype string `json:"resource_subtype"`
	Name            string `json:"name"`
}

type asanaEvent struct {
	Action    string    `json:"action"`
	CreatedAt string    `json:"created_at"`
	User      *asanaRef `json:"user"`
	Resource  asanaRef  `json:"resource"`
	Parent    *asanaRef `json:"parent"`
	Change    *struct {
		Field    string `json:"field"`
		Action   string `json:"action"`
		NewValue any    `json:"new_value"`
	} `json:"change"`
}

// toWire normalizes one compact Asana event into a wire event, enriching it
// from the API when a token is available. Returns nil for an event with no
// resource gid or action.
func (s *source) toWire(e asanaEvent) *wireEvent {
	gid, action := e.Resource.GID, strings.ToLower(e.Action)
	if gid == "" || action == "" {
		return nil
	}
	rt := strings.ToLower(e.Resource.ResourceType)
	subtype := e.Resource.ResourceSubtype
	field, changeAction := "", ""
	var newValue any
	if e.Change != nil {
		field, changeAction, newValue = e.Change.Field, e.Change.Action, e.Change.NewValue
	}
	parentGID, parentType := "", ""
	if e.Parent != nil {
		parentGID, parentType = e.Parent.GID, e.Parent.ResourceType
	}
	user := ""
	if e.User != nil {
		user = e.User.GID
	}

	ctx := map[string]any{
		"gid": gid, "action": action, "actions": action,
		"resource_type": rt, "resource_types": rt,
		"subtype": subtype, "subtypes": subtype,
		"parent_gid": parentGID, "parent_type": parentType,
		"field": field, "fields": field, "change_action": changeAction,
		"user": user, "users": user, "created_at": e.CreatedAt,
	}
	if newValue != nil {
		ctx["new_value"] = newValue
	}
	dedup := strings.Join([]string{gid, action, field, e.CreatedAt, parentGID}, ":")

	switch rt {
	case "task":
		name := e.Resource.Name
		if s.enrich && action != "deleted" {
			if t := s.fetch("tasks/"+url.PathEscape(gid), s.taskOptFields); t != nil {
				name = nonEmpty(gs(t, "name"), name)
				s.enrichTask(ctx, t)
			}
		}
		title := fmt.Sprintf("asana task %s %s", nonEmpty(name, gid), action)
		if field != "" {
			title += " (" + field + ")"
		}
		return &wireEvent{Event: "task", Kind: "task", Title: title, Context: ctx, Dedup: dedup}
	case "story":
		ctx["task_gid"] = parentGID
		if s.enrich && action != "deleted" {
			if st := s.fetch("stories/"+url.PathEscape(gid), storyOptFields); st != nil {
				s.enrichStory(ctx, st)
			}
		}
		sub := nonEmpty(str(ctx["subtype"]), "story")
		return &wireEvent{Event: "story", Kind: "story", Title: fmt.Sprintf("asana %s %s on task %s", sub, action, nonEmpty(str(ctx["task"]), str(ctx["task_gid"]))), Context: ctx, Dedup: dedup}
	case "project":
		name := e.Resource.Name
		if s.enrich && action != "deleted" {
			if pr := s.fetch("projects/"+url.PathEscape(gid), projectOptFields); pr != nil {
				name = nonEmpty(gs(pr, "name"), name)
				s.enrichProject(ctx, pr)
			}
		}
		title := fmt.Sprintf("asana project %s %s", nonEmpty(name, gid), action)
		if field != "" {
			title += " (" + field + ")"
		}
		return &wireEvent{Event: "project", Kind: "project", Title: title, Context: ctx, Dedup: dedup}
	}
	return &wireEvent{Event: "event", Kind: rt, Title: fmt.Sprintf("asana %s %s %s", nonEmpty(rt, "resource"), gid, action), Context: ctx, Dedup: dedup}
}

// fetch GETs one resource for enrichment, returning nil (and logging) on any
// failure so a flaky API never drops the underlying event.
func (s *source) fetch(path, optFields string) map[string]any {
	q := url.Values{}
	if optFields != "" {
		q.Set("opt_fields", optFields)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	data, _, err := s.plugin.call(ctx, s.token, http.MethodGet, s.conn.url(path, q), nil)
	if err != nil {
		s.logf("enrichment GET %s failed — emitting the compact event: %v", path, err)
		return nil
	}
	m, _ := data.(map[string]any)
	return m
}

// enrichTask copies the fields triggers filter on and template with out of a
// task record into the event context. Filterable values are scalars (the
// daemon's evaluator stringifies each context value); the full lists ride
// along under *_names / *_gids.
func (s *source) enrichTask(ctx map[string]any, t map[string]any) {
	ctx["name"] = gs(t, "name")
	completed, _ := t["completed"].(bool)
	ctx["completed"] = completed
	assignee, _ := t["assignee"].(map[string]any)
	ctx["assignee"], ctx["assignees"] = gs(assignee, "name"), gs(assignee, "name")
	ctx["assignee_gid"] = gs(assignee, "gid")
	ctx["due_on"], ctx["due_at"] = str(t["due_on"]), str(t["due_at"])
	ctx["url"] = gs(t, "permalink_url")
	ws, _ := t["workspace"].(map[string]any)
	ctx["workspace"] = gs(ws, "name")
	if sub := gs(t, "resource_subtype"); sub != "" {
		ctx["subtype"], ctx["subtypes"] = sub, sub
	}

	names, gids := []any{}, []any{}
	if projects, ok := t["projects"].([]any); ok {
		for _, p := range projects {
			pm, _ := p.(map[string]any)
			names, gids = append(names, gs(pm, "name")), append(gids, gs(pm, "gid"))
		}
	}
	ctx["project_names"], ctx["project_gids"] = names, gids
	first, firstGID := "", ""
	if len(names) > 0 {
		first, _ = names[0].(string)
		firstGID, _ = gids[0].(string)
	}
	ctx["project"], ctx["projects"], ctx["project_gid"] = first, first, firstGID

	sections := []any{}
	section := ""
	if memberships, ok := t["memberships"].([]any); ok {
		for _, m := range memberships {
			mm, _ := m.(map[string]any)
			sec, _ := mm["section"].(map[string]any)
			proj, _ := mm["project"].(map[string]any)
			sections = append(sections, gs(sec, "name"))
			if section == "" && (first == "" || gs(proj, "name") == first) {
				section = gs(sec, "name")
			}
		}
	}
	ctx["section"], ctx["sections"] = section, sections

	tags := []any{}
	if tl, ok := t["tags"].([]any); ok {
		for _, tg := range tl {
			tm, _ := tg.(map[string]any)
			tags = append(tags, gs(tm, "name"))
		}
	}
	ctx["tags"] = tags
}

func (s *source) enrichStory(ctx map[string]any, st map[string]any) {
	ctx["text"] = gs(st, "text")
	ctx["type"] = gs(st, "type")
	if sub := gs(st, "resource_subtype"); sub != "" {
		ctx["subtype"], ctx["subtypes"] = sub, sub
	}
	by, _ := st["created_by"].(map[string]any)
	ctx["author"] = gs(by, "name")
	if str(ctx["user"]) == "" {
		ctx["user"], ctx["users"] = gs(by, "gid"), gs(by, "gid")
	}
	target, _ := st["target"].(map[string]any)
	ctx["task"] = gs(target, "name")
	if str(ctx["task_gid"]) == "" {
		ctx["task_gid"] = gs(target, "gid")
	}
}

func (s *source) enrichProject(ctx map[string]any, pr map[string]any) {
	ctx["name"] = gs(pr, "name")
	ctx["url"] = gs(pr, "permalink_url")
	archived, _ := pr["archived"].(bool)
	ctx["archived"] = archived
	owner, _ := pr["owner"].(map[string]any)
	ctx["owner"] = gs(owner, "name")
	team, _ := pr["team"].(map[string]any)
	ctx["team"] = gs(team, "name")
	ws, _ := pr["workspace"].(map[string]any)
	ctx["workspace"] = gs(ws, "name")
}

func main() {
	if err := plugin.Serve(newAsanaPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-asana:", err)
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

func intv(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
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

// stringify renders an option value as one query-string value: lists are
// comma-joined (Asana's convention for multi-value params), scalars printed.
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []string, []any:
		return strings.Join(strList(x), ",")
	default:
		return fmt.Sprintf("%v", x)
	}
}

// setQ sets q[key] when v renders to a non-empty string.
func setQ(q url.Values, key string, v any) {
	if s := stringify(v); s != "" {
		q.Set(key, s)
	}
}

// addOptFields sets Asana's opt_fields projection from a list or comma string.
func addOptFields(q url.Values, v any) {
	if s := strings.Join(strList(v), ","); s != "" {
		q.Set("opt_fields", s)
	}
}

// setIfPresent copies v into m under key, skipping a nil or empty-string v so
// optional fields are omitted rather than sent as zero values.
func setIfPresent(m map[string]any, key string, v any) {
	if v == nil {
		return
	}
	if s, ok := v.(string); ok && s == "" {
		return
	}
	m[key] = v
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
