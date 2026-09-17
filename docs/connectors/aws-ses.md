# `aws-ses` connector

Amazon SES v2 (Simple Email Service — email API) as a connector: send simple
and templated email, manage email identities, read the account send quota,
and manage the account-level suppression list, plus a raw `api` escape
hatch. Every request is authenticated with a hand-rolled **AWS Signature
Version 4 (SigV4)** implementation, built only on the standard library
(`crypto/hmac`, `crypto/sha256`, `net/http`) — no AWS SDK, no AWS CLI.

- **Kind:** connector (verbs only — no source)
- **Source:** [`connectors/aws-ses/main.go`](../../connectors/aws-ses/main.go)
- **Provides:** `aws-ses`
- **Capabilities:** egress `["email.*.amazonaws.com:443"]`

```yaml
connectors:
  ses:
    use: aws-ses
    access_key_id: ${AWS_ACCESS_KEY_ID}
    secret_access_key: ${AWS_SECRET_ACCESS_KEY}
    region: us-east-1

steps:
  - uses: ses.send_email
    with:
      from: [email protected]
      to: ["[email protected]"]
      subject: "Order shipped"
      text: "Your order is on its way."
```

## Setup

Credentials are a plain IAM access key pair — no OAuth, no console app registration.

**Prerequisites:** an AWS account, and an IAM identity (user or role) with
permission to create access keys.

1. Sign in to the [AWS Console](https://console.aws.amazon.com/) → **IAM** →
   **Users** → **Create user** (or pick an existing user).
2. Attach a policy granting at least `ses:SendEmail` and
   `ses:SendRawEmail` (add `ses:GetEmailIdentity`,
   `ses:CreateEmailIdentity`, or the suppression-list actions if the workflow
   uses those verbs too).
3. Open the user → **Security credentials** tab → **Access keys** → **Create
   access key** → choose **Third-party service** → **Create access key**.
   Copy the access key ID and secret access key now — the secret is shown
   only once.
4. In the SES console (same region as `region` below), go to **Identities**
   → **Create identity**, verify a sender email address or domain, and open
   the verification link SES emails you. New accounts start in the **SES
   sandbox**: you can only send to other verified identities until you
   request production access (SES console → **Account dashboard** → **Request
   production access**).

Configure:

```yaml
connectors:
  ses:
    use: aws-ses
    access_key_id: ${AWS_ACCESS_KEY_ID}
    secret_access_key: ${AWS_SECRET_ACCESS_KEY}
    region: us-east-1
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `access_key_id` | string | AWS access key ID (required) |
| `secret_access_key` | string | AWS secret access key (required) |
| `region` | string | AWS region, e.g. `us-east-1` (required) |
| `session_token` | string | AWS STS session token, for temporary credentials (optional) |
| `endpoint` | string | override the default SES v2 endpoint `https://email.{region}.amazonaws.com` (optional; mainly for tests) |

A non-2xx response is returned as an error carrying the status code and the
AWS error body — nothing is swallowed.

## AWS SigV4 signing

Every request is signed for service `ses` in the connection's `region`, over
the following fixed header set: `Host`, `X-Amz-Date`,
`X-Amz-Content-Sha256`, and `X-Amz-Security-Token` (only when
`session_token` is set). The signing pipeline is the standard four steps —
canonical request, string-to-sign, an HMAC-SHA256 key-derivation chain
(`kDate -> kRegion -> kService -> kSigning`), and the final signature — each
implemented as a small, independently testable pure function (see
`canonicalRequestString`, `stringToSign`, `deriveSigningKey`, `signV4` in
`main.go`). The signing timestamp comes from an unexported `nowFunc` var
(default `time.Now`), so tests can inject a fixed time and get a
deterministic `Authorization` header.

The implementation is unit-tested directly against the AWS-documented
`get-vanilla` SigV4 test vector (the standard fixed test credentials
`AKIDEXAMPLE` / `wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY`, region
`us-east-1`, a generic `service`, and a fixed timestamp) — the canonical
request, string-to-sign, and final signature are asserted against the
vector's own published values, not merely checked for self-consistency.

## Verbs

Selected by `uses: <name>.<verb>`. Conventions shared across verbs:

- Every verb's outputs include `status_code`; most also return either
  `result` (a single object) or `items` (a list).
- A non-2xx response is returned as an error carrying the status code and
  AWS error body — nothing is swallowed.

Required options are marked `*`.

### Sending

- **`send_email`** — send a simple (non-template) email. `POST /v2/email/outbound-emails`. `from`* (`FromEmailAddress`), `to`* (list, `Destination.ToAddresses`), `cc` (list, `Destination.CcAddresses`), `bcc` (list, `Destination.BccAddresses`), `subject`* (`Content.Simple.Subject.Data`), `text` (`Content.Simple.Body.Text.Data`; text or html required), `html` (`Content.Simple.Body.Html.Data`; text or html required), `reply_to` (list, `ReplyToAddresses`). → `result`, `status_code`.
- **`send_templated_email`** — send an email rendered from an SES template. `POST /v2/email/outbound-emails`. `from`* (`FromEmailAddress`), `to`* (list, `Destination.ToAddresses`), `cc` (list, `Destination.CcAddresses`), `bcc` (list, `Destination.BccAddresses`), `template_name`* (`Content.Template.TemplateName`), `template_data` (`Content.Template.TemplateData`: a JSON object, encoded to a JSON string, or a JSON string used as-is), `reply_to` (list, `ReplyToAddresses`). → `result`, `status_code`.

### Identities

- **`identities`** — list email identities. `GET /v2/email/identities`. No options. → `items` (hoisted from `EmailIdentities`), `status_code`.
- **`identity_get`** — get one email identity's details. `GET /v2/email/identities/{email_identity}`. `email_identity`*. → `result`, `status_code`.
- **`create_identity`** — create (start verifying) an email identity. `POST /v2/email/identities`. `email_identity`*. → `result`, `status_code`.

### Account & suppression list

- **`get_send_quota`** — the account's sending quota and stats. `GET /v2/email/account`. No options. → `result`, `status_code`.
- **`suppressed_list`** — list the account-level suppressed destinations. `GET /v2/email/suppression/addresses`. No options. → `items` (hoisted from `SuppressedDestinationSummaries`), `status_code`.
- **`suppress`** — add an address to the account-level suppression list. `PUT /v2/email/suppression/addresses/{email}`. `email`*, `reason`* (`BOUNCE` | `COMPLAINT`). → `result`, `status_code`.
- **`unsuppress`** — remove an address from the account-level suppression list. `DELETE /v2/email/suppression/addresses/{email}`. `email`*. → `status_code`.

### Escape hatch

- **`api`** — raw escape hatch: any SES v2 endpoint. `method` (HTTP method, default `GET`), `path`* (path under the SES v2 endpoint, e.g. `/v2/email/identities`), `query` (map of query string parameters), `body` (any, JSON request body). → `result` (object response) or `items` (array response), `status_code`.

## Capabilities & security

Declares egress to `email.*.amazonaws.com:443` — the SES v2 endpoint for
every AWS region. Scope the supplied credentials to the least privilege the
workflow needs (e.g. `ses:SendEmail`, `ses:GetEmailIdentity`, and only the
suppression-list actions actually used).
