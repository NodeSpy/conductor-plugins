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
			"bot_token":   {Type: "string", Desc: "Discord bot token (from the developer portal)"},
			"webhook_url": {Type: "string", Desc: "an incoming-webhook URL — post-only alternative to a bot token"},
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
					"ref":    {Type: "string", Desc: "the Discord channel (or DM channel) id the question was posted to — equal to the conversation_reply event's `channel` fact that resolves this ask"},
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
				},
				Filters: plugin.Schema{
					"channel": {Type: "string"},
					"author":  {Type: "string"},
				},
				Semantics: &plugin.EventSemantics{
					ConversationReply: &plugin.ConversationReply{ID: "channel", Author: "author", Text: "text"},
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

	if _, err := postMessage(conn, channel, renderAsk(title, body)); err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "ask: "+err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"ref": channel}}, nil
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
	gwOpReconnect      = 7  // server->client: reconnect (a fresh session, here)
	gwOpInvalidSession = 9  // server->client: session invalid; reconnect
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

// gatewayState tracks the mutable state one gateway connection accumulates
// across frames: the last dispatch sequence number (required on every
// heartbeat) and the bot's own user id (captured from READY, so its own
// posts are never mistaken for a reply). Fresh per connection attempt.
type gatewayState struct {
	mu     sync.Mutex
	seq    *int
	selfID string
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

// gatewayAction tells the connection loop what to do after one frame.
// Parsing+deciding (handleFrame) is pure; only the loop performs I/O, which
// is what makes gateway behavior testable without a live socket.
type gatewayAction int

const (
	actionNone gatewayAction = iota
	actionIdentify
	actionReconnect
)

// helloPayload is HELLO's (op 10) `d`.
type helloPayload struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

// readyPayload is READY's (op 0, t="READY") `d` — only the field this
// gateway needs.
type readyPayload struct {
	User struct {
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
		return actionIdentify, h.HeartbeatInterval
	case gwOpReconnect, gwOpInvalidSession:
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
		case "MESSAGE_CREATE":
			var m messageCreatePayload
			if err := json.Unmarshal(f.D, &m); err != nil {
				log("discord gateway: malformed MESSAGE_CREATE: %v", err)
				return actionNone, 0
			}
			if m.Author.Bot || (gs.selfID != "" && m.Author.ID == gs.selfID) {
				return actionNone, 0
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
	for ctx.Err() == nil {
		err := runGatewayOnce(ctx, conn, emit)
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
func runGatewayOnce(ctx context.Context, conn discordConn, emit func(any) error) error {
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

	gs := &gatewayState{}
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
