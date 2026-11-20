# `backup` connector

Versioned backups across one or more storage **tiers**. Each tier has its
own driver and retention. A `put` writes one snapshot to every tier, skips
a tier whose newest snapshot has identical content, then applies that
tier's retention. `verify` proves the backups restore.

- **Kind:** connector (verbs only)
- **Source:** [`connectors/backup/`](../../connectors/backup/)
- **Provides:** `backup`
- **Capabilities:** commands `restic`, `rclone`, `borg`, `ssh` (whichever
  are installed); write `config:allow_paths`; egress `config:allow_hosts`.

## Drivers

| driver | where | encryption | dedup |
|---|---|---|---|
| `local` | a directory (any mounted path) | optional [age](https://age-encryption.org) | skip-if-unchanged |
| `rclone` | any rclone backend: S3-compatible, SFTP, B2, WebDAV, SMB, Drive, … | age (**required** unless `encrypt: false`, e.g. for an rclone `crypt` remote) | skip-if-unchanged |
| `restic` | any restic repo: path, `sftp:`, `s3:`, `b2:`, `rest:`, `rclone:` | restic's own (repo password) | restic's content-defined chunking + skip-if-unchanged |
| `borg` | a path or `ssh://user@host[:port]/path` (borg on the server) | borg `repokey-blake2` (passphrase), or `encrypt: false` | borg's chunking + skip-if-unchanged |

The tool for each driver you use must be installed on the conductor host.
Tested with restic 0.18, rclone 1.71 and borg 1.4.

## Connection

```yaml
connectors:
  backups:
    use: backup
    allow_paths: [/srv/backups]                    # every local dir / local repo must be under one
    allow_hosts: [nas.lan:22, s3.example.com:443]  # every remote host:port a tier reaches
    keep_daily: 7      # defaults for every tier
    keep_weekly: 2
    keep_monthly: 6
    tiers:
      - name: hot
        driver: local
        dir: /srv/backups/conductor
        keep_daily: 14
      - name: nas
        driver: restic
        repo: sftp:backup@nas.lan:/volume1/restic
        password: '{{ vault "house" "restic_password" }}'
        ssh:
          key: '{{ vault "house" "backup_ssh_key" }}'
          known_hosts: "nas.lan ssh-ed25519 AAAA..."
      - name: offsite
        driver: rclone
        remote: ":s3,provider=Other,endpoint='https://s3.example.com',env_auth=true:my-bucket/conductor"
        env: { AWS_ACCESS_KEY_ID: ..., AWS_SECRET_ACCESS_KEY: ... }
        age_recipients: [age1...]                  # public key only: this host can write, not read
        keep_daily: 0
        keep_monthly: 12
```

`dir: /path` with no `tiers:` is shorthand for a single local tier.

### Tier fields

| field | drivers | purpose |
|---|---|---|
| `name` | all | Tier name, used by `tier`/`tiers` options (default: the driver). |
| `driver` | all | `local`, `rclone`, `restic` or `borg`. |
| `keep_daily` / `keep_weekly` / `keep_monthly` | all | Retention for this tier. |
| `dir` | local | Absolute directory. |
| `remote` | rclone | An rclone path: a named remote (configure it with `env: {RCLONE_CONFIG_<NAME>_TYPE: ..., ...}`) or an on-the-fly one (`:s3,provider=...:bucket`). |
| `repo` | restic, borg | Repository location. |
| `password` | restic | Repository password. |
| `passphrase` | borg | Repokey passphrase. |
| `age_recipients` | local, rclone | Public keys to encrypt to. |
| `age_identity` | local, rclone | Private key. Needed to `read`/`verify` an encrypted tier (and to dedup against it). If set without `age_recipients`, its public key is used for writing. |
| `encrypt` | local, rclone, borg | `false` opts out (rclone: when the remote is a `crypt` remote; borg: `--encryption none`). |
| `env` | rclone, restic, borg | Extra environment for the tool (S3 credentials, rclone remote config). |
| `ssh` | rclone (sftp), restic (sftp), borg (ssh) | `{key, known_hosts, port, insecure_accept_host_key}`. `known_hosts` is required unless you opt into `insecure_accept_host_key`. |
| `init` | restic, borg | Create the repository on first use (default `true`). |
| `check_read_data` | restic | `verify mode: check` also reads this subset of pack data, e.g. `5%`. |
| `check_data` | borg | `verify mode: check` also runs `--verify-data`. |

Keep passwords, passphrases, keys and S3 credentials in the vault.

**Under the plugin jail:** network goes through conductor's egress proxy.
S3 and other HTTP backends use it via the proxy env. rclone's sftp backend
uses its `http_proxy` option. For restic and borg, ssh tunnels through this
plugin's own binary as a CONNECT `ProxyCommand`. Only `allow_hosts` are
reachable, and validation flags a tier whose host isn't listed.

## Snapshots

Each snapshot is named `<set>-<UTC stamp>.<ext>` and records the sha256 and
size of its plaintext:

- **local / rclone:** an object (`.age` suffix when encrypted) plus a
  `.<name>.meta` sidecar. The sidecar is encrypted too, so an encrypted tier
  doesn't leak content hashes. The meta is written first, so a half-finished
  put is never listed. Local writes are temp-file plus rename.
- **restic:** one snapshot of a single stdin file, tagged
  `conductor-set=<set>`, `sha256=…`, `size=…`.
- **borg:** one archive named after the snapshot, with the hash and size in
  its comment.

Files and archives that don't match the naming are never listed or pruned.

## Retention

Per tier: keep the newest snapshot overall, plus the newest of each of the
last `keep_daily` days, `keep_weekly` ISO weeks and `keep_monthly` months.
Anything else is removed (`restic forget --prune`, `borg delete` +
`compact`, or file deletes).

Tiering is retention: a `hot` local tier with `keep_daily: 14` and an
`offsite` tier with `keep_monthly: 12` together give fast recent restores
and long, cheap history. For cold storage classes (S3 Glacier, B2 archive),
use the bucket's own lifecycle rules on the rclone/restic target.

## Verbs

- **`put`**: `set`*, exactly one of `text` / `base64`, `ext` (default
  `txt`), `tiers` (default all), `skip_unchanged` (default true). If any tier
  fails, the call fails and names every failed tier. Tiers that succeeded
  keep their snapshot. → `sha256`, `size`, `tiers: [{tier, status:
  written|unchanged, name, pruned}]`.
- **`list`**: `set`*, `tier` (default: first). → `items: [{name, time,
  size, sha256, encryption}]`, newest first.
- **`read`**: `set`*, `tier`, `name` (default newest), `encoding`
  (`text`|`base64`), `max_bytes` (default 64 MiB). Restores the content
  and **fails if it doesn't match the recorded sha256 and size**. →
  `name`, `content`, `size`, `sha256`.
- **`prune`**: `set`*, `tiers`, `dry_run`. → `tiers: [{tier, pruned,
  kept}]`.
- **`verify`**: `set`*, `tiers`, `mode` (`restore` default | `check` |
  `both`), `sample` (snapshots to restore, newest first; default 1, `0` =
  all). `check` runs `restic check`, `borg check`, or a meta check for
  file tiers. `restore` restores and compares against the recorded hash.
  A set with no snapshots fails, because nothing was proven. Any problem
  fails the call, so a scheduled verify alerts like any failed run. →
  `ok`, `tiers: [{tier, checked, restored: [{name, ok, error}]}]`.

## Restore testing in practice

```yaml
triggers:
  - name: backup-restore-test
    "on": timer.weekly
    steps:
      - uses: backups.verify
        with: { set: calendar-work, mode: both, sample: 3 }
```

## Testing

Unit tests: `go test ./connectors/backup/`. End to end, with the real tools
against RustFS (S3) and an OpenSSH + borg container, including through an
allow-listing CONNECT proxy:

```sh
docker build -t backup-e2e-sshd e2e/testdata/backup-sshd
BACKUP_E2E=1 BACKUP_E2E_TOOLS=/dir/with/restic-rclone-borg go test ./e2e -run TestBackup -v
```
