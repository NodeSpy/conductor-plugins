package ghsource

import (
	"context"
	"testing"
)

func TestRevertRefs(t *testing.T) {
	pr := func(merged bool, title, body string) *prPayload {
		p := &prPayload{Number: 99, Merged: merged, Title: title}
		p.Body = body
		return p
	}
	cases := []struct {
		name string
		pr   *prPayload
		want []int
	}{
		{"github revert flow", pr(true, `Revert "fix the parser"`, "Reverts acme/widget#12"), []int{12}},
		{"multiple", pr(true, "Revert stack", "Reverts acme/widget#12\nReverts acme/widget#13"), []int{12, 13}},
		{"case-insensitive repo", pr(true, "Revert x", "Reverts ACME/Widget#7"), []int{7}},
		{"unmerged revert PR", pr(false, `Revert "x"`, "Reverts acme/widget#12"), nil},
		{"not revert-titled", pr(true, "fix things", "Reverts acme/widget#12"), nil},
		{"cross-repo revert", pr(true, "Revert y", "Reverts other/repo#12"), nil},
		{"bare number mention", pr(true, "Revert z", "this relates to #12"), nil},
		{"self reference", pr(true, "Revert w", "Reverts acme/widget#99"), nil},
		{"nil", nil, nil},
	}
	for _, tc := range cases {
		got := revertRefs("acme/widget", tc.pr)
		if len(got) != len(tc.want) {
			t.Errorf("%s: %v want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: %v want %v", tc.name, got, tc.want)
			}
		}
	}
}

// Regression (#36 review M9): a revert claim rides on attacker-editable
// title/body text — it only counts once corroborated by the PR's own commit
// messages, and the corroboration fails closed.
func TestCorroboratesRevert(t *testing.T) {
	cases := []struct {
		name string
		msgs []string
		want bool
	}{
		{"git revert trailer", []string{`Revert "fix"` + "\n\nThis reverts commit 0123456789abcdef0123456789abcdef01234567."}, true},
		{"short sha", []string{"This reverts commit 0123abc"}, true},
		{"mid-message line", []string{"cleanup\n\nThis reverts commit deadbeef99\nmore text"}, true},
		{"no trailer", []string{"totally a revert, trust me"}, false},
		{"inline mention only", []string{"see: this reverts commit-ish behavior"}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		if got := corroboratesRevert(tc.msgs); got != tc.want {
			t.Errorf("%s: %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestClosedRevertClaimFailsClosedWithoutCorroboration(t *testing.T) {
	g := newTestIntegration(t, richConfig())
	// A merged PR wearing the full revert costume in its editable text — but
	// with no way to check its commits (no installation), corroborated must
	// come out false.
	body := `{"action":"closed","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},
		"pull_request":{"number":90,"merged":true,"title":"Revert \"fix\"","body":"Reverts acme/w#5","head":{"sha":"h"}}}`
	trs := g.triggersFor(context.Background(), "pull_request", []byte(body))
	if len(trs) != 1 || trs[0].Kind != "_closed" {
		t.Fatalf("want _closed, got %+v", trs)
	}
	if got := trs[0].Context["reverts"].([]int); len(got) != 1 || got[0] != 5 {
		t.Fatalf("reverts: %v", trs[0].Context["reverts"])
	}
	if corr, _ := trs[0].Context["reverts_corroborated"].(bool); corr {
		t.Fatal("unverifiable revert claim must not be marked corroborated")
	}
}
