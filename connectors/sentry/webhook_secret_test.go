package main

import (
	"context"
	"strings"
	"testing"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// §7: sourcekit.VerifyHMAC returns true for an empty secret — it leaves
// the policy to the caller, and no caller had one. An unsigned listener
// accepts any POST on the address as a real provider event, which is
// remote trigger injection with no signal it happened.
func TestStartSourceRefusesAnUnsignedListener(t *testing.T) {
	err := sentry{}.StartSource(context.Background(), plugin.StartSourceRequest{
		Instance: "s",
		Config:   map[string]any{"listen": "127.0.0.1:0"},
	}, func(any) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "allow_unsigned") {
		t.Fatalf("a secretless listener must refuse to start and name the opt-out, got %v", err)
	}
}

// The opt-out is explicit and greppable, for someone fronting the
// listener with their own authentication.
func TestStartSourceAllowsUnsignedWhenOptedIn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Serve returns immediately; we only care that the guard passed
	err := sentry{}.StartSource(ctx, plugin.StartSourceRequest{
		Instance: "s",
		Config:   map[string]any{"listen": "127.0.0.1:0", "allow_unsigned": true},
	}, func(any) error { return nil })
	if err != nil && strings.Contains(err.Error(), "allow_unsigned") {
		t.Fatalf("allow_unsigned must let it start: %v", err)
	}
}

// A configured secret starts as before.
func TestStartSourceWithASecretPassesTheGuard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sentry{}.StartSource(ctx, plugin.StartSourceRequest{
		Instance: "s",
		Config:   map[string]any{"listen": "127.0.0.1:0", "client_secret": "shh"},
	}, func(any) error { return nil })
	if err != nil && strings.Contains(err.Error(), "allow_unsigned") {
		t.Fatalf("a configured secret must pass the guard: %v", err)
	}
}
