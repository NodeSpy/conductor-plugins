// Package ghsource is the PUBLIC, daemon-agnostic GitHub EVENT SOURCE: it
// turns webhook deliveries and a periodic catch-up sweep into conductor
// triggers. It is the source half of what pkg/githubkit is for verbs, and it is
// the ONE implementation conductor's bundled github connector and the
// external conductor-github plugin both run:
//
//   - event parsing for every delivery kind (pull_request, review, review
//     comment, issue comment, check run/suite, workflow run, status, issues,
//     projects_v2_item, release, deployment_status, alerts);
//   - review folding — a submitted review and its N inline-comment
//     deliveries become exactly one trigger, claimed once (reviewfold.go);
//   - the own-status guard (a commit status conductor posted never reads back
//     as CI), closed-PR drops, merge-state triggers (merge_conflict,
//     pr_behind, merge_ready), stuck-check polling;
//   - `me` identity (configured, or discovered from the write credential) and
//     the ownership gates built on it;
//   - the unified `filter:` facts, match keys and intrinsic-default lowering
//     (filter.go), over the public IR in pkg/sourcekit;
//   - the adaptive catch-up sweep, the smee relay, the direct webhook
//     listener, App JWT / installation-token handling, and the REST/GraphQL
//     reads all of the above need.
//
// It holds no durable state. A host supplies triggers (as Rules of Actions),
// an EmitFunc, and keeps whatever it persists — conductor's engine keeps the
// comment high-water marks every new_comment / changes_requested trigger is
// deduplicated against (comment_id / comment_kind in the context), and its
// own attempt and dedup bookkeeping. In-memory, per process, the source keeps
// only the review-claim cache that makes folding dispatch once.
//
// Standard library plus golang-jwt; nothing under conductor's internal/.
package ghsource

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"
)

// Config is one source instance's configuration.
type Config struct {
	App AppConfig `yaml:"app"`
	// Token is the App-less credential: a PAT used for reads/enrichment when
	// no GitHub App is configured. The full chain is app → token → the `gh`
	// binary (`gh auth token`); with no App, events arrive via a plain
	// webhook (+ secret) or the sweep, and repo globs are not expandable.
	Token    string        `yaml:"token"`
	Webhook  WebhookConfig `yaml:"webhook"`
	Sweep    SweepConfig   `yaml:"sweep"`
	Defaults Rule          `yaml:"defaults"`
	Rules    []Rule        `yaml:"rules"`

	// ProjectMap remaps a repo (owner/name) to the paseo project name of an
	// existing workspace, so checkouts reuse it instead of cloning a fresh one.
	// Useful when the forge repo and the registered paseo project differ in org
	// or casing (e.g. AcmeCorp/Widget -> acme/widget). Keys
	// are matched case-insensitively; only affects checkout resolution.
	ProjectMap map[string]string `yaml:"project_map"`

	// ProjectRewrite is a blanket fallback applied to every repo in this instance
	// that has no explicit ProjectMap entry — the shortcut for a whole org whose
	// paseo projects share a naming convention. See ProjectRewrite.
	ProjectRewrite ProjectRewrite `yaml:"project_rewrite"`

	// Identity is this source's credential policy: which token reads vs
	// writes, and commit authorship. The source itself reads it only to
	// discover `me` from the write credential; a host also hands it to the
	// agents it dispatches.
	Identity Identity `yaml:"identity"`

	// APIBase overrides the GitHub REST/GraphQL API base (GitHub Enterprise
	// Server, or a hermetic test double). Empty uses PC_GITHUB_API_BASE when
	// set, else the public API — which an external plugin, whose environment
	// conductor scrubs, cannot rely on inheriting.
	APIBase string `yaml:"api_base"`

	// MergeExt merges the host's Action.Ext payloads when a rule's variant is
	// overlaid onto the defaults' (see MergeRule) — conductor's builtin
	// adapter merges its config.Action there. nil keeps the override's Ext
	// when it has one, else the base's.
	MergeExt func(base, over any) any `yaml:"-"`
}

// Identity controls which credential reads vs writes, and commit authorship.
// Values are inline: a known keyword, or (after ${ENV} expansion) a literal token.
//   - ReadToken:  "app" (default) — the App installation token; "gh_auth" — `gh
//     auth token`; anything else — a literal token used verbatim for reads.
//   - WriteToken: "gh_auth" (default) — `gh auth token`; anything else — a literal
//     token (e.g. a PAT via ${GH_PAT}) used verbatim for posts. Writes are always
//     you, never the bot, so "app" is not a write option.
//   - CommitAuthor: "self" (default) — commits/pushes carry your git identity.
type Identity struct {
	ReadToken    string `yaml:"read_token" json:"read_token,omitempty"`
	WriteToken   string `yaml:"write_token" json:"write_token,omitempty"`
	CommitAuthor string `yaml:"commit_author" json:"commit_author,omitempty"`
}

// ProjectRewrite derives a paseo project name from a repo (owner/name) without
// listing each repo. Org, when set, replaces the owner segment (e.g. a webhook's
// AcmeCorp -> the registered acme). The result is always matched
// case-insensitively and normalized to lowercase, since paseo project names are
// lowercased — so casing differences between the forge repo and the registered
// project never force a fresh clone. It applies to every repo in the integration;
// ProjectMap entries take precedence. Only affects checkout.
type ProjectRewrite struct {
	Org string `yaml:"org"` // override the owner/org segment
}

// active reports whether the rewrite changes anything.
func (r ProjectRewrite) active() bool { return r.Org != "" }

// AppConfig holds the GitHub App credentials — and ONLY those. Webhook
// verification (the secret and the signature switch) is a property of the
// receiver, not of App auth, and lives on WebhookConfig.
type AppConfig struct {
	AppID          int64  `yaml:"app_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
}

// WebhookConfig configures how webhooks arrive: via a smee.io channel, a direct
// HTTP listener, or both — and how a delivery is authenticated once it does.
type WebhookConfig struct {
	SmeeURL string `yaml:"smee_url"` // subscribe to a smee.io SSE channel
	Listen  string `yaml:"listen"`   // bind a direct HTTP receiver, e.g. "127.0.0.1:8787"
	Path    string `yaml:"path"`     // HTTP path (default "/webhook")
	// Secret is the webhook secret GitHub signs each delivery with (the same
	// value configured on the App's or the repo's webhook). Required whenever
	// verification is on.
	Secret string `yaml:"secret"`
	// VerifySig switches HMAC verification of X-Hub-Signature-256. Nil means
	// on: a webhook receiver that does not check its signatures accepts
	// anything that reaches the port, so the default has to be the safe one.
	VerifySig *bool `yaml:"verify_signature"`
	// PublicURL is filled in by the HOST, never the operator: when `expose`
	// names an exposure connector (the `listeners` connection semantic —
	// plugin-contract.md §2.4), the engine opens it for Listen at instance
	// start and writes the public URL here before Start runs. Empty when no
	// `expose` is configured (a plain local listener), or while the
	// exposure is still being retried — see Source.Start.
	PublicURL string `yaml:"public_url"`
}

// Verify reports whether HMAC signature verification is on (default true).
func (w WebhookConfig) Verify() bool { return w.VerifySig == nil || *w.VerifySig }

// Configured reports whether a webhook transport is set up — a smee channel or a
// direct listener. It is the switch between the two sweep cadences: with a webhook
// carrying real-time, the sweep is catch-up and backs off; without one, the sweep
// IS the event source and polls at a fixed cadence.
func (w WebhookConfig) Configured() bool { return w.SmeeURL != "" || w.Listen != "" }

// SweepConfig configures the catch-up sweep. Every field is optional: an omitted
// sweep block is on by default (see IsEnabled), covering every repo the App is
// installed on, at a cadence chosen by whether a webhook is configured.
type SweepConfig struct {
	// Enabled defaults TRUE (nil → on). Without a webhook the sweep is the only
	// event source, so on-by-default is what makes conductor work out of the box;
	// set it false to turn polling off.
	Enabled *bool `yaml:"enabled"`
	// Interval is the CEILING of the adaptive cadence — the cadence a quiet,
	// webhook-connected daemon settles at (default 1h). Only used in webhook mode.
	Interval time.Duration `yaml:"interval"`
	// MinInterval is the tight cadence (default 2m): the floor the adaptive cadence
	// resets to on startup/reconnect in webhook mode, AND the fixed poll interval in
	// no-webhook mode (where there is nothing to back off from).
	MinInterval time.Duration `yaml:"min_interval"`
	// Repos optionally NARROWS the sweep to specific repos or owner-globs
	// (`acme/*`). Omitted → every repo across every App installation.
	Repos []string `yaml:"repos"`
}

// IsEnabled reports whether the sweep runs. Absent (nil) means yes — the sweep is
// on by default so a conductor with no webhook still receives events.
func (s SweepConfig) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// Rule is one entry in the instance's `rules` list (or the `defaults` block).
type Rule struct {
	Match    Match                `yaml:"match"`
	Me       Actors               `yaml:"me"`       // your GitHub login(s) — defines "you"
	Reviewer Actors               `yaml:"reviewer"` // whose requested review triggers review_requested
	Assignee Actors               `yaml:"assignee"` // whose assignment triggers issue_assigned
	Actions  map[string]ActionSet `yaml:"actions"`  // kind -> one or more named variants
}

// Match selects which events a rule applies to.
type Match struct {
	Repos   []string `yaml:"repos"`   // globs, e.g. "owner/*"
	Project string   `yaml:"project"` // Projects v2 title/number (M3)
	Status  string   `yaml:"status"`  // Projects v2 status/field value (M3)
}

// Source is one running github event source instance.
type Source struct {
	name string
	cfg  Config
	// clientMu guards app/rest. They are built once, lazily, by ensureClients
	// — historically safe only because Start() called it before sweep/webhook
	// goroutines existed. Since webhook-only instances start without
	// resolving credentials (lazy-start), sweep() and stuckPass() each call
	// ensureClients from their own goroutine, so the build-once check-and-set
	// and every other reader of app/rest (events.go, reviewfold.go, force.go,
	// sweep.go, discoverSelf, AppToken) must go through clientMu — via
	// appAuth()/restClient() for reads, never g.app/g.rest directly outside
	// ensureClients itself.
	clientMu sync.Mutex
	app      *appAuth
	rest     *restClient
	// self = your GitHub login(s), used to ignore your own comments, detect your
	// own PRs (self_review), and filter authored PRs during sweep. Built from
	// `me:` if set anywhere, else falls back to reviewer/assignee logins.
	self map[string]bool
	// projectMap is cfg.ProjectMap keyed lowercase for case-insensitive lookup.
	projectMap map[string]string
	// renew nudges the sweep to run now and reset its adaptive cadence — signaled by
	// a smee reconnect (dropped-webhook window) and by SweepNow() (a manual `sweep`).
	// Buffered+coalescing (a full buffer means a catch-up is already pending).
	renew chan struct{}
	// reviews caches submitted reviews' facts by id and which have been
	// turned into their one event, so a review's many deliveries become
	// exactly one trigger (see reviewfold.go).
	reviews reviewCache

	// ownStatus holds the commit-status contexts conductor posts under — the
	// configured ones plus whatever the progress reporter resolved at run
	// time (NoteOwnStatusContext). Guarded by ownMu: noted from run
	// goroutines, read on the webhook path.
	ownMu     sync.Mutex
	ownStatus map[string]bool
	// acting is the login your writes act as — published to every event as
	// {{.me.login}}: the one discovered from the write identity, else the
	// first login `self` was built from (me:, or the reviewer/assignee
	// fallback). Guarded by ownMu.
	acting string
}

// meFact is the `me` event fact: { login } — you, as your writes act. nil
// while no login is known.
func (g *Source) meFact() map[string]any {
	g.ownMu.Lock()
	defer g.ownMu.Unlock()
	if g.acting == "" {
		return nil
	}
	return map[string]any{"login": g.acting}
}

// NoteOwnStatusContext records a commit-status context conductor posts
// under, so a delivery of that status never reads as CI (see ownStatus).
func (g *Source) NoteOwnStatusContext(c string) {
	if c == "" {
		return
	}
	g.ownMu.Lock()
	defer g.ownMu.Unlock()
	if g.ownStatus == nil {
		g.ownStatus = map[string]bool{}
	}
	g.ownStatus[strings.ToLower(c)] = true
}

// OwnStatus reports whether a commit-status context is one conductor posts
// (see isOwnStatus).
func (g *Source) OwnStatus(c string) bool { return g.isOwnStatus(c) }

// isOwnStatus reports whether a commit status's context is one conductor
// posts: a configured/noted progress context, or one of your logins — the
// default progress context. A status conductor wrote must never come back as
// a failing check, or a `failure` verdict would launch the next fixer, whose
// verdict launches the next.
func (g *Source) isOwnStatus(c string) bool {
	c = strings.ToLower(strings.TrimSpace(c))
	if c == "" {
		return false
	}
	g.ownMu.Lock()
	own := g.ownStatus[c]
	g.ownMu.Unlock()
	return own || g.self[c]
}

// SweepNow triggers an immediate catch-up sweep (and resets the adaptive cadence)
// when the sweep is enabled. Non-blocking and coalescing. Returns false if the
// sweep isn't enabled for this integration.
func (g *Source) SweepNow() bool {
	if !g.cfg.Sweep.IsEnabled() || g.renew == nil {
		return false
	}
	select {
	case g.renew <- struct{}{}:
	default: // a catch-up is already pending
	}
	return true
}

// New builds a source instance from its configuration. It makes no network
// calls: credentials are resolved lazily on Start (or the first read), and
// `me` discovery runs at Start.
func New(name string, cfg Config) (*Source, error) {
	if cfg.Identity.ReadToken == "" {
		cfg.Identity.ReadToken = "app"
	}
	if cfg.Identity.WriteToken == "" {
		cfg.Identity.WriteToken = "gh_auth"
	}
	if cfg.Identity.CommitAuthor == "" {
		cfg.Identity.CommitAuthor = "self"
	}
	g := &Source{name: name, cfg: cfg, self: map[string]bool{},
		projectMap: map[string]string{}, renew: make(chan struct{}, 1)}
	for k, v := range cfg.ProjectMap {
		g.projectMap[strings.ToLower(k)] = v
	}
	rules := append([]Rule{cfg.Defaults}, cfg.Rules...)

	// Prefer an explicit `me:`; only fall back to reviewer/assignee if none set.
	for _, r := range rules {
		for _, l := range r.Me.Logins {
			g.self[strings.ToLower(l)] = true
			if g.acting == "" {
				g.acting = l // the first one you named; discovery overrides it
			}
		}
	}
	if len(g.self) == 0 {
		add := func(a Actors) {
			for _, l := range a.Logins {
				g.self[strings.ToLower(l)] = true
				if g.acting == "" {
					g.acting = l
				}
			}
		}
		for _, r := range rules {
			add(r.Reviewer) // rule-level fallback
			add(r.Assignee)
			for _, set := range r.Actions { // action-level reviewer/assignee (per variant)
				for _, a := range set {
					add(a.Reviewer)
					add(a.Assignee)
				}
			}
		}
	}
	return g, nil
}

// Name returns the instance name.
func (g *Source) Name() string { return g.name }

// ensureClients builds the App auth + REST client once (idempotent). Safe to
// call concurrently — sweep() and stuckPass() each call it from their own
// goroutine since the webhook-only lazy-start change, so the check-and-set
// is guarded by clientMu rather than left racing. A failure leaves g.app nil
// under the lock, so the NEXT call (from either goroutine) retries the build
// rather than latching a permanent failure.
func (g *Source) ensureClients() error {
	g.clientMu.Lock()
	defer g.clientMu.Unlock()
	if g.app != nil {
		return nil
	}
	var app *appAuth
	switch {
	case g.cfg.App.AppID > 0:
		a, err := newAppAuth(g.cfg.App.AppID, g.cfg.App.PrivateKeyPath)
		if err != nil {
			return err
		}
		app = a
	case g.cfg.Token != "":
		app = newStaticAuth(g.cfg.Token)
	default:
		// App-less, token-less: fall back to the gh CLI's stored login.
		tok, err := ghAuthToken()
		if err != nil {
			return fmt.Errorf("github[%s]: no credentials — configure app: or token:, or log in with `gh auth login`: %w", g.name, err)
		}
		app = newStaticAuth(tok)
	}
	if b := strings.TrimRight(g.cfg.APIBase, "/"); b != "" {
		app.apiBase = b
	}
	g.app = app
	g.rest = newRESTClient(app)
	return nil
}

// appAuth returns the current App auth client, or nil if ensureClients
// hasn't built one yet. The only safe way to read g.app outside
// ensureClients itself — see clientMu.
func (g *Source) appAuth() *appAuth {
	g.clientMu.Lock()
	defer g.clientMu.Unlock()
	return g.app
}

// restClient returns the current REST client, or nil if ensureClients hasn't
// built one yet. The only safe way to read g.rest outside ensureClients
// itself — see clientMu.
func (g *Source) restClient() *restClient {
	g.clientMu.Lock()
	defer g.clientMu.Unlock()
	return g.rest
}

// ghAuthToken shells out to `gh auth token` — the last link of the App-less
// credential chain (app → token → gh).
func ghAuthToken() (string, error) {
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

// discoverSelf auto-fills the `self` identity (`me:`) from the WRITE credential
// when nothing else identified you — so `me.logins` is optional. Your writes run
// as you (identity.write_token: `gh_auth` or a literal token, never the App), so
// `GET /user` on that token returns your login. Runs only when `self` is otherwise
// empty (an explicit `me:`, or a reviewer/assignee fallback, still wins), and is
// best-effort: a failure leaves `self` empty (the prior behavior) with a hint,
// never an error. Set `me.logins` to override — multiple accounts, or a write
// credential that isn't the human whose PRs/reviews you want tracked.
func (g *Source) discoverSelf(ctx context.Context) {
	if len(g.self) > 0 {
		return
	}
	if err := g.ensureClients(); err != nil {
		// No credentials yet (a webhook-only instance with no app:/token:
		// configured, say) — not an error, just nothing to discover from
		// until one resolves some other way (a later sweep, once
		// configured).
		log.Printf("github[%s]: me: not set and no credentials yet (%v) — set me.logins to identify your PRs/reviews", g.name, err)
		return
	}
	tok := g.cfg.Identity.WriteToken
	if tok == "" || tok == "gh_auth" {
		t, err := ghAuthToken()
		if err != nil {
			log.Printf("github[%s]: me: not set and auto-discovery unavailable (%v) — set me.logins to identify your PRs/reviews", g.name, err)
			return
		}
		tok = t
	}
	app := g.appAuth() // ensureClients succeeded above, so this is never nil
	login, err := githubWhoami(ctx, app.httpc, app.apiBase, tok)
	if err != nil {
		log.Printf("github[%s]: me: auto-discovery failed (%v) — set me.logins to identify your PRs/reviews", g.name, err)
		return
	}
	if login != "" {
		g.ownMu.Lock()
		g.acting = login
		g.ownMu.Unlock()
		g.self[strings.ToLower(login)] = true
		log.Printf("github[%s]: me: auto-discovered as %q from the write identity — set me.logins to override", g.name, login)
	}
}

// Translate maps a raw webhook event to Triggers. Exported for the `replay`
// dev command; conflict/behind kinds need live REST and are skipped when
// clients aren't initialized.
func (g *Source) Translate(ctx context.Context, eventType string, body []byte) []Trigger {
	return g.triggersFor(ctx, eventType, body)
}

// SweepOnce runs a single catch-up sweep (for the `sweep` command).
func (g *Source) SweepOnce(ctx context.Context, emit EmitFunc) error {
	if err := g.ensureClients(); err != nil {
		return err
	}
	return g.sweep(ctx, emit)
}

// AppToken mints a fresh App installation token for the given installation id.
// Used to re-mint the (short-lived) token when a persisted workflow resumes.
func (g *Source) AppToken(ctx context.Context, instID int64) (string, error) {
	if err := g.ensureClients(); err != nil {
		return "", err
	}
	return g.appAuth().installationToken(ctx, instID)
}

// SweepSettings exposes the effective catch-up sweep config (for the
// connector-lowering tests and introspection).
func (g *Source) SweepSettings() SweepConfig { return g.cfg.Sweep }

// IdentityTokens exposes this integration's credential policy (raw values, already
// ${ENV}-expanded by the loader). main resolves the "app"/"gh_auth" keywords
// against its App-token and `gh auth token` sources; any other value is a literal.
func (g *Source) IdentityTokens() (read, write, commitAuthor string) {
	id := g.cfg.Identity
	return id.ReadToken, id.WriteToken, id.CommitAuthor
}

// Validate checks the instance configuration.
func (g *Source) Validate() error {
	appless := g.cfg.App.AppID == 0 && g.cfg.App.PrivateKeyPath == ""
	if !appless {
		// A partially-configured App is a config bug, not App-less mode.
		if g.cfg.App.AppID == 0 {
			return fmt.Errorf("github[%s]: app.app_id is required when app: is configured", g.name)
		}
		if g.cfg.App.PrivateKeyPath == "" {
			return fmt.Errorf("github[%s]: app.private_key_path is required when app: is configured", g.name)
		}
		if _, err := os.Stat(expandHome(g.cfg.App.PrivateKeyPath)); err != nil {
			return fmt.Errorf("github[%s]: private key not readable: %w", g.name, err)
		}
	}
	hasWebhook := g.cfg.Webhook.SmeeURL != "" || g.cfg.Webhook.Listen != ""
	if hasWebhook && g.cfg.Webhook.Verify() && g.cfg.Webhook.Secret == "" {
		return fmt.Errorf("github[%s]: webhook.secret required when webhook.verify_signature is on", g.name)
	}
	if !hasWebhook && !g.cfg.Sweep.IsEnabled() {
		return fmt.Errorf("github[%s]: no event source — set webhook.smee_url/webhook.listen, or leave the sweep enabled to poll", g.name)
	}
	if appless {
		for _, r := range g.cfg.Sweep.Repos {
			if strings.Contains(r, "*") {
				return fmt.Errorf("github[%s]: sweep repo glob %q needs a GitHub App — list repos explicitly when using token/gh credentials", g.name, r)
			}
		}
	}
	// Action maps are keyed by kind; a typo or a renamed kind (e.g. the old
	// issue_labeled) would otherwise sit in config doing nothing. Reject unknown keys.
	check := func(where string, actions map[string]ActionSet) error {
		for k := range actions {
			if !knownKinds[k] {
				return fmt.Errorf("github[%s]: unknown action kind %q in %s", g.name, k, where)
			}
		}
		return nil
	}
	if err := check("defaults.actions", g.cfg.Defaults.Actions); err != nil {
		return err
	}
	for i, r := range g.cfg.Rules {
		if err := check(fmt.Sprintf("rules[%d].actions", i), r.Actions); err != nil {
			return err
		}
	}
	return nil
}

// knownKinds is the set of GitHub trigger kinds an action map may configure.
var knownKinds = map[string]bool{
	"merge_conflict": true, "pr_behind": true, "failing_checks": true,
	"changes_requested": true, "new_comment": true, "review_requested": true,
	"self_review": true, "merge_ready": true, "issue_matched": true,
	"release": true, "deployment_status": true, "dependabot_alert": true,
	"secret_scanning_alert": true, "stuck_checks": true,
	// issue_assigned + issue_project_moved were merged into issue_matched (v0.4.48).
}

// resolve returns the effective rule (reviewer/assignee/actions merged over
// defaults) for a repo, or ok=false if no rule matches.
func (g *Source) resolve(repo string) (Rule, bool) {
	i := BestRule(g.cfg.Rules, repo)
	if i < 0 {
		return Rule{}, false
	}
	return g.merge(g.cfg.Rules[i]), true
}

// BestRule picks the rule a repo resolves to: the index of the MOST-SPECIFIC
// match (not the first listed), so rule order doesn't matter — an exact
// "AcmeCorp/Widget" beats "AcmeCorp/*" beats "*/*". Ties (equally-specific
// matches) keep the earliest rule for determinism. -1 when none matches.
func BestRule(rules []Rule, repo string) int {
	bestIdx, bestScore := -1, -1
	for i, r := range rules {
		score := ruleSpecificity(r.Match.Repos, repo)
		if score > bestScore {
			bestScore, bestIdx = score, i
		}
	}
	return bestIdx
}

// PatternSpecificity scores a repo glob the way rule resolution does (see
// patternSpecificity).
func PatternSpecificity(p string) int { return patternSpecificity(p) }

// KnownKinds is the set of trigger kinds an action map may configure.
func KnownKinds() map[string]bool {
	out := make(map[string]bool, len(knownKinds))
	for k, v := range knownKinds {
		out[k] = v
	}
	return out
}

// ruleSpecificity returns the highest match specificity of any repo pattern in
// the rule that matches repo, or -1 if none match.
func ruleSpecificity(patterns []string, repo string) int {
	best := -1
	for _, p := range patterns {
		if p != repo {
			if ok, _ := path.Match(p, repo); !ok {
				continue
			}
		}
		if s := patternSpecificity(p); s > best {
			best = s
		}
	}
	return best
}

// patternSpecificity scores a repo glob: an exact (wildcard-free) pattern beats
// any wildcard pattern; among wildcard patterns, more literal (non-glob) chars
// wins, so "AcmeCorp/*" (13 literal) outranks "*/*" (1). Kept simple: `*`,
// `?`, and `[` are treated as glob metacharacters.
func patternSpecificity(p string) int {
	glob := strings.Count(p, "*") + strings.Count(p, "?") + strings.Count(p, "[")
	literal := len(p) - glob
	if glob == 0 {
		return 100000 + literal // exact match dominates any wildcard
	}
	return literal
}

// merge overlays a rule onto the instance defaults.
func (g *Source) merge(r Rule) Rule { return MergeRule(g.cfg.Defaults, r, g.cfg.MergeExt) }

// MergeRule overlays a rule onto a defaults rule — the resolve() semantics:
// the rule's reviewer/assignee win when set, and a rule's set for a kind
// replaces the defaults' set, each variant merged over the default base (the
// first default variant) for that kind. mergeExt merges the host payloads (nil
// keeps the override's Ext when set, else the base's).
func MergeRule(defaults, r Rule, mergeExt func(base, over any) any) Rule {
	out := Rule{
		Reviewer: defaults.Reviewer,
		Assignee: defaults.Assignee,
		Actions:  map[string]ActionSet{},
	}
	for k, v := range defaults.Actions {
		out.Actions[k] = v
	}
	if len(r.Reviewer.Logins) > 0 || len(r.Reviewer.Teams) > 0 {
		out.Reviewer = r.Reviewer
	}
	if len(r.Assignee.Logins) > 0 {
		out.Assignee = r.Assignee
	}
	for k, set := range r.Actions {
		var base Action
		if d := defaults.Actions[k]; len(d) > 0 {
			base = d[0]
		}
		merged := make(ActionSet, len(set))
		for i, v := range set {
			merged[i] = mergeAction(base, v, mergeExt)
		}
		out.Actions[k] = merged
	}
	return out
}

// mergeAction overlays override fields onto a base action (only non-zero
// override fields win), so a rule can tweak just one option of a default.
// These are exactly the fields this source reads; a host merges its own
// payload through mergeExt, with the same "non-zero override wins" rule.
func mergeAction(base, over Action, mergeExt func(base, over any) any) Action {
	switch {
	case mergeExt != nil:
		base.Ext = mergeExt(base.Ext, over.Ext)
	case over.Ext != nil:
		base.Ext = over.Ext
	}
	if over.Name != "" {
		base.Name = over.Name
	}
	if over.Enabled != nil {
		base.Enabled = over.Enabled
	}
	if len(over.IgnoreChecks) > 0 {
		base.IgnoreChecks = over.IgnoreChecks
	}
	if over.StuckAfter > 0 {
		base.StuckAfter = over.StuckAfter
	}
	if over.PollInterval > 0 {
		base.PollInterval = over.PollInterval
	}
	if len(over.FromUsers) > 0 {
		base.FromUsers = over.FromUsers
	}
	if len(over.IgnoreUsers) > 0 {
		base.IgnoreUsers = over.IgnoreUsers
	}
	if over.AuthorBot != nil {
		base.AuthorBot = over.AuthorBot
	}
	if len(over.LabelsAny) > 0 {
		base.LabelsAny = over.LabelsAny
	}
	if len(over.LabelsAll) > 0 {
		base.LabelsAll = over.LabelsAll
	}
	if len(over.Authors) > 0 {
		base.Authors = over.Authors
	}
	if over.SoleAssignee {
		base.SoleAssignee = over.SoleAssignee
	}
	if len(over.Reviewer.Logins) > 0 || len(over.Reviewer.Teams) > 0 {
		base.Reviewer = over.Reviewer
	}
	if len(over.Assignee.Logins) > 0 {
		base.Assignee = over.Assignee
	}
	if over.RequireLabel != "" {
		base.RequireLabel = over.RequireLabel
	}
	if over.IncludePrereleases {
		base.IncludePrereleases = over.IncludePrereleases
	}
	if len(over.Gates) > 0 {
		base.Gates = over.Gates
	}
	if len(over.Repos) > 0 {
		base.Repos = over.Repos
	}
	if len(over.ExcludeRepos) > 0 {
		base.ExcludeRepos = over.ExcludeRepos
	}
	if !over.Exclude.Empty() {
		base.Exclude = over.Exclude
	}
	if over.Filter != nil {
		base.Filter = over.Filter
	}
	return base
}

func matchRepo(patterns []string, repo string) bool {
	for _, p := range patterns {
		if p == repo {
			return true
		}
		if ok, _ := path.Match(p, repo); ok {
			return true
		}
	}
	return false
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}
