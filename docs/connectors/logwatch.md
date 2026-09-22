# `logwatch` connector

Log-line watch as a source: fire a trigger when a **line matches a regexp** in a
followed file or a streaming command's stdout. *"When this line appears, do X."*
The log-line sibling of `fswatch`. Matches are **debounced** so one error's
multi-line burst collapses into a single trigger.

- **Kind:** connector (source only — no verbs)
- **Source:** [`connectors/logwatch/main.go`](../../connectors/logwatch/main.go)
- **Provides:** `logwatch`
- **Capabilities:** none declared — it runs `tail`/your `command:` on the box.

```yaml
connectors:
  logs:
    use: logwatch
    watches:
      app-errors: { path: /var/log/app.log, pattern: 'ERROR (?P<msg>.*)' }
      unit-oom:   { command: [journalctl, -f, -u, myservice, -o, cat], pattern: 'Out of memory' }

triggers:
  - on: logs.app-errors
    steps:
      - id: page
        run: js
        code: "({ summary: inputs.groups.msg })"   # {{.line}}, {{.source}}, {{.groups.<name>}}, {{.watch}}
```

## Watch fields

| field | default | meaning |
|---|---|---|
| `path` | — | a file to follow (via `tail -n0 -F`, survives rotation) |
| `command` | — | a streaming command whose stdout is scanned (set **exactly one** of path/command) |
| `pattern` | (required) | RE2 regexp; a line matching it fires. Named groups `(?P<x>…)` are exposed |
| `debounce` | `2s` | quiet period before firing (collapses a multi-line burst) |

## journald / containers — no cgo backend

There is no journald-specific backend (that would need cgo `sdjournal`). Point a
watch's `command:` at `journalctl -f -u <unit> -o cat` — the same mechanism
covers `docker logs -f`, `kubectl logs -f`, and anything else that streams lines
to stdout. The command is respawned with backoff if it exits, so a source stays
durable.

## Event context

`on: <instance>.<watch>` fires with `{{.watch}}`, `{{.line}}` (the full matched
line), `{{.source}}` (the file path, or `"command"`), and `{{.groups.<name>}}`
for each named capture group.
