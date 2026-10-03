package ghsource

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// smeeClient streams the smee channel with tuned TCP keep-alive. smee sends no
// application heartbeat, so on a half-open connection (a network blip with no clean
// close) a plain reader blocks until the kernel's default retransmit timeout gives
// up — ~15 min of blindness. Keep-alive probes (idle 20s, every 15s, ×4) surface a
// dead peer in ~80s so the reconnect loop kicks in fast; because probes only error
// when they actually fail, a healthy-but-quiet channel is never reconnected (no
// event-drop window). No Client.Timeout — this is a long-lived stream.
var smeeClient = newStreamClient()

func newStreamClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 20 * time.Second, // legacy fallback; KeepAliveConfig governs on Go ≥1.23
		KeepAliveConfig: net.KeepAliveConfig{
			Enable: true, Idle: 20 * time.Second, Interval: 15 * time.Second, Count: 4,
		},
	}).DialContext
	return &http.Client{Transport: tr}
}

// Start builds the App auth + REST client (when it can) and runs the
// configured webhook transports (smee channel and/or a direct HTTP
// listener), plus the optional sweep, until ctx is cancelled. Both
// transports share one delivery-dedup set.
//
// ensureClients' failure is fatal here ONLY when there is no webhook
// transport configured either — then the sweep is the sole possible event
// source and a credential is mandatory, exactly as before. An instance
// configured with only a webhook listener/smee channel and no app:/token: at
// all (so ensureClients has nothing to resolve, not even a `gh auth token`
// fallback) must still be able to start and receive deliveries: enrichment
// facts that need a live read are already optional (events.go's g.rest
// nil-checks), so a credential-less webhook receiver is a supported
// configuration. Credentials then resolve lazily the first time something
// actually needs the API: discoverSelf (best-effort, below), and
// g.sweep/stuckPass in their own loops — each retries ensureClients itself
// and logs rather than blocking on failure.
func (g *Source) Start(ctx context.Context, emit EmitFunc) error {
	if err := g.ensureClients(); err != nil {
		if !g.cfg.Webhook.Configured() {
			// No webhook either: the sweep (if enabled) is the ONLY possible
			// event source, so a credential is mandatory now, not optional.
			return err
		}
		log.Printf("github[%s]: no credentials yet (%v) — continuing webhook-only; "+
			"resolves lazily once one is available", g.name, err)
	}
	// Auto-discover me: from the write identity when it wasn't set
	// (best-effort; runs before the sweep so `self` is populated for its
	// first pass). A no-op log, not an error, if ensureClients just failed
	// above and credentials still aren't available (see discoverSelf).
	g.discoverSelf(ctx)
	if g.cfg.Webhook.Verify() && g.cfg.Webhook.SmeeURL != "" {
		log.Printf("github[%s]: signature verification ON — note the smee re-serialization caveat; "+
			"set webhook.verify_signature:false if valid deliveries are being dropped", g.name)
	}
	// g.renew (created in the constructor) nudges the sweep to run now and reset its
	// adaptive cadence — on a smee reconnect (dropped-webhook window) and on a manual
	// `sweep` (SweepNow). Buffered+coalescing. The sweepLoop drains it only when the
	// sweep is enabled; the smee reconnect nudge is harmless otherwise.
	if g.cfg.Sweep.IsEnabled() {
		if g.cfg.Webhook.Configured() {
			go g.sweepLoop(ctx, emit, g.renew) // adaptive: catch-up behind the webhook
		} else {
			go g.fixedSweepLoop(ctx, emit, g.renew) // fixed: the sweep is the event source
		}
	}
	// Stuck-check detection is its own periodic watcher — NOT part of the sweep. It
	// runs on a fixed cadence (poll_interval on the stuck_checks action, independent
	// of the sweep's adaptive backoff) over the repos of the rule(s) that configure
	// stuck_checks. So it doesn't depend on the sweep block at all.
	if g.anyStuckChecks() && len(g.stuckRepos()) > 0 {
		go g.stuckLoop(ctx, emit)
	}

	seen := newDeliveryDedup(2048)
	errc := make(chan error, 2)
	started := 0
	if g.cfg.Webhook.SmeeURL != "" {
		started++
		go func() { errc <- g.runSmee(ctx, emit, seen, g.renew) }()
	}
	if g.cfg.Webhook.Listen != "" {
		started++
		go func() { errc <- g.serveHTTP(ctx, emit, seen) }()
	}
	if started == 0 && !g.cfg.Sweep.IsEnabled() {
		return fmt.Errorf("github[%s]: no event source — configure a webhook transport or leave the sweep enabled", g.name)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errc:
		return err
	}
}

// runSmee streams the smee channel, reconnecting with backoff. On a reconnect
// AFTER a drop it nudges `renew` (a webhook may have been lost while we were
// disconnected), so the sweep catches the gap. `renew` may be nil.
func (g *Source) runSmee(ctx context.Context, emit EmitFunc, seen *deliveryDedup, renew chan<- struct{}) error {
	backoff := time.Second
	wasDown := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := g.streamSmee(ctx, emit, seen, func() {
			// Connected. If we'd previously dropped, signal a catch-up sweep
			// (non-blocking — a full buffer means one is already pending), and reset
			// the backoff so a later drop starts fresh rather than at the last cap.
			if wasDown && renew != nil {
				select {
				case renew <- struct{}{}:
				default:
				}
			}
			wasDown = false
			backoff = time.Second
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wasDown = true
		log.Printf("github[%s]: smee stream ended (%v); reconnecting in %s", g.name, err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// deliver is the shared path for both transports: dedup by delivery id, verify
// the signature over the exact received body, translate to Triggers, and emit.
func (g *Source) deliver(ctx context.Context, emit EmitFunc, seen *deliveryDedup, event, delivery, sig string, body []byte) {
	if event == "" || len(body) == 0 {
		return
	}
	if delivery != "" && !seen.add(delivery) {
		return // duplicate (smee reconnect redelivery, or a retried POST)
	}
	if g.cfg.Webhook.Verify() {
		if !verifySignature(g.cfg.Webhook.Secret, body, sig) {
			log.Printf("github[%s]: signature mismatch for delivery %s (dropped)", g.name, delivery)
			return
		}
	}
	for _, t := range g.triggersFor(ctx, event, body) {
		emit(ctx, t)
	}
}

// smeePayload is one forwarded webhook as delivered over the smee SSE channel.
type smeePayload struct {
	Event    string          `json:"x-github-event"`
	Delivery string          `json:"x-github-delivery"`
	Sig      string          `json:"x-hub-signature-256"`
	Body     json.RawMessage `json:"body"`
}

func (g *Source) streamSmee(ctx context.Context, emit EmitFunc, seen *deliveryDedup, onUp func()) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.Webhook.SmeeURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := smeeClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("smee connect: HTTP %d", resp.StatusCode)
	}
	if onUp != nil {
		onUp() // connection established
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				g.handleSmeeData(ctx, emit, seen, data.String())
				data.Reset()
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// ignore event:/id:/retry: and comments
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("stream closed")
}

func (g *Source) handleSmeeData(ctx context.Context, emit EmitFunc, seen *deliveryDedup, data string) {
	var p smeePayload
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return // control frame ("ready", ping, etc.)
	}
	g.deliver(ctx, emit, seen, p.Event, p.Delivery, p.Sig, p.Body)
}

// deliveryDedup is a bounded set of recently-seen delivery ids.
type deliveryDedup struct {
	mu   sync.Mutex
	max  int
	set  map[string]struct{}
	ring []string
}

func newDeliveryDedup(max int) *deliveryDedup {
	return &deliveryDedup{max: max, set: map[string]struct{}{}}
}

// add records id and reports true if it was new.
func (d *deliveryDedup) add(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.set[id]; ok {
		return false
	}
	d.set[id] = struct{}{}
	d.ring = append(d.ring, id)
	if len(d.ring) > d.max {
		old := d.ring[0]
		d.ring = d.ring[1:]
		delete(d.set, old)
	}
	return true
}
