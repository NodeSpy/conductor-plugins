# conductor-plugins

Distributable **plugins** for [conductor](https://github.com/NodeSpy/conductor) —
external connectors, runtimes and step engines the daemon fetches at
`conductor init` instead of bundling into the core binary. Each plugin is an
out-of-process binary that speaks conductor's plugin protocol over stdio; the
daemon verifies, sandboxes, and runs it.

Plugin **source** lives in this repo, under `<kind>/<name>/` — `connectors/`,
`runtimes/` or `engines/`. That layout is load-bearing: conductor's `use:`
resolver turns a bare name in a config block into `<kind>/<name>` in THIS repo,
so a bare `use: sentry` under `connectors:` resolves to `connectors/sentry` and
nothing else, and `use: js` under `engines:` to `engines/js`. A connector, a
runtime and an engine may share a name without colliding. Each plugin is built
only against conductor's public SDK — `pkg/plugin`, plus `pkg/sourcekit` for
webhook sources and `pkg/githubkit` for the github client — and imports **no**
conductor internals. That is enforced, not asserted:

```console
$ go list -deps ./... | grep NodeSpy/conductor/internal   # must print nothing
$ go list -deps ./... | grep NodeSpy
github.com/NodeSpy/conductor/pkg/githubkit
github.com/NodeSpy/conductor/pkg/plugin
github.com/NodeSpy/conductor/pkg/sourcekit
github.com/NodeSpy/conductor-plugins/connectors/adguard
github.com/NodeSpy/conductor-plugins/connectors/alertmanager
github.com/NodeSpy/conductor-plugins/connectors/audiobookshelf
github.com/NodeSpy/conductor-plugins/connectors/aws-cli
github.com/NodeSpy/conductor-plugins/connectors/aws-ses
github.com/NodeSpy/conductor-plugins/connectors/aws-sns
github.com/NodeSpy/conductor-plugins/connectors/cloudflare
github.com/NodeSpy/conductor-plugins/connectors/datadog
github.com/NodeSpy/conductor-plugins/connectors/docker
github.com/NodeSpy/conductor-plugins/connectors/email
github.com/NodeSpy/conductor-plugins/connectors/ffmpeg
github.com/NodeSpy/conductor-plugins/connectors/git
github.com/NodeSpy/conductor-plugins/connectors/gitea
github.com/NodeSpy/conductor-plugins/connectors/github
github.com/NodeSpy/conductor-plugins/connectors/gitlab
github.com/NodeSpy/conductor-plugins/connectors/gmail
github.com/NodeSpy/conductor-plugins/connectors/google-calendar
github.com/NodeSpy/conductor-plugins/connectors/google-drive
github.com/NodeSpy/conductor-plugins/connectors/google-sheets
github.com/NodeSpy/conductor-plugins/connectors/grafana
github.com/NodeSpy/conductor-plugins/connectors/healthchecks
github.com/NodeSpy/conductor-plugins/connectors/helm
github.com/NodeSpy/conductor-plugins/connectors/homeassistant
github.com/NodeSpy/conductor-plugins/connectors/ifttt
github.com/NodeSpy/conductor-plugins/connectors/jira
github.com/NodeSpy/conductor-plugins/connectors/kubernetes
github.com/NodeSpy/conductor-plugins/connectors/libation
github.com/NodeSpy/conductor-plugins/connectors/lidarr
github.com/NodeSpy/conductor-plugins/connectors/linear
github.com/NodeSpy/conductor-plugins/connectors/matrix
github.com/NodeSpy/conductor-plugins/connectors/notifiarr
github.com/NodeSpy/conductor-plugins/connectors/notion
github.com/NodeSpy/conductor-plugins/connectors/ntfy
github.com/NodeSpy/conductor-plugins/connectors/opnsense
github.com/NodeSpy/conductor-plugins/connectors/pagerduty
github.com/NodeSpy/conductor-plugins/connectors/pihole
github.com/NodeSpy/conductor-plugins/connectors/plex
github.com/NodeSpy/conductor-plugins/connectors/portainer
github.com/NodeSpy/conductor-plugins/connectors/prowlarr
github.com/NodeSpy/conductor-plugins/connectors/proxmox
github.com/NodeSpy/conductor-plugins/connectors/pushover
github.com/NodeSpy/conductor-plugins/connectors/qbittorrent
github.com/NodeSpy/conductor-plugins/connectors/radarr
github.com/NodeSpy/conductor-plugins/connectors/sabnzbd
github.com/NodeSpy/conductor-plugins/connectors/sentry
github.com/NodeSpy/conductor-plugins/connectors/smart
github.com/NodeSpy/conductor-plugins/connectors/sonarr
github.com/NodeSpy/conductor-plugins/connectors/tailscale
github.com/NodeSpy/conductor-plugins/connectors/tautulli
github.com/NodeSpy/conductor-plugins/connectors/telegram
github.com/NodeSpy/conductor-plugins/connectors/terraform
github.com/NodeSpy/conductor-plugins/connectors/terraspace
github.com/NodeSpy/conductor-plugins/connectors/truenas
github.com/NodeSpy/conductor-plugins/connectors/twilio
github.com/NodeSpy/conductor-plugins/connectors/unifi
github.com/NodeSpy/conductor-plugins/connectors/unifi-protect
github.com/NodeSpy/conductor-plugins/connectors/uptimekuma
github.com/NodeSpy/conductor-plugins/connectors/uptimerobot
github.com/NodeSpy/conductor-plugins/connectors/wiz
github.com/NodeSpy/conductor-plugins/connectors/xero
github.com/NodeSpy/conductor-plugins/connectors/zapier
github.com/NodeSpy/conductor-plugins/engines/go-embed
github.com/NodeSpy/conductor-plugins/engines/js
github.com/NodeSpy/conductor-plugins/engines/lua
github.com/NodeSpy/conductor-plugins/engines/risor
github.com/NodeSpy/conductor-plugins/runtimes/paseo
```

The engines carry third-party interpreters (QuickJS, yaegi, risor, gopher-lua)
as ordinary module dependencies — that is the point of moving them out of the
daemon — but the conductor surface they touch is still `pkg/plugin` alone.

**Documentation** lives in [`docs/`](docs/README.md) — the plugin model, install
and capability details, and a reference page per plugin (linked from the table
below).

## Status — read this first

`go.mod` requires `github.com/NodeSpy/conductor v0.11.0` — the tagged release
that carries `pkg/githubkit`, the `plugin.SourceHandler`/`start_source` surface,
and the STEP-ENGINE SDK the `engines/` plugins need (`plugin.KindStep`,
`Decl.ABI`/`EngineABI`, `plugin.EngineFunc`, `plugin.run`, and the `plugin.Host`
client for the `host.kv`/`host.sql`/`host.memory` callbacks) — resolved straight
from the public module proxy. No `replace` directive.

## Available plugins

| Plugin | Kind | Provides | Bundled in conductor? | Notes |
|--------|------|----------|-----------------------|-------|
| [`sentry`](docs/connectors/sentry.md) | connector (source) | `sentry` | **No — removed from core.** This is the only way to get it. | Sentry Integration-Platform webhooks → `issue_alert` / `error_alert` / `event_alert`. HMAC-verified. |
| [`pagerduty`](docs/connectors/pagerduty.md) | connector (source) | `pagerduty` | **No — removed from core.** This is the only way to get it. | PagerDuty V3 incident webhooks → `incident`. Multi-signature (`v1=…,v1=…`) verified, so key rotation works. |
| [`github`](docs/connectors/github.md) | connector (verbs + source) | `github` | **Yes — still bundled.** This is additive. | Full verb surface (comment, submit_review, pr_diff, merge_pr, create_issue, checks, releases, gists, …) over token or GitHub-App auth, plus the webhook events derivable from a single delivery. Built on `pkg/githubkit`. |
| [`paseo`](docs/runtimes/paseo.md) | runtime | `paseo` | **Yes — still bundled.** This is additive, opt-in. | The paseo-daemon operations `internal/dispatch.Backend` needs, by shelling to the `paseo` CLI. Driven by conductor's `rpcBackend`. |
| [`alertmanager`](docs/connectors/alertmanager.md) | connector (source) | `alertmanager` | **No — never in core.** Add it here. | Prometheus Alertmanager **and** Grafana unified-alerting webhooks → one `alert` event per alert (status/severity/labels/annotations). Optional bearer-token auth, fail-closed. |
| [`aws-sns`](docs/connectors/aws-sns.md) | connector (source) | `aws-sns` | **No — never in core.** Add it here. | AWS SNS HTTP(S) subscriber: **auto-confirms** the subscription (gated on signature verification), verifies SNS message signatures (v1/v2, `SigningCertURL` host-allowlisted), emits a `notification` event per message. Optional **smee.io** SSE transport for endpoints with no public URL. |
| [`aws-ses`](docs/connectors/aws-ses.md) | connector (verbs) | `aws-ses` | **No — never in core.** Add it here. | AWS SES v2 email: `send_email`, `send_templated_email`, identities, send quota, suppression list + generic `api`. Pure-Go **SigV4** signing (no `aws` CLI dependency). |
| [`aws-cli`](docs/connectors/aws-cli.md) | connector (verbs) | `aws-cli` | **No — never in core.** Add it here. | AWS via the `aws` CLI: a generic `run` (any service/operation, params → flags, JSON parsed into `result`), plus `s3` (cp/sync/mv/rm/ls/mb/rb), `lambda_invoke`, `sts_identity`, and a `cli` escape hatch. `profile`/`region` select the target; credentials come from the ambient AWS environment. |
| [`docker`](docs/connectors/docker.md) | connector (verbs) | `docker` | **No — never in core.** Add it here. | The container-engine lifecycle as verbs (`run`, `exec`, `build`, `pull`, `push`, `ps`, `images`, `logs`, `stop`, `start`, `rm`, `inspect`, `compose`, `buildx`, `bake`, `cli`) by shelling to the `docker` (or `podman`) CLI. Local by default; `docker_host: ssh://…` / `context:` reach a remote engine. `buildx`/`bake` are docker-only. |
| [`libation`](docs/connectors/libation.md) | connector (verbs) | `libation` | **No — never in core.** Add it here. | Libation ([getlibation.com](https://getlibation.com)): download DRM-free M4B copies of an Audible library by shelling to `LibationCli` — `scan`, `export` (manifest, optionally parsed), `liberate` (with run limits), `set_status` (seeding), `search`, `list_accounts`, `cli`. The download half of an Audible→Audiobookshelf pipeline; compose with the `audiobookshelf` connector for the import half. 60m default timeout. |
| [`smart`](docs/connectors/smart.md) | connector (verbs + source) | `smart` | **No — never in core.** Add it here. | Disk S.M.A.R.T. health via `smartctl` (smartmontools): `scan`, `info`, `health`, `attributes`, `all`, `capabilities`, `test`, `log`, `cli` — every verb `--json`-parsed — plus a poll source emitting a `health` event when a device fails its self-assessment (deduped on device+status). smartctl's exit status is a **bitmask**, so a non-zero exit is data; `sudo: true` for raw device access. |
| [`email`](docs/connectors/email.md) | connector (verbs + source) | `email` | **No — never in core.** Add it here. | Email: SMTP `send`/`send_raw` (multipart text+HTML, STARTTLS/implicit-TLS) + an IMAP poll source emitting `message` events (minimal stdlib IMAP client, mark-seen, dedup). |
| [`plex`](docs/connectors/plex.md) | connector (verbs + source) | `plex` | **No — never in core.** Add it here. | Plex Media Server: sessions, library sections/scan, search, metadata, recently-added, watched/unwatched, refresh + a Plex webhook source (playback events). `X-Plex-Token`. |
| [`sonarr`](docs/connectors/sonarr.md) | connector (verbs + source) | `sonarr` | **No — never in core.** Add it here. | Sonarr (TV): series CRUD, lookup, episodes, commands, queue, calendar, wanted, profiles + a webhook source (Grab/Download/…). API key. |
| [`radarr`](docs/connectors/radarr.md) | connector (verbs + source) | `radarr` | **No — never in core.** Add it here. | Radarr (movies): movie CRUD, lookup, commands, queue, calendar, wanted, profiles + a webhook source. API key. |
| [`lidarr`](docs/connectors/lidarr.md) | connector (verbs + source) | `lidarr` | **No — never in core.** Add it here. | Lidarr (music): artist/album CRUD, lookup, commands, queue, calendar + a webhook source (Grab/Download/…). API key. |
| [`prowlarr`](docs/connectors/prowlarr.md) | connector (verbs + source) | `prowlarr` | **No — never in core.** Add it here. | Prowlarr (indexer manager): indexers, indexer stats, applications, release `search`, commands, system status, tags + generic `api`, plus a Health/ApplicationUpdate webhook source. API key. |
| [`audiobookshelf`](docs/connectors/audiobookshelf.md) | connector (verbs) | `audiobookshelf` | **No — never in core.** Add it here. | Audiobookshelf: libraries/items, `get_item`, `search`, `scan`, series, collections, progress, multipart `upload` (the import half of an Audible→ABS pipeline with `libation`) + generic `api`. Bearer token. |
| [`datadog`](docs/connectors/datadog.md) | connector (verbs + source) | `datadog` | **No — never in core.** Add it here. | Datadog: `post_event`, mute/unmute & get/list monitors, `submit_metric` + a webhook alert source (operator-templated payload, token-verified). API + APP keys. |
| [`jira`](docs/connectors/jira.md) | connector (verbs + source) | `jira` | **No — never in core.** Add it here. | Jira Cloud REST v3: create/update issues, comments (ADF), transitions, assign, search (JQL) + issue/comment webhook events. Basic auth (email + API token). |
| [`telegram`](docs/connectors/telegram.md) | connector (verbs + source) | `telegram` | **No — never in core.** Add it here. | Telegram Bot API: send message/photo/document, edit/delete, answer callbacks, set webhook + message/callback_query source (secret-token verified). Bot token. |
| [`matrix`](docs/connectors/matrix.md) | connector (verbs + source) | `matrix` | **No — never in core.** Add it here. | Matrix Client-Server: send messages/notices/events, join/leave/invite/redact, room state + a `/sync` long-poll source (message/invite). Access token. |
| [`homeassistant`](docs/connectors/homeassistant.md) | connector (verbs + source) | `homeassistant` | **No — never in core.** Add it here. | Home Assistant: `call_service`, get/set state, `fire_event`, `render_template`, history/logbook + an inbound webhook source. Long-lived access token. |
| [`proxmox`](docs/connectors/proxmox.md) | connector (verbs + source) | `proxmox` | **No — never in core.** Add it here. | Proxmox VE: nodes, `qemu` VMs + `lxc` containers (start/stop/shutdown/reboot/clone), cluster resources, storage, snapshots, `vzdump` backups, tasks + generic `api`, plus a task poll source (fires on a finished/failed backup or migration). API-token header; `insecure_skip_verify` for self-signed certs. |
| [`truenas`](docs/connectors/truenas.md) | connector (verbs + source) | `truenas` | **No — never in core.** Add it here. | TrueNAS SCALE: pools, datasets, snapshots, replication, apps, services, system info + generic `api`, plus an alert poll source. Bearer API key; `insecure_skip_verify`. |
| [`opnsense`](docs/connectors/opnsense.md) | connector (verbs) | `opnsense` | **No — never in core.** Add it here. | OPNsense firewall/router: firmware, services (restart/start/stop), firewall aliases (+ apply), interfaces, DHCP leases, gateway status, unbound DNS, reboot + generic `api`. HTTP Basic (key+secret); `insecure_skip_verify`. |
| [`tailscale`](docs/connectors/tailscale.md) | connector (verbs) | `tailscale` | **No — never in core.** Add it here. | Tailscale mesh VPN: devices (authorize/tags/routes/delete), auth keys, ACL get/set, DNS + generic `api`. Uses **managed OAuth2** (client-credentials) or a plain `api_key`. |
| [`unifi`](docs/connectors/unifi.md) | connector (verbs) | `unifi` | **No — never in core.** Add it here. | UniFi Network controller: sites, devices (+ restart), clients (block/unblock/reconnect), WLANs, networks, port-forwards, firewall rules, health, alarms, events + generic `api`. Cookie login (UniFi OS or legacy). |
| [`unifi-protect`](docs/connectors/unifi-protect.md) | connector (verbs) | `unifi-protect` | **No — never in core.** Add it here. | UniFi Protect NVR/cameras (Integration API): cameras, snapshot (JPEG→base64), PTZ, NVR, viewers, lights, sensors, chimes + generic `api`. `X-API-KEY`. |
| [`pihole`](docs/connectors/pihole.md) | connector (verbs) | `pihole` | **No — never in core.** Add it here. | Pi-hole v6: summary/history/queries/top stats, blocking enable/disable, allow/deny domains, lists, groups, clients, gravity update + generic `api`. Password → session (SID). |
| [`adguard`](docs/connectors/adguard.md) | connector (verbs) | `adguard` | **No — never in core.** Add it here. | AdGuard Home: status/stats/query-log, protection toggle, filter lists, rewrites, clients, DNS config, safebrowsing/parental + generic `api`. HTTP Basic. |
| [`portainer`](docs/connectors/portainer.md) | connector (verbs) | `portainer` | **No — never in core.** Add it here. | Portainer: endpoints, stacks (start/stop/delete), containers (actions/logs) + images via the Docker proxy, status + generic `api`. `X-API-Key`. |
| [`grafana`](docs/connectors/grafana.md) | connector (verbs + source) | `grafana` | **No — never in core.** Add it here. | Grafana: search, dashboards (get/create/delete), datasources, folders, alert rules, annotations, org + generic `api`, plus a firing-alerts poll source. Bearer service-account token. |
| [`ifttt`](docs/connectors/ifttt.md) | connector (verbs + source) | `ifttt` | **No — never in core.** Add it here. | IFTTT Maker Webhooks: `trigger` / `trigger_json` + an inbound webhook source (token-verified, fail-closed). Maker key. |
| [`healthchecks`](docs/connectors/healthchecks.md) | connector (verbs + source) | `healthchecks` | **No — never in core.** Add it here. | Healthchecks.io: check CRUD + `ping` (success/fail/start) + a check up/down webhook source. Management API key + ping URLs. |
| [`git`](docs/connectors/git.md) | connector (verbs) | `git` | **No — never in core.** Add it here. | The `git` CLI with **configurable credentials**: clone/fetch/pull/push/checkout/commit/branch/tag/merge/reset/… + `rev_parse`/`ls_remote`/`status` parsing. SSH key or HTTPS token (kept out of argv via GIT_ASKPASS), commit identity. |
| [`gitlab`](docs/connectors/gitlab.md) | connector (verbs + source) | `gitlab` | **No — never in core.** Add it here. | GitLab REST v4: MR/issue comments, create/update/merge MRs & issues, labels, branches, pipelines + generic `api`, plus push/MR/pipeline/issue/note webhook events (`X-Gitlab-Token` verified). |
| [`gitea`](docs/connectors/gitea.md) | connector (verbs + source) | `gitea` | **No — never in core.** Add it here. | Gitea/Forgejo API: issue/PR comments, create issue/PR, merge, labels, releases, branches, files + push/PR/issue webhook events (HMAC-verified). |
| [`linear`](docs/connectors/linear.md) | connector (verbs + source) | `linear` | **No — never in core.** Add it here. | Linear GraphQL: create/update/comment/archive issues, search + issue/comment/project webhook events (HMAC-verified). |
| [`ffmpeg`](docs/connectors/ffmpeg.md) | connector (verbs) | `ffmpeg` | **No — never in core.** Add it here. | Media via `ffmpeg`/`ffprobe`: `transcode`, `extract_audio`, `thumbnail`, `extract_frames`, `trim`, `scale`, `to_gif`, `concat`, `remux`, `overlay`, `probe` (JSON) + `cli`. |
| [`wiz`](docs/connectors/wiz.md) | connector (verbs + source) | `wiz` | **No — never in core.** Add it here. | Wiz cloud security: query/update issues & findings via GraphQL (OAuth2 client-credentials, token cached) + a new-issue webhook source. Deal with what Wiz finds. |
| [`uptimekuma`](docs/connectors/uptimekuma.md) | connector (source) | `uptimekuma` | **No — never in core.** Add it here. | Uptime Kuma webhook notifications → one `monitor` event per heartbeat (up/down/pending/maintenance, name/url/msg). Optional shared-token auth, fail-closed. |
| [`uptimerobot`](docs/connectors/uptimerobot.md) | connector (verbs + source) | `uptimerobot` | **No — never in core.** Add it here. | UptimeRobot: monitor CRUD + pause/resume over the v2 API, plus an alert webhook source (up/down, form or JSON). API key. |
| [`tautulli`](docs/connectors/tautulli.md) | connector (verbs + source) | `tautulli` | **No — never in core.** Add it here. | Tautulli (Plex monitoring): activity/history/stats/libraries/users/metadata + `notify`/`terminate_session`, plus a webhook source. API key. |
| [`qbittorrent`](docs/connectors/qbittorrent.md) | connector (verbs) | `qbittorrent` | **No — never in core.** Add it here. | qBittorrent WebUI: list/add/delete/pause/resume torrents, categories, tags, transfer info + generic `api`. Cookie login (username/password). |
| [`sabnzbd`](docs/connectors/sabnzbd.md) | connector (verbs) | `sabnzbd` | **No — never in core.** Add it here. | SABnzbd Usenet downloader: `queue`/`history`, `add_url`, pause/resume, delete, speed limit, status, categories + generic `api`. API key. |
| [`pushover`](docs/connectors/pushover.md) | connector (verbs) | `pushover` | **No — never in core.** Add it here. | Pushover push notifications: `send` (priority/sound/url/html), emergency receipts (`get_receipt`/`cancel_receipt`), `glances`, `validate_user`. App token + user key. |
| [`notifiarr`](docs/connectors/notifiarr.md) | connector (verbs) | `notifiarr` | **No — never in core.** Add it here. | Notifiarr passthrough Discord notifications (title/message/color/channel/ping/fields) + generic `api`. API key. |
| [`zapier`](docs/connectors/zapier.md) | connector (verbs + source) | `zapier` | **No — never in core.** Add it here. | Zapier: `send` to a Catch-Hook URL (host-validated to hooks.zapier.com) + an inbound webhook source (token-verified, fail-closed). |
| [`xero`](docs/connectors/xero.md) | connector (verbs) | `xero` | **No — never in core.** Add it here. | Xero accounting: invoices, contacts, accounts, payments, bank transactions, items, organisation, connections + raw `api`. Uses conductor's **managed OAuth2** — add an `auth:` block and run `conductor connector auth xero`; conductor injects the token, so plugin egress is `api.xero.com` only. |
| [`google-calendar`](docs/connectors/google-calendar.md) | connector (verbs) | `google-calendar` | **No — never in core.** Add it here. | Google Calendar v3: calendars, events (list/get/create/update/delete), quick_add, freebusy + `api`. **Managed OAuth2** (browser login; `conductor connector auth google-calendar`). |
| [`gmail`](docs/connectors/gmail.md) | connector (verbs) | `gmail` | **No — never in core.** Add it here. | Gmail v1: messages (list/get/send), labels, drafts, threads, modify/trash + `api`. **Managed OAuth2**; `send` builds the MIME itself. |
| [`google-drive`](docs/connectors/google-drive.md) | connector (verbs) | `google-drive` | **No — never in core.** Add it here. | Google Drive v3: files (list/get/upload/download/delete), folders, permissions + `api`. **Managed OAuth2**; multipart/related upload. |
| [`google-sheets`](docs/connectors/google-sheets.md) | connector (verbs) | `google-sheets` | **No — never in core.** Add it here. | Google Sheets v4: values (get/update/append/clear/batch), batch_update, create + `api`. **Managed OAuth2**. |
| [`ntfy`](docs/connectors/ntfy.md) | connector (verbs + source) | `ntfy` | **No — never in core.** Add it here. | ntfy pub/sub: `publish` notifications (title/priority/tags/click/attach) + a topic-subscribe source (JSON stream) emitting `message` events. ntfy.sh or self-hosted. |
| [`terraspace`](docs/connectors/terraspace.md) | connector (verbs) | `terraspace` | **No — never in core.** Add it here. | Terraspace (Terraform/OpenTofu framework) as verbs: `up`/`down`/`plan` per stack, `all_up`/`all_down`, `output`, `import`, `logs`, `list`, `new`, … via the `terraspace` CLI. `TS_ENV` selects the environment. |
| [`twilio`](docs/connectors/twilio.md) | connector (verbs + source) | `twilio` | **No — never in core.** Add it here. | Twilio: `send_sms`/`send_whatsapp`/`make_call` + an inbound SMS/voice webhook source (`X-Twilio-Signature` HMAC-verified). Basic auth (account SID + token). |
| [`notion`](docs/connectors/notion.md) | connector (verbs) | `notion` | **No — never in core.** Add it here. | Notion API: pages, databases (`query_database`), blocks, `search`, comments, users, and a generic `api`. Bearer token + `Notion-Version`. |
| [`cloudflare`](docs/connectors/cloudflare.md) | connector (verbs) | `cloudflare` | **No — never in core.** Add it here. | Cloudflare API: DNS (`dns_list`/`dns_create`/`dns_update`/`dns_delete`), `cache_purge`, zones, `worker_deploy`, rulesets, and a generic `api`. API-token or legacy key/email auth. |
| [`terraform`](docs/connectors/terraform.md) | connector (verbs) | `terraform` | **No — never in core.** Add it here. | Terraform/OpenTofu as verbs (`init`, `validate`, `plan`, `apply`, `destroy`, `output`, `show`, `fmt`, `workspace`, `state`, `import`, `refresh`, `providers`, `version`, `cli`) by shelling to `terraform`, **OpenTofu**, or **Terragrunt** (`engine:`). `-chdir` + non-interactive defaults (`-input=false`, auto-approve); terragrunt gets `run_all`; `output`/`show` parse `-json`. |
| [`helm`](docs/connectors/helm.md) | connector (verbs) | `helm` | **No — never in core.** Add it here. | Helm releases as verbs (`install`, `upgrade`, `uninstall`, `rollback`, `list`, `status`, `history`, `get_values`, `template`, `pull`, `repo_add`, `repo_update`, `test`, `lint`, `cli`) by shelling to `helm`. `list`/`status`/`history`/`get_values` parse JSON into structured outputs. |
| [`kubernetes`](docs/connectors/kubernetes.md) | connector (verbs) | `kubernetes` | **No — never in core.** Add it here. | The Kubernetes lifecycle as verbs (`apply`, `get`, `delete`, `describe`, `logs`, `exec`, `rollout`, `scale`, `patch`, `create`, `label`, `annotate`, `wait`, `top`, `cordon`/`uncordon`/`drain`, `cp`, `cli`) by shelling to `kubectl`. `kubeconfig`/`context`/`namespace` select the target; `get`/`apply` parse JSON into `result`. |

### Step engines

A **step engine** is what executes a code step's work — what a step's `use:`
selects. These four are conductor's own in-binary engines, lifted out: same
snippet contract, same `ctx` shape, same output contract, now in a separate
process the daemon spawns. A snippet written for the bundled `run: js` runs
unchanged on `engines/js`.

The engine that used to reach the daemon's stores through a Go binding now
**asks**: `ctx.store` / `ctx.sql` / `ctx.memory` become one `host.kv` /
`host.sql` / `host.memory` round trip per op, carrying the run's `run_id`, and
conductor authorizes each one against that step's own `DataGuard` before
running the same dispatcher it always did. The plugin holds no store, no
connection string and no capability beyond the ability to ask while its run is
in flight — which is why all four declare an **empty** permission manifest: no
egress, no filesystem, no spawns.

| Plugin | Kind | Provides | Bundled in conductor? | Notes |
|--------|------|----------|-----------------------|-------|
| [`js`](engines/js) | engine | `js` | **Yes — still bundled today.** Additive; the in-binary copy goes away in a later stage. | JavaScript on QuickJS-in-WASM (`fastschema/qjs` over wazero — no cgo). `ctx` is the step inputs, the snippet body is an IIFE whose return value is the outputs, 256 MiB heap cap, `timeout:` halts the module. |
| [`go-embed`](engines/go-embed) | engine | `go-embed` | **Yes — still bundled today.** Additive. | Real Go on `traefik/yaegi`, no toolchain needed. Snippet defines `func run(ctx map[string]any) (any, error)`. Sandboxed to a **data-shaping stdlib allowlist** (no `os`, `net`, `io`, `reflect`, `unsafe`) with `GoPath` pinned off-disk; ctx faces are `import "conductor/store"` / `"conductor/sql"` / `"conductor/memory"`. |
| [`risor`](engines/risor) | engine | `risor` | **Yes — still bundled today.** Additive. | `risor-io/risor` — pure-Go, Go-flavored scripting. `ctx` is the step inputs, the final expression is the outputs. Opts out of risor's default globals (which include `os`/`exec`/`http`/`net`) and grants a data-shaping allowlist plus `store()`, `sql()` and `memory`. |
| [`lua`](engines/lua) | engine | `lua` | **Yes — still bundled today.** Additive. | Lua 5.1 on `yuin/gopher-lua` (pure Go, no cgo). `ctx` is the step inputs as a table, the script `return`s its outputs. Only base/table/string/math are opened, and `dofile`/`loadfile`/`load`/`loadstring` are removed; ctx faces are `ctx.store(…)`, `ctx.sql(…)`, `ctx.memory`. |

### What the source plugins do NOT replace

`sentry` and `pagerduty` genuinely replace connectors that no longer exist in
conductor. They are **not** drop-in equivalents of the old bundled ones, and
`conductor config migrate` will refuse to auto-convert a legacy config for
exactly this reason:

- Event names changed — the bundled sentry connector had one `alert` event;
  this plugin declares `issue_alert` / `error_alert` / `event_alert`.
- Context is flat. Steps template `{{.level}}`, `{{.short_id}}`, … — not the old
  nested `{{.sentry.level}}`.
- Filters use the plugin's own vocabulary (`levels`, `projects`,
  `environments`, `event_types`, `services`, `urgencies`, `priorities`) and are
  evaluated by the daemon's generic list-contains evaluator. There is **no
  `exclude:` support**, so the legacy "first matching rule wins" precedence
  cannot be expressed — write mutually exclusive filters instead.

`github` and `paseo` are additive: conductor still bundles both, and its own
autopilot depends on the bundled github (App-token minting, the catch-up sweep,
the poll-derived events). Use the plugin when you want github verbs running in a
sandboxed subprocess with a narrowed credential, not as a replacement. See
conductor's `docs/design/connector-extraction.md`.

## Install a plugin

Name it in `use:` and run `conductor init`. There is no separate declaration —
the reference *is* the declaration:

```yaml
connectors:
  mysentry:
    use: sentry                          # bare name → connectors/sentry, here
    listen: ":9099"
    client_secret: ${SENTRY_CLIENT_SECRET}
triggers:
  - on: mysentry.issue_alert
    filters: { levels: [error, fatal] }
    steps: [ ... ]

runtimes:
  gpu: { use: paseo }                    # runtimes: → runtimes/paseo, here

engines:
  js: { use: js }                        # engines: → engines/js, here
```

…and a step then selects the engine the same way it always did:

```yaml
steps:
  - id: shape
    use: js                              # the engines: entry above
    code: |
      return { owner: ctx.repo.split("/")[0] };
```

`conductor init` resolves the highest compatible release tag
(`connectors/sentry/vX.Y.Z`), downloads the per-platform asset
`conductor-sentry_<os>_<arch>`, verifies it against the release
`checksums.txt`, and installs it under conductor's **state dir** — install state
is local to the machine, not a committed lockfile. Boot is offline. See the
conductor [Plugins wiki](https://github.com/NodeSpy/conductor/wiki/Plugins).

Pin an exact build with `use: sentry@v1.2.3`; leave it off and the plugin stays
current, with every update logged along with the sha it moved from. This repo
is in conductor's DEFAULT `plugin_trust` allowlist, so installing from here
needs no ceremony; a third-party repo needs an explicit `plugin_trust.allow`
entry.

### What it can do, and what it may do

Each plugin declares its own **permission manifest** — the hosts it calls and
the commands it spawns — in its `Describe`. Conductor records that at install,
shows it to you when you add the plugin, and confines the subprocess to it. A
connector's optional `network:` may NARROW that declaration, never widen it:

```yaml
connectors:
  gh: { use: github, network: ["api.github.com:443"] }
```

OS isolation is available but **not** required: it is opt-in hardening for a
locked-down box, not the default path.

```yaml
connectors:
  mysentry: { use: sentry, isolation: { mode: namespace } }
```

## Tests

`e2e/` drives each built binary over the real wire: newline-delimited JSON-RPC
2.0 on the subprocess's stdin/stdout, using the public SDK's own
request/response types via `internal/rpctest`. It covers HMAC verification and
normalized event shape for the two source plugins, github's token-auth and
App-auth verb paths plus its webhook source, and paseo's verb set and CLI round
trip.

`e2e/engines_test.go` does the same for the four step engines, and exercises the
direction only an engine has: it drives a `plugin.run` and then **answers the
plugin's `host.kv` callbacks** on the same stream while that run is still in
flight (`rpctest.Client.SetHost`), standing in for the daemon's ctx data plane.
Each engine's own package tests drive `run()` in process against a recording
host double (`internal/enginekit/hosttest`) for the interpreter behaviour, the
sandbox boundary, and the exact op/args that cross.

```console
$ go test ./...
```

These tests moved here from conductor's `internal/plugin` package when the
sources moved into this repo. What they do **not** cover is the daemon side —
its plugin client, the manifest confinement, and verify-before-execute. That
stays in conductor, tested against its in-repo `test/plugins/acme-*` reference
plugins, because the daemon's client is internal and deliberately unreachable
from a plugin module.

`e2e/manifest_test.go` additionally asserts that every plugin here declares its
own `Kind` and a permission manifest that matches what it actually does — a
plugin that declares nothing gives conductor nothing to enforce, and a plugin
that over-declares quietly widens what the operator is asked to accept.

## Releasing

Not automated on merge — pushing a tag publishes binaries, so it is a human
decision.

Tag shape is `<kind>/<name>/vX.Y.Z` — `connectors/sentry/v1.0.0`,
`runtimes/paseo/v1.0.0`, `engines/js/v1.0.0`. The kind prefix matches the source
directory AND the prefix conductor's resolver matches against when it resolves a
bare `use:`, so tags for a connector, a runtime and an engine of the same name
never mix.

Pushing one runs [`.github/workflows/release.yml`](.github/workflows/release.yml),
which cross-builds `<kind>/<name>` for linux/darwin/windows on amd64 and arm64,
checksums the set, and publishes `conductor-<name>_<os>_<arch>` +
`checksums.txt` to the release. Note the ASSET name is flat — the kind lives in
the tag, not in the filename.

The workflow builds straight against `github.com/NodeSpy/conductor v0.11.0` from
the public module proxy — no sibling checkout, no `replace` rewrite.

## Build your own plugin

You don't need this repo. A plugin is a standalone Go binary:

```go
package main

import plugin "github.com/NodeSpy/conductor/pkg/plugin"

func main() {
	plugin.Serve(plugin.ConnectorFunc(
		func() plugin.Decl { return plugin.Decl{Type: "acme", Verbs: []plugin.Verb{{Name: "ping"}}} },
		func(r plugin.InvokeRequest) (plugin.InvokeResult, error) {
			return plugin.InvokeResult{Outputs: map[string]any{"pong": true}}, nil
		},
	))
}
```

Release it from **your own** repo as `conductor-<name>_<os>_<arch>` assets
under `<kind>/<name>/vX.Y.Z` tags, and point `use:` at your repo
(`use: your-org/your-repo/<name>`) — a third-party source needs an explicit
`plugin_trust.allow` entry in the operator's config. Declare `Kind` in your
`Describe`: conductor checks it against the block the reference appeared in, so
a connector can never be wired as a runtime. Source
plugins (webhook/poll) additionally use `pkg/sourcekit` (HMAC verify, webhook
listener, dedup) and implement `plugin.SourceHandler`.
