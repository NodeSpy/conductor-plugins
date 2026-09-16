// Command conductor-telegram is the Telegram connector as a standalone
// external conductor plugin (#59). As a connector it drives the Telegram Bot
// API over net/http for messaging verbs (send/edit/delete messages, photos,
// documents, callback-query answers, webhook management, and a generic `api`
// escape hatch). As a source it receives Telegram's webhook deliveries (a
// bare Update object POSTed to the configured path, authenticated with the
// `X-Telegram-Bot-Api-Secret-Token` header rather than an HMAC signature) and
// streams a normalized event per update for "message" and "callback_query".
//
// Built ONLY against the public SDK (pkg/plugin) and the connector-kit
// (pkg/sourcekit) — no other internal daemon package, no third-party deps.
//
// Connection (used for both Invoke and StartSource):
//
//	token: "<bot token>"                 # from @BotFather
//	api_base: "https://api.example.com"  # override the Bot API base (tests)
//	webhook:
//	  listen: ":9097"                    # HTTP listener address (StartSource only; optional if smee is set)
//	  path: "/telegram"                  # request path (default /telegram)
//	  secret: "<secret token>"           # compared to X-Telegram-Bot-Api-Secret-Token
//	  allow_unsigned: false              # explicit opt-in to accept unsigned webhooks
//	  smee: "https://smee.io/AbC123"     # smee.io-style SSE relay URL (optional; also/instead of listen)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// defaultAPIBase is the real Telegram Bot API. apiURL lets tests point
// callTelegram at an httptest.Server instead.
const defaultAPIBase = "https://api.telegram.org"

type telegram struct{}

func (telegram) Describe() plugin.Decl {
	verbOutputs := plugin.Schema{
		"result": {Type: "any", Desc: "the API call's `result` field, unwrapped from the envelope"},
		"ok":     {Type: "boolean"},
	}
	idOrStr := plugin.Field{Type: "any", Required: true, Desc: "numeric chat id, or @channelusername"}
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "telegram",
		Desc: "Telegram bot: send/edit/delete messages, photos, and documents; answer callback queries; manage the webhook — over the Telegram Bot API. Source events (message, callback_query) from an inbound webhook.",
		Connection: plugin.Schema{
			"token":    {Type: "string", Desc: "bot token from @BotFather (required for verbs)"},
			"api_base": {Type: "string", Desc: "override the Telegram Bot API base URL (tests)"},
			"webhook":  {Type: "map", Desc: "source transport: listen (optional if smee is set), path (default /telegram), secret, allow_unsigned, smee (smee.io-style SSE relay URL, e.g. https://smee.io/AbC123 — also (or instead) receive forwarded deliveries over SSE when the endpoint has no public URL)"},
		},
		Events: []plugin.Event{
			{
				Name: "message",
				Desc: "an incoming message",
				Context: plugin.Schema{
					"chat_id":    {Type: "integer"},
					"chat_type":  {Type: "string", Desc: "private, group, supergroup, or channel"},
					"text":       {Type: "string"},
					"from":       {Type: "string", Desc: "sender username"},
					"from_id":    {Type: "integer"},
					"message_id": {Type: "integer"},
					"command":    {Type: "string", Desc: "the command word (no leading /, no @botname), if text is a command"},
				},
				Filters: plugin.Schema{
					"chat_ids":   {Type: "list"},
					"chat_types": {Type: "list"},
					"commands":   {Type: "list"},
					"chat_id":    {Type: "integer"},
					"chat_type":  {Type: "string"},
					"command":    {Type: "string"},
				},
			},
			{
				Name: "callback_query",
				Desc: "an inline-keyboard callback query",
				Context: plugin.Schema{
					"callback_data": {Type: "string"},
					"from":          {Type: "string", Desc: "sender username"},
					"message_id":    {Type: "integer", Desc: "id of the message the keyboard is attached to"},
					"chat_id":       {Type: "integer"},
				},
				Filters: plugin.Schema{
					"chat_ids": {Type: "list"},
					"chat_id":  {Type: "integer"},
				},
			},
		},
		Verbs: []plugin.Verb{
			{
				Name: "send_message", Desc: "send a text message",
				Options: plugin.Schema{
					"chat_id":                  idOrStr,
					"text":                     {Type: "string", Required: true},
					"parse_mode":               {Type: "string", Enum: []string{"Markdown", "HTML"}},
					"reply_to_message_id":      {Type: "integer"},
					"disable_web_page_preview": {Type: "boolean"},
					"reply_markup":             {Type: "map", Desc: "inline keyboard passthrough"},
				},
				Outputs: verbOutputs,
			},
			{
				Name: "send_photo", Desc: "send a photo",
				Options: plugin.Schema{
					"chat_id": idOrStr,
					"photo":   {Type: "string", Required: true, Desc: "a URL or a Telegram file_id"},
					"caption": {Type: "string"},
				},
				Outputs: verbOutputs,
			},
			{
				Name: "send_document", Desc: "send a document",
				Options: plugin.Schema{
					"chat_id":  idOrStr,
					"document": {Type: "string", Required: true, Desc: "a URL or a Telegram file_id"},
					"caption":  {Type: "string"},
				},
				Outputs: verbOutputs,
			},
			{
				Name: "edit_message_text", Desc: "edit a previously-sent message's text",
				Options: plugin.Schema{
					"chat_id":      idOrStr,
					"message_id":   {Type: "integer", Required: true},
					"text":         {Type: "string", Required: true},
					"parse_mode":   {Type: "string", Enum: []string{"Markdown", "HTML"}},
					"reply_markup": {Type: "map"},
				},
				Outputs: verbOutputs,
			},
			{
				Name: "delete_message", Desc: "delete a message",
				Options: plugin.Schema{
					"chat_id":    idOrStr,
					"message_id": {Type: "integer", Required: true},
				},
				Outputs: verbOutputs,
			},
			{
				Name: "answer_callback_query", Desc: "answer an inline-keyboard callback query",
				Options: plugin.Schema{
					"callback_query_id": {Type: "string", Required: true},
					"text":              {Type: "string"},
					"show_alert":        {Type: "boolean"},
				},
				Outputs: verbOutputs,
			},
			{
				Name: "get_updates", Desc: "long-poll for updates (getUpdates)",
				Options: plugin.Schema{
					"offset":  {Type: "integer"},
					"limit":   {Type: "integer"},
					"timeout": {Type: "integer"},
				},
				Outputs: verbOutputs,
			},
			{
				Name: "set_webhook", Desc: "register the webhook URL Telegram delivers updates to",
				Options: plugin.Schema{
					"url":             {Type: "string", Required: true},
					"secret_token":    {Type: "string", Desc: "sent back as X-Telegram-Bot-Api-Secret-Token on every delivery"},
					"allowed_updates": {Type: "list"},
				},
				Outputs: verbOutputs,
			},
			{
				Name: "delete_webhook", Desc: "remove the registered webhook",
				Outputs: verbOutputs,
			},
			{
				Name: "get_me", Desc: "fetch this bot's own user info",
				Outputs: verbOutputs,
			},
			{
				Name: "api", Desc: "call any Telegram Bot API method directly",
				Usage: "escape hatch for a method without a first-class verb",
				Options: plugin.Schema{
					"method": {Type: "string", Required: true, Desc: "the Telegram Bot API method name, e.g. pinChatMessage"},
					"params": {Type: "map", Desc: "the method's JSON body"},
				},
				Outputs: verbOutputs,
			},
		},
		// Talks to the Telegram Bot API and nothing else; spawns no processes.
		Capabilities: plugin.Capabilities{Egress: []string{"api.telegram.org:443"}},
	}
}

// --- verbs: Telegram Bot API calls ---

var httpClient = &http.Client{Timeout: 30 * time.Second}

// apiURL builds the Bot API endpoint for method. base is overridable so
// tests can point it at an httptest.Server instead of the real API.
func apiURL(base, token, method string) string {
	if base == "" {
		base = defaultAPIBase
	}
	return strings.TrimRight(base, "/") + "/bot" + token + "/" + method
}

// callTelegram POSTs params as JSON to method and unwraps Telegram's
// {ok, result, description} envelope. ok:false surfaces description as the
// error text; the caller (Invoke) wraps that as CodeInternalError.
func callTelegram(ctx context.Context, base, token, method string, params map[string]any) (any, error) {
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("telegram: encode params: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL(base, token, method), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer resp.Body.Close()

	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("telegram: %s: decode response: %w", method, err)
	}
	if !env.OK {
		return nil, fmt.Errorf("%s", strOr(env.Description, "telegram: "+method+" failed"))
	}
	var result any
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &result); err != nil {
			return nil, fmt.Errorf("telegram: %s: decode result: %w", method, err)
		}
	}
	return result, nil
}

func (telegram) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	token := str(req.Connection["token"])
	if token == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "telegram: connection.token is required")
	}
	base := str(req.Connection["api_base"])
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	method, params, err := verbCall(req.Verb, o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}
	result, err := callTelegram(context.Background(), base, token, method, params)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"result": result, "ok": true}}, nil
}

// verbCall maps one verb call onto (Telegram method, JSON body). Pure and
// hermetically testable — no request is sent here.
func verbCall(verb string, o map[string]any) (method string, params map[string]any, err error) {
	switch verb {
	case "send_message":
		chatID, err := requireField(o, "chat_id")
		if err != nil {
			return "", nil, err
		}
		text, err := requireField(o, "text")
		if err != nil {
			return "", nil, err
		}
		p := map[string]any{"chat_id": chatID, "text": text}
		setOpt(p, "parse_mode", o["parse_mode"])
		setOpt(p, "reply_to_message_id", o["reply_to_message_id"])
		setOpt(p, "disable_web_page_preview", o["disable_web_page_preview"])
		setOpt(p, "reply_markup", o["reply_markup"])
		return "sendMessage", p, nil
	case "send_photo":
		chatID, err := requireField(o, "chat_id")
		if err != nil {
			return "", nil, err
		}
		photo, err := requireField(o, "photo")
		if err != nil {
			return "", nil, err
		}
		p := map[string]any{"chat_id": chatID, "photo": photo}
		setOpt(p, "caption", o["caption"])
		return "sendPhoto", p, nil
	case "send_document":
		chatID, err := requireField(o, "chat_id")
		if err != nil {
			return "", nil, err
		}
		doc, err := requireField(o, "document")
		if err != nil {
			return "", nil, err
		}
		p := map[string]any{"chat_id": chatID, "document": doc}
		setOpt(p, "caption", o["caption"])
		return "sendDocument", p, nil
	case "edit_message_text":
		chatID, err := requireField(o, "chat_id")
		if err != nil {
			return "", nil, err
		}
		msgID, err := requireField(o, "message_id")
		if err != nil {
			return "", nil, err
		}
		text, err := requireField(o, "text")
		if err != nil {
			return "", nil, err
		}
		p := map[string]any{"chat_id": chatID, "message_id": msgID, "text": text}
		setOpt(p, "parse_mode", o["parse_mode"])
		setOpt(p, "reply_markup", o["reply_markup"])
		return "editMessageText", p, nil
	case "delete_message":
		chatID, err := requireField(o, "chat_id")
		if err != nil {
			return "", nil, err
		}
		msgID, err := requireField(o, "message_id")
		if err != nil {
			return "", nil, err
		}
		return "deleteMessage", map[string]any{"chat_id": chatID, "message_id": msgID}, nil
	case "answer_callback_query":
		id, err := requireField(o, "callback_query_id")
		if err != nil {
			return "", nil, err
		}
		p := map[string]any{"callback_query_id": id}
		setOpt(p, "text", o["text"])
		setOpt(p, "show_alert", o["show_alert"])
		return "answerCallbackQuery", p, nil
	case "get_updates":
		p := map[string]any{}
		setOpt(p, "offset", o["offset"])
		setOpt(p, "limit", o["limit"])
		setOpt(p, "timeout", o["timeout"])
		return "getUpdates", p, nil
	case "set_webhook":
		url, err := requireField(o, "url")
		if err != nil {
			return "", nil, err
		}
		p := map[string]any{"url": url}
		setOpt(p, "secret_token", o["secret_token"])
		setOpt(p, "allowed_updates", o["allowed_updates"])
		return "setWebhook", p, nil
	case "delete_webhook":
		return "deleteWebhook", map[string]any{}, nil
	case "get_me":
		return "getMe", map[string]any{}, nil
	case "api":
		m, err := requireField(o, "method")
		if err != nil {
			return "", nil, err
		}
		params, _ := o["params"].(map[string]any)
		if params == nil {
			params = map[string]any{}
		}
		return str(m), params, nil
	}
	return "", nil, fmt.Errorf("unknown verb")
}

// requireField returns o[key] or an error if it is absent, nil, or "".
func requireField(o map[string]any, key string) (any, error) {
	v, ok := o[key]
	if !ok || isBlank(v) {
		return nil, fmt.Errorf("%s is required", key)
	}
	return v, nil
}

func isBlank(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	}
	return false
}

// setOpt copies v into p under key, unless v is absent/blank — so an unset
// optional field is simply omitted from the JSON body rather than sent null.
func setOpt(p map[string]any, key string, v any) {
	if isBlank(v) {
		return
	}
	p[key] = v
}

// --- source: Telegram webhook updates ---

func (telegram) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	webhook, _ := cfg["webhook"].(map[string]any)
	secret := ""
	addr, path := "", "/telegram"
	smee := ""
	allowUnsigned := false
	if webhook != nil {
		secret = str(webhook["secret"])
		addr = str(webhook["listen"])
		if p := str(webhook["path"]); p != "" {
			path = p
		}
		allowUnsigned, _ = webhook["allow_unsigned"].(bool)
		smee = str(webhook["smee"])
	}
	// sourcekit.Listener.Secret stays empty: Telegram authenticates a webhook
	// delivery with a bare secret token it echoes back in the
	// X-Telegram-Bot-Api-Secret-Token header, not an HMAC signature — so
	// VerifyHMAC's scheme doesn't apply. We compare the header ourselves,
	// below, with a constant-time comparison.
	ln := sourcekit.Listener{Addr: addr, Path: path, Relay: smee}
	if ln.Addr == "" && ln.Relay == "" {
		return fmt.Errorf("telegram: no webhook.listen address or smee relay configured")
	}
	if err := requireWebhookSecret("telegram", secret, allowUnsigned); err != nil {
		return err
	}
	dedup := sourcekit.NewDedup(4096)
	if ln.Addr != "" {
		fmt.Fprintf(os.Stderr, "telegram[%s]: listening on %s%s\n", req.Instance, ln.Addr, ln.Path)
	}
	if ln.Relay != "" {
		fmt.Fprintf(os.Stderr, "telegram[%s]: relaying via smee channel %s\n", req.Instance, ln.Relay)
	}
	return ln.Serve(ctx, func(h http.Header, body []byte) {
		if !validSecretToken(secret, h.Get("X-Telegram-Bot-Api-Secret-Token"), allowUnsigned) {
			return
		}
		for _, ev := range parseUpdate(body) {
			if dk, _ := ev["dedup"].(string); dk != "" && !dedup.Add(dk) {
				continue
			}
			_ = emit(ev)
		}
	})
}

// validSecretToken reports whether an inbound delivery is authenticated. A
// configured secret must match exactly (constant-time); with no secret
// configured, StartSource already refused to start unless allow_unsigned is
// set, in which case every delivery passes.
func validSecretToken(want, got string, allowUnsigned bool) bool {
	if want == "" {
		return allowUnsigned
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// update is the subset of Telegram's Update object this plugin reads:
// https://core.telegram.org/bots/api#update
type update struct {
	UpdateID      int64       `json:"update_id"`
	Message       *tgMessage  `json:"message"`
	CallbackQuery *tgCallback `json:"callback_query"`
}

type tgUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgMessage struct {
	MessageID int64   `json:"message_id"`
	From      *tgUser `json:"from"`
	Chat      tgChat  `json:"chat"`
	Text      string  `json:"text"`
}

type tgCallback struct {
	ID      string     `json:"id"`
	Data    string     `json:"data"`
	From    *tgUser    `json:"from"`
	Message *tgMessage `json:"message"`
}

// parseUpdate turns one Update into zero or one normalized source events.
func parseUpdate(body []byte) []map[string]any {
	var u update
	if err := json.Unmarshal(body, &u); err != nil {
		return nil
	}
	dk := fmt.Sprintf("%d", u.UpdateID)
	switch {
	case u.Message != nil:
		return []map[string]any{messageEvent(dk, u.Message)}
	case u.CallbackQuery != nil:
		return []map[string]any{callbackQueryEvent(dk, u.CallbackQuery)}
	}
	return nil
}

func messageEvent(dedup string, m *tgMessage) map[string]any {
	username, fromID := "", int64(0)
	if m.From != nil {
		username, fromID = m.From.Username, m.From.ID
	}
	cmd := extractCommand(m.Text)
	return map[string]any{
		"event": "message", "kind": "message",
		"title": fmt.Sprintf("telegram message from %s", nonEmpty(username, fmt.Sprintf("%d", fromID))),
		"dedup": dedup,
		"context": map[string]any{
			"chat_id": m.Chat.ID, "chat_type": m.Chat.Type, "text": m.Text,
			"from": username, "from_id": fromID, "message_id": m.MessageID,
			"command": cmd,
			// Plural aliases so the documented filter vocabulary
			// (filters: {chat_ids/chat_types/commands: [...]}) matches
			// against the daemon's generic list-contains filter evaluator.
			"chat_ids": m.Chat.ID, "chat_types": m.Chat.Type, "commands": cmd,
		},
	}
}

func callbackQueryEvent(dedup string, cq *tgCallback) map[string]any {
	username, fromID := "", int64(0)
	if cq.From != nil {
		username, fromID = cq.From.Username, cq.From.ID
	}
	var msgID, chatID int64
	if cq.Message != nil {
		msgID = cq.Message.MessageID
		chatID = cq.Message.Chat.ID
	}
	return map[string]any{
		"event": "callback_query", "kind": "callback_query",
		"title": fmt.Sprintf("telegram callback_query from %s", nonEmpty(username, fmt.Sprintf("%d", fromID))),
		"dedup": dedup,
		"context": map[string]any{
			"callback_data": cq.Data, "from": username, "from_id": fromID,
			"message_id": msgID, "chat_id": chatID,
			"chat_ids": chatID,
		},
	}
}

// extractCommand returns the command word (no leading /, no @botname suffix)
// when text is a Telegram bot command, or "" otherwise.
func extractCommand(text string) string {
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	cmd := strings.TrimPrefix(fields[0], "/")
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	return cmd
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
	if err := plugin.Serve(telegram{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-telegram: %v\n", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

// requireWebhookSecret refuses to start an unauthenticated webhook
// listener.
//
// A missing secret is far more often a mistake than a choice — omitting it
// would otherwise silently accept ANY unsigned POST on the listen address as
// a real event from Telegram, which is remote trigger injection with no
// signal that it happened. It fails closed; `webhook.allow_unsigned: true` is
// the explicit, greppable way to say you meant it.
func requireWebhookSecret(who, secret string, allowUnsigned bool) error {
	if strings.TrimSpace(secret) != "" {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintf(os.Stderr, "%s: webhook.allow_unsigned is set — accepting UNSIGNED webhooks; anyone who can reach the listen address can fire triggers\n", who)
		return nil
	}
	return fmt.Errorf("%s: no webhook.secret configured — an unsigned listener accepts any POST on the listen address as a real event. Set it, or set `webhook.allow_unsigned: true` if you genuinely front this with something else that authenticates", who)
}
