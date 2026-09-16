// Command conductor-ntfy is the ntfy.sh connector as an external conductor
// plugin (#59). It exposes one verb — publish — and one source: a live
// subscription to one or more topics via ntfy's JSON-stream endpoint
// (GET {server}/{topics}/json), which is a long-lived newline-delimited JSON
// response, not a webhook. Built ONLY against the public SDK (pkg/plugin) —
// no sourcekit, no conductor internals, no third-party deps: the source here
// is an outbound GET this plugin holds open and reconnects, not an inbound
// listener, so sourcekit's webhook Listener has nothing to offer it, and its
// own dedup is a few lines it isn't worth a dependency for.
//
// Connection (used for both Invoke and StartSource):
//
//	server: "https://ntfy.sh"  # base URL (default); point at a self-hosted
//	                            # server to change/widen the egress target
//	token: "<access token>"    # Bearer auth
//	username: "<user>"         # Basic auth (paired with password)
//	password: "<pass>"
//	subscribe: [alerts, ci]    # topics to subscribe to (StartSource only)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type ntfyPlugin struct{}

func (ntfyPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "ntfy",
		Desc: "ntfy.sh push notifications: publish a message (verb), and subscribe to topics as a live source over ntfy's JSON-stream endpoint.",
		Connection: plugin.Schema{
			"server":    {Type: "string", Desc: "ntfy server base URL (default https://ntfy.sh); a self-hosted server changes/widens the egress target beyond the declared capability"},
			"token":     {Type: "string", Desc: "Bearer access token"},
			"username":  {Type: "string", Desc: "Basic auth username (paired with password)"},
			"password":  {Type: "string", Desc: "Basic auth password (paired with username)"},
			"subscribe": {Type: "list", Desc: "topics to subscribe to (StartSource only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name:  "publish",
				Desc:  "publish a message to a topic",
				Usage: "send a notification: topic is required, everything else shapes how it renders/behaves",
				Options: plugin.Schema{
					"topic":    {Type: "string", Required: true, Scope: "topic", Desc: "the ntfy topic to publish to"},
					"message":  {Type: "string", Desc: "message body"},
					"title":    {Type: "string", Desc: "notification title"},
					"priority": {Type: "any", Desc: "1 (min) .. 5 (max), or the name: min/low/default/high/max"},
					"tags":     {Type: "list", Desc: "tags / emoji shortcodes"},
					"click":    {Type: "string", Desc: "URL opened when the notification is tapped"},
					"attach":   {Type: "string", Desc: "URL of a file to attach"},
					"actions":  {Type: "any", Desc: "action buttons — passed through verbatim (list or map, per ntfy's action spec)"},
					"email":    {Type: "string", Desc: "also forward the message to this e-mail address"},
					"delay":    {Type: "string", Desc: "schedule delivery, e.g. \"30min\", \"tomorrow, 9am\""},
					"markdown": {Type: "boolean", Desc: "render the message as Markdown"},
				},
				Outputs: plugin.Schema{
					"status_code": {Type: "integer"},
					"result":      {Type: "any", Desc: "the parsed JSON response body"},
				},
			},
		},
		Events: []plugin.Event{
			{
				Name: "message",
				Desc: "a message was published to a subscribed topic",
				Context: plugin.Schema{
					"topic":    {Type: "string"},
					"message":  {Type: "string"},
					"title":    {Type: "string"},
					"priority": {Type: "integer"},
					"tags":     {Type: "list"},
					"click":    {Type: "string"},
					"id":       {Type: "string"},
					"time":     {Type: "integer"},
				},
				Filters: plugin.Schema{
					"topics":     {Type: "list"},
					"priorities": {Type: "list"},
					"tags":       {Type: "list"},
					"topic":      {Type: "string"},
					"priority":   {Type: "string"},
				},
			},
		},
		// It only ever calls out to the configured ntfy server (publish POST,
		// subscribe GET) and spawns nothing.
		Capabilities: plugin.Capabilities{
			Egress: []string{"ntfy.sh:443"},
			Spawns: false,
		},
	}
}

func (ntfyPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	if req.Verb != "publish" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "ntfy: unknown verb "+req.Verb)
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	topic := str(o["topic"])
	if topic == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "publish: topic is required")
	}

	body := map[string]any{"topic": topic}
	if m := str(o["message"]); m != "" {
		body["message"] = m
	}
	if t := str(o["title"]); t != "" {
		body["title"] = t
	}
	if p, ok := o["priority"]; ok && p != nil {
		n, err := priorityInt(p)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "publish: priority: "+err.Error())
		}
		body["priority"] = n
	}
	if tags := strList(o["tags"]); len(tags) > 0 {
		body["tags"] = tags
	}
	if c := str(o["click"]); c != "" {
		body["click"] = c
	}
	if a := str(o["attach"]); a != "" {
		body["attach"] = a
	}
	if actions, ok := o["actions"]; ok && actions != nil {
		body["actions"] = actions
	}
	if e := str(o["email"]); e != "" {
		body["email"] = e
	}
	if d := str(o["delay"]); d != "" {
		body["delay"] = d
	}
	if md, ok := o["markdown"]; ok {
		body["markdown"] = boolv(md)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "publish: "+err.Error())
	}

	server := strOr(req.Connection["server"], "https://ntfy.sh")
	httpReq, err := http.NewRequest(http.MethodPost, strings.TrimRight(server, "/")+"/", bytes.NewReader(raw))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "publish: "+err.Error())
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyAuth(httpReq, req.Connection)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "publish: "+err.Error())
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "publish: reading response: "+err.Error())
	}
	var result any
	_ = json.Unmarshal(respBody, &result)

	return plugin.InvokeResult{Outputs: map[string]any{
		"status_code": resp.StatusCode,
		"result":      result,
	}}, nil
}

func (ntfyPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	server := strOr(cfg["server"], "https://ntfy.sh")
	topics := strList(cfg["subscribe"])
	if len(topics) == 0 {
		return fmt.Errorf("ntfy: subscribe must list at least one topic")
	}
	streamURL := strings.TrimRight(server, "/") + "/" + strings.Join(topics, ",") + "/json"

	dedup := newDedup(2048)
	fmt.Fprintf(os.Stderr, "ntfy[%s]: subscribing to %s at %s\n", req.Instance, strings.Join(topics, ","), server)

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		err := streamOnce(ctx, streamURL, cfg, dedup, emit)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ntfy[%s]: stream error: %v\n", req.Instance, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
	return nil
}

func main() {
	if err := plugin.Serve(ntfyPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-ntfy: %v\n", err)
		os.Exit(1)
	}
}

// --- source stream: connect once, read newline-delimited JSON until it ends ---

// streamOnce holds one connection to the ntfy JSON-stream endpoint open,
// feeding each line to handleLine until the response ends or ctx is
// cancelled. Reconnection/backoff is the caller's (StartSource's) job.
func streamOnce(ctx context.Context, streamURL string, cfg map[string]any, dedup *dedupSet, emit func(any) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return err
	}
	applyAuth(req, cfg)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ntfy: subscribe GET returned %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		_ = handleLine(line, dedup, emit)
	}
	return scanner.Err()
}

// ntfyMessage is one line of ntfy's JSON-stream response. event is one of
// "open" (stream established), "keepalive" (periodic ping), or "message" (an
// actual published notification) — only "message" is ever emitted.
type ntfyMessage struct {
	ID       string   `json:"id"`
	Time     int64    `json:"time"`
	Event    string   `json:"event"`
	Topic    string   `json:"topic"`
	Message  string   `json:"message"`
	Title    string   `json:"title"`
	Tags     []string `json:"tags"`
	Priority int      `json:"priority"`
	Click    string   `json:"click"`
}

// handleLine parses one newline-delimited JSON line from the stream and, for
// a message event not already seen, emits it. It is the pure, testable core
// of the source: no network, no goroutine, just line in / emit out.
func handleLine(line []byte, dedup *dedupSet, emit func(any) error) error {
	var m ntfyMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return nil // malformed line: skip rather than fail the whole stream
	}
	if m.Event != "message" {
		return nil // "open" / "keepalive" carry no notification to emit
	}
	if m.ID == "" || !dedup.Add(m.ID) {
		return nil
	}
	title := nonEmpty(m.Title, "ntfy: "+m.Topic)
	context := map[string]any{
		"topic":    m.Topic,
		"message":  m.Message,
		"title":    m.Title,
		"priority": m.Priority,
		"tags":     m.Tags,
		"click":    m.Click,
		"id":       m.ID,
		"time":     m.Time,
		// Plural aliases so the documented filter vocabulary (filters:
		// {topics/priorities: [...]}) matches the daemon's generic
		// list-contains filter evaluator; tags is already a list, so it
		// needs no separate alias.
		"topics":     m.Topic,
		"priorities": m.Priority,
	}
	return emit(map[string]any{
		"event":   "message",
		"kind":    "message",
		"title":   title,
		"dedup":   m.ID,
		"context": context,
	})
}

// --- dedup: a bounded set of recently-seen message ids ---

// dedupSet is a bounded set of recently-seen ids, so a re-delivered message
// (ntfy retries GETs that drop mid-stream) doesn't emit twice. It is used
// from the single StartSource goroutine only, so it needs no lock.
type dedupSet struct {
	cap   int
	seen  map[string]struct{}
	order []string
}

func newDedup(capacity int) *dedupSet {
	if capacity <= 0 {
		capacity = 1024
	}
	return &dedupSet{cap: capacity, seen: make(map[string]struct{}, capacity)}
}

// Add records key and reports whether it was new (true) or a duplicate (false).
func (d *dedupSet) Add(key string) bool {
	if _, ok := d.seen[key]; ok {
		return false
	}
	d.seen[key] = struct{}{}
	d.order = append(d.order, key)
	if len(d.order) > d.cap {
		old := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, old)
	}
	return true
}

// --- auth ---

// applyAuth sets Bearer or Basic auth from the connection/config map onto
// req, per the connection schema: token (Bearer) OR username+password
// (Basic). token wins when both are set.
func applyAuth(req *http.Request, conn map[string]any) {
	if token := str(conn["token"]); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		return
	}
	if user := str(conn["username"]); user != "" {
		req.SetBasicAuth(user, str(conn["password"]))
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

// priorityInt normalizes ntfy's priority option: an int/float 1..5, a numeric
// string, or one of the named levels (min/low/default/high/max).
func priorityInt(v any) (int, error) {
	switch x := v.(type) {
	case float64:
		return int(x), nil
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "min":
			return 1, nil
		case "low":
			return 2, nil
		case "default":
			return 3, nil
		case "high":
			return 4, nil
		case "max":
			return 5, nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, fmt.Errorf("invalid priority %q", x)
		}
		return n, nil
	}
	return 0, fmt.Errorf("invalid priority %v", v)
}

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
