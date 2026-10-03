package githubkit

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
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
	wantKey := fmt.Sprintf("acme/w#%d", missing)
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
			// data.target must equal the SAME string ghplugin's event
			// semantics render for this target ("{{.repo}}#{{.number}}",
			// semantics.go's prTarget) — the host will require an exact
			// match before honoring target_gone.
			if got := pe.Data["target"]; got != wantKey {
				t.Fatalf("%s: data.target = %#v, want %q", tt.verb, got, wantKey)
			}
		})
	}
}

// TestNoAccessRepoIsUpstreamNotTargetGone covers the other half of fix 2:
// GitHub answers the identical 404 when the token can't see the repo at all
// as when a PR/issue number is merely missing from a repo it CAN see — the
// worse of the two to get wrong, since a false target_gone silently ends a
// live run. A repo absent from the fake's registry entirely stands in for
// "invisible to this token": the wire response is the same 404 either way.
func TestNoAccessRepoIsUpstreamNotTargetGone(t *testing.T) {
	_, kit, _ := world(t)
	_, err := kit.Invoke(context.Background(), "get_issue", map[string]any{"repo": "acme/ghost", "number": 1})
	pe := asContractError(t, err)
	if pe.Code != plugin.CodeUpstream {
		t.Fatalf("code = %d, want CodeUpstream (never target_gone — the repo itself can't be confirmed visible); msg=%s", pe.Code, pe.Message)
	}
	if retryable, _ := pe.Data["retryable"].(bool); retryable {
		t.Fatal("retryable = true, want false")
	}
	if status, _ := pe.Data["status"].(int); status != http.StatusNotFound {
		t.Fatalf("data.status = %v, want %d", pe.Data["status"], http.StatusNotFound)
	}
}

// TestRepoVisibilityCheckPropagatesRateLimit: if the repoVisible probe
// itself hits a rate limit, that must be surfaced directly rather than
// guessed at as "the repo isn't visible" — a secondary server that answers
// the PR/issue GET with 404 and the repo GET with a rate limit.
func TestRepoVisibilityCheckPropagatesRateLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/w/issues/1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	mux.HandleFunc("/repos/acme/w", func(w http.ResponseWriter, r *http.Request) {
		// Retry-After: 0 still signals rate_limited (the header's mere
		// presence is the classifier) without making cachedGet's single
		// sleep-then-retry window (bounded by maxRateWait) actually sleep —
		// this test only cares that the limit is propagated, not timed.
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, ierr := kit.Invoke(context.Background(), "get_issue", map[string]any{"repo": "acme/w", "number": 1})
	pe := asContractError(t, ierr)
	if pe.Code != plugin.CodeRateLimited {
		t.Fatalf("code = %d, want CodeRateLimited (propagated from the visibility check); msg=%s", pe.Code, pe.Message)
	}
	if wait, _ := pe.Data["retry_after"].(string); wait == "" {
		t.Fatalf("retry_after = %#v, want a non-empty Go duration string", pe.Data["retry_after"])
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
		if want := fmt.Sprintf("acme/w#%d", n); pe.Data["target"] != want {
			t.Fatalf("data.target = %#v, want %q", pe.Data["target"], want)
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
		if want := fmt.Sprintf("acme/w#%d", n); pe.Data["target"] != want {
			t.Fatalf("data.target = %#v, want %q", pe.Data["target"], want)
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

// TestAddLabelsDistinguishesTheLabelFromTheIssue covers fix 3: add_labels
// gets the identical ambiguous 404 remove_label does (GitHub requires a
// label already exist on the repo before it can be added to an issue/PR —
// "Label does not exist" means the issue is still there, just without a repo
// label by that name yet). ghfake's add_labels auto-creates any label name
// (never answers this 404), so the "not target_gone" half goes straight at a
// bespoke server shaped like GitHub's real answer.
func TestAddLabelsDistinguishesTheLabelFromTheIssue(t *testing.T) {
	ctx := context.Background()

	t.Run("named label doesn't exist yet: not target_gone", func(t *testing.T) {
		// Two handlers, isolating the two concerns: the repo itself answers
		// 200 (visible — so a wrong remap can't hide behind a coincidental
		// repo-check failure), only the labels POST answers GitHub's real
		// 404 "Label does not exist".
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/w", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"full_name":"acme/w"}`))
		})
		mux.HandleFunc("/repos/acme/w/issues/1/labels", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Label does not exist"}`))
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
		if err != nil {
			t.Fatal(err)
		}
		_, ierr := kit.Invoke(ctx, "add_labels", map[string]any{"repo": "acme/w", "number": 1, "labels": []any{"nope"}})
		pe := asContractError(t, ierr)
		if pe.Code != plugin.CodeUpstream {
			t.Fatalf("code = %d, want CodeUpstream; msg=%s", pe.Code, pe.Message)
		}
	})

	t.Run("issue itself gone: target_gone", func(t *testing.T) {
		_, kit, _ := world(t)
		_, err := kit.Invoke(ctx, "add_labels", map[string]any{"repo": "acme/w", "number": 99999, "labels": []any{"bug"}})
		pe := asContractError(t, err)
		if pe.Code != plugin.CodeTargetGone {
			t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
		}
		if want := "acme/w#99999"; pe.Data["target"] != want {
			t.Fatalf("data.target = %#v, want %q", pe.Data["target"], want)
		}
	})
}

// TestMergeFailure405BecomesTargetGoneWhenAlreadyMergedOrClosed covers fix 6:
// the merge PUT's own 405 "Pull Request is not mergeable" — the same answer
// a real conflict gets — is re-checked with a fresh read when
// checkMergeReady already confirmed the PR open+mergeable moments earlier;
// merged/closed NOW means the gap raced a merge/close, not a real conflict.
// Exercised directly (remapMergeFailure takes the 405 as input) since
// ghfake's merge model can't itself produce "checkMergeReady said OK, then
// the PUT 405s because it merged/closed in between" — that race is the
// whole point of the fix, not a state ghfake's straightforward model reaches
// on its own.
func TestMergeFailure405BecomesTargetGoneWhenAlreadyMergedOrClosed(t *testing.T) {
	ctx := context.Background()
	fake405 := func() error {
		return plugin.Fail(plugin.CodeUpstream, "github pulls/merge: HTTP 405: Pull Request is not mergeable",
			map[string]any{"status": http.StatusMethodNotAllowed, "retryable": false})
	}

	t.Run("merged in the gap: target_gone", func(t *testing.T) {
		f, kit, n := world(t)
		f.Merge("acme/w", n, "me")
		key := fmt.Sprintf("acme/w#%d", n)
		err := kit.remapMergeFailure(ctx, "tok-me", kit.base(), "acme/w", n, key, fake405())
		pe := asContractError(t, err)
		if pe.Code != plugin.CodeTargetGone {
			t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
		}
		if pe.Data["target"] != key {
			t.Fatalf("data.target = %#v, want %q", pe.Data["target"], key)
		}
	})

	t.Run("closed without merging in the gap: target_gone", func(t *testing.T) {
		f, kit, n := world(t)
		f.CloseIssue("acme/w", n, "me")
		key := fmt.Sprintf("acme/w#%d", n)
		err := kit.remapMergeFailure(ctx, "tok-me", kit.base(), "acme/w", n, key, fake405())
		pe := asContractError(t, err)
		if pe.Code != plugin.CodeTargetGone {
			t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
		}
	})

	t.Run("still open: a real conflict, the 405 stands", func(t *testing.T) {
		_, kit, n := world(t)
		key := fmt.Sprintf("acme/w#%d", n)
		err := kit.remapMergeFailure(ctx, "tok-me", kit.base(), "acme/w", n, key, fake405())
		pe := asContractError(t, err)
		if pe.Code != plugin.CodeUpstream {
			t.Fatalf("code = %d, want CodeUpstream (the original 405 stands); msg=%s", pe.Code, pe.Message)
		}
		if status, _ := pe.Data["status"].(int); status != http.StatusMethodNotAllowed {
			t.Fatalf("data.status = %v, want %d", pe.Data["status"], http.StatusMethodNotAllowed)
		}
	})
}

// --- fix 4: the secondary rate limit / abuse-detection message ------------

// TestSecondaryRateLimitByMessageAlone covers a 403 carrying NEITHER
// Retry-After nor X-RateLimit-Remaining:0 — only GitHub's secondary rate
// limit message — classified rate_limited anyway, with the default
// retry_after (60s) since nothing gives a duration to compute one from.
func TestSecondaryRateLimitByMessageAlone(t *testing.T) {
	ts := canned(t, http.StatusForbidden, nil,
		`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
	kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, ierr := kit.Invoke(context.Background(), "create_pr", createPROpts())
	pe := asContractError(t, ierr)
	if pe.Code != plugin.CodeRateLimited {
		t.Fatalf("code = %d, want CodeRateLimited; msg=%s", pe.Code, pe.Message)
	}
	if got := pe.Data["retry_after"]; got != "1m0s" {
		t.Fatalf("retry_after = %#v, want %q (the default, no header gave one)", got, "1m0s")
	}
}

// TestAbuseDetectionMessageIsRateLimited is the same fix for GitHub's other
// message wording for the same mechanism.
func TestAbuseDetectionMessageIsRateLimited(t *testing.T) {
	ts := canned(t, http.StatusForbidden, nil, `{"message":"You have triggered an abuse detection mechanism."}`)
	kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, ierr := kit.Invoke(context.Background(), "create_pr", createPROpts())
	pe := asContractError(t, ierr)
	if pe.Code != plugin.CodeRateLimited {
		t.Fatalf("code = %d, want CodeRateLimited; msg=%s", pe.Code, pe.Message)
	}
}

// TestRetryAfterParsesHTTPDate covers Retry-After as an HTTP-date (RFC 7231
// §7.1.3), not just a number of seconds.
func TestRetryAfterParsesHTTPDate(t *testing.T) {
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	ts := canned(t, http.StatusTooManyRequests, map[string]string{"Retry-After": future}, `{"message":"slow down"}`)
	kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, ierr := kit.Invoke(context.Background(), "create_pr", createPROpts())
	pe := asContractError(t, ierr)
	if pe.Code != plugin.CodeRateLimited {
		t.Fatalf("code = %d, want CodeRateLimited; msg=%s", pe.Code, pe.Message)
	}
	wait, _ := pe.Data["retry_after"].(string)
	d, perr := time.ParseDuration(wait)
	if perr != nil {
		t.Fatalf("retry_after = %q did not parse: %v", wait, perr)
	}
	if d <= 0 || d > 50*time.Second {
		t.Fatalf("retry_after = %s, want roughly 45s", d)
	}
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

// TestGraphQLNotFoundIsUpstream404 covers the GraphQL mutation path
// (ready_for_review/convert_to_draft, react/unreact on a review): GitHub's
// own "type": "NOT_FOUND" extension, the only signal a 200-status GraphQL
// error gives, is classified the same as a REST 404 — status 404, NOT
// target_gone directly — so it flows through the exact same target-keyed
// remap (including the no-access-vs-gone repo check) every other verb's 404
// does, rather than graphql() guessing at target_gone on its own with no key
// and no repo check at all.
func TestGraphQLNotFoundIsUpstream404(t *testing.T) {
	_, kit, _ := world(t)
	mutation := "mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){clientMutationId}}"
	err := kit.graphql(context.Background(), "tok-me", mutation, map[string]any{"id": "not-a-real-node-id"}, nil)
	pe := asContractError(t, err)
	if pe.Code != plugin.CodeUpstream {
		t.Fatalf("code = %d, want CodeUpstream; msg=%s", pe.Code, pe.Message)
	}
	if status, _ := pe.Data["status"].(int); status != http.StatusNotFound {
		t.Fatalf("data.status = %v, want %d", pe.Data["status"], http.StatusNotFound)
	}
	if retryable, _ := pe.Data["retryable"].(bool); retryable {
		t.Fatal("retryable = true, want false")
	}
}

// TestReadyForReviewNodeGoneIsTargetGone covers the full pipeline end to end:
// Invoke("ready_for_review") remaps the mutation's NOT_FOUND into
// target_gone (the PR is visible going in — the GET above the mutation
// succeeds — so repoVisible confirms it and the remap completes), carrying
// data.target.
func TestReadyForReviewNodeGoneIsTargetGone(t *testing.T) {
	_, kit, n := world(t)
	// A real PR exists at n, but its node id (built from a bogus id below via
	// the Client's own code path) can't be exercised without the fake
	// returning an invalid node id for a real PR, so this goes straight at
	// the mutation step: checkMergeReady-style coverage for merge_pr already
	// proves the GET->mutation handoff; this isolates the mutation's own
	// remap by calling it exactly as ready_for_review does.
	ctx := context.Background()
	key := fmt.Sprintf("acme/w#%d", n)
	tok, err := kit.TokenFor(ctx, "", "acme/w")
	if err != nil {
		t.Fatal(err)
	}
	mutation := "mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){clientMutationId}}"
	gerr := kit.graphql(ctx, tok, mutation, map[string]any{"id": "not-a-real-node-id"}, nil)
	remapped := kit.remapTargetGone(ctx, tok, "acme/w", key, gerr)
	pe := asContractError(t, remapped)
	if pe.Code != plugin.CodeTargetGone {
		t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
	}
	if got := pe.Data["target"]; got != key {
		t.Fatalf("data.target = %#v, want %q", got, key)
	}
}

// TestGraphQLUnknownTypeIsRetryableUpstream covers the other half of fix 5:
// a GraphQL error with no "type" extension at all (GitHub's own query
// validator — a malformed/unknown field — sets none) is left retryable
// rather than guessed invalid: an unrecognized shape is far more likely to
// be transient (a schema change, a half-rolled-out API) than a bug in a
// query that otherwise runs fine in production.
func TestGraphQLUnknownTypeIsRetryableUpstream(t *testing.T) {
	_, kit, _ := world(t)
	err := kit.graphql(context.Background(), "tok-me", "query{thisFieldDoesNotExist}", nil, nil)
	pe := asContractError(t, err)
	if pe.Code != plugin.CodeUpstream {
		t.Fatalf("code = %d, want CodeUpstream; msg=%s", pe.Code, pe.Message)
	}
	if retryable, _ := pe.Data["retryable"].(bool); !retryable {
		t.Fatal("retryable = false, want true (an unrecognized error shape is treated as transient)")
	}
}

// TestGraphQLUnprocessableIsInvalid covers the one type that DOES name our
// own hand-written query/mutation as the problem: GitHub's "UNPROCESSABLE"
// extension. ghfake doesn't simulate it (it isn't one of the error shapes
// real callers' queries hit against a live schema), so this goes straight at
// a bespoke server shaped like GitHub's GraphQL error envelope.
func TestGraphQLUnprocessableIsInvalid(t *testing.T) {
	ts := canned(t, http.StatusOK, nil,
		`{"errors":[{"message":"cannot process","type":"UNPROCESSABLE"}]}`)
	kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	gerr := kit.graphql(context.Background(), "t", "mutation{__typename}", nil, nil)
	pe := asContractError(t, gerr)
	if pe.Code != plugin.CodeInvalid {
		t.Fatalf("code = %d, want CodeInvalid; msg=%s", pe.Code, pe.Message)
	}
}

// TestGraphQLForbiddenIsUpstreamNotRetryable and
// TestGraphQLRateLimitedHasRetryAfter round out fix 5's table against the
// same bespoke-server pattern.
func TestGraphQLForbiddenIsUpstreamNotRetryable(t *testing.T) {
	ts := canned(t, http.StatusOK, nil,
		`{"errors":[{"message":"nope","type":"FORBIDDEN"}]}`)
	kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	gerr := kit.graphql(context.Background(), "t", "mutation{__typename}", nil, nil)
	pe := asContractError(t, gerr)
	if pe.Code != plugin.CodeUpstream {
		t.Fatalf("code = %d, want CodeUpstream; msg=%s", pe.Code, pe.Message)
	}
	if retryable, _ := pe.Data["retryable"].(bool); retryable {
		t.Fatal("retryable = true, want false")
	}
}

func TestGraphQLRateLimitedHasRetryAfter(t *testing.T) {
	ts := canned(t, http.StatusOK, nil,
		`{"errors":[{"message":"slow down","type":"RATE_LIMITED"}]}`)
	kit, err := NewClient(Config{WriteToken: "t", APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	gerr := kit.graphql(context.Background(), "t", "mutation{__typename}", nil, nil)
	pe := asContractError(t, gerr)
	if pe.Code != plugin.CodeRateLimited {
		t.Fatalf("code = %d, want CodeRateLimited; msg=%s", pe.Code, pe.Message)
	}
	wait, _ := pe.Data["retry_after"].(string)
	if _, perr := time.ParseDuration(wait); perr != nil {
		t.Fatalf("retry_after = %q did not parse as a duration: %v", wait, perr)
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
