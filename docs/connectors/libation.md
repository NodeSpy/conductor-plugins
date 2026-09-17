# `libation` connector

Drive [Libation](https://getlibation.com) by shelling out to its CLI,
`LibationCli`, to pull **DRM-free M4B copies of an Audible library**. The
subcommands a scheduled download pass needs are exposed as verbs — `scan`,
`export`, `liberate`, `set_status`, `search`, `list_accounts` — plus a `cli`
escape hatch for the rest.

This is the **download half** of an Audible → Audiobookshelf pipeline. The
import half is the separate [`audiobookshelf`](audiobookshelf.md) connector; a
conductor workflow composes the two, which is why nothing here knows anything
about Audiobookshelf. See [the full pipeline](#the-full-audiobookshelf-pipeline)
below.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/libation/main.go`](../../connectors/libation/main.go)
- **Provides:** `libation`
- **Capabilities:** spawns `LibationCli`; **no declared egress** — Libation
  itself reaches Audible and its CDN, so the operator scopes what the
  subprocess may dial with the instance's `network:`

```yaml
connectors:
  lib:
    use: libation
    env: { LIBATION_FILES_DIR: /config }
    dir: /data
triggers:
  - on: schedule
    every: 6h
    steps:
      - id: scan
        uses: lib.scan
      - id: fetch
        uses: lib.liberate
        options: { limit_books: 25 }
```

## Setup

You'll end up with an already-authenticated Libation install this connector
can shell out to.

**Prerequisites:** [Libation](https://getlibation.com) installed, with
`LibationCli` reachable (on `PATH`, or pointed at via `binary`).

1. Install Libation on the host/container this connector runs from.
2. Launch Libation interactively once and add your Audible account (Settings
   → Accounts → Add Account) — this is an interactive OAuth login, sometimes
   with a CAPTCHA, and cannot be scripted or automated.
3. Note the directory Libation stores its settings/database/credentials in
   — this is what `LIBATION_FILES_DIR` should point at.
4. Verify the account is healthy: `LibationCli list-accounts` should list it
   with valid stored credentials.

**Configure:**

```yaml
connectors:
  lib:
    use: libation
    binary: /libation/LibationCli
    env: { LIBATION_FILES_DIR: /config }
    dir: /data
```

## Downloads are slow, and partial failure is normal

Two things shape this connector:

1. **The default per-verb timeout is 60m**, not the minutes a normal CLI
   connector allows. A long title is tens of gigabytes and Audible throttles.
   Raise it per verb with `timeout` for a real backlog.
2. **A non-zero exit is data, not an error.** `liberate` returns non-zero when
   Audible refuses to license one title of many — the pass still made progress
   on the rest. Inspect `exit_code`; nothing is raised as an invocation error.

## Connection

Every field is optional.

| key | type | purpose |
|-----|------|---------|
| `binary` | string | `LibationCli` binary path (default `LibationCli`) |
| `dir` | string | working directory for every invocation |
| `env` | map | process environment — **`LIBATION_FILES_DIR`** selects Libation's config/database directory |
| `timeout` | duration | default per-verb timeout (default `60m`); a verb's `timeout` option overrides it |

Libation keeps its settings, credentials, and SQLite library database in one
directory. Point the connector at it with `LIBATION_FILES_DIR` rather than
relying on whatever the daemon's ambient environment happens to be:

```yaml
connectors:
  lib: { use: libation, binary: /libation/LibationCli, env: { LIBATION_FILES_DIR: /config } }
```

> **Logging in is out of scope.** Libation's Audible login is interactive
> (OAuth, sometimes a CAPTCHA), so it is done once by hand — `LibationCli` has
> no non-interactive login. This connector drives an **already-authenticated**
> Libation. `list_accounts` is the health check: an account whose stored
> credentials expired makes every `scan` fail.

## Common output shape

| output | type | notes |
|--------|------|-------|
| `stdout` | string | captured stdout |
| `stderr` | string | captured stderr |
| `exit_code` | integer | process exit code (0 = success) |

## Verbs

| verb | LibationCli | options |
|------|-------------|---------|
| `scan` | `scan [accounts…]` | `accounts` (list) |
| `export` | `export -p <path> -j\|-c\|-x [asins…]` | `path` *, `format`, `asins`, `parse` |
| `liberate` | `liberate [-i asin]… [--pdf] [--force] [--limit-*]` | `asins`, `pdf`, `force`, `limit_books`, `limit_mb`, `limit_gb` |
| `set_status` | `set-status --downloaded\|--download-pending [--force] [asins…]` | `status` *, `asins`, `force` |
| `search` | `search [-n N] [--bare] <query>` | `query` *, `count`, `bare` |
| `list_accounts` | `list-accounts [--bare]` | `bare` |
| `cli` | raw argv | `args` * |

### `scan` — index the Audible library

Asks Audible what is new and records it in Libation's database. Downloads
nothing, so it is cheap enough to run on a short interval. `accounts` (ids or
nicknames) narrows it; the default is every configured account.

### `export` — write the library manifest

`path` * (`-p`), `format` (`json` default → `-j`, `csv` → `-c`, `xlsx` → `-x`),
`asins` (limit the export), `parse`.

The manifest is how a workflow learns each book's `BookStatus` — `Liberated`, or
still pending. LibationCli writes it to **`path`**, not to stdout.

`parse: true` reads that file back into the **`library`** output (a list) plus
**`count`**. It is opt-in because a large library is a multi-megabyte manifest
most workflows would rather hand to the next step by path.

```yaml
uses: lib.export
options: { path: /data/library.json, parse: true }
# → LibationCli export -p /data/library.json -j
```

### `liberate` — download and decrypt

The slow step. With no `asins` it fetches every un-liberated title; naming
`asins` fetches exactly those (`-i` each), which is how you keep a title Audible
has already refused from being re-attempted every pass.

`pdf` (`--pdf`, PDFs only), `force` (`--force`, re-download a liberated title),
and one of `limit_books` / `limit_mb` / `limit_gb`. The three limits are
**mutually exclusive** in LibationCli itself — passing two is rejected here
rather than spending a process to be told so.

```yaml
uses: lib.liberate
options: { asins: [B00FJLGQO2, B017V4IM1G] }
# → LibationCli liberate -i B00FJLGQO2 -i B017V4IM1G
```

A **run limit is the safety rail on a fresh install**: an empty Libation
database reports the entire library as pending, and without a bound one pass
would pull hundreds of books.

```yaml
uses: lib.liberate
options: { limit_books: 25, timeout: 12h }
```

### `set_status` — seed what you already hold

`status` * (`downloaded` → `--downloaded`, `pending` → `--download-pending`),
`asins`, `force` (`--force`, set the status without looking for the audio file).

The seeding verb. Point a new Libation at a library Audiobookshelf already
holds, mark those books downloaded, and the first `liberate` fetches only what
is genuinely missing. `status` is **required** — defaulting it would let a typo
silently flip the library the wrong way.

### `search` / `list_accounts`

- `search`: `query` * (Lucene, positional and last), `count` (`-n`, 0 = all),
  `bare` (`--bare`, one ASIN per line, no titles).
- `list_accounts`: `bare` (`--bare`, tab-separated, no table borders). Reports
  each account's locale and **whether its stored credentials are still valid**.

### `cli` — any LibationCli subcommand

`args` * (raw argv appended after the binary). The escape hatch for `convert`,
`get-setting`, `upload`, `version`, and anything else the first-class verbs
don't model.

```yaml
uses: lib.cli
options: { args: [get-setting, -b] }
```

## The full Audible → Audiobookshelf pipeline

This replicates [NodeSpy/audiobookshelf-import](https://github.com/NodeSpy/audiobookshelf-import),
which wires the same thing together with shell scripts and cron: a discovery
pass (`scan` → `export` → `liberate`) that writes M4Bs into a directory
Audiobookshelf watches, and an import pass that makes ABS pick them up.

The two halves are **deliberately separate** there, and stay separate here: a
50-hour download must not delay detection of the next purchase. In conductor
that is two triggers on different intervals rather than one long step.

```yaml
connectors:
  lib:
    use: libation
    env: { LIBATION_FILES_DIR: /config }
    dir: /data                      # where the M4Bs land
  abs:
    use: audiobookshelf
    base_url: https://abs.example.com
    token: ${ABS_TOKEN}

triggers:
  # 1. Download half — ask Audible what is new, then fetch it.
  - on: schedule
    every: 6h
    steps:
      - id: scan
        uses: lib.scan
      - id: manifest
        uses: lib.export
        options: { path: /data/library.json, parse: true }
      - id: fetch
        uses: lib.liberate
        options: { limit_books: 25, timeout: 12h }

  # 2. Import half — let Audiobookshelf pick up whatever landed on disk.
  - on: schedule
    every: 1h
    steps:
      - id: import
        uses: abs.scan
        options: { library_id: ${ABS_LIBRARY_ID} }
```

Seeding a fresh install, so the first `liberate` does not pull the whole
library:

```yaml
steps:
  - id: scan
    uses: lib.scan
  - id: seed
    uses: lib.set_status
    options: { status: downloaded }   # anything whose audio file is already on disk
  - id: verify
    uses: lib.export
    options: { path: /data/library.json, parse: true }
```

## Capabilities & security

Declares `Commands: [LibationCli]` and `Spawns: true`; **no `Egress`**.

- The connector dials nothing itself — **Libation** reaches Audible and its
  CDN. A connector's `network:` can only *narrow* a declaration, so the egress
  the subprocess actually gets is the operator's to scope.
- Verb options become CLI argv **directly** (`exec.CommandContext`, no shell),
  so there is no shell-quoting/injection surface in how options are assembled.
- Libation holds **Audible credentials** in its files directory. `cli` can read
  and change Libation's settings (`get-setting`, `export-master-key`), so scope
  the connector — and the trigger — accordingly.
