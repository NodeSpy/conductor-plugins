package githubkit

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AppAuth mints GitHub App JWTs and caches per-installation access tokens. It
// also supports an App-less "static" mode: a fixed token (a PAT, or `gh auth
// token`) answers every installation-token request instead of minting one,
// for setups with no GitHub App configured.
type AppAuth struct {
	appID   int64
	key     *rsa.PrivateKey
	httpc   *http.Client
	apiBase string // overridable in tests via PC_GITHUB_API_BASE
	now     func() time.Time

	// static is the App-less mode's fixed token. When set, installation-id
	// lookups short-circuit to 0 (the sentinel InstallationToken answers with
	// the static token for).
	static string

	mu    sync.Mutex
	cache map[int64]cachedToken
}

type cachedToken struct {
	token string
	exp   time.Time
}

// NewStaticAppAuth builds the App-less auth: a personal setup with no GitHub
// App authenticates every read with one fixed token. Callers must not expand
// `owner/*` repo globs in this mode (that listing is an App endpoint).
func NewStaticAppAuth(token string) *AppAuth {
	return &AppAuth{
		static:  token,
		httpc:   &http.Client{Timeout: 20 * time.Second},
		apiBase: APIBaseURL(),
		now:     time.Now,
		cache:   map[int64]cachedToken{},
	}
}

// NewAppAuth builds GitHub App auth from an App ID and a private key file path
// (PEM, PKCS#1 or PKCS#8). A leading "~/" in the path is expanded to the user's
// home directory.
func NewAppAuth(appID int64, privateKeyPath string) (*AppAuth, error) {
	pem, err := os.ReadFile(expandHome(privateKeyPath))
	if err != nil {
		return nil, fmt.Errorf("read app key: %w", err)
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pem)
	if err != nil {
		return nil, fmt.Errorf("parse app key: %w", err)
	}
	return &AppAuth{
		appID:   appID,
		key:     key,
		httpc:   &http.Client{Timeout: 20 * time.Second},
		apiBase: APIBaseURL(),
		now:     time.Now,
		cache:   map[int64]cachedToken{},
	}, nil
}

// APIBaseURL is the GitHub REST API base. It defaults to the public API; a
// hermetic test harness may point conductor (or a plugin) at a mock API by
// setting PC_GITHUB_API_BASE. Unset — the production case — leaves behavior
// unchanged. Read dynamically (not cached) so tests may set it per-case.
func APIBaseURL() string {
	if v := os.Getenv("PC_GITHUB_API_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.github.com"
}

// expandHome expands a leading "~/" to the user's home directory.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

// appJWT builds a short-lived RS256 JWT identifying the App.
func (a *AppAuth) appJWT() (string, error) {
	now := a.now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.appID,
	})
	return tok.SignedString(a.key)
}

// RepoInstallationID resolves the App installation id covering a repository
// (used by the sweep, which has no webhook payload to read it from). In the
// App-less static-token mode there is no installation; 0 is the sentinel id
// InstallationToken answers with the static token.
func (a *AppAuth) RepoInstallationID(ctx context.Context, owner, repo string) (int64, error) {
	if a.static != "" {
		return 0, nil
	}
	return a.installationIDByURL(ctx, fmt.Sprintf("%s/repos/%s/%s/installation", a.apiBase, owner, repo))
}

// AccountInstallationID resolves the installation id for an org or user
// account (used to expand `owner/*` sweep globs). Tries the org endpoint, then
// user. Not available in static-token mode — glob expansion lists installation
// repos, an App endpoint — so App-less sweeps must name repos explicitly.
func (a *AppAuth) AccountInstallationID(ctx context.Context, account string) (int64, error) {
	if a.static != "" {
		return 0, fmt.Errorf("repo globs (%s/*) need a GitHub App; list repos explicitly when using token/gh credentials", account)
	}
	id, err := a.installationIDByURL(ctx, fmt.Sprintf("%s/orgs/%s/installation", a.apiBase, account))
	if err == nil {
		return id, nil
	}
	return a.installationIDByURL(ctx, fmt.Sprintf("%s/users/%s/installation", a.apiBase, account))
}

func (a *AppAuth) installationIDByURL(ctx context.Context, url string) (int64, error) {
	jwtStr, err := a.appJWT()
	if err != nil {
		return 0, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+jwtStr)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.httpc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("installation lookup %s: HTTP %d", url, resp.StatusCode)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// InstallationToken returns a cached (or freshly minted) installation access
// token for the given installation id. In static-token (App-less) mode the
// fixed token answers every id.
func (a *AppAuth) InstallationToken(ctx context.Context, instID int64) (string, error) {
	if a.static != "" {
		return a.static, nil
	}
	a.mu.Lock()
	if c, ok := a.cache[instID]; ok && a.now().Before(c.exp.Add(-5*time.Minute)) {
		a.mu.Unlock()
		return c.token, nil
	}
	a.mu.Unlock()

	jwtStr, err := a.appJWT()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiBase, instID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+jwtStr)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := a.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("installation token: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	a.mu.Lock()
	a.cache[instID] = cachedToken{token: out.Token, exp: out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}

// TokenForRepo returns a (cached) installation access token for the
// installation covering owner/repo — the composite a `bot` identity verb call
// needs, without the caller having to resolve the installation id itself.
func (a *AppAuth) TokenForRepo(ctx context.Context, owner, repo string) (string, error) {
	id, err := a.RepoInstallationID(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return a.InstallationToken(ctx, id)
}
