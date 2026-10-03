package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// defaultAPIBase is the real Slack Web API. A connection's api_base field
// overrides it (test doubles) — a plugin's own process environment is
// scrubbed, so this takes the place of the builtin's PC_SLACK_API_URL env
// var.
const defaultAPIBase = "https://slack.com/api"

// slackAPI is the Slack Web API + Socket Mode client for one instance.
type slackAPI struct {
	base     string
	botToken string
	appToken string
	httpc    *http.Client

	names userNames // thread verb's users.info cache
}

func newSlackAPI(botToken, appToken, apiBase string) *slackAPI {
	base := defaultAPIBase
	if apiBase != "" {
		base = strings.TrimRight(apiBase, "/")
	}
	return &slackAPI{base: base, botToken: botToken, appToken: appToken, httpc: &http.Client{Timeout: 15 * time.Second}}
}

// call POSTs a JSON payload to a Web API method as the bot and decodes the
// ok/error envelope plus whatever else the caller wants from the body.
func (a *slackAPI) call(ctx context.Context, method string, payload map[string]any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.botToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := a.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("slack %s: HTTP %d: %w", method, resp.StatusCode, err)
	}
	if !env.OK {
		return slackAPIError(method, resp, env.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// get calls a read-style Web API method with query parameters.
func (a *slackAPI) get(ctx context.Context, method string, params url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/"+method+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.botToken)
	resp, err := a.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("slack %s: HTTP %d: %w", method, resp.StatusCode, err)
	}
	if !env.OK {
		return slackAPIError(method, resp, env.Error)
	}
	return json.Unmarshal(raw, out)
}

// slackAPIError classifies a Web API failure into the plugin contract's
// error codes (plugin-contract.md §1.11) where Slack's own answer makes the
// code knowable: a 429 (or the "ratelimited" envelope error some methods use
// without necessarily pairing it with one) is rate_limited, with retry_after
// from Retry-After when Slack sent it; "channel_not_found" and
// "message_not_found" are the addressed channel/message having gone away —
// target_gone. Everything else is left as a plain error: Slack's error
// strings are a large, evolving vocabulary (invalid_auth, missing_scope,
// not_in_channel, …) and most of them aren't cleanly one contract bucket or
// another.
func slackAPIError(method string, resp *http.Response, envErr string) error {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || envErr == "ratelimited":
		data := map[string]any{}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && n >= 0 {
				data["retry_after"] = (time.Duration(n) * time.Second).String()
			}
		}
		msg := envErr
		if msg == "" {
			msg = "ratelimited"
		}
		return plugin.Fail(plugin.CodeRateLimited, fmt.Sprintf("slack %s: %s", method, msg), data)
	case envErr == "channel_not_found", envErr == "message_not_found":
		return plugin.Fail(plugin.CodeTargetGone, fmt.Sprintf("slack %s: %s", method, envErr), nil)
	default:
		return fmt.Errorf("slack %s: %s", method, envErr)
	}
}

func (a *slackAPI) postMessage(ctx context.Context, channel, threadTS, text string) (string, error) {
	payload := map[string]any{"channel": channel, "text": text}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}
	var out struct {
		TS string `json:"ts"`
	}
	if err := a.call(ctx, "chat.postMessage", payload, &out); err != nil {
		return "", err
	}
	return out.TS, nil
}

func (a *slackAPI) postEphemeral(ctx context.Context, channel, user, text string) error {
	return a.call(ctx, "chat.postEphemeral", map[string]any{"channel": channel, "user": user, "text": text}, nil)
}

func (a *slackAPI) postEphemeralBlocks(ctx context.Context, channel, user, text string, blocks []any) error {
	payload := map[string]any{"channel": channel, "user": user, "text": text, "blocks": blocks}
	return a.call(ctx, "chat.postEphemeral", payload, nil)
}

func (a *slackAPI) react(ctx context.Context, channel, ts, emoji string) error {
	return a.call(ctx, "reactions.add", map[string]any{"channel": channel, "timestamp": ts, "name": emoji}, nil)
}

func (a *slackAPI) openDM(ctx context.Context, user string) (string, error) {
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := a.call(ctx, "conversations.open", map[string]any{"users": user}, &out); err != nil {
		return "", err
	}
	return out.Channel.ID, nil
}

func (a *slackAPI) viewsOpen(ctx context.Context, triggerID string, view map[string]any) error {
	return a.call(ctx, "views.open", map[string]any{"trigger_id": triggerID, "view": view}, nil)
}

// openSocket opens a Socket Mode session and returns the WebSocket URL.
func (a *slackAPI) openSocket(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/apps.connections.open", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.appToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("apps.connections.open: %s", out.Error)
	}
	return out.URL, nil
}

// replies pages conversations.replies until limit messages or the end.
func (a *slackAPI) replies(ctx context.Context, channel, ts string, limit int) (msgs []slackMessage, truncated bool, err error) {
	cursor := ""
	for page := 0; page < 100; page++ {
		p := url.Values{"channel": {channel}, "ts": {ts}, "limit": {"200"}, "inclusive": {"true"}}
		if cursor != "" {
			p.Set("cursor", cursor)
		}
		var out struct {
			Messages []slackMessage `json:"messages"`
			HasMore  bool           `json:"has_more"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := a.get(ctx, "conversations.replies", p, &out); err != nil {
			return nil, false, err
		}
		for _, m := range out.Messages {
			if len(msgs) >= limit {
				return msgs, true, nil
			}
			msgs = append(msgs, m)
		}
		cursor = out.Meta.NextCursor
		if cursor == "" {
			return msgs, false, nil
		}
	}
	return msgs, true, nil
}

// userNames caches users.info display names for the connector's lifetime.
type userNames struct {
	mu    sync.Mutex
	names map[string]string
}

func (a *slackAPI) userName(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	a.names.mu.Lock()
	if n, ok := a.names.names[id]; ok {
		a.names.mu.Unlock()
		return n
	}
	a.names.mu.Unlock()
	var out struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	name := id
	if err := a.get(ctx, "users.info", url.Values{"user": {id}}, &out); err == nil {
		for _, n := range []string{out.User.Profile.DisplayName, out.User.Profile.RealName, out.User.RealName, out.User.Name} {
			if strings.TrimSpace(n) != "" {
				name = strings.TrimSpace(n)
				break
			}
		}
	}
	a.names.mu.Lock()
	if a.names.names == nil {
		a.names.names = map[string]string{}
	}
	a.names.names[id] = name
	a.names.mu.Unlock()
	return name
}

func (a *slackAPI) permalink(ctx context.Context, channel, ts string) string {
	var out struct {
		Permalink string `json:"permalink"`
	}
	if err := a.get(ctx, "chat.getPermalink", url.Values{"channel": {channel}, "message_ts": {ts}}, &out); err != nil {
		return ""
	}
	return out.Permalink
}

// fileURLAllowed reports whether a file URL may receive the bot token: an
// https URL on slack.com, or the configured API base host (hermetic tests).
func (a *slackAPI) fileURLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if base, err := url.Parse(a.base); err == nil && base.Host != "" && u.Host == base.Host && u.Scheme == base.Scheme {
		return true
	}
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && (host == "slack.com" || strings.HasSuffix(host, ".slack.com"))
}

// fetchFile downloads one private file to path, refusing more than max bytes.
func (a *slackAPI) fetchFile(ctx context.Context, rawURL, path string, max int64) (int64, error) {
	return fetchFile(ctx, a, rawURL, path, max)
}
