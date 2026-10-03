// Package ghfake is a stateful, in-memory GitHub: REST, GraphQL, App auth,
// and webhook deliveries driven from one model, as close to GitHub's
// documented behavior as a test double can be.
//
// It is TEST SUPPORT, in pkg/ (like net/http/httptest) rather than under
// internal/ or test/, for one reason: the parity proof between conductor's
// bundled github connector and the conductor-github plugin runs in two
// repositories, and the plugin's repository can only import conductor's
// public packages. Nothing in conductor's product imports it.
//
// What it models, and how faithfully:
//
//   - Accounts (users, Bot accounts like `cursor[bot]`, orgs), an App with
//     installations, repos with branches/commits/protection, PRs (draft,
//     head/base refs and shas, mergeability COMPUTED from the model — a
//     conflict flag, the base moving, required checks and approvals), issues,
//     conversation comments, reviews with their inline comments and threads
//     (resolved / outdated), requested reviewers, commit statuses and the
//     combined status, check runs/suites, Actions workflow runs and jobs
//     (incl. re-runs), reactions on every subject kind, labels, releases,
//     contents, gists.
//   - REST: every endpoint conductor and the plugin call (Endpoints lists
//     them), with response bodies shaped by GitHub's own OpenAPI description
//     (schema.go), real errors (404 unknown resource, 422 invalid input with
//     GitHub's error body, 401 bad credentials, 403 the wrong KIND of
//     credential), Link-header pagination with per_page, ETag /
//     If-None-Match → 304, and rate-limit headers. An endpoint it does not
//     implement answers 501 and is recorded (Unhandled) — tests fail on it;
//     there is no benign default.
//   - GraphQL: the queries and mutations conductor sends, parsed and
//     resolved against the same model, validated against a vendored subset of
//     GitHub's public schema; an unknown field is a GraphQL error shaped like
//     GitHub's.
//   - Webhooks: model changes deliver real webhook payloads (shaped by the
//     OpenAPI webhook schemas) to registered hooks, HMAC-signed
//     (X-Hub-Signature-256) with an X-GitHub-Delivery GUID, with GitHub's
//     ordering quirks configurable: a submitted review's review and inline
//     comment deliveries in either or seeded-random order, duplicates, and
//     redeliveries.
//   - A scenario API (scenario.go; also over HTTP under /_fake/ for
//     out-of-process harnesses, admin.go) for acting as other people —
//     "this reviewer submits CHANGES_REQUESTED with 3 inline comments",
//     "CI fails check X", "the PR is merged" — and assertion helpers that
//     read the resulting MODEL STATE (statuses on which sha, reactions by
//     whom, comments posted, reviewers re-requested).
package ghfake

import (
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// ReviewOrder is the order a submitted review's deliveries go out in.
type ReviewOrder int

// Review delivery orders. GitHub sends the pull_request_review event and one
// pull_request_review_comment per inline comment in no fixed order.
const (
	ReviewFirst   ReviewOrder = iota // the review, then its comments
	CommentsFirst                    // the comments, then the review
	Shuffled                         // seeded-random (Options.Seed)
)

// Options tune the fake's GitHub-isms.
type Options struct {
	// ReviewOrder is the order of a submitted review's deliveries.
	ReviewOrder ReviewOrder
	// Seed seeds Shuffled ordering (and nothing else).
	Seed int64
	// DuplicateDeliveries delivers every webhook twice, with the SAME
	// X-GitHub-Delivery GUID — what a GitHub retry looks like.
	DuplicateDeliveries bool
	// MergeabilityDelay: the first read of a PR after its head or base moved
	// answers mergeable null / mergeable_state "unknown", as GitHub does while
	// it computes mergeability in the background.
	MergeabilityDelay bool
	// RateLimit is the per-token hourly budget reported in X-RateLimit-*
	// (default 5000); a token that exhausts it gets 403 "API rate limit
	// exceeded".
	RateLimit int
	// Logf logs (default log.Printf). Every unhandled request is logged.
	Logf func(string, ...any)
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// identity is who a credential acts as.
type identity struct {
	kind  string // user | installation | app
	user  *User  // user: the user; installation: the App's bot account
	inst  int64  // installation id
	token string
}

// App is the GitHub App (one per fake).
type App struct {
	ID        int64
	Slug      string
	Name      string
	Bot       *User
	PublicKey *rsa.PublicKey // nil: App JWTs are checked for shape and issuer only
}

// Installation is one App installation on an account.
type Installation struct {
	ID      int64
	Account *User
	Repos   []string // lowercase full names; empty = all of the account's repos
}

// Hook is one webhook endpoint (an App webhook, or a repository webhook when
// Repos is set).
type Hook struct {
	ID     int64
	URL    string
	Secret string
	Repos  []string // lowercase full names; empty = every repo
	Events []string // empty = every event
}

// Fake is the in-memory GitHub. Create with New; serve with Start (or use
// Handler); act with the scenario methods; assert with the query methods.
type Fake struct {
	mu   sync.Mutex
	opts Options
	seq  int64
	// shaSeq makes minted shas unique.
	shaSeq int

	users         map[string]*User
	repos         map[string]*Repo
	app           *App
	installations map[int64]*Installation
	tokens        map[string]*identity
	usage         map[string]int // per-token request count (rate limit)
	gists         map[string]*Gist

	hooks      []*Hook
	deliveries []*Delivery
	queue      chan *Delivery
	pending    sync.WaitGroup
	unhandled  []string

	srv     *httptest.Server
	baseURL string
	closed  bool
}

// New returns an empty fake.
func New(opts Options) *Fake {
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RateLimit == 0 {
		opts.RateLimit = 5000
	}
	f := &Fake{
		opts: opts, seq: 1000,
		users: map[string]*User{}, repos: map[string]*Repo{},
		installations: map[int64]*Installation{},
		tokens:        map[string]*identity{}, usage: map[string]int{},
		gists: map[string]*Gist{},
		queue: make(chan *Delivery, 4096),
	}
	go f.deliverLoop()
	return f
}

func (f *Fake) now() time.Time { return f.opts.Now().UTC().Truncate(time.Second) }

// Start serves the fake on a local test server and returns its base URL.
func (f *Fake) Start() string {
	f.srv = httptest.NewServer(f.Handler())
	f.mu.Lock()
	f.baseURL = f.srv.URL
	f.mu.Unlock()
	return f.srv.URL
}

// SetBaseURL sets the URL the fake writes into the `url` fields of its
// responses and payloads (Start does it; a harness serving Handler itself
// sets it).
func (f *Fake) SetBaseURL(u string) {
	f.mu.Lock()
	f.baseURL = strings.TrimRight(u, "/")
	f.mu.Unlock()
}

// URL is the base URL.
func (f *Fake) URL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.baseURL
}

// Close stops the server and the delivery worker after in-flight deliveries.
func (f *Fake) Close() {
	f.Flush()
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		close(f.queue)
	}
	f.mu.Unlock()
	if f.srv != nil {
		f.srv.Close()
	}
}

// Unhandled lists every request the fake could not serve (each answered 501).
// A test harness asserts it is empty.
func (f *Fake) Unhandled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unhandled...)
}

// --- accounts, the App, credentials ----------------------------------------

// AddUser adds an account (typ: User, Bot, Organization; "" = User; a login
// ending in [bot] defaults to Bot).
func (f *Fake) AddUser(login, typ string) *User {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addUser(login, typ)
}

func (f *Fake) addUser(login, typ string) *User {
	if u := f.user(login); u != nil {
		return u
	}
	if typ == "" {
		typ = "User"
		if strings.HasSuffix(lower(login), "[bot]") {
			typ = "Bot"
		}
	}
	u := &User{ID: f.id(), Login: login, Type: typ, Name: login, Email: lower(strings.TrimSuffix(login, "[bot]")) + "@users.noreply.github.example"}
	f.users[lower(login)] = u
	return u
}

// UserToken registers a personal access token acting as login.
func (f *Fake) UserToken(login, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.addUser(login, "")
	f.tokens[token] = &identity{kind: "user", user: u, token: token}
}

// SetApp creates the GitHub App. pub, when set, is the key App JWTs must be
// signed with.
func (f *Fake) SetApp(id int64, slug string, pub *rsa.PublicKey) *App {
	f.mu.Lock()
	defer f.mu.Unlock()
	bot := f.addUser(slug+"[bot]", "Bot")
	f.app = &App{ID: id, Slug: slug, Name: slug, Bot: bot, PublicKey: pub}
	return f.app
}

// Install installs the App on an account, optionally limited to repos.
func (f *Fake) Install(account string, repos ...string) *Installation {
	f.mu.Lock()
	defer f.mu.Unlock()
	acct := f.addUser(account, "")
	in := &Installation{ID: f.id(), Account: acct}
	for _, r := range repos {
		in.Repos = append(in.Repos, lower(r))
	}
	f.installations[in.ID] = in
	for _, r := range f.repos {
		if f.covers(in, r) {
			r.InstallationID = in.ID
		}
	}
	return in
}

func (f *Fake) covers(in *Installation, r *Repo) bool {
	if lower(r.Owner.Login) != lower(in.Account.Login) {
		return false
	}
	if len(in.Repos) == 0 {
		return true
	}
	for _, x := range in.Repos {
		if x == lower(r.FullName()) {
			return true
		}
	}
	return false
}

func (f *Fake) mintInstallationToken(inst int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("inst-%d-%d", inst, f.id())))
	tok := "ghs_" + hex.EncodeToString(sum[:])[:36]
	f.tokens[tok] = &identity{kind: "installation", user: f.app.Bot, inst: inst, token: tok}
	return tok
}

// AddHook registers a webhook endpoint. repos (owner/name) limits it to those
// repositories; none means every repo (an App webhook).
func (f *Fake) AddHook(url, secret string, repos ...string) *Hook {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := &Hook{ID: f.id(), URL: url, Secret: secret}
	for _, r := range repos {
		h.Repos = append(h.Repos, lower(r))
	}
	f.hooks = append(f.hooks, h)
	return h
}

// --- repos -------------------------------------------------------------------

// AddRepo creates owner/name with a default branch holding one commit. The
// owner account is created if needed. Default protection: one required
// approval, branches must be up to date.
func (f *Fake) AddRepo(full string) *Repo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addRepo(full)
}

func (f *Fake) addRepo(full string) *Repo {
	if r := f.repo(full); r != nil {
		return r
	}
	owner, name, _ := strings.Cut(full, "/")
	o := f.addUser(owner, "")
	r := &Repo{
		ID: f.id(), Owner: o, Name: name, DefaultBranch: "main",
		Protection:    Protection{RequiredApprovals: 1, RequireUpToDate: true},
		Collaborators: map[string]bool{lower(owner): true},
		Branches:      map[string]string{}, Commits: map[string]*Commit{},
		Issues: map[int]*Issue{}, Labels: map[string]*Label{}, Contents: map[string]*Content{},
		CreatedAt: f.now(),
	}
	root := &Commit{SHA: f.sha(full), Message: "initial commit", Author: o, At: f.now()}
	r.Commits[root.SHA] = root
	r.Branches["main"] = root.SHA
	f.repos[lower(full)] = r
	for _, in := range f.installations {
		if f.covers(in, r) {
			r.InstallationID = in.ID
		}
	}
	return r
}

// Repo returns a repository's model (nil if absent). The caller must not hold
// it across scenario calls that mutate it.
func (f *Fake) Repo(full string) *Repo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repo(full)
}

// AddCollaborator lets login be requested as a reviewer on repo.
func (f *Fake) AddCollaborator(repo, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addUser(login, "")
	f.repo(repo).Collaborators[lower(login)] = true
}

// SetProtection replaces repo's base-branch protection.
func (f *Fake) SetProtection(repo string, p Protection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo(repo).Protection = p
	for _, is := range f.repo(repo).Issues {
		if is.Pull != nil {
			is.Pull.touched()
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Handler is the fake's HTTP handler (REST, GraphQL, the /_fake admin API).
func (f *Fake) Handler() http.Handler { return http.HandlerFunc(f.serve) }
