# `fswatch` connector

Filesystem watch as a source: fire a trigger when a **matching file settles** in
a watched directory — the low-latency counterpart to a `cron` sweep. Built on
[fsnotify](https://github.com/fsnotify/fsnotify) (inotify/kqueue, pure Go).
Matches are **debounced** so one download's burst of events collapses into a
single trigger; new subdirectories are followed (fsnotify is not recursive).

- **Kind:** connector (source only — no verbs)
- **Source:** [`connectors/fswatch/main.go`](../../connectors/fswatch/main.go)
- **Provides:** `fswatch`
- **Capabilities:** none declared — it only reads directory metadata on the box.

```yaml
connectors:
  files:
    use: fswatch
    watches:
      staged: { path: /srv/incoming, match: "*.m4b", debounce: 15s, recursive: true }

triggers:
  - on: files.staged      # <instance>.<watch>
    steps:
      - id: handle
        run: js
        code: "({ picked: inputs.file })"   # {{.path}} full path, {{.file}} basename, {{.op}}, {{.watch}}
```

## Watch fields

| field | default | meaning |
|---|---|---|
| `path` | (required) | directory to watch |
| `match` | `*` | glob on the file basename |
| `debounce` | `15s` | quiet period before firing (collapses a burst) |
| `recursive` | `true` | follow subdirectories, including ones created later |
| `events` | `create,write,rename` | fsnotify ops to react to (`create`/`write`/`rename`/`remove`/`chmod`) |

## Event context

`on: <instance>.<watch>` fires with `{{.watch}}`, `{{.path}}` (full path),
`{{.file}}` (basename — **not** `{{.name}}`, which is the reserved repo-name
key), and `{{.op}}`.

## Keep a cron sweep alongside it

inotify is lossy — events are missed while the daemon is down, dropped on queue
overflow, and unevenly delivered on some FUSE mounts. Run a periodic `cron`
trigger on the same step as a backstop so a missed event costs latency, not a
file that is never handled.
