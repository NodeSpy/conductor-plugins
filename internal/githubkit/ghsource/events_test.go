package ghsource

import (
	"context"
	"testing"
)

// as1 wraps single actions into one-variant ActionSets, keeping the many
// single-action test configs terse after the multi-variant (ActionSet) change.
func as1(m map[string]Action) map[string]ActionSet {
	out := make(map[string]ActionSet, len(m))
	for k, v := range m {
		out[k] = ActionSet{v}
	}
	return out
}

// newTestIntegration builds a Source from an in-memory Config.
func newTestIntegration(t *testing.T, cfg Config) *Source {
	t.Helper()
	g, err := New("test", cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func baseConfig() Config {
	return Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Rules: []Rule{{
			Match:    Match{Repos: []string{"acme/*"}},
			Reviewer: Actors{Logins: []string{"me"}},
			Assignee: Actors{Logins: []string{"me"}},
			Actions: as1(map[string]Action{
				"changes_requested":     {},
				"new_comment":           {},
				"issue_matched":         {},
				"release":               {},
				"deployment_status":     {},
				"dependabot_alert":      {},
				"secret_scanning_alert": {},
			}),
		}},
	}
}

func TestChangesRequested(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	body := []byte(`{
		"action":"submitted",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"pull_request":{"number":7,"head":{"sha":"abc123"},"base":{"ref":"main"},"user":{"login":"me"}},
		"review":{"state":"changes_requested","id":99,"user":{"login":"reviewer"}}
	}`)
	trs := g.triggersFor(context.Background(), "pull_request_review", body)
	if len(trs) != 1 {
		t.Fatalf("want 1 trigger, got %d", len(trs))
	}
	if trs[0].Kind != "changes_requested" || trs[0].Target.PR != 7 || trs[0].Target.HeadSHA != "abc123" {
		t.Fatalf("unexpected trigger: %+v", trs[0])
	}
	if trs[0].Action == nil {
		t.Fatal("resolved action not attached")
	}
}

func TestProjectMapStampsTarget(t *testing.T) {
	cfg := baseConfig()
	// Key casing differs from the incoming repo to exercise case-insensitivity.
	cfg.ProjectMap = map[string]string{"Acme/Widget": "acme-internal/widget"}
	g := newTestIntegration(t, cfg)
	body := []byte(`{
		"action":"submitted",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"pull_request":{"number":7,"head":{"sha":"abc123"},"base":{"ref":"main"},"user":{"login":"me"}},
		"review":{"state":"changes_requested","id":99,"user":{"login":"reviewer"}}
	}`)
	trs := g.triggersFor(context.Background(), "pull_request_review", body)
	if len(trs) != 1 {
		t.Fatalf("want 1 trigger, got %d", len(trs))
	}
	tgt := trs[0].Target
	if tgt.Repo != "acme/widget" {
		t.Fatalf("forge repo must be preserved, got %q", tgt.Repo)
	}
	if tgt.Project != "acme-internal/widget" {
		t.Fatalf("Project should be remapped, got %q", tgt.Project)
	}
	if got := tgt.CheckoutRepo(); got != "acme-internal/widget" {
		t.Fatalf("CheckoutRepo should use Project, got %q", got)
	}
}

func TestProjectMapUnsetLeavesTargetRepo(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	tgt := g.target("acme/widget", 7, "abc", "main", "")
	if tgt.Project != "" {
		t.Fatalf("unmapped repo should leave Project empty, got %q", tgt.Project)
	}
	if tgt.CheckoutRepo() != "acme/widget" {
		t.Fatalf("CheckoutRepo should fall back to Repo, got %q", tgt.CheckoutRepo())
	}
}

func TestProjectRewrite(t *testing.T) {
	cases := []struct {
		name    string
		rewrite ProjectRewrite
		repo    string
		want    string // expected Target.Project ("" = falls back to Repo)
	}{
		{"org remap normalizes case", ProjectRewrite{Org: "acme"}, "AcmeCorp/Widget", "acme/widget"},
		{"noop when already normalized", ProjectRewrite{Org: "acme"}, "acme/widget", ""},
		{"noop is case-insensitive", ProjectRewrite{Org: "acme"}, "Acme/Widget", ""},
		{"inactive rewrite", ProjectRewrite{}, "AcmeCorp/Widget", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.ProjectRewrite = tc.rewrite
			g := newTestIntegration(t, cfg)
			if got := g.mapProject(tc.repo); got != tc.want {
				t.Fatalf("mapProject(%q) = %q, want %q", tc.repo, got, tc.want)
			}
		})
	}
}

func TestProjectMapWinsOverRewrite(t *testing.T) {
	// An explicit per-repo mapping takes precedence over the org-wide rewrite.
	cfg := baseConfig()
	cfg.ProjectMap = map[string]string{"AcmeCorp/Special": "custom/project"}
	cfg.ProjectRewrite = ProjectRewrite{Org: "acme"}
	g := newTestIntegration(t, cfg)
	if got := g.mapProject("AcmeCorp/Special"); got != "custom/project" {
		t.Fatalf("explicit map should win, got %q", got)
	}
	// A repo without an explicit entry still gets the rewrite.
	if got := g.mapProject("AcmeCorp/Other"); got != "acme/other" {
		t.Fatalf("rewrite should apply to unlisted repos, got %q", got)
	}
}

func TestReviewApprovedIgnored(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	body := []byte(`{
		"action":"submitted",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"pull_request":{"number":7,"head":{"sha":"abc"}},
		"review":{"state":"approved","id":1,"user":{"login":"reviewer"}}
	}`)
	if trs := g.triggersFor(context.Background(), "pull_request_review", body); len(trs) != 0 {
		t.Fatalf("approved review should not trigger, got %d", len(trs))
	}
}

func TestNewCommentIgnoresSelf(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	// Comment authored by "me" (a reviewer/assignee login) must be ignored.
	self := []byte(`{
		"action":"created",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
		"comment":{"id":11,"user":{"login":"me"},"body":"hi"}
	}`)
	if trs := g.triggersFor(context.Background(), "issue_comment", self); len(trs) != 0 {
		t.Fatalf("self comment should be ignored, got %d", len(trs))
	}
	// Comment by someone else on a PR should trigger.
	other := []byte(`{
		"action":"created",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
		"comment":{"id":12,"user":{"login":"bot"},"body":"please fix"}
	}`)
	trs := g.triggersFor(context.Background(), "issue_comment", other)
	if len(trs) != 1 || trs[0].Kind != "new_comment" {
		t.Fatalf("want 1 new_comment, got %+v", trs)
	}
	// The source comment id must be stamped so the engine's high-water mark (and
	// the sweep's missed-comment recovery) can gate on it.
	if got, _ := trs[0].Context["comment_id"].(int64); got != 12 {
		t.Fatalf("comment_id not stamped, got %v", trs[0].Context["comment_id"])
	}
	if got := trs[0].Context["comment_kind"]; got != CommentKindIssue {
		t.Fatalf("issue_comment should be stamped comment_kind=issue, got %v", got)
	}
}

// An inline review comment is stamped comment_kind=review so the engine gates it
// against the review high-water mark, not the (far higher) issue-comment one.
func TestNewCommentReviewCommentKind(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	body := []byte(`{
		"action":"created",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"pull_request":{"number":3,"html_url":"u","head":{"sha":"h","ref":"feat"},"base":{"ref":"main"},"user":{"login":"me"}},
		"comment":{"id":3918412084,"user":{"login":"reviewer"},"body":"nit"}
	}`)
	trs := g.triggersFor(context.Background(), "pull_request_review_comment", body)
	if len(trs) != 1 || trs[0].Kind != "new_comment" {
		t.Fatalf("want 1 new_comment, got %+v", trs)
	}
	if got, _ := trs[0].Context["comment_id"].(int64); got != 3918412084 {
		t.Fatalf("comment_id not stamped, got %v", trs[0].Context["comment_id"])
	}
	if got := trs[0].Context["comment_kind"]; got != CommentKindReview {
		t.Fatalf("pull_request_review_comment should be stamped comment_kind=review, got %v", got)
	}
}

func TestNewCommentIgnoreUsers(t *testing.T) {
	cfg := baseConfig()
	cfg.Rules[0].Actions = as1(map[string]Action{
		"new_comment": {IgnoreUsers: []string{"github-actions[bot]"}},
	})
	g := newTestIntegration(t, cfg)

	// A CI report bot on the ignore list must NOT trigger new_comment.
	report := []byte(`{
		"action":"created",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
		"comment":{"id":20,"user":{"login":"github-actions[bot]"},"body":"All tests passed!"}
	}`)
	if trs := g.triggersFor(context.Background(), "issue_comment", report); len(trs) != 0 {
		t.Fatalf("ignored CI bot comment should not trigger, got %d", len(trs))
	}
	// A human comment still triggers.
	human := []byte(`{
		"action":"created",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
		"comment":{"id":21,"user":{"login":"reviewer"},"body":"please fix"}
	}`)
	if trs := g.triggersFor(context.Background(), "issue_comment", human); len(trs) != 1 {
		t.Fatalf("non-ignored comment should trigger, got %d", len(trs))
	}
}

func TestRepoNotMatchedNoTrigger(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	body := []byte(`{
		"action":"submitted",
		"repository":{"full_name":"other/repo","name":"repo","owner":{"login":"other"}},
		"pull_request":{"number":1,"head":{"sha":"x"}},
		"review":{"state":"changes_requested","id":1,"user":{"login":"r"}}
	}`)
	if trs := g.triggersFor(context.Background(), "pull_request_review", body); len(trs) != 0 {
		t.Fatalf("unmatched repo should not trigger, got %d", len(trs))
	}
}

func TestDeploymentStatusFailure(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	body := func(state string) []byte {
		return []byte(`{"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			"deployment":{"sha":"dsha","ref":"main"},
			"deployment_status":{"state":"` + state + `","environment":"production","target_url":"https://x/deploy"}}`)
	}
	trs := g.triggersFor(context.Background(), "deployment_status", body("failure"))
	if len(trs) != 1 || trs[0].Kind != "deployment_status" {
		t.Fatalf("want deployment_status on failure, got %+v", trs)
	}
	if trs[0].Context["environment"] != "production" || trs[0].Target.HeadSHA != "dsha" {
		t.Fatalf("unexpected context/target: %+v", trs[0])
	}
	// success is ignored.
	if trs := g.triggersFor(context.Background(), "deployment_status", body("success")); len(trs) != 0 {
		t.Fatalf("success deployment should not fire, got %d", len(trs))
	}
}

func TestDependabotAndSecretAlerts(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	dep := []byte(`{"action":"created","repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"alert":{"number":7,"html_url":"https://x/da/7","dependency":{"package":{"name":"golang.org/x/net"}},
			"security_advisory":{"severity":"high","summary":"DoS"}}}`)
	trs := g.triggersFor(context.Background(), "dependabot_alert", dep)
	if len(trs) != 1 || trs[0].Kind != "dependabot_alert" || trs[0].Dedup != "dependabot:7" {
		t.Fatalf("want dependabot_alert #7, got %+v", trs)
	}
	if trs[0].Context["severity"] != "high" || trs[0].Context["package"] != "golang.org/x/net" {
		t.Fatalf("dependabot context wrong: %+v", trs[0].Context)
	}
	// A non-created action (e.g. dismissed) does not fire.
	dismissed := []byte(`{"action":"dismissed","repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},"alert":{"number":7}}`)
	if trs := g.triggersFor(context.Background(), "dependabot_alert", dismissed); len(trs) != 0 {
		t.Fatalf("dismissed dependabot alert should not fire, got %d", len(trs))
	}

	sec := []byte(`{"action":"created","repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"alert":{"number":3,"secret_type_display_name":"AWS Access Key","html_url":"https://x/ss/3"}}`)
	trs = g.triggersFor(context.Background(), "secret_scanning_alert", sec)
	if len(trs) != 1 || trs[0].Kind != "secret_scanning_alert" || trs[0].Context["secret_type"] != "AWS Access Key" {
		t.Fatalf("want secret_scanning_alert with type, got %+v", trs)
	}
}

func TestReleasePublished(t *testing.T) {
	g := newTestIntegration(t, baseConfig()) // release action, no include_prereleases
	body := func(action string, pre, draft bool) []byte {
		return []byte(`{
			"action":"` + action + `",
			"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			"release":{"tag_name":"v1.2.3","name":"1.2.3","html_url":"https://x/releases/v1.2.3",
				"target_commitish":"main","prerelease":` + b2s(pre) + `,"draft":` + b2s(draft) + `}
		}`)
	}

	// A published, non-prerelease, non-draft release fires and exposes tag_name.
	trs := g.triggersFor(context.Background(), "release", body("published", false, false))
	if len(trs) != 1 || trs[0].Kind != "release" {
		t.Fatalf("want 1 release trigger, got %+v", trs)
	}
	if trs[0].Dedup != "release:v1.2.3" {
		t.Fatalf("dedup should be tag-based, got %q", trs[0].Dedup)
	}
	if trs[0].Context["tag_name"] != "v1.2.3" {
		t.Fatalf("tag_name not in context: %+v", trs[0].Context)
	}

	// Prereleases are skipped by default.
	if trs := g.triggersFor(context.Background(), "release", body("published", true, false)); len(trs) != 0 {
		t.Fatalf("prerelease should be skipped by default, got %d", len(trs))
	}
	// Draft publishes and non-published actions never fire.
	if trs := g.triggersFor(context.Background(), "release", body("published", false, true)); len(trs) != 0 {
		t.Fatalf("draft release should not fire, got %d", len(trs))
	}
	if trs := g.triggersFor(context.Background(), "release", body("created", false, false)); len(trs) != 0 {
		t.Fatalf("only published should fire, got %d", len(trs))
	}
}

func TestReleaseIncludePrereleases(t *testing.T) {
	cfg := baseConfig()
	cfg.Rules[0].Actions["release"] = ActionSet{
		{IncludePrereleases: true},
	}
	g := newTestIntegration(t, cfg)
	body := []byte(`{
		"action":"published",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"release":{"tag_name":"v2.0.0-rc1","target_commitish":"main","prerelease":true}
	}`)
	if trs := g.triggersFor(context.Background(), "release", body); len(trs) != 1 {
		t.Fatalf("include_prereleases should fire on a prerelease, got %d", len(trs))
	}
}

// b2s renders a Go bool as a JSON literal for inline test payloads.
func b2s(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestIssueMatchedOnAssign(t *testing.T) {
	g := newTestIntegration(t, baseConfig()) // issue_matched, no filters → me-assignee gate only
	// Assigned to me (issue.assignees carries the current set) → fires.
	body := []byte(`{
		"action":"assigned",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"issue":{"number":42,"assignees":[{"login":"me"}]},
		"assignee":{"login":"me"}
	}`)
	trs := g.triggersFor(context.Background(), "issues", body)
	if len(trs) != 1 || trs[0].Kind != "issue_matched" || trs[0].Target.Issue != 42 {
		t.Fatalf("want issue_matched for #42, got %+v", trs)
	}
	// Assigned to someone else → no trigger (me-assignee gate).
	body2 := []byte(`{
		"action":"assigned",
		"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
		"issue":{"number":42,"assignees":[{"login":"stranger"}]},
		"assignee":{"login":"stranger"}
	}`)
	if trs := g.triggersFor(context.Background(), "issues", body2); len(trs) != 0 {
		t.Fatalf("assignment to another user should not trigger, got %d", len(trs))
	}
}

// A merged/closed PR has no branch left to fix: review and comment events that
// keep arriving after the merge (a late approval, a reply) must not launch a
// fixer. The same events on an open PR still do.
func TestClosedPRDropsBranchKinds(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	for _, tc := range []struct {
		name, event, body string
		want              int
	}{
		{"review on closed PR", "pull_request_review", `{
			"action":"submitted",
			"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			"pull_request":{"number":7,"state":"closed","merged":true,"head":{"sha":"abc"},"base":{"ref":"main"},"user":{"login":"me"}},
			"review":{"state":"changes_requested","id":99,"user":{"login":"reviewer"}}}`, 0},
		{"review comment on closed PR", "pull_request_review_comment", `{
			"action":"created",
			"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			"pull_request":{"number":7,"state":"closed","head":{"sha":"abc","ref":"feat"},"base":{"ref":"main"},"user":{"login":"me"}},
			"comment":{"id":21,"user":{"login":"reviewer"},"body":"one more thing"}}`, 0},
		{"conversation comment on closed PR", "issue_comment", `{
			"action":"created",
			"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			"issue":{"number":3,"state":"closed","pull_request":{},"user":{"login":"me"}},
			"comment":{"id":22,"user":{"login":"reviewer"},"body":"please fix"}}`, 0},
		{"review on open PR", "pull_request_review", `{
			"action":"submitted",
			"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			"pull_request":{"number":7,"state":"open","head":{"sha":"abc"},"base":{"ref":"main"},"user":{"login":"me"}},
			"review":{"state":"changes_requested","id":99,"user":{"login":"reviewer"}}}`, 1},
		{"conversation comment on open PR", "issue_comment", `{
			"action":"created",
			"repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			"issue":{"number":3,"state":"open","pull_request":{},"user":{"login":"me"}},
			"comment":{"id":23,"user":{"login":"reviewer"},"body":"please fix"}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if trs := g.triggersFor(context.Background(), tc.event, []byte(tc.body)); len(trs) != tc.want {
				t.Fatalf("want %d trigger(s), got %+v", tc.want, trs)
			}
		})
	}
}
