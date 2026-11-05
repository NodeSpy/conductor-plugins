package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghfake"
	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestGithubPluginRerunTriage replays, against the stateful GitHub fake, how
// a flaky re-run went wrong. The release build of the plugin runs over the
// bare wire, and its own declared failing_checks remediation is applied with
// the host's rules (RemediateVerdict).
//
// A fix pushed revision B to a PR while the run for A was going. The
// workflow's concurrency group (cancel-in-progress) cancelled A's run when
// B's started. Remediation then re-ran A's run. Re-running a run puts it back
// in its concurrency group, so that cancelled B's run, and B's one flaky
// re-run was spent on that cancellation. B's re-run then lost its runner,
// and nothing re-ran it.
func TestGithubPluginRerunTriage(t *testing.T) {
	bin := rpctest.BuildConnector(t, "github")
	f := ghfake.New(ghfake.Options{Logf: t.Logf})
	f.Start()
	t.Cleanup(f.Close)
	f.AddUser("acme", "Organization")
	f.AddRepo("acme/app")
	f.UserToken("me", "tok-me")
	const repo = "acme/app"
	n := f.OpenPR(repo, "me", ghfake.PROpts{Title: "parser fix", Head: "fix-parser"})
	group := "ci-refs/pull/1/merge"

	c := rpctest.Start(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	decl, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sem *plugin.EventSemantics
	for _, e := range decl.Events {
		if e.Name == "failing_checks" {
			sem = e.Semantics
		}
	}
	if sem == nil || sem.Remediate == nil || sem.RemediateTriage == nil {
		t.Fatalf("failing_checks declares no remediate + remediate_triage: %+v", sem)
	}
	conn := map[string]any{"api_base": f.URL(), "identity": map[string]any{"write_token": "tok-me", "read_token": "tok-me"}}
	// invoke calls a declared verb the way the host does: args templated
	// from the event's facts, which arrive as JSON (numbers are float64).
	invoke := func(verb string, args map[string]string, head string, run int64) map[string]any {
		t.Helper()
		var facts map[string]any
		b, _ := json.Marshal(map[string]any{"repo": repo, "number": n, "head": head, "run_id": run})
		_ = json.Unmarshal(b, &facts)
		opts := map[string]any{}
		for k, v := range args {
			var lit any
			switch name := plugin.FactName(v); {
			case name != v:
				opts[k] = facts[name]
			case json.Unmarshal([]byte(v), &lit) == nil:
				opts[k] = lit
			default:
				opts[k] = v
			}
		}
		out, err := c.Invoke(ctx, plugin.InvokeRequest{Instance: "github1", Verb: verb, Connection: conn, Options: opts})
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		return out
	}
	verdict := func(head string, run int64) string {
		t.Helper()
		out := invoke(sem.Remediate.Status.Verb, sem.Remediate.Status.Args, head, run)
		v, err := sem.RemediateVerdict(out)
		if err != nil {
			t.Fatalf("triage of run %d: %v", run, err)
		}
		return v
	}
	remedy := func(head string, run int64) {
		t.Helper()
		invoke(sem.Remediate.Action.Verb, sem.Remediate.Action.Args, head, run)
	}
	wantRun := func(run int64, status, conclusion string) {
		t.Helper()
		if s, c, _ := f.Run(repo, run); s != status || c != conclusion {
			t.Fatalf("run %d is %s/%s, want %s/%s", run, s, c, status, conclusion)
		}
	}

	shaA := f.HeadSHA(repo, n)
	runA, _ := f.StartRun(repo, shaA, "ci", group)
	if v := verdict(shaA, runA); v != plugin.RemediationWait {
		t.Fatalf("A's run in progress: verdict %q, want wait", v)
	}
	// The fix lands as B; B's run cancels A's.
	shaB := f.Push(repo, "fix-parser", "me", "fix the parser", "parser.go")
	runB, _ := f.StartRun(repo, shaB, "ci", group)
	wantRun(runA, "completed", "cancelled")

	// A's run is stale and cancelled: left alone, so B's run carries on.
	if v := verdict(shaA, runA); v != plugin.RemediationSkip {
		t.Fatalf("A's stale, cancelled run: verdict %q, want skip", v)
	}
	wantRun(runB, "in_progress", "")

	// What the old remediation did instead, and why it hurt: re-running A
	// cancels B's run.
	remedy(shaA, runA)
	wantRun(runB, "completed", "cancelled")
	// B's run, cancelled by someone else: no re-run, no fixer.
	if v := verdict(shaB, runB); v != plugin.RemediationSkip {
		t.Fatalf("B's cancelled run: verdict %q, want skip", v)
	}
	// A's re-run failing is still stale: skipped, not re-run again.
	f.CompleteRun(repo, runA, "failure")
	if v := verdict(shaA, runA); v != plugin.RemediationSkip {
		t.Fatalf("A's stale failed run: verdict %q, want skip", v)
	}

	// B's run goes again (a person re-runs it) and loses its runner: an
	// infrastructure re-run.
	remedy(shaB, runB)
	f.CompleteRun(repo, runB, "failure", ghfake.Annotation{Path: ".github", Level: "failure",
		Message: "The runner has received a shutdown signal. This can happen when the runner service is stopped, or a manually started runner is canceled."})
	if v := verdict(shaB, runB); v != plugin.RemediationInfra {
		t.Fatalf("B's run lost its runner: verdict %q, want infra", v)
	}
	remedy(shaB, runB)
	if s, _, attempt := f.Run(repo, runB); s != "in_progress" || attempt != 3 {
		t.Fatalf("B's run after the infra re-run: %s, attempt %d; want in_progress, attempt 3", s, attempt)
	}

	// A genuine test failure on B is an ordinary failure: the flaky budget,
	// then the fixer.
	f.CompleteRun(repo, runB, "failure", ghfake.Annotation{Path: "parser_test.go", StartLine: 9, EndLine: 9, Level: "failure",
		Message: "want 2, got 3"})
	if v := verdict(shaB, runB); v != plugin.RemediationRemedy {
		t.Fatalf("B's test failure: verdict %q, want remedy", v)
	}

	if u := f.Unhandled(); len(u) > 0 {
		t.Fatalf("the fake was asked for endpoints it does not implement: %v", u)
	}
}
