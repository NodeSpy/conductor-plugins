package enginekit

import (
	"reflect"
	"testing"
)

func TestStepVarsSortsAndAligns(t *testing.T) {
	names, values, err := StepVars(map[string]string{"B": "2", "A": "1", "C": "3"})
	if err != nil {
		t.Fatal(err)
	}
	// Names are sorted, and values follow position-for-position so gojq's
	// positional (variables, values) pairing is deterministic.
	if !reflect.DeepEqual(names, []string{"A", "B", "C"}) {
		t.Fatalf("names = %#v, want sorted [A B C]", names)
	}
	if !reflect.DeepEqual(values, []string{"1", "2", "3"}) {
		t.Fatalf("values = %#v, want [1 2 3] aligned to names", values)
	}
}

func TestStepVarsEmpty(t *testing.T) {
	names, values, err := StepVars(nil)
	if err != nil {
		t.Fatal(err)
	}
	if names != nil || values != nil {
		t.Fatalf("empty env should yield nil slices, got %#v / %#v", names, values)
	}
}

func TestStepVarsRejectsBadNames(t *testing.T) {
	for _, bad := range []string{"has-dash", "1leading", "has space", "", "a.b"} {
		if _, _, err := StepVars(map[string]string{bad: "x"}); err == nil {
			t.Fatalf("name %q should be rejected", bad)
		}
	}
}

func TestStepVarsAcceptsIdentifiers(t *testing.T) {
	for _, ok := range []string{"A", "a", "_x", "REGION", "max_count", "v1"} {
		if _, _, err := StepVars(map[string]string{ok: "x"}); err != nil {
			t.Fatalf("name %q should be accepted, got %v", ok, err)
		}
	}
}
