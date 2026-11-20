package main

// The `changed` source: Google Calendar push notifications (events.watch).
//
// Google pushes a notification to an HTTPS address whenever a watched
// calendar's events change. The notification has an EMPTY body; it carries
// only headers — X-Goog-Channel-Id / -Token / X-Goog-Resource-State. It says
// THAT something changed, never what, so the event this source emits is a
// nudge: the consumer re-reads the calendar (events verb) to see the change.
//
// This source owns the channel lifecycle end to end, so a config needs no
// setup workflow and no renewal cron:
//
//   - start: reuse the persisted channel (host.state) when it still has more
//     than renew_before left on it and points at the same address; otherwise
//     open a new one (POST /calendars/{id}/events/watch) with a fresh random
//     channel id and secret token.
//   - receive: a POST is accepted only when its channel id is the current
//     one AND X-Goog-Channel-Token equals the channel's secret (constant
//     time). The handshake ping (resource state "sync") is dropped. A burst of
//     notifications collapses into one event after `debounce` (Google often
//     sends several for one edit).
//   - renew: renew_before ahead of expiry, open a replacement channel, then
//     stop the old one (POST /channels/stop) so there is no gap.
//   - stop: the channel is LEFT OPEN on shutdown so a restart within its
//     lifetime reuses it with no gap; it expires on its own otherwise.
//
// Reachability: Google only delivers to a public https:// URL. Either set
// `watch.address` yourself, or set `watch.expose` to an exposure connector
// (tailscale funnel, cloudflared, ngrok, a smee relay, …) and conductor opens
// it at instance start and fills `watch.public_url` in.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

const (
	stateChannelKey     = "watch_channel"
	defaultWatchPath    = "/gcal"
	defaultDebounce     = 5 * time.Second
	defaultRenewBefore  = 24 * time.Hour
	defaultWatchTTL     = 7 * 24 * time.Hour // Google's ceiling for events.watch
	minRetryBackoff     = 30 * time.Second
	maxRetryBackoff     = 30 * time.Minute
	resourceStateSync   = "sync"
	changedEvent        = "changed"
	headerChannelID     = "X-Goog-Channel-Id"
	headerChannelToken  = "X-Goog-Channel-Token"
	headerResourceState = "X-Goog-Resource-State"
	headerMessageNumber = "X-Goog-Message-Number"
)

// channel is one open events.watch channel, as persisted in host.state.
type channel struct {
	ID         string `json:"id"`
	Token      string `json:"token"`
	ResourceID string `json:"resource_id"`
	Address    string `json:"address"`
	CalendarID string `json:"calendar_id"`
	Expiration int64  `json:"expiration"` // unix ms, Google's own format
}

func (c *channel) expires() time.Time { return time.UnixMilli(c.Expiration) }

// watchCfg is the connection's `watch:` block.
type watchCfg struct {
	listen      string
	path        string
	address     string // the public https URL Google posts to
	debounce    time.Duration
	renewBefore time.Duration
	ttl         time.Duration
}

func parseWatch(cfg map[string]any) (watchCfg, error) {
	w, _ := cfg["watch"].(map[string]any)
	if w == nil {
		return watchCfg{}, fmt.Errorf("no watch: block — the changed source needs watch.listen and a public address (watch.address or watch.expose)")
	}
	wc := watchCfg{
		listen:  str(w["listen"]),
		path:    strOr(str(w["path"]), defaultWatchPath),
		address: strOr(str(w["address"]), str(w["public_url"])),
	}
	if !strings.HasPrefix(wc.path, "/") {
		wc.path = "/" + wc.path
	}
	var err error
	if wc.debounce, err = durOr(w["debounce"], defaultDebounce); err != nil {
		return wc, fmt.Errorf("watch.debounce: %w", err)
	}
	if wc.renewBefore, err = durOr(w["renew_before"], defaultRenewBefore); err != nil {
		return wc, fmt.Errorf("watch.renew_before: %w", err)
	}
	if wc.ttl, err = durOr(w["ttl"], defaultWatchTTL); err != nil {
		return wc, fmt.Errorf("watch.ttl: %w", err)
	}
	if wc.listen == "" {
		return wc, fmt.Errorf("watch.listen is required")
	}
	if wc.address == "" {
		return wc, fmt.Errorf("no public address: set watch.address, or watch.expose so conductor opens one")
	}
	// A public_url from an exposure is the tunnel's base; the host joins the
	// listener path itself only when the exposure is path-aware, so append
	// ours when it is missing.
	if u, perr := url.Parse(wc.address); perr == nil && (u.Path == "" || u.Path == "/") {
		u.Path = wc.path
		wc.address = u.String()
	}
	if !strings.HasPrefix(wc.address, "https://") {
		return wc, fmt.Errorf("watch address %q must be https:// (Google refuses anything else)", wc.address)
	}
	if wc.renewBefore >= wc.ttl {
		return wc, fmt.Errorf("watch.renew_before (%s) must be shorter than watch.ttl (%s)", wc.renewBefore, wc.ttl)
	}
	return wc, nil
}

func durOr(v any, def time.Duration) (time.Duration, error) {
	if v == nil || v == "" {
		return def, nil
	}
	return sourcekit.ParseDuration(v)
}

// SetHost implements plugin.HostAware: the source needs host.auth (a live
// token for watch/stop calls, which happen outside any verb invoke) and
// host.state (the channel, across restarts).
func (p *googleCalendarPlugin) SetHost(h *plugin.HostConn) { p.host = h }

// hostAPI is the slice of the host channel the watcher uses (a seam for
// tests; production is hostConnAPI over plugin.HostConn).
type hostAPI interface {
	Token(ctx context.Context, refresh bool) (string, error)
	Get(ctx context.Context, key string) (any, error)
	Put(ctx context.Context, key string, value any) error
}

type hostConnAPI struct {
	h        *plugin.HostConn
	instance string
}

func (a hostConnAPI) Token(ctx context.Context, refresh bool) (string, error) {
	return a.h.Auth(a.instance).Token(ctx, refresh)
}
func (a hostConnAPI) Get(ctx context.Context, key string) (any, error) {
	return a.h.State(a.instance).Get(ctx, key)
}
func (a hostConnAPI) Put(ctx context.Context, key string, value any) error {
	return a.h.State(a.instance).Put(ctx, key, value, "")
}

// watcher is one running `changed` source instance.
type watcher struct {
	p        *googleCalendarPlugin
	host     hostAPI
	instance string
	conn     gcalConn
	cfg      watchCfg
	emit     func(any) error
	logf     func(string, ...any)
	now      func() time.Time

	mu      sync.Mutex
	current *channel
	pending *time.Timer // debounce timer; nil when idle
	count   int         // notifications folded into the pending event
}

func (p *googleCalendarPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	if p.host == nil {
		return fmt.Errorf("google-calendar[%s]: host channel unavailable (conductor too old for host.auth/host.state)", req.Instance)
	}
	cfg, err := parseWatch(req.Config)
	if err != nil {
		return fmt.Errorf("google-calendar[%s]: %w", req.Instance, err)
	}
	w := &watcher{
		p:        p,
		host:     hostConnAPI{h: p.host, instance: req.Instance},
		instance: req.Instance,
		conn: gcalConn{
			apiBase:      strings.TrimRight(strOr(str(req.Config["api_base"]), defaultAPIBase), "/"),
			defaultCalID: strOr(str(req.Config["calendar_id"]), "primary"),
		},
		cfg:  cfg,
		emit: emit,
		logf: func(f string, a ...any) {
			msg := fmt.Sprintf(f, a...)
			fmt.Fprintf(os.Stderr, "google-calendar[%s]: %s\n", req.Instance, msg)
			p.host.Log(ctx, req.Instance, msg)
		},
		now: time.Now,
	}
	return w.run(ctx)
}

func (w *watcher) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ln := sourcekit.Listener{Addr: w.cfg.listen, Path: w.cfg.path, MaxBytes: 64 << 10}
	errc := make(chan error, 1)
	go func() { errc <- ln.ServeReq(ctx, w.receive) }()

	w.adopt(ctx)
	go w.maintain(ctx)

	err := <-errc
	w.mu.Lock()
	if w.pending != nil {
		w.pending.Stop()
	}
	w.mu.Unlock()
	return err
}

// adopt reuses the persisted channel when it is still good for this
// calendar and address.
func (w *watcher) adopt(ctx context.Context) {
	v, err := w.host.Get(ctx, stateChannelKey)
	if err != nil || v == nil {
		return
	}
	raw, _ := json.Marshal(v)
	var c channel
	if json.Unmarshal(raw, &c) != nil || c.ID == "" {
		return
	}
	if c.Address != w.cfg.address || c.CalendarID != w.conn.defaultCalID {
		w.logf("persisted channel %s is for %s @ %s; replacing it", c.ID, c.CalendarID, c.Address)
		w.stopChannel(ctx, &c)
		return
	}
	if c.expires().Sub(w.now()) <= w.cfg.renewBefore {
		return // maintain renews it straight away
	}
	w.mu.Lock()
	w.current = &c
	w.mu.Unlock()
	w.logf("reusing watch channel %s (expires %s)", c.ID, c.expires().Format(time.RFC3339))
}

// maintain opens the channel when there is none and renews it ahead of
// expiry, retrying failures with capped backoff, until ctx ends.
func (w *watcher) maintain(ctx context.Context) {
	backoff := minRetryBackoff
	for {
		w.mu.Lock()
		cur := w.current
		w.mu.Unlock()
		var wait time.Duration
		if cur != nil {
			wait = cur.expires().Add(-w.cfg.renewBefore).Sub(w.now())
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		if err := w.open(ctx, cur); err != nil {
			w.logf("opening watch channel failed (retrying in %s): %v", backoff, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxRetryBackoff)
			continue
		}
		backoff = minRetryBackoff
	}
}

// open registers a new channel, persists it, makes it current, then stops
// old (if any). Old stays valid until the new one is in place: no gap.
func (w *watcher) open(ctx context.Context, old *channel) error {
	c := &channel{
		ID:         "conductor-" + randHex(12),
		Token:      randHex(24),
		Address:    w.cfg.address,
		CalendarID: w.conn.defaultCalID,
	}
	body := map[string]any{
		"id":      c.ID,
		"type":    "web_hook",
		"address": c.Address,
		"token":   c.Token,
		"params":  map[string]any{"ttl": strconv.FormatInt(int64(w.cfg.ttl/time.Second), 10)},
	}
	var res struct {
		ResourceID string          `json:"resourceId"`
		Expiration json.RawMessage `json:"expiration"` // int64 ms; Google sends it as a string
	}
	path := "/calendars/" + url.PathEscape(c.CalendarID) + "/events/watch"
	if err := w.call(ctx, http.MethodPost, path, body, &res); err != nil {
		return err
	}
	c.ResourceID = res.ResourceID
	exp, _ := strconv.ParseInt(strings.Trim(string(res.Expiration), `"`), 10, 64)
	if exp == 0 {
		exp = w.now().Add(w.cfg.ttl).UnixMilli()
	}
	c.Expiration = exp
	if err := w.host.Put(ctx, stateChannelKey, c); err != nil {
		w.logf("could not persist watch channel %s (a restart will open a new one): %v", c.ID, err)
	}
	w.mu.Lock()
	w.current = c
	w.mu.Unlock()
	w.logf("watching %s on channel %s until %s", c.CalendarID, c.ID, c.expires().Format(time.RFC3339))
	if old != nil {
		w.stopChannel(ctx, old)
	}
	return nil
}

func (w *watcher) stopChannel(ctx context.Context, c *channel) {
	if err := w.call(ctx, http.MethodPost, "/channels/stop", map[string]any{"id": c.ID, "resourceId": c.ResourceID}, nil); err != nil {
		// 404: already expired or stopped. Anything else: it lapses on its
		// own at expiry, and its notifications are refused meanwhile.
		w.logf("stopping old channel %s: %v", c.ID, err)
	}
}

// call is an authenticated API request with the live managed token, retried
// once with a refreshed token on 401.
func (w *watcher) call(ctx context.Context, method, path string, body, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := w.host.Token(ctx, attempt > 0)
		if err != nil {
			return err
		}
		status, raw, err := w.p.do(token, method, w.conn.apiBase+path, nil, body)
		if status == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		if err != nil {
			return err
		}
		if out != nil && len(raw) > 0 {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
	return fmt.Errorf("%s %s: unauthorized after token refresh", method, path)
}

// receive handles one POST from Google. sourcekit answers 202 after it
// returns, refused or not — Google only needs a 2xx to stop retrying, and
// a refused delivery must not tell the sender why.
func (w *watcher) receive(r *sourcekit.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.current
	if c == nil || r.Header.Get(headerChannelID) != c.ID {
		return // a stale or foreign channel
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(headerChannelToken)), []byte(c.Token)) != 1 {
		return
	}
	if r.Header.Get(headerResourceState) == resourceStateSync {
		return // the handshake Google sends when a channel opens
	}
	w.count++
	if w.pending != nil {
		return // already scheduled; this one folds in
	}
	w.pending = time.AfterFunc(w.cfg.debounce, w.fire)
}

func (w *watcher) fire() {
	w.mu.Lock()
	n := w.count
	calID := ""
	if w.current != nil {
		calID = w.current.CalendarID
	}
	w.count = 0
	w.pending = nil
	w.mu.Unlock()
	if n == 0 {
		return
	}
	now := w.now().UTC()
	_ = w.emit(map[string]any{
		"event": changedEvent,
		"kind":  changedEvent,
		"title": fmt.Sprintf("calendar changed: %s", calID),
		// Unique per burst: every fired burst is a real change to act on.
		"dedup": fmt.Sprintf("%s:%d", calID, now.UnixNano()),
		"context": map[string]any{
			"calendar_id":   calID,
			"notifications": n,
			"at":            now.Format(time.RFC3339),
		},
	})
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
