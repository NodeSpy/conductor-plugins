package githubkit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghfake"
)

// goTestLog is an Actions job log the way the logs endpoint serves it: a BOM,
// a timestamp on every line, ANSI colors, nested groups, a progress bar
// redrawn with carriage returns, the failed step's ##[error], then the post
// steps that run after it.
const goTestLog = "\ufeff2026-11-02T10:00:00.0000000Z Current runner version: '2.320.0'\n" +
	"2026-11-02T10:00:00.1000000Z ##[group]Operating System\n" +
	"2026-11-02T10:00:00.1100000Z Ubuntu 24.04\n" +
	"2026-11-02T10:00:00.1200000Z ##[endgroup]\n" +
	"2026-11-02T10:00:01.0000000Z ##[group]Run actions/checkout@v4\n" +
	"2026-11-02T10:00:01.1000000Z with:\n" +
	"2026-11-02T10:00:01.2000000Z ##[endgroup]\n" +
	"2026-11-02T10:00:02.0000000Z Syncing repository\n" +
	"2026-11-02T10:00:03.0000000Z ##[group]Run go test ./...\n" +
	"2026-11-02T10:00:03.1000000Z \x1b[36;1mgo test ./...\x1b[0m\n" +
	"2026-11-02T10:00:03.2000000Z ##[endgroup]\n" +
	"2026-11-02T10:00:04.0000000Z downloading  10%\rdownloading  60%\rdownloading 100%\n" +
	"2026-11-02T10:00:05.0000000Z --- FAIL: TestParse (0.00s)\n" +
	"2026-11-02T10:00:05.1000000Z     parse_test.go:42: got \x1b[31m\"b\"\x1b[0m, want \"a\"\n" +
	"2026-11-02T10:00:05.2000000Z FAIL\tgithub.example/acme/w/parse\t0.004s\n" +
	"2026-11-02T10:00:05.3000000Z ##[error]Process completed with exit code 1.\n" +
	"2026-11-02T10:00:06.0000000Z Post job cleanup.\n" +
	"2026-11-02T10:00:06.1000000Z [command]/usr/bin/git version\n" +
	"2026-11-02T10:00:06.2000000Z Cleaning up orphan processes\n"

func invokeCheckFailures(t *testing.T, kit *Client, opts map[string]any) map[string]any {
	t.Helper()
	opts["repo"] = "acme/w"
	out, err := kit.Invoke(context.Background(), "check_failures", opts)
	if err != nil {
		t.Fatalf("check_failures %v: %v", opts, err)
	}
	return out
}

func failureNamed(t *testing.T, out map[string]any, name string) map[string]any {
	t.Helper()
	for _, f := range out["failures"].([]any) {
		if m := f.(map[string]any); m["name"] == name {
			return m
		}
	}
	t.Fatalf("no failure %q in %v", name, out["failures"])
	return nil
}

// The whole shape on a PR's head: an Actions job (its failed step and the
// excerpt of its log that shows the failure), a third-party app's check (its
// output and annotations only, failures first), and the rest counted —
// passing, pending, and ignored.
func TestCheckFailuresOnAPRHead(t *testing.T) {
	f, kit, n := world(t)
	head := f.HeadSHA("acme/w", n)
	f.CheckWith("acme/w", head, "test", ghfake.CheckOpts{Conclusion: "failure", Log: goTestLog,
		Steps: []ghfake.JobStep{{Name: "Set up job", Conclusion: "success"}, {Name: "Run actions/checkout@v4", Conclusion: "success"},
			{Name: "Run tests", Conclusion: "failure"}, {Name: "Post Run actions/checkout@v4", Conclusion: "success"}},
		Annotations: []ghfake.Annotation{{Path: ".github", StartLine: 1, EndLine: 1, Level: "failure", Message: "Process completed with exit code 1."}}})
	f.CheckWith("acme/w", head, "external-ci", ghfake.CheckOpts{Conclusion: "timed_out", App: "external-ci",
		Output: ghfake.CheckOutput{Title: "2 of 40 failed", Summary: "TestA\u202e and TestB\x07 failed", Text: "details"},
		Annotations: []ghfake.Annotation{
			{Path: "a_test.go", StartLine: 7, EndLine: 7, Level: "warning", Message: "slow"},
			{Path: "b_test.go", StartLine: 9, EndLine: 11, Level: "failure", Title: "TestB", Message: "want 2, got 3"},
		}})
	f.Check("acme/w", head, "build", "success")
	f.Check("acme/w", head, "docs", "failure")
	f.StartCheck("acme/w", head, "slow-e2e", time.Now())

	out := invokeCheckFailures(t, kit, map[string]any{"pr": n, "ignore": []any{"DOCS"}})
	if out["sha"] != head || out["passing"] != 1 || out["ignored"] != 1 || out["omitted"] != 0 || out["truncated"] != false {
		t.Fatalf("summary = sha %v passing %v ignored %v omitted %v truncated %v", out["sha"], out["passing"], out["ignored"], out["omitted"], out["truncated"])
	}
	if p := fmt.Sprint(out["pending"]); p != "[slow-e2e]" {
		t.Fatalf("pending = %s", p)
	}
	if names := failureNames(out); names != "external-ci,test" {
		t.Fatalf("failures = %s, want external-ci,test (by name, docs ignored)", names)
	}

	job := failureNamed(t, out, "test")
	if job["app"] != "github-actions" || job["conclusion"] != "failure" || job["failed_step"] != "Run tests" || job["run_id"] == nil || job["job_id"] == nil {
		t.Fatalf("actions entry = %v", job)
	}
	wantLog := "##[group]Run go test ./...\ngo test ./...\ndownloading 100%\n--- FAIL: TestParse (0.00s)\n" +
		"    parse_test.go:42: got \"b\", want \"a\"\nFAIL\tgithub.example/acme/w/parse\t0.004s\n##[error]Process completed with exit code 1."
	if job["log"] != wantLog {
		t.Fatalf("log excerpt =\n%q\nwant\n%q", job["log"], wantLog)
	}
	if _, cut := job["log_truncated"]; cut {
		t.Fatal("a log that fits is not truncated")
	}

	ext := failureNamed(t, out, "external-ci")
	if ext["app"] != "external-ci" || ext["title"] != "2 of 40 failed" || ext["summary"] != "TestA and TestB failed" || ext["text"] != "details" {
		t.Fatalf("third-party entry = %v", ext)
	}
	if _, ok := ext["log"]; ok {
		t.Fatal("a non-Actions check has no log")
	}
	if a := fmt.Sprint(ext["annotations"]); !strings.HasPrefix(a, "[map[end_line:11 level:failure message:want 2, got 3 path:b_test.go start_line:9 title:TestB]") {
		t.Fatalf("annotations (failures first) = %s", a)
	}

	// The log came from the log host, which never saw the GitHub token.
	reads := f.LogReads()
	if len(reads) != 1 || reads[0].Status != http.StatusOK || reads[0].Authorization != "" {
		t.Fatalf("log host reads = %+v, want one 200 without Authorization", reads)
	}
}

func failureNames(out map[string]any) string {
	var names []string
	for _, f := range out["failures"].([]any) {
		names = append(names, f.(map[string]any)["name"].(string))
	}
	return strings.Join(names, ",")
}

// A fixer re-checking after its push reads the PR's NEW head, uncached: the
// failures of the commit it replaced are not reported against it.
func TestCheckFailuresAfterAPushReadsTheNewHead(t *testing.T) {
	f, kit, n := world(t)
	old := f.HeadSHA("acme/w", n)
	f.Check("acme/w", old, "test", "failure")
	if out := invokeCheckFailures(t, kit, map[string]any{"pr": n}); failureNames(out) != "test" {
		t.Fatalf("before the push: %v", out)
	}
	newHead := f.Push("acme/w", "feat", "me", "fix the parser")
	runID := f.StartCheck("acme/w", newHead, "test", time.Now())
	out := invokeCheckFailures(t, kit, map[string]any{"pr": n})
	if out["sha"] != newHead || failureNames(out) != "" || fmt.Sprint(out["pending"]) != "[test]" {
		t.Fatalf("after the push: %v", out)
	}
	f.CompleteRerun("acme/w", runID, "success")
	out = invokeCheckFailures(t, kit, map[string]any{"pr": n})
	if failureNames(out) != "" || out["passing"] != 1 || fmt.Sprint(out["pending"]) != "[]" {
		t.Fatalf("after it went green: %v", out)
	}
}

// run_id reads one workflow run's checks and nothing else on the commit; a
// re-run reports its latest attempt.
func TestCheckFailuresForOneRunAndItsLatestAttempt(t *testing.T) {
	f, kit, n := world(t)
	head := f.HeadSHA("acme/w", n)
	runID, firstJob := f.CheckWith("acme/w", head, "test", ghfake.CheckOpts{Conclusion: "failure", Log: goTestLog,
		Steps: []ghfake.JobStep{{Name: "Run tests", Conclusion: "failure"}}})
	f.Check("acme/w", head, "lint", "failure")

	out := invokeCheckFailures(t, kit, map[string]any{"run_id": runID})
	if out["sha"] != head || failureNames(out) != "test" {
		t.Fatalf("run %d: %v", runID, out)
	}
	if job := failureNamed(t, out, "test"); job["job_id"] != firstJob || job["run_id"] != runID {
		t.Fatalf("first attempt = %v, want job %d of run %d", job, firstJob, runID)
	}

	// A failed-jobs re-run fails the same way: the latest attempt — a new job —
	// is what is reported, with the same failed step.
	if _, err := kit.Invoke(context.Background(), "rerun_run", map[string]any{"repo": "acme/w", "run_id": runID, "failed_only": true}); err != nil {
		t.Fatal(err)
	}
	f.CompleteRerun("acme/w", runID, "failure")
	out = invokeCheckFailures(t, kit, map[string]any{"run_id": runID})
	job := failureNamed(t, out, "test")
	if job["job_id"] == firstJob || job["failed_step"] != "Run tests" || !strings.Contains(job["log"].(string), "--- FAIL: TestParse") {
		t.Fatalf("re-run attempt = %v", job)
	}
	// A re-run that passes leaves nothing failing.
	if _, err := kit.Invoke(context.Background(), "rerun_run", map[string]any{"repo": "acme/w", "run_id": runID, "failed_only": true}); err != nil {
		t.Fatal(err)
	}
	f.CompleteRerun("acme/w", runID, "success")
	if out := invokeCheckFailures(t, kit, map[string]any{"run_id": runID}); failureNames(out) != "" || out["passing"] != 1 {
		t.Fatalf("after a green re-run: %v", out)
	}
}

// The caps: one job's log within log_bytes (its step header kept, the middle
// cut), all text within max_bytes, and what did not fit marked.
func TestCheckFailuresStaysWithinItsCaps(t *testing.T) {
	f, kit, n := world(t)
	head := f.HeadSHA("acme/w", n)
	var big strings.Builder
	big.WriteString("2026-11-02T10:00:00.0000000Z ##[group]Run make test\n")
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&big, "2026-11-02T10:00:01.0000000Z ok   case %05d\n", i)
	}
	big.WriteString("2026-11-02T10:00:02.0000000Z FAIL case 05000\n2026-11-02T10:00:02.1000000Z ##[error]Process completed with exit code 2.\n")
	for _, name := range []string{"a", "b", "c", "d"} {
		f.CheckWith("acme/w", head, name, ghfake.CheckOpts{Conclusion: "failure", Log: big.String(),
			Output: ghfake.CheckOutput{Summary: strings.Repeat("s", 5000)}})
	}

	out := invokeCheckFailures(t, kit, map[string]any{"pr": n, "log_bytes": 1000, "max_bytes": 5000})
	if out["truncated"] != true {
		t.Fatal("cut text must be marked truncated")
	}
	total := 0
	for _, e := range out["failures"].([]any) {
		m := e.(map[string]any)
		for _, k := range []string{"title", "summary", "text", "log"} {
			if s, ok := m[k].(string); ok {
				total += len(s)
			}
		}
		if s, ok := m["log"].(string); ok && len(s) > 1000 {
			t.Fatalf("%s: log is %d bytes, over log_bytes 1000", m["name"], len(s))
		}
	}
	if total > 5000 {
		t.Fatalf("returned %d bytes of text, over max_bytes 5000", total)
	}
	a := failureNamed(t, out, "a")
	log := a["log"].(string)
	if !strings.HasPrefix(log, "##[group]Run make test\n…[") || !strings.HasSuffix(log, "FAIL case 05000\n##[error]Process completed with exit code 2.") || a["log_truncated"] != true {
		t.Fatalf("a long step keeps its header and its end:\n%s", log)
	}
	if d := failureNamed(t, out, "d"); d["log"] != nil && d["log_omitted"] != true {
		t.Fatalf("past the budget a check is listed without its text: %v", d)
	}

	// Options past their ceilings are clamped, not refused.
	out = invokeCheckFailures(t, kit, map[string]any{"pr": n, "log_bytes": 1 << 30, "max_bytes": "999999999"})
	if len(failureNamed(t, out, "a")["log"].(string)) > MaxFailureLogBytes {
		t.Fatal("log_bytes is capped at MaxFailureLogBytes")
	}
}

func TestCheckFailuresNeedsATarget(t *testing.T) {
	_, kit, _ := world(t)
	if _, err := kit.Invoke(context.Background(), "check_failures", map[string]any{"repo": "acme/w"}); err == nil {
		t.Fatal("no pr, ref, or run_id must be an error")
	}
}

// A log the credential may not read (or that retention expired) is noted on
// its entry; the rest of the answer stands.
func TestCheckFailuresNotesAnUnreadableLog(t *testing.T) {
	logHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error>signed url " + r.URL.RawQuery + "</Error>"))
	}))
	defer logHost.Close()
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/r/commits/abc/check-runs":
			fmt.Fprint(w, `{"total_count":2,"check_runs":[
				{"id":5,"name":"unit","status":"completed","conclusion":"failure","html_url":"https://github.example/o/r/runs/5","app":{"slug":"github-actions"},"output":{}},
				{"id":6,"name":"e2e","status":"completed","conclusion":"failure","html_url":"https://github.example/o/r/runs/6","app":{"slug":"github-actions"},"output":{}}]}`)
		case "/repos/o/r/actions/jobs/5", "/repos/o/r/actions/jobs/6":
			fmt.Fprint(w, `{"run_id":9,"steps":[]}`)
		case "/repos/o/r/actions/jobs/5/logs":
			http.Redirect(w, r, logHost.URL+"/blob?sig=secret", http.StatusFound)
		case "/repos/o/r/actions/jobs/6/logs":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	kit, err := NewClient(Config{WriteToken: "tok", APIBase: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	out, err := kit.Invoke(context.Background(), "check_failures", map[string]any{"repo": "o/r", "ref": "abc"})
	if err != nil {
		t.Fatal(err)
	}
	unit := failureNamed(t, out, "unit")
	if e, _ := unit["log_error"].(string); !strings.HasPrefix(e, "HTTP 403 (a job log needs") || strings.Contains(e, "secret") {
		t.Fatalf("unit log_error = %q (the log host's body, which echoes the signed URL, stays out)", e)
	}
	e2e := failureNamed(t, out, "e2e")
	if e, _ := e2e["log_error"].(string); !strings.HasPrefix(e, "HTTP 404: Not Found (a job log needs") {
		t.Fatalf("e2e log_error = %q", e)
	}
}

// The redirect policy of a log fetch: the credential goes no further than the
// API's own origin (a port change included), and never down to http.
func TestLogRedirectPolicy(t *testing.T) {
	mk := func(raw string) *http.Request {
		u, _ := url.Parse(raw)
		r := &http.Request{URL: u, Header: http.Header{"Authorization": {"Bearer tok"}}}
		return r
	}
	api := mk("https://api.github.com/repos/o/r/actions/jobs/1/logs")
	for _, tc := range []struct {
		to       string
		keepAuth bool
		refused  bool
	}{
		{"https://api.github.com/other", true, false},
		{"https://productionresultssa3.blob.core.windows.net/x?sig=1", false, false},
		{"https://api.github.com:8443/x", false, false},
		{"https://api.github.com.evil.example/x", false, false},
		{"http://productionresultssa3.blob.core.windows.net/x", false, true},
	} {
		next := mk(tc.to)
		err := logRedirect(next, []*http.Request{api})
		if (err != nil) != tc.refused {
			t.Errorf("%s: err = %v, refused want %v", tc.to, err, tc.refused)
		}
		if got := next.Header.Get("Authorization") != ""; !tc.refused && got != tc.keepAuth {
			t.Errorf("%s: Authorization kept = %v, want %v", tc.to, got, tc.keepAuth)
		}
	}
	hops := []*http.Request{api, api, api, api, api}
	if err := logRedirect(mk("https://api.github.com/x"), hops); err == nil {
		t.Error("a sixth hop must be refused")
	}
}

func TestLogExcerpt(t *testing.T) {
	for _, tc := range []struct {
		name, log string
		limit     int
		want      string
		cut       bool
	}{
		{"no error: the tail", "a\nb\nc\nd\n", 4, "c\nd", true},
		{"no error, fits", "a\nb\n", 100, "a\nb", false},
		{"the failed step, not an earlier step that logged errors and carried on",
			"##[group]Run actions/cache@v4\n##[error]tar: File exists\n##[error]tar: File exists\nok\n##[group]Run make test\n--- FAIL: TestX\n##[error]Process completed with exit code 1.\nPost job cleanup.\n##[error]post noise\n", 100,
			"##[group]Run make test\n--- FAIL: TestX\n##[error]Process completed with exit code 1.", false},
		{"one failed step, the steps after it left out",
			"##[group]Run one\nok\n##[group]Run two\nboom\n##[error]bad\nmore\nPost job cleanup.\nx\n", 100,
			"##[group]Run two\nboom\n##[error]bad\nmore", false},
		{"errors only in the post phase: the first such step",
			"##[group]Run one\nok\nPost job cleanup.\n##[error]save failed\nPost job cleanup.\n##[error]other\n", 100,
			"Post job cleanup.\n##[error]save failed", false},
		{"no post phase: the last errored step",
			"##[group]Run a\n##[error]x\n##[group]Run b\n##[error]y\n", 100, "##[group]Run b\n##[error]y", false},
		{"an error before any step", "setting up\n##[error]no runner\nPost job cleanup.\nx\n", 100, "setting up\n##[error]no runner", false},
		{"endgroup markers dropped", "##[group]Run x\n##[endgroup]\n##[error]e\n", 100, "##[group]Run x\n##[error]e", false},
		{"progress bar: the last redraw", "10%\r50%\r100%\n", 100, "100%", false},
		{"OSC hyperlink and lone escapes", "\x1b]8;;https://x.example\x07link\x1b]8;;\x07 \x1bcdone\n", 100, "link done", false},
		{"controls, bidi and invalid UTF-8", "a\x00b\u202ec\xffd\n", 100, "abc\ufffdd", false},
		{"a line longer than the limit", strings.Repeat("x", 50) + "\n", 12, "xxx …[cut]", true},
		{"windows line endings", "a\r\nb\r\n", 100, "a\nb", false},
	} {
		got, cut, err := logExcerpt(strings.NewReader(tc.log), tc.limit)
		if err != nil || got != tc.want || cut != tc.cut {
			t.Errorf("%s: got %q cut %v err %v, want %q cut %v", tc.name, got, cut, err, tc.want, tc.cut)
		}
		if len(got) > tc.limit {
			t.Errorf("%s: %d bytes, over the %d limit", tc.name, len(got), tc.limit)
		}
	}
}

func TestIgnoreListShapes(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{nil, "[]"}, {"", "[]"}, {"<no value>", "[]"}, {"[]", "[]"},
		{[]any{"docs", " lint "}, "[docs lint]"}, {"docs, lint", "[docs lint]"}, {[]string{"a"}, "[a]"},
	} {
		if got := fmt.Sprint(append([]string{}, ignoreList(tc.in)...)); got != tc.want {
			t.Errorf("ignoreList(%#v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
