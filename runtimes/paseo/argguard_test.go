package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// §18: every one of these values reaches the paseo CLI as the word after a
// flag or as a positional. A value like "--json" is not read as that
// value — the CLI reads it as the next FLAG, so caller content chooses
// paseo's options.
func TestArgOfRefusesFlagShapedValues(t *testing.T) {
	for _, bad := range []string{"--json", "-C", "--label=x", "-"} {
		if _, err := argOf(map[string]any{"repo": bad}, "repo"); err == nil {
			t.Errorf("value %q must be refused — the CLI would read it as a flag", bad)
		}
	}
}

func TestArgOfAllowsOrdinaryValues(t *testing.T) {
	for _, ok := range []string{"", "acme/app", "/tmp/wt", "feature/thing", "a-b", "review this -x please"} {
		got, err := argOf(map[string]any{"k": ok}, "k")
		if err != nil {
			t.Errorf("value %q should be allowed: %v", ok, err)
		}
		if got != ok {
			t.Errorf("argOf(%q) = %q", ok, got)
		}
	}
}

// The refusal names the field and the value, so an operator can see what
// they passed rather than a generic parse failure downstream.
func TestArgOfErrorNamesTheField(t *testing.T) {
	_, err := argOf(map[string]any{"newBranch": "--force"}, "newBranch")
	if err == nil || !strings.Contains(err.Error(), "newBranch") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("the error should name the field and the value, got %v", err)
	}
}

// §8: every paseo CLI call ran unbounded. A paseo that hangs — a wedged
// agent, a stuck git operation, a filesystem that stops answering — held
// the goroutine forever, and before the serve loop became concurrent it
// held the whole plugin with it.
func TestRunCmdIsBounded(t *testing.T) {
	start := time.Now()
	_, _, err := runCmdCtx(context.Background(), 100*time.Millisecond, "/bin/sh", "-c", "sleep 30")
	if err == nil {
		t.Fatal("a hanging CLI call must be cut off, not waited on forever")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("the error should name the deadline, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the deadline did not fire (took %s)", elapsed)
	}
}
