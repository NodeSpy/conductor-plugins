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
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// DefaultMaxLeasesPerInstance bounds how many concurrent exposures a single
// instance may hold open. Without a cap, a misconfigured caller (or a bug
// that retries "open" without ever calling "close") can spawn an unbounded
// number of tunnel subprocesses.
const DefaultMaxLeasesPerInstance = 16

// fallbackGrace is how long RunAndScanPreferred waits, once a fallback-only
// match has been seen, for a preferred-pattern match to still arrive before
// giving up on it and returning the fallback. Short enough that a provider
// with no preferred match configured (or none ever printed) doesn't make
// every call eat the full timeout waiting on a pattern that was never going
// to show up.
const fallbackGrace = 500 * time.Millisecond

// ProcessReaper synchronizes a spawned process's background reap (the
// goroutine every long-running spawn here starts, so a process that exits on
// its own doesn't sit as a zombie until something happens to call stop) with
// that stop path's kill signal. Once Wait has observed the process already
// exited, KillIfRunning must not send a signal: the OS is free to reuse that
// pid (as a new, unrelated process's own process-group leader) the instant
// it is reaped, and a kill-by-pgid landing after that point can hit that
// unrelated process instead. The zero value is ready to use.
type ProcessReaper struct {
	mu     sync.Mutex
	exited bool
}

// Wait runs waitFn — expected to block on the spawned process's Wait, e.g.
// `func() { _ = cmd.Wait() }` — and then marks the process reaped. Call this
// from the background goroutine every spawn here already starts to avoid
// zombies.
func (r *ProcessReaper) Wait(waitFn func()) {
	waitFn()
	r.mu.Lock()
	r.exited = true
	r.mu.Unlock()
}

// KillIfRunning calls kill — expected to signal the process/group, e.g.
// `func() { KillProcessGroup(cmd) }` — unless Wait has already observed the
// process exit. The check and the call share Wait's lock, so the two methods
// can never interleave wrongly: either this call completes before Wait can
// record the exit, or Wait has already recorded it and the signal is skipped
// entirely — there is no window where a signal is sent after the exit was
// observed.
func (r *ProcessReaper) KillIfRunning(kill func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exited {
		return
	}
	kill()
}

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

// ExposesWithPath is Exposes plus `exposes.path` (conductor
// docs/design/plugin-contract.md §2.3): the engine passes a consumer
// listener's resolved HTTP path in the OPTION named pathOption, and uses
// this verb's returned URL as is — for a relay whose own public address
// already carries wherever a delivery should land (it replays locally to
// local_addr+path itself), as opposed to a byte-level tunnel (plain Exposes
// above) that forwards the whole origin and has the engine append the path
// to the URL it returns instead. The caller must also add an option named
// pathOption to its "open" Verb (OpenVerb does not include one).
func ExposesWithPath(release, pathOption string) *plugin.VerbSemantics {
	return &plugin.VerbSemantics{
		HostOnly: true,
		Exposes:  &plugin.Exposes{Local: "local_addr", URL: "public_url", Lease: "lease", Release: release, Path: pathOption},
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

// AddCapped is Add with a per-instance cap: if instance already holds max
// open leases, it returns an error instead of adding one (the caller is
// responsible for tearing down whatever it already started — the stub
// subprocess, the relay goroutine — before returning that error, since this
// call never takes ownership of stop). The check and the add happen under
// one lock, so concurrent opens for the same instance cannot both slip past
// the cap.
func (l *Leases) AddCapped(instance string, stop func(), max int) (string, error) {
	l.mu.Lock()
	n := 0
	for _, e := range l.m {
		if e.instance == instance {
			n++
		}
	}
	if n >= max {
		l.mu.Unlock()
		return "", fmt.Errorf("too many open leases for instance %q (max %d); close one before opening another", instance, max)
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	l.m[id] = &entry{instance: instance, stop: stop}
	l.mu.Unlock()
	return id, nil
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

// ReleaseAll releases every open lease, regardless of instance. The plugin
// contract (docs/design/plugin-contract.md §"stdin closes": the plugin kills
// its children when stdin closes) requires this on the way out: pkg/plugin's
// Serve returns plainly on a clean stdin EOF with no per-instance "stop"
// guaranteed to have run first, so a main that just exits after Serve leaves
// every still-open tunnel subprocess running, reparented to init. Every
// exposure plugin's main must call this after Serve returns, before the
// process exits.
func (l *Leases) ReleaseAll() {
	l.mu.Lock()
	ids := make([]string, 0, len(l.m))
	for id := range l.m {
		ids = append(ids, id)
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
// spawning provider forwards to a local port, often by splicing the port
// straight into a vendor CLI's argv (e.g. `tailscale serve --bg <port>`).
// The port must be a bare 1-65535 number and the host (when present) must
// not start with '-': either one landing in argv unchecked lets a crafted
// local_addr inject an extra flag into that argv instead of being read as an
// address.
func PortOf(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", fmt.Errorf("local_addr %q is not host:port", addr)
	}
	if strings.HasPrefix(host, "-") {
		return "", fmt.Errorf("local_addr %q: host must not start with '-'", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("local_addr %q: port must be a number from 1-65535", addr)
	}
	return port, nil
}

// SafeHostPort validates a caller-supplied "host:port" address meant to be
// spliced into a vendor CLI's argv as its own token — not concatenated into
// another flag's value — and returns it reconstructed via net.JoinHostPort
// rather than the original string verbatim, so an input net.SplitHostPort
// happened to accept but that doesn't round-trip identically can't smuggle
// anything through. The port must be numeric 1-65535; the host (when
// present) must not start with '-' — a leading dash is how an address
// masquerades as a flag to a CLI's own argument parser. An empty host
// defaults to 127.0.0.1.
func SafeHostPort(addr string) (string, error) {
	port, err := PortOf(addr)
	if err != nil {
		return "", err
	}
	host, _, _ := net.SplitHostPort(addr) // already validated by PortOf above
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
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

// OutputPipes wires cmd's stdout and stderr to pipes the caller owns, in
// place of StdoutPipe/StderrPipe. Wait closes a StdoutPipe's read end as soon
// as the process exits, which can discard a line the process wrote just
// before exiting — a tunnel that prints its URL and then exits loses the URL.
// Here the child gets plain *os.File write ends, so Wait waits only for the
// process itself (never for a grandchild still holding a write end) and
// never touches the read ends: the readers see EOF once every writer is
// gone. Call closeWriters right after Start (the parent's copies must not
// keep the pipes open), and close each reader when done with it.
func OutputPipes(cmd *exec.Cmd) (stdout, stderr *os.File, closeWriters func(), err error) {
	or, ow, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	er, ew, err := os.Pipe()
	if err != nil {
		or.Close()
		ow.Close()
		return nil, nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = ow, ew
	return or, er, func() { ow.Close(); ew.Close() }, nil
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
	SetNewProcessGroup(cmd)
	stdout, stderr, closeWriters, err := OutputPipes(cmd)
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		closeWriters()
		stdout.Close()
		stderr.Close()
		cancel()
		return "", nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	closeWriters()

	// Always reap in the background, not only when stop() is called: a
	// tunnel binary that crashes or exits on its own is otherwise left a
	// zombie until (if ever) close/plugin.stop runs cmd.Wait() for it.
	var reaper ProcessReaper
	reaped := make(chan struct{})
	go func() {
		reaper.Wait(func() { _ = cmd.Wait() })
		close(reaped)
	}()

	var closeOnce sync.Once
	stopFn := func() {
		closeOnce.Do(func() {
			// Kill the group BEFORE cancel(): cancel makes CommandContext
			// kill only the direct child, the reaper then sees it gone and
			// KillIfRunning would skip the group, leaving grandchildren.
			// Skip the signal if the process has already exited on its own —
			// see ProcessReaper: signaling a pid the reaper already observed
			// as gone risks hitting a reused pid's unrelated process group.
			reaper.KillIfRunning(func() { KillProcessGroup(cmd) })
			cancel()
			<-reaped // the background goroutine above owns the one Wait call
			// Unblock the scanners even if a grandchild outside the
			// process group still holds a write end.
			stdout.Close()
			stderr.Close()
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

// RunAndScanPreferred is a variant of RunAndScan for a provider whose own
// confirmation line can be preceded by unrelated output carrying an
// incidental URL — e.g. an ssh session's MOTD banner, printed before the
// tunnel host's own "forwarding to ..." line. Each scanned line is checked
// against preferred first, in order: a match returns immediately, since
// these are expected to be tight enough (a specific provider's own hostname
// suffix) that nothing but the real confirmation line could match. If no
// preferred pattern EVER matches within timeout, it falls back to the LAST
// line matching fallback rather than the first — a banner's own URL, if one
// appears at all, appears before the real line, not after.
func RunAndScanPreferred(argv []string, preferred []*regexp.Regexp, fallback *regexp.Regexp, timeout time.Duration, onLine func(string)) (publicURL string, stop func(), err error) {
	if len(argv) == 0 {
		return "", nil, fmt.Errorf("empty command")
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return "", nil, fmt.Errorf("%s not found on PATH (install it): %w", argv[0], err)
	}

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, argv[0], argv[1:]...)
	SetNewProcessGroup(cmd)
	stdout, stderr, closeWriters, err := OutputPipes(cmd)
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		closeWriters()
		stdout.Close()
		stderr.Close()
		cancel()
		return "", nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	closeWriters()

	var reaper ProcessReaper
	reaped := make(chan struct{})
	go func() {
		reaper.Wait(func() { _ = cmd.Wait() })
		close(reaped)
	}()
	var closeOnce sync.Once
	stopFn := func() {
		closeOnce.Do(func() {
			// Kill the group BEFORE cancel(): cancel makes CommandContext
			// kill only the direct child, the reaper then sees it gone and
			// KillIfRunning would skip the group, leaving grandchildren.
			// Skip the signal if the process has already exited on its own —
			// see ProcessReaper: signaling a pid the reaper already observed
			// as gone risks hitting a reused pid's unrelated process group.
			reaper.KillIfRunning(func() { KillProcessGroup(cmd) })
			cancel()
			<-reaped
			stdout.Close()
			stderr.Close()
		})
	}

	preferredFound := make(chan string, 1)
	fallbackSeen := make(chan struct{})
	var fallbackSeenOnce sync.Once
	var mu sync.Mutex
	var lastFallback string
	scan := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		buf := make([]byte, 0, 64*1024)
		sc.Buffer(buf, 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if onLine != nil {
				onLine(line)
			}
			matchedPreferred := false
			for _, re := range preferred {
				if m := re.FindString(line); m != "" {
					select {
					case preferredFound <- m:
					default:
					}
					matchedPreferred = true
					break
				}
			}
			if !matchedPreferred && fallback != nil {
				if m := fallback.FindString(line); m != "" {
					mu.Lock()
					lastFallback = m
					mu.Unlock()
					fallbackSeenOnce.Do(func() { close(fallbackSeen) })
				}
			}
		}
	}
	go scan(stdout)
	go scan(stderr)

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case url := <-preferredFound:
		return url, stopFn, nil
	case <-fallbackSeen:
		// A fallback match showed up. Rather than keep waiting for the FULL
		// timeout on a preferred pattern that may never arrive (or was never
		// configured to begin with), give it one short grace window to still
		// show up, then settle for the fallback.
		grace := time.NewTimer(fallbackGrace)
		defer grace.Stop()
		select {
		case url := <-preferredFound:
			return url, stopFn, nil
		case <-grace.C:
			mu.Lock()
			url := lastFallback
			mu.Unlock()
			return url, stopFn, nil
		case <-timer.C:
			mu.Lock()
			url := lastFallback
			mu.Unlock()
			return url, stopFn, nil
		}
	case <-timer.C:
		mu.Lock()
		url := lastFallback
		mu.Unlock()
		if url == "" {
			stopFn()
			return "", nil, fmt.Errorf("%s: no URL detected within %s", argv[0], timeout)
		}
		return url, stopFn, nil
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
