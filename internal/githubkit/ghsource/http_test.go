package ghsource

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// When the engine has opened webhook.expose for this listener (the
// `listeners` connection semantic) it hands back the public URL in
// webhook.public_url; serveHTTP logs it, since registering it with GitHub
// is the operator's job, not this plugin's.
func TestServeHTTPLogsThePublicURLWhenExposed(t *testing.T) {
	g := newTestIntegration(t, richConfig())
	g.cfg.Webhook.Listen = "127.0.0.1:0"
	g.cfg.Webhook.PublicURL = "https://hook.example/abc"

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = g.serveHTTP(ctx, func(context.Context, Trigger) {}, newDeliveryDedup(8))
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // let ListenAndServe log before we cancel
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveHTTP did not stop after cancel")
	}

	if !strings.Contains(buf.String(), "https://hook.example/abc") {
		t.Fatalf("log output = %q, want it to mention the public URL", buf.String())
	}
}

// With no public URL configured (expose unset, or not yet opened), nothing
// extra is logged.
func TestServeHTTPLogsNothingWithNoPublicURL(t *testing.T) {
	g := newTestIntegration(t, richConfig())
	g.cfg.Webhook.Listen = "127.0.0.1:0"
	g.cfg.Webhook.PublicURL = ""

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = g.serveHTTP(ctx, func(context.Context, Trigger) {}, newDeliveryDedup(8))
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveHTTP did not stop after cancel")
	}

	if strings.Contains(buf.String(), "is reachable at") {
		t.Fatalf("log output = %q, want no public-URL line", buf.String())
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
