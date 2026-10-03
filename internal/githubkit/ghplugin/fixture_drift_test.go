package ghplugin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Conductor's own tests drive a SNAPSHOT of this plugin's declaration
// (internal/core/coretest/testdata/forge-decl.json in the conductor module this repo
// pins). This guard keeps the two equal: a declaration change here must ship
// with the regenerated snapshot in conductor, and the pin bumped to it — else
// conductor's suite would keep passing against a declaration no plugin sends.
func TestConductorFixtureSnapshotMatchesTheDeclaration(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/NodeSpy/conductor").Output()
	if err != nil {
		t.Skipf("conductor module not resolvable here: %v", err)
	}
	snap, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "internal", "core", "coretest", "testdata", "forge-decl.json"))
	if err != nil {
		t.Fatalf("conductor's fixture snapshot: %v", err)
	}
	b, err := json.Marshal(Decl())
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(snap, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("this plugin's declaration differs from conductor's fixture snapshot (forge-decl.json): " +
			"regenerate the snapshot in conductor from this Decl(), then bump this repo's conductor requirement to that commit")
	}
}
