package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestPagerDutyPluginEndToEnd proves the conductor-pagerduty source path
// including the multi-value X-PagerDuty-Signature (v1=…,v1=…) that sourcekit
// verifies ANY-match, as PagerDuty sends during signing-key rotation.
func TestPagerDutyPluginEndToEnd(t *testing.T) {
	bin := rpctest.Build(t, "pagerduty")
	c := rpctest.Start(t, bin)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	decl, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if decl.Type != "pagerduty" {
		t.Fatalf("decl.Type = %q, want pagerduty", decl.Type)
	}

	addr := rpctest.FreeAddr(t)
	secret := "pd-signing-secret"
	sink := rpctest.NewEventSink()
	req := plugin.StartSourceRequest{Instance: "pd1", Config: map[string]any{
		"listen": addr, "path": "/pagerduty", "signing_secret": secret,
	}}
	if err := c.StartSource(ctx, req, sink.Emit); err != nil {
		t.Fatalf("StartSource: %v", err)
	}

	body := []byte(`{"event":{"event_type":"incident.triggered","data":{"id":"PD1","number":42,"status":"triggered","title":"DB down","html_url":"https://pd/x","urgency":"high","priority":{"summary":"P1"},"service":{"id":"SVC1","summary":"api"}}}}`)
	// Two v1= values, only the second valid — proves ANY-match rotation handling.
	sig := "v1=deadbeef,v1=" + rpctest.HMACHex(secret, body)

	rpctest.PostUntilAccepted(t, "http://"+addr+"/pagerduty",
		map[string]string{"X-PagerDuty-Signature": sig}, body)

	if err := sink.Wait(ctx); err != nil {
		t.Fatalf("timed out waiting for the streamed pagerduty event: %v", err)
	}
	ev := sink.At(0)
	if ev["event"] != "incident" || ev["kind"] != "incident.triggered" {
		t.Fatalf("event/kind wrong: %+v", ev)
	}
	ectx, _ := ev["context"].(map[string]any)
	if ectx["urgencies"] != "high" || ectx["event_types"] != "incident.triggered" {
		t.Fatalf("filter-key context wrong: %+v", ectx)
	}
}
