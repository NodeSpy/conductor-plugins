package ghsource

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDeliveryDedup(t *testing.T) {
	d := newDeliveryDedup(2)
	if !d.add("a") || !d.add("b") {
		t.Fatal("new ids should be accepted")
	}
	if d.add("a") {
		t.Fatal("duplicate should be rejected")
	}
	// Evict oldest ("a") when capacity exceeded; "a" then looks new again.
	d.add("c")
	if !d.add("a") {
		t.Fatal("evicted id should be treated as new")
	}
}

func smeeData(t *testing.T, event, sig string, body string) string {
	t.Helper()
	p := map[string]any{
		"x-github-event":      event,
		"x-github-delivery":   "d1",
		"x-hub-signature-256": sig,
		"body":                json.RawMessage(body),
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func changesRequestedBody() string {
	return `{"action":"submitted","repository":{"full_name":"acme/w","name":"w","owner":{"login":"acme"}},` +
		`"pull_request":{"number":6,"head":{"sha":"h"},"user":{"login":"me"}},"review":{"state":"changes_requested","id":1,"user":{"login":"r"}}}`
}

func TestHandleSmeeData(t *testing.T) {
	g := newTestIntegration(t, richConfig()) // verify defaults true, but we set it below
	off := false
	g.cfg.Webhook.VerifySig = &off

	var got []Trigger
	emit := func(_ context.Context, tr Trigger) { got = append(got, tr) }
	seen := newDeliveryDedup(8)

	data := smeeData(t, "pull_request_review", "", changesRequestedBody())
	g.handleSmeeData(context.Background(), emit, seen, data)
	if len(got) != 1 || got[0].Kind != "changes_requested" {
		t.Fatalf("want 1 changes_requested, got %+v", got)
	}
	// Same delivery id again → deduped.
	g.handleSmeeData(context.Background(), emit, seen, data)
	if len(got) != 1 {
		t.Fatalf("duplicate delivery should be ignored, got %d", len(got))
	}
	// Control frame (not JSON object with event) → ignored.
	g.handleSmeeData(context.Background(), emit, seen, `"ready"`)
	if len(got) != 1 {
		t.Fatalf("control frame should be ignored, got %d", len(got))
	}
}

func TestHandleSmeeSignature(t *testing.T) {
	g := newTestIntegration(t, richConfig())
	on := true
	g.cfg.Webhook.VerifySig = &on
	g.cfg.Webhook.Secret = "s3cr3t"
	body := changesRequestedBody()

	var got []Trigger
	emit := func(_ context.Context, tr Trigger) { got = append(got, tr) }

	// Bad signature → dropped.
	bad := smeeData(t, "pull_request_review", "sha256=deadbeef", body)
	g.handleSmeeData(context.Background(), emit, newDeliveryDedup(8), bad)
	if len(got) != 0 {
		t.Fatalf("bad signature should be dropped, got %d", len(got))
	}

	// Correct signature over the exact body bytes → accepted.
	good := smeeData(t, "pull_request_review", sign("s3cr3t", []byte(body)), body)
	g.handleSmeeData(context.Background(), emit, newDeliveryDedup(8), good)
	if len(got) != 1 {
		t.Fatalf("valid signature should be accepted, got %d", len(got))
	}
}
