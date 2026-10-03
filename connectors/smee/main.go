// Command conductor-smee is an EXPOSURE connector (conductor
// docs/design/plugin-contract.md §2.3, the `exposes` verb semantic) for
// smee.io-style relay channels. Unlike cloudflared/ngrok/localxpose/ssh —
// which forward real inbound traffic to a local port — a smee channel is
// reached the other way around: the public side (GitHub, GitLab, …) POSTs to
// the channel URL, and THIS machine makes an outbound, long-lived SSE
// connection to receive each forwarded delivery. So "open" here means:
// create (or reuse) a channel, and start relaying every delivery it receives
// as an HTTP POST to the local address — from a consumer's point of view
// (the generic `listeners` semantic, §2.4) this is indistinguishable from a
// real tunnel: traffic aimed at the returned public_url arrives at
// local_addr.
//
// Connection:
//
//	channel:    a pinned smee channel URL, e.g. https://smee.io/AbC123
//	            (optional — a fresh channel is created when absent)
//	smee_base:  override https://smee.io (a self-hosted smee server, or tests)
//	path:       the local HTTP path deliveries are replayed to (default "/")
//	persist:    remember a CREATED channel across plugin restarts via
//	            host.state, so a webhook already registered against it on
//	            the sender's side keeps working (default true; irrelevant
//	            when channel is pinned)
//
// Every lease starts its own relay goroutine (no subprocess — this plugin
// never spawns); close (or conductor stopping the instance) stops it.
// stdout is the RPC transport; all plugin logging goes to stderr.
//
// KNOWN LIMITATION (documented, not a contract gap): the `listeners`
// semantic only carries a listen ADDRESS (host:port), not the HTTP path the
// consuming plugin's listener expects — fine for a byte-level tunnel
// (cloudflared et al. forward the whole origin, path included), but this
// plugin must reconstruct an HTTP request, so the operator sets `path` to
// match the consumer's own webhook path when it isn't "/". A second,
// inherent smee.io limitation (pre-existing, not introduced here): smee.io
// re-serializes JSON bodies, so an HMAC computed over the relayed bytes may
// not match one computed over the sender's original bytes — the same
// caveat this repo's other smee-consuming plugins already document for
// their own `webhook.smee` fields.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/exposurekit"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

const defaultSmeeBase = "https://smee.io"

type smeePlugin struct {
	leases *exposurekit.Leases
	client *http.Client
	// host is the channel back to the daemon for host.state (SetHost); nil
	// on a host too old to offer it — persist then degrades to "always
	// create a fresh channel", never an error.
	mu   sync.Mutex
	host *plugin.HostConn
	// ReconnectBackoff is the relay loop's initial reconnect delay (a var
	// for tests; production default 1s, doubling to 30s as the builtin
	// tunnel/cloudflared's own backoff loops do).
	ReconnectBackoff time.Duration
}

func newSmeePlugin() *smeePlugin {
	return &smeePlugin{
		leases:           exposurekit.NewLeases(),
		client:           &http.Client{Timeout: 15 * time.Second},
		ReconnectBackoff: time.Second,
	}
}

// SetHost implements plugin.HostAware.
func (p *smeePlugin) SetHost(h *plugin.HostConn) {
	p.mu.Lock()
	p.host = h
	p.mu.Unlock()
}

func (p *smeePlugin) getHost() *plugin.HostConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.host
}

func (p *smeePlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "smee",
		Desc: "Expose a local address through a smee.io-style relay channel: an outbound SSE connection receives deliveries, replayed locally as HTTP POSTs — for reaching a webhook listener with no inbound port to forward to.",
		Connection: plugin.Schema{
			"channel":   {Type: "string", Desc: "a pinned smee channel URL, e.g. https://smee.io/AbC123 (default: create a fresh one)"},
			"smee_base": {Type: "string", Desc: "override https://smee.io (a self-hosted smee server, or tests)"},
			"path":      {Type: "string", Desc: "local HTTP path deliveries are replayed to (default /)"},
			"persist":   {Type: "boolean", Desc: "remember a created channel across restarts via host.state (default true)"},
		},
		Verbs: []plugin.Verb{
			exposurekit.OpenVerb("create or reuse a smee channel and relay its deliveries to a local address"),
			exposurekit.CloseVerb(),
		},
		Capabilities: plugin.Capabilities{Egress: []string{"smee.io:443"}},
	}
}

func (p *smeePlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	switch req.Verb {
	case "open":
		return p.open(req)
	case "close":
		id, _ := req.Options["lease"].(string)
		if id == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "lease is required")
		}
		p.leases.Release(id)
		return plugin.InvokeResult{}, nil
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "smee verbs are open and close")
	}
}

func (p *smeePlugin) Stop(_ context.Context, req plugin.StopRequest) error {
	p.leases.StopInstance(req.Instance)
	return nil
}

const stateChannelKey = "channel"

func (p *smeePlugin) open(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	localAddr, _ := req.Options["local_addr"].(string)
	if _, err := exposurekit.PortOf(localAddr); err != nil {
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, "smee: "+err.Error(), nil)
	}
	base := strOr(str(req.Connection["smee_base"]), defaultSmeeBase)
	path := strOr(str(req.Connection["path"]), "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	persist := boolOr(req.Connection["persist"], true)
	ctx := context.Background()

	channel := str(req.Connection["channel"])
	if channel == "" && persist {
		if h := p.getHost(); h != nil {
			if v, err := h.State(req.Instance).Get(ctx, stateChannelKey); err == nil {
				if s, ok := v.(string); ok && s != "" {
					channel = s
				}
			}
		}
	}
	created := false
	if channel == "" {
		c, err := createChannel(p.client, base)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Fail(plugin.CodeUpstream, "smee: create channel: "+err.Error(), nil)
		}
		channel, created = c, true
	}
	if created && persist {
		if h := p.getHost(); h != nil {
			_ = h.State(req.Instance).Put(ctx, stateChannelKey, channel, "")
		}
	}

	target := "http://" + localAddr + path
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay(rctx, p.client, channel, target, p.ReconnectBackoff)
	}()
	stop := func() {
		cancel()
		<-done
	}
	lease, err := p.leases.AddCapped(req.Instance, stop, exposurekit.DefaultMaxLeasesPerInstance)
	if err != nil {
		stop()
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, "smee: "+err.Error(), nil)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"public_url": channel, "lease": lease}}, nil
}

// createChannel asks smee.io (or a compatible self-hosted server) for a
// fresh channel: GET <base>/new redirects to the new channel's own URL.
func createChannel(client *http.Client, base string) (string, error) {
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Get(strings.TrimRight(base, "/") + "/new")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("GET %s/new: unexpected status %d", base, resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("GET %s/new: no Location header", base)
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("parse Location %q: %w", loc, err)
	}
	if u.IsAbs() {
		return u.String(), nil
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	return baseURL.ResolveReference(u).String(), nil
}

// relay connects to channel and replays every forwarded delivery to target
// as an HTTP POST, reconnecting with capped exponential backoff until ctx is
// cancelled. Mirrors conductor's pre-contract-cutover
// internal/inbound/smee.go (Smee/streamSmee/parseSmeeFrame), generalized to
// replay over HTTP instead of handing frames to an in-process callback.
func relay(ctx context.Context, client *http.Client, channel, target string, backoff time.Duration) {
	if backoff <= 0 {
		backoff = time.Second
	}
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		err := relayOnce(ctx, client, channel, target)
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintf(os.Stderr, "smee: relay stream for %s ended (%v); reconnecting in %s\n", channel, err, backoff)
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

func relayOnce(ctx context.Context, client *http.Client, channel, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, channel, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	// This request IS the long-lived stream; it must not inherit client's
	// short per-call timeout.
	streamClient := *client
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("connect: HTTP %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				if h, body, ok := parseSmeeFrame(data.String()); ok {
					replayFrame(client, target, h, body)
				}
				data.Reset()
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// event:/id:/retry: lines and comments carry no delivery.
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("stream closed")
}

// parseSmeeFrame turns one SSE data payload into (headers, body). smee
// forwards the original request as a JSON object whose scalar top-level
// keys are the headers (host, content-type, x-*) plus a `body` field
// carrying the (re-serialized) payload; `query` is dropped (there is nowhere
// to put it on a POST replay — see the path/query KNOWN LIMITATION above
// this file's package doc). Control frames (smee's own "ready"/ping
// messages) carry no body and are skipped. Ported from conductor's
// pre-contract-cutover internal/inbound/smee.go:parseSmeeFrame.
func parseSmeeFrame(data string) (map[string]string, []byte, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, nil, false
	}
	rawBody, ok := raw["body"]
	if !ok {
		return nil, nil, false
	}
	body := []byte(rawBody)
	var asString string
	if json.Unmarshal(rawBody, &asString) == nil {
		body = []byte(asString)
	}
	headers := map[string]string{}
	for k, v := range raw {
		if k == "body" || k == "query" {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			headers[strings.ToLower(k)] = s
		}
	}
	return headers, body, true
}

// replayFrame POSTs one relayed delivery to target, carrying its original
// headers. Best-effort: a failed replay is logged, not fatal — the relay
// loop keeps running for the next delivery.
func replayFrame(client *http.Client, target string, headers map[string]string, body []byte) {
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "smee: build replay request for %s: %v\n", target, err)
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smee: replay to %s: %v\n", target, err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func main() {
	p := newSmeePlugin()
	err := plugin.Serve(p)
	p.leases.ReleaseAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "conductor-smee:", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) ---

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func boolOr(v any, def bool) bool {
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}
