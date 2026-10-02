// Command conductor-localxpose is an EXPOSURE connector (conductor
// docs/design/plugin-contract.md §2.3, the `exposes` verb semantic): it runs
// `loclx tunnel http` to make a local address reachable from outside at a
// *.loclx.io URL, and reads it off the CLI's own output.
//
// Connection:
//
//	binary:        override the loclx binary path (default "loclx")
//	extra_args:    raw flags inserted before the --to flag (e.g. a reserved
//	               subdomain, region, or access token your loclx config needs)
//	start_timeout: how long to wait for the URL before failing (default 30s)
//
// Every lease spawns its own loclx process; close (or conductor stopping the
// instance) kills it. stdout is the RPC transport; all plugin logging goes
// to stderr.
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

// loclxURLRe matches localxpose's tunnel URL. Ported verbatim from
// conductor's pre-contract-cutover internal/handoff/tunnel.go:loclxURLRe.
var loclxURLRe = regexp.MustCompile(`https://[a-zA-Z0-9.-]+\.loclx\.io\S*`)

const defaultStartTimeout = 30 * time.Second

type localxposePlugin struct {
	leases *exposurekit.Leases
}

func newLocalxposePlugin() *localxposePlugin {
	return &localxposePlugin{leases: exposurekit.NewLeases()}
}

func (p *localxposePlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "localxpose",
		Desc: "Expose a local address through LocalXpose (loclx): a *.loclx.io public URL, read off the CLI's own output.",
		Connection: plugin.Schema{
			"binary":        {Type: "string", Desc: "override the loclx binary path (default loclx)"},
			"extra_args":    {Type: "list", Desc: "raw flags inserted before --to, e.g. a reserved subdomain or access token"},
			"start_timeout": {Type: "duration", Desc: "how long to wait for the URL before failing (default 30s)"},
		},
		Verbs: []plugin.Verb{
			exposurekit.OpenVerb("start a LocalXpose tunnel to a local address"),
			exposurekit.CloseVerb(),
		},
		Capabilities: plugin.Capabilities{Commands: []string{"loclx"}, Spawns: true},
	}
}

func (p *localxposePlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "localxpose verbs are open and close")
	}
}

func (p *localxposePlugin) Stop(_ context.Context, req plugin.StopRequest) error {
	p.leases.StopInstance(req.Instance)
	return nil
}

func (p *localxposePlugin) open(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	localAddr, _ := req.Options["local_addr"].(string)
	host, port, err := net.SplitHostPort(localAddr)
	if err != nil || port == "" {
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, fmt.Sprintf("localxpose: local_addr %q is not host:port", localAddr), nil)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, port)

	binary := strOr(str(req.Connection["binary"]), "loclx")
	extra := strList(req.Connection["extra_args"])
	timeout := durationOr(req.Connection["start_timeout"], defaultStartTimeout)

	argv := append([]string{binary, "tunnel", "http"}, extra...)
	argv = append(argv, "--to", addr)

	url, stop, err := exposurekit.RunAndScan(argv, loclxURLRe, timeout, nil)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeUpstream, "localxpose: "+err.Error(), nil)
	}
	lease := p.leases.Add(req.Instance, stop)
	return plugin.InvokeResult{Outputs: map[string]any{"public_url": url, "lease": lease}}, nil
}

func main() {
	if err := plugin.Serve(newLocalxposePlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-localxpose:", err)
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
