package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// mockAWS stands in for Cost Explorer (by X-Amz-Target), Savings Plans (by
// path) and EC2 (by Action), records every call, and checks each one is
// SigV4-signed for the right service.
type mockAWS struct {
	*httptest.Server
	t     *testing.T
	mu    sync.Mutex
	calls []call
	json  map[string]any    // CE target op / SP path → JSON response
	xml   map[string]string // EC2 Action → XML response
	fail  map[string]string // EC2 Action → XML error body (HTTP 412)
}

type call struct {
	service, op string
	json        map[string]any
	form        url.Values
}

func newMock(t *testing.T, service string) *mockAWS {
	m := &mockAWS{t: t, json: map[string]any{}, xml: map[string]string{}, fail: map[string]string{}}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, "/"+service+"/aws4_request") || !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKID/") {
			t.Errorf("%s call not SigV4-signed for %s: %q", r.URL.Path, service, auth)
		}
		raw, _ := io.ReadAll(r.Body)
		c := call{service: service}
		switch service {
		case "ce":
			c.op = strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AWSInsightsIndexService.")
			_ = json.Unmarshal(raw, &c.json)
		case "savingsplans":
			c.op = r.URL.Path
			_ = json.Unmarshal(raw, &c.json)
		case "ec2":
			c.form, _ = url.ParseQuery(string(raw))
			c.op = c.form.Get("Action")
		}
		m.mu.Lock()
		m.calls = append(m.calls, c)
		m.mu.Unlock()
		if x, ok := m.fail[c.op]; ok {
			w.WriteHeader(412)
			_, _ = w.Write([]byte(x))
			return
		}
		if x, ok := m.xml[c.op]; ok {
			_, _ = w.Write([]byte(x))
			return
		}
		if j, ok := m.json[c.op]; ok {
			b, _ := json.Marshal(j)
			_, _ = w.Write(b)
			return
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"__type":"UnknownOperation","message":"no canned response for ` + c.op + `"}`))
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mockAWS) ops() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, c := range m.calls {
		out = append(out, c.op)
	}
	return out
}

func (m *mockAWS) last() call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[len(m.calls)-1]
}

type mocks struct{ ce, sp, ec2 *mockAWS }

func newMocks(t *testing.T) mocks {
	return mocks{ce: newMock(t, "ce"), sp: newMock(t, "savingsplans"), ec2: newMock(t, "ec2")}
}

func (m mocks) conn() map[string]any {
	return map[string]any{
		"region": "us-west-2", "access_key_id": "AKID", "secret_access_key": "SECRET",
		"endpoint_ce": m.ce.URL, "endpoint_savingsplans": m.sp.URL, "endpoint_ec2": m.ec2.URL,
	}
}

func invoke(t *testing.T, m mocks, verb string, opts map[string]any) (map[string]any, error) {
	t.Helper()
	res, err := costPlugin{}.Invoke(plugin.InvokeRequest{Verb: verb, Connection: m.conn(), Options: opts})
	return res.Outputs, err
}

func TestDescribePurchaseAmountsAreTyped(t *testing.T) {
	d := costPlugin{}.Describe()
	if d.Type != "aws-cost" || d.Kind != plugin.KindConnector {
		t.Fatalf("decl: %s %s", d.Type, d.Kind)
	}
	want := map[string]string{"purchase_savings_plan": "total_commitment_usd", "purchase_reserved_instances": "total_cost_usd"}
	seen := 0
	for _, v := range d.Verbs {
		if opt, ok := want[v.Name]; ok {
			f := v.Options[opt]
			if f.Type != "number" || !f.Required {
				t.Errorf("%s.%s must be a required number (the spend cap's typed amount), got %+v", v.Name, opt, f)
			}
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("purchase verbs missing: %d", seen)
	}
	if !d.Connection["secret_access_key"].Secret {
		t.Error("secret_access_key must be marked secret")
	}
}

func TestCostAndUsageSumsAndSigns(t *testing.T) {
	m := newMocks(t)
	m.ce.json["GetCostAndUsage"] = map[string]any{"ResultsByTime": []any{
		map[string]any{"Groups": []any{
			map[string]any{"Keys": []any{"EC2"}, "Metrics": map[string]any{"UnblendedCost": map[string]any{"Amount": "100.10", "Unit": "USD"}}},
			map[string]any{"Keys": []any{"S3"}, "Metrics": map[string]any{"UnblendedCost": map[string]any{"Amount": "20.25", "Unit": "USD"}}},
		}},
		map[string]any{"Total": map[string]any{"UnblendedCost": map[string]any{"Amount": "3", "Unit": "USD"}}},
	}}
	out, err := invoke(t, m, "cost_and_usage", map[string]any{"start": "2026-09-01", "end": "2026-10-01", "group_by": "SERVICE"})
	if err != nil {
		t.Fatal(err)
	}
	if out["total"] != 123.35 || out["unit"] != "USD" {
		t.Fatalf("total %v %v", out["total"], out["unit"])
	}
	c := m.ce.last()
	gb, _ := c.json["GroupBy"].([]any)
	if c.op != "GetCostAndUsage" || len(gb) != 1 || c.json["Granularity"] != "MONTHLY" {
		t.Fatalf("request: %+v", c)
	}
	for _, bad := range []map[string]any{{"start": "x", "end": "2026-10-01"}, {"start": "2026-10-02", "end": "2026-10-01"}} {
		if _, err := invoke(t, m, "cost_and_usage", bad); err == nil {
			t.Errorf("bad period accepted: %v", bad)
		}
	}
}

func TestReadsHitTheRightOperations(t *testing.T) {
	m := newMocks(t)
	m.ce.json["GetReservationUtilization"] = map[string]any{"Total": map[string]any{"UtilizationPercentage": "91"}, "UtilizationsByTime": []any{}}
	m.ce.json["GetSavingsPlansPurchaseRecommendation"] = map[string]any{"SavingsPlansPurchaseRecommendation": map[string]any{"TermInYears": "ONE_YEAR"}}
	m.sp.json["/DescribeSavingsPlans"] = map[string]any{"savingsPlans": []any{map[string]any{"savingsPlanId": "sp-1"}}}
	m.ec2.xml["DescribeReservedInstances"] = `<DescribeReservedInstancesResponse><reservedInstancesSet><item><reservedInstancesId>ri-1</reservedInstancesId><instanceType>m5.large</instanceType><instanceCount>2</instanceCount><state>active</state></item></reservedInstancesSet></DescribeReservedInstancesResponse>`
	if out, err := invoke(t, m, "reservation_utilization", map[string]any{"start": "2026-09-01", "end": "2026-10-01"}); err != nil || out["total"] == nil {
		t.Fatalf("utilization: %v %v", out, err)
	}
	if out, err := invoke(t, m, "savings_plans_recommendation", nil); err != nil || out["recommendation"] == nil {
		t.Fatalf("recommendation: %v %v", out, err)
	}
	if c := m.ce.last(); c.json["SavingsPlansType"] != "COMPUTE_SP" || c.json["LookbackPeriodInDays"] != "THIRTY_DAYS" {
		t.Fatalf("recommendation defaults: %+v", c.json)
	}
	if out, err := invoke(t, m, "savings_plans", nil); err != nil || len(out["savings_plans"].([]any)) != 1 {
		t.Fatalf("savings_plans: %v %v", out, err)
	}
	out, err := invoke(t, m, "reserved_instances", map[string]any{"state": "active"})
	if err != nil {
		t.Fatal(err)
	}
	ris := out["reserved_instances"].([]any)
	if len(ris) != 1 || ris[0].(map[string]any)["instance_count"] != 2 {
		t.Fatalf("reserved_instances: %+v", ris)
	}
	if f := m.ec2.last().form; f.Get("Filter.1.Value.1") != "active" || f.Get("Version") == "" {
		t.Fatalf("ec2 form: %v", f)
	}
}

const spOffering = "off-1"

func spMocks(t *testing.T, currency string) mocks {
	m := newMocks(t)
	m.sp.json["/DescribeSavingsPlansOfferings"] = map[string]any{"searchResults": []any{
		map[string]any{"offeringId": spOffering, "durationSeconds": 31536000, "currency": currency, "paymentOption": "No Upfront"},
	}}
	m.sp.json["/CreateSavingsPlan"] = map[string]any{"savingsPlanId": "sp-new"}
	return m
}

// 0.50/hour × 8760 hours = 4380.00
func TestPurchaseSavingsPlanVerifiesTheTotal(t *testing.T) {
	m := spMocks(t, "USD")
	out, err := invoke(t, m, "purchase_savings_plan", map[string]any{"offering_id": spOffering, "commitment_per_hour": 0.5, "total_commitment_usd": 4380.0})
	if err != nil {
		t.Fatal(err)
	}
	if out["savings_plan_id"] != "sp-new" || out["total_commitment"] != 4380.0 {
		t.Fatalf("out: %v", out)
	}
	c := m.sp.last()
	if c.op != "/CreateSavingsPlan" || c.json["commitment"] != "0.5" || c.json["savingsPlanOfferingId"] != spOffering || c.json["clientToken"] == "" {
		t.Fatalf("create request: %+v", c.json)
	}
}

func TestPurchaseSavingsPlanRefusesAnyOtherAmount(t *testing.T) {
	cases := map[string]map[string]any{
		"understated total": {"offering_id": spOffering, "commitment_per_hour": 0.5, "total_commitment_usd": 100.0},
		"overstated total":  {"offering_id": spOffering, "commitment_per_hour": 0.5, "total_commitment_usd": 5000.0},
		"string total":      {"offering_id": spOffering, "commitment_per_hour": 0.5, "total_commitment_usd": "4380"},
		"missing total":     {"offering_id": spOffering, "commitment_per_hour": 0.5},
		"zero hourly":       {"offering_id": spOffering, "commitment_per_hour": 0.0, "total_commitment_usd": 0.0},
		"no offering":       {"commitment_per_hour": 0.5, "total_commitment_usd": 4380.0},
		"upfront > total":   {"offering_id": spOffering, "commitment_per_hour": 0.5, "total_commitment_usd": 4380.0, "upfront_payment": 9999.0},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			m := spMocks(t, "USD")
			if _, err := invoke(t, m, "purchase_savings_plan", opts); err == nil {
				t.Fatal("purchase went through")
			}
			for _, op := range m.sp.ops() {
				if op == "/CreateSavingsPlan" {
					t.Fatal("CreateSavingsPlan was called")
				}
			}
		})
	}
	t.Run("wrong currency", func(t *testing.T) {
		m := spMocks(t, "EUR")
		if _, err := invoke(t, m, "purchase_savings_plan", map[string]any{"offering_id": spOffering, "commitment_per_hour": 0.5, "total_commitment_usd": 4380.0}); err == nil || !strings.Contains(err.Error(), "EUR") {
			t.Fatalf("currency mismatch: %v", err)
		}
	})
}

const riOfferingXML = `<DescribeReservedInstancesOfferingsResponse><reservedInstancesOfferingsSet><item>
<reservedInstancesOfferingId>ri-off-1</reservedInstancesOfferingId><instanceType>m5.large</instanceType>
<duration>31536000</duration><fixedPrice>500.0</fixedPrice><usagePrice>0.0</usagePrice><currencyCode>USD</currencyCode>
<offeringType>Partial Upfront</offeringType><offeringClass>standard</offeringClass><productDescription>Linux/UNIX</productDescription>
<recurringCharges><item><frequency>Hourly</frequency><amount>0.05</amount></item></recurringCharges>
</item></reservedInstancesOfferingsSet></DescribeReservedInstancesOfferingsResponse>`

// one instance: 500 upfront + 0.05/hour × 8760 = 938.00; two = 1876.00
func TestPurchaseReservedInstancesVerifiesTheTotal(t *testing.T) {
	m := newMocks(t)
	m.ec2.xml["DescribeReservedInstancesOfferings"] = riOfferingXML
	m.ec2.xml["PurchaseReservedInstancesOffering"] = `<PurchaseReservedInstancesOfferingResponse><reservedInstancesId>ri-new</reservedInstancesId></PurchaseReservedInstancesOfferingResponse>`
	offs, err := invoke(t, m, "reserved_instance_offerings", map[string]any{"offering_ids": []any{"ri-off-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if per := offs["offerings"].([]any)[0].(map[string]any)["total_cost_per_instance"]; per != 938.0 {
		t.Fatalf("per-instance total %v", per)
	}
	if _, err := invoke(t, m, "purchase_reserved_instances", map[string]any{"offering_id": "ri-off-1", "instance_count": 2, "total_cost_usd": 1000.0}); err == nil {
		t.Fatal("mismatched total bought")
	}
	for _, op := range m.ec2.ops() {
		if op == "PurchaseReservedInstancesOffering" {
			t.Fatal("purchase called on a mismatch")
		}
	}
	out, err := invoke(t, m, "purchase_reserved_instances", map[string]any{"offering_id": "ri-off-1", "instance_count": 2, "total_cost_usd": 1876.0})
	if err != nil {
		t.Fatal(err)
	}
	if out["reserved_instances_id"] != "ri-new" || out["total_cost"] != 1876.0 {
		t.Fatalf("out %v", out)
	}
	f := m.ec2.last().form
	if f.Get("InstanceCount") != "2" || f.Get("LimitPrice.Amount") != "1000.00" || f.Get("LimitPrice.CurrencyCode") != "USD" {
		t.Fatalf("purchase form: %v", f)
	}
	if _, err := invoke(t, m, "purchase_reserved_instances", map[string]any{"offering_id": "ri-off-1", "instance_count": 0, "total_cost_usd": 0.0}); err == nil {
		t.Fatal("zero count accepted")
	}
}

func TestPurchaseReservedInstancesDryRun(t *testing.T) {
	m := newMocks(t)
	m.ec2.xml["DescribeReservedInstancesOfferings"] = riOfferingXML
	// EC2 answers a dry run that WOULD succeed with a DryRunOperation error.
	m.ec2.fail["PurchaseReservedInstancesOffering"] = `<Response><Errors><Error><Code>DryRunOperation</Code><Message>Request would have succeeded, but DryRun flag is set.</Message></Error></Errors></Response>`
	out, err := invoke(t, m, "purchase_reserved_instances", map[string]any{"offering_id": "ri-off-1", "instance_count": 1, "total_cost_usd": 938.0, "dry_run": true})
	if err != nil || out["dry_run"] != true || out["reserved_instances_id"] != "" {
		t.Fatalf("dry run: %v %v", out, err)
	}
	if f := m.ec2.last().form; f.Get("DryRun") != "true" {
		t.Fatalf("DryRun not sent: %v", f)
	}
	// Any other refusal surfaces, dry run or not.
	m.ec2.fail["PurchaseReservedInstancesOffering"] = `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>no</Message></Error></Errors></Response>`
	if _, err := invoke(t, m, "purchase_reserved_instances", map[string]any{"offering_id": "ri-off-1", "instance_count": 1, "total_cost_usd": 938.0, "dry_run": true}); err == nil || !strings.Contains(err.Error(), "UnauthorizedOperation") {
		t.Fatalf("authorization failure hidden: %v", err)
	}
}

func TestUnknownVerb(t *testing.T) {
	m := newMocks(t)
	if _, err := invoke(t, m, "explode", nil); err == nil {
		t.Fatal("unknown verb accepted")
	}
}
