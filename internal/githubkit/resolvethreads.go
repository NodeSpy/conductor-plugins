package githubkit

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SubmissionKey orders review submissions: by submission second, then by
// review id within the second. A changes_requested event's feedback_cursor is
// the largest over the feedback it carries, and resolve_threads' seen_through
// reads one. GitHub numbers reviews and comments when they are DRAFTED, so ids
// alone misorder a review drafted before another but submitted after it.
func SubmissionKey(at time.Time, reviewID int64) int64 {
	if at.IsZero() {
		return 0
	}
	return at.Unix()*1_000_000 + reviewID%1_000_000
}

// resolveThreadsQuery reads the PR's threads with every comment's author and
// submission, and who the caller is.
const resolveThreadsQuery = `query($o:String!,$n:String!,$num:Int!){
  viewer{login}
  repository(owner:$o,name:$n){ pullRequest(number:$num){
    author{login}
    reviewThreads(first:100){nodes{id isResolved
      comments(first:100){nodes{databaseId author{login} pullRequestReview{databaseId submittedAt}}}
    }}
  }}}`

// resolveThreads resolves the PR's review threads named by comment ids (any
// comment in the thread) or thread node ids — the ones a run's agent reports
// it addressed, never every open thread. A named thread is left open when it
// is not on this PR, or (seen_through > 0) when someone other than you or the
// PR's author has commented in it since that cursor: feedback the agent never
// saw. Already-resolved threads count as done.
func (c *Client) resolveThreads(ctx context.Context, tok, repo string, number int, opts map[string]any) (map[string]any, error) {
	if number == 0 {
		return nil, fmt.Errorf("github.resolve_threads: options.pr is required")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("github.resolve_threads: options.repo must be owner/name")
	}
	wantComments := map[int64]bool{}
	for _, id := range int64List(opts["comments"]) {
		wantComments[id] = true
	}
	wantThreads := map[string]bool{}
	for _, id := range stringList(opts["threads"]) {
		wantThreads[id] = true
	}
	if len(wantComments) == 0 && len(wantThreads) == 0 {
		return map[string]any{"ok": true, "resolved": []any{}, "skipped": "no threads named"}, nil
	}
	seenThrough := int64Of(opts["seen_through"])

	var data struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Repository struct {
			PullRequest *struct {
				Author *struct {
					Login string `json:"login"`
				} `json:"author"`
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						Comments   struct {
							Nodes []struct {
								DatabaseID int64 `json:"databaseId"`
								Author     *struct {
									Login string `json:"login"`
								} `json:"author"`
								Review *struct {
									DatabaseID  int64     `json:"databaseId"`
									SubmittedAt time.Time `json:"submittedAt"`
								} `json:"pullRequestReview"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.graphql(ctx, tok, resolveThreadsQuery, map[string]any{"o": owner, "n": name, "num": number}, &data); err != nil {
		return nil, err
	}
	pr := data.Repository.PullRequest
	if pr == nil {
		return nil, fmt.Errorf("github.resolve_threads: %s#%d not found", repo, number)
	}
	ours := map[string]bool{strings.ToLower(data.Viewer.Login): true}
	if pr.Author != nil {
		ours[strings.ToLower(pr.Author.Login)] = true
	}
	resolved := []any{}
	var why []string
	found := map[string]bool{} // "c:<id>" / "t:<id>" the PR's threads hold
	for _, th := range pr.ReviewThreads.Nodes {
		named := wantThreads[th.ID]
		if named {
			found["t:"+th.ID] = true
		}
		newer := false
		for _, cm := range th.Comments.Nodes {
			if wantComments[cm.DatabaseID] {
				named = true
				found["c:"+strconv.FormatInt(cm.DatabaseID, 10)] = true
			}
			if seenThrough > 0 && cm.Author != nil && !ours[strings.ToLower(cm.Author.Login)] && cm.Review != nil &&
				SubmissionKey(cm.Review.SubmittedAt, cm.Review.DatabaseID) > seenThrough {
				newer = true
			}
		}
		if !named {
			continue
		}
		switch {
		case th.IsResolved:
			resolved = append(resolved, th.ID)
		case newer:
			why = append(why, th.ID+" has new feedback")
		default:
			err := c.graphql(ctx, tok,
				"mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{id isResolved}}}",
				map[string]any{"id": th.ID}, nil)
			if err != nil {
				return nil, err
			}
			resolved = append(resolved, th.ID)
		}
	}
	if len(found) < len(wantComments)+len(wantThreads) {
		why = append(why, "some named comments or threads are not in this PR's threads")
	}
	out := map[string]any{"ok": true, "resolved": resolved}
	if len(why) > 0 {
		out["skipped"] = strings.Join(why, "; ")
	}
	return out, nil
}

// int64List reads a list of ids: numbers, or numeric strings (a rendered
// template), or one comma-separated string. Anything else is dropped.
func int64List(v any) []int64 {
	var out []int64
	add := func(x any) {
		if id := int64Of(x); id > 0 {
			out = append(out, id)
		}
	}
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			add(x)
		}
	case []int64:
		out = append(out, l...)
	case []int:
		for _, x := range l {
			add(x)
		}
	case string:
		for _, s := range strings.Split(strings.Trim(l, "[] "), ",") {
			add(strings.TrimSpace(s))
		}
	default:
		add(v)
	}
	return out
}

// int64Of reads one integer option: a number or a numeric string.
func int64Of(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	}
	return 0
}

// stringList reads a list of non-empty strings.
func stringList(v any) []string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case []string:
		out = append(out, l...)
	case string:
		if s := strings.TrimSpace(l); s != "" {
			out = append(out, s)
		}
	}
	return out
}
