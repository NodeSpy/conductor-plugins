package e2e

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestAsanaPluginEndToEnd is the source-path proof for conductor-asana over
// the real plugin protocol. Asana's webhook model differs from the other
// sources here: the signing secret is installed by a HANDSHAKE (a POST
// carrying X-Hook-Secret that the receiver must echo with a 200), and
// deliveries are then signed with X-Hook-Signature. This test builds the
// plugin, starts its source in bootstrap mode (no pinned secret), completes
// the handshake, delivers a signed compact event, and asserts the normalized
// task event streams back — then checks a bad signature is refused.
func TestAsanaPluginEndToEnd(t *testing.T) {
	bin := rpctest.BuildConnector(t, "asana")
	c := rpctest.Start(t, bin)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	decl, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if decl.Type != "asana" || decl.Kind != plugin.KindConnector {
		t.Fatalf("decl = %s/%s, want connector/asana", decl.Kind, decl.Type)
	}
	if len(decl.Capabilities.Egress) != 1 || decl.Capabilities.Egress[0] != "app.asana.com:443" {
		t.Fatalf("declared egress = %v, want exactly [app.asana.com:443]", decl.Capabilities.Egress)
	}
	if decl.Auth == nil || decl.Auth.TokenURL == "" {
		t.Fatal("asana should declare managed-OAuth2 endpoints")
	}

	addr := rpctest.FreeAddr(t)
	sink := rpctest.NewEventSink()
	// No token → enrichment is off; the compact event streams as-is.
	req := plugin.StartSourceRequest{Instance: "asana1", Config: map[string]any{
		"webhook": map[string]any{"listen": addr, "path": "/asana"},
	}}
	if err := c.StartSource(ctx, req, sink.Emit); err != nil {
		t.Fatalf("StartSource: %v", err)
	}
	url := "http://" + addr + "/asana"

	// 1. Handshake: retried until the listener is up; the secret must be echoed.
	secret := "asana-handshake-secret"
	handshake(t, url, secret)

	// 2. A delivery signed under the handshaken secret streams a task event.
	body := []byte(`{"events":[{"action":"changed","created_at":"2026-09-21T10:00:00.000Z","user":{"gid":"u1","resource_type":"user"},"resource":{"gid":"t1","resource_type":"task","resource_subtype":"default_task"},"parent":{"gid":"p1","resource_type":"project"},"change":{"field":"completed","action":"changed"}}]}`)
	if got := rpctest.PostStatus(t, url, map[string]string{
		"X-Hook-Signature": rpctest.HMACHex(secret, body),
	}, body); got != http.StatusOK {
		t.Fatalf("signed delivery returned %d, want 200", got)
	}
	if err := sink.Wait(ctx); err != nil {
		t.Fatalf("timed out waiting for the streamed asana event: %v", err)
	}
	ev := sink.At(0)
	if ev["event"] != "task" {
		t.Fatalf("event = %v, want task", ev["event"])
	}
	ectx, _ := ev["context"].(map[string]any)
	if ectx["gid"] != "t1" || ectx["action"] != "changed" || ectx["field"] != "completed" || ectx["parent_gid"] != "p1" || ectx["user"] != "u1" {
		t.Fatalf("event context wrong: %+v", ectx)
	}

	// 3. A bad-signature delivery is refused (401) and produces no event.
	before := sink.Len()
	if got := rpctest.PostStatus(t, url, map[string]string{"X-Hook-Signature": "deadbeef"}, body); got != http.StatusUnauthorized {
		t.Fatalf("bad-signature webhook returned %d, want 401", got)
	}
	// A second handshake is refused once a secret is installed: an intruder
	// who can reach the port must not be able to swap in their own key.
	r, _ := http.NewRequest(http.MethodPost, url, nil)
	r.Header.Set("X-Hook-Secret", "intruder")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("second handshake returned %d, want 403", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if after := sink.Len(); after != before {
		t.Fatalf("refused deliveries produced an event: before=%d after=%d", before, after)
	}
}

// handshake POSTs Asana's X-Hook-Secret handshake until the listener answers,
// then asserts the 200 + echoed header the Asana API requires.
func handshake(t *testing.T, url, secret string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		r, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(nil))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("X-Hook-Secret", secret)
		resp, err := http.DefaultClient.Do(r)
		if err == nil {
			last = resp.StatusCode
			echo := resp.Header.Get("X-Hook-Secret")
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if echo != secret {
					t.Fatalf("handshake answered 200 but echoed X-Hook-Secret %q, want %q", echo, secret)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never completed the asana handshake against %s (last status %d)", url, last)
}
