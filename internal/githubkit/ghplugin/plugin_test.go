package ghplugin

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// The declaration the plugin describes itself with IS the bundled one, and it
// opts into the source extension.
func TestDescribeIsTheSharedDecl(t *testing.T) {
	d := New().Describe()
	if !reflect.DeepEqual(d, Decl()) {
		t.Fatal("Describe must return Decl() itself")
	}
	if d.Type != "github" || d.Kind != plugin.KindConnector {
		t.Fatalf("type=%q kind=%q abi=%d", d.Type, d.Kind, d.ABI)
	}
	// The poll semantic names the engine's poll verb `sweep`, and the
	// declaration carries it (so `gh.sweep` keeps its name).
	if d.Semantics == nil || d.Semantics.Poll == nil || d.Semantics.Poll.VerbName != "sweep" {
		t.Fatalf("poll semantic = %+v", d.Semantics)
	}
	var sweep bool
	for _, v := range d.Verbs {
		sweep = sweep || v.Name == "sweep"
	}
	if !sweep {
		t.Fatal("the sweep (poll) verb must be declared")
	}
	for _, e := range d.Events {
		if e.Name == "new_comment" && (len(e.Facts) == 0 || len(e.MatchKeys) == 0) {
			t.Fatal("new_comment must declare its unified filter surface")
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// statusAPI accepts the commit-status write set_status makes, recording the
// credential each one carried.
func statusAPI(t *testing.T, auth *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/repos/o/r/statuses/") {
			mu.Lock()
			*auth = append(*auth, r.Header.Get("Authorization"))
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// THE OWN-STATUS GUARD, through the plugin. A status context set_status posts
// under is conductor's own from then on — the source never reads a delivery
// of it back as CI — whether the post lands before the instance's source has
// started (noted, then applied) or after (applied directly). This is the
// plugin half of the bundled connector's set_status → NoteOwnStatusContext.
func TestSetStatusMakesTheContextOwn(t *testing.T) {
	var auth []string
	api := statusAPI(t, &auth)
	conn := map[string]any{
		"token": "tok", "api_base": api.URL,
		// The write identity: verbs act as you with this, never the PAT.
		"identity": map[string]any{"write_token": "write-tok"},
		"me":       map[string]any{"logins": []any{"me"}},
		"webhook":  map[string]any{"listen": freeAddr(t), "secret": "s"},
		"sweep":    map[string]any{"enabled": false},
	}
	p := New()
	invoke := func(ctxName string) {
		t.Helper()
		_, err := p.Invoke(plugin.InvokeRequest{Instance: "gh", Verb: "set_status", Connection: conn,
			Options: map[string]any{"repo": "o/r", "sha": "abc", "state": "pending", "context": ctxName}})
		if err != nil {
			t.Fatalf("set_status %s: %v", ctxName, err)
		}
	}
	invoke("early / review") // before the source exists

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = p.StartSource(ctx, plugin.StartSourceRequest{Instance: "gh", Config: conn}, func(any) error { return nil })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := p.source("gh"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("source never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	invoke("late / review") // after
	src, _ := p.source("gh")
	for _, c := range []string{"early / review", "late / review", "ME"} {
		if !src.OwnStatus(c) {
			t.Errorf("%q must be conductor's own status context", c)
		}
	}
	if src.OwnStatus("ci / build") {
		t.Error("a context conductor never posted is CI, not its own")
	}
	for _, a := range auth {
		if !strings.HasSuffix(a, "write-tok") {
			t.Errorf("a write went out as %q, not identity.write_token", a)
		}
	}
	if len(auth) != 2 {
		t.Fatalf("want 2 status writes, got %d", len(auth))
	}
}

// A routed event carries the trigger id the source lowered into the Action,
// the target as the kit built it, and the claims — nothing reshaped.
func TestEventCarriesTheRoute(t *testing.T) {
	src, err := BuildSource("gh", map[string]any{
		"token": "t", "me": map[string]any{"logins": []any{"me"}},
		"webhook": map[string]any{"listen": "127.0.0.1:1", "secret": "s"}, "sweep": map[string]any{"enabled": false},
	}, []plugin.SourceTrigger{{ID: "0:gh.release", Name: "rel", Event: "release"}})
	if err != nil {
		t.Fatal(err)
	}
	trs := src.Translate(context.Background(), "release",
		[]byte(`{"action":"published","repository":{"full_name":"o/r","name":"r","owner":{"login":"o"}},"release":{"tag_name":"v1"}}`))
	if len(trs) != 1 {
		t.Fatalf("want one release trigger, got %d", len(trs))
	}
	ev := Event("gh", trs[0])
	if ev.Trigger != "0:gh.release" || ev.Event != "release" || ev.Kind != "release" || ev.Instance != "gh" ||
		ev.Target.Repo != "o/r" || !ev.TargetTrusted || ev.Context["tag_name"] != "v1" {
		t.Fatalf("event: %+v", ev)
	}
	raw, _ := json.Marshal(ev)
	if !strings.Contains(string(raw), `"trigger":"0:gh.release"`) {
		t.Fatalf("wire form: %s", raw)
	}
}

// A trigger on the PR close builds (it is a known kind) and takes the close,
// routed to it like any other event.
func TestATriggerOnTheCloseTakesIt(t *testing.T) {
	src, err := BuildSource("gh", map[string]any{
		"token": "t", "me": map[string]any{"logins": []any{"me"}},
		"webhook": map[string]any{"listen": "127.0.0.1:1", "secret": "s"}, "sweep": map[string]any{"enabled": false},
	}, []plugin.SourceTrigger{{ID: "0:gh._closed", Name: "merged", Event: "_closed"}})
	if err != nil {
		t.Fatal(err)
	}
	trs := src.Translate(context.Background(), "pull_request",
		[]byte(`{"action":"closed","repository":{"full_name":"o/r","name":"r","default_branch":"main","owner":{"login":"o"}},
		"pull_request":{"number":3,"merged":true,"user":{"login":"me"},"head":{"sha":"h","ref":"feat","repo":{"full_name":"o/r"}},"base":{"ref":"main"}}}`))
	if len(trs) != 1 {
		t.Fatalf("want one close, got %d", len(trs))
	}
	if ev := Event("gh", trs[0]); ev.Trigger != "0:gh._closed" || ev.Kind != "_closed" || ev.Context["mine"] != true || ev.Context["head_ref"] != "feat" {
		t.Fatalf("event: %+v", ev)
	}
}

// The connection parses the bundled connector's spellings, and refuses the
// retired app-block webhook keys the way it does.
func TestParseConnection(t *testing.T) {
	c, err := ParseConnection(map[string]any{
		"app":      map[string]any{"app_id": "12", "private_key_path": "/k.pem"},
		"sweep":    map[string]any{"interval": "1d2h", "min_interval": 90},
		"webhook":  map[string]any{"smee_url": "https://smee.example/x", "verify_signature": false},
		"identity": map[string]any{"write_token": "w"},
		"api_base": "https://ghe.example/api/v3",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := c.Source
	if s.App.AppID != 12 || s.Sweep.Interval != 26*time.Hour || s.Sweep.MinInterval != 90*time.Second ||
		s.Webhook.Verify() || s.APIBase != "https://ghe.example/api/v3" || s.Identity.WriteToken != "w" {
		t.Fatalf("source connection: %+v", s)
	}
	if c.Client.App == nil || c.Client.App.AppID != 12 || c.Client.WriteToken != "w" || c.Client.APIBase != s.APIBase {
		t.Fatalf("client config: %+v", c.Client)
	}
	if _, err := ParseConnection(map[string]any{"app": map[string]any{"webhook_secret": "x"}}); err != ErrAppWebhookMoved {
		t.Fatalf("retired key: %v", err)
	}
}

// The engine fills in webhook.public_url (the `listeners` connection
// semantic — plugin-contract.md §2.4) once it has opened webhook.expose;
// ParseConnection threads it straight to the source, which is what Start
// logs it from. webhook.expose itself is the engine's business, not read
// back out here.
func TestParseConnectionWebhookPublicURL(t *testing.T) {
	c, err := ParseConnection(map[string]any{
		"webhook": map[string]any{"listen": "127.0.0.1:0", "expose": "tun", "public_url": "https://hook.example/abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source.Webhook.PublicURL != "https://hook.example/abc" {
		t.Fatalf("webhook.public_url not threaded: %+v", c.Source.Webhook)
	}
}

// The declaration names the webhook listener's listen/expose/url_to fields
// as the `listeners` connection semantic, so the engine knows to open an
// exposure for it when webhook.expose is configured.
func TestDeclDeclaresListeners(t *testing.T) {
	sem := Decl().Semantics
	if sem == nil || len(sem.Listeners) != 1 {
		t.Fatalf("semantics.listeners = %+v, want exactly one", sem)
	}
	l := sem.Listeners[0]
	if l.Listen != "webhook.listen" || l.Expose != "webhook.expose" || l.URLTo != "webhook.public_url" || l.Path != "webhook.path" {
		t.Fatalf("listener = %+v", l)
	}
}

// While webhook.smee_url is accepted (deprecated, until the next major
// release), the plugin dials smee.io itself — and a conductor older than the
// declared jail does not resolve a config: egress reference. So smee.io stays
// declared literally beside config:webhook.smee_url; dropping it is how a box
// on such a conductor lost its webhook stream once already.
func TestDeprecatedSmeeKeepsLiteralEgress(t *testing.T) {
	eg := Decl().Capabilities.Egress
	has := func(want string) bool {
		for _, e := range eg {
			if e == want {
				return true
			}
		}
		return false
	}
	if !has("smee.io:443") || !has("config:webhook.smee_url") {
		t.Fatalf("egress %v: while smee_url is accepted, both smee.io:443 and config:webhook.smee_url stay declared", eg)
	}
}

// gh keeps its login in the OS keyring by default (gh auth login): the
// declaration names that item for github.com, for a GitHub Enterprise
// api_base's host and for GH_HOST, and nothing else, so a jailed gh reads it
// on Linux as on macOS. Keychain stays for conductors that do not grant
// keyring items.
func TestDeclaresGhKeyringLogin(t *testing.T) {
	c := Decl().Capabilities
	if err := c.Validate(Decl().Connection); err != nil {
		t.Fatal(err)
	}
	g, err := c.Resolve(plugin.ResolveEnv{
		Conns: []map[string]any{{"api_base": "https://ghe.example.test/api/v3"}, {}},
		Getenv: func(n string) (string, bool) {
			return map[string]string{"GH_HOST": "acme.ghe.com"}[n], n == "GH_HOST"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range g.Keyring {
		if len(it.Attributes) != 1 {
			t.Fatalf("item %+v: want service alone", it)
		}
		got = append(got, it.Attributes["service"])
	}
	sort.Strings(got)
	if want := []string{"gh:acme.ghe.com", "gh:ghe.example.test", "gh:github.com"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("resolved keyring services %v, want %v", got, want)
	}
	if !g.Keyring[0].Matches(map[string]string{"service": g.Keyring[0].Attributes["service"], "username": "someone"}) {
		t.Fatal("a declared item does not match gh's per-account item")
	}
	// No api_base and no GH_HOST: github.com's login alone.
	g, err = c.Resolve(plugin.ResolveEnv{Conns: []map[string]any{{}}, Getenv: func(string) (string, bool) { return "", false }})
	if err != nil || len(g.Keyring) != 1 || g.Keyring[0].Attributes["service"] != "gh:github.com" {
		t.Fatalf("default keyring %+v %v: want gh:github.com alone", g.Keyring, err)
	}
	if !c.Keychain {
		t.Fatal("keychain: true is dropped: conductors before keyring lose the macOS login")
	}
}
