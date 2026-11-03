package githubkit

import (
	"context"
	"fmt"
	"strings"
)

// runnerLostMarkers are what GitHub Actions writes, as an annotation on the
// job, when a job failed because the machine running it went away rather
// than because of anything the job ran: a runner that was shut down (a
// self-hosted runner's service stopped, its host rebooted or scaled in), or
// one that stopped answering. GitHub has no check conclusion of its own for
// this; such a job concludes failure (or cancelled), so the annotation is the
// signal. Matched case-insensitively.
var runnerLostMarkers = []string{
	"the runner has received a shutdown signal",
	"lost communication with the server",
}

// maxRunnerLostJobs bounds the failed jobs whose annotations one diagnosis
// reads.
const maxRunnerLostJobs = 10

// runnerLost reports whether a failed job of a workflow run (its check suite)
// lost its runner. A read that fails answers false: the run is then treated
// as an ordinary failure, which is what it was before anyone asked.
func (c *Client) runnerLost(ctx context.Context, tok, base, repo string, suiteID int64) bool {
	if suiteID == 0 {
		return false
	}
	runs, err := c.listCheckRuns(ctx, tok, fmt.Sprintf("%s/repos/%s/check-suites/%d/check-runs", base, repo, suiteID))
	if err != nil {
		return false
	}
	read := 0
	for _, cr := range runs {
		if cr.Status != "completed" || !FailureConclusions[cr.Conclusion] {
			continue
		}
		if read == maxRunnerLostJobs {
			break
		}
		read++
		var anns []struct {
			Title   string `json:"title"`
			Message string `json:"message"`
		}
		if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/check-runs/%d/annotations?per_page=50", base, repo, cr.ID), &anns); err != nil {
			continue
		}
		for _, a := range anns {
			if isRunnerLost(a.Title + "\n" + a.Message) {
				return true
			}
		}
	}
	return false
}

func isRunnerLost(text string) bool {
	text = strings.ToLower(text)
	for _, m := range runnerLostMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}
