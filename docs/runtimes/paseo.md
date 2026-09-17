# `paseo` runtime

The paseo runtime as a plugin: it exposes, as verbs, exactly the paseo-daemon
operations conductor's `internal/dispatch.Backend` needs — launching an agent
turn, listing/inspecting agents, archiving agents/workspaces, creating
worktrees/workspaces, cloning a repo, sending a follow-up, and waiting for an
agent to go idle — each implemented by shelling to the `paseo` CLI against
paseo's own long-lived, multi-agent daemon.

- **Kind:** runtime (`kind: runtime`)
- **Source:** [`runtimes/paseo/main.go`](../../runtimes/paseo/main.go)
- **Provides:** `paseo`
- **Capabilities:** spawns `paseo` (egress is paseo's own business — it talks to
  its daemon, not the network, from this plugin's point of view)
- **Bundled in conductor?** Yes — additive and opt-in. Conductor's `rpcBackend`
  drives it over the ordinary `plugin.invoke` RPC.

```yaml
runtimes:
  gpu: { use: paseo }
```

> **Why a connector-shaped verb plugin, not an ACP `kind: runtime`?** paseo's
> daemon-wide dedup/reaper/hold semantics have no ACP equivalent — an ACP
> subprocess *is* one session, and paseo is the opposite: a stateless CLI client
> against a long-lived daemon. So this is a verb plugin invoked via
> `plugin.invoke`, not a new wire protocol. (Declared `kind: runtime` so
> conductor wires it as a runtime.)

## Setup

You'll end up with the `paseo` CLI available where conductor runs, so it can
drive agents through paseo's long-lived daemon.

**Prerequisites:** the **paseo** CLI installed and on `PATH` (or at a known
path), with its daemon reachable. This runtime is a thin client that shells to
`paseo` — it starts no daemon of its own.

1. Install the paseo CLI on the box conductor runs on (per paseo's own install
   docs), so `paseo --version` works from conductor's shell.
2. Confirm the daemon is reachable — e.g. `paseo ls` succeeds. The verbs here
   (`run`, `list_agents`, `inspect`, `send`, `wait`, …) are exactly that CLI.
3. If `paseo` isn't on `PATH`, point the runtime at the binary with `paseo_bin`.

**Configure:**

```yaml
runtimes:
  gpu:
    use: paseo
    # paseo_bin: /opt/paseo/bin/paseo   # only if paseo isn't on PATH
```

No credentials live here — authenticating whatever models or agents paseo
launches is paseo's own configuration, not this plugin's.

## Connection

| key | type | purpose |
|-----|------|---------|
| `paseo_bin` | string | override the `paseo` binary path (default `paseo`; tests point this at a stub) |

## Verbs

Each verb is a `paseo …` shell-out (the command it runs is shown). `*` marks a
required option. This is the same operation set the CLI-direct backend drives —
an alternate, opt-in transport, not a different feature set. Conductor drives
these itself for agent dispatch; you rarely call them by hand.

### Agents

- **`run`** — launch (or re-launch) a coding-agent turn (`paseo run <args…>`). `args`* (list) — the full `paseo run` argument list (everything after `run` itself). → `output`, `agentId`.
- **`send`** — queue a follow-up prompt to a live agent (`paseo send <id> <prompt>`). `id`*, `prompt`*, `json` (ask for JSON output). → `output`.
- **`wait`** — block until an agent goes idle (`paseo wait <id>`). `id`*.
- **`inspect`** — inspect one agent (`paseo inspect <id> --json`). `id`*. → `cwd`, `lastUsage`, `updatedAt`, `createdAt`, `pendingPermissions` (outstanding permission prompts — empty means it isn't waiting on the user).
- **`list_agents`** — list non-archived agents (`paseo ls --json`). `labels` (map — exact-match label filters). → `agents`.
- **`archive_agent`** — soft-delete one agent (`paseo archive <id>`). `id`*.

### Workspaces

- **`create_worktree`** — create an isolated PR/branch worktree workspace (`paseo workspace create --mode …`). `isolation`*, `path`*, `strategy`* (`checkout-pr` | `branch-off`), `prNumber` (with `checkout-pr`), `forge`, `newBranch` (with `branch-off`), `baseRef`. → `workspaceId`, `cwd`.
- **`create_workspace`** — create a plain (non-worktree) workspace (`paseo workspace create --isolation …`). `isolation`*, `path`*, `title`. → `workspaceId`.
- **`list_workspaces`** — list every workspace (`paseo workspace ls --json`). → `workspaces`.
- **`archive_workspace`** — soft-delete a workspace, reclaiming any worktree it owns (`paseo workspace archive <id>`). `id`*.
- **`clone`** — clone a repo and register it with paseo (`paseo clone`). `repo`*, `dir`*, `protocol` (`https` | `ssh`).
