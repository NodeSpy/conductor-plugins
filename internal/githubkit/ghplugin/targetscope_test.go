package ghplugin

import (
	"testing"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// Every write verb that names a PR or issue gates that option in the
// number dimension, and every branch write its branch — with what an absent
// branch names; reads stay unscoped beyond the repo.
func TestWriteVerbsAreScopedToTheTarget(t *testing.T) {
	verbs := map[string]plugin.Verb{}
	for _, v := range Decl().Verbs {
		verbs[v.Name] = v
	}
	for name := range targetWrites {
		v, ok := verbs[name]
		if !ok {
			t.Errorf("%s: not a declared verb", name)
			continue
		}
		tagged := 0
		for _, opt := range []string{"pr", "number"} {
			if f, ok := v.Options[opt]; ok {
				if f.Scope != dimNumber {
					t.Errorf("%s.%s: scope %q, want %q", name, opt, f.Scope, dimNumber)
				}
				tagged++
			}
		}
		if tagged == 0 {
			t.Errorf("%s: no pr/number option to bind", name)
		}
	}
	for name, absent := range branchWrites {
		f := verbs[name].Options["branch"]
		if f.Scope != dimBranch || f.ScopeAbsent != absent {
			t.Errorf("%s.branch: scope %q absent %q, want %q %q", name, f.Scope, f.ScopeAbsent, dimBranch, absent)
		}
	}
	// Every verb that names a PR, issue or branch is either a read (left
	// open) or a bound write: a new write verb that forgets its scope fails
	// here, not in production.
	reads := map[string]bool{"pr_get": true, "pr_head": true, "pr_files": true, "pr_diff": true,
		"review_comments": true, "get_issue": true, "list_runs": true}
	for _, v := range Decl().Verbs {
		for _, opt := range []string{"pr", "number", "branch"} {
			f, ok := v.Options[opt]
			if !ok {
				continue
			}
			switch {
			case reads[v.Name] && f.Scope != "":
				t.Errorf("read %s.%s is scoped %q: reads stay open beyond the repo", v.Name, opt, f.Scope)
			case !reads[v.Name] && f.Scope == "":
				t.Errorf("write %s.%s names the target but is not scoped to it", v.Name, opt)
			}
		}
	}
}

// The PR's own number and head branch are the dispatch's own only while it
// lives (bound); the repo is not bound. _reopened declares reopens_target
// and _closed closes_target.
func TestTargetScopeFactsAreBound(t *testing.T) {
	sem := EventSemantics("merge_conflict")
	got := map[string]plugin.ScopeFact{}
	for _, sf := range sem.Target.Scope {
		got[sf.Dimension] = sf
	}
	if f := got["repo"]; f.Fact != "repo" || f.Bound {
		t.Errorf("repo scope: %+v", f)
	}
	if f := got[dimNumber]; f.Fact != "number" || !f.Bound {
		t.Errorf("number scope: %+v", f)
	}
	if f := got[dimBranch]; f.Fact != "head_ref" || !f.Bound {
		t.Errorf("branch scope: %+v", f)
	}
	if !EventSemantics(ReopenedEvent).ReopensTarget || EventSemantics(ClosedEvent).ClosesTarget == nil {
		t.Error("_reopened must reopen and _closed close the target")
	}
}
