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

Selected by `uses: <name>.<verb>`. See `Describe()` for each verb's full
option schema. Every verb's outputs include `status_code`; most also return
either `result` (a single object) or `items` (a list).

| verb | endpoint | outputs |
|------|----------|---------|
| `send_email` | `POST /v2/email/outbound-emails` (`from`\*, `to`\*, `cc`, `bcc`, `subject`\*, `text`, `html` — one of `text`/`html` required, `reply_to`) | `result` |
| `send_templated_email` | `POST /v2/email/outbound-emails` (`from`\*, `to`\*, `cc`, `bcc`, `template_name`\*, `template_data`, `reply_to`) | `result` |
| `identities` | `GET /v2/email/identities` | `items` (hoisted from `EmailIdentities`) |
| `identity_get` | `GET /v2/email/identities/{email_identity}` | `result` |
| `create_identity` | `POST /v2/email/identities` (`email_identity`\*) | `result` |
| `get_send_quota` | `GET /v2/email/account` | `result` |
| `suppressed_list` | `GET /v2/email/suppression/addresses` | `items` (hoisted from `SuppressedDestinationSummaries`) |
| `suppress` | `PUT /v2/email/suppression/addresses/{email}` (`email`\*, `reason`\* — `BOUNCE` or `COMPLAINT`) | `status_code` (+ `result` if the body is non-empty) |
| `unsuppress` | `DELETE /v2/email/suppression/addresses/{email}` (`email`\*) | `status_code` (+ `result` if the body is non-empty) |
| `api` | `method` + `path` (under the SES v2 endpoint) + `query` + `body` — escape hatch for anything without a first-class verb | `result` (object response) or `items` (array response) |

`template_data` (for `send_templated_email`) accepts either a JSON object
(encoded to the JSON string SES v2's `TemplateData` field requires) or a
string that is already JSON, used as-is.

## Capabilities & security

Declares egress to `email.*.amazonaws.com:443` — the SES v2 endpoint for
every AWS region. Scope the supplied credentials to the least privilege the
workflow needs (e.g. `ses:SendEmail`, `ses:GetEmailIdentity`, and only the
suppression-list actions actually used).
