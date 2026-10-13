package ghfake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// route is one REST operation: GitHub's OpenAPI path template, the auth it
// takes, and its handler.
type route struct {
	method string
	path   string // OpenAPI template, e.g. /repos/{owner}/{repo}/pulls/{pull_number}
	auth   string // app | repo | user | any
	h      func(*call) (int, any)
}

// call is one request in flight.
type call struct {
	f      *Fake
	r      *http.Request
	w      http.ResponseWriter
	params map[string]string
	who    *identity
	body   map[string]any
	raw    []byte
	repo   *Repo
	hdr    http.Header
}

func (c *call) p(name string) string { return c.params[name] }

func (c *call) n(name string) int {
	v, _ := strconv.Atoi(c.params[name])
	return v
}

func (c *call) i64(name string) int64 {
	v, _ := strconv.ParseInt(c.params[name], 10, 64)
	return v
}

// actor is the account a write is attributed to: the user for a user token,
// the App's bot account for an installation token.
func (c *call) actor() *User { return c.who.user }

// Endpoints lists every REST operation the fake implements, as
// "METHOD /openapi/template".
func Endpoints() []string {
	var out []string
	for _, rt := range routes() {
		out = append(out, rt.method+" "+rt.path)
	}
	sort.Strings(out)
	return out
}

// OnResponse, when set, sees every REST response the fake writes (op is
// "METHOD /openapi/template") — the hook the schema-validation tests use.
var OnResponse func(op string, status int, body []byte)

const docsURL = "https://docs.github.com/rest"

func ghErr(status int, msg string) (int, any) {
	return status, map[string]any{"message": msg, "documentation_url": docsURL, "status": strconv.Itoa(status)}
}

func notFound() (int, any) { return ghErr(404, "Not Found") }

// invalid is GitHub's 422 Validation Failed body.
func invalid(resource, field, code, msg string) (int, any) {
	e := map[string]any{"resource": resource, "field": field, "code": code}
	if msg != "" {
		e["message"] = msg
	}
	return 422, map[string]any{"message": "Validation Failed", "errors": []any{e}, "documentation_url": docsURL, "status": "422"}
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_fake/") || r.URL.Path == "/_fake" {
		f.admin(w, r)
		return
	}
	raw, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if r.URL.Path == "/graphql" && r.Method == http.MethodPost {
		f.serveGraphQL(w, r, raw)
		return
	}
	rt, params := match(r.Method, r.URL.Path)
	if rt == nil {
		f.mu.Lock()
		f.unhandled = append(f.unhandled, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		f.opts.Logf("ghfake: UNHANDLED %s %s — not implemented (501)", r.Method, r.URL.RequestURI())
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"message": "ghfake: not implemented: " + r.Method + " " + r.URL.Path, "documentation_url": docsURL, "status": "501"})
		return
	}
	f.mu.Lock()
	c := &call{f: f, r: r, w: w, params: params, raw: raw, hdr: http.Header{}}
	status, out := f.authorize(c, rt)
	if status == 0 && len(raw) > 0 && r.Header.Get("Content-Type") != "application/octet-stream" && !strings.HasPrefix(rt.path, "/repos/{owner}/{repo}/releases/{release_id}/assets") {
		if err := json.Unmarshal(raw, &c.body); err != nil {
			status, out = ghErr(400, "Problems parsing JSON")
		}
	}
	if c.body == nil {
		c.body = map[string]any{}
	}
	if status == 0 {
		status, out = rt.h(c)
	}
	f.mu.Unlock()
	f.respond(c, rt, status, out)
}

func match(method, path string) (*route, map[string]string) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for _, rt := range routes() {
		if rt.method != method {
			continue
		}
		tsegs := strings.Split(strings.Trim(rt.path, "/"), "/")
		// {path} in the contents API spans the rest of the path.
		if len(tsegs) > 0 && tsegs[len(tsegs)-1] == "{path}" && len(segs) >= len(tsegs) {
			params := map[string]string{}
			ok := true
			for i, t := range tsegs[:len(tsegs)-1] {
				if !segMatch(t, segs[i], params) {
					ok = false
					break
				}
			}
			if ok {
				params["path"] = strings.Join(segs[len(tsegs)-1:], "/")
				return &rt, params
			}
			continue
		}
		if len(tsegs) != len(segs) {
			continue
		}
		params := map[string]string{}
		ok := true
		for i, t := range tsegs {
			if !segMatch(t, segs[i], params) {
				ok = false
				break
			}
		}
		if ok {
			return &rt, params
		}
	}
	return nil, nil
}

func segMatch(t, s string, params map[string]string) bool {
	if strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
		if s == "" {
			return false
		}
		v, err := url.PathUnescape(s)
		if err != nil {
			return false
		}
		params[t[1:len(t)-1]] = v
		return true
	}
	return t == s
}

// authorize resolves the credential and checks it fits the route; 0 = ok.
func (f *Fake) authorize(c *call, rt *route) (int, any) {
	h := c.r.Header.Get("Authorization")
	tok := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(h, "token "), "Bearer "))
	if h == "" || tok == "" {
		return ghErr(401, "Requires authentication")
	}
	if rt.auth == "app" {
		if f.app == nil {
			return ghErr(401, "A JSON web token could not be decoded")
		}
		if err := f.checkJWT(tok); err != nil {
			return ghErr(401, err.Error())
		}
		c.who = &identity{kind: "app", user: f.app.Bot}
		return 0, nil
	}
	id := f.tokens[tok]
	if id == nil {
		if strings.Count(tok, ".") == 2 {
			return ghErr(401, "A JSON web token can only be used to authenticate as the App — use an installation token")
		}
		return ghErr(401, "Bad credentials")
	}
	c.who = id
	f.usage[tok]++
	if f.usage[tok] > f.opts.RateLimit {
		c.hdr.Set("X-RateLimit-Remaining", "0")
		return ghErr(403, "API rate limit exceeded for "+id.user.Login+".")
	}
	if rt.auth == "user" && id.kind != "user" {
		return ghErr(403, "Resource not accessible by integration")
	}
	if owner, name := c.p("owner"), c.p("repo"); owner != "" && name != "" {
		r := f.repo(owner + "/" + name)
		if r == nil {
			return notFound()
		}
		if id.kind == "installation" && r.InstallationID != id.inst {
			return notFound() // GitHub hides repos an installation cannot see
		}
		c.repo = r
	}
	return 0, nil
}

func (f *Fake) checkJWT(tok string) error {
	claims := jwt.MapClaims{}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}), jwt.WithLeeway(60*time.Second))
	var err error
	if f.app.PublicKey == nil {
		_, _, err = parser.ParseUnverified(tok, claims)
	} else {
		_, err = parser.ParseWithClaims(tok, claims, func(*jwt.Token) (any, error) { return f.app.PublicKey, nil })
	}
	if err != nil {
		return fmt.Errorf("A JSON web token could not be decoded")
	}
	// GitHub takes the App id (an integer, or its client id as a string).
	if iss := fmt.Sprint(claims["iss"]); iss != strconv.FormatInt(f.app.ID, 10) {
		return fmt.Errorf("'Issuer' claim ('iss') must be an Integer")
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return fmt.Errorf("'Expiration time' claim ('exp') must be a numeric value representing the future time at which the assertion expires")
	}
	if exp.Sub(f.now()) > 10*time.Minute+time.Minute {
		return fmt.Errorf("'Expiration time' claim ('exp') is too far in the future")
	}
	return nil
}

// respond writes status/out with GitHub's headers: rate limit, ETag and
// If-None-Match → 304 on reads, and Link pagination (set by paginate).
func (f *Fake) respond(c *call, rt *route, status int, out any) {
	w := c.w
	for k, vs := range c.hdr {
		for _, v := range vs {
			w.Header().Set(k, v)
		}
	}
	if c.who != nil && c.who.token != "" {
		f.mu.Lock()
		used := f.usage[c.who.token]
		f.mu.Unlock()
		rem := f.opts.RateLimit - used
		if rem < 0 {
			rem = 0
		}
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(f.opts.RateLimit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(rem))
		w.Header().Set("X-RateLimit-Used", strconv.Itoa(used))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(f.now().Truncate(time.Hour).Add(time.Hour).Unix(), 10))
		w.Header().Set("X-RateLimit-Resource", "core")
	}
	w.Header().Set("X-GitHub-Api-Version-Selected", "2022-11-28")
	if text, ok := out.(textBody); ok {
		w.Header().Set("Content-Type", text.ctype)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, text.body)
		if OnResponse != nil {
			OnResponse(rt.method+" "+rt.path, status, nil) // a raw media type: no JSON schema applies
		}
		return
	}
	if status == http.StatusNoContent || out == nil {
		w.WriteHeader(status)
		if OnResponse != nil {
			OnResponse(rt.method+" "+rt.path, status, nil)
		}
		return
	}
	b, _ := json.Marshal(out)
	if c.r.Method == http.MethodGet && status == 200 {
		sum := sha256.Sum256(b)
		etag := `W/"` + hex.EncodeToString(sum[:16]) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, max-age=60, s-maxage=60")
		if inm := c.r.Header.Get("If-None-Match"); inm != "" && (inm == etag || strings.TrimPrefix(inm, "W/") == strings.TrimPrefix(etag, "W/")) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
	if OnResponse != nil && status < 300 {
		OnResponse(rt.method+" "+rt.path, status, b)
	}
}

type textBody struct{ ctype, body string }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// paginate slices items by per_page (default 30, max 100) and page, and sets
// GitHub's Link header (first/prev/next/last).
func (c *call) paginate(items []any) []any {
	q := c.r.URL.Query()
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		per = 30
	}
	if per > 100 {
		per = 100
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	last := (len(items) + per - 1) / per
	if last == 0 {
		last = 1
	}
	lo, hi := (page-1)*per, page*per
	if lo > len(items) {
		lo = len(items)
	}
	if hi > len(items) {
		hi = len(items)
	}
	link := func(p int, rel string) string {
		u := *c.r.URL
		qq := u.Query()
		qq.Set("page", strconv.Itoa(p))
		qq.Set("per_page", strconv.Itoa(per))
		u.RawQuery = qq.Encode()
		return fmt.Sprintf("<%s%s>; rel=%q", c.f.baseURL, u.RequestURI(), rel)
	}
	var links []string
	if page > 1 {
		links = append(links, link(page-1, "prev"), link(1, "first"))
	}
	if page < last {
		links = append(links, link(page+1, "next"), link(last, "last"))
	}
	if len(links) > 0 {
		c.hdr.Set("Link", strings.Join(links, ", "))
	}
	return items[lo:hi]
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func strs(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case string:
		out = append(out, x)
	}
	return out
}

func toI(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func sortedIssueNumbers(r *Repo) []int {
	out := make([]int, 0, len(r.Issues))
	for n := range r.Issues {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

var reactionContent = map[string]bool{"+1": true, "-1": true, "laugh": true, "confused": true, "heart": true, "hooray": true, "rocket": true, "eyes": true}

func routes() []route {
	return []route{
		// --- App auth ---
		{"POST", "/app/installations/{installation_id}/access_tokens", "app", hMintToken},
		{"GET", "/app/installations", "app", hListInstallations},
		{"GET", "/repos/{owner}/{repo}/installation", "app", hRepoInstallation},
		{"GET", "/orgs/{org}/installation", "app", hAccountInstallation},
		{"GET", "/users/{username}/installation", "app", hAccountInstallation},
		{"GET", "/installation/repositories", "any", hInstallationRepos},
		{"GET", "/user", "user", hWhoami},
		// --- repo visibility (githubkit's remapGoneIfMissing: a 404 on a
		// PR/issue number is ambiguous between "gone" and "token can't see
		// the repo at all" — this is the probe that tells them apart) ---
		{"GET", "/repos/{owner}/{repo}", "repo", hGetRepo},
		// --- pulls ---
		{"GET", "/repos/{owner}/{repo}/pulls", "repo", hListPulls},
		{"POST", "/repos/{owner}/{repo}/pulls", "repo", hCreatePull},
		{"GET", "/repos/{owner}/{repo}/pulls/{pull_number}", "repo", hGetPull},
		{"PATCH", "/repos/{owner}/{repo}/pulls/{pull_number}", "repo", hUpdatePull},
		{"GET", "/repos/{owner}/{repo}/pulls/{pull_number}/files", "repo", hPullFiles},
		{"GET", "/repos/{owner}/{repo}/pulls/{pull_number}/commits", "repo", hPullCommits},
		{"PUT", "/repos/{owner}/{repo}/pulls/{pull_number}/merge", "repo", hMergePull},
		{"GET", "/repos/{owner}/{repo}/pulls/{pull_number}/requested_reviewers", "repo", hGetRequested},
		{"POST", "/repos/{owner}/{repo}/pulls/{pull_number}/requested_reviewers", "repo", hRequestReviewers},
		{"DELETE", "/repos/{owner}/{repo}/pulls/{pull_number}/requested_reviewers", "repo", hRemoveReviewers},
		{"GET", "/repos/{owner}/{repo}/pulls/{pull_number}/reviews", "repo", hListReviews},
		{"POST", "/repos/{owner}/{repo}/pulls/{pull_number}/reviews", "repo", hCreateReview},
		{"GET", "/repos/{owner}/{repo}/pulls/{pull_number}/reviews/{review_id}", "repo", hGetReview},
		{"GET", "/repos/{owner}/{repo}/pulls/{pull_number}/reviews/{review_id}/comments", "repo", hReviewComments},
		{"GET", "/repos/{owner}/{repo}/pulls/{pull_number}/comments", "repo", hPullComments},
		{"POST", "/repos/{owner}/{repo}/pulls/{pull_number}/comments/{comment_id}/replies", "repo", hReply},
		{"GET", "/repos/{owner}/{repo}/pulls/comments/{comment_id}/reactions", "repo", hListReactions("pull")},
		{"POST", "/repos/{owner}/{repo}/pulls/comments/{comment_id}/reactions", "repo", hAddReaction("pull")},
		{"DELETE", "/repos/{owner}/{repo}/pulls/comments/{comment_id}/reactions/{reaction_id}", "repo", hDeleteReaction("pull")},
		// --- issues ---
		{"GET", "/repos/{owner}/{repo}/issues", "repo", hListIssues},
		{"POST", "/repos/{owner}/{repo}/issues", "repo", hCreateIssue},
		{"GET", "/repos/{owner}/{repo}/issues/{issue_number}", "repo", hGetIssue},
		{"PATCH", "/repos/{owner}/{repo}/issues/{issue_number}", "repo", hUpdateIssue},
		{"GET", "/repos/{owner}/{repo}/issues/{issue_number}/comments", "repo", hListIssueComments},
		{"POST", "/repos/{owner}/{repo}/issues/{issue_number}/comments", "repo", hCreateIssueComment},
		{"POST", "/repos/{owner}/{repo}/issues/{issue_number}/assignees", "repo", hAssign(true)},
		{"DELETE", "/repos/{owner}/{repo}/issues/{issue_number}/assignees", "repo", hAssign(false)},
		{"POST", "/repos/{owner}/{repo}/issues/{issue_number}/labels", "repo", hAddLabels},
		{"DELETE", "/repos/{owner}/{repo}/issues/{issue_number}/labels/{name}", "repo", hRemoveLabel},
		{"GET", "/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions", "repo", hListReactions("issue")},
		{"POST", "/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions", "repo", hAddReaction("issue")},
		{"DELETE", "/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions/{reaction_id}", "repo", hDeleteReaction("issue")},
		{"GET", "/search/issues", "any", hSearchIssues},
		// --- statuses, commits, checks ---
		{"POST", "/repos/{owner}/{repo}/statuses/{sha}", "repo", hCreateStatus},
		{"GET", "/repos/{owner}/{repo}/commits/{ref}", "repo", hGetCommit},
		{"GET", "/repos/{owner}/{repo}/commits/{ref}/status", "repo", hCombinedStatus},
		{"GET", "/repos/{owner}/{repo}/commits/{ref}/statuses", "repo", hListStatuses},
		{"GET", "/repos/{owner}/{repo}/commits/{ref}/check-runs", "repo", hCheckRunsForRef},
		// --- actions ---
		{"GET", "/repos/{owner}/{repo}/actions/runs", "repo", hListRuns},
		{"GET", "/repos/{owner}/{repo}/actions/runs/{run_id}", "repo", hGetRun},
		{"POST", "/repos/{owner}/{repo}/actions/runs/{run_id}/rerun", "repo", hRerun(false)},
		{"POST", "/repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs", "repo", hRerun(true)},
		{"POST", "/repos/{owner}/{repo}/actions/runs/{run_id}/cancel", "repo", hCancelRun},
		{"GET", "/repos/{owner}/{repo}/actions/jobs/{job_id}", "repo", hGetJob},
		{"POST", "/repos/{owner}/{repo}/actions/workflows/{workflow_id}/dispatches", "repo", hDispatch},
		// --- contents, refs, releases ---
		{"GET", "/repos/{owner}/{repo}/contents/{path}", "repo", hGetContent},
		{"PUT", "/repos/{owner}/{repo}/contents/{path}", "repo", hPutContent},
		{"DELETE", "/repos/{owner}/{repo}/contents/{path}", "repo", hDeleteContent},
		{"POST", "/repos/{owner}/{repo}/git/refs", "repo", hCreateRef},
		{"POST", "/repos/{owner}/{repo}/releases", "repo", hCreateRelease},
		{"GET", "/repos/{owner}/{repo}/releases/{release_id}", "repo", hGetRelease},
		{"POST", "/repos/{owner}/{repo}/releases/{release_id}/assets", "repo", hUploadAsset},
		// --- gists ---
		{"GET", "/gists", "user", hListGists},
		{"POST", "/gists", "user", hCreateGist},
		{"GET", "/gists/{gist_id}", "user", hGetGist},
		{"PATCH", "/gists/{gist_id}", "user", hUpdateGist},
		{"DELETE", "/gists/{gist_id}", "user", hDeleteGist},
		{"GET", "/users/{username}/gists", "user", hUserGists},
	}
}

// --- App auth -----------------------------------------------------------------

func hMintToken(c *call) (int, any) {
	in := c.f.installations[c.i64("installation_id")]
	if in == nil {
		return notFound()
	}
	tok := c.f.mintInstallationToken(in.ID)
	return 201, ShapeOf("installation-token", map[string]any{
		"token": tok, "expires_at": c.f.now().Add(time.Hour).Format(time.RFC3339),
		"permissions":          map[string]any{"contents": "write", "pull_requests": "write", "issues": "write", "checks": "write"},
		"repository_selection": "all",
	})
}

func hListInstallations(c *call) (int, any) {
	var ids []int64
	for id := range c.f.installations {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []any{}
	for _, id := range ids {
		out = append(out, c.f.installationJSON(c.f.installations[id]))
	}
	return 200, c.paginate(out)
}

func hRepoInstallation(c *call) (int, any) {
	r := c.f.repo(c.p("owner") + "/" + c.p("repo"))
	if r == nil || r.InstallationID == 0 {
		return notFound()
	}
	return 200, c.f.installationJSON(c.f.installations[r.InstallationID])
}

func hAccountInstallation(c *call) (int, any) {
	acct := c.p("org")
	if acct == "" {
		acct = c.p("username")
	}
	for _, in := range c.f.installations {
		if lower(in.Account.Login) == lower(acct) {
			if c.p("org") != "" && in.Account.Type != "Organization" {
				return notFound()
			}
			if c.p("username") != "" && in.Account.Type == "Organization" {
				return notFound()
			}
			return 200, c.f.installationJSON(in)
		}
	}
	return notFound()
}

func hInstallationRepos(c *call) (int, any) {
	if c.who.kind != "installation" {
		return ghErr(403, "This endpoint requires an installation access token")
	}
	var names []string
	for k, r := range c.f.repos {
		if r.InstallationID == c.who.inst {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	all := []any{}
	for _, n := range names {
		all = append(all, c.f.repoJSON(c.f.repos[n]))
	}
	return 200, map[string]any{"total_count": len(all), "repository_selection": "all", "repositories": c.paginate(all)}
}

func hWhoami(c *call) (int, any) {
	u := c.f.userJSON(c.actor())
	return 200, ShapeOf("public-user", merge(u, map[string]any{
		"name": c.actor().Name, "company": nil, "blog": "", "location": nil, "email": nil, "hireable": nil,
		"bio": nil, "public_repos": 0, "public_gists": 0, "followers": 0, "following": 0,
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
	}))
}

// --- pulls ---------------------------------------------------------------------

func (c *call) pull() (*Pull, bool) {
	p := c.repo.pull(c.n("pull_number"))
	return p, p != nil
}

func hListPulls(c *call) (int, any) {
	q := c.r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	out := []any{}
	nums := sortedIssueNumbers(c.repo)
	for i := len(nums) - 1; i >= 0; i-- { // newest first, as GitHub's default sort
		p := c.repo.Issues[nums[i]].Pull
		if p == nil || (state != "all" && p.Issue.State != state) {
			continue
		}
		if h := q.Get("head"); h != "" && h != c.repo.Owner.Login+":"+p.HeadRef {
			continue
		}
		if b := q.Get("base"); b != "" && b != p.BaseRef {
			continue
		}
		out = append(out, c.f.pullJSON(c.repo, p, true))
	}
	return 200, c.paginate(out)
}

func hCreatePull(c *call) (int, any) {
	head, base, title := str(c.body, "head"), str(c.body, "base"), str(c.body, "title")
	if title == "" {
		return invalid("PullRequest", "title", "missing_field", "")
	}
	if _, ok := c.repo.Branches[head]; !ok {
		return invalid("PullRequest", "head", "invalid", "")
	}
	if _, ok := c.repo.Branches[base]; !ok {
		return invalid("PullRequest", "base", "invalid", "")
	}
	for _, is := range c.repo.Issues {
		if p := is.Pull; p != nil && p.HeadRef == head && p.BaseRef == base && is.State == "open" {
			return invalid("PullRequest", "", "custom", "A pull request already exists for "+c.repo.Owner.Login+":"+head+".")
		}
	}
	draft, _ := c.body["draft"].(bool)
	p := c.f.openPull(c.repo, c.actor(), PROpts{Title: title, Body: str(c.body, "body"), Head: head, Base: base, Draft: draft})
	return 201, c.f.pullJSON(c.repo, p, false)
}

// hGetRepo answers GET /repos/{owner}/{repo}. authorize already resolved
// c.repo (404 before this handler ever runs when the repo doesn't exist, or
// an installation token can't see it), so reaching here always means
// visible.
func hGetRepo(c *call) (int, any) {
	return 200, c.f.repoJSON(c.repo)
}

func hGetPull(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	if strings.Contains(c.r.Header.Get("Accept"), "diff") {
		return 200, textBody{"text/plain; charset=utf-8", diffText(p)}
	}
	return 200, c.f.pullJSON(c.repo, p, false)
}

func diffText(p *Pull) string {
	var b strings.Builder
	for _, fl := range p.Files {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", fl.Path, fl.Path, fl.Path, fl.Path)
		for _, l := range fl.Lines {
			fmt.Fprintf(&b, "@@ -%d,0 +%d,1 @@\n+line %d\n", l, l, l)
		}
	}
	return b.String()
}

func hUpdatePull(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	if t := str(c.body, "title"); t != "" {
		p.Issue.Title = t
	}
	if b, ok := c.body["body"].(string); ok {
		p.Issue.Body = b
	}
	if b := str(c.body, "base"); b != "" {
		if _, ok := c.repo.Branches[b]; !ok {
			return invalid("PullRequest", "base", "invalid", "")
		}
		p.BaseRef = b
		p.touched()
	}
	switch s := str(c.body, "state"); s {
	case "closed":
		if p.Issue.State == "open" {
			c.f.closePull(c.repo, p, c.actor())
		}
	case "open":
		if p.Issue.State == "closed" && !p.Merged {
			p.Issue.State, p.Issue.ClosedAt = "open", time.Time{}
			c.f.emitPull(c.repo, p, "reopened", c.actor(), nil)
		}
	case "":
	default:
		return invalid("PullRequest", "state", "invalid", "")
	}
	p.Issue.UpdatedAt = c.f.now()
	return 200, c.f.pullJSON(c.repo, p, false)
}

func hPullFiles(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	out := []any{}
	for _, fl := range p.Files {
		out = append(out, ShapeOf("diff-entry", map[string]any{
			"sha": c.f.sha(fl.Path), "filename": fl.Path, "status": fl.Status, "additions": fl.Additions,
			"deletions": fl.Deletions, "changes": fl.Additions + fl.Deletions,
			"blob_url":     fmt.Sprintf("%s/%s/blob/%s/%s", htmlBase, c.repo.FullName(), p.HeadSHA, fl.Path),
			"raw_url":      fmt.Sprintf("%s/%s/raw/%s/%s", htmlBase, c.repo.FullName(), p.HeadSHA, fl.Path),
			"contents_url": c.f.api("/repos/%s/contents/%s?ref=%s", c.repo.FullName(), fl.Path, p.HeadSHA),
		}))
	}
	return 200, c.paginate(out)
}

func hPullCommits(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	var chain []*Commit
	for sha := p.HeadSHA; sha != "" && sha != p.MergeBase; {
		cm := c.repo.Commits[sha]
		if cm == nil {
			break
		}
		chain = append([]*Commit{cm}, chain...)
		if len(cm.Parents) == 0 {
			break
		}
		sha = cm.Parents[0]
	}
	out := []any{}
	for _, cm := range chain {
		out = append(out, c.f.commitJSON(c.repo, cm))
	}
	return 200, c.paginate(out)
}

func hMergePull(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	if p.Merged || p.Issue.State == "closed" {
		return ghErr(405, "Pull Request is not mergeable")
	}
	if sha := str(c.body, "sha"); sha != "" && sha != p.HeadSHA {
		return ghErr(409, "Head branch was modified. Review and try the merge again.")
	}
	if m, st := c.f.mergeState(c.repo, p); m == nil || !*m || st == "blocked" || st == "behind" || st == "draft" {
		return ghErr(405, "Pull Request is not mergeable")
	}
	sha := c.f.mergePull(c.repo, p, c.actor(), str(c.body, "commit_title"))
	return 200, ShapeOf("pull-request-merge-result", map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
}

func hGetRequested(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	teams := []any{}
	for _, t := range p.RequestedTeams {
		teams = append(teams, c.f.teamJSON(c.repo, t))
	}
	return 200, ShapeOf("pull-request-review-request", map[string]any{"users": c.f.usersJSON(p.RequestedUsers), "teams": teams})
}

func hRequestReviewers(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	users, teams := strs(c.body["reviewers"]), strs(c.body["team_reviewers"])
	if len(users) == 0 && len(teams) == 0 {
		return invalid("PullRequest", "reviewers", "missing_field", "")
	}
	for _, u := range users {
		if lower(u) == lower(p.Issue.User.Login) {
			return 422, map[string]any{"message": "Review cannot be requested from pull request author.", "documentation_url": docsURL, "status": "422"}
		}
		if acct := c.f.user(u); acct == nil || !c.repo.Collaborators[lower(u)] {
			return 422, map[string]any{"message": "Reviews may only be requested from collaborators. One or more of the users or teams you specified is not a collaborator of the " + c.repo.FullName() + " repository.", "documentation_url": docsURL, "status": "422"}
		}
	}
	for _, u := range users {
		c.f.requestReview(c.repo, p, c.actor(), u)
	}
	for _, t := range teams {
		if !contains(p.RequestedTeams, lower(t)) {
			p.RequestedTeams = append(p.RequestedTeams, lower(t))
		}
	}
	return 201, c.f.pullJSON(c.repo, p, true)
}

func hRemoveReviewers(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	for _, u := range strs(c.body["reviewers"]) {
		p.RequestedUsers = remove(p.RequestedUsers, lower(u))
	}
	for _, t := range strs(c.body["team_reviewers"]) {
		p.RequestedTeams = remove(p.RequestedTeams, lower(t))
	}
	return 200, c.f.pullJSON(c.repo, p, true)
}

func contains(xs []string, x string) bool {
	for _, e := range xs {
		if e == x {
			return true
		}
	}
	return false
}

func remove(xs []string, x string) []string {
	out := xs[:0]
	for _, e := range xs {
		if e != x {
			out = append(out, e)
		}
	}
	return out
}

func hListReviews(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	out := []any{}
	for _, rv := range p.Reviews {
		if rv.State == "PENDING" && lower(rv.User.Login) != lower(c.actor().Login) {
			continue
		}
		out = append(out, c.f.reviewJSON(c.repo, p, rv))
	}
	return 200, c.paginate(out)
}

func hCreateReview(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	event := str(c.body, "event")
	state := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED", "": "PENDING"}[event]
	if state == "" {
		return invalid("PullRequestReview", "event", "invalid", "")
	}
	body := str(c.body, "body")
	if (state == "CHANGES_REQUESTED" || state == "COMMENTED") && body == "" && c.body["comments"] == nil {
		return 422, map[string]any{"message": "Unprocessable Entity", "errors": []any{"Review body is required for " + event}, "documentation_url": docsURL, "status": "422"}
	}
	if lower(c.actor().Login) == lower(p.Issue.User.Login) && state != "COMMENTED" && state != "PENDING" {
		verb := map[string]string{"APPROVED": "approve", "CHANGES_REQUESTED": "request changes on"}[state]
		return 422, map[string]any{"message": "Unprocessable Entity", "errors": []any{"Can not " + verb + " your own pull request"}, "documentation_url": docsURL, "status": "422"}
	}
	var inline []InlineComment
	if cs, ok := c.body["comments"].([]any); ok {
		for _, e := range cs {
			m, _ := e.(map[string]any)
			ic := InlineComment{Path: str(m, "path"), Line: toI(m["line"]), Body: str(m, "body")}
			if !p.inDiff(ic.Path, ic.Line) {
				return 422, map[string]any{"message": "Unprocessable Entity", "errors": []any{"Pull request review thread line must be part of the diff"}, "documentation_url": docsURL, "status": "422"}
			}
			inline = append(inline, ic)
		}
	}
	rv := c.f.submitReview(c.repo, p, c.actor(), state, body, inline)
	return 200, c.f.reviewJSON(c.repo, p, rv)
}

func (p *Pull) inDiff(path string, line int) bool {
	for _, fl := range p.Files {
		if fl.Path != path {
			continue
		}
		for _, l := range fl.Lines {
			if l == line {
				return true
			}
		}
	}
	return false
}

func (c *call) review() (*Pull, *Review) {
	p, ok := c.pull()
	if !ok {
		return nil, nil
	}
	for _, rv := range p.Reviews {
		if rv.ID == c.i64("review_id") {
			return p, rv
		}
	}
	return p, nil
}

func hGetReview(c *call) (int, any) {
	p, rv := c.review()
	if rv == nil {
		return notFound()
	}
	return 200, c.f.reviewJSON(c.repo, p, rv)
}

func hReviewComments(c *call) (int, any) {
	p, rv := c.review()
	if rv == nil {
		return notFound()
	}
	out := []any{}
	for _, cm := range rv.Comments {
		out = append(out, c.f.reviewCommentJSON(c.repo, p, cm, "review-comment"))
	}
	return 200, c.paginate(out)
}

func hPullComments(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	cs := append([]*ReviewComment(nil), p.ReviewComments...)
	if c.r.URL.Query().Get("direction") == "desc" {
		sort.SliceStable(cs, func(i, j int) bool {
			return cs[i].CreatedAt.After(cs[j].CreatedAt) || (cs[i].CreatedAt.Equal(cs[j].CreatedAt) && cs[i].ID > cs[j].ID)
		})
	}
	out := []any{}
	for _, cm := range cs {
		if cm.ReviewID != 0 && pendingReview(p, cm.ReviewID) {
			continue
		}
		out = append(out, c.f.reviewCommentJSON(c.repo, p, cm, "pull-request-review-comment"))
	}
	return 200, c.paginate(out)
}

func pendingReview(p *Pull, id int64) bool {
	for _, rv := range p.Reviews {
		if rv.ID == id {
			return rv.State == "PENDING"
		}
	}
	return false
}

func hReply(c *call) (int, any) {
	p, ok := c.pull()
	if !ok {
		return notFound()
	}
	var parent *ReviewComment
	for _, cm := range p.ReviewComments {
		if cm.ID == c.i64("comment_id") {
			parent = cm
		}
	}
	if parent == nil {
		return notFound()
	}
	body := str(c.body, "body")
	if body == "" {
		return invalid("PullRequestReviewComment", "body", "missing_field", "")
	}
	cm := c.f.reply(c.repo, p, parent, c.actor(), body)
	return 201, c.f.reviewCommentJSON(c.repo, p, cm, "pull-request-review-comment")
}

// reactable finds the reactions list of a comment subject.
func (c *call) reactable(kind string) *[]*Reaction {
	id := c.i64("comment_id")
	for _, is := range c.repo.Issues {
		if kind == "issue" {
			for _, cm := range is.Comments {
				if cm.ID == id {
					return &cm.Reactions
				}
			}
		} else if is.Pull != nil {
			for _, cm := range is.Pull.ReviewComments {
				if cm.ID == id {
					return &cm.Reactions
				}
			}
		}
	}
	return nil
}

func hListReactions(kind string) func(*call) (int, any) {
	return func(c *call) (int, any) {
		rs := c.reactable(kind)
		if rs == nil {
			return notFound()
		}
		want := c.r.URL.Query().Get("content")
		out := []any{}
		for _, x := range *rs {
			if want == "" || x.Content == want {
				out = append(out, c.f.reactionJSON(x))
			}
		}
		return 200, c.paginate(out)
	}
}

func hAddReaction(kind string) func(*call) (int, any) {
	return func(c *call) (int, any) {
		rs := c.reactable(kind)
		if rs == nil {
			return notFound()
		}
		content := str(c.body, "content")
		if !reactionContent[content] {
			return invalid("Reaction", "content", "invalid", "")
		}
		x, created := c.f.react(rs, c.actor(), content)
		if !created {
			return 200, c.f.reactionJSON(x)
		}
		return 201, c.f.reactionJSON(x)
	}
}

func hDeleteReaction(kind string) func(*call) (int, any) {
	return func(c *call) (int, any) {
		rs := c.reactable(kind)
		if rs == nil {
			return notFound()
		}
		id := c.i64("reaction_id")
		for i, x := range *rs {
			if x.ID == id {
				if lower(x.User.Login) != lower(c.actor().Login) {
					return ghErr(403, "Must have admin rights to Repository.")
				}
				*rs = append((*rs)[:i], (*rs)[i+1:]...)
				return 204, nil
			}
		}
		return notFound()
	}
}

// --- issues --------------------------------------------------------------------

func (c *call) issue() *Issue { return c.repo.Issues[c.n("issue_number")] }

func hListIssues(c *call) (int, any) {
	q := c.r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	labels := strs(strings.Split(q.Get("labels"), ","))
	out := []any{}
	nums := sortedIssueNumbers(c.repo)
	for i := len(nums) - 1; i >= 0; i-- {
		is := c.repo.Issues[nums[i]]
		if state != "all" && is.State != state {
			continue
		}
		ok := true
		for _, l := range labels {
			if l != "" && !containsFold(is.Labels, l) {
				ok = false
			}
		}
		if a := q.Get("assignee"); a != "" && a != "*" && !containsFold(is.Assignees, a) {
			ok = false
		}
		if ok {
			out = append(out, c.f.issueJSON(c.repo, is)) // PRs included, as GitHub's issues list does
		}
	}
	return 200, c.paginate(out)
}

func containsFold(xs []string, x string) bool {
	for _, e := range xs {
		if strings.EqualFold(e, x) {
			return true
		}
	}
	return false
}

func hCreateIssue(c *call) (int, any) {
	title := str(c.body, "title")
	if title == "" {
		return invalid("Issue", "title", "missing_field", "")
	}
	is := c.f.openIssue(c.repo, c.actor(), IssueOpts{Title: title, Body: str(c.body, "body"), Labels: strs(c.body["labels"]), Assignees: strs(c.body["assignees"])})
	return 201, c.f.issueJSON(c.repo, is)
}

func hGetIssue(c *call) (int, any) {
	is := c.issue()
	if is == nil {
		return notFound()
	}
	return 200, c.f.issueJSON(c.repo, is)
}

func hUpdateIssue(c *call) (int, any) {
	is := c.issue()
	if is == nil {
		return notFound()
	}
	if t := str(c.body, "title"); t != "" {
		is.Title = t
	}
	if b, ok := c.body["body"].(string); ok {
		is.Body = b
	}
	if ls, ok := c.body["labels"]; ok {
		is.Labels = strs(ls)
	}
	switch s := str(c.body, "state"); s {
	case "closed":
		if is.State == "open" {
			if is.Pull != nil {
				c.f.closePull(c.repo, is.Pull, c.actor())
			} else {
				is.State, is.ClosedAt = "closed", c.f.now()
				is.StateReason = str(c.body, "state_reason")
				if is.StateReason == "" {
					is.StateReason = "completed"
				}
				c.f.emitIssue(c.repo, is, "closed", c.actor(), nil)
			}
		}
	case "open":
		if is.State == "closed" {
			is.State, is.ClosedAt, is.StateReason = "open", time.Time{}, "reopened"
			c.f.emitIssue(c.repo, is, "reopened", c.actor(), nil)
		}
	case "":
	default:
		return invalid("Issue", "state", "invalid", "")
	}
	is.UpdatedAt = c.f.now()
	return 200, c.f.issueJSON(c.repo, is)
}

func hListIssueComments(c *call) (int, any) {
	is := c.issue()
	if is == nil {
		return notFound()
	}
	cs := append([]*IssueComment(nil), is.Comments...)
	if c.r.URL.Query().Get("direction") == "desc" {
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].ID > cs[j].ID })
	}
	out := []any{}
	for _, cm := range cs {
		out = append(out, c.f.issueCommentJSON(c.repo, cm))
	}
	return 200, c.paginate(out)
}

func hCreateIssueComment(c *call) (int, any) {
	is := c.issue()
	if is == nil {
		return notFound()
	}
	body := str(c.body, "body")
	if body == "" {
		return invalid("IssueComment", "body", "missing_field", "")
	}
	cm := c.f.comment(c.repo, is, c.actor(), body)
	return 201, c.f.issueCommentJSON(c.repo, cm)
}

func hAssign(add bool) func(*call) (int, any) {
	return func(c *call) (int, any) {
		is := c.issue()
		if is == nil {
			return notFound()
		}
		for _, a := range strs(c.body["assignees"]) {
			if add {
				if c.f.user(a) != nil && !containsFold(is.Assignees, a) {
					c.f.assign(c.repo, is, c.actor(), a)
				}
			} else {
				out := is.Assignees[:0]
				for _, e := range is.Assignees {
					if !strings.EqualFold(e, a) {
						out = append(out, e)
					}
				}
				is.Assignees = out
			}
		}
		if add {
			return 201, c.f.issueJSON(c.repo, is)
		}
		return 200, c.f.issueJSON(c.repo, is)
	}
}

func hAddLabels(c *call) (int, any) {
	is := c.issue()
	if is == nil {
		return notFound()
	}
	ls := strs(c.body["labels"])
	if len(ls) == 0 {
		return invalid("Label", "labels", "missing_field", "")
	}
	for _, l := range ls {
		c.f.addLabel(c.repo, is, c.actor(), l)
	}
	return 200, c.f.labelsJSON(c.repo, is.Labels)
}

func hRemoveLabel(c *call) (int, any) {
	is := c.issue()
	if is == nil {
		return notFound()
	}
	name := c.p("name")
	if !containsFold(is.Labels, name) {
		return ghErr(404, "Label does not exist")
	}
	out := is.Labels[:0]
	for _, l := range is.Labels {
		if !strings.EqualFold(l, name) {
			out = append(out, l)
		}
	}
	is.Labels = out
	return 200, c.f.labelsJSON(c.repo, is.Labels)
}

func hSearchIssues(c *call) (int, any) {
	q := c.r.URL.Query().Get("q")
	if q == "" {
		return invalid("Search", "q", "missing", "")
	}
	var repo *Repo
	var terms []string
	isPR, isIssue := false, false
	for _, t := range strings.Fields(q) {
		switch {
		case strings.HasPrefix(t, "repo:"):
			repo = c.f.repo(strings.TrimPrefix(t, "repo:"))
		case t == "is:pr" || t == "type:pr":
			isPR = true
		case t == "is:issue" || t == "type:issue":
			isIssue = true
		case strings.HasPrefix(t, "is:") || strings.Contains(t, ":"):
		default:
			terms = append(terms, lower(t))
		}
	}
	items := []any{}
	if repo != nil {
		for _, n := range sortedIssueNumbers(repo) {
			is := repo.Issues[n]
			if (isPR && is.Pull == nil) || (isIssue && is.Pull != nil) {
				continue
			}
			hay := lower(is.Title + " " + is.Body)
			ok := true
			for _, t := range terms {
				if !strings.Contains(hay, t) {
					ok = false
				}
			}
			if ok {
				items = append(items, ShapeOf("issue-search-result-item", merge(c.f.issueJSON(repo, is), map[string]any{"score": 1, "body": is.Body})))
			}
		}
	}
	return 200, map[string]any{"total_count": len(items), "incomplete_results": false, "items": c.paginate(items)}
}

// --- statuses, commits, checks ---------------------------------------------------

func (c *call) resolveRef(ref string) string {
	if sha, ok := c.repo.Branches[strings.TrimPrefix(ref, "heads/")]; ok {
		return sha
	}
	if _, ok := c.repo.Commits[ref]; ok {
		return ref
	}
	return ""
}

func hCreateStatus(c *call) (int, any) {
	sha := c.p("sha")
	if _, ok := c.repo.Commits[sha]; !ok {
		return invalid("Status", "sha", "invalid", "No commit found for SHA: "+sha)
	}
	state := str(c.body, "state")
	switch state {
	case "error", "failure", "pending", "success":
	default:
		return invalid("Status", "state", "custom", "state is not included in the list")
	}
	desc := str(c.body, "description")
	if len([]rune(desc)) > 140 {
		return invalid("Status", "description", "custom", "description is too long (maximum is 140 characters)")
	}
	ctxName := str(c.body, "context")
	if ctxName == "" {
		ctxName = "default"
	}
	s := c.f.setStatus(c.repo, sha, c.actor(), state, ctxName, desc, str(c.body, "target_url"))
	return 201, c.f.statusJSON(c.repo, s)
}

func hGetCommit(c *call) (int, any) {
	sha := c.resolveRef(c.p("ref"))
	if sha == "" {
		return invalid("Commit", "sha", "invalid", "No commit found for SHA: "+c.p("ref"))
	}
	return 200, c.f.commitJSON(c.repo, c.repo.Commits[sha])
}

func hCombinedStatus(c *call) (int, any) {
	sha := c.resolveRef(c.p("ref"))
	if sha == "" {
		return notFound()
	}
	latest := latestStatuses(c.repo, sha)
	out := []any{}
	for _, s := range latest {
		out = append(out, ShapeOf("simple-commit-status", c.f.statusJSON(c.repo, s)))
	}
	return 200, ShapeOf("combined-commit-status", map[string]any{
		"state": combinedState(latest), "sha": sha, "total_count": len(latest), "statuses": out,
		"repository": c.f.minimalRepoJSON(c.repo), "commit_url": c.f.api("/repos/%s/commits/%s", c.repo.FullName(), sha),
		"url": c.f.api("/repos/%s/commits/%s/status", c.repo.FullName(), sha),
	})
}

func hListStatuses(c *call) (int, any) {
	sha := c.resolveRef(c.p("ref"))
	if sha == "" {
		return notFound()
	}
	out := []any{}
	for i := len(c.repo.Statuses) - 1; i >= 0; i-- { // newest first
		if s := c.repo.Statuses[i]; s.SHA == sha {
			out = append(out, c.f.statusJSON(c.repo, s))
		}
	}
	return 200, c.paginate(out)
}

func hCheckRunsForRef(c *call) (int, any) {
	sha := c.resolveRef(c.p("ref"))
	if sha == "" {
		return notFound()
	}
	latest := map[string]*CheckRun{}
	var order []string
	for _, cr := range c.repo.CheckRuns { // filter=latest is GitHub's default
		if cr.HeadSHA != sha {
			continue
		}
		if _, seen := latest[cr.Name]; !seen {
			order = append(order, cr.Name)
		}
		latest[cr.Name] = cr
	}
	out := []any{}
	for _, n := range order {
		out = append(out, c.f.checkRunJSON(c.repo, latest[n]))
	}
	return 200, map[string]any{"total_count": len(out), "check_runs": c.paginate(out)}
}

// --- actions -------------------------------------------------------------------

func hListRuns(c *call) (int, any) {
	q := c.r.URL.Query()
	out := []any{}
	for i := len(c.repo.Runs) - 1; i >= 0; i-- {
		w := c.repo.Runs[i]
		if v := q.Get("head_sha"); v != "" && w.HeadSHA != v {
			continue
		}
		if v := q.Get("branch"); v != "" && w.HeadBranch != v {
			continue
		}
		if v := q.Get("status"); v != "" && w.Status != v && w.Conclusion != v {
			continue
		}
		if v := q.Get("check_suite_id"); v != "" && strconv.FormatInt(w.SuiteID, 10) != v {
			continue
		}
		out = append(out, c.f.runJSON(c.repo, w))
	}
	return 200, map[string]any{"total_count": len(out), "workflow_runs": c.paginate(out)}
}

func (c *call) run() *WorkflowRun {
	for _, w := range c.repo.Runs {
		if w.ID == c.i64("run_id") {
			return w
		}
	}
	return nil
}

func hGetRun(c *call) (int, any) {
	w := c.run()
	if w == nil {
		return notFound()
	}
	return 200, c.f.runJSON(c.repo, w)
}

func hRerun(failedOnly bool) func(*call) (int, any) {
	return func(c *call) (int, any) {
		w := c.run()
		if w == nil {
			return notFound()
		}
		if w.Status != "completed" {
			return ghErr(403, "This workflow is already running")
		}
		c.f.rerun(c.repo, w, failedOnly, c.actor())
		return 201, map[string]any{}
	}
}

func hCancelRun(c *call) (int, any) {
	w := c.run()
	if w == nil {
		return notFound()
	}
	if w.Status == "completed" {
		return ghErr(409, "Cannot cancel a workflow run that is completed.")
	}
	c.f.completeRun(c.repo, w, "cancelled")
	return 202, map[string]any{}
}

func hGetJob(c *call) (int, any) {
	for _, j := range c.repo.Jobs {
		if j.ID == c.i64("job_id") {
			return 200, c.f.jobJSON(c.repo, j)
		}
	}
	return notFound()
}

func hDispatch(c *call) (int, any) {
	ref := str(c.body, "ref")
	if ref == "" {
		return invalid("WorkflowDispatch", "ref", "missing_field", "")
	}
	sha := c.resolveRef(ref)
	if sha == "" {
		return invalid("WorkflowDispatch", "ref", "invalid", "No ref found for: "+ref)
	}
	c.f.startRun(c.repo, sha, ref, c.p("workflow_id"), "workflow_dispatch")
	return 204, nil
}

// --- contents, refs, releases ------------------------------------------------------

func hGetContent(c *call) (int, any) {
	ct := c.repo.Contents[c.p("path")]
	if ct == nil {
		return notFound()
	}
	if strings.Contains(c.r.Header.Get("Accept"), "raw") {
		return 200, textBody{"text/plain; charset=utf-8", ct.Text}
	}
	return 200, c.f.contentJSON(c.repo, ct)
}

func (f *Fake) contentJSON(r *Repo, ct *Content) map[string]any {
	api := f.api("/repos/%s/contents/%s", r.FullName(), ct.Path)
	html := fmt.Sprintf("%s/%s/blob/%s/%s", htmlBase, r.FullName(), r.DefaultBranch, ct.Path)
	return ShapeOf("content-file", map[string]any{
		"type": "file", "encoding": "base64", "size": len(ct.Text), "name": ct.Path[strings.LastIndex(ct.Path, "/")+1:],
		"path": ct.Path, "content": b64(ct.Text), "sha": ct.SHA, "url": api, "git_url": api, "html_url": html,
		"download_url": html, "_links": map[string]any{"self": api, "git": api, "html": html},
	})
}

func hPutContent(c *call) (int, any) {
	path, msg := c.p("path"), str(c.body, "message")
	if msg == "" {
		return invalid("Content", "message", "missing_field", "")
	}
	text, err := unb64(str(c.body, "content"))
	if err != nil {
		return invalid("Content", "content", "invalid", "content is not valid Base64")
	}
	old := c.repo.Contents[path]
	if old != nil && str(c.body, "sha") != old.SHA {
		return 409, map[string]any{"message": path + " does not match " + str(c.body, "sha"), "documentation_url": docsURL, "status": "409"}
	}
	if old == nil && str(c.body, "sha") != "" {
		return 422, map[string]any{"message": "Invalid request.\n\n\"sha\" wasn't supplied.", "documentation_url": docsURL, "status": "422"}
	}
	ct := &Content{Path: path, Text: text, SHA: c.f.sha(path)}
	c.repo.Contents[path] = ct
	cm := c.f.commitTo(c.repo, c.repo.DefaultBranch, c.actor(), msg)
	status := 201
	if old != nil {
		status = 200
	}
	return status, ShapeOf("file-commit", map[string]any{"content": c.f.contentJSON(c.repo, ct), "commit": c.f.gitCommitJSON(c.repo, cm)})
}

func hDeleteContent(c *call) (int, any) {
	path := c.p("path")
	old := c.repo.Contents[path]
	if old == nil {
		return notFound()
	}
	if str(c.body, "sha") != old.SHA {
		return 409, map[string]any{"message": path + " does not match " + str(c.body, "sha"), "documentation_url": docsURL, "status": "409"}
	}
	delete(c.repo.Contents, path)
	cm := c.f.commitTo(c.repo, c.repo.DefaultBranch, c.actor(), str(c.body, "message"))
	return 200, ShapeOf("file-commit", map[string]any{"content": nil, "commit": c.f.gitCommitJSON(c.repo, cm)})
}

func (f *Fake) gitCommitJSON(r *Repo, cm *Commit) map[string]any {
	who := map[string]any{"name": cm.Author.Login, "email": cm.Author.Email, "date": ts(cm.At)}
	return map[string]any{"sha": cm.SHA, "node_id": nodeID("C", f.id()), "url": f.api("/repos/%s/git/commits/%s", r.FullName(), cm.SHA),
		"html_url": fmt.Sprintf("%s/%s/commit/%s", htmlBase, r.FullName(), cm.SHA), "author": who, "committer": who,
		"message": cm.Message, "tree": map[string]any{"sha": cm.SHA, "url": f.api("/repos/%s/git/trees/%s", r.FullName(), cm.SHA)},
		"parents": []any{}, "verification": map[string]any{"verified": false, "reason": "unsigned", "signature": nil, "payload": nil, "verified_at": nil}}
}

func hCreateRef(c *call) (int, any) {
	ref, sha := str(c.body, "ref"), str(c.body, "sha")
	if !strings.HasPrefix(ref, "refs/") || strings.Count(ref, "/") < 2 {
		return invalid("Reference", "ref", "invalid", "ref must start with 'refs/' and have at least two slashes.")
	}
	if _, ok := c.repo.Commits[sha]; !ok {
		return invalid("Reference", "sha", "invalid", "Object does not exist")
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if _, exists := c.repo.Branches[branch]; exists {
		return invalid("Reference", "ref", "already_exists", "Reference already exists")
	}
	c.repo.Branches[branch] = sha
	return 201, ShapeOf("git-ref", map[string]any{"ref": ref, "node_id": nodeID("REF", c.f.id()),
		"url":    c.f.api("/repos/%s/git/%s", c.repo.FullName(), ref),
		"object": map[string]any{"type": "commit", "sha": sha, "url": c.f.api("/repos/%s/git/commits/%s", c.repo.FullName(), sha)}})
}

func hCreateRelease(c *call) (int, any) {
	tag := str(c.body, "tag_name")
	if tag == "" {
		return invalid("Release", "tag_name", "missing_field", "")
	}
	for _, rel := range c.repo.Releases {
		if rel.TagName == tag {
			return invalid("Release", "tag_name", "already_exists", "")
		}
	}
	draft, _ := c.body["draft"].(bool)
	pre, _ := c.body["prerelease"].(bool)
	rel := c.f.publishRelease(c.repo, c.actor(), ReleaseOpts{Tag: tag, Name: str(c.body, "name"), Body: str(c.body, "body"), Target: str(c.body, "target_commitish"), Draft: draft, Prerelease: pre})
	return 201, c.f.releaseJSON(c.repo, rel)
}

func hGetRelease(c *call) (int, any) {
	for _, rel := range c.repo.Releases {
		if rel.ID == c.i64("release_id") {
			return 200, c.f.releaseJSON(c.repo, rel)
		}
	}
	return notFound()
}

func hUploadAsset(c *call) (int, any) {
	var rel *Release
	for _, x := range c.repo.Releases {
		if x.ID == c.i64("release_id") {
			rel = x
		}
	}
	if rel == nil {
		return notFound()
	}
	name := c.r.URL.Query().Get("name")
	if name == "" {
		return invalid("ReleaseAsset", "name", "missing_field", "")
	}
	id := c.f.id()
	return 201, ShapeOf("release-asset", map[string]any{
		"url": c.f.api("/repos/%s/releases/assets/%d", c.repo.FullName(), id), "id": id, "node_id": nodeID("RA", id),
		"name": name, "label": nil, "state": "uploaded", "content_type": c.r.Header.Get("Content-Type"), "size": len(c.raw),
		"download_count": 0, "created_at": ts(c.f.now()), "updated_at": ts(c.f.now()), "uploader": c.f.userJSON(c.actor()),
		"browser_download_url": fmt.Sprintf("%s/%s/releases/download/%s/%s", htmlBase, c.repo.FullName(), rel.TagName, name),
		"digest":               nil,
	})
}

func b64(s string) string { return encodeB64(s) }

// --- small helpers --------------------------------------------------------------

func readJSONBody(r io.Reader) map[string]any {
	var m map[string]any
	b, _ := io.ReadAll(r)
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}
	}
	_ = json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}
