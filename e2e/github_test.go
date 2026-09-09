package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestGithubPluginVerbTokenAuth proves the verb path: the plugin, given a
// literal identity.write_token in its Connection, performs the same REST call
// conductor's bundled github connector's `comment` verb performs — a POST to
// /repos/{owner}/{repo}/issues/{number}/comments — using that token (not
// falling through to `gh auth token`, which a plugin subprocess in a real
// environment could otherwise reach).
func TestGithubPluginVerbTokenAuth(t *testing.T) {
	bin := rpctest.Build(t, "github")

	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /repos/o/r/issues/7/comments" {
			t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "html_url": "https://example/c/9"})
	}))
	defer srv.Close()

	c := rpctest.Start(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	decl, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if decl.Type != "github" {
		t.Fatalf("decl.Type = %q, want github", decl.Type)
	}
	var hasComment, hasSweep bool
	for _, v := range decl.Verbs {
		if v.Name == "comment" {
			hasComment = true
		}
		if v.Name == "sweep" {
			hasSweep = true
		}
	}
	if !hasComment {
		t.Fatal("decl missing comment verb")
	}
	if hasSweep {
		t.Fatal("decl should not declare the daemon-global sweep verb")
	}

	out, err := c.Invoke(ctx, plugin.InvokeRequest{
		Instance: "github1", Verb: "comment",
		Options: map[string]any{"repo": "o/r", "number": 7, "body": "from the plugin"},
		Connection: map[string]any{
			"api_base": srv.URL,
			"identity": map[string]any{"write_token": "tok123"},
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("Authorization = %q, want Bearer tok123", gotAuth)
	}
	if gotBody["body"] != "from the plugin" {
		t.Fatalf("posted body = %+v", gotBody)
	}
	if id, _ := out["id"].(float64); id != 9 {
		t.Fatalf("out[id] = %v (%T), want 9", out["id"], out["id"])
	}
}

// TestGithubPluginVerbAppAuth proves the App/bot identity path: given
// app_id/private_key_path, the plugin mints a JWT, resolves the repo's
// installation, mints an installation token, and uses THAT (not the JWT) to
// perform the verb call.
func TestGithubPluginVerbAppAuth(t *testing.T) {
	bin := rpctest.Build(t, "github")

	var installAuth, mintAuth, commentAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/org/repo/installation":
			installAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 555})
		case "POST /app/installations/555/access_tokens":
			mintAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "ghs_minted", "expires_at": time.Now().Add(time.Hour).UTC(),
			})
		case "POST /repos/org/repo/issues/7/comments":
			commentAuth = r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "html_url": "https://example/c/9"})
		default:
			t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := rpctest.Start(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := c.Invoke(ctx, plugin.InvokeRequest{
		Instance: "github2", Verb: "comment",
		Options: map[string]any{"repo": "org/repo", "number": 7, "body": "from the app", "as": "bot"},
		Connection: map[string]any{
			"api_base": srv.URL,
			"app": map[string]any{
				"app_id":           float64(99),
				"private_key_path": writeTestRSAKey(t),
			},
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.HasPrefix(installAuth, "Bearer ") {
		t.Fatalf("installation lookup auth = %q, want a Bearer JWT", installAuth)
	}
	if !strings.HasPrefix(mintAuth, "Bearer ") {
		t.Fatalf("token mint auth = %q, want a Bearer JWT", mintAuth)
	}
	if commentAuth != "Bearer ghs_minted" {
		t.Fatalf("comment auth = %q, want Bearer ghs_minted (the minted installation token, not the JWT)", commentAuth)
	}
	if gotBody["body"] != "from the app" {
		t.Fatalf("posted body = %+v", gotBody)
	}
	if _, ok := out["id"]; !ok {
		t.Fatalf("missing id in outputs: %+v", out)
	}
}

func writeTestRSAKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	p := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGithubPluginSourceWebhook proves the source path: a signed
// X-Hub-Signature-256 issue_comment webhook delivered to the plugin's listener
// streams a normalized new_comment event with the daemon-compatible
// target/context shape; a bad-signature delivery is rejected and produces no
// event.
func TestGithubPluginSourceWebhook(t *testing.T) {
	bin := rpctest.Build(t, "github")
	c := rpctest.Start(t, bin)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	addr := rpctest.FreeAddr(t)
	secret := "github-hmac-secret"
	sink := rpctest.NewEventSink()
	req := plugin.StartSourceRequest{Instance: "github3", Config: map[string]any{
		"webhook": map[string]any{"listen": addr, "path": "/github", "secret": secret},
	}}
	if err := c.StartSource(ctx, req, sink.Emit); err != nil {
		t.Fatalf("StartSource: %v", err)
	}

	body := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/widgets", "owner": {"login": "acme"}, "name": "widgets"},
		"issue": {"number": 42, "html_url": "https://github.com/acme/widgets/pull/42", "pull_request": {}},
		"comment": {"id": 555, "body": "looks good", "user": {"login": "reviewer1", "type": "User"}}
	}`)
	url := "http://" + addr + "/github"
	rpctest.PostUntilAccepted(t, url, map[string]string{
		"X-GitHub-Event":      "issue_comment",
		"X-Hub-Signature-256": "sha256=" + rpctest.HMACHex(secret, body),
	}, body)

	if err := sink.Wait(ctx); err != nil {
		t.Fatalf("timed out waiting for the streamed new_comment event: %v", err)
	}
	ev := sink.At(0)
	if ev["event"] != "new_comment" {
		t.Fatalf("event = %v, want new_comment", ev["event"])
	}
	target, _ := ev["target"].(map[string]any)
	if fmt.Sprint(target["Repo"]) != "acme/widgets" || fmt.Sprint(target["Owner"]) != "acme" {
		t.Fatalf("event target wrong: %+v", target)
	}
	ectx, _ := ev["context"].(map[string]any)
	if ectx["author"] != "reviewer1" || ectx["comment_body"] != "looks good" {
		t.Fatalf("event context wrong: %+v", ectx)
	}

	// A bad-signature delivery must be rejected (401) and produce no new event.
	before := sink.Len()
	if got := rpctest.PostStatus(t, url, map[string]string{
		"X-GitHub-Event": "issue_comment", "X-Hub-Signature-256": "sha256=deadbeef",
	}, body); got != http.StatusUnauthorized {
		t.Fatalf("bad-signature webhook returned %d, want 401", got)
	}
	if after := sink.Len(); after != before {
		t.Fatalf("bad-signature delivery produced an event: before=%d after=%d", before, after)
	}
}
