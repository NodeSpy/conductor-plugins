# GitHub App Setup

The recommended way to run the [`github`](github.md) connector is a **GitHub
App**: the App carries the webhook subscription and all of conductor's own API
reads (its own rate pool), while writes are attributed to you. An App is not
*required* — see [Running without an App](#running-without-an-app) — but it is
the full-featured path. This page covers registering the App (permissions,
events, the private key, the webhook secret) and the two ways a webhook delivery
reaches conductor: a smee.io relay or a direct HTTP listener.

This is the config for the **plugin** connector (`use: github`). For the
conductor-side model of what each event becomes, see the wiki's
[Integration-GitHub](https://github.com/NodeSpy/conductor/wiki/Integration-GitHub).

## Register the App

Create a new GitHub App at:

- Personal account: <https://github.com/settings/apps/new>
- Organization: `https://github.com/organizations/<your-org>/settings/apps/new`

([GitHub's own guide](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).)

| Field | Suggested value |
| --- | --- |
| GitHub App name | Must be globally unique — personalize it, e.g. `conductor-<your-handle>` or `<your-org>-conductor`. This name becomes the bot login used in identity matching. |
| Homepage URL | Anything valid — the conductor repo or `https://paseo.sh`. |
| Webhook URL | A smee.io channel URL (see [Transports](#transports-smee-andor-direct-http)) or your own listener's public address. |
| Webhook secret | A random string, e.g. `openssl rand -hex 32`. Store it as `GH_WEBHOOK_SECRET` in `conductor.env`. |

Set **permissions before events** — GitHub only lists webhook events for
permissions you've already granted.

## Permissions

| Scope | Permission | Access |
| --- | --- | --- |
| Repository | Contents | Read & write |
| Repository | Pull requests | Read & write |
| Repository | Issues | Read & write |
| Repository | Checks | Read-only |
| Repository | Metadata | Read-only |

Grant only what your workflows actually use — the connector's verbs need
Contents / Pull requests / Issues to write, and Metadata to read.

## Webhook events

Subscribe to the events your triggers use — for the full source surface:

```
pull_request
pull_request_review
pull_request_review_comment
issue_comment
check_run
check_suite
workflow_run
push
issues
release
deployment_status
```

(The connector derives its events — `review_requested`, `changes_requested`,
`new_comment`, `release`, `deployment_status`, `dependabot_alert`,
`secret_scanning_alert` — from these deliveries; see the [source events
table](github.md#source-events).)

## Credentials

After granting permissions and events:

1. **Generate a private key** (App settings → "Generate a private key") and save
   the downloaded `.pem` at the path you'll put in `app.private_key_path`.
2. **Install the App** on the repos/orgs you want conductor to act on.
3. Note the **App ID** (shown at the top of the App's settings page).
4. Put the App id, key path, and webhook secret into the connector:

```yaml
connectors:
  gh:
    use: github
    app:
      app_id: 123456                                        # the App's numeric id
      private_key_path: ~/.config/conductor/github-app.pem  # the generated .pem
      webhook_secret: ${GH_WEBHOOK_SECRET}                  # HMAC secret from conductor.env
    webhook:
      smee: ${GH_SMEE_URL}          # https://smee.io/<channel> — and/or a direct listener:
      # listen: ":9099"
      # path: /webhook
    identity:
      write_token: ${GH_WRITE_TOKEN}   # your PAT for attributed writes; "gh_auth" shells out to `gh auth token`
```

| Field | Meaning |
| --- | --- |
| `app.app_id` | The App's numeric id. |
| `app.private_key_path` | Path to the App's generated `.pem`, used to mint installation tokens. |
| `app.webhook_secret` | The secret configured on the App's webhook — verifies each delivery's `X-Hub-Signature-256` HMAC. (May instead be set as `webhook.secret`.) |
| `webhook.smee` | A smee.io channel URL — conductor subscribes to it itself; no inbound port needed. |
| `webhook.listen` | A direct HTTP listen address (e.g. `:9099`) for a plain webhook receiver. Optional if `smee` is set. |
| `webhook.path` | HTTP path for the direct listener. |
| `identity.write_token` | The credential writes are attributed to — a literal PAT, or `gh_auth` to shell out to `gh auth token`. |

An **installation id** is implicit — conductor resolves it from the App's
installations at startup; the App only needs to be installed on the target
repos/orgs.

## Transports: smee and/or direct HTTP

Set `webhook.smee`, `webhook.listen`, or both. Either way the delivery's HMAC is
checked against the webhook secret — set `allow_unsigned: true` only if you
genuinely front the listener with something else that authenticates it.

### smee.io (no inbound port)

Open <https://smee.io/new>, copy the channel URL it shows (e.g.
`https://smee.io/AbC123`), and use that **same URL** in two places: the App's
Webhook URL, and `webhook.smee` (via `GH_SMEE_URL` in `conductor.env`).

You do **not** install or run the `smee` client. smee.io is a public relay;
conductor subscribes to your channel itself (auto-reconnecting) and receives the
forwarded deliveries — there is nothing else to start. smee.io does not buffer,
so a delivery sent while conductor is disconnected is lost.

### Direct HTTP

`webhook.listen: ":9099"` (with optional `webhook.path`) runs a plain webhook
receiver with no relay in between. Point the App's Webhook URL at it — typically
via your own tunnel if the box has no public address of its own.

## Running without an App

An App is not required. The connector's credentials resolve `app:` → `token:` (a
PAT) → the `gh` CLI's stored login. App-less:

- Events arrive via a **plain repository/organization webhook** pointed at
  `webhook.listen` (set the same secret in the webhook and in
  `app.webhook_secret` / `webhook.secret`), with no `app:` block.
- Reads use the PAT / `gh` token; writes are you, as always.
- Verb calls that need App credentials (e.g. acting as the bot identity) fail
  with a clear error without them.

```yaml
connectors:
  gh:
    use: github
    token: ${GITHUB_TOKEN}          # PAT; or omit to use `gh auth token`
    webhook:
      listen: ":9099"
      secret: ${GH_WEBHOOK_SECRET}
```

The App still buys a separate read-rate pool, webhook management on install, and
a bot identity — but a personal setup runs with none of it.
