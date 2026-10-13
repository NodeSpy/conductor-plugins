// Package githubkit is the PUBLIC, daemon-agnostic GitHub client (#59): the
// reusable HTTP client (REST + GraphQL + raw uploads + response cache +
// rate-limit handling), GitHub App auth (JWT minting + installation token
// resolution, see auth.go), and the per-verb operations (Invoke, see
// invoke.go) that both the conductor daemon's bundled github connector and an
// external conductor-github plugin build on. It has no dependency on any
// conductor-internal package, so a plugin module can import it directly.
package githubkit

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// AppConfig is the GitHub App credentials a Client needs to mint installation
// tokens for the `bot` identity.
type AppConfig struct {
	AppID          int64
	PrivateKeyPath string
}

// Config configures a Client's credentials. The `me` identity resolves in this
// order: a literal WriteToken (when set to something other than "" or
// "gh_auth"), then GhToken (defaults to shelling out to `gh auth token`), then
// Token as a last-resort fallback. The `bot` identity requires App to be set.
type Config struct {
	// Token is the PAT fallback for `as: me` when WriteToken/gh CLI aren't
	// available.
	Token string
	// WriteToken is identity.write_token: a literal token, or "" / "gh_auth" to
	// fall through to GhToken then Token, or "app" for the App's token.
	WriteToken string
	// ReadToken is identity.read_token: "" / "app" for the App's installation
	// token (the static token when App-less), "gh_auth" for the write chain,
	// or a literal token. It is what the read credential mints.
	ReadToken string
	// GhToken resolves a token via `gh auth token` (or a test stub). Defaults
	// to shelling out to the real `gh` CLI.
	GhToken func() (string, error)
	// App, if non-nil (AppID > 0), enables the `bot` identity via GitHub App
	// installation tokens.
	App *AppConfig
	// HTTPClient is used for every API call (defaults to a 20s-timeout client).
	HTTPClient *http.Client
	// CacheTTL is how long a cached GET body is served without revalidating
	// (default 45s; see DefaultCacheTTL).
	CacheTTL time.Duration
	// APIBase overrides the GitHub REST/GraphQL API base URL (default: the
	// public API, or PC_GITHUB_API_BASE when set — see APIBaseURL). This is
	// for GitHub Enterprise Server, and for a caller (e.g. an external plugin
	// subprocess) that cannot rely on inheriting the parent process's
	// environment to reach a hermetic test double.
	APIBase string
}

// DefaultCacheTTL is how long a GET body is served without revalidating —
// long enough that a review fan-out (several reviewers/verifiers, same PR)
// hits the API once, short enough that a read after a write sees fresh data
// soon.
const DefaultCacheTTL = 45 * time.Second

// Client is a GitHub API client for the connector verb surface: it resolves
// credentials per `as: me|bot` call, issues authenticated REST/GraphQL/raw
// requests, and caches GET responses with ETag revalidation and rate-limit
// awareness.
type Client struct {
	token      string
	writeToken string
	readToken  string
	ghToken    func() (string, error)
	app        *AppAuth // nil when App-less (no `bot` identity)

	httpc *http.Client

	// apiBase overrides APIBaseURL() when non-empty (see Config.APIBase).
	apiBase string

	// GET response cache (reads only) + last-seen rate-limit state, so a
	// fan-out of reviewers/verifiers that all want the same diff/metadata hits
	// GitHub once and backs off gracefully near the limit. Guarded by mu.
	//
	// CacheTTL is exported so a caller may retune it after construction (e.g. a
	// test forcing revalidation on every read).
	CacheTTL time.Duration

	mu          sync.Mutex
	getCache    map[string]*cacheEntry
	logins      map[string]string // token key -> GET /user login (see Login)
	rlRemaining int               // X-RateLimit-Remaining from the last response (-1 = unknown)
	rlReset     time.Time
}

// cacheEntry is one cached GET body + its ETag (for cheap revalidation).
type cacheEntry struct {
	etag    string
	body    []byte
	fetched time.Time
}

// NewClient builds a Client from cfg. An App config with AppID > 0 requires a
// readable, parseable private key file (returns an error otherwise).
func NewClient(cfg Config) (*Client, error) {
	httpc := cfg.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: 20 * time.Second}
	}
	ghToken := cfg.GhToken
	if ghToken == nil {
		ghToken = GhAuthToken
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	c := &Client{
		token: cfg.Token, writeToken: cfg.WriteToken, readToken: cfg.ReadToken, ghToken: ghToken,
		httpc: httpc, CacheTTL: ttl, apiBase: cfg.APIBase,
		getCache: map[string]*cacheEntry{}, logins: map[string]string{}, rlRemaining: -1,
	}
	if cfg.App != nil && cfg.App.AppID > 0 {
		app, err := NewAppAuth(cfg.App.AppID, cfg.App.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("app credentials: %w", err)
		}
		if cfg.APIBase != "" {
			app.apiBase = cfg.APIBase
		}
		c.app = app
	}
	return c, nil
}

// base returns the GitHub API base URL this client uses: Config.APIBase when
// set, else the package default (APIBaseURL, which honors PC_GITHUB_API_BASE).
func (c *Client) base() string {
	if c.apiBase != "" {
		return c.apiBase
	}
	return APIBaseURL()
}

// GhAuthToken shells out to `gh auth token` — the last link of the
// write_token → gh → token credential chain.
func GhAuthToken() (string, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w", err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("gh auth token returned empty")
	}
	return tok, nil
}

// TokenFor resolves the identity a verb call acts as. `me` follows the
// configured write-token policy (gh auth token by default, a literal
// WriteToken otherwise, the PAT as a fallback when gh isn't available); `bot`
// requires App credentials and posts as the App's bot user for the given repo
// (owner/name form).
func (c *Client) TokenFor(ctx context.Context, as, repo string) (string, error) {
	switch as {
	case "", "me":
		wt := c.writeToken
		if wt != "" && wt != "gh_auth" {
			return wt, nil // literal token (already resolved by the caller)
		}
		tok, err := c.ghToken()
		if err == nil {
			return tok, nil
		}
		if c.token != "" {
			return c.token, nil
		}
		return "", fmt.Errorf("as: me — no write credential: %v (configure identity.write_token, token:, or log in with gh)", err)
	case "bot":
		if c.app == nil {
			return "", fmt.Errorf("as: bot needs GitHub App credentials (app:)")
		}
		owner, name, ok := strings.Cut(repo, "/")
		if !ok {
			return "", fmt.Errorf("as: bot needs a repo in owner/name form, got %q", repo)
		}
		return c.app.TokenForRepo(ctx, owner, name)
	}
	return "", fmt.Errorf("as: must be me|bot, got %q", as)
}

// MintCredential resolves one of the credentials the github connector declares
// for agents (plugin-contract.md §2.4): "read" by identity.read_token (default:
// the App's installation token for repo, the static token when App-less),
// "write" by identity.write_token (default: the `me` chain; "app" for the
// App's token).
func (c *Client) MintCredential(ctx context.Context, which, repo string) (string, error) {
	policy := c.writeToken
	if which == "read" {
		policy = c.readToken
		if policy == "" {
			policy = "app"
		}
	}
	switch policy {
	case "app":
		if c.app == nil {
			return c.TokenFor(ctx, "me", repo)
		}
		return c.TokenFor(ctx, "bot", repo)
	case "", "gh_auth":
		saved := c.writeToken
		if which == "read" && saved != "" && saved != "gh_auth" && saved != "app" {
			return saved, nil
		}
		return c.TokenFor(ctx, "me", repo)
	}
	return policy, nil // a literal token
}
