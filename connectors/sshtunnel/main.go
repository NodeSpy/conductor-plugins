// Command conductor-sshtunnel is an EXPOSURE connector (conductor
// docs/design/plugin-contract.md §2.3, the `exposes` verb semantic): it
// opens an ssh remote-port-forward to a tunnelling host — localhost.run,
// serveo.net, pinggy, or any other service offering the same style of
// "ssh in, get a public URL back" — and reads the URL off the ssh session's
// banner.
//
// Most of these services (localhost.run, serveo.net, and any host offering
// the same convention) use the plain `-R 80:localhost:<port>` form and print
// the assigned URL once connected. Pinggy needs its own port/remote-forward
// form (`-p 443 -R 0:localhost:<port>`); it is auto-detected from the host
// name (`pinggy` or anything under `.pinggy.io`) or pinned with `preset`.
//
// Connection:
//
//	host:            the ssh tunnel host, e.g. localhost.run, serveo.net, a.pinggy.io (required)
//	preset:          "generic" | "pinggy" — overrides host-name auto-detection
//	user:            ssh user, if the host wants one other than its default
//	identity_file:   path to an ssh private key (-i)
//	port:            ssh connection port (default 22, or 443 for the pinggy preset)
//	remote_port:     the forwarded remote port for the generic preset (default 80); ignored for pinggy, which always requests 0 (assigned)
//	extra_args:      raw ssh flags inserted before the host
//	start_timeout:   how long to wait for the URL before failing (default 30s)
//
// Every lease opens its own ssh process; close (or conductor stopping the
// instance) kills it, tearing the forward down. stdout is the RPC transport;
// all plugin logging goes to stderr.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/exposurekit"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

const defaultStartTimeout = 30 * time.Second

type sshtunnelPlugin struct {
	leases *exposurekit.Leases
}

func newSSHTunnelPlugin() *sshtunnelPlugin {
	return &sshtunnelPlugin{leases: exposurekit.NewLeases()}
}

func (p *sshtunnelPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "sshtunnel",
		Desc: "Expose a local address through an ssh reverse tunnel to a tunnelling host (localhost.run, serveo.net, pinggy, or any host offering the same -R forward + printed-URL convention).",
		Connection: plugin.Schema{
			"host":          {Type: "string", Required: true, Desc: "the ssh tunnel host, e.g. localhost.run, serveo.net, a.pinggy.io"},
			"preset":        {Type: "string", Enum: []string{"generic", "pinggy"}, Desc: "overrides host-name auto-detection"},
			"user":          {Type: "string", Desc: "ssh user, if the host wants one other than its default"},
			"identity_file": {Type: "string", Desc: "path to an ssh private key (-i)"},
			"port":          {Type: "string", Desc: "ssh connection port (default 22, or 443 for the pinggy preset)"},
			"remote_port":   {Type: "string", Desc: "forwarded remote port for the generic preset (default 80); ignored for pinggy"},
			"extra_args":    {Type: "list", Desc: "raw ssh flags inserted before the host"},
			"start_timeout": {Type: "duration", Desc: "how long to wait for the URL before failing (default 30s)"},
		},
		Verbs: []plugin.Verb{
			exposurekit.OpenVerb("open an ssh reverse tunnel to a local address"),
			exposurekit.CloseVerb(),
		},
		Capabilities: plugin.Capabilities{Commands: []string{"ssh"}, Spawns: true},
	}
}

func (p *sshtunnelPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "sshtunnel verbs are open and close")
	}
}

func (p *sshtunnelPlugin) Stop(_ context.Context, req plugin.StopRequest) error {
	p.leases.StopInstance(req.Instance)
	return nil
}

func (p *sshtunnelPlugin) open(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	localAddr, _ := req.Options["local_addr"].(string)
	port, err := exposurekit.PortOf(localAddr)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, "sshtunnel: "+err.Error(), nil)
	}
	host := str(req.Connection["host"])
	if host == "" {
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, "sshtunnel: host is required", nil)
	}
	preset := strOr(str(req.Connection["preset"]), detectPreset(host))
	timeout := durationOr(req.Connection["start_timeout"], defaultStartTimeout)

	argv := sshArgv(sshArgs{
		preset:     preset,
		host:       host,
		user:       str(req.Connection["user"]),
		identity:   str(req.Connection["identity_file"]),
		port:       str(req.Connection["port"]),
		remotePort: str(req.Connection["remote_port"]),
		extra:      strList(req.Connection["extra_args"]),
		localPort:  port,
	})

	url, stop, err := exposurekit.RunAndScan(argv, exposurekit.DefaultURL, timeout, nil)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeUpstream, "sshtunnel: "+err.Error(), nil)
	}
	lease, err := p.leases.AddCapped(req.Instance, stop, exposurekit.DefaultMaxLeasesPerInstance)
	if err != nil {
		stop()
		return plugin.InvokeResult{}, plugin.Fail(plugin.CodeInvalid, "sshtunnel: "+err.Error(), nil)
	}
	return plugin.InvokeResult{Outputs: map[string]any{"public_url": url, "lease": lease}}, nil
}

// detectPreset chooses the pinggy preset for pinggy's own hosts, and generic
// for everything else (localhost.run, serveo.net, a private host offering
// the same -R convention).
func detectPreset(host string) string {
	h := strings.ToLower(host)
	if h == "pinggy" || h == "a.pinggy.io" || strings.HasSuffix(h, ".pinggy.io") {
		return "pinggy"
	}
	return "generic"
}

type sshArgs struct {
	preset, host, user, identity, port, remotePort string
	extra                                          []string
	localPort                                      string
}

// sshArgv builds the ssh invocation. Ported and generalized from
// conductor's pre-contract-cutover internal/handoff/tunnel.go:sshArgv —
// pinggy needs its own port/remote-forward form (-p 443, -R0:...);
// localhost.run/serveo.net (and any other host offering the same
// convention) use the common -R 80:localhost:<port> form. host/port/
// remote_port/identity/user/extra_args generalize what was hard-coded
// per-preset into connection fields.
func sshArgv(a sshArgs) []string {
	argv := []string{"ssh", "-o", "StrictHostKeyChecking=accept-new"}
	if a.identity != "" {
		argv = append(argv, "-i", a.identity)
	}
	switch a.preset {
	case "pinggy":
		port := strOr(a.port, "443")
		argv = append(argv, "-p", port, "-R", "0:localhost:"+a.localPort)
	default:
		if a.port != "" {
			argv = append(argv, "-p", a.port)
		}
		remote := strOr(a.remotePort, "80")
		argv = append(argv, "-R", remote+":localhost:"+a.localPort)
	}
	argv = append(argv, a.extra...)
	target := a.host
	if a.user != "" {
		target = a.user + "@" + a.host
	}
	return append(argv, target)
}

func main() {
	p := newSSHTunnelPlugin()
	err := plugin.Serve(p)
	p.leases.ReleaseAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "conductor-sshtunnel:", err)
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
