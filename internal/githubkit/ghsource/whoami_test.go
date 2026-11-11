package ghsource

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// userStub serves GET /user as the given login (the write-identity whoami).
func userStub(t *testing.T, login string) *appAuth {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"login":%q}`, login)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	return &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}
}

// TestDiscoverSelfFromWriteIdentity: with no me: and a literal write token, self is
// auto-discovered from GET /user.
func TestDiscoverSelfFromWriteIdentity(t *testing.T) {
	g := newTestIntegration(t, Config{
		App:      AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Identity: Identity{WriteToken: "literal-write-tok"}, // literal → skips gh auth token
	})
	g.app = userStub(t, "Octocat")

	g.discoverSelf(context.Background())

	if !g.isSelf("octocat") || !g.isSelf("OctoCat") {
		t.Fatalf("self should be auto-discovered as octocat, got %v", *g.self.Load())
	}
}

// TestDiscoverSelfSkippedWhenMeSet: an explicit me: wins; no whoami, no override.
func TestDiscoverSelfSkippedWhenMeSet(t *testing.T) {
	g := newTestIntegration(t, Config{
		App:      AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Identity: Identity{WriteToken: "literal-write-tok"},
		Defaults: Rule{Me: Actors{Logins: []string{"danielcbaldwin"}}},
	})
	// A stub that would return a DIFFERENT login — must not be consulted.
	g.app = userStub(t, "someone-else")

	g.discoverSelf(context.Background())

	if !g.isSelf("danielcbaldwin") || g.isSelf("someone-else") {
		t.Fatalf("explicit me: must win over auto-discovery, got %v", *g.self.Load())
	}
}

// TestDiscoverSelfGracefulOnError: a whoami failure leaves self empty (prior
// behavior) rather than erroring.
func TestDiscoverSelfGracefulOnError(t *testing.T) {
	g := newTestIntegration(t, Config{
		App:      AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Identity: Identity{WriteToken: "literal-write-tok"},
	})
	// Point at a server that 401s /user.
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	g.app = &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}

	if err := g.discoverSelf(context.Background()); err == nil {
		t.Fatal("a failed whoami must report an error so Start retries it")
	}
	if g.hasSelf() {
		t.Fatalf("a failed whoami should leave self empty, got %v", *g.self.Load())
	}
}

// TestDiscoverSelfLoopRecovers: a whoami that fails at Start (network down while
// the plugin restarted) is retried in the background until it succeeds; success
// nudges a catch-up sweep. Before the retry, `self` stayed empty for the life of
// the process and no review request ever matched.
func TestDiscoverSelfLoopRecovers(t *testing.T) {
	oldMin, oldMax := selfRetryMin, selfRetryMax
	selfRetryMin, selfRetryMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { selfRetryMin, selfRetryMax = oldMin, oldMax })

	g := newTestIntegration(t, Config{
		App:      AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Identity: Identity{WriteToken: "literal-write-tok"},
	})
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 3 { // the outage
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"login":"Octocat"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	g.app = &appAuth{appID: 1, key: key, httpc: http.DefaultClient, apiBase: srv.URL, now: time.Now, cache: map[int64]cachedToken{}}

	if err := g.discoverSelf(context.Background()); err == nil {
		t.Fatal("first attempt should fail during the outage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { g.discoverSelfLoop(ctx); close(done) }()
	// Concurrent readers, as the webhook/sweep goroutines are (run under -race).
	go func() {
		for ctx.Err() == nil && !g.hasSelf() {
			g.isSelf("octocat")
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("discoverSelfLoop never recovered")
	}
	if !g.isSelf("octocat") {
		t.Fatalf("self should be discovered after the outage, got %v", *g.self.Load())
	}
	select {
	case <-g.renew:
	default:
		t.Fatal("recovery should nudge a catch-up sweep")
	}
}

// TestDiscoverSelfLoopStopsOnCancel: a permanently failing discovery doesn't
// leak the retry goroutine past the source's lifetime. PATH holds no `gh`, so
// the credential chain fails at once and never reaches the host's own gh
// login (or the network).
func TestDiscoverSelfLoopStopsOnCancel(t *testing.T) {
	oldMin := selfRetryMin
	selfRetryMin = time.Millisecond
	t.Cleanup(func() { selfRetryMin = oldMin })
	t.Setenv("PATH", t.TempDir())
	g := newTestIntegration(t, Config{Identity: Identity{WriteToken: "literal-write-tok"}}) // no credentials at all
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.discoverSelfLoop(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("discoverSelfLoop ignored cancellation")
	}
	if g.hasSelf() {
		t.Fatal("self must stay empty without credentials")
	}
}

// TestDiscoverSelfLoopStopsOnCancelMidAttempt: cancellation also ends an
// attempt in flight. Before, `gh auth token` ran without the context, so a
// slow or hung gh (a locked keyring, a loaded host) held the loop past
// cancel for as long as gh took.
func TestDiscoverSelfLoopStopsOnCancelMidAttempt(t *testing.T) {
	oldMin := selfRetryMin
	selfRetryMin = time.Millisecond
	t.Cleanup(func() { selfRetryMin = oldMin })
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n: > "+started+"\nexec "+sleep+" 600\n"), 0o755)
	t.Setenv("PATH", dir)
	g := newTestIntegration(t, Config{Identity: Identity{WriteToken: "literal-write-tok"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { g.discoverSelfLoop(ctx); close(done) }()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatal("discoverSelfLoop returned before cancel")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("discoverSelfLoop waited on an in-flight gh past cancel")
	}
}
