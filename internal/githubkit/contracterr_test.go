package githubkit

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghfake"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// asContractError fails the test unless err is (or wraps) a *plugin.Error,
// and returns it.
func asContractError(t *testing.T, err error) *plugin.Error {
	t.Helper()
	if err == nil {
		t.Fatal("got a nil error, want a contract error")
	}
	var pe *plugin.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v (%T) is not a *plugin.Error", err, err)
	}
	return pe
}

// world builds a small fake GitHub (one org, one repo, an app installed, an
// open PR) and a Client wired to it with a `me` identity, the shape every
// test below starts from.
func world(t *testing.T) (*ghfake.Fake, *Client, int) {
	t.Helper()
	return worldWithOptions(t, ghfake.Options{})
}

func worldWithOptions(t *testing.T, opts ghfake.Options) (*ghfake.Fake, *Client, int) {
	t.Helper()
	if opts.Logf == nil {
		opts.Logf = t.Logf
	}
	f := ghfake.New(opts)
	f.Start()
	t.Cleanup(f.Close)
	f.AddUser("acme", "Organization")
	f.AddRepo("acme/w")
	f.UserToken("me", "tok-me")
	f.UserToken("rev", "tok-rev")
	f.AddCollaborator("acme/w", "rev")
	n := f.OpenPR("acme/w", "me", ghfake.PROpts{Title: "feature", Head: "feat"})
	kit, err := NewClient(Config{WriteToken: "tok-me", APIBase: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	return f, kit, n
}

// TestTargetGoneOnTheMissingPROrIssue covers plugin-contract.md §1.11's
// target_gone (-32011) for the verbs keyed directly by a PR/issue number:
// a 404 on the number itself is the target going away, never the generic
// upstream bucket.
func TestTargetGoneOnTheMissingPROrIssue(t *testing.T) {
	_, kit, _ := world(t)
	ctx := context.Background()
	const missing = 99999

	tests := []struct {
		verb string
		opts map[string]any
	}{
		{"get_issue", map[string]any{"number": missing}},
		{"pr_get", map[string]any{"pr": missing}},
		{"pr_head", map[string]any{"pr": missing}},
		{"pr_diff", map[string]any{"pr": missing}},
		{"pr_files", map[string]any{"pr": missing}},
		{"review_comments", map[string]any{"pr": missing}},
		{"comment", map[string]any{"pr": missing, "body": "hi"}},
		{"update_pr", map[string]any{"pr": missing, "title": "x"}},
		{"update_issue", map[string]any{"number": missing, "title": "x"}},
		{"add_labels", map[string]any{"number": missing, "labels": []any{"bug"}}},
		{"assign", map[string]any{"number": missing, "add": []any{"rev"}}},
		{"submit_review", map[string]any{"pr": missing, "event": "APPROVE"}},
		{"request_review", map[string]any{"pr": missing, "reviewers": []any{"rev"}}},
		{"merge_pr", map[string]any{"pr": missing}},
		{"ready_for_review", map[string]any{"pr": missing}},
	}
	for _, tt := range tests {
		t.Run(tt.verb, func(t *testing.T) {
			opts := map[string]any{"repo": "acme/w"}
			for k, v := range tt.opts {
				opts[k] = v
			}
			_, err := kit.Invoke(ctx, tt.verb, opts)
			pe := asContractError(t, err)
			if pe.Code != plugin.CodeTargetGone {
				t.Fatalf("%s: code = %d, want CodeTargetGone (%d); msg=%s", tt.verb, pe.Code, plugin.CodeTargetGone, pe.Message)
			}
		})
	}
}

// TestMergePRClosedOrMergedIsTargetGone covers the other half of target_gone:
// a PR that merge_pr needs open, but which is already merged or closed —
// told apart from a real merge conflict (which stays the generic upstream
// 4xx, not retried either, but a different code) via checkMergeReady's
// authoritative pre-read.
func TestMergePRClosedOrMergedIsTargetGone(t *testing.T) {
	ctx := context.Background()

	t.Run("already merged", func(t *testing.T) {
		f, kit, n := world(t)
		f.Merge("acme/w", n, "me")
		_, err := kit.Invoke(ctx, "merge_pr", map[string]any{"repo": "acme/w", "pr": n})
		pe := asContractError(t, err)
		if pe.Code != plugin.CodeTargetGone {
			t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
		}
	})

	t.Run("closed without merging", func(t *testing.T) {
		f, kit, n := world(t)
		f.CloseIssue("acme/w", n, "me")
		_, err := kit.Invoke(ctx, "merge_pr", map[string]any{"repo": "acme/w", "pr": n})
		pe := asContractError(t, err)
		if pe.Code != plugin.CodeTargetGone {
			t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
		}
	})

	t.Run("a real conflict is upstream, not target_gone", func(t *testing.T) {
		f, kit, n := world(t)
		f.SubmitReview("acme/w", n, "rev", "APPROVED", "lgtm") // clear the one-approval requirement
		f.SetConflict("acme/w", n, true)
		_, err := kit.Invoke(ctx, "merge_pr", map[string]any{"repo": "acme/w", "pr": n})
		pe := asContractError(t, err)
		if pe.Code != plugin.CodeUpstream {
			t.Fatalf("code = %d, want CodeUpstream (a conflict, not target_gone or not_ready); msg=%s", pe.Code, pe.Message)
		}
		if retryable, _ := pe.Data["retryable"].(bool); retryable {
			t.Fatal("a 4xx merge conflict must not be retryable")
		}
	})
}

// TestMergePRNotReadyWhenMergeabilityIsUnknown covers not_ready (-32014):
// GitHub answers mergeable null right after a PR's head or base moves, while
// it computes mergeability in the background (ghfake's MergeabilityDelay
// models exactly this) — checkMergeReady catches it before even attempting
// the merge, rather than letting it fall into the merge endpoint's
// ambiguous 405 "Pull Request is not mergeable".
func TestMergePRNotReadyWhenMergeabilityIsUnknown(t *testing.T) {
	_, kit, n := worldWithOptions(t, ghfake.Options{MergeabilityDelay: true})
	_, err := kit.Invoke(context.Background(), "merge_pr", map[string]any{"repo": "acme/w", "pr": n})
	pe := asContractError(t, err)
	if pe.Code != plugin.CodeNotReady {
		t.Fatalf("code = %d, want CodeNotReady; msg=%s", pe.Code, pe.Message)
	}
}

// TestRemoveLabelDistinguishesTheLabelFromTheIssue: a 404 "Label does not
// exist" means the issue/PR is still there, just without that label — not
// the target itself gone. A 404 on the issue number itself still is.
func TestRemoveLabelDistinguishesTheLabelFromTheIssue(t *testing.T) {
	ctx := context.Background()

	t.Run("label not present: not target_gone", func(t *testing.T) {
		_, kit, n := world(t)
		_, err := kit.Invoke(ctx, "remove_label", map[string]any{"repo": "acme/w", "number": n, "label": "nope"})
		pe := asContractError(t, err)
		if pe.Code != plugin.CodeUpstream {
			t.Fatalf("code = %d, want CodeUpstream; msg=%s", pe.Code, pe.Message)
		}
	})

	t.Run("issue itself gone: target_gone", func(t *testing.T) {
		_, kit, _ := world(t)
		_, err := kit.Invoke(ctx, "remove_label", map[string]any{"repo": "acme/w", "number": 99999, "label": "nope"})
		pe := asContractError(t, err)
		if pe.Code != plugin.CodeTargetGone {
			t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
		}
	})
}

// TestInvalidOn422 covers invalid (-32012): a 422 validation error never
// retries.
func TestInvalidOn422(t *testing.T) {
	_, kit, n := world(t)
	_, err := kit.Invoke(context.Background(), "submit_review",
		map[string]any{"repo": "acme/w", "pr": n, "event": "NOT_A_REAL_EVENT"})
	pe := asContractError(t, err)
	if pe.Code != plugin.CodeInvalid {
		t.Fatalf("code = %d, want CodeInvalid; msg=%s", pe.Code, pe.Message)
	}
}

// TestGraphQLNotFoundIsTargetGone covers the GraphQL mutation path
// (ready_for_review/convert_to_draft, react/unreact on a review): GitHub's
// own "type": "NOT_FOUND" extension, the only signal a 200-status GraphQL
// error gives, is told apart from a malformed query (invalid).
func TestGraphQLNotFoundIsTargetGone(t *testing.T) {
	_, kit, _ := world(t)
	mutation := "mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){clientMutationId}}"
	err := kit.graphql(context.Background(), "tok-me", mutation, map[string]any{"id": "not-a-real-node-id"}, nil)
	pe := asContractError(t, err)
	if pe.Code != plugin.CodeTargetGone {
		t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
	}
}

func TestGraphQLMalformedQueryIsInvalid(t *testing.T) {
	_, kit, _ := world(t)
	err := kit.graphql(context.Background(), "tok-me", "query{thisFieldDoesNotExist}", nil, nil)
	pe := asContractError(t, err)
	if pe.Code != plugin.CodeInvalid {
		t.Fatalf("code = %d, want CodeInvalid; msg=%s", pe.Code, pe.Message)
	}
}

// --- rate_limited and generic upstream, against a bespoke server -----------
//
// These don't need the fake's full model, just exact control over status and
// headers, so a plain httptest server stands in for GitHub directly.

func canned(t *testing.T, status int, hdr map[string]string, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func createPROpts() map[string]any {
	return map[string]any{"repo": "acme/w", "title": "t", "head": "h", "base": "b"}
}

// TestRateLimitedPrimarySignal covers rate_limited (-32013) via the primary
// signal (403 + X-RateLimit-Remaining: 0), retry_after from X-RateLimit-Reset.
func TestRateLimitedPrimarySignal(t *testing.T) {
	reset := time.Now().Add(90 * time.Second).Unix()
	ts := canned(t, http.StatusForbidden, map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(reset, 10),
	}, `{"message":"API rate limit exceeded"}`)
	kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, ierr := kit.Invoke(context.Background(), "create_pr", createPROpts())
	pe := asContractError(t, ierr)
	if pe.Code != plugin.CodeRateLimited {
		t.Fatalf("code = %d, want CodeRateLimited; msg=%s", pe.Code, pe.Message)
	}
	wait, ok := pe.Data["retry_after"].(string)
	if !ok || wait == "" {
		t.Fatalf("data.retry_after = %#v, want a non-empty Go duration string", pe.Data["retry_after"])
	}
	if d, perr := time.ParseDuration(wait); perr != nil || d <= 0 {
		t.Fatalf("retry_after = %q did not parse to a positive duration: %v", wait, perr)
	}
}

// TestRateLimitedSecondarySignal covers the secondary signal (429 +
// Retry-After), retry_after taken straight from the header.
func TestRateLimitedSecondarySignal(t *testing.T) {
	ts := canned(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, `{"message":"secondary rate limit"}`)
	kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, ierr := kit.Invoke(context.Background(), "create_pr", createPROpts())
	pe := asContractError(t, ierr)
	if pe.Code != plugin.CodeRateLimited {
		t.Fatalf("code = %d, want CodeRateLimited; msg=%s", pe.Code, pe.Message)
	}
	if got := pe.Data["retry_after"]; got != "30s" {
		t.Fatalf("retry_after = %#v, want %q", got, "30s")
	}
}

// TestUpstreamClassification covers upstream (-32010): a 5xx is retryable, a
// non-rate-limited 4xx is not.
func TestUpstreamClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		retryable bool
	}{
		{"server error", http.StatusBadGateway, true},
		{"forbidden, no rate-limit signal", http.StatusForbidden, false},
		{"conflict", http.StatusConflict, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := canned(t, tt.status, nil, `{"message":"boom"}`)
			kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, ierr := kit.Invoke(context.Background(), "create_pr", createPROpts())
			pe := asContractError(t, ierr)
			if pe.Code != plugin.CodeUpstream {
				t.Fatalf("code = %d, want CodeUpstream; msg=%s", pe.Code, pe.Message)
			}
			status, _ := pe.Data["status"].(int)
			if status != tt.status {
				t.Fatalf("data.status = %v, want %d", pe.Data["status"], tt.status)
			}
			if retryable, _ := pe.Data["retryable"].(bool); retryable != tt.retryable {
				t.Fatalf("data.retryable = %v, want %v", pe.Data["retryable"], tt.retryable)
			}
		})
	}
}

// --- the read_token/write_token mint ----------------------------------------

// testAppKey writes a throwaway RSA key PEM to disk (NewAppAuth reads a
// path, not bytes) and returns its path. The bespoke server below answers
// every request from status/headers alone, so the key never needs to
// verify against anything — it only has to parse.
func testAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	blk := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	p := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(p, blk, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMintCredentialContractErrors covers the read_token/write_token mint
// (MintCredential -> TokenFor("bot", …) -> AppAuth): the installation lookup
// and the installation-token mint both answer through the same classifier as
// every other call.
func TestMintCredentialContractErrors(t *testing.T) {
	keyPath := testAppKey(t)

	t.Run("installation lookup: not found is upstream, not target_gone (no PR/issue involved)", func(t *testing.T) {
		ts := canned(t, http.StatusNotFound, nil, `{"message":"Not Found"}`)
		kit, err := NewClient(Config{APIBase: ts.URL, App: &AppConfig{AppID: 1, PrivateKeyPath: keyPath}})
		if err != nil {
			t.Fatal(err)
		}
		_, ierr := kit.Invoke(context.Background(), "read_token", map[string]any{"repo": "acme/w"})
		pe := asContractError(t, ierr)
		if pe.Code != plugin.CodeUpstream {
			t.Fatalf("code = %d, want CodeUpstream; msg=%s", pe.Code, pe.Message)
		}
	})

	t.Run("installation token mint: rate limited", func(t *testing.T) {
		ts := canned(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "12"}, `{"message":"rate limited"}`)
		kit, err := NewClient(Config{WriteToken: "app", APIBase: ts.URL, App: &AppConfig{AppID: 1, PrivateKeyPath: keyPath}})
		if err != nil {
			t.Fatal(err)
		}
		_, ierr := kit.Invoke(context.Background(), "write_token", map[string]any{"repo": "acme/w"})
		pe := asContractError(t, ierr)
		if pe.Code != plugin.CodeRateLimited {
			t.Fatalf("code = %d, want CodeRateLimited; msg=%s", pe.Code, pe.Message)
		}
		if got := pe.Data["retry_after"]; got != "12s" {
			t.Fatalf("retry_after = %#v, want %q", got, "12s")
		}
	})
}
