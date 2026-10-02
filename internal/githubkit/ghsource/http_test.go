package ghsource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookHandlerDirect(t *testing.T) {
	g := newTestIntegration(t, richConfig())
	on := true
	g.cfg.Webhook.VerifySig = &on
	g.cfg.Webhook.Secret = "s3cr3t"

	var got []Trigger
	emit := func(_ context.Context, tr Trigger) { got = append(got, tr) }
	h := g.webhookHandler(context.Background(), emit, newDeliveryDedup(8))
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := changesRequestedBody()

	// Valid signature over the exact raw body → accepted (this is the win over smee).
	post(t, srv.URL, "pull_request_review", "d1", sign("s3cr3t", []byte(body)), body, http.StatusAccepted)
	if len(got) != 1 || got[0].Kind != "changes_requested" {
		t.Fatalf("want 1 changes_requested, got %+v", got)
	}

	// Duplicate delivery id → deduped (still 202, but no new trigger).
	post(t, srv.URL, "pull_request_review", "d1", sign("s3cr3t", []byte(body)), body, http.StatusAccepted)
	if len(got) != 1 {
		t.Fatalf("duplicate delivery should be ignored, got %d", len(got))
	}

	// Bad signature → dropped.
	post(t, srv.URL, "pull_request_review", "d2", "sha256=bad", body, http.StatusAccepted)
	if len(got) != 1 {
		t.Fatalf("bad signature should be dropped, got %d", len(got))
	}

	// ping → 200 pong, no trigger.
	post(t, srv.URL, "ping", "d3", "", `{"zen":"hi"}`, http.StatusOK)
	if len(got) != 1 {
		t.Fatalf("ping should not trigger, got %d", len(got))
	}
}

func post(t *testing.T, url, event, delivery, sig, body string, wantStatus int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("event %q: status %d, want %d", event, resp.StatusCode, wantStatus)
	}
}
