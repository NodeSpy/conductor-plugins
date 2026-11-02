package ghfake

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Actions job logs, the way GitHub serves them. GET
// /repos/{owner}/{repo}/actions/jobs/{job_id}/logs answers 302 with a
// Location on a DIFFERENT host (pipelines.actions.githubusercontent.com, or
// the results service's blob storage): a signed URL that expires within a
// minute or so, carrying its own authorization in the query. The log host
// knows nothing of GitHub tokens — a request that brings one is refused here,
// so a client that forwards its Authorization header across the redirect
// fails loudly instead of quietly leaking the token to a third host.
//
// Start serves the log host on its own server. A harness serving Handler
// itself serves LogsHandler on a second origin and calls SetLogsURL; until it
// does, the logs endpoint answers 500 rather than invent a same-origin log
// host GitHub does not have.

// logURLTTL is how long a signed log URL stays valid.
const logURLTTL = time.Minute

// logPathMarker is the path shape of a signed log URL (after the docs'
// example): /<token>/_apis/pipelines/1/jobs/<job_id>/signedlogcontent.
const logPathMarker = "/_apis/pipelines/1/jobs/"

// LogRead is one request the log host received.
type LogRead struct {
	JobID int64
	// Authorization is the Authorization header the request carried ("" when
	// none — the only way a client should arrive).
	Authorization string
	Status        int
}

// LogsHandler serves the job-log host.
func (f *Fake) LogsHandler() http.Handler { return http.HandlerFunc(f.serveLog) }

// SetLogsURL sets the origin signed log URLs point at (Start does it).
func (f *Fake) SetLogsURL(u string) {
	f.mu.Lock()
	f.logsURL = strings.TrimRight(u, "/")
	f.mu.Unlock()
}

// LogReads lists every request the log host received, in order.
func (f *Fake) LogReads() []LogRead {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LogRead(nil), f.logReads...)
}

func (f *Fake) logSig(jobID int64, expires string) string {
	m := hmac.New(sha256.New, f.logKey)
	fmt.Fprintf(m, "%d\n%s", jobID, expires)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// signedLogURL is the Location a job's logs endpoint redirects to.
func (f *Fake) signedLogURL(jobID int64) string {
	origin := f.logsURL
	exp := f.now().Add(logURLTTL).Format(time.RFC3339)
	q := url.Values{"urlExpires": {exp}, "urlSigningMethod": {"HMACV1"}, "urlSignature": {f.logSig(jobID, exp)}}
	return fmt.Sprintf("%s/ab1f3cCFPB34Nd6imvFxpGZH5hNlDp2wijMwl2gDoO0bcrrlJj%s%d/signedlogcontent?%s", origin, logPathMarker, jobID, q.Encode())
}

// hJobLogs is GET /repos/{owner}/{repo}/actions/jobs/{job_id}/logs.
func hJobLogs(c *call) (int, any) {
	for _, j := range c.repo.Jobs {
		if j.ID == c.i64("job_id") {
			if c.f.logsURL == "" {
				return ghErr(500, "ghfake: no job-log host — serve LogsHandler on its own origin and call SetLogsURL")
			}
			c.hdr.Set("Location", c.f.signedLogURL(j.ID))
			return http.StatusFound, nil
		}
	}
	return notFound()
}

func isLogPath(p string) bool {
	return strings.Contains(p, logPathMarker) && strings.HasSuffix(p, "/signedlogcontent")
}

// serveLog is the log host: a signed, unexpired URL for a job that exists,
// fetched WITHOUT a GitHub credential, gets the job's raw log.
func (f *Fake) serveLog(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	defer f.mu.Unlock()
	read := LogRead{Authorization: r.Header.Get("Authorization")}
	fail := func(status int, msg string) {
		read.Status = status
		f.logReads = append(f.logReads, read)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"utf-8\"?><Error><Code>AuthenticationFailed</Code><Message>%s</Message></Error>", msg)
	}
	if r.Method != http.MethodGet || !isLogPath(r.URL.Path) {
		fail(http.StatusNotFound, "The specified resource does not exist.")
		return
	}
	_, rest, _ := strings.Cut(r.URL.Path, logPathMarker)
	id, err := strconv.ParseInt(strings.TrimSuffix(rest, "/signedlogcontent"), 10, 64)
	if err != nil {
		fail(http.StatusNotFound, "The specified resource does not exist.")
		return
	}
	read.JobID = id
	if read.Authorization != "" {
		fail(http.StatusForbidden, "Server failed to authenticate the request: the URL is signed; an Authorization header is not accepted.")
		return
	}
	q := r.URL.Query()
	exp := q.Get("urlExpires")
	if !hmac.Equal([]byte(q.Get("urlSignature")), []byte(f.logSig(id, exp))) {
		fail(http.StatusForbidden, "Server failed to authenticate the request: signature mismatch.")
		return
	}
	if t, err := time.Parse(time.RFC3339, exp); err != nil || f.now().After(t) {
		fail(http.StatusForbidden, "Server failed to authenticate the request: the signed URL has expired.")
		return
	}
	for _, rp := range f.repos {
		for _, j := range rp.Jobs {
			if j.ID == id {
				read.Status = http.StatusOK
				f.logReads = append(f.logReads, read)
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, j.Log)
				return
			}
		}
	}
	fail(http.StatusNotFound, "The specified blob does not exist.")
}
