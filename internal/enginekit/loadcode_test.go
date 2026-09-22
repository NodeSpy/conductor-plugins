package enginekit

import (
	"os"
	"strings"
	"testing"
)

func TestLoadCodeFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.jq"
	if err := os.WriteFile(path, []byte(".items | length\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCode("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	if got != ".items | length\n" {
		t.Fatalf("got %q", got)
	}
	// whitespace around the reference tolerated
	if got, err := LoadCode("  file:" + path + "  "); err != nil || got != ".items | length\n" {
		t.Fatalf("trimmed: got %q err %v", got, err)
	}
}

func TestLoadCodeInlineUnchanged(t *testing.T) {
	for _, in := range []string{"keys", "return 1", ".items[]", "1 + 1", "./looks-like-a-path.js"} {
		if got, err := LoadCode(in); err != nil || got != in {
			t.Fatalf("inline %q changed to %q (err %v)", in, got, err)
		}
	}
}

func TestLoadCodeMissing(t *testing.T) {
	if _, err := LoadCode("file:/no/such/x.lua"); err == nil || !strings.Contains(err.Error(), "reading") {
		t.Fatalf("missing file should error, got %v", err)
	}
	if _, err := LoadCode("file:"); err == nil {
		t.Fatal("empty file: path should error")
	}
}
