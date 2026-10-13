// Package ghsourcetest is the github source CONFORMANCE suite: one table of
// cases — a connection, triggers, a mock GitHub API, webhook deliveries, a
// sweep — each with the triggers it must fire, and a runner that plays them
// against any implementation behind a Starter.
//
// It exists so parity is a test, not a claim. Conductor runs the table
// against its bundled github connector AND against the conductor-github
// plugin driven through the real daemon path (spawn, describe, start_source
// with triggers, routed events, the trust grant); the plugin repository runs
// it against its own released build over the bare wire (WireStarter). An
// implementation that fires a different trigger, misses one, fires an extra
// one, or carries different facts fails the same assertion in every repo.
//
// Every delivery goes through the implementation's real webhook listener as a
// signed HTTP POST, and the sweep through its real REST/GraphQL reads against
// the mock — nothing is fed to internals.
package ghsourcetest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Secret is the webhook secret every case signs its deliveries with.
const Secret = "conformance-secret"

// Env is what one case run is wired to.
type Env struct {
	// API is the mock GitHub API base URL (the connection's api_base).
	API string
	// Listen is the address the implementation's webhook listener must bind.
	Listen string
	// Path is the webhook path.
	Path string
}

// Trigger is one configured trigger, in the operator's own terms.
type Trigger struct {
	// On is the event (`on: gh.<On>`).
	On   string
	Name string
	// Filter is the `filter:` in its surface form — a string, a map, or a
	// list — exactly as YAML would decode it. nil for none.
	Filter  any
	Options map[string]any
}

// Route is one mock GitHub API response. The first route whose method, path
// (query ignored) and body substring match answers the request.
type Route struct {
	Method  string
	Path    string
	BodyHas string
	Status  int
	Body    any
}

// Step is one thing the case does: a webhook delivery, or a sweep nudge.
type Step struct {
	Event string // X-GitHub-Event
	Body  string
	// Sweep nudges the implementation's catch-up sweep instead of
	// delivering.
	Sweep bool
}

// Want is one trigger the case must fire.
type Want struct {
	Kind    string
	Trigger string // the trigger's name ("" for _closed, which no trigger is on)
	Repo    string
	Number  int
	CatchUp bool
	// Context holds facts the trigger must carry (compared as strings, so a
	// JSON round trip's float64 equals an int).
	Context map[string]any
	// Len holds context lists' required lengths.
	Len map[string]int
	// Absent are context facts the trigger must NOT carry.
	Absent []string
}

// Case is one conformance case.
type Case struct {
	Name       string
	Connection func(Env) map[string]any
	Triggers   []Trigger
	Routes     []Route
	Steps      []Step
	Want       []Want
}

// Got is one trigger an implementation fired, as the runner compares it.
type Got struct {
	Kind          string
	Trigger       string
	Repo          string
	Number        int
	CatchUp       bool
	TargetTrusted bool
	Context       map[string]any
}

// Driver is one running implementation.
type Driver interface {
	// Nudge asks the running source for a catch-up sweep now.
	Nudge(t *testing.T)
	// Events returns every trigger fired so far.
	Events() []Got
	// Close stops the source.
	Close()
}

// Starter starts an implementation for one case: its connection (with env
// filled in) and its triggers. It returns once the source is starting; the
// runner waits for the webhook listener itself.
type Starter func(t *testing.T, c Case, env Env) Driver

// Run plays every case against start.
func Run(t *testing.T, start Starter) {
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) { RunCase(t, c, start) })
	}
}

// RunCase plays one case.
func RunCase(t *testing.T, c Case, start Starter) { RunCaseExpecting(t, c, c.Want, start) }

// RunCaseExpecting plays one case's steps against start and compares what
// fired with want instead of the case's own expectations — for running a
// case under a configuration that should change the outcome.
func RunCaseExpecting(t *testing.T, c Case, want []Want, start Starter) {
	api := mockAPI(t, c.Routes)
	env := Env{API: api.URL, Listen: freeAddr(t), Path: "/webhook"}
	d := start(t, c, env)
	defer d.Close()
	waitListener(t, env)
	for i, s := range c.Steps {
		if s.Sweep {
			d.Nudge(t)
			continue
		}
		deliver(t, env, fmt.Sprintf("%s-%d", strings.ReplaceAll(c.Name, " ", "-"), i), s.Event, s.Body)
	}
	got := waitFor(d, len(want))
	compare(t, want, got)
}

// waitFor waits for want triggers, then a grace period for any extra one.
func waitFor(d Driver, want int) []Got {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(d.Events()) < want {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	return d.Events()
}

func compare(t *testing.T, want []Want, got []Got) {
	t.Helper()
	for _, p := range Diff(want, got) {
		t.Error(p)
	}
}

// Diff reports how got differs from want: every wanted trigger not fired
// (each Want consumes one matching Got), and every fired trigger nothing
// wanted. Empty means they agree.
func Diff(want []Want, got []Got) []string {
	var problems []string
	used := make([]bool, len(got))
	for _, w := range want {
		found := -1
		for i, g := range got {
			if !used[i] && matches(w, g) {
				found = i
				break
			}
		}
		if found < 0 {
			problems = append(problems, fmt.Sprintf("missing trigger %s (trigger %q) on %s#%d catch_up=%v context⊇%v len=%v absent=%v\n  got: %s",
				w.Kind, w.Trigger, w.Repo, w.Number, w.CatchUp, w.Context, w.Len, w.Absent, describe(got)))
			continue
		}
		used[found] = true
	}
	for i, g := range got {
		if !used[i] {
			problems = append(problems, fmt.Sprintf("unexpected trigger %s (trigger %q) on %s#%d catch_up=%v", g.Kind, g.Trigger, g.Repo, g.Number, g.CatchUp))
		}
	}
	return problems
}

func matches(w Want, g Got) bool {
	if w.Kind != g.Kind || w.Trigger != g.Trigger || w.Repo != g.Repo || w.Number != g.Number || w.CatchUp != g.CatchUp {
		return false
	}
	// Every trigger a GitHub source fires concerns a target GitHub assigned.
	if !g.TargetTrusted {
		return false
	}
	for k, v := range w.Context {
		if fmt.Sprint(g.Context[k]) != fmt.Sprint(v) {
			return false
		}
	}
	for k, n := range w.Len {
		l, ok := g.Context[k].([]any)
		if !ok || len(l) != n {
			return false
		}
	}
	for _, k := range w.Absent {
		if _, ok := g.Context[k]; ok {
			return false
		}
	}
	return true
}

func describe(got []Got) string {
	parts := make([]string, 0, len(got))
	for _, g := range got {
		parts = append(parts, fmt.Sprintf("%s(%q %s#%d catch_up=%v trusted=%v)", g.Kind, g.Trigger, g.Repo, g.Number, g.CatchUp, g.TargetTrusted))
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}

// --- transport ---------------------------------------------------------------

// Sign is the X-Hub-Signature-256 header for body under Secret.
func Sign(body []byte) string {
	m := hmac.New(sha256.New, []byte(Secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func deliver(t *testing.T, env Env, id, event, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+env.Listen+env.Path, strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-Hub-Signature-256", Sign([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("deliver %s: %v", event, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("deliver %s: HTTP %d", event, resp.StatusCode)
	}
}

func waitListener(t *testing.T, env Env) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+env.Listen+env.Path, strings.NewReader("{}"))
		req.Header.Set("X-GitHub-Event", "ping")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("webhook listener on %s never came up", env.Listen)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// mockAPI serves the case's routes; an unrouted request is a 404 (the source
// treats it as "can't read", which every case's expectations account for).
func mockAPI(t *testing.T, routes []Route) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		for _, rt := range routes {
			if rt.Method != r.Method || rt.Path != r.URL.Path || (rt.BodyHas != "" && !bytes.Contains(body, []byte(rt.BodyHas))) {
				continue
			}
			w.Header().Set("Content-Type", "application/json")
			if rt.Status != 0 {
				w.WriteHeader(rt.Status)
			}
			_ = json.NewEncoder(w).Encode(rt.Body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}
