// Command conductor-cloudflared is an EXPOSURE connector (conductor
// docs/design/plugin-contract.md §2.3, the `exposes` verb semantic): it
// spawns `cloudflared tunnel` to make a local address reachable from
// outside, and reads the public URL off its output. It is the named-vendor
// counterpart to conductor's vendor-neutral `tunnel` builtin (which runs any
// tunnelling command) — this plugin bakes in cloudflared's own argv shapes,
// URL pattern and named-tunnel wiring so the operator doesn't have to.
//
// Two modes:
//
//   - quick (default): `cloudflared tunnel --url http://<addr>` — a free,
//     ephemeral *.trycloudflare.com hostname, no Cloudflare account needed.
//     The public URL is read off cloudflared's own banner.
//   - named: `cloudflared tunnel run --url http://<addr> <tunnel>` — a
//     persistent tunnel you created and DNS-routed ahead of time (Cloudflare
//     dashboard, or `cloudflared tunnel create`/`route dns`). The public
//     hostname is already fixed by that DNS route, so it is a connection
//     field (`hostname`) rather than parsed from output; the plugin waits
//     for cloudflared to report the connection registered before returning.
//
// Connection:
//
//	mode:             "quick" | "named" (default "quick")
//	binary:           override the cloudflared binary path (default "cloudflared")
//	tunnel:            tunnel name or UUID (named mode, required)
//	hostname:          the DNS-routed public hostname for that tunnel (named mode, required)
//	credentials_file:  path to the tunnel's credentials JSON (named mode, optional — cloudflared's own default search path is used otherwise)
//	config:            path to a cloudflared config.yml (optional, either mode)
//	extra_args:        raw flags inserted right after the `tunnel` subcommand
//	start_timeout:     how long to wait for the URL/connection before failing (default 30s)
//
// Every lease spawns its own cloudflared process; close (or conductor
// stopping the instance) kills it. stdout is the RPC transport; all plugin
// logging goes to stderr — cloudflared's own stdout/stderr lines are only
// scanned in-process, never echoed to this plugin's stdout.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/exposurekit"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// cloudflareURLRe matches cloudflared's quick-tunnel banner URL. Ported
// verbatim from conductor's pre-contract-cutover
// internal/handoff/tunnel.go:cloudflareURLRe.
var cloudflareURLRe = regexp.MustCompile(`https://[a-zA-Z0-9.-]+\.trycloudflare\.com\S*`)

// registeredRe matches cloudflared's "a connection came up" line for a named
// tunnel (cloudflared prints variations like "Registered tunnel connection"
// / "Connection <id> registered"); we don't need the URL out of it — the
// hostname is already known from config — only the confirmation that the
// tunnel is live.
var registeredRe = regexp.MustCompile(`(?i)registered tunnel connection|connection .* registered`)

const defaultStartTimeout = 30 * time.Second

type cloudflaredPlugin struct {
	leases *exposurekit.Leases
}

func newCloudflaredPlugin() *cloudflaredPlugin {
	return &cloudflaredPlugin{leases: exposurekit.NewLeases()}
}

func (p *cloudflaredPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "cloudflared",
		Desc: "Expose a local address through Cloudflare Tunnel (cloudflared): a free ephemeral *.trycloudflare.com quick tunnel, or a persistent named tunnel you've already DNS-routed.",
		Connection: plugin.Schema{
			"mode":             {Type: "string", Enum: []string{"quick", "named"}, Desc: "quick: ephemeral trycloudflare.com URL (default); named: a pre-created, DNS-routed tunnel"},
			"binary":           {Type: "string", Desc: "override the cloudflared binary path (default cloudflared)"},
			"tunnel":           {Type: "string", Desc: "named mode: the tunnel's name or UUID"},
			"hostname":         {Type: "string", Desc: "named mode: the public hostname already DNS-routed to this tunnel"},
			"credentials_file": {Type: "string", Desc: "named mode: path to the tunnel's credentials JSON (default: cloudflared's own search path)"},
			"config":           {Type: "string", Desc: "path to a cloudflared config.yml"},
			"extra_args":       {Type: "list", Desc: "raw flags inserted after the tunnel subcommand"},
			"start_timeout":    {Type: "duration", Desc: "how long to wait for the tunnel to come up (default 30s)"},
		},
		Verbs: []plugin.Verb{
			exposurekit.OpenVerb("start a Cloudflare quick or named tunnel to a local address"),
			exposurekit.CloseVerb(),
		},
		Capabilities: plugin.Capabilities{Commands: []string{"cloudflared"}, Spawns: true},
	}
}

func (p *cloudflaredPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "cloudflared verbs are open and close")
	}
}

// Stop releases every lease the instance holds (plugin.stop).
func (p *cloudflaredPlugin) Stop(_ context.Context, req plugin.StopRequest) error {
	p.leases.StopInstance(req.Instance)
	return nil
}

func (p *cloudflaredPlugin) open(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	localAddr, _ := req.Options["local_addr"].(string)
	host, port, err := net.SplitHostPort(localAddr)
	if err != nil || port == "" {
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, fmt.Sprintf("cloudflared: local_addr %q is not host:port", localAddr), nil)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, port)

	binary := strOr(str(req.Connection["binary"]), "cloudflared")
	mode := strOr(str(req.Connection["mode"]), "quick")
	extra := strList(req.Connection["extra_args"])
	timeout := durationOr(req.Connection["start_timeout"], defaultStartTimeout)

	argv := []string{binary, "tunnel"}
	argv = append(argv, extra...)
	if cfg := str(req.Connection["config"]); cfg != "" {
		argv = append(argv, "--config", cfg)
	}

	var url string
	var stop func()
	var runErr error

	switch mode {
	case "quick":
		argv = append(argv, "--url", "http://"+addr, "--no-autoupdate")
		url, stop, runErr = exposurekit.RunAndScan(argv, cloudflareURLRe, timeout, nil)
	case "named":
		tunnel := str(req.Connection["tunnel"])
		hostname := str(req.Connection["hostname"])
		if tunnel == "" || hostname == "" {
			return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, "cloudflared: named mode requires tunnel and hostname", nil)
		}
		if cred := str(req.Connection["credentials_file"]); cred != "" {
			argv = append(argv, "--cred-file", cred)
		}
		argv = append(argv, "run", "--url", "http://"+addr, tunnel)
		_, stop, runErr = exposurekit.RunAndScan(argv, registeredRe, timeout, nil)
		url = "https://" + hostname
	default:
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, fmt.Sprintf("cloudflared: mode must be quick or named, got %q", mode), nil)
	}
	if runErr != nil {
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeUpstream, "cloudflared: "+runErr.Error(), nil)
	}
	lease := p.leases.Add(req.Instance, stop)
	return plugin.InvokeResult{Outputs: map[string]any{"public_url": url, "lease": lease}}, nil
}

func main() {
	if err := plugin.Serve(newCloudflaredPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-cloudflared:", err)
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

// durationOr parses the duration connection field (a wire value coming in
// as a Go duration string, e.g. "45s") or returns def.
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
