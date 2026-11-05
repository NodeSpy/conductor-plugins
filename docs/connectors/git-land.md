# `git-land` connector

Land commits made in another repository — an agent's workspace — on a remote
branch that **moved since they were made**: keep them under a ref, replay them
onto the branch's new head without a working tree or hooks, and push the
result fast-forward only. When they don't apply, replay names the commit and
the paths that conflict, and the work stays at the kept ref.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/git-land/`](../../connectors/git-land/)
- **Provides:** `git-land`
- **Capabilities:** spawns `git` / `ssh`; no declared egress (the remote URL
  or SSH host is reached by git's own transport)
- **Since:** 1.0.0

It is a connector of its own rather than verbs on [`git`](git.md) because
conductor bundles a connector type named `git` (a sandboxed agent's
`git.push`), and a plugin cannot provide a bundled type.

```yaml
connectors:
  land:
    use: git-land
    dir: /var/lib/conductor/land.git       # its own repository, never a workspace
    ssh_key_path: /etc/conductor/deploy_key
    strict_host_key_checking: accept-new
triggers:
  - on: github.pull_request.synchronize
    steps:
      - id: kept
        uses: land.keep
        options: { from: "{{.fix.workdir}}", ref: "refs/kept/pr-{{.pr}}" }
      - id: replayed
        uses: land.replay
        options:
          head: "{{.kept.ref}}"
          base: "{{.fix.push.base}}"        # where the refused workspace started
          remote: "git@github.com:{{.repo}}.git"
          branch: "{{.branch}}"
      - id: landed
        if: "replayed.clean && replayed.commits > 0"
        uses: land.push
        options: { remote: "git@github.com:{{.repo}}.git", rev: "{{.replayed.head}}", branch: "{{.branch}}" }
```

The [pr-autopilot pack](https://github.com/NodeSpy/conductor-packs/tree/main/pr-autopilot)
uses it for a PR branch that moved while its fixer agent worked.

## Setup

**Prerequisites:** `git` ≥ 2.38 on PATH (`merge-tree --write-tree`; override
with `binary`); `ssh` on PATH for SSH remotes.

1. Pick a directory the daemon owns for `dir`. It is created as a bare
   repository on the first `keep`; kept refs accumulate there, so prune them
   (`git for-each-ref` + `update-ref -d`) on whatever schedule suits you.
2. Give it push credentials for the remote, as for the [`git`](git.md#setup)
   connector: an SSH deploy key with write access (`ssh_key_path` /
   `ssh_key`), or an HTTPS token (`token`, with `username`).
3. To sign the commits `replay` writes, set `sign: true` and the signing key
   through `config`, e.g. `config: { gpg.format: ssh, user.signingkey: /etc/conductor/signing_key }`.

## Connection

| key | type | purpose |
|-----|------|---------|
| `dir` | string | **required** — the repository commits are kept and replayed in; created bare when missing |
| `binary` | string | override the `git` binary path (default `git`) |
| `ssh_key` | string | inline SSH private key (PEM); written to a 0600 temp file for the call and removed on return |
| `ssh_key_path` | string | path to an existing SSH private key file (takes precedence over `ssh_key`) |
| `strict_host_key_checking` | string | `yes` / `no` / `accept-new` |
| `known_hosts` | string | `ssh -o UserKnownHostsFile=<path>` |
| `username` | string | HTTPS credential username (paired with `token`) |
| `token` | string | HTTPS credential token, delivered via a `GIT_ASKPASS` temp script — never placed in argv |
| `sign` | boolean | sign the commits `replay` writes (`git commit-tree -S`, with the key set in `config`) |
| `config` | map | additional `-c key=value` flags (sorted), applied before every subcommand |
| `env` | map | default process environment for every invocation |
| `timeout` | duration | default per-verb timeout (default `10m`); a verb's `timeout` option overrides it |

Every verb also returns `stdout`, `stderr` and `exit_code`.

## Verbs

### `keep` — copy a revision out of a workspace under a ref

| option | | |
|---|---|---|
| `from` | required | path of the repository or worktree holding the commits |
| `rev` | | the revision there (default `HEAD`) |
| `ref` | required | the ref to keep it under in `dir`, `refs/…` |

Outputs `sha` (the kept commit) and `ref`. A fetch from `from`: the only git
that runs there is `upload-pack`; nothing in `dir` moves but `ref`.

### `replay` — re-create base..head on another base

| option | | |
|---|---|---|
| `head` | required | the tip of the commits to replay (a kept ref) |
| `base` | | the commit they were made on (exclusive), an ancestor of `head`; default the merge base of `head` and the new base |
| `onto` | | the new base, a revision in `dir` — or: |
| `remote` + `branch` | | fetch `branch` from `remote` and replay onto its head |

| output | |
|---|---|
| `onto` | the new base |
| `moved` | the new base is not an ancestor of `head`: pushing `head` would not fast-forward. Only then is anything replayed |
| `base` | the base the commits were replayed from |
| `clean` | every commit replayed without a conflict |
| `head` | the replayed tip (when clean); `onto` itself when it already holds every change |
| `commits` | commits replayed |
| `dropped` | commits whose change the new base already holds |
| `conflicts` | the paths the conflicting commit conflicts on |
| `conflict_commit` | the commit that conflicts (or the merge that stopped it) |
| `reason` | why it stopped when that is not a conflict: a merge commit in the range |

Pass `base` when you know it. The merge-base default is right while the branch
only gains commits; after a force-push it is older than where the work
started, and the commits between are replayed too.

Each commit's own change (against its parent) is merged onto the running tip
with `git merge-tree --write-tree`, in memory: no working tree, no index, no
hooks. Authors, committers, dates and messages are kept. Only a linear run of
commits is replayed.

### `push` — fast-forward a remote branch to a commit

| option | | |
|---|---|---|
| `remote` | required | the remote URL |
| `rev` | required | the commit to push (a replayed `head`) |
| `branch` | required | the remote branch to update |

Outputs `pushed`, and `rejected` when the remote refused it. There is no force
option and a `rev` starting with `+` is refused: a branch that moved again
since the replay is a rejection, never an overwrite. `--no-verify`.

## Capabilities & security

Declares `Commands: [git, ssh]` and `Spawns: true`; no `Egress`.

- Every git command runs in `dir` with `core.hooksPath=/dev/null`; nothing
  runs with a working tree. The repository the commits came from is only read,
  through `upload-pack`, so its config and hooks never execute.
- Options become argv directly (`exec.CommandContext`, no shell); options
  that could read as flags (a leading `-`) are refused, and refs pass
  `git check-ref-format`.
- `remote` is a repo-scoped option: take it from trusted configuration (a
  pack setting), not from anything the agent wrote.
- Secrets never touch argv, as in the [`git`](git.md#capabilities--security) connector.
