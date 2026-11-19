package ghplugin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit"
	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghfake"
	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghsource"
)

// identity.git_fetch picks whose credential the declared git fetch
// credential (git_token) mints, against the stateful fake: the App is
// installed on acme for acme/app only (selected repositories), and acme/dep
// is a private dependency outside the installation that the operator's own
// login can read. self (the default) mints the operator's token, which reads
// acme/dep; app mints the installation token, which GitHub hides acme/dep
// from. The read and write credentials — what agents' API reads and writes
// use — are the same under both.
func TestGitFetchIdentity(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	f := ghfake.New(ghfake.Options{Logf: t.Logf})
	f.Start()
	t.Cleanup(f.Close)
	f.AddUser("acme", "Organization")
	f.SetApp(77, "conductor-test", &key.PublicKey)
	f.AddRepo("acme/app")
	f.AddRepo("acme/dep")
	f.Install("acme", "acme/app")
	f.UserToken("me", "tok-me")

	// status is GET path's status with tok, straight to the fake.
	status := func(path, tok string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", f.URL()+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	mint := func(identity map[string]any) map[string]string {
		t.Helper()
		conn, err := ParseConnection(map[string]any{
			"api_base": f.URL(),
			"app":      map[string]any{"app_id": 77, "private_key_path": keyPath},
			"identity": identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		conn.Client.GhToken = func() (string, error) { return "tok-me", nil }
		kit, err := githubkit.NewClient(conn.Client)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, verb := range []string{"read_token", "write_token", "git_token"} {
			o, err := kit.Invoke(context.Background(), verb, map[string]any{"repo": "acme/app"})
			if err != nil {
				t.Fatalf("%v %s: %v", identity, verb, err)
			}
			out[verb], _ = o["token"].(string)
		}
		return out
	}

	for name, identity := range map[string]map[string]any{
		"default":             {},
		"self":                {"git_fetch": "self"},
		"self, literal write": {"git_fetch": "self", "write_token": "tok-me"},
	} {
		got := mint(identity)
		if got["git_token"] != "tok-me" {
			t.Errorf("%s: git_token = %q, want the operator's own token", name, got["git_token"])
		}
		if status("/repos/acme/dep", got["git_token"]) != 200 {
			t.Errorf("%s: the git credential cannot read the dependency outside the installation", name)
		}
		if !strings.HasPrefix(got["read_token"], "ghs_") || got["write_token"] != "tok-me" {
			t.Errorf("%s: read/write credentials changed: %v", name, got)
		}
	}

	got := mint(map[string]any{"git_fetch": "app"})
	if !strings.HasPrefix(got["git_token"], "ghs_") {
		t.Fatalf("app: git_token = %q, want an installation token", got["git_token"])
	}
	if status("/repos/acme/app", got["git_token"]) != 200 || status("/repos/acme/dep", got["git_token"]) != 404 {
		t.Error("app: the git credential is not the installation's (selected repositories)")
	}
	if !strings.HasPrefix(got["read_token"], "ghs_") || got["write_token"] != "tok-me" {
		t.Errorf("app: read/write credentials changed: %v", got)
	}

	// identity.read_token: gh_auth makes app the read credential, whatever
	// it is: app follows the read credential, not the App itself.
	got = mint(map[string]any{"git_fetch": "app", "read_token": "gh_auth"})
	if got["git_token"] != got["read_token"] || got["git_token"] != "tok-me" {
		t.Errorf("app with read_token gh_auth: %v", got)
	}
}

// An identity.git_fetch other than self or app is a config error, and an
// unset one is self.
func TestGitFetchIdentityValidates(t *testing.T) {
	for v, want := range map[string]string{"": "", "self": "", "app": "", "me": "identity.git_fetch", "write": "identity.git_fetch"} {
		src, err := ghsource.New("gh", ghsource.Config{Token: "t", Identity: ghsource.Identity{GitFetch: v}})
		if err != nil {
			t.Fatal(err)
		}
		err = src.Validate()
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("git_fetch %q: %v", v, err)
		}
	}
}
