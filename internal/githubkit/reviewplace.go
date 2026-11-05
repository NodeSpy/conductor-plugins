package githubkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// hunk is one @@ range of a file's patch: the old-side (LEFT) and new-side
// (RIGHT) lines it covers, inclusive. A side with no lines has end < start.
type hunk struct {
	oldStart, oldEnd int
	newStart, newEnd int
}

var hunkHeader = regexp.MustCompile(`(?m)^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parseHunks reads the @@ headers of a unified-diff patch. A missing count is
// 1, as in diff's own shorthand.
func parseHunks(patch string) []hunk {
	var out []hunk
	for _, m := range hunkHeader.FindAllStringSubmatch(patch, -1) {
		num := func(s string, def int) int {
			if s == "" {
				return def
			}
			n, _ := strconv.Atoi(s)
			return n
		}
		olds, oldc := num(m[1], 0), num(m[2], 1)
		news, newc := num(m[3], 0), num(m[4], 1)
		out = append(out, hunk{oldStart: olds, oldEnd: olds + oldc - 1, newStart: news, newEnd: news + newc - 1})
	}
	return out
}

func (h hunk) covers(side string, line int) bool {
	if strings.EqualFold(side, "LEFT") {
		return line >= h.oldStart && line <= h.oldEnd
	}
	return line >= h.newStart && line <= h.newEnd
}

// prHunks maps every file in a pull request to its patch's hunks. A file
// GitHub sends no patch for (binary, or too large on its own) is present with
// no hunks: nothing in it can take a line comment.
func (c *Client) prHunks(ctx context.Context, tok, base, repo string, number int) (map[string][]hunk, error) {
	out := map[string][]hunk{}
	err := c.listAll(ctx, tok, true, 100, func(page int) string {
		return fmt.Sprintf("%s/repos/%s/pulls/%d/files?per_page=100&page=%d", base, repo, number, page)
	}, func(b []byte) (int, error) {
		var raw []struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			return 0, err
		}
		for _, f := range raw {
			out[f.Filename] = parseHunks(f.Patch)
		}
		return len(raw), nil
	})
	return out, err
}

// movedComment is an inline comment that goes into the review body instead,
// and why.
type movedComment struct {
	c   map[string]any
	why string
}

// placeReviewComments splits a review's comments into those GitHub will take
// inline (each line, and start_line if set, inside one hunk of the file on
// its side) and those it would reject — which would 422 the whole review —
// so they go into the body instead. GitHub's create-review call has no
// file-level comment (subject_type is only on the single-comment endpoint),
// so a line outside the hunks lands in the body under its file:line too.
func placeReviewComments(comments []map[string]any, files map[string][]hunk) (inline []map[string]any, moved []movedComment) {
	for _, cm := range comments {
		if why := unplaceable(cm, files); why != "" {
			moved = append(moved, movedComment{cm, why})
			continue
		}
		inline = append(inline, cm)
	}
	return inline, moved
}

func unplaceable(cm map[string]any, files map[string][]hunk) string {
	path, _ := cm["path"].(string)
	hunks, inPR := files[path]
	if !inPR {
		return "file is not part of the pull request"
	}
	line := toInt(cm["line"])
	if line == 0 {
		return "comment names no line"
	}
	if len(hunks) == 0 {
		return "GitHub has no patch for this file"
	}
	side, _ := cm["side"].(string)
	start := toInt(cm["start_line"])
	startSide, _ := cm["start_side"].(string)
	if startSide == "" {
		startSide = side
	}
	for _, h := range hunks {
		if h.covers(side, line) && (start == 0 || h.covers(startSide, start)) {
			return ""
		}
	}
	return "line is outside the pull request's diff hunks"
}

// commentLocation names a comment's file and line(s): "a.go:14", "a.go:10-14",
// or just "a.go".
func commentLocation(cm map[string]any) string {
	loc, _ := cm["path"].(string)
	line, start := toInt(cm["line"]), toInt(cm["start_line"])
	switch {
	case line > 0 && start > 0 && start != line:
		loc += fmt.Sprintf(":%d-%d", start, line)
	case line > 0:
		loc += fmt.Sprintf(":%d", line)
	}
	if side, _ := cm["side"].(string); strings.EqualFold(side, "LEFT") {
		loc += " (old)"
	}
	return loc
}

// reviewBodyWith appends moved comments to a review body, each under a
// heading naming its file:line, so nothing a reviewer wrote is lost.
func reviewBodyWith(body string, moved []movedComment) string {
	if len(moved) == 0 {
		return body
	}
	var sb strings.Builder
	if body != "" {
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}
	sb.WriteString("### Comments not placed inline")
	for _, m := range moved {
		cbody, _ := m.c["body"].(string)
		fmt.Fprintf(&sb, "\n\n#### `%s`\n\n%s", commentLocation(m.c), cbody)
	}
	return sb.String()
}

// submitReview posts a review, first moving any inline comment GitHub would
// reject (outside the PR's diff, or on a file not in it) into the body. If
// GitHub still answers 422, it retries once with every comment in the body,
// so the verdict and all the review's text always post.
func (c *Client) submitReview(ctx context.Context, tok, base, repo string, number int, event, body string, comments []map[string]any) (int64, int, error) {
	var moved []movedComment
	if len(comments) > 0 {
		files, err := c.prHunks(ctx, tok, base, repo, number)
		if err != nil {
			// Post as given: GitHub is the judge, and the 422 retry below
			// still keeps the review.
			log.Printf("github: submit_review %s#%d: could not read the PR's files to check comment lines, posting as given: %v", repo, number, err)
		} else {
			comments, moved = placeReviewComments(comments, files)
		}
	}
	post := func(inline []map[string]any, moved []movedComment) (int64, error) {
		req := map[string]any{"event": event}
		if b := reviewBodyWith(body, moved); b != "" {
			req["body"] = b
		}
		if len(inline) > 0 {
			req["comments"] = inline
		}
		var out struct {
			ID int64 `json:"id"`
		}
		err := c.post(ctx, tok, fmt.Sprintf("%s/repos/%s/pulls/%d/reviews", base, repo, number), req, &out)
		return out.ID, err
	}
	logMoved := func(moved []movedComment) {
		for _, m := range moved {
			log.Printf("github: submit_review %s#%d: comment on %s moved into the review body: %s", repo, number, commentLocation(m.c), m.why)
		}
	}
	logMoved(moved)
	id, err := post(comments, moved)
	var pe *plugin.Error
	if err != nil && len(comments) > 0 && errors.As(err, &pe) && pe.Code == plugin.CodeInvalid {
		retry := make([]movedComment, 0, len(comments))
		for _, cm := range comments {
			retry = append(retry, movedComment{cm, "GitHub rejected the review with them inline: " + pe.Message})
		}
		logMoved(retry)
		moved = append(moved, retry...)
		id, err = post(nil, moved)
	}
	return id, len(moved), err
}
