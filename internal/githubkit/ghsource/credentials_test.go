package ghsource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureClientsCredentialChain(t *testing.T) {
	// token: static auth.
	cfg := baseConfig()
	cfg.App = AppConfig{}
	cfg.Token = "pat-123"
	g := newTestIntegration(t, cfg)
	if err := g.ensureClients(); err != nil {
		t.Fatalf("token chain: %v", err)
	}
	tok, err := g.app.installationToken(context.Background(), 0)
	if err != nil || tok != "pat-123" {
		t.Fatalf("static auth token: %q %v", tok, err)
	}

	// gh CLI fallback.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/usr/bin/env bash\necho gh-tok\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg.Token = ""
	g2 := newTestIntegration(t, cfg)
	if err := g2.ensureClients(); err != nil {
		t.Fatalf("gh fallback: %v", err)
	}
	tok, _ = g2.app.installationToken(context.Background(), 0)
	if tok != "gh-tok" {
		t.Fatalf("gh token: %q", tok)
	}

	// Empty gh output errors with the configure guidance.
	os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/usr/bin/env bash\necho\n"), 0o755)
	g3 := newTestIntegration(t, cfg)
	if err := g3.ensureClients(); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("empty gh token: %v", err)
	}
}
