package ghsource

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureClientsCredentialChain(t *testing.T) {
	// token: static auth.
	cfg := baseConfig()
	cfg.App = AppConfig{}
	cfg.Token = "pat-123"
	g := newTestIntegration(t, cfg)
	if err := g.ensureClients(); err != nil {
		t.Fatalf("token chain: %v", err)
	}
	tok, err := g.app.installationToken(context.Background(), 0)
	if err != nil || tok != "pat-123" {
		t.Fatalf("static auth token: %q %v", tok, err)
	}

	// gh CLI fallback.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/usr/bin/env bash\necho gh-tok\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg.Token = ""
	g2 := newTestIntegration(t, cfg)
	if err := g2.ensureClients(); err != nil {
		t.Fatalf("gh fallback: %v", err)
	}
	tok, _ = g2.app.installationToken(context.Background(), 0)
	if tok != "gh-tok" {
		t.Fatalf("gh token: %q", tok)
	}

	// Empty gh output errors with the configure guidance.
	os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/usr/bin/env bash\necho\n"), 0o755)
	g3 := newTestIntegration(t, cfg)
	if err := g3.ensureClients(); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("empty gh token: %v", err)
	}
}

// freeTCPAddr picks a free "127.0.0.1:port" address by briefly binding :0 and
// releasing it — the standard small-race-window trick, fine for a single
// short-lived test server.
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// TestStartWebhookOnlyNoCredentialsNeeded covers fix 7: a source configured
// with ONLY a webhook listener and no app:/token: at all must still START —
// the direct HTTP listener binds and processes a signed delivery — even
// though nothing can mint a credential for it (PATH points at an empty
// directory, so even the gh-CLI fallback ensureClients tries last is
// unavailable). Before the fix, Start called ensureClients unconditionally
// and that failure aborted Start before the listener ever bound.
func TestStartWebhookOnlyNoCredentialsNeeded(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no `gh` binary reachable at all

	addr := freeTCPAddr(t)
	cfg := Config{
		Webhook: WebhookConfig{Listen: addr, Secret: "s3cr3t"},
		Sweep:   SweepConfig{Enabled: boolp(false)}, // isolate the listener; sweep's own laziness is covered elsewhere
		Rules: []Rule{{
			Match:    Match{Repos: []string{"acme/*"}},
			Reviewer: Actors{Logins: []string{"me"}},
			Actions:  as1(map[string]Action{"changes_requested": {}}),
		}},
	}
	g := newTestIntegration(t, cfg)

	var got []Trigger
	emit := func(_ context.Context, tr Trigger) { got = append(got, tr) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx, emit) }()

	url := "http://" + addr + "/webhook"
	body := changesRequestedBody()
	sig := sign("s3cr3t", []byte(body))

	// The listener binds asynchronously; retry the POST until it answers
	// (or the deadline below gives up) rather than racing a fixed sleep.
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	ok := false
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request_review")
		req.Header.Set("X-GitHub-Delivery", "d1")
		req.Header.Set("X-Hub-Signature-256", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("delivery: status %d, want %d", resp.StatusCode, http.StatusAccepted)
		}
		ok = true
		break
	}
	if !ok {
		t.Fatalf("listener never came up (no credentials should not have blocked it): %v", lastErr)
	}
	if len(got) != 1 || got[0].Kind != "changes_requested" {
		t.Fatalf("want 1 changes_requested from the signed delivery, got %+v", got)
	}
	if g.app != nil {
		t.Fatal("credentials must not have been resolved — nothing needed the API")
	}

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Start should return the context error on cancel, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop after cancel")
	}
}

// TestSweepResolvesCredentialsLazily covers the OTHER lazy-resolution point
// fix 7 adds: sweep (and stuckPass, the same guard) no longer assumes Start
// already called ensureClients — each calls it itself. A Source built here
// with g.app left nil (unlike every other sweep test in this package, which
// pre-wires g.app/g.rest by hand) proves sweep() resolves its own
// credentials rather than nil-pointer-panicking on g.rest.
func TestSweepResolvesCredentialsLazily(t *testing.T) {
	cfg := Config{Token: "pat-123", Sweep: SweepConfig{Enabled: boolp(true)}}
	g := newTestIntegration(t, cfg)
	if g.app != nil {
		t.Fatal("precondition: g.app must start nil for this test to mean anything")
	}
	var got []Trigger
	err := g.sweep(context.Background(), func(_ context.Context, tr Trigger) { got = append(got, tr) })
	if err != nil {
		t.Fatalf("sweep should resolve its own credentials (App-less static token) and succeed, got %v", err)
	}
	if g.app == nil {
		t.Fatal("sweep should have called ensureClients and set g.app")
	}
}
