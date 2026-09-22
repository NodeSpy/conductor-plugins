# conductor plugins

The official plugin catalog for [conductor](https://github.com/NodeSpy/conductor) —
**connectors** to the services you already run, sandboxed **code engines** for
your steps, and agent **runtimes**. Conductor's core ships lean; everything here
is fetched on demand.

You don't clone this repo. **Name a plugin in your config and run `conductor init`** —
conductor downloads it, verifies the checksum, and runs it as an isolated
subprocess.

```yaml
connectors:
  gh:  { use: github, token: ${GITHUB_TOKEN} }
  hue: { use: homeassistant, base_url: http://homeassistant.local:8123, token: ${HA_TOKEN} }

steps:
  - use: gh.create_issue
    with: { repo: me/app, title: "Nightly build failed" }
```

Pin an exact build with `use: github@v1.2.3`; leave it off and it stays current.
Each plugin has a full reference page under [`docs/`](docs/README.md).

> **Logins are handled for you.** Connectors that use OAuth — **Xero**, the
> **Google** set, **Tailscale**, optionally **Asana** — plug into conductor's
> managed OAuth2: add an `auth:` block and run `conductor connector auth <name>`
> once (a browser login), and conductor stores and refreshes the token. The
> plugin never sees your secret.

---

## Connectors

### Source control & project tracking
- **[github](docs/connectors/github.md)** — pull requests, reviews, issues, checks, releases, gists + webhook events. Token or GitHub-App auth.
- **[gitlab](docs/connectors/gitlab.md)** — merge requests, issues, labels, branches, pipelines + push/MR/pipeline webhooks.
- **[gitea](docs/connectors/gitea.md)** — Gitea/Forgejo issues, PRs, releases, files + webhooks.
- **[git](docs/connectors/git.md)** — the `git` CLI with managed credentials: clone, commit, push, branch, tag, merge…
- **[asana](docs/connectors/asana.md)** — Asana tasks, projects, sections, comments, search + task/story/project webhook events. Token or managed OAuth2.
- **[jira](docs/connectors/jira.md)** — Jira Cloud issues, comments, transitions, JQL search + webhooks.
- **[linear](docs/connectors/linear.md)** — Linear issues: create/update/comment/search + webhooks.
- **[notion](docs/connectors/notion.md)** — Notion pages, databases, blocks, search, comments.

### Cloud & infrastructure
- **[aws-cli](docs/connectors/aws-cli.md)** — AWS via the `aws` CLI: any service/operation, plus `s3`, Lambda invoke, STS identity.
- **[cloudflare](docs/connectors/cloudflare.md)** — DNS records, cache purge, zones, Workers, rulesets.
- **[terraform](docs/connectors/terraform.md)** — Terraform / OpenTofu / Terragrunt: init, plan, apply, destroy, output, state…
- **[terraspace](docs/connectors/terraspace.md)** — the Terraspace framework: per-stack up/down/plan, `all_up`/`all_down`, output.
- **[kubernetes](docs/connectors/kubernetes.md)** — `kubectl` lifecycle: apply, get, logs, exec, rollout, scale, drain…
- **[helm](docs/connectors/helm.md)** — Helm releases: install, upgrade, rollback, list, status, template, repos.
- **[docker](docs/connectors/docker.md)** — Docker/Podman lifecycle: run, exec, build, compose, buildx, logs… local or remote.
- **[postgres](docs/connectors/postgres.md)** — PostgreSQL LISTEN/NOTIFY as an event source (react to a row change, no polling) + query/exec/notify verbs.
- **[redis](docs/connectors/redis.md)** — Redis / Valkey: get/set/del/incr/expire, publish, raw `command`, and a live pub/sub source (SUBSCRIBE / PSUBSCRIBE).
- **[aws-sqs](docs/connectors/aws-sqs.md)** — Amazon SQS: long-poll a queue as an event source + send/receive/delete/attributes verbs. SDK-free & CLI-free (hand-rolled SigV4).

### Homelab — media
- **[sonarr](docs/connectors/sonarr.md)** · **[radarr](docs/connectors/radarr.md)** · **[lidarr](docs/connectors/lidarr.md)** — Servarr for TV / movies / music: library CRUD, queue, calendar + Grab/Download webhooks.
- **[prowlarr](docs/connectors/prowlarr.md)** — Servarr indexer manager: indexers, apps, release search + health webhooks.
- **[qbittorrent](docs/connectors/qbittorrent.md)** · **[sabnzbd](docs/connectors/sabnzbd.md)** — torrent / Usenet downloaders: queue, add, pause/resume, limits.
- **[plex](docs/connectors/plex.md)** — Plex Media Server: sessions, libraries, search, scan + playback webhooks.
- **[tautulli](docs/connectors/tautulli.md)** — Plex monitoring: activity, history, stats, notify + webhooks.
- **[audiobookshelf](docs/connectors/audiobookshelf.md)** — audiobook/podcast server: libraries, search, scan, upload.
- **[libation](docs/connectors/libation.md)** — download DRM-free copies of your Audible library (pairs with audiobookshelf).

### Homelab — servers, network & storage
- **[proxmox](docs/connectors/proxmox.md)** — Proxmox VE: VMs & containers (start/stop/clone/migrate), storage, snapshots, backups + a task-finished source.
- **[truenas](docs/connectors/truenas.md)** — TrueNAS SCALE: pools, datasets, snapshots, replication, apps + an alert source.
- **[synology](docs/connectors/synology.md)** — Synology DSM: system, storage, FileStation, DownloadStation.
- **[unifi](docs/connectors/unifi.md)** — UniFi Network: devices, clients (block/reconnect), WLANs, firewall, health, alarms.
- **[unifi-protect](docs/connectors/unifi-protect.md)** — UniFi Protect cameras: snapshots, PTZ, NVR, sensors.
- **[opnsense](docs/connectors/opnsense.md)** · **[pfsense](docs/connectors/pfsense.md)** — firewalls: rules, aliases, services, interfaces, DHCP, DNS.
- **[tailscale](docs/connectors/tailscale.md)** — Tailscale mesh VPN: devices, auth keys, ACLs, DNS.
- **[pihole](docs/connectors/pihole.md)** · **[adguard](docs/connectors/adguard.md)** — DNS ad-blocking: stats, blocking toggle, allow/deny lists.
- **[nginx-proxy-manager](docs/connectors/nginx-proxy-manager.md)** — reverse proxy: proxy hosts, redirects, streams, certs.
- **[portainer](docs/connectors/portainer.md)** — container management: environments, stacks, containers, images.
- **[homeassistant](docs/connectors/homeassistant.md)** — Home Assistant: call services, get/set state, fire events, templates + webhooks.
- **[smart](docs/connectors/smart.md)** — disk S.M.A.R.T. health via `smartctl` + a failing-drive source.
- **[mqtt](docs/connectors/mqtt.md)** — MQTT broker: subscribe to topic filters as a live message source + publish to a topic (Home Assistant, Zigbee2MQTT, sensors).

### Monitoring & alerting
- **[grafana](docs/connectors/grafana.md)** — dashboards, datasources, folders, alert rules, annotations + a firing-alerts source.
- **[netdata](docs/connectors/netdata.md)** — metrics, charts, alarms + an active-alarm source.
- **[datadog](docs/connectors/datadog.md)** — events, monitors, metrics + a webhook alert source.
- **[sentry](docs/connectors/sentry.md)** — Sentry issue/error alerts (webhook source).
- **[pagerduty](docs/connectors/pagerduty.md)** — PagerDuty incident webhooks (source).
- **[alertmanager](docs/connectors/alertmanager.md)** — Prometheus / Grafana alerts (webhook source).
- **[uptimekuma](docs/connectors/uptimekuma.md)** · **[uptimerobot](docs/connectors/uptimerobot.md)** · **[healthchecks](docs/connectors/healthchecks.md)** — uptime & cron monitoring.
- **[aws-sns](docs/connectors/aws-sns.md)** — AWS SNS subscriber: auto-confirms and emits an event per message (source).
- **[wiz](docs/connectors/wiz.md)** — Wiz cloud security: issues & findings + a new-issue source.

### Notifications, chat & email
- **[telegram](docs/connectors/telegram.md)** · **[matrix](docs/connectors/matrix.md)** — messaging: send + inbound message source.
- **[ntfy](docs/connectors/ntfy.md)** · **[pushover](docs/connectors/pushover.md)** · **[notifiarr](docs/connectors/notifiarr.md)** — push notifications.
- **[twilio](docs/connectors/twilio.md)** — SMS / WhatsApp / voice + an inbound source.
- **[email](docs/connectors/email.md)** — SMTP send + IMAP inbox source.
- **[aws-ses](docs/connectors/aws-ses.md)** — AWS SES email: send, templates, identities, suppression list.
- **[ifttt](docs/connectors/ifttt.md)** · **[zapier](docs/connectors/zapier.md)** — automation webhooks, out and in.

### Identity & access
- **[keycloak](docs/connectors/keycloak.md)** — Keycloak admin: realms, users, groups, clients, roles, sessions.
- **[authentik](docs/connectors/authentik.md)** — authentik: users, groups, applications, providers, flows, events.

### Google Workspace  *(managed OAuth2)*
- **[google-calendar](docs/connectors/google-calendar.md)** — calendars, events, quick-add, free/busy.
- **[gmail](docs/connectors/gmail.md)** — messages, send, labels, drafts, threads.
- **[google-drive](docs/connectors/google-drive.md)** — files (upload/download), folders, permissions.
- **[google-sheets](docs/connectors/google-sheets.md)** — read/write ranges, append, batch update.
- **[google-tasks](docs/connectors/google-tasks.md)** — task lists and tasks.
- **[google-contacts](docs/connectors/google-contacts.md)** — contacts and search (People API).

### Business & media
- **[xero](docs/connectors/xero.md)** — Xero accounting: invoices, contacts, accounts, payments, bank transactions.  *(managed OAuth2)*
- **[ffmpeg](docs/connectors/ffmpeg.md)** — media via `ffmpeg`/`ffprobe`: transcode, thumbnail, trim, concat, probe.

---

## Code engines

A **code engine** runs a step's `code:` in a sandboxed language — pick it with the
step's `use:`. The step's inputs arrive as `ctx` (or the input document); the
result becomes the step's outputs.

| Engine | Language |
|--------|----------|
| **[js](docs/engines/js.md)** | JavaScript (QuickJS) |
| **[lua](docs/engines/lua.md)** | Lua 5.1 |
| **[risor](docs/engines/risor.md)** | Risor — Go-flavored scripting |
| **[go-embed](docs/engines/go-embed.md)** | real Go (yaegi, no toolchain) |
| **[starlark](docs/engines/starlark.md)** | Starlark — a deterministic Python dialect |
| **[cel](docs/engines/cel.md)** | CEL — expression evaluation for computed fields/conditions |
| **[jq](docs/engines/jq.md)** | jq — JSON transforms |
| **[yq](docs/engines/yq.md)** | yq — YAML transforms (comment-preserving) |
| **[wasm](docs/engines/wasm.md)** | any WebAssembly module (any language → wasm) |

All run sandboxed — no filesystem, network, or spawns unless the step is granted
conductor's `ctx.store` / `ctx.sql` / `ctx.memory`.

## Runtimes

- **[paseo](docs/runtimes/paseo.md)** — run agents through the `paseo` CLI.

---

## For contributors

Building a plugin, releasing, and the plugin protocol itself are covered in
[`docs/`](docs/README.md) and the conductor
[Plugins wiki](https://github.com/NodeSpy/conductor/wiki/Plugins). In short: a
plugin is a standalone Go binary built only against conductor's public SDK
(`pkg/plugin`, `pkg/sourcekit`, `pkg/githubkit`) — no conductor internals — and a
tag `<kind>/<name>/vX.Y.Z` publishes its per-platform binaries.
