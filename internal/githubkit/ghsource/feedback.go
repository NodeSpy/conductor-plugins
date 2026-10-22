package ghsource

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit"
)

// EVERY UNADDRESSED REVIEW, IN EVERY RUN.
//
// A changes_requested run carries all the review feedback on the PR that is
// still waiting on you, not just the review that fired it. Two reviews a
// moment apart (or one landing while a run is working) are then covered by
// whichever event is handled, in any order.
//
// Unaddressed means:
//   - an unresolved thread whose most recent comments are not yours: the
//     comments after your last reply in it (a thread you answered last is
//     waiting on the reviewer, not on you). A thread opened by a reviewer
//     whose latest review approved is left out: they signed off.
//   - a reviewer's latest review requesting changes, submitted on the PR's
//     current head: nothing has been pushed since it asked.
//
// The event's cursor (feedback_cursor) is the latest SUBMISSION among those,
// not the highest comment id. GitHub numbers a review and its comments when
// they are drafted, so a review drafted before another but submitted after it
// has the lower ids: a mark over ids dropped it as already handled.

// submissionKey is githubkit.SubmissionKey (shared with resolve_threads'
// seen_through guard).
func submissionKey(at time.Time, reviewID int64) int64 { return githubkit.SubmissionKey(at, reviewID) }

// fbComment is one review comment as the fold reads it.
type fbComment struct {
	ID          int64
	Author      string
	AuthorIsBot bool
	Path        string
	Line        int
	Body        string
	URL         string
	ReviewID    int64
	SubmittedAt time.Time
}

// fbThread is one review thread: its opening comment and its most recent
// comments (which may include the opener), oldest first.
type fbThread struct {
	ID       string
	Resolved bool
	Opener   fbComment
	Recent   []fbComment
}

// fbReview is a reviewer's latest opinionated review.
type fbReview struct {
	ID          int64
	Author      string
	AuthorIsBot bool
	State       string // APPROVED | CHANGES_REQUESTED | DISMISSED
	Body        string
	URL         string
	Commit      string
	SubmittedAt time.Time
}

// prFeedback is what the fold reads off a PR in one query.
type prFeedback struct {
	Head    string
	Threads []fbThread
	Latest  []fbReview
}

// fbRecentComments bounds the comments read per thread: the tail the fold
// needs (replies since your last one) is short on any thread a human reads.
const fbRecentComments = 30

var feedbackQuery = fmt.Sprintf(`query($o:String!,$n:String!,$num:Int!){
  repository(owner:$o,name:$n){ pullRequest(number:$num){
    headRefOid
    reviewThreads(first:100){nodes{id isResolved
      opener: comments(first:1){nodes{databaseId author{login __typename} path line originalLine body url pullRequestReview{databaseId submittedAt}}}
      recent: comments(last:%d){nodes{databaseId author{login __typename} path line originalLine body url pullRequestReview{databaseId submittedAt}}}
    }}
    latestOpinionatedReviews(first:50){nodes{databaseId state body url submittedAt author{login __typename} commit{oid}}}
  }}}`, fbRecentComments)

type gqlActor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

// login is the actor's REST-style login: GraphQL names a Bot by its bare slug
// ("cursor"), REST and webhooks say "cursor[bot]".
func (a *gqlActor) login() (string, bool) {
	if a == nil {
		return "", false
	}
	bot := isBotActor(a.Typename, a.Login)
	if bot && !isBotLogin(a.Login) {
		return a.Login + "[bot]", true
	}
	return a.Login, bot
}

type gqlFBComment struct {
	DatabaseID   int64     `json:"databaseId"`
	Author       *gqlActor `json:"author"`
	Path         string    `json:"path"`
	Line         *int      `json:"line"`
	OriginalLine *int      `json:"originalLine"`
	Body         string    `json:"body"`
	URL          string    `json:"url"`
	Review       *struct {
		DatabaseID  int64     `json:"databaseId"`
		SubmittedAt time.Time `json:"submittedAt"`
	} `json:"pullRequestReview"`
}

func (c gqlFBComment) comment() fbComment {
	out := fbComment{ID: c.DatabaseID, Path: c.Path, Line: lineOf(c.Line, c.OriginalLine), Body: c.Body, URL: c.URL}
	out.Author, out.AuthorIsBot = c.Author.login()
	if c.Review != nil {
		out.ReviewID, out.SubmittedAt = c.Review.DatabaseID, c.Review.SubmittedAt
	}
	return out
}

// prFeedback reads a PR's review threads and latest reviews (on App creds).
func (c *restClient) prFeedback(ctx context.Context, instID int64, owner, name string, number int) (prFeedback, error) {
	var data struct {
		Repository struct {
			PullRequest *struct {
				HeadRefOid    string `json:"headRefOid"`
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						Opener     struct {
							Nodes []gqlFBComment `json:"nodes"`
						} `json:"opener"`
						Recent struct {
							Nodes []gqlFBComment `json:"nodes"`
						} `json:"recent"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
				LatestOpinionatedReviews struct {
					Nodes []struct {
						DatabaseID  int64     `json:"databaseId"`
						State       string    `json:"state"`
						Body        string    `json:"body"`
						URL         string    `json:"url"`
						SubmittedAt time.Time `json:"submittedAt"`
						Author      *gqlActor `json:"author"`
						Commit      *struct {
							Oid string `json:"oid"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"latestOpinionatedReviews"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.graphql(ctx, instID, feedbackQuery, map[string]any{"o": owner, "n": name, "num": number}, &data); err != nil {
		return prFeedback{}, err
	}
	pr := data.Repository.PullRequest
	if pr == nil {
		return prFeedback{}, fmt.Errorf("pull request %s/%s#%d not found", owner, name, number)
	}
	out := prFeedback{Head: pr.HeadRefOid}
	for _, t := range pr.ReviewThreads.Nodes {
		if len(t.Opener.Nodes) == 0 {
			continue
		}
		th := fbThread{ID: t.ID, Resolved: t.IsResolved, Opener: t.Opener.Nodes[0].comment()}
		for _, c := range t.Recent.Nodes {
			th.Recent = append(th.Recent, c.comment())
		}
		out.Threads = append(out.Threads, th)
	}
	sort.Slice(out.Threads, func(i, j int) bool { return out.Threads[i].ID < out.Threads[j].ID })
	for _, r := range pr.LatestOpinionatedReviews.Nodes {
		rv := fbReview{ID: r.DatabaseID, State: r.State, Body: r.Body, URL: r.URL, SubmittedAt: r.SubmittedAt}
		rv.Author, rv.AuthorIsBot = r.Author.login()
		if r.Commit != nil {
			rv.Commit = r.Commit.Oid
		}
		out.Latest = append(out.Latest, rv)
	}
	return out, nil
}

// subject is one reaction subject (a reaction_subjects entry).
type subject struct {
	kind   string
	id     int64
	review int64 // the review a comment subject belongs to (0: a review subject)
}

// fold is the unaddressed feedback on a PR, ready to ride an event.
type fold struct {
	// Comments: each unaddressed thread's opener (for context) and its
	// unaddressed tail, thread by thread.
	Comments []reviewComment
	// Reviews: the outstanding changes-requests (their summaries).
	Reviews []fbReview
	// Threads: the unaddressed threads' ids (the sweep's signature).
	Threads []string
	// Subjects: what the run reacts on — each thread's latest unaddressed
	// comment and each outstanding review.
	Subjects []subject
	// Cursor: the latest submission the fold covers (submissionKey).
	Cursor int64
	// MaxCommentID: the highest comment id it covers (the comment_id fact).
	MaxCommentID int64
	// tailAuthors: who the unaddressed comments are from, in fold order.
	tailAuthors []fbComment
}

func (f *fold) empty() bool { return len(f.Comments) == 0 && len(f.Reviews) == 0 }

// foldFeedback applies the package note's rules to what prFeedback read.
func (g *Source) foldFeedback(fb prFeedback) fold {
	var out fold
	approved := map[string]bool{}
	for _, r := range fb.Latest {
		if r.State == "APPROVED" {
			approved[strings.ToLower(r.Author)] = true
		}
	}
	for _, th := range fb.Threads {
		if th.Resolved || approved[strings.ToLower(th.Opener.Author)] {
			continue
		}
		// The tail: the comments after your last one. (A thread longer than
		// the read window whose window holds none of yours counts whole.)
		tail := th.Recent
		for i := len(th.Recent) - 1; i >= 0; i-- {
			if g.isSelf(th.Recent[i].Author) {
				tail = th.Recent[i+1:]
				break
			}
		}
		if len(tail) == 0 {
			continue
		}
		out.Threads = append(out.Threads, th.ID)
		if tail[0].ID != th.Opener.ID {
			out.Comments = append(out.Comments, th.Opener.reviewComment(th.ID))
		}
		for _, c := range tail {
			out.Comments = append(out.Comments, c.reviewComment(th.ID))
			out.tailAuthors = append(out.tailAuthors, c)
			if k := submissionKey(c.SubmittedAt, c.ReviewID); k > out.Cursor {
				out.Cursor = k
			}
			if c.ID > out.MaxCommentID {
				out.MaxCommentID = c.ID
			}
		}
		last := tail[len(tail)-1]
		out.Subjects = append(out.Subjects, subject{githubkit.SubjectReviewComment, last.ID, last.ReviewID})
	}
	for _, r := range fb.Latest {
		if r.State != "CHANGES_REQUESTED" || g.isSelf(r.Author) || (fb.Head != "" && r.Commit != fb.Head) {
			continue
		}
		out.Reviews = append(out.Reviews, r)
		out.Subjects = append(out.Subjects, subject{githubkit.SubjectReview, r.ID, 0})
		if k := submissionKey(r.SubmittedAt, r.ID); k > out.Cursor {
			out.Cursor = k
		}
	}
	return out
}

func (c fbComment) reviewComment(threadID string) reviewComment {
	return reviewComment{ID: c.ID, Author: c.Author, Path: c.Path, Line: c.Line, Body: c.Body, URL: c.URL, Thread: threadID}
}

// readFold reads and folds a PR's feedback; ok=false when it can't be read
// (no REST client, or the query failed).
func (g *Source) readFold(ctx context.Context, instID int64, repo string, number int) (fold, prFeedback, bool) {
	rest := g.restClient()
	if rest == nil {
		return fold{}, prFeedback{}, false
	}
	owner, name := splitRepo(repo)
	fb, err := rest.prFeedback(ctx, instID, owner, name, number)
	if err != nil {
		log.Printf("github[%s]: %s#%d review feedback: %v", g.name, repo, number, err)
		return fold{}, prFeedback{}, false
	}
	return g.foldFeedback(fb), fb, true
}

// has reports whether the read saw a comment in any thread.
func (fb prFeedback) has(commentID int64) bool {
	for _, th := range fb.Threads {
		if th.Opener.ID == commentID {
			return true
		}
		for _, c := range th.Recent {
			if c.ID == commentID {
				return true
			}
		}
	}
	return false
}

// reviewFold is the fold a submitted review's event carries: the PR's
// unaddressed feedback, plus the review's own comments and submission when
// the read does not show them yet (GitHub's reads can trail its webhooks)
// or could not be made at all.
func (g *Source) reviewFold(ctx context.Context, instID int64, repo string, number int, reviewID int64, ri reviewInfo, cs []reviewComment) fold {
	f, fb, ok := g.readFold(ctx, instID, repo, number)
	for _, c := range cs {
		if ok && fb.has(c.ID) {
			continue // the read saw it: its decision (addressed or not) stands
		}
		f.Comments = append(f.Comments, c)
		if c.ID > f.MaxCommentID {
			f.MaxCommentID = c.ID
		}
	}
	if k := submissionKey(ri.SubmittedAt, reviewID); k > f.Cursor {
		f.Cursor = k
	}
	// The review that fired is reacted on as a whole: none of its own
	// comments (nor itself again) as separate subjects.
	kept := f.Subjects[:0]
	for _, sj := range f.Subjects {
		if sj.review != reviewID && !(sj.kind == githubkit.SubjectReview && sj.id == reviewID) {
			kept = append(kept, sj)
		}
	}
	f.Subjects = kept
	return f
}

// stampFold puts a fold on a changes_requested event's context: the comments
// and outstanding reviews it covers, what it reacts on (after any subjects
// already there — the triggering review's), and its cursor.
func stampFold(extra map[string]any, f fold) {
	addReviewComments(extra, f.Comments)
	if len(f.Reviews) > 0 {
		rs := make([]any, 0, len(f.Reviews))
		for _, r := range f.Reviews {
			m := map[string]any{"author": r.Author, "body": clip(r.Body, maxReviewCommentBody), "review_id": r.ID}
			if r.URL != "" {
				m["url"] = r.URL
			}
			rs = append(rs, m)
		}
		extra["outstanding_reviews"] = rs
	}
	subjects, _ := extra["reaction_subjects"].([]any)
	type key struct {
		kind string
		id   int64
	}
	seen := map[key]bool{}
	for _, s := range subjects {
		if m, ok := s.(map[string]any); ok {
			k, _ := m["kind"].(string)
			id, _ := m["id"].(int64)
			seen[key{k, id}] = true
		}
	}
	for _, s := range f.Subjects {
		if len(subjects) >= maxThreadReactions {
			break
		}
		if k := (key{s.kind, s.id}); !seen[k] {
			seen[k] = true
			subjects = append(subjects, map[string]any{"kind": s.kind, "id": s.id})
		}
	}
	extra["reaction_subjects"] = subjects
	if f.Cursor > 0 {
		extra["feedback_cursor"] = f.Cursor
	}
	if f.MaxCommentID > 0 {
		extra["comment_id"], extra["comment_kind"] = f.MaxCommentID, CommentKindReview
	}
}

// foldReviewerFacts names the reviewer behind a fold — the first human with
// an outstanding changes-request, else the first human with an unaddressed
// comment, else a bot — as the author/author_is_bot facts. nil when there is
// none.
func (g *Source) foldReviewerFacts(f fold) map[string]any {
	var bot string
	for _, r := range f.Reviews {
		if r.Author == "" || g.isSelf(r.Author) {
			continue
		}
		if !r.AuthorIsBot {
			return map[string]any{"author": r.Author, "author_is_bot": false}
		}
		if bot == "" {
			bot = r.Author
		}
	}
	for _, c := range f.tailAuthors {
		if c.Author == "" || g.isSelf(c.Author) {
			continue
		}
		if !c.AuthorIsBot {
			return map[string]any{"author": c.Author, "author_is_bot": false}
		}
		if bot == "" {
			bot = c.Author
		}
	}
	if bot != "" {
		return map[string]any{"author": bot, "author_is_bot": true}
	}
	return nil
}
