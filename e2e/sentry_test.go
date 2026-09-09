// Package e2e drives each plugin binary in this repo over the real plugin
// protocol (see internal/rpctest). These tests moved here from conductor's
// internal/plugin package when the plugin sources moved into this repo; they
// keep the same assertions, minus the daemon-client/sandbox layer, which is
// conductor's own concern and covered by its in-repo acme-* reference plugins.
package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestSentryPluginEndToEnd is the source-path proof for conductor-sentry: it
// builds the real plugin, starts its source over the protocol, delivers a
// signed synthetic Sentry webhook to the plugin's HTTP listener, and asserts
// the normalized event comes back over the event stream. This exercises SDK
// Serve + SourceHandler, sourcekit HMAC + listener, and the StartSource
// protocol.
func TestSentryPluginEndToEnd(t *testing.T) {
	bin := rpctest.Build(t, "sentry")
	c := rpctest.Start(t, bin)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	decl, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if decl.Type != "sentry" {
		t.Fatalf("decl.Type = %q, want sentry", decl.Type)
	}

	addr := rpctest.FreeAddr(t)
	secret := "sentry-hmac-secret"
	sink := rpctest.NewEventSink()
	req := plugin.StartSourceRequest{Instance: "sentry1", Config: map[string]any{
		"listen": addr, "path": "/sentry", "client_secret": secret,
	}}
	if err := c.StartSource(ctx, req, sink.Emit); err != nil {
		t.Fatalf("StartSource: %v", err)
	}

	body := []byte(`{"action":"created","data":{"issue":{"title":"NPE in checkout","level":"error","shortID":"WEB-1","culprit":"checkout.go","permalink":"https://sentry.io/x","project":{"slug":"web"}}}}`)
	url := "http://" + addr + "/sentry"
	rpctest.PostUntilAccepted(t, url, map[string]string{
		"Sentry-Hook-Resource":  "issue",
		"Sentry-Hook-Signature": rpctest.HMACHex(secret, body),
	}, body)

	if err := sink.Wait(ctx); err != nil {
		t.Fatalf("timed out waiting for the streamed sentry event: %v", err)
	}
	ev := sink.At(0)
	if ev["event"] != "issue_alert" {
		t.Fatalf("event = %v, want issue_alert", ev["event"])
	}
	ectx, _ := ev["context"].(map[string]any)
	if ectx["level"] != "error" || ectx["project"] != "web" || ectx["short_id"] != "WEB-1" {
		t.Fatalf("event context wrong: %+v", ectx)
	}

	// A bad-signature delivery must be rejected (401) and produce no event.
	before := sink.Len()
	if got := rpctest.PostStatus(t, url, map[string]string{
		"Sentry-Hook-Resource": "issue", "Sentry-Hook-Signature": "deadbeef",
	}, body); got != http.StatusUnauthorized {
		t.Fatalf("bad-signature webhook returned %d, want 401", got)
	}
	if after := sink.Len(); after != before {
		t.Fatalf("bad-signature delivery produced an event: before=%d after=%d", before, after)
	}
}
