// Command conductor-ngrok is an EXPOSURE connector (conductor
// docs/design/plugin-contract.md §2.3, the `exposes` verb semantic): it
// spawns `ngrok http` to make a local address reachable from outside, and
// reads the assigned public URL off ngrok's own local API
// (http://127.0.0.1:4040/api/tunnels by default) rather than scanning its
// TUI output, which isn't line-scannable.
//
// Connection:
//
//	binary:        override the ngrok binary path (default "ngrok")
//	authtoken:     passed as --authtoken; otherwise ngrok uses its own configured default
//	domain:        a reserved/static ngrok domain, passed as --domain
//	api_addr:      ngrok's local API address (default 127.0.0.1:4040); also
//	               passed to ngrok as --web-addr, so concurrent leases can
//	               each run their own ngrok agent on their own api_addr
//	extra_args:    raw flags inserted before the target address
//	start_timeout: how long to wait for ngrok's API to report the tunnel (default 30s)
//
// Every lease spawns its own ngrok process; close (or conductor stopping the
// instance) kills it. stdout is the RPC transport; all plugin logging goes
// to stderr.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/exposurekit"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

const (
	defaultAPIAddr      = "127.0.0.1:4040"
	defaultStartTimeout = 30 * time.Second
	pollInterval        = 300 * time.Millisecond
)

type ngrokPlugin struct {
	leases *exposurekit.Leases
}

func newNgrokPlugin() *ngrokPlugin {
	return &ngrokPlugin{leases: exposurekit.NewLeases()}
}

func (p *ngrokPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "ngrok",
		Desc: "Expose a local address through ngrok: the public URL is read off ngrok's own local API, not scanned from its TUI output.",
		Connection: plugin.Schema{
			"binary":        {Type: "string", Desc: "override the ngrok binary path (default ngrok)"},
			"authtoken":     {Type: "string", Desc: "passed as --authtoken; otherwise ngrok uses its own configured default"},
			"domain":        {Type: "string", Desc: "a reserved/static ngrok domain, passed as --domain"},
			"api_addr":      {Type: "string", Desc: "ngrok's local API address (default 127.0.0.1:4040); also passed to ngrok as --web-addr"},
			"extra_args":    {Type: "list", Desc: "raw flags inserted before the target address"},
			"start_timeout": {Type: "duration", Desc: "how long to wait for ngrok's API to report the tunnel (default 30s)"},
		},
		Verbs: []plugin.Verb{
			exposurekit.OpenVerb("start an ngrok tunnel to a local address"),
			exposurekit.CloseVerb(),
		},
		Capabilities: plugin.Capabilities{Commands: []string{"ngrok"}, Spawns: true},
	}
}

func (p *ngrokPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "ngrok verbs are open and close")
	}
}

func (p *ngrokPlugin) Stop(_ context.Context, req plugin.StopRequest) error {
	p.leases.StopInstance(req.Instance)
	return nil
}

func (p *ngrokPlugin) open(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	localAddr, _ := req.Options["local_addr"].(string)
	// local_addr is a per-call option, not an admin-set connection field —
	// reconstruct it rather than trust the raw string verbatim, and reject
	// anything shaped to look like a flag once it lands in argv below.
	safeAddr, err := exposurekit.SafeHostPort(localAddr)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, "ngrok: "+err.Error(), nil)
	}
	binary := strOr(str(req.Connection["binary"]), "ngrok")
	apiAddr := strOr(str(req.Connection["api_addr"]), defaultAPIAddr)
	timeout := durationOr(req.Connection["start_timeout"], defaultStartTimeout)

	argv := []string{binary, "http", "--web-addr", apiAddr}
	if tok := str(req.Connection["authtoken"]); tok != "" {
		argv = append(argv, "--authtoken", tok)
	}
	if dom := str(req.Connection["domain"]); dom != "" {
		argv = append(argv, "--domain", dom)
	}
	argv = append(argv, strList(req.Connection["extra_args"])...)
	// "--" ends flag parsing, so even a safeAddr that somehow still looked
	// flag-shaped would be read as the positional target, not a flag.
	argv = append(argv, "--", safeAddr)

	if _, err := exec.LookPath(argv[0]); err != nil {
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeUpstream, fmt.Sprintf("ngrok: %s not found on PATH (install it): %v", argv[0], err), nil)
	}

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, argv[0], argv[1:]...)
	exposurekit.SetNewProcessGroup(cmd)
	// Read via explicit pipes (not cmd.Stdout/Stderr writers): ngrok may
	// leave grandchildren holding the pipe's write end open, and Cmd.Wait
	// with a plain io.Writer blocks until EOF on that pipe — i.e. until
	// every holder of the fd exits, not just ngrok itself. StdoutPipe /
	// StderrPipe make Wait only wait for ngrok's own exit, so stop() (kill +
	// Wait) returns promptly instead of hanging for the process's whole
	// lifetime.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeUpstream, fmt.Sprintf("ngrok: %v", err), nil)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeUpstream, fmt.Sprintf("ngrok: %v", err), nil)
	}
	var buf bytes.Buffer
	var bufMu sync.Mutex
	capture := func(r io.Reader) {
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			if n > 0 {
				bufMu.Lock()
				buf.Write(b[:n])
				bufMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeUpstream, fmt.Sprintf("ngrok: start: %v", err), nil)
	}
	go capture(stdout)
	go capture(stderr)
	// Always reap in the background, not only when stop() is called: ngrok
	// exiting on its own (crash, killed by something else) would otherwise
	// leave a zombie until close/plugin.stop happens to run cmd.Wait() —
	// which, for a tunnel nobody explicitly closes, may be never.
	var reaper exposurekit.ProcessReaper
	reaped := make(chan struct{})
	go func() {
		reaper.Wait(func() { _ = cmd.Wait() })
		close(reaped)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			// Skip the signal if ngrok already exited on its own — see
			// ProcessReaper: signaling a pid the reaper already observed as
			// gone risks hitting a reused pid's unrelated process group.
			reaper.KillIfRunning(func() { exposurekit.KillProcessGroup(cmd) })
			<-reaped
		})
	}

	url, pollErr := pollNgrokAPI(context.Background(), apiAddr, timeout)
	if pollErr != nil {
		stop() // kill + wait FIRST so the process stops writing buf before we read it.
		bufMu.Lock()
		output := strings.TrimSpace(buf.String())
		bufMu.Unlock()
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeUpstream,
			fmt.Sprintf("ngrok: %v (output so far: %s)", pollErr, output), nil)
	}
	lease, err := p.leases.AddCapped(req.Instance, stop, exposurekit.DefaultMaxLeasesPerInstance)
	if err != nil {
		stop()
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, "ngrok: "+err.Error(), nil)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"public_url": url, "lease": lease}}, nil
}

// pollNgrokAPI retries ngrok's local API until it reports a tunnel or
// timeout elapses — the API isn't up the instant the process starts. Ported
// from conductor's pre-contract-cutover internal/handoff/tunnel.go.
func pollNgrokAPI(ctx context.Context, apiAddr string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		if url, err := fetchNgrokTunnelURL(client, "http://"+apiAddr+"/api/tunnels"); err == nil && url != "" {
			return url, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no tunnel URL from local API within %s", timeout)
		}
		time.Sleep(pollInterval)
	}
}

func fetchNgrokTunnelURL(client *http.Client, apiURL string) (string, error) {
	resp, err := client.Get(apiURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return parseNgrokTunnelsResponse(body)
}

// parseNgrokTunnelsResponse extracts the first tunnel's public_url from
// ngrok's `GET /api/tunnels` JSON body. Ported verbatim from conductor's
// pre-contract-cutover internal/handoff/tunnel.go.
func parseNgrokTunnelsResponse(body []byte) (string, error) {
	var out struct {
		Tunnels []struct {
			PublicURL string `json:"public_url"`
		} `json:"tunnels"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode ngrok tunnels response: %w", err)
	}
	for _, tun := range out.Tunnels {
		if tun.PublicURL != "" {
			return tun.PublicURL, nil
		}
	}
	return "", fmt.Errorf("no tunnels reported yet")
}

func main() {
	p := newNgrokPlugin()
	err := plugin.Serve(p)
	p.leases.ReleaseAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "conductor-ngrok:", err)
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

func strList(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return x
	}
	return nil
}

func durationOr(v any, def time.Duration) time.Duration {
	s, _ := v.(string)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
