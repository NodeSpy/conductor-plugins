# `git` connector

Drive `git` directly — clone/init/fetch/pull/push, the working-tree verbs
(`checkout`, `switch`, `add`, `commit`, `status`, `diff`, `log`),
branch/tag/remote/stash management, `merge`/`rebase`/`reset`, the read-only
`rev_parse`/`ls_remote`/`show`, and a `cli` escape hatch for any subcommand a
first-class verb does not cover.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/git/main.go`](../../connectors/git/main.go)
- **Provides:** `git`
- **Capabilities:** spawns `git` / `ssh`; no declared egress (the remote URL
  or SSH host is reached by git's own transport)

```yaml
connectors:
  repo: { use: git, dir: /work/checkout }
triggers:
  - on: schedule.tick
    steps:
      - id: sync
        uses: repo.pull
        options: { remote: origin, branch: main, ff_only: true }
```

## Setup

Installs `git` itself; authentication is whatever the remote's transport requires — an SSH key or an HTTPS token — configured on the connector, not the CLI's global config.

**Prerequisites:** `git` on PATH (override with `binary`); `ssh` on PATH for SSH remotes.

1. Install git — usually preinstalled on Linux/macOS; otherwise your distro's package or [git-scm.com](https://git-scm.com/downloads).
2. For an **SSH** remote: generate a deploy key (`ssh-keygen -t ed25519 -f deploy_key -N ""`) and add the public half to the remote host (e.g. a GitHub deploy key); point the connector at the private half with `ssh_key_path` (or inline it via `ssh_key`).
3. For an **HTTPS** remote: mint a personal access token (or a GitHub App installation token) from the provider, and pass it as `token` (with `username`, e.g. `x-access-token` for GitHub Apps).
4. Set commit identity (`user_name`/`user_email`) if the connector will create commits.

```yaml
connectors:
  repo:
    use: git
    ssh_key_path: /etc/conductor/deploy_key
    strict_host_key_checking: accept-new
    user_name: conductor-bot
    user_email: bot@example.com
```

## Connection

Every field is optional. Credentials/targets are read per-invocation from the
connector instance.

| key | type | purpose |
|-----|------|---------|
| `dir` | string | working directory for every invocation (`cmd.Dir`) |
| `binary` | string | override the `git` binary path (default `git`) |
| `ssh_key` | string | inline SSH private key (PEM); written to a 0600 temp file for the call and removed on return |
| `ssh_key_path` | string | path to an existing SSH private key file (used as-is; takes precedence over `ssh_key`) |
| `strict_host_key_checking` | string | `yes` / `no` / `accept-new` — `ssh -o StrictHostKeyChecking=<value>` |
| `known_hosts` | string | `ssh -o UserKnownHostsFile=<path>` |
| `username` | string | HTTPS credential username; `-c credential.username=<username>` (paired with `token`) |
| `token` | string | HTTPS credential token/password, delivered via a `GIT_ASKPASS` temp script — never placed in argv |
| `user_name` | string | `-c user.name=<value>` |
| `user_email` | string | `-c user.email=<value>` |
| `config` | map | additional `-c key=value` flags (sorted), applied before every subcommand |
| `env` | map | default process environment for every invocation |
| `timeout` | duration | default per-verb timeout (default `10m`); a verb's `timeout` option overrides it |

### Credentials, materialized safely per invocation

The connection can carry SSH and/or HTTPS credentials. They are never baked
into a long-lived environment or config file — each `Invoke` call materializes
what it needs into a temp file, uses it, and removes it before returning
(`defer`), so nothing outlives the call and nothing appears in argv.

**SSH** (`ssh_key` / `ssh_key_path`):

```yaml
connectors:
  repo:
    use: git
    ssh_key: ${secrets.deploy_key}          # inline PEM, written to a 0600 temp file
    strict_host_key_checking: accept-new
    known_hosts: /etc/ssh/known_hosts
```

This sets `GIT_SSH_COMMAND` to
`ssh -o IdentitiesOnly=yes -i <keyfile> -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/etc/ssh/known_hosts`
for that one invocation. `ssh_key_path` skips the temp file and uses the given
path directly.

**HTTPS token** (`username` / `token`):

```yaml
connectors:
  repo:
    use: git
    username: x-access-token
    token: ${secrets.gh_token}
```

The token is written to a `GIT_ASKPASS` helper script (0700 temp file) that
prints it to stdout; `GIT_ASKPASS` and `GIT_TERMINAL_PROMPT=0` are set on the
child process, and `username` (when set) becomes
`-c credential.username=<username>` — so the secret is read by git from a
file descriptor, **never passed as a CLI argument** and never visible in a
process listing.

**Identity and arbitrary config:**

```yaml
connectors:
  repo:
    use: git
    user_name: conductor-bot
    user_email: bot@example.com
    config: { http.sslVerify: "true", core.autocrlf: "false" }
```

`user_name`/`user_email`/`config` become `-c` flags, sorted and placed
**before** the subcommand on every invocation.

## Common output shape

Every verb returns the process result; a **non-zero exit is data, not an
error** — inspect `exit_code`.

| output | type | notes |
|--------|------|-------|
| `stdout` | string | captured stdout |
| `stderr` | string | captured stderr |
| `exit_code` | integer | process exit code (0 = success) |

`status`, `rev_parse`, and `ls_remote` add structured outputs (below),
populated only on `exit_code == 0`.

## Verbs

### `clone` — clone a repository

`url` * (Scope: repo), `dir`, `branch` (`--branch`), `depth` (`--depth`),
`single_branch` (`--single-branch`), `recursive` (`--recursive`), `bare`
(`--bare`).

### `init` — create a new repository

`dir` (default `.`), `bare` (`--bare`).

### `fetch` — download objects and refs from a remote

`remote` (Scope: repo), `refspec`, `all` (`--all`), `prune` (`--prune`),
`tags` (`--tags`), `depth` (`--depth`).

### `pull` — fetch and integrate a remote branch

`remote` (Scope: repo), `branch`, `rebase` (`--rebase`), `ff_only`
(`--ff-only`).

### `push` — update remote refs

`remote` (Scope: repo), `refspec` (or `branch` as an alias), `tags`
(`--tags`), `force` (`--force`), `force_with_lease` (`--force-with-lease`,
takes precedence over `force`), `set_upstream` (`-u`), `delete` (`--delete`).

### `checkout` / `switch`

- `checkout`: `ref` *, `create` (`-b`), `force` (`-f`), `track` (`--track`).
- `switch`: `ref` *, `create` (`-c`).

### `add` — stage file contents

`paths` (list) or `all` (`-A`) — one of the two is required.

### `commit` — record staged changes

`message` (`-m`, required unless `amend`), `all` (`-a`), `allow_empty`
(`--allow-empty`), `amend` (`--amend`), `author` (`--author`).

### `status` — working-tree status

No options. Always runs `git status --porcelain=v1 -b`. Extra outputs:
**`files`** (`[{status, path}]`) and **`branch`** (parsed from the `## ` header
line, tracking info stripped).

### `log` — commit history

`max_count` (`-n`), `oneline` (`--oneline`), `format` (`--format=`), `since`
(`--since=`), `paths` (list, appended after `--`).

### `diff` — show changes

`cached` (`--cached`), `name_only` (`--name-only`), `stat` (`--stat`), `paths`
(list, appended after `--`).

### `branch` — list, create, delete, or rename branches

| option | applies to | effect |
|--------|-----------|--------|
| `subcommand` * | — | `list`, `create`, `delete`, `rename` |
| `name` | create/delete/rename | branch name |
| `new_name` | rename | new branch name |
| `start_point` | create | starting commit/branch |
| `force` | delete | `-D` instead of `-d` |

### `tag` — list, create, or delete tags

`subcommand` * (`list`/`create`/`delete`), `name` (create/delete), `message`
(`-m`, create — makes it annotated), `ref` (create, default `HEAD`), `force`
(`-f`, create).

### `merge` — join two or more development histories

`ref` *, `no_ff` (`--no-ff`), `ff_only` (`--ff-only`), `message` (`-m`).

### `rebase` — reapply commits on top of another base

`upstream` (required unless `subcommand` is set), `onto` (`--onto`),
`subcommand` (`abort` / `continue`).

### `reset`

`mode` (`soft`/`mixed`/`hard` → `--<mode>`), `ref`.

### `rev_parse` — resolve a ref to a SHA

`ref` *. Extra output: **`sha`** (trimmed stdout).

### `ls_remote` — list references of a remote

`remote_or_url` * (Scope: repo). Extra output: **`refs`**
(`[{sha, ref}]`, parsed from the tab-separated output).

### `remote` — manage tracked remotes

`subcommand` * (`add`/`remove`/`set_url`/`list`), `name` (add/remove/set_url),
`url` (add/set_url).

### `config_get` / `config_set`

`config_get`: `key` *. `config_set`: `key` *, `value` *.

### `stash`

`subcommand` (`push`/`pop`/`list`/`drop`/`apply`/`show`, default `push`),
`message` (`-m`, push).

### `clean` — remove untracked files

`force` (`-f`), `dirs` (`-d`), `dry_run` (`-n`, takes precedence over
`force`).

### `show`

`ref`, `format` (`--format=`).

### `cli` — any git subcommand

`args` * (raw argv appended after the binary and `-c` flags). The escape
hatch for anything the first-class verbs don't model.

```yaml
uses: repo.cli
options: { args: [gc, --aggressive] }
```

## Capabilities & security

Declares `Commands: [git, ssh]` and `Spawns: true`; no `Egress`.

- git's own transport (an `https://` remote, or SSH to a `git@host` remote)
  reaches the network on the default, un-isolated path — same reasoning as
  the `docker` connector's `docker_host`. Under OS isolation, a connector
  instance's `network:` can only **narrow** egress.
- Verb options become CLI argv **directly** (`exec.CommandContext`, no
  shell), so there is no shell-quoting/injection surface in how options are
  assembled.
- Secrets never touch argv: an SSH key is written to a private (0600) temp
  file consumed via `GIT_SSH_COMMAND`, and an HTTPS token is written to a
  private (0700) `GIT_ASKPASS` script — both removed before `Invoke` returns.
