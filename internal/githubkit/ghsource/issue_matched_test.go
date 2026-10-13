package ghsource

import (
	"context"
	"testing"
)

// issueMatchedConfig builds a config whose issue_matched has the given variants,
// with "me" as the self identity.
func issueMatchedConfig(variants ...Action) Config {
	return Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: "https://smee.io/x", Secret: "s"},
		Rules: []Rule{{
			Match:   Match{Repos: []string{"acme/*"}},
			Me:      Actors{Logins: []string{"me"}},
			Actions: map[string]ActionSet{"issue_matched": variants},
		}},
	}
}

// issueBody builds an `issues` webhook body with the given assignees + labels.
func issueBody(action, author string, assignees, labels []string) string {
	a := "["
	for i, s := range assignees {
		if i > 0 {
			a += ","
		}
		a += `{"login":"` + s + `"}`
	}
	a += "]"
	l := "["
	for i, s := range labels {
		if i > 0 {
			l += ","
		}
		l += `{"name":"` + s + `"}`
	}
	l += "]"
	return `{"action":"` + action + `","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},` +
		`"issue":{"number":7,"title":"do a thing","user":{"login":"` + author + `"},"assignees":` + a + `,"labels":` + l + `}}`
}

func TestIssueMatchedPayloadFilters(t *testing.T) {
	act := Action{
		LabelsAll: []string{"Ready", "backend"},
		Authors:   []string{"boss"},
		Exclude:   Exclude{Labels: []string{"blocked"}},
	}
	g := newTestIntegration(t, issueMatchedConfig(act))

	// All labels present, allowed author, assigned to me, no excluded label → match.
	ok := issueBody("labeled", "boss", []string{"me"}, []string{"Ready", "backend"})
	if k := do(t, g, "issues", ok); len(k) != 1 || k[0] != "issue_matched" {
		t.Fatalf("expected issue_matched, got %v", k)
	}
	// Missing one required label → no match (labels_all).
	if k := do(t, g, "issues", issueBody("labeled", "boss", []string{"me"}, []string{"Ready"})); len(k) != 0 {
		t.Fatalf("labels_all not satisfied should not match, got %v", k)
	}
	// Excluded label present → no match.
	if k := do(t, g, "issues", issueBody("labeled", "boss", []string{"me"}, []string{"Ready", "backend", "blocked"})); len(k) != 0 {
		t.Fatalf("excluded label should block, got %v", k)
	}
	// Wrong author → no match.
	if k := do(t, g, "issues", issueBody("labeled", "someoneelse", []string{"me"}, []string{"Ready", "backend"})); len(k) != 0 {
		t.Fatalf("author not allowed should not match, got %v", k)
	}
	// Not assigned to me → no match (me-gate).
	if k := do(t, g, "issues", issueBody("labeled", "boss", []string{"teammate"}, []string{"Ready", "backend"})); len(k) != 0 {
		t.Fatalf("not assigned to me should not match, got %v", k)
	}
}

func TestIssueMatchedSoleAssignee(t *testing.T) {
	g := newTestIntegration(t, issueMatchedConfig(Action{SoleAssignee: true}))
	if k := do(t, g, "issues", issueBody("assigned", "x", []string{"me"}, nil)); len(k) != 1 {
		t.Fatalf("sole assignee (just me) should match, got %v", k)
	}
	if k := do(t, g, "issues", issueBody("assigned", "x", []string{"me", "teammate"}, nil)); len(k) != 0 {
		t.Fatalf("a co-assignee should fail sole_assignee, got %v", k)
	}
}

func TestIssueMatchedMultiVariant(t *testing.T) {
	g := newTestIntegration(t, issueMatchedConfig(
		Action{Name: "backend", LabelsAll: []string{"Ready", "backend"}},
		Action{Name: "frontend", LabelsAll: []string{"Ready", "frontend"}},
	))
	// A backend-ready issue fires only the backend variant.
	trs := g.triggersFor(context.Background(), "issues", []byte(issueBody("labeled", "x", []string{"me"}, []string{"Ready", "backend"})))
	if len(trs) != 1 || trs[0].Variant != "backend" || trs[0].Kind != "issue_matched" {
		t.Fatalf("expected only the backend variant, got %+v", trs)
	}
	// An issue with BOTH labels fires BOTH variants (distinct routes).
	trs = g.triggersFor(context.Background(), "issues", []byte(issueBody("labeled", "x", []string{"me"}, []string{"Ready", "backend", "frontend"})))
	if len(trs) != 2 {
		t.Fatalf("both variants should fire for a both-labeled issue, got %+v", trs)
	}
}

func TestIssueMatchedGates(t *testing.T) {
	act := Action{
		Gates: map[string]any{"no_branch": true, "project": map[string]any{"Priority": []any{"High", "Urgent"}}}}
	cfg := issueMatchedConfig(act)

	// No branch/PR, Priority=High → passes the gates.
	clean := `{"data":{"repository":{"issue":{
		"linkedBranches":{"totalCount":0},
		"closedByPullRequestsReferences":{"totalCount":0},
		"projectItems":{"nodes":[{"fieldValues":{"nodes":[{"name":"High","field":{"name":"Priority"}}]}}]}
	}}}}`
	g := newTestIntegration(t, cfg)
	g.app = graphqlStub(t, clean)
	g.rest = newRESTClient(g.app)
	body := issueBodyInst(issueBody("labeled", "x", []string{"me"}, nil))
	if k := do(t, g, "issues", body); len(k) != 1 {
		t.Fatalf("clean gates should match, got %v", k)
	}

	// A linked branch exists → no_branch gate fails.
	withBranch := `{"data":{"repository":{"issue":{
		"linkedBranches":{"totalCount":1},
		"closedByPullRequestsReferences":{"totalCount":0},
		"projectItems":{"nodes":[{"fieldValues":{"nodes":[{"name":"High","field":{"name":"Priority"}}]}}]}
	}}}}`
	g2 := newTestIntegration(t, cfg)
	g2.app = graphqlStub(t, withBranch)
	g2.rest = newRESTClient(g2.app)
	if k := do(t, g2, "issues", body); len(k) != 0 {
		t.Fatalf("existing branch should fail no_branch gate, got %v", k)
	}

	// Wrong Priority → project gate fails.
	lowPri := `{"data":{"repository":{"issue":{
		"linkedBranches":{"totalCount":0},
		"closedByPullRequestsReferences":{"totalCount":0},
		"projectItems":{"nodes":[{"fieldValues":{"nodes":[{"name":"Low","field":{"name":"Priority"}}]}}]}
	}}}}`
	g3 := newTestIntegration(t, cfg)
	g3.app = graphqlStub(t, lowPri)
	g3.rest = newRESTClient(g3.app)
	if k := do(t, g3, "issues", body); len(k) != 0 {
		t.Fatalf("Priority=Low should fail the project gate, got %v", k)
	}
}

// issueBodyInst adds an installation id (needed for gate enrichment) to an issue body.
func issueBodyInst(body string) string {
	return `{"installation":{"id":42},` + body[1:]
}
