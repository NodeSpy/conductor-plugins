package ghsource

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
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

	if !g.self["octocat"] { // lowercased
		t.Fatalf("self should be auto-discovered as octocat, got %v", g.self)
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

	if !g.self["danielcbaldwin"] || g.self["someone-else"] {
		t.Fatalf("explicit me: must win over auto-discovery, got %v", g.self)
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

	g.discoverSelf(context.Background()) // must not panic
	if len(g.self) != 0 {
		t.Fatalf("a failed whoami should leave self empty, got %v", g.self)
	}
}
