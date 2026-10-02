// Package exposurekit is shared plumbing for this repo's EXPOSURE connectors
// — plugins that declare a verb with the `exposes` semantic
// (conductor docs/design/plugin-contract.md §2.3: `{local, url, lease,
// release}`). conductor itself ships two vendor-neutral builtins on this
// semantic, `lan` and `tunnel` (internal/builtins/exposure in conductor);
// every NAMED vendor — cloudflared, ngrok, localxpose, an ssh reverse
// tunnel, tailscale funnel — is a plugin here, and they share:
//
//   - Leases: the open/close bookkeeping every exposure verb needs (an
//     opaque lease id per open call, released individually or in bulk when
//     an instance stops) — the same shape as the tunnel builtin's own
//     `leases map[string]*lease`.
//   - OpenVerb/CloseVerb: the Verb declarations every exposure plugin
//     repeats verbatim (only Desc and Options vary).
//   - RunAndScan: spawn argv, scan its combined stdout+stderr for the first
//     line matching a regexp, return the match with a kill-on-release stop
//     func. Ported from conductor's pre-contract-cutover
//     internal/handoff/tunnel.go (runTunnel) — ONE copy, since cloudflared,
//     localxpose and the ssh reverse-tunnel plugin are all "run a command,
//     read the URL off its output" with only the argv template and the URL
//     pattern differing.
//   - PortOf/ExpandArgv: the `{{.port}}`/`{{.addr}}` template conventions
//     every spawning provider offers, ported from the same file.
package exposurekit

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// Exposes is the verb-semantics value every exposure plugin's "open" verb
// declares: {local: "local_addr", url: "public_url", lease: "lease",
// release: <release>} — the same field names the tunnel builtin uses, so a
// config switching `use: tunnel` for `use: cloudflared` (say) needs no other
// change. release is the verb name that ends the exposure ("close" for a
// dedicated exposure plugin; something else when the exposes verb lives
// alongside other verbs on a multi-purpose connector, e.g. tailscale's
// funnel_close).
func Exposes(release string) *plugin.VerbSemantics {
	return &plugin.VerbSemantics{
		HostOnly: true,
		Exposes:  &plugin.Exposes{Local: "local_addr", URL: "public_url", Lease: "lease", Release: release},
	}
}

// OpenVerb is the "open" verb every exposure plugin declares: takes
// local_addr, returns public_url + lease, and names "close" as its release
// verb. desc is the vendor-specific one line ("start a Cloudflare
// quick/named tunnel to a local address").
func OpenVerb(desc string) plugin.Verb {
	return plugin.Verb{
		Name: "open", Desc: desc, Semantics: Exposes("close"),
		Options: plugin.Schema{"local_addr": {Type: "string", Required: true, Desc: "host:port to expose, e.g. 127.0.0.1:8099"}},
		Outputs: plugin.Schema{
			"public_url": {Type: "string", Required: true},
			"lease":      {Type: "string", Required: true},
		},
	}
}

// CloseVerb is the "close" verb every exposure plugin declares: ends one
// lease opened by "open".
func CloseVerb() plugin.Verb {
	return plugin.Verb{
		Name: "close", Desc: "end an exposure opened by open", Semantics: &plugin.VerbSemantics{HostOnly: true},
		Options: plugin.Schema{"lease": {Type: "string", Required: true}},
	}
}

// Leases tracks open exposures, keyed by an opaque lease id, so "close" and
// plugin.stop (an instance going away) can tear them down. Safe for
// concurrent use.
type Leases struct {
	mu sync.Mutex
	m  map[string]*entry
}

type entry struct {
	instance string
	stop     func()
}

// NewLeases builds an empty lease table.
func NewLeases() *Leases { return &Leases{m: map[string]*entry{}} }

// Add records a new lease for instance, whose release runs stop, and
// returns its opaque id (an output of "open").
func (l *Leases) Add(instance string, stop func()) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	l.mu.Lock()
	l.m[id] = &entry{instance: instance, stop: stop}
	l.mu.Unlock()
	return id
}

// Release ends one lease (the "close" verb). A second call, or an unknown
// id, is a no-op — close must be idempotent.
func (l *Leases) Release(id string) {
	l.mu.Lock()
	e := l.m[id]
	delete(l.m, id)
	l.mu.Unlock()
	if e != nil {
		e.stop()
	}
}

// StopInstance releases every lease the named instance holds (plugin.stop:
// the instance is being removed or reloaded).
func (l *Leases) StopInstance(instance string) {
	l.mu.Lock()
	var ids []string
	for id, e := range l.m {
		if e.instance == instance {
			ids = append(ids, id)
		}
	}
	l.mu.Unlock()
	for _, id := range ids {
		l.Release(id)
	}
}

// Len reports how many leases are open (tests, status).
func (l *Leases) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// DefaultURL matches the first http(s) URL in a line — the fallback pattern
// for a provider with no tighter vendor-specific regexp.
var DefaultURL = regexp.MustCompile(`https?://\S+`)

// PortOf pulls the port out of a "host:port" (or ":port") address — every
// spawning provider forwards to a local port.
func PortOf(addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", fmt.Errorf("local_addr %q is not host:port", addr)
	}
	return port, nil
}

// ExpandArgv substitutes {{.port}} and {{.addr}} in every argument of tmpl.
func ExpandArgv(tmpl []string, port, addr string) []string {
	r := strings.NewReplacer("{{.port}}", port, "{{.addr}}", addr)
	out := make([]string, len(tmpl))
	for i, a := range tmpl {
		out[i] = r.Replace(a)
	}
	return out
}

// RunAndScan starts argv, scans its combined stdout+stderr line by line for
// the first match of urlRe, and returns that match as the public URL
// together with a stop func that kills the process (idempotent — safe to
// call more than once). It errors if the binary isn't on PATH, or if no URL
// is seen within timeout. onLine, when non-nil, receives every scanned line
// (for a plugin's own logging); it may be nil.
//
// Ported from conductor's pre-contract-cutover internal/handoff/tunnel.go
// (runTunnel) — same behavior: the process is started against its own
// background context so it outlives this call (it IS the tunnel) until stop
// is called; the passed-in timeout only bounds the wait for the first URL.
func RunAndScan(argv []string, urlRe *regexp.Regexp, timeout time.Duration, onLine func(string)) (publicURL string, stop func(), err error) {
	if len(argv) == 0 {
		return "", nil, fmt.Errorf("empty command")
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return "", nil, fmt.Errorf("%s not found on PATH (install it): %w", argv[0], err)
	}

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, argv[0], argv[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, fmt.Errorf("start %s: %w", argv[0], err)
	}

	var closeOnce sync.Once
	stopFn := func() {
		closeOnce.Do(func() {
			cancel()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		})
	}

	found := make(chan string, 1)
	scan := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		buf := make([]byte, 0, 64*1024)
		sc.Buffer(buf, 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if onLine != nil {
				onLine(line)
			}
			if m := urlRe.FindString(line); m != "" {
				select {
				case found <- m:
				default:
				}
			}
		}
	}
	go scan(stdout)
	go scan(stderr)

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case url := <-found:
		return url, stopFn, nil
	case <-timer.C:
		stopFn()
		return "", nil, fmt.Errorf("%s: no URL detected within %s", argv[0], timeout)
	}
}

// RunOnce runs argv to completion (bounded by timeout) and returns its
// combined output — for a vendor CLI's own short-lived control commands
// (bring a mapping up, query status, tear it down), as opposed to
// RunAndScan's long-lived "stays running until stop" subprocess.
func RunOnce(ctx context.Context, argv []string, timeout time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
