package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestAWSCostPluginEndToEnd drives the built conductor-aws-cost binary over
// the plugin protocol: describe declares the purchase verbs with typed
// (number, required) amounts, a signed Cost Explorer read round-trips, and a
// purchase whose stated amount does not match the offering is refused before
// any purchase call reaches AWS.
func TestAWSCostPluginEndToEnd(t *testing.T) {
	bin := rpctest.BuildConnector(t, "aws-cost")
	c := rpctest.Start(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	decl, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if decl.Type != "aws-cost" {
		t.Fatalf("type %q", decl.Type)
	}
	for _, v := range decl.Verbs {
		if v.Name == "purchase_savings_plan" {
			if f := v.Options["total_commitment_usd"]; f.Type != "number" || !f.Required {
				t.Fatalf("total_commitment_usd not a required number: %+v", f)
			}
		}
	}

	purchased := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(403)
			return
		}
		switch {
		case r.Header.Get("X-Amz-Target") == "AWSInsightsIndexService.GetCostAndUsage":
			_, _ = w.Write([]byte(`{"ResultsByTime":[{"Total":{"UnblendedCost":{"Amount":"42.5","Unit":"USD"}}}]}`))
		case r.URL.Path == "/DescribeSavingsPlansOfferings":
			_, _ = w.Write([]byte(`{"searchResults":[{"offeringId":"o1","durationSeconds":31536000,"currency":"USD"}]}`))
		case r.URL.Path == "/CreateSavingsPlan":
			purchased = true
			_, _ = w.Write([]byte(`{"savingsPlanId":"sp-x"}`))
		default:
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	conn := map[string]any{"access_key_id": "AKID", "secret_access_key": "S", "endpoint_ce": srv.URL, "endpoint_savingsplans": srv.URL}

	out, err := c.Invoke(ctx, plugin.InvokeRequest{Verb: "cost_and_usage", Connection: conn, Options: map[string]any{"start": "2026-09-01", "end": "2026-10-01"}})
	if err != nil {
		t.Fatal(err)
	}
	if out["total"] != 42.5 {
		t.Fatalf("total %v", out["total"])
	}
	if _, err := c.Invoke(ctx, plugin.InvokeRequest{Verb: "purchase_savings_plan", Connection: conn,
		Options: map[string]any{"offering_id": "o1", "commitment_per_hour": 1.0, "total_commitment_usd": 10.0}}); err == nil {
		t.Fatal("understated purchase accepted over the wire")
	}
	if purchased {
		t.Fatal("CreateSavingsPlan reached AWS for a mismatched amount")
	}
}
