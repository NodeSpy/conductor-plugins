package githubkit

import (
	"os"
	"path/filepath"
	"testing"
)

// The login gh saved is read from hosts.yml when gh itself cannot run (a
// read-only config dir without config.yml): both the flat and the
// multi-account layouts, for GH_HOST or github.com.
func TestGhHostsToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", dir)
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("github.com:\n    oauth_token: gho_flat\n    user: me\n")
	if tok, err := ghHostsToken(); err != nil || tok != "gho_flat" {
		t.Fatalf("flat: %q %v", tok, err)
	}
	write("github.com:\n    git_protocol: https\n    users:\n        me:\n            oauth_token: gho_multi\n    user: me\nghe.example:\n    oauth_token: ghe_tok\n")
	if tok, err := ghHostsToken(); err != nil || tok != "gho_multi" {
		t.Fatalf("multi-account: %q %v", tok, err)
	}
	t.Setenv("GH_HOST", "ghe.example")
	if tok, err := ghHostsToken(); err != nil || tok != "ghe_tok" {
		t.Fatalf("GH_HOST: %q %v", tok, err)
	}
	t.Setenv("GH_HOST", "other.example")
	if _, err := ghHostsToken(); err == nil {
		t.Fatal("a host gh has no login for")
	}
	t.Setenv("GH_HOST", "")
	write("github.com:\n    user: me\n    users:\n        me: {}\n")
	if _, err := ghHostsToken(); err == nil {
		t.Fatal("a keyring login has no token in hosts.yml")
	}
}
