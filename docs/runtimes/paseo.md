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

## Connection

| key | type | purpose |
|-----|------|---------|
| `paseo_bin` | string | override the `paseo` binary path (default `paseo`; tests point this at a stub) |

## Verbs

| verb | shells to | key options | outputs |
|------|-----------|-------------|---------|
| `run` | `paseo run <args…>` | `args` * (list) | `output`, `agentId` |
| `list_agents` | `paseo ls --json [--label k=v …]` | `labels` (map) | `agents` (list) |
| `inspect` | `paseo inspect <id> --json` | `id` * | `cwd`, `lastUsage`, `updatedAt`, `createdAt`, `pendingPermissions` |
| `archive_agent` | `paseo archive <id>` | `id` * | — |
| `archive_workspace` | `paseo workspace archive <id>` | `id` * | — |
| `create_worktree` | `paseo workspace create --mode …` | `isolation` *, `path` *, `strategy` * (`checkout-pr`/`branch-off`), `prNumber`, `forge`, `newBranch`, `baseRef` | `workspaceId`, `cwd` |
| `create_workspace` | `paseo workspace create --isolation …` | `isolation` *, `path` *, `title` | `workspaceId` |
| `list_workspaces` | `paseo workspace ls --json` | — | `workspaces` (list) |
| `clone` | `paseo clone` | `repo` *, `dir` *, `protocol` | — |
| `send` | `paseo send <id> <prompt> [--json]` | `id` *, `prompt` *, `json` | `output` |
| `wait` | `paseo wait <id>` | `id` * | — |

`*` required. This is the same operation set the CLI-direct backend drives — an
alternate, opt-in transport, not a different feature set.
