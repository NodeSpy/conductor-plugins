# `aws-cost` connector

AWS cost and commitments as verbs: Cost Explorer reads (spend, forecast,
Reserved Instance / Savings Plans utilization and coverage, purchase
recommendations), the account's Savings Plans and EC2 Reserved Instances with
their offerings — and two **purchase** verbs, a Savings Plan and an EC2
Reserved Instance offering.

**No AWS SDK, no AWS CLI.** Plain HTTPS signed with hand-rolled SigV4
([`internal/awskit`](../../internal/awskit/sigv4.go)). Cost Explorer and Savings
Plans are global services signed in `us-east-1`; EC2 is regional.

- **Kind:** connector (verbs)
- **Source:** [`connectors/aws-cost/main.go`](../../connectors/aws-cost/main.go)
- **Provides:** `aws-cost`
- **Capabilities:** no commands, spawns nothing; no fixed egress (the EC2
  endpoint is region-derived) — narrow it with `network:`.

```yaml
connectors:
  aws:
    use: aws-cost
    region: us-east-1          # where EC2 Reserved Instances live
    currency: USD              # purchases must be priced in this
    network: ["ce.us-east-1.amazonaws.com:443", "savingsplans.amazonaws.com:443", "ec2.us-east-1.amazonaws.com:443"]
```

## Purchases carry a verified, typed amount

`purchase_savings_plan` and `purchase_reserved_instances` each take a
**required number** for the total they commit:

| verb | amount option | must equal |
|---|---|---|
| `purchase_savings_plan` | `total_commitment_usd` | `commitment_per_hour × hours in the offering's term` |
| `purchase_reserved_instances` | `total_cost_usd` | `instance_count × (upfront + (usage + hourly recurring) × term hours)` |

That number is what a spend cap governs — conductor's member gate
(`autonomy.amounts: { aws.purchase_savings_plan: total_commitment_usd }` under
an `autonomy.spend_cap`) holds the purchase as a `commit_money` proposal and
refuses an amount over the cap. The plugin does not trust the number: before
buying it looks the offering up, derives the real total from the offering's own
terms, and **refuses unless the stated amount equals it to the cent** and the
offering is priced in the connector's `currency`. A string amount is refused
rather than parsed. So the amount an operator approved, under the cap, is the
amount spent. A Reserved Instance purchase additionally sets EC2's
`LimitPrice` to the verified upfront total; `dry_run: true` asks EC2 to
validate without buying.

## Setup

Credentials for an IAM principal with what you use: `ce:Get*` for the Cost
Explorer reads, `savingsplans:Describe*` (+ `savingsplans:CreateSavingsPlan`
to buy), `ec2:DescribeReservedInstances*` (+
`ec2:PurchaseReservedInstancesOffering` to buy). Give a read-only member a
principal without the purchase permissions. Credentials come from
`access_key_id` / `secret_access_key` (+ `session_token`) or the standard
`AWS_*` environment variables.

## Connection

| key | type | purpose |
|-----|------|---------|
| `region` | string | EC2 region for Reserved Instances (default `us-east-1`) |
| `currency` | string | the currency purchases must be priced in (default `USD`) |
| `access_key_id` / `secret_access_key` / `session_token` | string | credentials (or the `AWS_*` env vars) |
| `endpoint_ce` / `endpoint_savingsplans` / `endpoint_ec2` | string | endpoint overrides (tests, VPC endpoints) |

## Verbs

| verb | options | outputs |
|---|---|---|
| `cost_and_usage` | `start`, `end` (YYYY-MM-DD), `granularity?`, `metric?` (UnblendedCost), `group_by?` (a DIMENSION, e.g. SERVICE) | `results`, `total`, `unit` |
| `cost_forecast` | `start`, `end`, `granularity?` | `total`, `unit`, `results` |
| `reservation_utilization` / `reservation_coverage` | `start`, `end` | `total`, `results` |
| `savings_plans_utilization` / `savings_plans_coverage` | `start`, `end` | `total?`, `results` |
| `savings_plans_recommendation` | `plan_type?` (COMPUTE_SP), `term?` (ONE_YEAR), `payment_option?` (NO_UPFRONT), `lookback?` (THIRTY_DAYS) | `recommendation` |
| `reservation_recommendation` | `service?`, `term?`, `payment_option?`, `lookback?` | `recommendations` |
| `savings_plans` | `states?` | `savings_plans` |
| `savings_plans_offerings` | `offering_ids?`, `plan_type?`, `payment_option?`, `duration_years?` | `offerings` |
| `reserved_instances` | `state?` | `reserved_instances` |
| `reserved_instance_offerings` | `offering_ids?`, `instance_type?`, `product_description?`, `offering_class?`, `offering_type?` | `offerings` (each with `total_cost_per_instance`) |
| `purchase_savings_plan` | `offering_id`, `commitment_per_hour` (number), `total_commitment_usd` (number), `upfront_payment?`, `client_token?` | `savings_plan_id`, `total_commitment`, `currency` |
| `purchase_reserved_instances` | `offering_id`, `instance_count`, `total_cost_usd` (number), `dry_run?` | `reserved_instances_id`, `total_cost`, `currency`, `dry_run` |

## Using it from a member

```yaml
members:
  sre-acme:
    skill:
      verbs: [aws.cost_*, aws.reservation_*, aws.savings_plans*, aws.reserved_*, aws.purchase_*]
    autonomy:
      tiers:
        observe: [aws.cost_*, aws.reservation_*, aws.savings_plans*, aws.reserved_*]
        commit_money: [aws.purchase_*]
      amounts:
        aws.purchase_savings_plan: total_commitment_usd
        aws.purchase_reserved_instances: total_cost_usd
      spend_cap: { per_action: 2000, per_period: 5000, period: 720h, currency: USD }
```
