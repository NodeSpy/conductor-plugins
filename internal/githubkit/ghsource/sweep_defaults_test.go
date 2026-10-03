package ghsource

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The sweep's new defaults: on unless explicitly off, all installed repos when no
// `repos:` is given, and a cadence that depends on whether a webhook is configured.

func TestSweepIsEnabledDefault(t *testing.T) {
	if !(SweepConfig{}).IsEnabled() {
		t.Fatal("an unset sweep must default to enabled")
	}
	if !(SweepConfig{Enabled: boolp(true)}).IsEnabled() {
		t.Fatal("explicit true")
	}
	if (SweepConfig{Enabled: boolp(false)}).IsEnabled() {
		t.Fatal("explicit false must disable")
	}
}

func TestWebhookConfigured(t *testing.T) {
	if (WebhookConfig{}).Configured() {
		t.Fatal("an empty webhook is not configured")
	}
	if !(WebhookConfig{SmeeURL: "https://smee.io/x"}).Configured() {
		t.Fatal("smee_url means configured")
	}
	if !(WebhookConfig{Listen: "127.0.0.1:8787"}).Configured() {
		t.Fatal("listen means configured")
	}
}

func TestFixedSweepInterval(t *testing.T) {
	if iv := fixedSweepInterval(SweepConfig{}); iv != 2*time.Minute {
		t.Fatalf("default fixed interval = %s, want 2m", iv)
	}
	if iv := fixedSweepInterval(SweepConfig{MinInterval: dur(t, "5m")}); iv != 5*time.Minute {
		t.Fatalf("honored = %s, want 5m", iv)
	}
	if iv := fixedSweepInterval(SweepConfig{MinInterval: dur(t, "5s")}); iv != sweepFloor {
		t.Fatalf("below-floor should clamp to %s, got %s", sweepFloor, iv)
	}
}

// allInstalledStub serves the list-installations endpoint, one installation's repo
// list, its token, and a review-pending PR on the single returned repo.
func allInstalledStub(t *testing.T) *appAuth {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id":77}]`)
	})
	mux.HandleFunc("/app/installations/77/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":1,"repositories":[
			{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}}]}`)
	})
	mux.HandleFunc("/repos/acme/widget/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"number":7,"user":{"login":"teammate"},"head":{"sha":"h7","ref":"feat"},"base":{"ref":"main"},
			"html_url":"u7","requested_reviewers":[{"login":"me"}]}]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	return &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
}

func allInstalledConfig() Config {
	return Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Sweep:   SweepConfig{Enabled: boolp(true)}, // no Repos → all installed
		Rules: []Rule{{
			Match:    Match{Repos: []string{"acme/widget"}},
			Reviewer: Actors{Logins: []string{"me"}},
			Actions:  as1(map[string]Action{"review_requested": {}}),
		}},
	}
}

// TestSweepAllInstalledWhenReposEmpty: with no `repos:`, the sweep enumerates every
// App installation, lists its repos, and sweeps them — finding the review-pending PR.
func TestSweepAllInstalledWhenReposEmpty(t *testing.T) {
	g := newTestIntegration(t, allInstalledConfig())
	g.app = allInstalledStub(t)
	g.rest = newRESTClient(g.app)

	var got []Trigger
	if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "review_requested" || got[0].Target.Number != 7 {
		t.Fatalf("all-installed sweep should have found review_requested on #7, got %+v", got)
	}
}

// TestSweepAllInstalledApplessWarns: in App-less (static-token) mode there is no
// installation to enumerate, so an empty-repos sweep warns and emits nothing rather
// than erroring the daemon.
func TestSweepAllInstalledApplessWarns(t *testing.T) {
	g := newTestIntegration(t, Config{Token: "tok", Sweep: SweepConfig{Enabled: boolp(true)}})
	g.app = newStaticAuth("tok")
	g.rest = newRESTClient(g.app)

	var got []Trigger
	if err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) }); err != nil {
		t.Fatalf("App-less all-installed sweep must not error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("App-less sweep with no repos should emit nothing, got %+v", got)
	}
}

// TestStartNoWebhookUsesFixedSweep: with no webhook but the sweep enabled, Start
// does NOT reject "no event source" (the relaxed guard), launches the FIXED-cadence
// loop (not the adaptive one), and runs until the context is cancelled.
func TestStartNoWebhookUsesFixedSweep(t *testing.T) {
	var buf syncBuf
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	cfg := allInstalledConfig()
	cfg.Webhook = WebhookConfig{} // no webhook → fixed cadence, sweep is the source
	g := newTestIntegration(t, cfg)
	g.app = allInstalledStub(t)
	g.rest = newRESTClient(g.app)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx, func(context.Context, Trigger) {}) }()

	// Wait for the fixed loop to announce itself (it logs before the first sweep).
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(buf.String(), "sweep enabled — fixed") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("fixed-cadence sweep never started; log:\n%s", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("Start should return the context error on cancel, got %v", err)
	}
	if strings.Contains(buf.String(), "no event source") {
		t.Fatal("Start must not reject a no-webhook config when the sweep is enabled")
	}
	if strings.Contains(buf.String(), "adaptive") {
		t.Fatalf("no-webhook mode must use the fixed loop, not adaptive; log:\n%s", buf.String())
	}
}

// syncBuf is a goroutine-safe bytes.Buffer for capturing log output while a loop
// runs concurrently.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
