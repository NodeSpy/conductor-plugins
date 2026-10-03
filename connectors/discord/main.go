// Command conductor-discord is the Discord connector as a standalone external
// conductor plugin, at parity with conductor's former bundled `discord`
// connector (deleted from conductor core now that vendor code lives only in
// plugins). It posts messages and hand-off "ask" questions over a bot (or a
// post-only incoming webhook), and runs a persistent Discord gateway
// connection to capture replies.
//
// A reply is emitted as a `reply` source event carrying the plugin
// contract's `conversation_reply` semantic (docs/design/plugin-contract.md
// §2.2): the ENGINE — not this plugin — routes it to the pending ask it
// answers, by matching the event's `channel` fact against the `ref` output
// the `ask` verb returned when it opened that conversation. This plugin's
// `ask` verb therefore does not block waiting for a reply the way the old
// in-process implementation did: it posts the question and returns
// immediately with `ref`; the eventual decision is assembled by the host
// from the matching `reply` event.
//
// Connection:
//
//	bot_token:   "<bot token>"              # from the Discord developer portal
//	webhook_url: "<incoming webhook URL>"   # post-only alternative to bot_token
//	api_base:    "https://discord.com/api/v10"  # override the REST API base (tests)
//	gateway_url: "wss://example.test/gw"    # override the gateway dial URL (tests only);
//	                                         # when set, the normal GET /gateway/bot
//	                                         # bootstrap call is skipped entirely
//
// A plugin's own process environment is the scrubbed minimal one every
// plugin gets (docs/design/plugin-contract.md §1.6), so — unlike the old
// bundled connector's PC_DISCORD_API_URL env var — a test double overrides
// api_base/gateway_url through the connection config instead.
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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// defaultAPIBase is Discord's REST API origin. Overridable per-connection via
// api_base.
const defaultAPIBase = "https://discord.com/api/v10"

// discordGatewayPath is the bootstrap REST endpoint returning the wss:// URL
// to dial, resolved against api_base. gateway_url, when set, is dialed
// directly and this bootstrap call is skipped — the override a fake-gateway
// test double needs, since it can't serve a believable /gateway/bot response
// pointing back at a URL it is also reachable at.
const discordGatewayPath = "/gateway/bot"

// discordIntents is the numeric sum of gateway intents this client needs:
// GUILDS (1<<0) so guild/channel state resolves, GUILD_MESSAGES (1<<9) so a
// channel/thread reply's MESSAGE_CREATE fires, DIRECT_MESSAGES (1<<12) so a
// DM reply's does too, and MESSAGE_CONTENT (1<<15) — a privileged intent
// that must ALSO be turned on for the bot in the developer portal — so
// `content` is actually populated on those events.
const discordIntents = 1<<0 | 1<<9 | 1<<12 | 1<<15

type discordPlugin struct{}

func (discordPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "discord",
		Desc: "Discord: post messages and hand-off questions over a bot (or a post-only incoming webhook); captures gateway replies as conversation_reply events.",
		Connection: plugin.Schema{
			"bot_token":   {Type: "string", Secret: true, Desc: "Discord bot token (from the developer portal)"},
			"webhook_url": {Type: "string", Secret: true, Desc: "an incoming-webhook URL — post-only alternative to a bot token"},
			"api_base":    {Type: "string", Desc: "override the Discord REST API base URL (tests, or a private gateway); default " + defaultAPIBase},
			"gateway_url": {Type: "string", Desc: "override the gateway URL this plugin dials to capture replies (tests only); when set, the GET /gateway/bot bootstrap call is skipped"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "post", Desc: "post a message to a channel or DM",
				Options: plugin.Schema{
					"channel": {Type: "string", Scope: "channel", Desc: "channel id (or set user: for a DM)"},
					"user":    {Type: "string", Scope: "user", Desc: "user id to DM"},
					"text":    {Type: "string", Required: true},
				},
				Outputs:   plugin.Schema{"id": {Type: "string"}, "channel": {Type: "string"}},
				Semantics: &plugin.VerbSemantics{ConversationPost: true},
			},
			{
				Name: "ask", Desc: "present a question/draft; the reply arrives later as a conversation_reply event",
				Usage: "posts the question and returns `ref` immediately — the eventual reply is matched and assembled by the host, not awaited inside this verb",
				Ask:   true,
				Options: plugin.Schema{
					"to":        {Type: "string", Enum: []string{"dm", "thread"}, Required: true},
					"user":      {Type: "string", Scope: "user", Desc: "user id (to: dm)"},
					"channel":   {Type: "string", Scope: "channel", Desc: "channel id (to: thread)"},
					"approvers": {Type: "list", Desc: "to: thread — only these user ids may resolve the ask (default: anyone in the channel)"},
					"prompt":    {Type: "string", Required: true, Desc: "the question to present"},
					"draft":     {Type: "string", Desc: "editable draft text presented with the question"},
					"title":     {Type: "string", Desc: "presentation title (default: the prompt's first line)"},
					"timeout":   {Type: "duration", Desc: "how long the host waits for an answer (default 1h)"},
				},
				Outputs: plugin.Schema{
					"ref":    {Type: "string", Desc: "channel:message_id of the question — equal to what the conversation_reply event's `channel`+`replied_to` facts render to when the human replies to THIS message (Discord's own reply-to feature). A plain message with no reply-to resolves no ask, so two pending asks in the same channel never collide."},
					"action": {Type: "string", Enum: []string{"approve", "revise", "discard"}, Desc: "the human's decision (filled in once a reply resolves this ask)"},
					"text":   {Type: "string", Desc: "their reply text (a revision), or the draft on approve (filled in once a reply resolves this ask)"},
				},
				Semantics: &plugin.VerbSemantics{
					OpensConversation: &plugin.OpensConversation{ID: "ref", Approvers: "approvers"},
				},
			},
		},
		Events: []plugin.Event{
			{
				Name: "reply",
				Desc: "a message was posted in a channel or DM the gateway is watching. The host consumes it to resolve a pending ask when it matches one; otherwise it is an ordinary event.",
				Context: plugin.Schema{
					"channel":    {Type: "string", Desc: "the channel (or DM channel) id the message landed in"},
					"author":     {Type: "string", Desc: "the Discord user id of the message's author"},
					"author_bot": {Type: "boolean", Desc: "true if the author is a bot account"},
					"text":       {Type: "string", Desc: "the message content"},
					"message_id": {Type: "string"},
					"replied_to": {Type: "string", Desc: "the message id this one replies to (Discord's native reply-to feature), empty for a plain message"},
				},
				Filters: plugin.Schema{
					"channel": {Type: "string"},
					"author":  {Type: "string"},
				},
				Semantics: &plugin.EventSemantics{
					// A bare "channel" (no template braces) rendered as the
					// literal string "channel" for every reply, colliding
					// every channel/ask into one slot. The id now also
					// requires Discord's own reply-to (replied_to) to match
					// the ask's own message id, so two pending asks in the
					// same channel resolve independently — a plain message
					// with no reply-to (replied_to empty) matches no ask's
					// ref (which always has a real message id suffix) and
					// is left as an ordinary, unconsumed reply event.
					ConversationReply: &plugin.ConversationReply{ID: "{{.channel}}:{{.replied_to}}", Author: "author", Text: "text"},
					Author:            &plugin.AuthorSemantics{Login: "author", Automated: "author_bot"},
				},
			},
		},
		// It only ever calls the Discord REST API and gateway, and spawns
		// nothing.
		Capabilities: plugin.Capabilities{
			Egress: []string{"discord.com:443", "gateway.discord.gg:443"},
			Spawns: false,
		},
	}
}

// Validate is the plugin's own config check (plugin.validate, optional):
// closes the gap the old bundled connector's Validate() covered but that
// `conductor validate` never ran for a plugin.
func (discordPlugin) Validate(_ context.Context, req plugin.ValidateRequest) (plugin.ValidateResult, error) {
	if str(req.Config["bot_token"]) == "" && str(req.Config["webhook_url"]) == "" {
		return plugin.ValidateResult{Problems: []plugin.Problem{
			{Message: "set bot_token (bot API) or webhook_url (post-only)"},
		}}, nil
	}
	return plugin.ValidateResult{}, nil
}

// discordConn is one invocation's resolved connection config.
type discordConn struct {
	BotToken   string
	WebhookURL string
	APIBase    string
	GatewayURL string
}

func parseConn(m map[string]any) discordConn {
	return discordConn{
		BotToken:   str(m["bot_token"]),
		WebhookURL: str(m["webhook_url"]),
		APIBase:    strOr(m["api_base"], defaultAPIBase),
		GatewayURL: str(m["gateway_url"]),
	}
}

func (discordPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := parseConn(req.Connection)
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	switch req.Verb {
	case "post":
		return invokePost(conn, o)
	case "ask":
		return invokeAsk(conn, o)
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeMethodNotFound, "discord: unknown verb "+req.Verb)
}

func invokePost(conn discordConn, o map[string]any) (plugin.InvokeResult, error) {
	text := str(o["text"])
	if text == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "post: options.text is required")
	}
	// Webhook-only connection: post {"content": …} to the incoming webhook —
	// byte-identical to the legacy notify sink / the old bundled connector's
	// webhook-only path.
	if conn.BotToken == "" {
		if conn.WebhookURL == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "post: set bot_token or webhook_url")
		}
		if err := postWebhook(conn.WebhookURL, text); err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "post: "+err.Error())
		}
		return plugin.InvokeResult{Outputs: map[string]any{"id": "", "channel": ""}}, nil
	}
	channel := str(o["channel"])
	user := str(o["user"])
	if channel == "" && user == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "post: set options.channel or options.user")
	}
	if channel == "" {
		dm, err := openDM(conn, user)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "post: open dm: "+err.Error())
		}
		channel = dm
	}
	id, err := postMessage(conn, channel, text)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "post: "+err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"id": id, "channel": channel}}, nil
}

func invokeAsk(conn discordConn, o map[string]any) (plugin.InvokeResult, error) {
	if conn.BotToken == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "ask: needs a bot_token (the webhook_url connection is post-only)")
	}
	prompt := str(o["prompt"])
	if strings.TrimSpace(prompt) == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "ask: options.prompt is required")
	}
	title := str(o["title"])
	if title == "" {
		title = firstLine(prompt)
	}
	body := str(o["draft"])
	if body == "" {
		body = prompt
	}

	var channel string
	switch to := str(o["to"]); to {
	case "dm":
		user := str(o["user"])
		if user == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "ask: to: dm needs options.user (a Discord user id)")
		}
		dm, err := openDM(conn, user)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "ask: open dm: "+err.Error())
		}
		channel = dm
	case "thread":
		channel = str(o["channel"])
		if channel == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "ask: to: thread needs options.channel")
		}
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, fmt.Sprintf("ask: options.to must be dm|thread, got %q", to))
	}

	msgID, err := postMessage(conn, channel, renderAsk(title, body))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "ask: "+err.Error())
	}
	// channel alone collides two concurrent asks in the same channel (and,
	// worse, the same thread/DM reused for a later ask) onto one
	// conversation id; the posted question's own message id disambiguates
	// them, matched by the human using Discord's reply-to feature (see the
	// "reply" event's replied_to fact).
	ref := channel + ":" + msgID
	return plugin.InvokeResult{Outputs: map[string]any{"ref": ref}}, nil
}

// renderAsk formats the question/draft the way the old bundled connector's
// hand-off presentation did.
func renderAsk(title, body string) string {
	var b strings.Builder
	if title != "" {
		b.WriteString("**")
		b.WriteString(title)
		b.WriteString("**\n")
	}
	b.WriteString(body)
	b.WriteString("\n\n_Reply here:_ `approve`, `discard`, or the revised text to send back to the agent.")
	return b.String()
}

// firstLine trims a string to its first non-empty line (for titles).
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			if len(t) > 120 {
				return t[:120] + "…"
			}
			return t
		}
	}
	return s
}

// --- REST: post / open-dm / webhook ---

// discordCall issues an authenticated JSON request against the Discord REST
// API (conn.APIBase) and decodes the response.
func discordCall(conn discordConn, method, path string, payload any) (map[string]any, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	httpReq, err := http.NewRequest(method, strings.TrimRight(conn.APIBase, "/")+path, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bot "+conn.BotToken)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var eb struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &eb)
		if eb.Message != "" {
			return nil, fmt.Errorf("%s %s: %s (%d)", method, path, eb.Message, resp.StatusCode)
		}
		return nil, fmt.Errorf("%s %s: unexpected status %d", method, path, resp.StatusCode)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decoding response: %w", err)
		}
	}
	return out, nil
}

// postMessage sends text to channel via POST /channels/{id}/messages and
// returns the new message's id.
func postMessage(conn discordConn, channel, text string) (string, error) {
	out, err := discordCall(conn, http.MethodPost, "/channels/"+channel+"/messages", map[string]string{"content": text})
	if err != nil {
		return "", err
	}
	id, _ := out["id"].(string)
	return id, nil
}

// openDM opens (or reuses — Discord's endpoint is idempotent per recipient)
// a DM channel with user via POST /users/@me/channels.
func openDM(conn discordConn, user string) (string, error) {
	out, err := discordCall(conn, http.MethodPost, "/users/@me/channels", map[string]string{"recipient_id": user})
	if err != nil {
		return "", err
	}
	id, _ := out["id"].(string)
	return id, nil
}

// postWebhook POSTs a JSON {"content": …} payload to a Discord incoming
// webhook — the post-only transport, kept byte-identical to the legacy
// notify sink / the old bundled connector's webhook-only path.
func postWebhook(webhookURL, content string) error {
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook post: HTTP %d", resp.StatusCode)
	}
	return nil
}

// --- source: the gateway, captures replies ---

// Gateway opcodes this client speaks.
// https://discord.com/developers/docs/topics/opcodes-and-status-codes
const (
	gwOpDispatch       = 0  // server->client: an event (t names it, d carries it)
	gwOpHeartbeat      = 1  // client->server: keep the connection alive
	gwOpIdentify       = 2  // client->server: authenticate + declare intents
	gwOpResume         = 6  // client->server: resume a prior session (token, session_id, seq)
	gwOpReconnect      = 7  // server->client: reconnect, then RESUME if possible
	gwOpInvalidSession = 9  // server->client: d:true resumable, d:false must re-identify
	gwOpHello          = 10 // server->client: first frame; carries heartbeat_interval
	gwOpHeartbeatACK   = 11 // server->client: heartbeat acknowledged
)

// gatewayFrame is one Discord gateway payload, sent or received.
type gatewayFrame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int            `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// gatewayState tracks the mutable state one gateway SESSION accumulates —
// the last dispatch sequence number (required on every heartbeat and on
// RESUME), the bot's own user id (captured from READY, so its own posts are
// never mistaken for a reply), and the session id RESUME needs. Unlike a
// connection attempt, a session survives a reconnect: it is created once per
// StartSource and handed to every runGatewayOnce call, so a dropped
// connection can RESUME instead of losing events to a brand-new session.
// selfID and sessionID are only ever touched by the single read-loop
// goroutine that owns a gatewayState (handleFrame's caller), so — like
// selfID already was — they need no lock; seq alone is also read by the
// separate heartbeat goroutine and keeps its own.
type gatewayState struct {
	mu        sync.Mutex
	seq       *int
	selfID    string
	sessionID string
}

func (gs *gatewayState) setSeq(s int) {
	gs.mu.Lock()
	gs.seq = &s
	gs.mu.Unlock()
}

func (gs *gatewayState) seqValue() (int, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.seq == nil {
		return 0, false
	}
	return *gs.seq, true
}

// resetSession discards the session (sessionID and seq): the next HELLO must
// IDENTIFY fresh rather than attempt to RESUME it. Called when the gateway
// tells us the session is gone for good (INVALID_SESSION with d:false).
func (gs *gatewayState) resetSession() {
	gs.mu.Lock()
	gs.seq = nil
	gs.mu.Unlock()
	gs.sessionID = ""
}

// gatewayAction tells the connection loop what to do after one frame.
// Parsing+deciding (handleFrame) is pure; only the loop performs I/O, which
// is what makes gateway behavior testable without a live socket.
type gatewayAction int

const (
	actionNone gatewayAction = iota
	actionIdentify
	actionResume
	actionReconnect
)

// helloPayload is HELLO's (op 10) `d`.
type helloPayload struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

// readyPayload is READY's (op 0, t="READY") `d` — only the fields this
// gateway needs. SessionID is what a later RESUME (op 6) identifies the
// session by.
type readyPayload struct {
	SessionID string `json:"session_id"`
	User      struct {
		ID string `json:"id"`
	} `json:"user"`
}

// messageCreatePayload is MESSAGE_CREATE's (op 0, t="MESSAGE_CREATE") `d`.
type messageCreatePayload struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
	Author    struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	} `json:"author"`
	// MessageReference is set when the human used Discord's own reply-to
	// feature — the only way a reply in a busy channel can be tied to a
	// SPECIFIC prior message (the ask's own) rather than just "any message
	// in this channel", which is what lets two concurrent asks in one
	// channel resolve independently.
	MessageReference *struct {
		MessageID string `json:"message_id"`
	} `json:"message_reference"`
}

// handleFrame parses one raw gateway frame, updates gs (the sequence number
// off every frame that carries one; the bot's own user id off READY),
// emits a "reply" source event for an inbound MESSAGE_CREATE — skipping the
// bot's own messages (author.bot true, or author.id == gs.selfID) so the
// gateway can never resolve its own ask by posting the question — and
// reports what the connection loop should do next. heartbeatMS is only
// meaningful (and only set) when the returned action is actionIdentify.
//
// Pure and testable without a live socket; mirrors conductor's former
// bundled discord_gateway.go's handleDiscordFrame.
func handleFrame(gs *gatewayState, raw []byte, emit func(any) error, log func(string, ...any)) (action gatewayAction, heartbeatMS int) {
	if log == nil {
		log = func(string, ...any) {}
	}
	var f gatewayFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		log("discord gateway: malformed frame: %v", err)
		return actionNone, 0
	}
	if f.S != nil {
		gs.setSeq(*f.S)
	}
	switch f.Op {
	case gwOpHello:
		var h helloPayload
		if err := json.Unmarshal(f.D, &h); err != nil {
			log("discord gateway: malformed HELLO: %v", err)
			return actionNone, 0
		}
		// A session carried over from a prior connection (set on READY, kept
		// across reconnects by StartSource reusing the same gatewayState)
		// means RESUME instead of a fresh IDENTIFY — Discord replays
		// whatever dispatches were missed during the gap instead of this
		// plugin silently losing them (a MESSAGE_CREATE it never saw is a
		// reply the ask it was answering can never resolve).
		if gs.sessionID != "" {
			return actionResume, h.HeartbeatInterval
		}
		return actionIdentify, h.HeartbeatInterval
	case gwOpReconnect:
		// Discord's own docs: on op 7, close and reconnect, then RESUME —
		// the session is still good. Nothing to invalidate here.
		return actionReconnect, 0
	case gwOpInvalidSession:
		// d is `true` if the session MAY be resumed, `false` if a fresh
		// IDENTIFY is required. A malformed/absent d decodes to the zero
		// value false — the safe side (re-identify) if we can't tell.
		var resumable bool
		_ = json.Unmarshal(f.D, &resumable)
		if !resumable {
			gs.resetSession()
		}
		return actionReconnect, 0
	case gwOpDispatch:
		switch f.T {
		case "READY":
			var r readyPayload
			if err := json.Unmarshal(f.D, &r); err != nil {
				log("discord gateway: malformed READY: %v", err)
				return actionNone, 0
			}
			gs.selfID = r.User.ID
			gs.sessionID = r.SessionID
		case "MESSAGE_CREATE":
			var m messageCreatePayload
			if err := json.Unmarshal(f.D, &m); err != nil {
				log("discord gateway: malformed MESSAGE_CREATE: %v", err)
				return actionNone, 0
			}
			if m.Author.Bot || (gs.selfID != "" && m.Author.ID == gs.selfID) {
				return actionNone, 0
			}
			repliedTo := ""
			if m.MessageReference != nil {
				repliedTo = m.MessageReference.MessageID
			}
			_ = emit(map[string]any{
				"event": "reply",
				"title": "discord: reply in " + m.ChannelID,
				"dedup": m.ID,
				"context": map[string]any{
					"channel":    m.ChannelID,
					"author":     m.Author.ID,
					"author_bot": m.Author.Bot,
					"text":       m.Content,
					"message_id": m.ID,
					"replied_to": repliedTo,
				},
			})
		}
	}
	return actionNone, 0
}

// StartSource runs the persistent Discord gateway connection for one
// instance, reconnecting with capped backoff until ctx is cancelled. An
// instance with no bot_token (webhook_url-only, or unconfigured) has nothing
// to capture replies with, so it idles until the instance is torn down —
// there is nothing to clean up either way.
func (discordPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	conn := parseConn(req.Config)
	if conn.BotToken == "" {
		<-ctx.Done()
		return nil
	}
	fmt.Fprintf(os.Stderr, "discord[%s]: gateway connecting\n", req.Instance)
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	// One gatewayState for the whole instance, not one per connection
	// attempt: its session id and sequence number must survive a reconnect
	// for RESUME to have anything to resume.
	gs := &gatewayState{}
	for ctx.Err() == nil {
		err := runGatewayOnce(ctx, conn, gs, emit)
		if ctx.Err() != nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "discord[%s]: gateway connection ended (%v); reconnecting in %s\n", req.Instance, err, backoff)
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

// gatewayDialURL resolves the wss:// URL to dial: conn.GatewayURL verbatim
// when set (the override a test double needs), else the bootstrapped
// GET /gateway/bot URL (authenticated, rate-limited per bot rather than
// globally — unlike the unauthenticated GET /gateway).
func gatewayDialURL(conn discordConn) (string, error) {
	if conn.GatewayURL != "" {
		return conn.GatewayURL, nil
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(conn.APIBase, "/")+discordGatewayPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+conn.BotToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gateway/bot: unexpected status %d", resp.StatusCode)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("gateway/bot: empty url")
	}
	return out.URL + "?v=10&encoding=json", nil
}

// runGatewayOnce opens one gateway session and pumps frames (via handleFrame)
// until it closes or the gateway asks for a reconnect.
func runGatewayOnce(ctx context.Context, conn discordConn, gs *gatewayState, emit func(any) error) error {
	wss, err := gatewayDialURL(conn)
	if err != nil {
		return fmt.Errorf("gateway url: %w", err)
	}
	c, _, err := websocket.DefaultDialer.DialContext(ctx, wss, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetReadLimit(1 << 20)

	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()

	var startHeartbeat sync.Once
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return err
		}
		action, heartbeatMS := handleFrame(gs, data, emit, nil)
		switch action {
		case actionIdentify:
			if err := sendIdentify(c, conn.BotToken); err != nil {
				return fmt.Errorf("identify: %w", err)
			}
			startHeartbeat.Do(func() {
				go heartbeatLoop(hbCtx, c, gs, time.Duration(heartbeatMS)*time.Millisecond)
			})
		case actionResume:
			if err := sendResume(c, gs, conn.BotToken); err != nil {
				return fmt.Errorf("resume: %w", err)
			}
			startHeartbeat.Do(func() {
				go heartbeatLoop(hbCtx, c, gs, time.Duration(heartbeatMS)*time.Millisecond)
			})
		case actionReconnect:
			return fmt.Errorf("gateway requested reconnect")
		}
	}
}

// heartbeatLoop sends op 1 (heartbeat, carrying the last-seen sequence
// number) every interval until ctx is done or a send fails — a failed send
// ends the read loop too (the connection is dead either way), which
// runGatewayOnce's caller reconnects on.
func heartbeatLoop(ctx context.Context, c *websocket.Conn, gs *gatewayState, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second // HELLO should always set this; a floor in case it doesn't.
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := sendHeartbeat(c, gs); err != nil {
				return
			}
		}
	}
}

func sendHeartbeat(c *websocket.Conn, gs *gatewayState) error {
	f := gatewayFrame{Op: gwOpHeartbeat, D: json.RawMessage("null")}
	if seq, ok := gs.seqValue(); ok {
		b, err := json.Marshal(seq)
		if err != nil {
			return err
		}
		f.D = b
	}
	return c.WriteJSON(f)
}

// sendResume sends op 6 (Resume): the session id and last-seen sequence
// number from a PRIOR connection, carried over in gs — the whole point of a
// gatewayState outliving a single runGatewayOnce call.
func sendResume(c *websocket.Conn, gs *gatewayState, botToken string) error {
	seq, _ := gs.seqValue()
	d, err := json.Marshal(map[string]any{
		"token":      botToken,
		"session_id": gs.sessionID,
		"seq":        seq,
	})
	if err != nil {
		return err
	}
	return c.WriteJSON(gatewayFrame{Op: gwOpResume, D: d})
}

func sendIdentify(c *websocket.Conn, botToken string) error {
	d, err := json.Marshal(map[string]any{
		"token":   botToken,
		"intents": discordIntents,
		"properties": map[string]string{
			"os":      "linux",
			"browser": "conductor",
			"device":  "conductor",
		},
	})
	if err != nil {
		return err
	}
	return c.WriteJSON(gatewayFrame{Op: gwOpIdentify, D: d})
}

func main() {
	if err := plugin.Serve(discordPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-discord:", err)
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
