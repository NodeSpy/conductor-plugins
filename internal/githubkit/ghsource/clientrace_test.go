package ghsource

import (
	"sync"
	"testing"
)

// TestEnsureClientsConcurrentIsRaceFree covers the fix for the data race on
// g.app/g.rest: since the webhook-only lazy-start change, sweep() and
// stuckPass() each call ensureClients from their OWN goroutine (sweepLoop /
// fixedSweepLoop vs stuckLoop — see smee.go's Start), and events.go /
// reviewfold.go / force.go read g.rest/g.app from yet another goroutine (the
// webhook delivery path) without ever calling ensureClients themselves. This
// reproduces the reviewer's repro: many goroutines calling ensureClients
// concurrently, plus concurrent readers via the accessors every call site
// now goes through. Run with -race; the assertions are a secondary check
// that every caller converges on the one client actually built.
func TestEnsureClientsConcurrentIsRaceFree(t *testing.T) {
	cfg := baseConfig()
	cfg.App = AppConfig{}
	cfg.Token = "pat-123" // static auth — builds instantly, no file/network I/O
	g := newTestIntegration(t, cfg)

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = g.ensureClients()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: ensureClients: %v", i, err)
		}
	}

	app := g.appAuth()
	rest := g.restClient()
	if app == nil || rest == nil {
		t.Fatal("ensureClients succeeded on every goroutine, but appAuth()/restClient() returned nil")
	}

	// Concurrent readers (standing in for events.go/reviewfold.go/force.go/
	// sweep.go, each reading from their own goroutine) must all see the SAME
	// fully-built client — never a torn or stale pointer.
	var wg2 sync.WaitGroup
	for i := 0; i < n; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if a := g.appAuth(); a != app {
				t.Error("appAuth() returned a different pointer across goroutines")
			}
			if r := g.restClient(); r != rest {
				t.Error("restClient() returned a different pointer across goroutines")
			}
		}()
	}
	wg2.Wait()
}

// TestEnsureClientsRetriesAfterFailure covers the "still allow retry after a
// failure" half of the fix: a mutex (rather than a sync.Once, which latches
// a failure forever) must let a LATER call rebuild once credentials become
// available — concurrent failing attempts must not poison the instance.
func TestEnsureClientsRetriesAfterFailure(t *testing.T) {
	cfg := baseConfig()
	cfg.App = AppConfig{AppID: 1, PrivateKeyPath: "/nonexistent/does-not-exist.pem"}
	cfg.Token = ""
	g := newTestIntegration(t, cfg)

	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.ensureClients(); err == nil {
				t.Error("ensureClients: want an error (bad key path), got nil")
			}
		}()
	}
	wg.Wait()
	if g.appAuth() != nil {
		t.Fatal("appAuth() is non-nil after every build attempt failed")
	}

	// Credentials become available — a later call must retry, not stay
	// permanently failed.
	g.cfg.App = AppConfig{}
	g.cfg.Token = "pat-now-configured"
	if err := g.ensureClients(); err != nil {
		t.Fatalf("ensureClients after fixing credentials: %v", err)
	}
	if g.appAuth() == nil {
		t.Fatal("appAuth() is still nil after a successful retry")
	}
}
