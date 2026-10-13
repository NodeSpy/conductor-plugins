package ghsource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSweepBounds(t *testing.T) {
	// Defaults when unset: 2m floor, 1h ceiling.
	min, max := sweepBounds(SweepConfig{})
	if min != 2*time.Minute || max != time.Hour {
		t.Fatalf("defaults wrong: min=%s max=%s", min, max)
	}
	// Configured values honored.
	min, max = sweepBounds(SweepConfig{Interval: dur(t, "30m"), MinInterval: dur(t, "1m")})
	if min != time.Minute || max != 30*time.Minute {
		t.Fatalf("configured wrong: min=%s max=%s", min, max)
	}
	// A below-floor min_interval is clamped up to sweepFloor (no API flood).
	min, _ = sweepBounds(SweepConfig{MinInterval: dur(t, "5s")})
	if min != sweepFloor {
		t.Fatalf("floor clamp: min=%s, want %s", min, sweepFloor)
	}
	// Floor clamped to never exceed the ceiling.
	min, max = sweepBounds(SweepConfig{Interval: dur(t, "1m"), MinInterval: dur(t, "10m")})
	if min != time.Minute || max != time.Minute {
		t.Fatalf("floor should clamp to ceiling: min=%s max=%s", min, max)
	}
}

func TestBackoffInterval(t *testing.T) {
	max := time.Hour
	got := []time.Duration{}
	cur := 2 * time.Minute
	for i := 0; i < 7; i++ {
		cur = backoffInterval(cur, max)
		got = append(got, cur)
	}
	// 2m → 4,8,16,32,64→cap 60m, stays 60m.
	want := []time.Duration{4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour, time.Hour}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff[%d] = %s, want %s (seq %v)", i, got[i], want[i], got)
		}
	}
}

// dur is a test helper to build a duration from a string like "30m".
func dur(t *testing.T, s string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSmeeReconnectSignalsRenew(t *testing.T) {
	// Always accept the connection then immediately end the stream (empty body) so
	// runSmee cycles: first connect (no renew), drop, reconnect → renew.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := newTestIntegration(t, Config{
		App:     AppConfig{AppID: 1, PrivateKeyPath: "x"},
		Webhook: WebhookConfig{SmeeURL: srv.URL, Secret: "s"},
	})
	renew := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.runSmee(ctx, func(context.Context, Trigger) {}, newDeliveryDedup(8), renew)

	select {
	case <-renew:
		// Reconnect after the first drop nudged a catch-up sweep — the point.
	case <-time.After(5 * time.Second):
		t.Fatal("expected a renew signal after a smee reconnect")
	}
}
