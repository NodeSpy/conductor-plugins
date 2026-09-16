// Command conductor-matrix is the Matrix connector as a standalone external
// conductor plugin. It talks to a homeserver's Matrix Client-Server API over
// plain net/http (no third-party Matrix SDK), sending messages/events as
// verbs and streaming new room messages (and invites) as a source.
//
// Unlike the webhook-based connectors in this repo (github, sentry), Matrix
// has no webhook push model for a bot's own account: the source here runs a
// /sync long-poll loop against the homeserver instead of listening for
// inbound HTTP.
//
// Connection (used for both Invoke and StartSource):
//
//	homeserver:   "https://matrix.example.org"  # required
//	access_token: "<access token>"              # required
//	user_id:      "@bot:example.org"             # informational; not required by the API itself
//	api_base:     "http://127.0.0.1:NNNN/_matrix/client/v3" # override the client-server base (tests, non-standard deployments)
//
// Built ONLY against the public SDK (pkg/plugin) and the standard library —
// no sourcekit (the source here is a poll loop, not a webhook listener) and
// no conductor internals.
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
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type matrixPlugin struct{}

func (matrixPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "matrix",
		Desc: "Matrix: send messages/events and manage rooms via the Client-Server API; new room messages (and invites) in via a /sync long-poll source.",
		Connection: plugin.Schema{
			"homeserver":   {Type: "string", Required: true, Desc: "homeserver base URL, e.g. https://matrix.example.org"},
			"access_token": {Type: "string", Required: true, Desc: "the bot/account access token"},
			"user_id":      {Type: "string", Desc: "the bot's own Matrix user id, e.g. @bot:example.org (informational)"},
			"api_base":     {Type: "string", Desc: "override the Client-Server API base URL (default: homeserver + /_matrix/client/v3); for tests or non-standard deployments"},
		},
		Events: []plugin.Event{
			{
				Name: "message", Desc: "a new m.room.message event arrived in a joined room",
				Filters: plugin.Schema{
					"room_ids": {Type: "list"}, "senders": {Type: "list"}, "msgtypes": {Type: "list"},
					"room_id": {Type: "string"}, "sender": {Type: "string"}, "msgtype": {Type: "string"},
				},
				Context: plugin.Schema{
					"room_id": {Type: "string"}, "sender": {Type: "string"}, "body": {Type: "string"},
					"msgtype": {Type: "string"}, "event_id": {Type: "string"}, "formatted_body": {Type: "string"},
					// Plural aliases (same value as the scalar field) so the
					// documented filter vocabulary (filters: {room_ids/senders/
					// msgtypes: [...]}) matches the daemon's generic
					// list-contains filter evaluator, mirroring the sentry
					// connector's levels/projects/environments convention.
					"room_ids": {Type: "string"}, "senders": {Type: "string"}, "msgtypes": {Type: "string"},
				},
			},
			{
				Name: "invite", Desc: "the connected account was invited to a room (m.room.member, membership=invite)",
				Filters: plugin.Schema{
					"room_ids": {Type: "list"}, "senders": {Type: "list"},
					"room_id": {Type: "string"}, "sender": {Type: "string"},
				},
				Context: plugin.Schema{
					"room_id": {Type: "string"}, "sender": {Type: "string"}, "user_id": {Type: "string"}, "event_id": {Type: "string"},
					"room_ids": {Type: "string"}, "senders": {Type: "string"},
				},
			},
		},
		Verbs: matrixVerbs(),
		// Egress is intentionally empty: the homeserver host is operator-
		// supplied per connection (there is no fixed Matrix API host to
		// declare statically, unlike github's api.github.com). It is
		// documented in Connection.homeserver instead, and narrowed per
		// instance with `network:` the same as any other connector.
		Capabilities: plugin.Capabilities{},
	}
}

func matrixVerbs() []plugin.Verb {
	withEventID := plugin.Schema{"result": {Type: "any", Desc: "the raw API response"}, "event_id": {Type: "string"}}
	resultOnly := plugin.Schema{"result": {Type: "any", Desc: "the raw API response"}}

	return []plugin.Verb{
		{
			Name: "send_message", Desc: "send an m.room.message to a room",
			Options: plugin.Schema{
				"room_id":        {Type: "string", Required: true, Scope: "room"},
				"body":           {Type: "string", Required: true},
				"msgtype":        {Type: "string", Desc: "default m.text"},
				"formatted_body": {Type: "string", Desc: "HTML body; sets format org.matrix.custom.html unless format is given"},
				"format":         {Type: "string", Desc: "override the format when formatted_body is set"},
			},
			Outputs: withEventID,
		},
		{
			Name: "send_notice", Desc: "send an m.notice message to a room (bot-style, muted notifications for most clients)",
			Options: plugin.Schema{
				"room_id": {Type: "string", Required: true, Scope: "room"},
				"body":    {Type: "string", Required: true},
			},
			Outputs: withEventID,
		},
		{
			Name: "send_event", Desc: "send an arbitrary room event",
			Options: plugin.Schema{
				"room_id":    {Type: "string", Required: true, Scope: "room"},
				"event_type": {Type: "string", Required: true, Desc: "e.g. m.room.message, m.reaction"},
				"content":    {Type: "map", Required: true},
			},
			Outputs: withEventID,
		},
		{
			Name: "join_room", Desc: "join a room by id or alias",
			Options: plugin.Schema{
				"room_id_or_alias": {Type: "string", Required: true, Scope: "room"},
			},
			Outputs: resultOnly,
		},
		{
			Name: "leave_room", Desc: "leave a room",
			Options: plugin.Schema{
				"room_id": {Type: "string", Required: true, Scope: "room"},
			},
			Outputs: resultOnly,
		},
		{
			Name: "invite", Desc: "invite a user to a room",
			Options: plugin.Schema{
				"room_id": {Type: "string", Required: true, Scope: "room"},
				"user_id": {Type: "string", Required: true},
			},
			Outputs: resultOnly,
		},
		{
			Name: "redact", Desc: "redact (delete the content of) an event",
			Options: plugin.Schema{
				"room_id":  {Type: "string", Required: true, Scope: "room"},
				"event_id": {Type: "string", Required: true},
				"reason":   {Type: "string"},
			},
			Outputs: withEventID,
		},
		{
			Name: "set_name", Desc: "set a room's name",
			Options: plugin.Schema{
				"room_id": {Type: "string", Required: true, Scope: "room"},
				"value":   {Type: "string", Required: true},
			},
			Outputs: resultOnly,
		},
		{
			Name: "set_topic", Desc: "set a room's topic",
			Options: plugin.Schema{
				"room_id": {Type: "string", Required: true, Scope: "room"},
				"value":   {Type: "string", Required: true},
			},
			Outputs: resultOnly,
		},
		{
			Name: "get_messages", Desc: "paginate a room's message history",
			Options: plugin.Schema{
				"room_id": {Type: "string", Required: true, Scope: "room"},
				"limit":   {Type: "integer", Desc: "default 10"},
				"from":    {Type: "string", Desc: "pagination token (e.g. a prior sync's next_batch, or a prior call's end)"},
			},
			Outputs: resultOnly,
		},
		{
			Name:    "whoami",
			Desc:    "identify the account behind the connection's access token",
			Options: plugin.Schema{},
			Outputs: resultOnly,
		},
		{
			Name: "api", Desc: "escape hatch: call any Client-Server API endpoint under the client/v3 base",
			Usage: "for an endpoint without a first-class verb",
			Options: plugin.Schema{
				"method": {Type: "string", Required: true, Enum: []string{"GET", "POST", "PUT", "DELETE"}},
				"path":   {Type: "string", Required: true, Desc: "path under the client/v3 base, e.g. /rooms/!abc:example.org/state"},
				"query":  {Type: "map", Desc: "query string parameters"},
				"body":   {Type: "any", Desc: "JSON request body"},
			},
			Outputs: resultOnly,
		},
	}
}

// --- connection / HTTP client -----------------------------------------

type matrixClient struct {
	base  string
	token string
	http  *http.Client
}

func newClient(conn map[string]any) (*matrixClient, error) {
	homeserver := str(conn["homeserver"])
	token := str(conn["access_token"])
	if homeserver == "" {
		return nil, fmt.Errorf("connection.homeserver is required")
	}
	if token == "" {
		return nil, fmt.Errorf("connection.access_token is required")
	}
	base := strings.TrimRight(homeserver, "/") + "/_matrix/client/v3"
	if ab := str(conn["api_base"]); ab != "" {
		base = strings.TrimRight(ab, "/")
	}
	return &matrixClient{base: base, token: token, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// do performs one Client-Server API request and decodes a JSON object
// response. A non-2xx status is returned as a plugin.Errorf(CodeInternalError,
// ...) carrying the status and response body, per the connector's contract.
func (c *matrixClient) do(ctx context.Context, method, path string, query url.Values, body any) (map[string]any, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, plugin.Errorf(plugin.CodeInternalError, fmt.Sprintf("matrix: %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))))
	}
	if len(bytes.TrimSpace(respBody)) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, plugin.Errorf(plugin.CodeInternalError, "matrix: decoding response: "+err.Error())
	}
	return out, nil
}

// --- Invoke -------------------------------------------------------------

func (matrixPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	client, err := newClient(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	ctx := context.Background()

	switch req.Verb {
	case "send_message":
		roomID := str(o["room_id"])
		if roomID == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "room_id is required")
		}
		body := str(o["body"])
		if body == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "body is required")
		}
		content := map[string]any{"msgtype": strOr(o["msgtype"], "m.text"), "body": body}
		if fb := str(o["formatted_body"]); fb != "" {
			content["formatted_body"] = fb
			content["format"] = strOr(o["format"], "org.matrix.custom.html")
		}
		return sendEvent(ctx, client, roomID, "m.room.message", content)

	case "send_notice":
		roomID := str(o["room_id"])
		if roomID == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "room_id is required")
		}
		body := str(o["body"])
		if body == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "body is required")
		}
		content := map[string]any{"msgtype": "m.notice", "body": body}
		return sendEvent(ctx, client, roomID, "m.room.message", content)

	case "send_event":
		roomID := str(o["room_id"])
		if roomID == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "room_id is required")
		}
		eventType := str(o["event_type"])
		if eventType == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "event_type is required")
		}
		content, _ := o["content"].(map[string]any)
		if content == nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "content is required")
		}
		return sendEvent(ctx, client, roomID, eventType, content)

	case "join_room":
		id := str(o["room_id_or_alias"])
		if id == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "room_id_or_alias is required")
		}
		out, err := client.do(ctx, http.MethodPost, "/join/"+url.PathEscape(id), nil, map[string]any{})
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return plugin.InvokeResult{Outputs: map[string]any{"result": out}}, nil

	case "leave_room":
		roomID := str(o["room_id"])
		if roomID == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "room_id is required")
		}
		out, err := client.do(ctx, http.MethodPost, "/rooms/"+url.PathEscape(roomID)+"/leave", nil, map[string]any{})
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return plugin.InvokeResult{Outputs: map[string]any{"result": out}}, nil

	case "invite":
		roomID := str(o["room_id"])
		userID := str(o["user_id"])
		if roomID == "" || userID == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "room_id and user_id are required")
		}
		out, err := client.do(ctx, http.MethodPost, "/rooms/"+url.PathEscape(roomID)+"/invite", nil, map[string]any{"user_id": userID})
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return plugin.InvokeResult{Outputs: map[string]any{"result": out}}, nil

	case "redact":
		roomID := str(o["room_id"])
		eventID := str(o["event_id"])
		if roomID == "" || eventID == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "room_id and event_id are required")
		}
		body := map[string]any{}
		if r := str(o["reason"]); r != "" {
			body["reason"] = r
		}
		txn := txnID()
		path := fmt.Sprintf("/rooms/%s/redact/%s/%s", url.PathEscape(roomID), url.PathEscape(eventID), url.PathEscape(txn))
		out, err := client.do(ctx, http.MethodPut, path, nil, body)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		outputs := map[string]any{"result": out}
		if eid, ok := out["event_id"].(string); ok {
			outputs["event_id"] = eid
		}
		return plugin.InvokeResult{Outputs: outputs}, nil

	case "set_name":
		roomID := str(o["room_id"])
		value := str(o["value"])
		if roomID == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "room_id is required")
		}
		path := "/rooms/" + url.PathEscape(roomID) + "/state/m.room.name"
		out, err := client.do(ctx, http.MethodPut, path, nil, map[string]any{"name": value})
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return plugin.InvokeResult{Outputs: map[string]any{"result": out}}, nil

	case "set_topic":
		roomID := str(o["room_id"])
		value := str(o["value"])
		if roomID == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "room_id is required")
		}
		path := "/rooms/" + url.PathEscape(roomID) + "/state/m.room.topic"
		out, err := client.do(ctx, http.MethodPut, path, nil, map[string]any{"topic": value})
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return plugin.InvokeResult{Outputs: map[string]any{"result": out}}, nil

	case "get_messages":
		roomID := str(o["room_id"])
		if roomID == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "room_id is required")
		}
		q := url.Values{}
		q.Set("dir", "b")
		if limit := intStr(o["limit"]); limit != "" {
			q.Set("limit", limit)
		} else {
			q.Set("limit", "10")
		}
		if from := str(o["from"]); from != "" {
			q.Set("from", from)
		}
		out, err := client.do(ctx, http.MethodGet, "/rooms/"+url.PathEscape(roomID)+"/messages", q, nil)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return plugin.InvokeResult{Outputs: map[string]any{"result": out}}, nil

	case "whoami":
		out, err := client.do(ctx, http.MethodGet, "/account/whoami", nil, nil)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return plugin.InvokeResult{Outputs: map[string]any{"result": out}}, nil

	case "api":
		method := strings.ToUpper(str(o["method"]))
		if method == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "method is required")
		}
		path := str(o["path"])
		if path == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "path is required")
		}
		q := url.Values{}
		if m, ok := o["query"].(map[string]any); ok {
			for k, v := range m {
				q.Set(k, fmt.Sprintf("%v", v))
			}
		}
		var body any
		if b, ok := o["body"]; ok {
			body = b
		}
		out, err := client.do(ctx, method, path, q, body)
		if err != nil {
			return plugin.InvokeResult{}, err
		}
		return plugin.InvokeResult{Outputs: map[string]any{"result": out}}, nil

	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// sendEvent PUTs one room event under a fresh transaction id and returns its
// event_id alongside the raw response.
func sendEvent(ctx context.Context, client *matrixClient, roomID, eventType string, content map[string]any) (plugin.InvokeResult, error) {
	txn := txnID()
	path := fmt.Sprintf("/rooms/%s/send/%s/%s", url.PathEscape(roomID), url.PathEscape(eventType), url.PathEscape(txn))
	out, err := client.do(ctx, http.MethodPut, path, nil, content)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	outputs := map[string]any{"result": out}
	if eid, ok := out["event_id"].(string); ok {
		outputs["event_id"] = eid
	}
	return plugin.InvokeResult{Outputs: outputs}, nil
}

// txnID mints a transaction id for a PUT .../send|redact/{txnId} call. Time in
// nanoseconds is unique enough within one plugin process, which is the whole
// requirement (Matrix scopes txn ids to the access token).
func txnID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}

// --- StartSource: /sync long-poll ---------------------------------------

func (matrixPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	homeserver := str(cfg["homeserver"])
	token := str(cfg["access_token"])
	if homeserver == "" {
		return fmt.Errorf("matrix: no connection.homeserver configured")
	}
	if token == "" {
		return fmt.Errorf("matrix: no connection.access_token configured")
	}
	base := strings.TrimRight(homeserver, "/") + "/_matrix/client/v3"
	if ab := str(cfg["api_base"]); ab != "" {
		base = strings.TrimRight(ab, "/")
	}
	httpClient := &http.Client{Timeout: 35 * time.Second} // longer than the 30s sync timeout

	fmt.Fprintf(os.Stderr, "matrix[%s]: starting /sync loop against %s\n", req.Instance, base)

	since := ""
	firstSync := true
	seen := map[string]bool{}
	for {
		if ctx.Err() != nil {
			return nil
		}
		body, err := doSync(ctx, httpClient, base, token, since)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintf(os.Stderr, "matrix[%s]: sync error: %v\n", req.Instance, err)
			if !sleepOrDone(ctx, 5*time.Second) {
				return nil
			}
			continue
		}
		events, next, err := processSync(body, firstSync, seen)
		if err != nil {
			fmt.Fprintf(os.Stderr, "matrix[%s]: sync decode error: %v\n", req.Instance, err)
			if !sleepOrDone(ctx, 5*time.Second) {
				return nil
			}
			continue
		}
		if next != "" {
			since = next
		}
		firstSync = false
		for _, ev := range events {
			_ = emit(ev)
		}
	}
}

// sleepOrDone waits d, returning false if ctx is cancelled first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func doSync(ctx context.Context, client *http.Client, base, token, since string) ([]byte, error) {
	u := base + "/sync?timeout=30000"
	if since != "" {
		u += "&since=" + url.QueryEscape(since)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("sync: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// --- /sync response parsing (pure, testable) -----------------------------

// wireEvent is the normalized event streamed to the daemon's plugin source
// adapter. Target's keys must match core.Target's exact (untagged,
// capitalized) Go field names, since the daemon decodes this JSON straight
// into that struct — mirrored from the github/sentry connectors in this repo.
type wireEvent struct {
	Event   string         `json:"event"`
	Kind    string         `json:"kind,omitempty"`
	Title   string         `json:"title,omitempty"`
	Target  map[string]any `json:"target,omitempty"`
	Context map[string]any `json:"context,omitempty"`
	Dedup   string         `json:"dedup,omitempty"`
}

type syncPayload struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events []syncEvent `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
	} `json:"rooms"`
}

type syncEvent struct {
	Type     string          `json:"type"`
	Sender   string          `json:"sender"`
	EventID  string          `json:"event_id"`
	StateKey *string         `json:"state_key,omitempty"`
	Content  json.RawMessage `json:"content"`
}

type messageContent struct {
	MsgType       string `json:"msgtype"`
	Body          string `json:"body"`
	FormattedBody string `json:"formatted_body"`
}

type memberContent struct {
	Membership string `json:"membership"`
}

// processSync decodes one /sync response into normalized events, advancing
// since (returned as next) and deduping by event_id via seen (mutated across
// calls by the caller — StartSource passes the same map every iteration).
//
// On the FIRST sync (firstSync true, i.e. no prior since token), next_batch is
// still returned so the caller can start following the room from now on, but
// NO events are emitted — otherwise every room's entire timeline backlog
// would fire as new events the moment the source starts.
func processSync(body []byte, firstSync bool, seen map[string]bool) (events []wireEvent, next string, err error) {
	var p syncPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, "", err
	}
	next = p.NextBatch
	if firstSync {
		return nil, next, nil
	}
	for roomID, room := range p.Rooms.Join {
		for _, ev := range room.Timeline.Events {
			if ev.EventID != "" {
				if seen[ev.EventID] {
					continue
				}
			}
			switch ev.Type {
			case "m.room.message":
				var c messageContent
				_ = json.Unmarshal(ev.Content, &c)
				if ev.EventID != "" {
					seen[ev.EventID] = true
				}
				events = append(events, wireEvent{
					Event: "message", Kind: "message",
					Title: fmt.Sprintf("message from %s in %s", ev.Sender, roomID),
					Dedup: ev.EventID,
					Target: map[string]any{
						"RoomID": roomID, "Sender": ev.Sender, "EventID": ev.EventID,
					},
					Context: map[string]any{
						"room_id": roomID, "sender": ev.Sender, "body": c.Body,
						"msgtype": c.MsgType, "event_id": ev.EventID, "formatted_body": c.FormattedBody,
						"room_ids": roomID, "senders": ev.Sender, "msgtypes": c.MsgType,
					},
				})
			case "m.room.member":
				if ev.StateKey == nil {
					continue
				}
				var c memberContent
				_ = json.Unmarshal(ev.Content, &c)
				if c.Membership != "invite" {
					continue
				}
				if ev.EventID != "" {
					seen[ev.EventID] = true
				}
				events = append(events, wireEvent{
					Event: "invite", Kind: "invite",
					Title: fmt.Sprintf("%s invited %s to %s", ev.Sender, *ev.StateKey, roomID),
					Dedup: ev.EventID,
					Target: map[string]any{
						"RoomID": roomID, "Sender": ev.Sender, "EventID": ev.EventID,
					},
					Context: map[string]any{
						"room_id": roomID, "sender": ev.Sender, "user_id": *ev.StateKey, "event_id": ev.EventID,
						"room_ids": roomID, "senders": ev.Sender,
					},
				})
			}
		}
	}
	return events, next, nil
}

// --- small option helpers (stdlib only) ----------------------------------

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, def string) string {
	if s := str(v); s != "" {
		return s
	}
	return def
}

func intStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case string:
		return x
	}
	return ""
}

func main() {
	if err := plugin.Serve(matrixPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-matrix:", err)
		os.Exit(1)
	}
}
