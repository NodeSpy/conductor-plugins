package ghsourcetest

import (
	"strings"
	"testing"
)

// The comparator has to be able to fail: a suite that cannot tell two
// implementations apart proves nothing about their parity.
func TestDiffCatchesEveryKindOfDifference(t *testing.T) {
	w := Want{Kind: "new_comment", Trigger: "nc", Repo: "o/r", Number: 7, Context: map[string]any{"comment_id": 5}, Len: map[string]int{"l": 1}, Absent: []string{"x"}}
	ok := Got{Kind: "new_comment", Trigger: "nc", Repo: "o/r", Number: 7, TargetTrusted: true,
		Context: map[string]any{"comment_id": float64(5), "l": []any{1}}}
	if p := Diff([]Want{w}, []Got{ok}); len(p) != 0 {
		t.Fatalf("an exact match (JSON-typed numbers and all) must agree: %v", p)
	}
	mut := func(f func(*Got)) Got {
		g := ok
		g.Context = map[string]any{"comment_id": float64(5), "l": []any{1}}
		f(&g)
		return g
	}
	for name, g := range map[string]Got{
		"kind":     mut(func(g *Got) { g.Kind = "changes_requested" }),
		"trigger":  mut(func(g *Got) { g.Trigger = "other" }),
		"number":   mut(func(g *Got) { g.Number = 8 }),
		"catch_up": mut(func(g *Got) { g.CatchUp = true }),
		"trust":    mut(func(g *Got) { g.TargetTrusted = false }),
		"fact":     mut(func(g *Got) { g.Context["comment_id"] = 6 }),
		"len":      mut(func(g *Got) { g.Context["l"] = []any{1, 2} }),
		"absent":   mut(func(g *Got) { g.Context["x"] = true }),
	} {
		if p := Diff([]Want{w}, []Got{g}); len(p) != 2 || !strings.HasPrefix(p[0], "missing") || !strings.HasPrefix(p[1], "unexpected") {
			t.Errorf("%s: a differing trigger must be reported missing AND unexpected, got %v", name, p)
		}
	}
	if p := Diff([]Want{w}, []Got{ok, ok}); len(p) != 1 || !strings.HasPrefix(p[0], "unexpected") {
		t.Errorf("a duplicate must be reported: %v", p)
	}
	if p := Diff([]Want{w, w}, []Got{ok}); len(p) != 1 || !strings.HasPrefix(p[0], "missing") {
		t.Errorf("a missing second fire must be reported: %v", p)
	}
}
