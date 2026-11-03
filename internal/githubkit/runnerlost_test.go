package githubkit

import (
	"context"
	"testing"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghfake"
)

func getRun(t *testing.T, kit *Client, opts map[string]any) map[string]any {
	t.Helper()
	opts["repo"] = "acme/w"
	out, err := kit.Invoke(context.Background(), "get_run", opts)
	if err != nil {
		t.Fatalf("get_run %v: %v", opts, err)
	}
	return out
}

// get_run's pr option says whether the run is still for the PR's head.
func TestGetRunStaleAgainstThePRHead(t *testing.T) {
	f, kit, n := world(t)
	old := f.HeadSHA("acme/w", n)
	run, _ := f.Check("acme/w", old, "test", "failure")
	out := getRun(t, kit, map[string]any{"run_id": run, "pr": n})
	if out["stale"] != false || out["pr_head_sha"] != old {
		t.Fatalf("a run for the PR's head: %v", out)
	}
	head := f.Push("acme/w", "feat", "me", "fix the parser")
	out = getRun(t, kit, map[string]any{"run_id": run, "pr": n})
	if out["stale"] != true || out["pr_head_sha"] != head || out["head_sha"] != old {
		t.Fatalf("a run for a commit the PR has moved past: %v", out)
	}
	// Without pr, nothing is read about the PR.
	if out := getRun(t, kit, map[string]any{"run_id": run}); out["stale"] != nil || out["pr_head_sha"] != nil {
		t.Fatalf("no pr: %v", out)
	}
}

// get_run's diagnose option says whether a failed job lost its runner.
func TestGetRunDiagnosesALostRunner(t *testing.T) {
	f, kit, n := world(t)
	head := f.HeadSHA("acme/w", n)
	for _, c := range []struct {
		name       string
		conclusion string
		message    string
		want       bool
	}{
		{"shutdown signal", "failure", "The runner has received a shutdown signal. This can happen when the runner service is stopped, or a manually started runner is canceled.", true},
		{"self-hosted runner lost", "failure", "The self-hosted runner: ci-7 lost communication with the server. Verify the machine is running and has a healthy network connection.", true},
		{"hosted runner lost", "failure", "The hosted runner: GitHub Actions 12 lost communication with the server.", true},
		{"a test failure", "failure", "Process completed with exit code 1.", false},
		// A cancelled run is triaged as cancelled; its jobs are not read.
		{"cancelled", "cancelled", "The runner has received a shutdown signal.", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			run, _ := f.CheckWith("acme/w", head, c.name, ghfake.CheckOpts{Conclusion: c.conclusion,
				Annotations: []ghfake.Annotation{{Path: ".github", Level: "failure", Message: c.message}}})
			if out := getRun(t, kit, map[string]any{"run_id": run, "diagnose": true}); out["runner_lost"] != c.want {
				t.Fatalf("runner_lost = %v, want %v (%v)", out["runner_lost"], c.want, out)
			}
		})
	}
	// Still running: nothing to diagnose yet.
	run, _ := f.StartRun("acme/w", head, "slow", "")
	if out := getRun(t, kit, map[string]any{"run_id": run, "diagnose": true}); out["runner_lost"] != false {
		t.Fatalf("a running run: %v", out)
	}
}
