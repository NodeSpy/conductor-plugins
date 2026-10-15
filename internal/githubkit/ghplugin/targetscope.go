package ghplugin

import "github.com/NodeSpy/conductor/pkg/plugin"

// Write verbs are bound to the dispatch's target by conductor's resource
// scoping (conductor docs/design/skill-verb-scope.md): beyond the repo, an
// option naming the PR or issue a write acts on is in the "number"
// dimension, and one naming the branch a write commits to is in "branch".
// The event declares which facts carry the dispatch's own values for both
// (prTarget's target.scope, bound to the target — so a PR that merged or
// closed is no longer the dispatch's own), and conductor refuses a value
// outside them unless the operator's grant names it (skill.scopes /
// allow_scopes). Reads are left unscoped beyond the repo: reading another
// PR of the same repo stays allowed.
const (
	dimNumber = "number"
	dimBranch = "branch"
	// defaultBranchScope is what a file write WITHOUT a branch names: the
	// repo's default branch, which is never a dispatch's own — a grant
	// lists "@default-branch" to allow it.
	defaultBranchScope = "@default-branch"
)

// targetWrites are the write verbs whose pr/number options name the PR or
// issue they act on.
var targetWrites = map[string]bool{
	"comment": true, "reply": true, "request_review": true, "rerequest_review": true,
	"remove_reviewer": true, "submit_review": true, "merge_pr": true, "update_pr": true,
	"update_issue": true, "assign": true, "remove_label": true, "add_labels": true,
	"ready_for_review": true, "convert_to_draft": true, "react": true, "set_status": true,
}

// branchWrites are the verbs that commit to or create a branch, and what an
// absent branch option names on each.
var branchWrites = map[string]string{
	"put_file": defaultBranchScope, "delete_file": defaultBranchScope, "create_branch": "",
}

// scopeTargetWrites tags the target-naming options of d's write verbs.
func scopeTargetWrites(d plugin.Decl) plugin.Decl {
	for i := range d.Verbs {
		v := &d.Verbs[i]
		if targetWrites[v.Name] {
			for _, opt := range []string{"pr", "number"} {
				if f, ok := v.Options[opt]; ok {
					f.Scope = dimNumber
					v.Options[opt] = f
				}
			}
		}
		if absent, ok := branchWrites[v.Name]; ok {
			if f, ok := v.Options["branch"]; ok {
				f.Scope, f.ScopeAbsent = dimBranch, absent
				v.Options["branch"] = f
			}
		}
	}
	return d
}
