package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// memHost is an in-memory hostAPI: a fixed token (or a sequence of tokens
// on refresh) and a state map shared across "restarts".
type memHost struct {
	mu        sync.Mutex
	store     map[string]any
	refreshes int
}

func newMemHost() *memHost { return &memHost{store: map[string]any{}} }

func (m *memHost) Token(_ context.Context, refresh bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if refresh {
		m.refreshes++
		return "fresh-token", nil
	}
	return "test-token", nil
}
func (m *memHost) Get(_ context.Context, k string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Round-trip through JSON like the real host does.
	v, ok := m.store[k]
	if !ok {
		return nil, nil
	}
	var out any
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &out)
	return out, nil
}
func (m *memHost) Put(_ context.Context, k string, v any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store[k] = v
	return nil
}

// fakeGoogle records watch/stop calls and answers like the Calendar API.
type fakeGoogle struct {
	srv      *httptest.Server
	watches  atomic.Int32
	stops    atomic.Int32
	lastBody atomic.Value // map[string]any of the last watch body
	expireIn atomic.Int64 // time.Duration
	fail401  atomic.Int32 // answer this many requests with 401 first
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	g := &fakeGoogle{}
	g.expireIn.Store(int64(7 * 24 * time.Hour))
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.fail401.Load() > 0 {
			g.fail401.Add(-1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events/watch"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.lastBody.Store(body)
			g.watches.Add(1)
			writeJSON(w, 200, map[string]any{
				"kind": "api#channel", "id": body["id"], "resourceId": "res-1",
				"expiration": strconv.FormatInt(time.Now().Add(time.Duration(g.expireIn.Load())).UnixMilli(), 10),
			})
		case r.Method == http.MethodPost && r.URL.Path == "/channels/stop":
			g.stops.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func itoa(n int64) string {
	return strings.TrimSpace(json.Number(func() string { b, _ := json.Marshal(n); return string(b) }()).String())
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}

type events struct {
	mu  sync.Mutex
	got []map[string]any
}

func (e *events) emit(v any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, v.(map[string]any))
	return nil
}
func (e *events) n() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.got) }

func newTestWatcher(t *testing.T, g *fakeGoogle, h hostAPI, ev *events, debounce time.Duration) *watcher {
	t.Helper()
	listen := freeAddr(t)
	return &watcher{
		p:        newGoogleCalendarPlugin(),
		host:     h,
		instance: "cal",
		conn:     gcalConn{apiBase: g.srv.URL, defaultCalID: "me@example.com"},
		cfg: watchCfg{
			listen: listen, path: "/gcal", address: "https://hooks.example.com/gcal",
			debounce: debounce, renewBefore: 24 * time.Hour, ttl: 7 * 24 * time.Hour,
		},
		emit: ev.emit,
		logf: func(f string, a ...any) { t.Logf(f, a...) },
		now:  time.Now,
	}
}

func startWatcher(t *testing.T, w *watcher) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.run(ctx); close(done) }()
	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.current != nil
	})
	waitFor(t, func() bool {
		c, err := net.Dial("tcp", w.cfg.listen)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	})
	return func() { cancel(); <-done }
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func cur(w *watcher) channel {
	w.mu.Lock()
	defer w.mu.Unlock()
	return *w.current
}

func push(t *testing.T, w *watcher, id, token, state string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+w.cfg.listen+w.cfg.path, nil)
	req.Header.Set(headerChannelID, id)
	req.Header.Set(headerChannelToken, token)
	req.Header.Set(headerResourceState, state)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestWatchOpensChannelAndDebouncesVerifiedPushes(t *testing.T) {
	g := newFakeGoogle(t)
	h := newMemHost()
	ev := &events{}
	w := newTestWatcher(t, g, h, ev, 150*time.Millisecond)
	stop := startWatcher(t, w)
	defer stop()

	body := g.lastBody.Load().(map[string]any)
	if body["type"] != "web_hook" || body["address"] != "https://hooks.example.com/gcal" {
		t.Fatalf("watch body: %v", body)
	}
	w.mu.Lock()
	c := *w.current
	w.mu.Unlock()
	if body["token"] != c.Token || body["id"] != c.ID || c.ResourceID != "res-1" {
		t.Fatalf("channel %+v vs body %v", c, body)
	}

	push(t, w, c.ID, c.Token, "sync")             // handshake: dropped
	push(t, w, c.ID, "wrong", "exists")           // bad token: dropped
	push(t, w, "someone-else", c.Token, "exists") // foreign channel: dropped
	for i := 0; i < 3; i++ {
		if code := push(t, w, c.ID, c.Token, "exists"); code != http.StatusAccepted {
			t.Fatalf("push status %d", code)
		}
	}
	waitFor(t, func() bool { return ev.n() == 1 })
	time.Sleep(300 * time.Millisecond)
	if ev.n() != 1 {
		t.Fatalf("a burst should fire once, got %d", ev.n())
	}
	got := ev.got[0]
	ctx := got["context"].(map[string]any)
	if got["event"] != "changed" || ctx["calendar_id"] != "me@example.com" || ctx["notifications"] != 3 {
		t.Fatalf("event: %v", got)
	}
}

func TestWatchReusesPersistedChannelAcrossRestart(t *testing.T) {
	g := newFakeGoogle(t)
	h := newMemHost()
	ev := &events{}
	w1 := newTestWatcher(t, g, h, ev, 50*time.Millisecond)
	stop := startWatcher(t, w1)
	first := cur(w1).ID
	stop()

	w2 := newTestWatcher(t, g, h, ev, 50*time.Millisecond)
	w2.cfg.listen = freeAddr(t)
	stop = startWatcher(t, w2)
	defer stop()
	if c := cur(w2); c.ID != first || g.watches.Load() != 1 {
		t.Fatalf("restart should reuse channel %s; got %s after %d watch calls", first, c.ID, g.watches.Load())
	}
	push(t, w2, first, cur(w2).Token, "exists")
	waitFor(t, func() bool { return ev.n() == 1 })
}

func TestWatchReplacesChannelForADifferentAddress(t *testing.T) {
	g := newFakeGoogle(t)
	h := newMemHost()
	w1 := newTestWatcher(t, g, h, &events{}, time.Second)
	stop := startWatcher(t, w1)
	stop()

	w2 := newTestWatcher(t, g, h, &events{}, time.Second)
	w2.cfg.address = "https://elsewhere.example.com/gcal"
	stop = startWatcher(t, w2)
	defer stop()
	if g.watches.Load() != 2 || g.stops.Load() != 1 {
		t.Fatalf("watches=%d stops=%d, want a new channel and the old one stopped", g.watches.Load(), g.stops.Load())
	}
}

func TestWatchRenewsAheadOfExpiryWithoutAGap(t *testing.T) {
	g := newFakeGoogle(t)
	g.expireIn.Store(int64(24*time.Hour + 300*time.Millisecond)) // inside renew_before almost at once
	h := newMemHost()
	w := newTestWatcher(t, g, h, &events{}, time.Second)
	stop := startWatcher(t, w)
	defer stop()
	first := cur(w).ID
	g.expireIn.Store(int64(7 * 24 * time.Hour))
	waitFor(t, func() bool { return g.watches.Load() == 2 && g.stops.Load() == 1 })
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current.ID == first {
		t.Fatal("renewal should switch to the new channel")
	}
}

func TestWatchRetriesOnceWithARefreshedTokenOn401(t *testing.T) {
	g := newFakeGoogle(t)
	g.fail401.Store(1)
	h := newMemHost()
	w := newTestWatcher(t, g, h, &events{}, time.Second)
	stop := startWatcher(t, w)
	defer stop()
	h.mu.Lock()
	refreshes := h.refreshes
	h.mu.Unlock()
	if refreshes != 1 || g.watches.Load() != 1 {
		t.Fatalf("refreshes=%d watches=%d", refreshes, g.watches.Load())
	}
}

func TestParseWatch(t *testing.T) {
	ok, err := parseWatch(map[string]any{"watch": map[string]any{"listen": "127.0.0.1:1", "public_url": "https://x.ts.net"}})
	if err != nil || ok.address != "https://x.ts.net/gcal" || ok.debounce != defaultDebounce {
		t.Fatalf("public_url base gets the path appended: %+v %v", ok, err)
	}
	for name, cfg := range map[string]map[string]any{
		"no block":       {},
		"no listen":      {"watch": map[string]any{"address": "https://x/gcal"}},
		"no address":     {"watch": map[string]any{"listen": "127.0.0.1:1"}},
		"http address":   {"watch": map[string]any{"listen": "127.0.0.1:1", "address": "http://x/gcal"}},
		"renew over ttl": {"watch": map[string]any{"listen": "127.0.0.1:1", "address": "https://x/gcal", "renew_before": "200h"}},
		"bad debounce":   {"watch": map[string]any{"listen": "127.0.0.1:1", "address": "https://x/gcal", "debounce": "soon"}},
	} {
		if _, err := parseWatch(cfg); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

var (
	_ plugin.HostAware = (*googleCalendarPlugin)(nil)
	_                  = sourcekit.Request{}
)
