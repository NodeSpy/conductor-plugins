package ghfake

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// The admin API: the scenario API and the state queries over HTTP, under
// /_fake/, for a harness that runs the fake out of process (conductor's
// dockerized e2e). It needs no credentials — it is the test's side of the
// glass, never GitHub's.
//
//	GET  /_fake/health                       {"ok": true}
//	GET  /_fake/unhandled                    requests the fake answered 501
//	GET  /_fake/deliveries                   every delivery attempt
//	POST /_fake/seed      <Seed>             build the world
//	POST /_fake/act       {"op": …, …}       one scenario step (see act)
//	GET  /_fake/q?what=…&…                   one state query (see query)

// Seed describes a world to build.
type Seed struct {
	App *struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
		// PrivateKeyPath, when set, is the App's PEM key: JWTs must be
		// signed with it.
		PrivateKeyPath string `json:"private_key_path"`
	} `json:"app"`
	Users []struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"users"`
	// Tokens maps a personal access token to the login it acts as.
	Tokens   map[string]string `json:"tokens"`
	Installs []struct {
		Account string   `json:"account"`
		Repos   []string `json:"repos"`
	} `json:"installs"`
	Repos []struct {
		Name          string      `json:"name"`
		Protection    *Protection `json:"protection"`
		Collaborators []string    `json:"collaborators"`
	} `json:"repos"`
	Hooks []struct {
		URL    string   `json:"url"`
		Secret string   `json:"secret"`
		Repos  []string `json:"repos"`
	} `json:"hooks"`
	PRs []SeedPR `json:"prs"`
}

// SeedPR is one PR in a Seed. They open in order, so a repo's first PR is
// number 1. Opening a seeded PR delivers no webhook.
type SeedPR struct {
	Repo     string   `json:"repo"`
	Author   string   `json:"author"`
	Head     string   `json:"head"`
	Base     string   `json:"base"`
	Title    string   `json:"title"`
	Draft    bool     `json:"draft"`
	Conflict bool     `json:"conflict"`
	Behind   bool     `json:"behind"`
	Labels   []string `json:"labels"`
	// Reviewers have a pending review request.
	Reviewers []string `json:"reviewers"`
	// Approved by these logins (submitted APPROVED reviews).
	ApprovedBy []string `json:"approved_by"`
}

// LoadSeed builds the world a Seed describes.
func (f *Fake) LoadSeed(s Seed) error {
	for _, u := range s.Users {
		f.AddUser(u.Login, u.Type)
	}
	if s.App != nil {
		var pub *rsa.PublicKey
		if s.App.PrivateKeyPath != "" {
			k, err := readRSAKey(s.App.PrivateKeyPath)
			if err != nil {
				return err
			}
			pub = &k.PublicKey
		}
		f.SetApp(s.App.ID, s.App.Slug, pub)
	}
	for tok, login := range s.Tokens {
		f.UserToken(login, tok)
	}
	for _, r := range s.Repos {
		f.AddRepo(r.Name)
		if r.Protection != nil {
			f.SetProtection(r.Name, *r.Protection)
		}
		for _, c := range r.Collaborators {
			f.AddCollaborator(r.Name, c)
		}
	}
	for _, in := range s.Installs {
		f.Install(in.Account, in.Repos...)
	}
	// Seeded PRs exist before any hook: they deliver nothing.
	for _, p := range s.PRs {
		f.AddRepo(p.Repo)
		n := f.OpenPR(p.Repo, p.Author, PROpts{Title: p.Title, Head: p.Head, Base: p.Base, Draft: p.Draft, Labels: p.Labels})
		if p.Conflict {
			f.SetConflict(p.Repo, n, true)
		}
		if p.Behind {
			base := p.Base
			if base == "" {
				base = "main"
			}
			f.mu.Lock()
			r := f.repo(p.Repo)
			f.commitTo(r, base, r.Owner, "base moves on")
			r.Issues[n].Pull.touched()
			f.mu.Unlock()
		}
		for _, rv := range p.Reviewers {
			f.mu.Lock()
			r := f.repo(p.Repo)
			f.addUser(rv, "")
			r.Collaborators[lower(rv)] = true
			pr := r.Issues[n].Pull
			pr.RequestedUsers = append(pr.RequestedUsers, lower(rv))
			f.mu.Unlock()
		}
		for _, a := range p.ApprovedBy {
			f.mu.Lock()
			r := f.repo(p.Repo)
			pr := r.Issues[n].Pull
			pr.Reviews = append(pr.Reviews, &Review{ID: f.id(), User: f.addUser(a, ""), State: "APPROVED", CommitID: pr.HeadSHA, SubmittedAt: f.now()})
			f.mu.Unlock()
		}
	}
	f.Flush()
	for _, h := range s.Hooks {
		f.AddHook(h.URL, h.Secret, h.Repos...)
	}
	return nil
}

func readRSAKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an RSA key", path)
	}
	return rk, nil
}

func (f *Fake) admin(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if e := recover(); e != nil {
			writeJSON(w, 400, map[string]any{"error": fmt.Sprint(e)})
		}
	}()
	switch r.URL.Path {
	case "/_fake/health":
		writeJSON(w, 200, map[string]any{"ok": true})
	case "/_fake/unhandled":
		writeJSON(w, 200, f.Unhandled())
	case "/_fake/deliveries":
		var out []map[string]any
		for _, d := range f.Deliveries() {
			out = append(out, map[string]any{"guid": d.GUID, "event": d.Event, "action": d.Action, "repo": d.Repo,
				"url": d.URL, "status": d.Status, "error": d.Err, "attempt": d.Attempt})
		}
		writeJSON(w, 200, out)
	case "/_fake/seed":
		var s Seed
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		if err := f.LoadSeed(s); err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case "/_fake/act":
		out, err := f.act(readJSONBody(r.Body))
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, out)
	case "/_fake/q":
		out, err := f.query(r)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, out)
	default:
		writeJSON(w, 404, map[string]any{"error": "no admin route " + r.URL.Path})
	}
}

// act runs one scenario step from JSON. Field names follow the Go methods.
func (f *Fake) act(a map[string]any) (map[string]any, error) {
	repo, by := str(a, "repo"), str(a, "by")
	n := toI(a["number"])
	inline := func() []InlineComment {
		var out []InlineComment
		if cs, ok := a["comments"].([]any); ok {
			for _, c := range cs {
				m, _ := c.(map[string]any)
				out = append(out, InlineComment{Path: str(m, "path"), Line: toI(m["line"]), Body: str(m, "body")})
			}
		}
		return out
	}
	switch op := str(a, "op"); op {
	case "open_pr":
		num := f.OpenPR(repo, by, PROpts{Title: str(a, "title"), Head: str(a, "head"), Base: str(a, "base"), Draft: a["draft"] == true, Labels: strs(a["labels"])})
		return map[string]any{"number": num, "head_sha": f.HeadSHA(repo, num)}, nil
	case "push":
		return map[string]any{"sha": f.Push(repo, str(a, "branch"), by, str(a, "message"), strs(a["paths"])...)}, nil
	case "push_sha":
		f.PushSHA(repo, str(a, "branch"), by, str(a, "sha"))
	case "set_conflict":
		f.SetConflict(repo, n, a["conflict"] != false)
	case "comment":
		return map[string]any{"id": f.Comment(repo, n, by, str(a, "body"))}, nil
	case "review":
		id, cids := f.SubmitReview(repo, n, by, str(a, "state"), str(a, "body"), inline()...)
		return map[string]any{"id": id, "comment_ids": cids}, nil
	case "edit_review":
		f.EditReview(repo, n, int64(toI(a["review_id"])), str(a, "body"))
	case "reply":
		return map[string]any{"id": f.Reply(repo, n, int64(toI(a["comment_id"])), by, str(a, "body"))}, nil
	case "resolve_thread":
		f.ResolveThread(repo, n, int64(toI(a["comment_id"])), by, a["resolved"] != false)
	case "request_review":
		f.RequestReview(repo, n, by, str(a, "reviewer"))
	case "check":
		sha := str(a, "sha")
		if sha == "" {
			sha = f.HeadSHA(repo, n)
		}
		run, check := f.Check(repo, sha, str(a, "name"), str(a, "conclusion"))
		return map[string]any{"run_id": run, "check_id": check}, nil
	case "start_check":
		sha := str(a, "sha")
		if sha == "" {
			sha = f.HeadSHA(repo, n)
		}
		ago, _ := time.ParseDuration(str(a, "started_ago"))
		return map[string]any{"run_id": f.StartCheck(repo, sha, str(a, "name"), time.Now().Add(-ago))}, nil
	case "status":
		sha := str(a, "sha")
		if sha == "" {
			sha = f.HeadSHA(repo, n)
		}
		f.Status(repo, sha, by, str(a, "state"), str(a, "context"), str(a, "description"))
	case "merge":
		return map[string]any{"sha": f.Merge(repo, n, by)}, nil
	case "close":
		f.CloseIssue(repo, n, by)
	case "set_draft":
		f.SetDraft(repo, n, by, a["draft"] == true)
	case "label":
		f.Label(repo, n, by, str(a, "label"))
	case "open_issue":
		return map[string]any{"number": f.OpenIssue(repo, by, IssueOpts{Title: str(a, "title"), Body: str(a, "body"), Labels: strs(a["labels"]), Assignees: strs(a["assignees"])})}, nil
	case "assign":
		f.Assign(repo, n, by, str(a, "assignee"))
	case "release":
		return map[string]any{"id": f.Release(repo, by, ReleaseOpts{Tag: str(a, "tag"), Prerelease: a["prerelease"] == true})}, nil
	case "redeliver":
		return map[string]any{"ok": f.Redeliver(str(a, "guid"))}, nil
	case "flush":
		f.Flush()
	default:
		return nil, fmt.Errorf("unknown op %q", op)
	}
	return map[string]any{"ok": true}, nil
}

// query answers one state question.
func (f *Fake) query(r *http.Request) (any, error) {
	q := r.URL.Query()
	repo := q.Get("repo")
	n, _ := strconv.Atoi(q.Get("number"))
	switch what := q.Get("what"); what {
	case "pr":
		p := f.PR(repo, n)
		return map[string]any{"head_sha": p.HeadSHA, "head_ref": p.HeadRef, "merged": p.Merged, "state": p.Issue.State,
			"draft": p.Draft, "requested_reviewers": f.RequestedReviewers(repo, n)}, nil
	case "statuses":
		sha := q.Get("sha")
		if sha == "" {
			sha = f.HeadSHA(repo, n)
		}
		return f.Statuses(repo, sha), nil
	case "reactions":
		id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
		return f.Reactions(repo, q.Get("kind"), id), nil
	case "comments":
		return f.Comments(repo, n), nil
	case "review_requests":
		return map[string]any{"count": f.ReviewRequestsOf(repo, n, q.Get("reviewer"))}, nil
	case "run_attempts":
		id, _ := strconv.ParseInt(q.Get("run_id"), 10, 64)
		return map[string]any{"attempts": f.RunAttempts(repo, id)}, nil
	}
	return nil, fmt.Errorf("unknown query %q", q.Get("what"))
}
