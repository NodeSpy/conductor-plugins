package githubkit

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// check_failures: what failed on a PR's head (or a ref, or one workflow run),
// in a form an agent can diagnose from without any GitHub access of its own —
// the failing check runs with what each reported (output title, summary,
// text, annotations), and for a GitHub Actions job the part of its log that
// shows the failure. Every check app's runs are covered; only Actions ones
// have a log. Commit statuses are not: they are not a CI signal the github
// connector acts on (failing_checks comes from check runs too).
//
// Everything it returns is other people's text (a test can print anything),
// bounded and cleaned — ANSI escapes, timestamps and control characters out,
// invalid UTF-8 repaired — but NOT made safe to follow: a caller handing it to
// an agent fences it as data.

// FailureConclusions are the check conclusions the github connector treats as
// failing: the failing_checks event fires on them, and check_failures reports
// them.
var FailureConclusions = map[string]bool{
	"failure": true, "timed_out": true, "cancelled": true,
	"startup_failure": true, "stale": true, "action_required": true,
}

const (
	// DefaultFailureLogBytes / MaxFailureLogBytes bound one failed job's
	// log excerpt (the log_bytes option).
	DefaultFailureLogBytes = 8 << 10
	MaxFailureLogBytes     = 64 << 10
	// DefaultFailureBytes / MaxFailureBytes bound all the text one call
	// returns — outputs, annotations and logs together (max_bytes).
	DefaultFailureBytes = 32 << 10
	MaxFailureBytes     = 256 << 10

	maxFailuresListed  = 20   // failing checks described; the rest are counted (omitted)
	maxPendingListed   = 20   // pending check names listed
	maxAnnotationsKept = 20   // per check, failures first
	outputFieldCap     = 2000 // bytes of a check's output summary / text, each
	annotationCap      = 500  // bytes of one annotation's message
	checkRunPages      = 3    // pages of 100 check runs read
	// logReadCap bounds the bytes of a raw log read while looking for the
	// end of the failed step; logTimeout one log fetch, redirect included.
	logReadCap = 32 << 20
	logTimeout = 60 * time.Second
)

type checkRunJSON struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	DetailsURL string `json:"details_url"`
	App        struct {
		Slug string `json:"slug"`
	} `json:"app"`
	Output struct {
		Title            string `json:"title"`
		Summary          string `json:"summary"`
		Text             string `json:"text"`
		AnnotationsCount int    `json:"annotations_count"`
	} `json:"output"`
}

// checkFailures implements the check_failures verb.
func (c *Client) checkFailures(ctx context.Context, as, tok, base, repo string, number int, opts map[string]any) (map[string]any, error) {
	logBytes := clampOpt(opts["log_bytes"], DefaultFailureLogBytes, MaxFailureLogBytes)
	budget := clampOpt(opts["max_bytes"], DefaultFailureBytes, MaxFailureBytes)
	ignore := ignoreList(opts["ignore"])

	var sha, listURL string
	runID := toInt64(opts["run_id"])
	ref, _ := opts["ref"].(string)
	switch {
	case runID > 0:
		var run struct {
			HeadSHA      string `json:"head_sha"`
			CheckSuiteID int64  `json:"check_suite_id"`
		}
		if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/actions/runs/%d", base, repo, runID), &run); err != nil {
			return nil, err
		}
		sha = run.HeadSHA
		listURL = fmt.Sprintf("%s/repos/%s/check-suites/%d/check-runs", base, repo, run.CheckSuiteID)
	case strings.TrimSpace(ref) != "":
		sha = strings.TrimSpace(ref)
		listURL = fmt.Sprintf("%s/repos/%s/commits/%s/check-runs", base, repo, url.PathEscape(sha))
	case number > 0:
		// The PR's head as of now (uncached): a fixer re-checking after a
		// push asks about the commit it pushed, not the one the event saw.
		head, _, err := c.PRHead(ctx, as, repo, number)
		if err != nil {
			return nil, err
		}
		sha = head
		listURL = fmt.Sprintf("%s/repos/%s/commits/%s/check-runs", base, repo, url.PathEscape(sha))
	default:
		return nil, fmt.Errorf("github.check_failures: options.pr, ref, or run_id is required")
	}

	runs, err := c.listCheckRuns(ctx, tok, listURL)
	if err != nil {
		return nil, err
	}
	var failing []checkRunJSON
	pending := []any{}
	passing, ignored := 0, 0
	for _, cr := range runs {
		switch {
		case containsFoldName(ignore, cr.Name):
			ignored++
		case cr.Status != "completed":
			if len(pending) < maxPendingListed {
				pending = append(pending, cr.Name)
			}
		case FailureConclusions[cr.Conclusion]:
			failing = append(failing, cr)
		default:
			passing++
		}
	}
	sort.SliceStable(failing, func(i, j int) bool { return failing[i].Name < failing[j].Name })
	omitted := 0
	if len(failing) > maxFailuresListed {
		omitted = len(failing) - maxFailuresListed
		failing = failing[:maxFailuresListed]
	}

	b := &textBudget{left: budget}
	failures := make([]any, 0, len(failing))
	for _, cr := range failing {
		failures = append(failures, c.describeFailure(ctx, tok, base, repo, cr, logBytes, b))
	}
	out := map[string]any{
		"sha": sha, "failures": failures, "pending": pending,
		"passing": passing, "ignored": ignored, "omitted": omitted,
		"truncated": b.cut || omitted > 0,
	}
	return out, nil
}

// listCheckRuns reads a check-runs list (filter=latest: a re-run's latest
// attempt per name), fresh — a re-check after a push must see now, not a
// cached answer from before it.
func (c *Client) listCheckRuns(ctx context.Context, tok, listURL string) ([]checkRunJSON, error) {
	var all []checkRunJSON
	for page := 1; page <= checkRunPages; page++ {
		var out struct {
			Total     int            `json:"total_count"`
			CheckRuns []checkRunJSON `json:"check_runs"`
		}
		u := fmt.Sprintf("%s?filter=latest&per_page=100&page=%d", listURL, page)
		if err := c.getFresh(ctx, tok, u, &out); err != nil {
			return nil, err
		}
		all = append(all, out.CheckRuns...)
		if len(out.CheckRuns) < 100 || len(all) >= out.Total {
			break
		}
	}
	return all, nil
}

// describeFailure is one failing check, as much of it as the budget allows:
// what it reported, its annotations, and — for an Actions job — its failed
// step and the log excerpt showing the failure. A read that fails is noted
// on the entry (annotations_error, log_error), never fails the call.
func (c *Client) describeFailure(ctx context.Context, tok, base, repo string, cr checkRunJSON, logBytes int, b *textBudget) map[string]any {
	e := map[string]any{"name": cr.Name, "conclusion": cr.Conclusion, "url": cr.HTMLURL, "app": cr.App.Slug}
	if cr.DetailsURL != "" && cr.DetailsURL != cr.HTMLURL {
		e["details_url"] = cr.DetailsURL
	}
	b.put(e, "title", cleanText(cr.Output.Title), 300)
	b.put(e, "summary", cleanText(cr.Output.Summary), outputFieldCap)
	b.put(e, "text", cleanText(cr.Output.Text), outputFieldCap)
	if n := cr.Output.AnnotationsCount; n > 0 {
		if b.left <= 0 {
			e["annotations_omitted"], b.cut = n, true
		} else if anns, err := c.annotations(ctx, tok, base, repo, cr.ID, b); err != nil {
			e["annotations_error"] = shortReadError(err, "")
		} else {
			e["annotations"] = anns
			if n > len(anns) {
				e["annotations_omitted"], b.cut = n-len(anns), true
			}
		}
	}
	if cr.App.Slug != "github-actions" {
		return e
	}
	// An Actions check run IS its job: the same id.
	e["job_id"] = cr.ID
	var job struct {
		RunID int64 `json:"run_id"`
		Steps []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"steps"`
	}
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/actions/jobs/%d", base, repo, cr.ID), &job); err == nil {
		e["run_id"] = job.RunID
		for _, st := range job.Steps {
			if FailureConclusions[st.Conclusion] {
				e["failed_step"] = cleanText(st.Name)
				break
			}
		}
	}
	limit := min(logBytes, b.left)
	if limit <= 0 {
		e["log_omitted"], b.cut = true, true
		return e
	}
	excerpt, cut, err := c.jobLogExcerpt(ctx, tok, base, repo, cr.ID, limit)
	if err != nil {
		e["log_error"] = shortReadError(err, logAccessHint)
		return e
	}
	if excerpt != "" {
		e["log"] = excerpt
		b.left -= len(excerpt)
	}
	if cut {
		e["log_truncated"], b.cut = true, true
	}
	return e
}

// annotations reads a check run's annotations (one page of 50), failures
// first, each message capped.
func (c *Client) annotations(ctx context.Context, tok, base, repo string, id int64, b *textBudget) ([]any, error) {
	var raw []struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Level     string `json:"annotation_level"`
		Title     string `json:"title"`
		Message   string `json:"message"`
	}
	if err := c.getFresh(ctx, tok, fmt.Sprintf("%s/repos/%s/check-runs/%d/annotations?per_page=50", base, repo, id), &raw); err != nil {
		return nil, err
	}
	rank := map[string]int{"failure": 0, "warning": 1, "notice": 2}
	sort.SliceStable(raw, func(i, j int) bool {
		ri, ok := rank[raw[i].Level]
		if !ok {
			ri = 3
		}
		rj, ok := rank[raw[j].Level]
		if !ok {
			rj = 3
		}
		return ri < rj
	})
	out := []any{}
	for _, a := range raw {
		if len(out) == maxAnnotationsKept || b.left <= 0 {
			break
		}
		m := map[string]any{"path": cleanText(a.Path), "start_line": a.StartLine, "end_line": a.EndLine, "level": a.Level}
		b.put(m, "title", cleanText(a.Title), 200)
		b.put(m, "message", cleanText(a.Message), annotationCap)
		out = append(out, m)
	}
	return out, nil
}

// jobLogExcerpt fetches an Actions job's log and returns its failure excerpt
// (logExcerpt) within limit bytes.
//
// GitHub answers the logs endpoint with a redirect to a short-lived signed URL
// on another host (the results service's blob storage). The GitHub credential
// must not follow it there: the URL carries its own authorization, and the
// token is nobody else's business. net/http drops Authorization only when the
// redirect changes the host NAME, so the header is removed here on any change
// of origin (a different port included).
func (c *Client) jobLogExcerpt(ctx context.Context, tok, base, repo string, jobID int64, limit int) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, logTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/actions/jobs/%d/logs", base, repo, jobID), nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	hc := *c.httpc
	hc.Timeout = 0 // bounded by ctx instead: a log is bigger than an API answer
	hc.CheckRedirect = logRedirect
	resp, err := hc.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		if resp.Request != nil && resp.Request.URL.Host != req.URL.Host {
			// The log host's error body is not GitHub's (and may echo the
			// signed URL): the status says enough.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			return "", false, fmt.Errorf("log download: HTTP %d", resp.StatusCode)
		}
		return "", false, c.httpFailure("GET", req.URL.String(), resp)
	}
	body := &capReader{r: resp.Body, left: logReadCap}
	excerpt, cut, err := logExcerpt(body, limit)
	return excerpt, cut || body.hit, err
}

// capReader reads at most left bytes and records whether there was more.
type capReader struct {
	r    io.Reader
	left int64
	hit  bool
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var one [1]byte
		if n, _ := c.r.Read(one[:]); n > 0 {
			c.hit = true
		}
		return 0, io.EOF
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// logRedirect is the job-log fetch's redirect policy: at most 5 hops, never
// from https down to http, and the GitHub credential dropped the moment a hop
// leaves the API's origin.
func logRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("log download: too many redirects")
	}
	first := via[0].URL
	if first.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("log download: refusing a redirect from https to " + req.URL.Scheme)
	}
	if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		req.Header.Del("Authorization")
	}
	return nil
}

// --- the log excerpt ---------------------------------------------------------

var (
	// logTimestamp is the per-line prefix Actions writes:
	// 2026-11-02T10:00:00.1234567Z
	logTimestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z ?`)
	// ansiEscape is a CSI sequence (colors, cursor moves), an OSC sequence
	// (hyperlinks, titles), or a lone escape and the byte it introduces.
	ansiEscape = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)?|\x1b[0-~]?")
)

// isStepStart reports whether a cleaned log line opens a new top-level step:
// Actions prints "##[group]Run <command or action>" first in every step, and
// "Post job cleanup." first in the post phase.
func isStepStart(line string) bool {
	return strings.HasPrefix(line, "##[group]Run ") || line == "Post job cleanup."
}

// logExcerpt reads a raw Actions job log and returns, within limit bytes, the
// part that shows the failure: the step that printed the first ##[error] line
// — from its "Run …" header through the line before the next step starts —
// keeping its END when it does not fit (the header stays, the middle goes).
// A log with no ##[error] line gives its last limit bytes. Lines are cleaned
// (cleanLogLine) and ##[endgroup] markers dropped. cut reports that anything
// of the chosen part was left out.
func logExcerpt(r io.Reader, limit int) (excerpt string, cut bool, err error) {
	if limit <= 0 {
		return "", false, nil
	}
	whole := newLineTail(limit)   // the log's tail, for a log with no error
	section := newLineTail(limit) // the current step's tail
	header := ""
	errSeen, longLine := false, false
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		raw, rerr := readLogLine(br, limit+1)
		if raw != "" || rerr == nil {
			line := cleanLogLine(raw)
			if len(line) > limit {
				line, longLine = capBytes(line, limit), true
			}
			switch {
			case line == "##[endgroup]":
			case isStepStart(line) && errSeen:
				// The failed step ended: everything after it is cleanup.
				return section.render(header, limit), section.dropped > 0 || longLine, nil
			default:
				if isStepStart(line) {
					section, header = newLineTail(limit), line
				}
				whole.add(line)
				section.add(line)
				if strings.HasPrefix(line, "##[error]") {
					errSeen = true
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", false, rerr
		}
	}
	if errSeen {
		return section.render(header, limit), section.dropped > 0 || longLine, nil
	}
	return whole.render("", limit), whole.dropped > 0 || longLine, nil
}

// readLogLine reads one line (without its newline), keeping at most max bytes
// of a pathologically long one.
func readLogLine(br *bufio.Reader, max int) (string, error) {
	var sb strings.Builder
	for {
		chunk, isPrefix, err := br.ReadLine()
		if sb.Len() < max {
			room := max - sb.Len()
			if len(chunk) > room {
				chunk = chunk[:room]
			}
			sb.Write(chunk)
		}
		if err != nil {
			return sb.String(), err
		}
		if !isPrefix {
			return sb.String(), nil
		}
	}
}

// cleanLogLine strips what a log line carries for a terminal, not a reader:
// the leading BOM and timestamp, ANSI escapes, a progress bar's overwritten
// segments (everything up to the last carriage return), control characters,
// and invalid UTF-8.
func cleanLogLine(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = logTimestamp.ReplaceAllString(s, "")
	if i := strings.LastIndexByte(strings.TrimRight(s, "\r"), '\r'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimRight(cleanText(s), " \t")
}

// cleanText removes ANSI escapes, invalid UTF-8, and characters that are not
// there to be read from other people's text: control characters other than
// newline and tab, and the invisible format characters (zero-width spaces and
// joiners, bidi overrides and isolates, line/paragraph separators, BOM) that
// can make text read differently than it is.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = ansiEscape.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0),
			r >= 0x200b && r <= 0x200f, r >= 0x2028 && r <= 0x202e,
			r >= 0x2060 && r <= 0x2069, r == 0xfeff:
			return -1
		}
		return r
	}, s)
}

// lineTail keeps the last lines of a stream within a byte budget.
type lineTail struct {
	lines   []string
	size    int
	limit   int
	dropped int
}

func newLineTail(limit int) *lineTail { return &lineTail{limit: limit} }

func (t *lineTail) add(line string) {
	t.lines = append(t.lines, line)
	t.size += len(line) + 1
	for t.size > t.limit && len(t.lines) > 1 {
		t.size -= len(t.lines[0]) + 1
		t.lines = t.lines[1:]
		t.dropped++
	}
}

// render joins the kept lines; when some were dropped and the step header is
// one of them, the header leads, then a marker, then as much of the tail as
// still fits in limit.
func (t *lineTail) render(header string, limit int) string {
	lines := t.lines
	if t.dropped > 0 && header != "" && (len(lines) == 0 || lines[0] != header) {
		prefix := header + "\n…[" + strconv.Itoa(t.dropped) + " earlier lines cut]…"
		size := len(prefix)
		keep := len(lines)
		for i := len(lines) - 1; i >= 0; i-- {
			if size+1+len(lines[i]) > limit {
				break
			}
			size += 1 + len(lines[i])
			keep = i
		}
		if keep == len(lines) {
			return capBytes(prefix, limit)
		}
		return prefix + "\n" + strings.Join(lines[keep:], "\n")
	}
	return capBytes(strings.Join(lines, "\n"), limit)
}

// --- shared helpers ------------------------------------------------------------

// textBudget is the byte budget shared by every text field of one answer.
type textBudget struct {
	left int
	cut  bool
}

// put stores s under key in m, capped to the smaller of max and what is left
// of the budget; empty text stores nothing.
func (b *textBudget) put(m map[string]any, key, s string, max int) {
	if s == "" {
		return
	}
	lim := min(max, b.left)
	if lim <= 0 {
		b.cut = true
		return
	}
	if len(s) > lim {
		s, b.cut = capBytes(s, lim), true
	}
	m[key] = s
	b.left -= len(s)
}

// capBytes cuts s to at most n bytes on a rune boundary, marking the cut.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = " …[cut]"
	if n <= len(mark) {
		return ""
	}
	s = s[:n-len(mark)]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + mark
}

// clampOpt reads a positive integer option, defaulting to def and capped at max.
func clampOpt(v any, def, max int) int {
	n := toInt(v)
	if s, ok := v.(string); ok {
		n, _ = strconv.Atoi(strings.TrimSpace(s))
	}
	if n <= 0 {
		return def
	}
	return min(n, max)
}

// ignoreList reads the ignore option: a list of check names, or one string
// of them separated by commas or newlines (what a template renders a list
// fact to; an unset fact renders to "").
func ignoreList(v any) []string {
	var out []string
	for _, s := range toStrings(v) {
		for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
			if p = strings.TrimSpace(p); p != "" && p != "<no value>" && p != "<nil>" && p != "[]" {
				out = append(out, p)
			}
		}
	}
	return out
}

func containsFoldName(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// logAccessHint explains a refused or missing job log.
const logAccessHint = " (a job log needs the credential's Actions read permission, and expires with the repository's log retention)"

// shortReadError is a read failure as an entry note: its status and GitHub's
// message, not a URL; a 403/404 gets hint appended.
func shortReadError(err error, hint string) string {
	msg := err.Error()
	if i := strings.Index(msg, "HTTP "); i >= 0 {
		msg = msg[i:]
	}
	if hint != "" && (strings.Contains(msg, "HTTP 403") || strings.Contains(msg, "HTTP 404")) {
		msg += hint
	}
	return capBytes(msg, 400)
}
