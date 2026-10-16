// Command conductor-aws-cost is the AWS cost & commitments connector as an
// external conductor plugin: Cost Explorer reads (spend, forecast,
// reservation / Savings Plans utilization, coverage and purchase
// recommendations), the account's Savings Plans and EC2 Reserved Instances
// with their offerings, and the two PURCHASE verbs — a Savings Plan and an
// EC2 Reserved Instance offering.
//
// The purchase verbs carry a TYPED, REQUIRED total amount
// (total_commitment_usd / total_cost_usd). Conductor's spend cap (a bot's
// autonomy.spend_cap, keyed by autonomy.amounts) caps that number — so the
// plugin does not trust it: before buying, it looks the offering up, derives
// the real total commitment from the offering's own terms, and refuses unless
// the stated amount equals it (to the cent) and the offering's currency is the
// connector's. The amount a cap approved is therefore the amount spent.
//
// SDK-free and CLI-free like the other AWS plugins here: plain HTTPS signed
// with internal/awskit's SigV4. Cost Explorer and Savings Plans are global
// services signed in us-east-1; EC2 is regional.
//
// Connection:
//
//	region: us-east-1           # EC2 region (Reserved Instances); default us-east-1
//	currency: USD               # the currency purchases must be priced in (default USD)
//	access_key_id / secret_access_key / session_token   # or the AWS_* env vars
//	endpoint_ce / endpoint_savingsplans / endpoint_ec2  # overrides (tests, VPC endpoints)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/awskit"
)

// nowFunc is the signing clock; a var so tests can pin it.
var nowFunc = time.Now

type costPlugin struct{}

var period = plugin.Schema{
	"start": {Type: "string", Required: true, Desc: "inclusive start date, YYYY-MM-DD"},
	"end":   {Type: "string", Required: true, Desc: "exclusive end date, YYYY-MM-DD"},
}

func with(base plugin.Schema, extra plugin.Schema) plugin.Schema {
	out := plugin.Schema{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (costPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "aws-cost",
		Desc: "AWS cost & commitments: Cost Explorer spend/forecast/utilization/coverage/recommendations, Savings Plans and EC2 Reserved Instances, and purchase verbs whose typed total amount is verified against the offering before buying. Hand-rolled SigV4 — no AWS SDK, no AWS CLI.",
		Connection: plugin.Schema{
			"region":                {Type: "string", Desc: "EC2 region for Reserved Instances (default us-east-1)"},
			"currency":              {Type: "string", Desc: "the currency purchases must be priced in (default USD)"},
			"access_key_id":         {Type: "string", Desc: "AWS access key id (or AWS_ACCESS_KEY_ID)"},
			"secret_access_key":     {Type: "string", Desc: "AWS secret access key (or AWS_SECRET_ACCESS_KEY)", Secret: true},
			"session_token":         {Type: "string", Desc: "session token for temporary/role credentials (or AWS_SESSION_TOKEN)", Secret: true},
			"endpoint_ce":           {Type: "string", Desc: "override the Cost Explorer endpoint (tests)"},
			"endpoint_savingsplans": {Type: "string", Desc: "override the Savings Plans endpoint (tests)"},
			"endpoint_ec2":          {Type: "string", Desc: "override the EC2 endpoint (tests)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "cost_and_usage", Desc: "spend for a period (Cost Explorer GetCostAndUsage)",
				Usage: "read spend for a period, optionally grouped by a dimension (SERVICE, LINKED_ACCOUNT, …)",
				Options: with(period, plugin.Schema{
					"granularity": {Type: "string", Enum: []string{"DAILY", "MONTHLY", "HOURLY"}, Desc: "default MONTHLY"},
					"metric":      {Type: "string", Desc: "default UnblendedCost"},
					"group_by":    {Type: "string", Desc: "a DIMENSION key to group by, e.g. SERVICE"},
				}),
				Outputs: plugin.Schema{"results": {Type: "list", Desc: "ResultsByTime"}, "total": {Type: "number", Desc: "sum of the metric over the period"}, "unit": {Type: "string"}},
			},
			{
				Name: "cost_forecast", Desc: "forecast spend for a future period (GetCostForecast)",
				Options: with(period, plugin.Schema{
					"granularity": {Type: "string", Enum: []string{"DAILY", "MONTHLY"}, Desc: "default MONTHLY"},
				}),
				Outputs: plugin.Schema{"total": {Type: "number"}, "unit": {Type: "string"}, "results": {Type: "list"}},
			},
			{Name: "reservation_utilization", Desc: "Reserved Instance utilization (GetReservationUtilization)", Options: period, Outputs: plugin.Schema{"total": {Type: "map"}, "results": {Type: "list"}}},
			{Name: "reservation_coverage", Desc: "Reserved Instance coverage (GetReservationCoverage)", Options: period, Outputs: plugin.Schema{"total": {Type: "map"}, "results": {Type: "list"}}},
			{Name: "savings_plans_utilization", Desc: "Savings Plans utilization (GetSavingsPlansUtilization)", Options: period, Outputs: plugin.Schema{"total": {Type: "map"}, "results": {Type: "list"}}},
			{Name: "savings_plans_coverage", Desc: "Savings Plans coverage (GetSavingsPlansCoverage)", Options: period, Outputs: plugin.Schema{"results": {Type: "list"}}},
			{
				Name: "savings_plans_recommendation", Desc: "Savings Plans purchase recommendation (GetSavingsPlansPurchaseRecommendation)",
				Options: plugin.Schema{
					"plan_type":      {Type: "string", Enum: []string{"COMPUTE_SP", "EC2_INSTANCE_SP", "SAGEMAKER_SP"}, Desc: "default COMPUTE_SP"},
					"term":           {Type: "string", Enum: []string{"ONE_YEAR", "THREE_YEARS"}, Desc: "default ONE_YEAR"},
					"payment_option": {Type: "string", Enum: []string{"NO_UPFRONT", "PARTIAL_UPFRONT", "ALL_UPFRONT"}, Desc: "default NO_UPFRONT"},
					"lookback":       {Type: "string", Enum: []string{"SEVEN_DAYS", "THIRTY_DAYS", "SIXTY_DAYS"}, Desc: "default THIRTY_DAYS"},
				},
				Outputs: plugin.Schema{"recommendation": {Type: "map"}},
			},
			{
				Name: "reservation_recommendation", Desc: "Reserved Instance purchase recommendation (GetReservationPurchaseRecommendation)",
				Options: plugin.Schema{
					"service":        {Type: "string", Desc: "default \"Amazon Elastic Compute Cloud - Compute\""},
					"term":           {Type: "string", Enum: []string{"ONE_YEAR", "THREE_YEARS"}, Desc: "default ONE_YEAR"},
					"payment_option": {Type: "string", Enum: []string{"NO_UPFRONT", "PARTIAL_UPFRONT", "ALL_UPFRONT"}, Desc: "default NO_UPFRONT"},
					"lookback":       {Type: "string", Enum: []string{"SEVEN_DAYS", "THIRTY_DAYS", "SIXTY_DAYS"}, Desc: "default THIRTY_DAYS"},
				},
				Outputs: plugin.Schema{"recommendations": {Type: "list"}},
			},
			{
				Name: "savings_plans", Desc: "the account's Savings Plans (DescribeSavingsPlans)",
				Options: plugin.Schema{"states": {Type: "list", Desc: "filter by state (active, queued, …)"}},
				Outputs: plugin.Schema{"savings_plans": {Type: "list"}},
			},
			{
				Name: "savings_plans_offerings", Desc: "look up Savings Plans offerings (DescribeSavingsPlansOfferings)",
				Usage: "find an offering id and its terms before proposing a purchase",
				Options: plugin.Schema{
					"offering_ids":   {Type: "list", Desc: "specific offering ids"},
					"plan_type":      {Type: "string", Desc: "Compute | EC2Instance | SageMaker"},
					"payment_option": {Type: "string", Desc: "All Upfront | Partial Upfront | No Upfront"},
					"duration_years": {Type: "integer", Desc: "1 or 3"},
				},
				Outputs: plugin.Schema{"offerings": {Type: "list"}},
			},
			{
				Name: "reserved_instances", Desc: "the account's EC2 Reserved Instances (DescribeReservedInstances)",
				Options: plugin.Schema{"state": {Type: "string", Desc: "active | retired | payment-pending | …"}},
				Outputs: plugin.Schema{"reserved_instances": {Type: "list"}},
			},
			{
				Name: "reserved_instance_offerings", Desc: "look up EC2 Reserved Instance offerings (DescribeReservedInstancesOfferings)",
				Options: plugin.Schema{
					"offering_ids":        {Type: "list", Desc: "specific offering ids"},
					"instance_type":       {Type: "string"},
					"product_description": {Type: "string", Desc: "e.g. Linux/UNIX"},
					"offering_class":      {Type: "string", Desc: "standard | convertible"},
					"offering_type":       {Type: "string", Desc: "No Upfront | Partial Upfront | All Upfront"},
				},
				Outputs: plugin.Schema{"offerings": {Type: "list"}},
			},
			{
				Name: "purchase_savings_plan", Desc: "BUY a Savings Plan (CreateSavingsPlan) — commits money",
				Usage: "purchase a Savings Plan offering; total_commitment_usd must equal the hourly commitment times the term's hours",
				Options: plugin.Schema{
					"offering_id":          {Type: "string", Required: true, Desc: "from savings_plans_offerings"},
					"commitment_per_hour":  {Type: "number", Required: true, Desc: "hourly commitment, in the connector's currency"},
					"total_commitment_usd": {Type: "number", Required: true, Desc: "the TOTAL commitment over the term (hourly × term hours) — what a spend cap governs; verified against the offering"},
					"upfront_payment":      {Type: "number", Desc: "upfront amount for a Partial Upfront offering"},
					"client_token":         {Type: "string", Desc: "idempotency token (default: random)"},
				},
				Outputs: plugin.Schema{"savings_plan_id": {Type: "string"}, "total_commitment": {Type: "number"}, "currency": {Type: "string"}},
			},
			{
				Name: "purchase_reserved_instances", Desc: "BUY EC2 Reserved Instances (PurchaseReservedInstancesOffering) — commits money",
				Usage: "purchase an EC2 Reserved Instance offering; total_cost_usd must equal count × (upfront + hourly charges × term hours)",
				Options: plugin.Schema{
					"offering_id":    {Type: "string", Required: true, Desc: "from reserved_instance_offerings"},
					"instance_count": {Type: "integer", Required: true, Desc: "how many instances"},
					"total_cost_usd": {Type: "number", Required: true, Desc: "the TOTAL cost over the term — what a spend cap governs; verified against the offering"},
					"dry_run":        {Type: "boolean", Desc: "ask EC2 to validate the purchase without making it"},
				},
				Outputs: plugin.Schema{"reserved_instances_id": {Type: "string"}, "total_cost": {Type: "number"}, "currency": {Type: "string"}, "dry_run": {Type: "boolean"}},
			},
		},
		// Endpoints are AWS service hosts (ce/savingsplans in us-east-1, ec2
		// regional); narrow per instance with network:. Spawns nothing.
		Capabilities: plugin.Capabilities{Spawns: false},
	}
}

func (costPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	c := req.Connection
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := dispatch(ctx, c, req.Verb, o)
	if err != nil {
		if pe, ok := err.(*plugin.Error); ok {
			return plugin.InvokeResult{}, pe
		}
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	return plugin.InvokeResult{Outputs: out}, nil
}

func invalid(format string, a ...any) error {
	return plugin.Errorf(plugin.CodeInvalidParams, fmt.Sprintf(format, a...))
}

func timePeriod(o map[string]any) (map[string]any, error) {
	s, e := str(o["start"]), str(o["end"])
	for _, d := range []string{s, e} {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			return nil, invalid("start and end must be YYYY-MM-DD dates (got %q)", d)
		}
	}
	if e <= s {
		return nil, invalid("end must be after start")
	}
	return map[string]any{"Start": s, "End": e}, nil
}

func dispatch(ctx context.Context, c map[string]any, verb string, o map[string]any) (map[string]any, error) {
	switch verb {
	case "cost_and_usage":
		tp, err := timePeriod(o)
		if err != nil {
			return nil, err
		}
		metric := strOr(o["metric"], "UnblendedCost")
		p := map[string]any{"TimePeriod": tp, "Granularity": strOr(o["granularity"], "MONTHLY"), "Metrics": []string{metric}}
		if g := str(o["group_by"]); g != "" {
			p["GroupBy"] = []map[string]any{{"Type": "DIMENSION", "Key": g}}
		}
		res, err := ce(ctx, c, "GetCostAndUsage", p)
		if err != nil {
			return nil, err
		}
		total, unit := sumMetric(res["ResultsByTime"], metric)
		return map[string]any{"results": res["ResultsByTime"], "total": total, "unit": unit}, nil
	case "cost_forecast":
		tp, err := timePeriod(o)
		if err != nil {
			return nil, err
		}
		res, err := ce(ctx, c, "GetCostForecast", map[string]any{"TimePeriod": tp, "Granularity": strOr(o["granularity"], "MONTHLY"), "Metric": "UNBLENDED_COST"})
		if err != nil {
			return nil, err
		}
		tot, _ := res["Total"].(map[string]any)
		amt, _ := strconv.ParseFloat(str(tot["Amount"]), 64)
		return map[string]any{"total": amt, "unit": str(tot["Unit"]), "results": res["ForecastResultsByTime"]}, nil
	case "reservation_utilization", "reservation_coverage", "savings_plans_utilization", "savings_plans_coverage":
		tp, err := timePeriod(o)
		if err != nil {
			return nil, err
		}
		op := map[string]string{
			"reservation_utilization":   "GetReservationUtilization",
			"reservation_coverage":      "GetReservationCoverage",
			"savings_plans_utilization": "GetSavingsPlansUtilization",
			"savings_plans_coverage":    "GetSavingsPlansCoverage",
		}[verb]
		res, err := ce(ctx, c, op, map[string]any{"TimePeriod": tp})
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for _, k := range []string{"Total", "UtilizationsByTime", "CoveragesByTime", "SavingsPlansUtilizationsByTime", "SavingsPlansCoverages"} {
			if v, ok := res[k]; ok {
				if k == "Total" {
					out["total"] = v
				} else {
					out["results"] = v
				}
			}
		}
		return out, nil
	case "savings_plans_recommendation":
		res, err := ce(ctx, c, "GetSavingsPlansPurchaseRecommendation", map[string]any{
			"SavingsPlansType":     strOr(o["plan_type"], "COMPUTE_SP"),
			"TermInYears":          strOr(o["term"], "ONE_YEAR"),
			"PaymentOption":        strOr(o["payment_option"], "NO_UPFRONT"),
			"LookbackPeriodInDays": strOr(o["lookback"], "THIRTY_DAYS"),
		})
		if err != nil {
			return nil, err
		}
		rec, _ := res["SavingsPlansPurchaseRecommendation"].(map[string]any)
		return map[string]any{"recommendation": rec}, nil
	case "reservation_recommendation":
		res, err := ce(ctx, c, "GetReservationPurchaseRecommendation", map[string]any{
			"Service":              strOr(o["service"], "Amazon Elastic Compute Cloud - Compute"),
			"TermInYears":          strOr(o["term"], "ONE_YEAR"),
			"PaymentOption":        strOr(o["payment_option"], "NO_UPFRONT"),
			"LookbackPeriodInDays": strOr(o["lookback"], "THIRTY_DAYS"),
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"recommendations": res["Recommendations"]}, nil
	case "savings_plans":
		p := map[string]any{}
		if st := strList(o["states"]); len(st) > 0 {
			p["states"] = st
		}
		res, err := sp(ctx, c, "/DescribeSavingsPlans", p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"savings_plans": res["savingsPlans"]}, nil
	case "savings_plans_offerings":
		p := map[string]any{}
		if ids := strList(o["offering_ids"]); len(ids) > 0 {
			p["offeringIds"] = ids
		}
		if v := str(o["plan_type"]); v != "" {
			p["planTypes"] = []string{v}
		}
		if v := str(o["payment_option"]); v != "" {
			p["paymentOptions"] = []string{v}
		}
		if y, ok := toInt(o["duration_years"]); ok {
			p["durations"] = []int64{int64(y) * 365 * 24 * 3600}
		}
		res, err := sp(ctx, c, "/DescribeSavingsPlansOfferings", p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"offerings": res["searchResults"]}, nil
	case "reserved_instances":
		q := url.Values{}
		if s := str(o["state"]); s != "" {
			q.Set("Filter.1.Name", "state")
			q.Set("Filter.1.Value.1", s)
		}
		var r describeRIResponse
		if err := ec2(ctx, c, "DescribeReservedInstances", q, &r); err != nil {
			return nil, err
		}
		out := make([]any, 0, len(r.Items))
		for _, it := range r.Items {
			out = append(out, map[string]any{
				"id": it.ID, "instance_type": it.InstanceType, "instance_count": it.InstanceCount,
				"state": it.State, "start": it.Start, "end": it.End, "duration_seconds": it.Duration,
				"fixed_price": it.FixedPrice, "usage_price": it.UsagePrice, "currency": it.Currency,
				"offering_type": it.OfferingType, "offering_class": it.OfferingClass, "product": it.Product,
			})
		}
		return map[string]any{"reserved_instances": out}, nil
	case "reserved_instance_offerings":
		q := url.Values{}
		for i, id := range strList(o["offering_ids"]) {
			q.Set(fmt.Sprintf("ReservedInstancesOfferingId.%d", i+1), id)
		}
		if v := str(o["instance_type"]); v != "" {
			q.Set("InstanceType", v)
		}
		if v := str(o["product_description"]); v != "" {
			q.Set("ProductDescription", v)
		}
		if v := str(o["offering_class"]); v != "" {
			q.Set("OfferingClass", v)
		}
		if v := str(o["offering_type"]); v != "" {
			q.Set("OfferingType", v)
		}
		offs, err := riOfferings(ctx, c, q)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(offs))
		for _, of := range offs {
			out = append(out, map[string]any{
				"id": of.ID, "instance_type": of.InstanceType, "duration_seconds": of.Duration,
				"fixed_price": of.FixedPrice, "usage_price": of.UsagePrice, "hourly_recurring": of.hourlyRecurring(),
				"currency": of.Currency, "offering_type": of.OfferingType, "offering_class": of.OfferingClass,
				"product": of.Product, "total_cost_per_instance": round2(of.totalPerInstance()),
			})
		}
		return map[string]any{"offerings": out}, nil
	case "purchase_savings_plan":
		return purchaseSavingsPlan(ctx, c, o)
	case "purchase_reserved_instances":
		return purchaseReservedInstances(ctx, c, o)
	}
	return nil, invalid("aws-cost: unknown verb %q", verb)
}

// ---- purchases: the stated total must be the real total ----

func currency(c map[string]any) string { return strings.ToUpper(strOr(c["currency"], "USD")) }

func purchaseSavingsPlan(ctx context.Context, c map[string]any, o map[string]any) (map[string]any, error) {
	id := str(o["offering_id"])
	if id == "" {
		return nil, invalid("purchase_savings_plan: offering_id is required")
	}
	hourly, ok := num(o["commitment_per_hour"])
	if !ok || hourly <= 0 {
		return nil, invalid("purchase_savings_plan: commitment_per_hour must be a positive number")
	}
	stated, ok := num(o["total_commitment_usd"])
	if !ok || stated <= 0 {
		return nil, invalid("purchase_savings_plan: total_commitment_usd must be a positive number")
	}
	res, err := sp(ctx, c, "/DescribeSavingsPlansOfferings", map[string]any{"offeringIds": []string{id}})
	if err != nil {
		return nil, fmt.Errorf("look up offering: %w", err)
	}
	list, _ := res["searchResults"].([]any)
	if len(list) != 1 {
		return nil, invalid("purchase_savings_plan: offering %s not found (or ambiguous)", id)
	}
	off, _ := list[0].(map[string]any)
	if cur := strings.ToUpper(str(off["currency"])); cur != currency(c) {
		return nil, invalid("purchase_savings_plan: offering %s is priced in %s, not %s", id, cur, currency(c))
	}
	secs, ok := num(off["durationSeconds"])
	if !ok || secs <= 0 {
		return nil, fmt.Errorf("offering %s has no duration", id)
	}
	total := round2(hourly * secs / 3600)
	if math.Abs(total-stated) > 0.005 {
		return nil, invalid("purchase_savings_plan: total_commitment_usd %.2f does not match the offering: %v/hour × %.0f hours = %.2f %s — refusing to buy an amount other than the one stated", stated, hourly, secs/3600, total, currency(c))
	}
	body := map[string]any{
		"savingsPlanOfferingId": id,
		"commitment":            strconv.FormatFloat(hourly, 'f', -1, 64),
		"clientToken":           strOr(o["client_token"], randomToken()),
	}
	if up, ok := num(o["upfront_payment"]); ok && up > 0 {
		if up > total {
			return nil, invalid("purchase_savings_plan: upfront_payment exceeds the total commitment")
		}
		body["upfrontPaymentAmount"] = strconv.FormatFloat(up, 'f', -1, 64)
	}
	out, err := sp(ctx, c, "/CreateSavingsPlan", body)
	if err != nil {
		return nil, err
	}
	return map[string]any{"savings_plan_id": str(out["savingsPlanId"]), "total_commitment": total, "currency": currency(c)}, nil
}

func purchaseReservedInstances(ctx context.Context, c map[string]any, o map[string]any) (map[string]any, error) {
	id := str(o["offering_id"])
	if id == "" {
		return nil, invalid("purchase_reserved_instances: offering_id is required")
	}
	count, ok := toInt(o["instance_count"])
	if !ok || count <= 0 {
		return nil, invalid("purchase_reserved_instances: instance_count must be a positive integer")
	}
	stated, ok := num(o["total_cost_usd"])
	if !ok || stated <= 0 {
		return nil, invalid("purchase_reserved_instances: total_cost_usd must be a positive number")
	}
	q := url.Values{}
	q.Set("ReservedInstancesOfferingId.1", id)
	offs, err := riOfferings(ctx, c, q)
	if err != nil {
		return nil, fmt.Errorf("look up offering: %w", err)
	}
	if len(offs) != 1 {
		return nil, invalid("purchase_reserved_instances: offering %s not found (or ambiguous)", id)
	}
	of := offs[0]
	if cur := strings.ToUpper(of.Currency); cur != currency(c) {
		return nil, invalid("purchase_reserved_instances: offering %s is priced in %s, not %s", id, cur, currency(c))
	}
	total := round2(float64(count) * of.totalPerInstance())
	if math.Abs(total-stated) > 0.005 {
		return nil, invalid("purchase_reserved_instances: total_cost_usd %.2f does not match the offering: %d × (%.2f upfront + %v/hour × %.0f hours) = %.2f %s — refusing to buy an amount other than the one stated", stated, count, of.FixedPrice, of.UsagePrice+of.hourlyRecurring(), float64(of.Duration)/3600, total, currency(c))
	}
	pq := url.Values{}
	pq.Set("ReservedInstancesOfferingId", id)
	pq.Set("InstanceCount", strconv.Itoa(count))
	// LimitPrice bounds the UPFRONT charge EC2 may take: never more than the
	// offering's upfront price times the count that was verified above.
	pq.Set("LimitPrice.Amount", strconv.FormatFloat(round2(of.FixedPrice*float64(count)), 'f', 2, 64))
	pq.Set("LimitPrice.CurrencyCode", currency(c))
	dry := boolv(o["dry_run"])
	if dry {
		pq.Set("DryRun", "true")
	}
	var r purchaseRIResponse
	if err := ec2(ctx, c, "PurchaseReservedInstancesOffering", pq, &r); err != nil {
		if dry && strings.Contains(err.Error(), "DryRunOperation") {
			return map[string]any{"reserved_instances_id": "", "total_cost": total, "currency": currency(c), "dry_run": true}, nil
		}
		return nil, err
	}
	return map[string]any{"reserved_instances_id": r.ID, "total_cost": total, "currency": currency(c), "dry_run": dry}, nil
}

// ---- transports ----

func signedDo(ctx context.Context, c map[string]any, endpoint, service, region, path string, body []byte, headers map[string]string) ([]byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad endpoint %q", endpoint)
	}
	signed := awskit.Sign(awskit.SignInput{
		Method: http.MethodPost, Host: u.Host, Path: path, Body: body, Headers: headers,
		Service: service, Region: region, Creds: awskit.Load(c), Time: nowFunc(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range signed {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: %s: %s", service, path, resp.Status, awsError(b))
	}
	return b, nil
}

func decodeJSON(b []byte) (map[string]any, error) {
	var out map[string]any
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

// ce calls Cost Explorer (JSON 1.1, global, signed in us-east-1).
func ce(ctx context.Context, c map[string]any, op string, payload map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(payload)
	b, err := signedDo(ctx, c, strOr(c["endpoint_ce"], "https://ce.us-east-1.amazonaws.com"), "ce", "us-east-1", "/", body,
		map[string]string{"content-type": "application/x-amz-json-1.1", "x-amz-target": "AWSInsightsIndexService." + op})
	if err != nil {
		return nil, err
	}
	return decodeJSON(b)
}

// sp calls Savings Plans (REST-JSON, global, signed in us-east-1).
func sp(ctx context.Context, c map[string]any, path string, payload map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(payload)
	b, err := signedDo(ctx, c, strOr(c["endpoint_savingsplans"], "https://savingsplans.amazonaws.com"), "savingsplans", "us-east-1", path, body,
		map[string]string{"content-type": "application/json"})
	if err != nil {
		return nil, err
	}
	return decodeJSON(b)
}

// ec2 calls the EC2 Query API (form-encoded, XML) in the connection's region.
func ec2(ctx context.Context, c map[string]any, action string, q url.Values, into any) error {
	region := strOr(c["region"], "us-east-1")
	q.Set("Action", action)
	q.Set("Version", "2016-11-15")
	body := []byte(q.Encode())
	b, err := signedDo(ctx, c, strOr(c["endpoint_ec2"], "https://ec2."+region+".amazonaws.com"), "ec2", region, "/", body,
		map[string]string{"content-type": "application/x-www-form-urlencoded; charset=utf-8"})
	if err != nil {
		return err
	}
	if err := xml.Unmarshal(b, into); err != nil {
		return fmt.Errorf("ec2 %s: decode response: %w", action, err)
	}
	return nil
}

func awsError(b []byte) string {
	var j struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
		Msg2    string `json:"Message"`
	}
	if json.Unmarshal(b, &j) == nil && (j.Type != "" || j.Message != "" || j.Msg2 != "") {
		return strings.TrimSpace(j.Type + " " + j.Message + j.Msg2)
	}
	var x struct {
		Errors []struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Errors>Error"`
	}
	if xml.Unmarshal(b, &x) == nil && len(x.Errors) > 0 {
		return x.Errors[0].Code + " " + x.Errors[0].Message
	}
	s := string(b)
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}

// ---- EC2 XML shapes ----

type recurring struct {
	Frequency string  `xml:"frequency"`
	Amount    float64 `xml:"amount"`
}

type riOffering struct {
	ID            string      `xml:"reservedInstancesOfferingId"`
	InstanceType  string      `xml:"instanceType"`
	Duration      int64       `xml:"duration"`
	FixedPrice    float64     `xml:"fixedPrice"`
	UsagePrice    float64     `xml:"usagePrice"`
	Currency      string      `xml:"currencyCode"`
	OfferingType  string      `xml:"offeringType"`
	OfferingClass string      `xml:"offeringClass"`
	Product       string      `xml:"productDescription"`
	Recurring     []recurring `xml:"recurringCharges>item"`
}

// hourlyRecurring sums the offering's hourly recurring charges.
func (o riOffering) hourlyRecurring() float64 {
	var t float64
	for _, r := range o.Recurring {
		if strings.EqualFold(r.Frequency, "Hourly") {
			t += r.Amount
		}
	}
	return t
}

// totalPerInstance is one instance's total cost over the term: upfront plus
// hourly (usage + recurring) charges for every hour of the term.
func (o riOffering) totalPerInstance() float64 {
	return o.FixedPrice + (o.UsagePrice+o.hourlyRecurring())*float64(o.Duration)/3600
}

type describeRIOfferingsResponse struct {
	Items []riOffering `xml:"reservedInstancesOfferingsSet>item"`
}

func riOfferings(ctx context.Context, c map[string]any, q url.Values) ([]riOffering, error) {
	var r describeRIOfferingsResponse
	if err := ec2(ctx, c, "DescribeReservedInstancesOfferings", q, &r); err != nil {
		return nil, err
	}
	return r.Items, nil
}

type describeRIResponse struct {
	Items []struct {
		ID            string  `xml:"reservedInstancesId"`
		InstanceType  string  `xml:"instanceType"`
		InstanceCount int     `xml:"instanceCount"`
		State         string  `xml:"state"`
		Start         string  `xml:"start"`
		End           string  `xml:"end"`
		Duration      int64   `xml:"duration"`
		FixedPrice    float64 `xml:"fixedPrice"`
		UsagePrice    float64 `xml:"usagePrice"`
		Currency      string  `xml:"currencyCode"`
		OfferingType  string  `xml:"offeringType"`
		OfferingClass string  `xml:"offeringClass"`
		Product       string  `xml:"productDescription"`
	} `xml:"reservedInstancesSet>item"`
}

type purchaseRIResponse struct {
	ID string `xml:"reservedInstancesId"`
}

// ---- helpers ----

func sumMetric(results any, metric string) (float64, string) {
	list, _ := results.([]any)
	var total float64
	unit := ""
	for _, r := range list {
		rm, _ := r.(map[string]any)
		add := func(m map[string]any) {
			mt, _ := m[metric].(map[string]any)
			if a, err := strconv.ParseFloat(str(mt["Amount"]), 64); err == nil {
				total += a
				unit = str(mt["Unit"])
			}
		}
		if tot, ok := rm["Total"].(map[string]any); ok && len(tot) > 0 {
			add(tot)
			continue
		}
		groups, _ := rm["Groups"].([]any)
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			if m, ok := gm["Metrics"].(map[string]any); ok {
				add(m)
			}
		}
	}
	return round2(total), unit
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func randomToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		// A purchase amount is a NUMBER; a string is refused rather than parsed,
		// so the spend cap and the plugin can never read the value differently.
		return 0, false
	}
	return 0, false
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		if x == math.Trunc(x) {
			return int(x), true
		}
	case json.Number:
		n, err := x.Int64()
		return int(n), err == nil
	}
	return 0, false
}

func boolv(v any) bool { b, _ := v.(bool); return b }

func strList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if x != "" {
			return []string{x}
		}
	}
	return nil
}

func main() {
	if err := plugin.Serve(costPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-aws-cost: %v\n", err)
		os.Exit(1)
	}
}
