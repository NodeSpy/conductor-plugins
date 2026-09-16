# `docker` connector

Drive a local or remote container engine — Docker or Podman — by shelling out to
its CLI. The container lifecycle is exposed as verbs (`run`, `exec`, `build`,
`pull`, `push`, `ps`, `images`, `logs`, `stop`, `start`, `rm`, `inspect`), plus
`compose`, `buildx`, `bake`, and a `cli` escape hatch for any subcommand a
first-class verb does not cover.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/docker/main.go`](../../connectors/docker/main.go)
- **Provides:** `docker`
- **Capabilities:** spawns `docker` / `podman`; no declared egress (talks to the
  local engine socket by default)

```yaml
connectors:
  d: { use: docker }
triggers:
  - on: gh.release
    steps:
      - id: deploy
        uses: d.run
        options: { image: myapp:latest, cmd: [make, deploy], remove: true }
```

## Connection

Every field is optional. Credentials/targets are read per-invocation from the
connector instance.

| key | type | purpose |
|-----|------|---------|
| `engine` | string | `docker` (default) or `podman` |
| `binary` | string | override the CLI binary path (default = engine name) |
| `docker_host` | string | sets `DOCKER_HOST` — `ssh://user@host`, `tcp://host:2376`, `unix:///path.sock` |
| `context` | string | `--context <name>` (uses `~/.docker/contexts`) |
| `tls_verify` | boolean | sets `DOCKER_TLS_VERIFY=1` (TLS to a `tcp://` engine) |
| `cert_path` | string | sets `DOCKER_CERT_PATH` (client certs for a `tcp://` engine) |
| `env` | map | default process environment for every invocation |
| `timeout` | duration | default per-verb timeout (default `10m`); a verb's `timeout` option overrides it |

### Local, remote, and Podman

Remote engines are reached through **docker's own remoting**, not any SSH
re-implementation:

```yaml
connectors:
  local:  { use: docker }                                   # local socket
  ci:     { use: docker, docker_host: ssh://ci@build-box }  # remote over SSH
  swarm:  { use: docker, context: prod }                    # a docker context
  pod:    { use: docker, engine: podman }                   # podman (docker-compatible CLI)
```

> **Podman note:** `compose` and every lifecycle verb work on both engines
> (`podman compose …`). `buildx` and `bake` are **docker-only** — invoking them
> with `engine: podman` returns an error instead of shelling out.

## Common output shape

Every verb returns the process result; a **non-zero exit is data, not an
error** — inspect `exit_code`.

| output | type | notes |
|--------|------|-------|
| `stdout` | string | captured stdout |
| `stderr` | string | captured stderr |
| `exit_code` | integer | process exit code (0 = success) |

Some verbs add structured outputs (below), populated only on `exit_code == 0`.

## Verbs

### `run` — run a container from an image

| option | type | flag |
|--------|------|------|
| `image` * | string | the image (positional) |
| `cmd` | list | argv passed to the container |
| `env` | map | `-e KEY=VALUE` (keys sorted) |
| `volumes` | list | `-v` each |
| `ports` | list | `-p` each |
| `workdir` | string | `-w` |
| `name` | string | `--name` |
| `detach` | boolean | `-d` (background; `container_id` output is the printed id) |
| `remove` | boolean | `--rm` |
| `network` | string | `--network` |
| `entrypoint` | string | `--entrypoint` |
| `user` | string | `--user` |
| `platform` | string | `--platform` |
| `pull` | string | `--pull` (`always`/`missing`/`never`) |
| `extra_args` | list | raw flags inserted before the image |

Extra output: `container_id` (when `detach: true`).

```yaml
uses: d.run
options:
  image: alpine:3
  cmd: [sh, -c, "echo hi"]
  env: { GREETING: hi }
  volumes: ["/src:/work"]
  workdir: /work
  remove: true
```

### `exec` — run a command in a running container

`container` *, `cmd` * (argv), `env` (map), `workdir`, `user`, `detach`.

### `build` — build an image from a context directory

`context` (default `.`), `dockerfile` (`-f`), `tag` (`-t`), `build_args`
(map → `--build-arg`), `target`, `platform`, `pull` (bool), `no_cache` (bool),
`extra_args`. Extra output: `image` (the tag).

### `pull` / `push`

`pull`: `image` *, `platform`. `push`: `image` *.

### `ps` — list containers

`all` (`-a`), `filter` (list → `--filter` each), `limit` (`-n`). Always runs with
`--format '{{json .}}'`; extra output **`containers`** is the parsed list of
objects.

### `images` — list images

`all`, `filter`. Extra output **`images`** (parsed list).

### `logs` — fetch a container's logs

`container` *, `tail`, `since`, `timestamps` (`-t`).

### `stop` / `start` / `rm`

- `stop`: `container` * (name or list), `timeout` (`-t` seconds).
- `start`: `container` * (name or list).
- `rm`: `container` * (name or list), `force` (`-f`), `volumes` (`-v`).

### `inspect` — low-level info

`target` * (name or list), `type` (`container`/`image`/`network`/`volume`).
Extra output **`inspected`** (the parsed JSON array).

### `compose` — `docker compose <subcommand>`

Compose's global flags come **before** the subcommand; the builder emits them
first, then the subcommand, then curated subcommand flags, then `services`.

| option | applies to | effect |
|--------|-----------|--------|
| `subcommand` * | — | up, down, start, stop, restart, ps, logs, build, pull, push, config, run, exec, create, rm, kill, pause, unpause |
| `files` | global | `-f` each |
| `project` | global | `-p` |
| `project_directory` | global | `--project-directory` |
| `env_files` | global | `--env-file` each |
| `profiles` | global | `--profile` each |
| `services` | — | appended last |
| `detach` | up/run/exec | `-d` |
| `build` | up | `--build` |
| `remove_orphans` | up/down | `--remove-orphans` |
| `volumes` | down | `-v` |
| `tail` / `since` / `timestamps` | logs | `--tail` / `--since` / `-t` |
| `no_cache` / `pull` | build | `--no-cache` / `--pull` |
| `json` | ps/config | `--format json`, parsed into `containers` / `config` |
| `extra_args` | — | raw, before services |

```yaml
uses: d.compose
options:
  files: [docker-compose.yml, docker-compose.prod.yml]
  project: web
  subcommand: up
  detach: true
  build: true
# → docker compose -f docker-compose.yml -f docker-compose.prod.yml -p web up -d --build
```

### `buildx` — `docker buildx build` (docker only)

Multi-platform superset of `build`: adds `builder` (`--builder`), `platform`
(list → repeated `--platform`), `push`/`load`, `cache_from`/`cache_to` (lists),
`output`, on top of `dockerfile`, `tag`, `build_args`, `target`, `no_cache`,
`pull`, `context` (default `.`), `extra_args`. Extra output: `image` (the tag).

```yaml
uses: d.buildx
options:
  tag: registry.example.com/app:1.4
  platform: [linux/amd64, linux/arm64]
  push: true
  builder: ci
```

### `bake` — `docker buildx bake` (docker only)

`files` (list → `-f`), `targets` (positional list), `set` (map → `--set K=V`
sorted, or a list of raw `KEY=VALUE`), `push`, `load`, `print` (`--print`),
`builder`, `no_cache`, `pull`, `extra_args`. With `print: true`, the resolved
definition is parsed into the **`metadata`** output.

### `cli` — any engine subcommand

`args` * (raw argv appended after the binary). The escape hatch for anything the
first-class verbs don't model.

```yaml
uses: d.cli
options: { args: [system, prune, -f] }
```

## Capabilities & security

Declares `Commands: [docker, podman]` and `Spawns: true`; no `Egress`.

- Under OS isolation, a `tcp://`/`ssh://` `docker_host` reaches the network — a
  connector's `network:` can only **narrow** egress, so remote-over-network is
  used on the default, un-isolated path.
- Verb options become CLI argv **directly** (`exec.Command`, no shell), so there
  is no shell-quoting/injection surface in how options are assembled. The
  commands you let the engine run are still the engine's full power — scope the
  connector (and the trigger) accordingly.
