package ghfake

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// The admin API drives the same model the scenario API does.
func TestAdminAPI(t *testing.T) {
	f := New(Options{Logf: t.Logf})
	f.Start()
	t.Cleanup(f.Close)
	sink := &hookSink{}
	hs := httptest.NewServer(sink)
	t.Cleanup(hs.Close)

	seed := map[string]any{
		"app":      map[string]any{"id": 9, "slug": "c"},
		"users":    []map[string]any{{"login": "acme", "type": "Organization"}},
		"tokens":   map[string]string{"tok-me": "me"},
		"installs": []map[string]any{{"account": "acme"}},
		"repos":    []map[string]any{{"name": "acme/w", "collaborators": []string{"rev"}}},
		"hooks":    []map[string]any{{"url": hs.URL, "secret": "s", "repos": []string{"acme/w"}}},
		"prs": []map[string]any{
			{"repo": "acme/w", "author": "me", "head": "a", "reviewers": []string{"rev"}},
			{"repo": "acme/w", "author": "me", "head": "b", "conflict": true},
		},
	}
	if resp, b := do(t, f, "POST", "/_fake/seed", "", seed); resp.StatusCode != 200 {
		t.Fatalf("seed: %d %s", resp.StatusCode, b)
	}
	f.Flush()
	if got := sink.events(); len(got) != 0 {
		t.Fatalf("seeding delivered %v", got)
	}
	if got := f.RequestedReviewers("acme/w", 1); len(got) != 1 || got[0] != "rev" {
		t.Fatalf("seeded reviewers = %v", got)
	}
	_, b := do(t, f, "GET", "/repos/acme/w/pulls/2", "tok-me", nil)
	var pr map[string]any
	_ = json.Unmarshal(b, &pr)
	if pr["mergeable_state"] != "dirty" {
		t.Fatalf("seeded conflict: mergeable_state = %v", pr["mergeable_state"])
	}

	resp, b := do(t, f, "POST", "/_fake/act", "", map[string]any{"op": "comment", "repo": "acme/w", "number": 1, "by": "rev", "body": "hi"})
	if resp.StatusCode != 200 {
		t.Fatalf("act: %d %s", resp.StatusCode, b)
	}
	f.Flush()
	if got := sink.events(); len(got) != 1 || got[0] != "issue_comment.created" {
		t.Fatalf("deliveries = %v", got)
	}
	_, b = do(t, f, "GET", "/_fake/q?what=comments&repo=acme/w&number=1", "", nil)
	var cs []map[string]any
	if err := json.Unmarshal(b, &cs); err != nil || len(cs) != 1 {
		t.Fatalf("comments query = %s", b)
	}
	if resp, _ := do(t, f, "POST", "/_fake/act", "", map[string]any{"op": "nope"}); resp.StatusCode != 400 {
		t.Fatalf("unknown op answered %d", resp.StatusCode)
	}
	if resp, _ := do(t, f, "POST", "/_fake/act", "", map[string]any{"op": "comment", "repo": "acme/none", "number": 1, "by": "x"}); resp.StatusCode != 400 {
		t.Fatalf("a scenario mistake answered %d", resp.StatusCode)
	}
}
