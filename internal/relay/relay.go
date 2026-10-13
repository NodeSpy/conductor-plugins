// Package relay is the smee-style SSE relay client the plugins here use to
// receive webhooks at an endpoint with no public URL. It left conductor's SDK
// (pkg/sourcekit) when reaching a listener from outside became an exposure
// connector's job (conductor docs/design/plugin-contract.md §2.3); the
// plugins that offer a `relay:` / `smee_url:` setting keep it here, behind a
// Listener with the shape they were built on.
package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// Listener is sourcekit.Listener plus an optional smee-style relay feeding the
// same handler.
type Listener struct {
	Addr      string // listen address (empty = relay-only)
	Path      string
	Secret    string
	SigHeader string
	MaxBytes  int64
	Relay     string // optional smee-style SSE relay URL
}

// Serve is ServeReq with only headers and body.
func (l Listener) Serve(ctx context.Context, h func(http.Header, []byte)) error {
	return l.ServeReq(ctx, func(r *sourcekit.Request) { h(r.Header, r.Body) })
}

// ServeReq runs the HTTP listener (when Addr is set) and the relay (when Relay
// is set), both feeding h, with the same signature check. Blocks until ctx
// ends.
func (l Listener) ServeReq(ctx context.Context, h func(*sourcekit.Request)) error {
	if l.Addr == "" && l.Relay == "" {
		return errors.New("relay: Listener needs Addr or Relay")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	if l.Relay != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runRelay(ctx, l.Relay, func(r *sourcekit.Request) {
				if l.Secret == "" || sourcekit.VerifyHMAC(l.Secret, r.Body, r.Header.Get(l.SigHeader)) {
					h(r)
				}
			})
		}()
	}
	var err error
	if l.Addr != "" {
		err = sourcekit.Listener{Addr: l.Addr, Path: l.Path, Secret: l.Secret, SigHeader: l.SigHeader, MaxBytes: l.MaxBytes}.ServeReq(ctx, h)
	} else {
		<-ctx.Done()
	}
	cancel()
	wg.Wait()
	return err
}

// --- smee.io-style SSE relay client (stdlib) ---

// runRelay connects to a smee-style relay channel and feeds every forwarded
// request into dispatch, reconnecting with backoff until ctx is cancelled.
func runRelay(ctx context.Context, relayURL string, dispatch func(*sourcekit.Request)) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		_ = relayOnce(ctx, relayURL, dispatch)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// relayOnce holds one relay connection open, parsing the SSE stream into
// forwarded-request payloads until the connection drops or ctx cancels.
func relayOnce(ctx context.Context, relayURL string, dispatch func(*sourcekit.Request)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, relayURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("sourcekit: relay stream status " + resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var data strings.Builder
	flush := func() {
		if data.Len() == 0 {
			return
		}
		payload := data.String()
		data.Reset()
		if r, ok := parseRelayPayload([]byte(payload)); ok {
			dispatch(r)
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			flush()
		default:
			// event:, id:, retry:, and comments (":...") carry no delivery
			// payload — the "ready"/keep-alive events a relay sends land here
			// and are silently ignored.
		}
	}
	flush()
	return scanner.Err()
}

// parseRelayPayload decodes one smee-forwarded `data:` JSON envelope into a
// Request. smee delivers the original request as headers at the top level, plus
// body/query/host/timestamp; body may arrive as a nested JSON object or as a raw
// string, and query as an object. Returns ok=false for an envelope with no body
// (a relay's own ready/keep-alive events).
func parseRelayPayload(raw []byte) (*sourcekit.Request, bool) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false
	}
	bodyVal, present := payload["body"]
	if !present {
		return nil, false
	}
	var body []byte
	switch b := bodyVal.(type) {
	case string:
		body = []byte(b)
	case nil:
		return nil, false
	default:
		marshaled, err := json.Marshal(b)
		if err != nil {
			return nil, false
		}
		body = marshaled
	}

	// Every top-level string value is a forwarded request header (smee preserves
	// the original casing). body/query are structural, not headers.
	header := http.Header{}
	for k, v := range payload {
		if strings.EqualFold(k, "body") || strings.EqualFold(k, "query") {
			continue
		}
		if s, ok := v.(string); ok {
			header.Set(k, s)
		}
	}

	query := url.Values{}
	if q, ok := payload["query"].(map[string]any); ok {
		for k, v := range q {
			switch vv := v.(type) {
			case string:
				query.Set(k, vv)
			case []any:
				for _, item := range vv {
					if s, ok := item.(string); ok {
						query.Add(k, s)
					}
				}
			}
		}
	}

	return &sourcekit.Request{Header: header, Query: query, Body: body}, true
}

// Dedup is a bounded set of recently-seen delivery keys, so a redelivered
// webhook doesn't emit twice. Safe for concurrent use.
