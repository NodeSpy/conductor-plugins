# `backup` connector

Versioned backups to a local directory, with daily/weekly/monthly
retention. Each `put` writes one snapshot file into a named **set** and then
prunes that set. Built on the standard library only.

- **Kind:** connector (verbs only)
- **Source:** [`connectors/backup/main.go`](../../connectors/backup/main.go)
- **Provides:** `backup`
- **Capabilities:** write `config:dir` only. No network.

To get backups off the machine, point `dir` at a mounted or synced path
(NFS, SMB, an rclone mount, a Syncthing folder).

## Connection

| key | default | purpose |
|---|---|---|
| `dir` | (required) | Absolute backup root. The plugin can write only here. |
| `keep_daily` | `7` | Keep the newest snapshot of each of this many most recent days. |
| `keep_weekly` | `2` | ...of each of this many most recent ISO weeks. |
| `keep_monthly` | `6` | ...of each of this many most recent months. |

The newest snapshot is always kept. A verb's own `keep_*` option overrides
the connection's value for that call.

## Layout

```
<dir>/<set>/<set>-20261120T031500Z.ics
```

Set names use letters, digits, `.`, `_` and `-`. Timestamps are UTC and
unique within a set. Snapshots are written to a temp file and renamed into
place, so a crash never leaves a partial snapshot. Files in a set directory
that don't match the snapshot naming are never touched by retention.

## Verbs

- **`put`**: `set`*, exactly one of `text` / `base64`, `ext` (default
  `txt`), `keep_*`. Writes a snapshot, then applies retention.
  → `path`, `size`, `sha256`, `pruned`.
- **`list`**: `set`*. → `items` (`{name, path, time, size}`, newest first).
- **`read`**: `set`*, `name` (default: newest), `encoding` (`text` |
  `base64`), `max_bytes` (default 16 MiB). → `name`, `content`, `size`.
- **`prune`**: `set`*, `keep_*`, `dry_run`. Applies retention without
  writing. → `pruned`, `kept`.

## Example

```yaml
connectors:
  backups: { use: backup, dir: /srv/backups/conductor }

steps:
  - id: export
    uses: cal_work.events
    with: { timeMin: "...", singleEvents: true }
  - id: save
    uses: backups.put
    with: { set: calendar-work, ext: json, text: "{{ toJson .export.items }}" }
```
