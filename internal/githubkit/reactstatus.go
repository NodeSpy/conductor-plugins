package githubkit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Reaction subject kinds — what a `react` call points at. Each names one
// GitHub object with its own id sequence: a PR conversation comment, an
// inline review (diff) comment, or a submitted review.
const (
	SubjectIssueComment  = "issue_comment"
	SubjectReviewComment = "review_comment"
	SubjectReview        = "review"
)

// reactionContents are the REST reaction names `react` accepts, mapped to
// their GraphQL ReactionContent spelling (reviews react over GraphQL only:
// REST has no reactions endpoint for a PullRequestReview).
var reactionContents = map[string]string{
	"+1": "THUMBS_UP", "-1": "THUMBS_DOWN", "laugh": "LAUGH", "confused": "CONFUSED",
	"heart": "HEART", "hooray": "HOORAY", "rocket": "ROCKET", "eyes": "EYES",
}

// ReactionContents lists the reaction names `react` accepts (for the verb's
// option enum).
func ReactionContents() []string {
	return []string{"+1", "-1", "laugh", "confused", "heart", "hooray", "rocket", "eyes"}
}

// StatusStates lists the commit-status states `set_status` accepts.
func StatusStates() []string { return []string{"pending", "success", "failure", "error"} }

// maxStatusDescription is GitHub's limit on a commit status description; a
// longer one 422s the whole call.
const maxStatusDescription = 140

// react adds one reaction to each subject. Reacting is idempotent on GitHub's
// side — re-adding a reaction you already left returns the existing one (REST
// 200 instead of 201; GraphQL returns the same reaction) — so a retried run
// never stacks duplicates. Every subject is attempted; the errors of the
// ones that failed are joined.
func (c *Client) react(ctx context.Context, tok, base, repo string, number int, opts map[string]any) (map[string]any, error) {
	content, _ := opts["content"].(string)
	gqlContent, ok := reactionContents[content]
	if !ok {
		return nil, fmt.Errorf("github.react: options.content must be one of %s, got %q",
			strings.Join(ReactionContents(), "|"), content)
	}
	subjects := reactionSubjects(opts)
	if len(subjects) == 0 {
		return nil, fmt.Errorf("github.react: set options.subjects (or kind + id)")
	}
	if optBool(opts, "remove", false) {
		return c.unreact(ctx, tok, base, repo, number, subjects, content, gqlContent)
	}
	var errs []error
	n := 0
	for _, s := range subjects {
		var err error
		switch s.Kind {
		case SubjectIssueComment:
			err = c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/issues/comments/%d/reactions", base, repo, s.ID),
				map[string]any{"content": content}, nil)
		case SubjectReviewComment:
			err = c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/comments/%d/reactions", base, repo, s.ID),
				map[string]any{"content": content}, nil)
		case SubjectReview:
			err = c.reactReview(ctx, tok, base, repo, number, s.ID, gqlContent)
		default:
			err = fmt.Errorf("unknown subject kind %q (want %s|%s|%s)", s.Kind, SubjectIssueComment, SubjectReviewComment, SubjectReview)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %d: %w", s.Kind, s.ID, err))
			continue
		}
		n++
	}
	if len(errs) > 0 {
		return map[string]any{"ok": false, "reacted": n}, fmt.Errorf("github.react: %w", errors.Join(errs...))
	}
	return map[string]any{"ok": true, "reacted": n}, nil
}

// reactReview reacts to a submitted review. A PullRequestReview is Reactable
// only through GraphQL (addReaction), which wants its node id — read from the
// REST review, the one place a numeric review id resolves.
func (c *Client) reactReview(ctx context.Context, tok, base, repo string, number int, reviewID int64, content string) error {
	if number == 0 {
		return fmt.Errorf("a review subject needs options.pr")
	}
	var rv struct {
		NodeID string `json:"node_id"`
	}
	if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d/reviews/%d", base, repo, number, reviewID), &rv); err != nil {
		return err
	}
	if rv.NodeID == "" {
		return fmt.Errorf("review has no node id")
	}
	return c.graphql(ctx, tok,
		"mutation($id:ID!,$c:ReactionContent!){addReaction(input:{subjectId:$id,content:$c}){reaction{content}}}",
		map[string]any{"id": rv.NodeID, "c": content}, nil)
}

// unreact removes the acting user's reaction of `content` from each subject —
// only theirs, never anyone else's — and is idempotent: a subject that has no
// such reaction (never added, or already removed) is a no-op, not an error.
// Comments go through REST (find your reaction's id among that content's,
// then DELETE it); a review, which REST can't reach, through GraphQL
// removeReaction, which removes the viewer's own.
func (c *Client) unreact(ctx context.Context, tok, base, repo string, number int, subjects []ReactionSubject, content, gqlContent string) (map[string]any, error) {
	var errs []error
	removed := 0
	for _, s := range subjects {
		var n int
		var err error
		switch s.Kind {
		case SubjectIssueComment:
			n, err = c.unreactREST(ctx, tok, fmt.Sprintf("%s/repos/%s/issues/comments/%d/reactions", base, repo, s.ID), content)
		case SubjectReviewComment:
			n, err = c.unreactREST(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/comments/%d/reactions", base, repo, s.ID), content)
		case SubjectReview:
			n, err = c.unreactReview(ctx, tok, base, repo, number, s.ID, gqlContent)
		default:
			err = fmt.Errorf("unknown subject kind %q (want %s|%s|%s)", s.Kind, SubjectIssueComment, SubjectReviewComment, SubjectReview)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %d: %w", s.Kind, s.ID, err))
			continue
		}
		removed += n
	}
	if len(errs) > 0 {
		return map[string]any{"ok": false, "removed": removed}, fmt.Errorf("github.react: remove: %w", errors.Join(errs...))
	}
	return map[string]any{"ok": true, "removed": removed}, nil
}

// unreactREST deletes the acting user's `content` reaction listed at
// reactionsURL (a comment's reactions collection). Read fresh: a reaction
// added seconds ago must be found.
func (c *Client) unreactREST(ctx context.Context, tok, reactionsURL, content string) (int, error) {
	login, err := c.Login(ctx, tok)
	if err != nil {
		return 0, err
	}
	var rs []struct {
		ID   int64 `json:"id"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := c.getFresh(ctx, tok, reactionsURL+"?per_page=100&content="+url.QueryEscape(content), &rs); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rs {
		if !strings.EqualFold(r.User.Login, login) {
			continue
		}
		if err := c.del(ctx, tok, fmt.Sprintf("%s/%d", reactionsURL, r.ID), nil); err != nil && !isNotFound(err) {
			return n, err
		}
		n++
	}
	return n, nil
}

// unreactReview removes the viewer's `content` reaction from a review.
func (c *Client) unreactReview(ctx context.Context, tok, base, repo string, number int, reviewID int64, content string) (int, error) {
	if number == 0 {
		return 0, fmt.Errorf("a review subject needs options.pr")
	}
	var rv struct {
		NodeID string `json:"node_id"`
	}
	if err := c.get(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d/reviews/%d", base, repo, number, reviewID), &rv); err != nil {
		return 0, err
	}
	if rv.NodeID == "" {
		return 0, fmt.Errorf("review has no node id")
	}
	err := c.graphql(ctx, tok,
		"mutation($id:ID!,$c:ReactionContent!){removeReaction(input:{subjectId:$id,content:$c}){reaction{content}}}",
		map[string]any{"id": rv.NodeID, "c": content}, nil)
	if err != nil {
		return 0, err
	}
	return 1, nil
}

// isNotFound reports a 404 — for a DELETE, "already gone".
func isNotFound(err error) bool { return strings.Contains(err.Error(), "HTTP 404") }

// ReactionSubject is one thing a reaction lands on.
type ReactionSubject struct {
	Kind string
	ID   int64
}

// reactionSubjects reads `subjects: [{kind, id}, …]`, or the single-subject
// shorthand `kind:` + `id:`. Entries missing either half are dropped.
func reactionSubjects(opts map[string]any) []ReactionSubject {
	var out []ReactionSubject
	add := func(kind any, id any) {
		k, _ := kind.(string)
		n := toInt64(id)
		if k != "" && n > 0 {
			out = append(out, ReactionSubject{Kind: k, ID: n})
		}
	}
	switch xs := opts["subjects"].(type) {
	case []any:
		for _, e := range xs {
			if m, ok := e.(map[string]any); ok {
				add(m["kind"], m["id"])
			}
		}
	case []map[string]any:
		for _, m := range xs {
			add(m["kind"], m["id"])
		}
	}
	if opts["kind"] != nil || opts["id"] != nil {
		add(opts["kind"], opts["id"])
	}
	return out
}

// setStatus posts a commit status on `sha`, or — given `pr` instead — on the
// PR's head as it is at call time (read fresh, so a status posted after a
// push lands on the new commit). The context is the caller's; only when it is
// unset does it default to the login of the identity the call acts as.
func (c *Client) setStatus(ctx context.Context, tok, base, repo string, number int, opts map[string]any) (map[string]any, error) {
	sha, _ := opts["sha"].(string)
	if sha == "" && number > 0 {
		var pr struct {
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", base, repo, number), &pr); err != nil {
			key := fmt.Sprintf("%s#%d", repo, number)
			return nil, fmt.Errorf("github.set_status: read PR head: %w", c.remapTargetGone(ctx, tok, repo, key, err))
		}
		sha = pr.Head.SHA
	}
	if sha == "" {
		return nil, fmt.Errorf("github.set_status: options.sha or options.pr is required")
	}
	state, _ := opts["state"].(string)
	if !contains(StatusStates(), state) {
		return nil, fmt.Errorf("github.set_status: options.state must be one of %s, got %q",
			strings.Join(StatusStates(), "|"), state)
	}
	sctx, _ := opts["context"].(string)
	if sctx == "" {
		login, err := c.Login(ctx, tok)
		if err != nil {
			return nil, fmt.Errorf("github.set_status: default context: %w", err)
		}
		sctx = login
	}
	body := map[string]any{"state": state, "context": sctx}
	if d, _ := opts["description"].(string); d != "" {
		body["description"] = ClipStatusDescription(d)
	}
	if u, _ := opts["target_url"].(string); u != "" {
		body["target_url"] = u
	}
	if err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/statuses/%s", base, repo, url.PathEscape(sha)), body, nil); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "context": sctx, "sha": sha}, nil
}

// ClipStatusDescription fits a description into GitHub's 140-character
// commit-status limit, ending a clipped one with an ellipsis.
func ClipStatusDescription(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxStatusDescription {
		return s
	}
	r := []rune(s)
	return string(r[:maxStatusDescription-1]) + "…"
}

// Login returns the login the token authenticates as (GET /user), cached per
// token for the client's lifetime — a login does not change under a token.
func (c *Client) Login(ctx context.Context, tok string) (string, error) {
	key := cacheKey(tok, "login", "")
	c.mu.Lock()
	if l, ok := c.logins[key]; ok {
		c.mu.Unlock()
		return l, nil
	}
	c.mu.Unlock()
	var u struct {
		Login string `json:"login"`
	}
	if err := c.getFresh(ctx, tok, c.base()+"/user", &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", fmt.Errorf("GET /user returned no login")
	}
	c.mu.Lock()
	c.logins[key] = u.Login
	c.mu.Unlock()
	return u.Login, nil
}

// PRHead reads a PR's current head sha and state — "open", "closed", or
// "merged" (a closed PR that merged) — bypassing the read cache: a caller
// comparing heads across a push must not be served the pre-push copy.
func (c *Client) PRHead(ctx context.Context, as, repo string, number int) (sha, state string, err error) {
	tok, err := c.TokenFor(ctx, as, repo)
	if err != nil {
		return "", "", err
	}
	var pr struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d", c.base(), repo, number), &pr); err != nil {
		key := fmt.Sprintf("%s#%d", repo, number)
		return "", "", c.remapTargetGone(ctx, tok, repo, key, err)
	}
	if pr.Merged {
		return pr.Head.SHA, "merged", nil
	}
	return pr.Head.SHA, pr.State, nil
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case uint64:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
