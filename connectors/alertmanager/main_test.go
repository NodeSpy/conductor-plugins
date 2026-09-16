package main

import (
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// samplePayload is a captured (trimmed) Alertmanager v4 webhook with two
// alerts in one batch, one of them carrying an extra operator-defined label
// ("team") to prove label-flattening.
const samplePayload = `{
  "version": "4",
  "groupKey": "{}:{alertname=\"HighErrorRate\"}",
  "status": "firing",
  "receiver": "conductor-webhook",
  "groupLabels": {"alertname": "HighErrorRate"},
  "commonLabels": {"alertname": "HighErrorRate"},
  "commonAnnotations": {},
  "externalURL": "http://alertmanager.internal:9093",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "HighErrorRate",
        "severity": "critical",
        "instance": "web-1:9090",
        "job": "web",
        "team": "payments"
      },
      "annotations": {
        "summary": "error rate above threshold",
        "description": "5xx rate exceeded 5% for 5m",
        "runbook_url": "https://runbooks.example.com/high-error-rate"
      },
      "startsAt": "2026-09-16T10:00:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "http://prometheus.internal:9090/graph?g0.expr=...",
      "fingerprint": "abc123"
    },
    {
      "status": "firing",
      "labels": {
        "alertname": "DiskSpaceLow",
        "severity": "warning",
        "instance": "db-2:9100",
        "job": "node"
      },
      "annotations": {
        "summary": "disk space below 10%"
      },
      "startsAt": "2026-09-16T10:01:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "http://prometheus.internal:9090/graph?g0.expr=...",
      "fingerprint": "def456"
    }
  ]
}`

// TestParseEmitsOnePerAlert asserts the payload above yields one emitted
// event map per alert, each carrying the documented event/context/filter
// keys, and that label flattening does not clobber the named context keys.
func TestParseEmitsOnePerAlert(t *testing.T) {
	w, err := parse([]byte(samplePayload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(w.Alerts) != 2 {
		t.Fatalf("alerts: got %d want 2", len(w.Alerts))
	}

	got := make([]map[string]any, len(w.Alerts))
	for i, a := range w.Alerts {
		got[i] = alertEvent(w, a)
	}

	// First alert: full field coverage.
	e0 := got[0]
	if e0["event"] != "alert" || e0["kind"] != "firing" {
		t.Fatalf("event/kind: %#v %#v", e0["event"], e0["kind"])
	}
	if e0["dedup"] != "abc123\x00firing" {
		t.Fatalf("dedup: got %q", e0["dedup"])
	}
	ctx0, ok := e0["context"].(map[string]any)
	if !ok {
		t.Fatalf("context: not a map: %#v", e0["context"])
	}
	wantCtx := map[string]any{
		"status":       "firing",
		"alertname":    "HighErrorRate",
		"severity":     "critical",
		"summary":      "error rate above threshold",
		"description":  "5xx rate exceeded 5% for 5m",
		"instance":     "web-1:9090",
		"job":          "web",
		"runbook_url":  "https://runbooks.example.com/high-error-rate",
		"generatorURL": "http://prometheus.internal:9090/graph?g0.expr=...",
		"externalURL":  "http://alertmanager.internal:9093",
		"receiver":     "conductor-webhook",
		"alertnames":   "HighErrorRate",
		"severities":   "critical",
		"statuses":     "firing",
		"receivers":    "conductor-webhook",
		// operator-defined label, flattened, not clobbering any named key.
		"team": "payments",
	}
	for k, want := range wantCtx {
		if got := ctx0[k]; got != want {
			t.Errorf("context[%q]: got %#v want %#v", k, got, want)
		}
	}

	// Second alert: distinct fingerprint/labels, no "team" label present.
	e1 := got[1]
	if e1["dedup"] != "def456\x00firing" {
		t.Fatalf("dedup: got %q", e1["dedup"])
	}
	ctx1 := e1["context"].(map[string]any)
	if ctx1["alertname"] != "DiskSpaceLow" || ctx1["severity"] != "warning" {
		t.Errorf("second alert context: %#v", ctx1)
	}
	if _, present := ctx1["team"]; present {
		t.Errorf("second alert should carry no team label: %#v", ctx1)
	}
}

// TestDedupFiringThenResolved proves the fingerprint+status dedup key lets the
// same alert re-fire an event on the firing -> resolved transition (a
// distinct, wanted delivery) while rejecting an exact redelivery.
func TestDedupFiringThenResolved(t *testing.T) {
	dedup := sourcekit.NewDedup(2048)

	firing := alert{Fingerprint: "abc123", Status: "firing"}
	resolved := alert{Fingerprint: "abc123", Status: "resolved"}

	if !dedup.Add(dedupKey(firing)) {
		t.Fatal("first firing delivery should be new")
	}
	if dedup.Add(dedupKey(firing)) {
		t.Fatal("redelivered firing alert should be a duplicate")
	}
	if !dedup.Add(dedupKey(resolved)) {
		t.Fatal("the resolved transition is a distinct key and should be new")
	}
	if dedup.Add(dedupKey(resolved)) {
		t.Fatal("redelivered resolved alert should be a duplicate")
	}
}

// TestDescribe asserts the declared surface: kind, type, empty capabilities,
// the single "alert" event, and its filter keys.
func TestDescribe(t *testing.T) {
	d := alertmanager{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "alertmanager" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Egress) != 0 || len(d.Capabilities.Commands) != 0 ||
		len(d.Capabilities.FS) != 0 || d.Capabilities.Spawns {
		t.Fatalf("capabilities: want empty manifest, got %#v", d.Capabilities)
	}
	if len(d.Events) != 1 || d.Events[0].Name != "alert" {
		t.Fatalf("events: %#v", d.Events)
	}
	wantFilters := []string{"alertnames", "severities", "statuses", "receivers", "alertname", "severity", "status", "receiver"}
	for _, k := range wantFilters {
		if _, ok := d.Events[0].Filters[k]; !ok {
			t.Errorf("missing filter key %q", k)
		}
	}
	if len(d.Events[0].Filters) != len(wantFilters) {
		t.Errorf("filter count: got %d want %d (%#v)", len(d.Events[0].Filters), len(wantFilters), d.Events[0].Filters)
	}
}

// TestInvokeIsSourceOnly asserts Invoke refuses every verb call.
func TestInvokeIsSourceOnly(t *testing.T) {
	_, err := alertmanager{}.Invoke(plugin.InvokeRequest{Verb: "anything"})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams, got %v", err)
	}
}

// TestVerifyBearer drives the bearer-token check directly (no live listener):
// accept, reject wrong token, reject missing/malformed header, and the
// empty-secret pass-through the caller (requireBearerSecret) gates.
func TestVerifyBearer(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		header string
		want   bool
	}{
		{"correct token", "s3cret", "Bearer s3cret", true},
		{"wrong token", "s3cret", "Bearer nope", false},
		{"missing prefix", "s3cret", "s3cret", false},
		{"empty header", "s3cret", "", false},
		{"empty secret always passes", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyBearer(tc.secret, tc.header); got != tc.want {
				t.Errorf("verifyBearer(%q, %q): got %v want %v", tc.secret, tc.header, got, tc.want)
			}
		})
	}
}

// TestRequireBearerSecret asserts the fail-closed policy: empty secret and no
// allow_unsigned refuses to start; either a secret or allow_unsigned lets it
// through.
func TestRequireBearerSecret(t *testing.T) {
	if err := requireBearerSecret("alertmanager", "", map[string]any{}); err == nil {
		t.Fatal("empty secret with no allow_unsigned should fail closed")
	}
	if err := requireBearerSecret("alertmanager", "", map[string]any{"allow_unsigned": true}); err != nil {
		t.Fatalf("allow_unsigned should permit an empty secret: %v", err)
	}
	if err := requireBearerSecret("alertmanager", "s3cret", map[string]any{}); err != nil {
		t.Fatalf("a configured secret should never fail: %v", err)
	}
}

// asPluginError unwraps a *plugin.Error without importing errors just for the
// test (the SDK returns the concrete type directly).
func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}
