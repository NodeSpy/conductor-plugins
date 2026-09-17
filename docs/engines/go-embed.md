# `go-embed` engine

Code steps written in real Go, interpreted by
[yaegi](https://github.com/traefik/yaegi) rather than compiled — for when a
`code:` step needs actual control flow and static types but installing the Go
toolchain on every conductor host is a bridge too far.

- **Kind:** step engine (`use: go-embed` on a `steps:` entry — an engine has
  no top-level block of its own; `run:` is the same key for configs written
  before the rename)
- **Provides:** `go-embed`
- **Sandbox:** a narrow stdlib import allowlist — yaegi can only resolve
  packages it's been given, so anything off the list (`os`, `os/exec`,
  `net`, `net/http`, `io`, `reflect`, `unsafe`, …) simply fails to `import`

## Use it

```yaml
steps:
  - id: greet
    use: go-embed
    code: |
      import "conductor/store"

      func run(ctx map[string]any) (any, error) {
        name, _ := ctx["name"].(string)
        if name == "" {
          name = "world"
        }

        cache, err := store.Use("cache")
        if err != nil {
          return nil, err
        }
        if err := cache.Set("greetings", name, "hello"); err != nil {
          return nil, err
        }

        return map[string]any{
          "greeting": "Hello, " + name + "!",
        }, nil
      }
```

## Contract

`ctx` is the step's inputs, passed as `map[string]any` — the same JSON-shaped
data every engine receives.

`code:` must define exactly one of these two shapes for `run`, checked by
reflection before it's ever called (a mismatch is a clear contract error
naming both accepted shapes, not a `reflect` panic):

```go
func run(ctx map[string]any) (any, error)
func run(ctx map[string]any) any
```

Top-level declarations in `code:` run as ordinary code too (global
initializers execute), not just `func`/`var` declarations.

The value `run` returns decides the step's outputs:

- `nil` (or the error is non-nil) — a non-nil `error` fails the step;
  `nil, nil` produces no outputs
- a `map[string]any` — becomes the step's named outputs, verbatim
- anything else (a number, a string, a slice) — becomes the step's single
  `value` output

## Sandbox

yaegi has no notion of a *forbidden* package — it can only resolve an
`import` it has been explicitly given source or `Use()`-registered binary
symbols for. This engine registers only a fixed allowlist of stdlib import
paths: `bytes`, `encoding/json`, `encoding/base64`, `errors`, `fmt`, `math`,
`math/rand`, `net/url`, `path`, `regexp`, `sort`, `strconv`, `strings`,
`time`, `unicode`, `unicode/utf8` — general-purpose data-shaping packages,
nothing that touches the outside world. Anything else, including `os`,
`os/exec`, `net`, `net/http`, `io`, `reflect` and `unsafe`, is never
registered, so an `import` of it fails with yaegi's ordinary "unable to find
source related to" error. The interpreter's `GoPath` is also pinned to a
path that never exists, so a source-based import can never pull real files
off the host — the allowlist is the entire boundary.

The three ctx faces keep their in-binary spelling as virtual packages, each
reaching conductor's data plane one host round trip per call:

- `import "conductor/store"` — `st, err := store.Use("cache")` returns a
  handle with `Get`, `Set`, `SetNX`, `Merge`, `Delete`, `Incr`, `Append`,
  `Remove`, `Contains`, `List`, `First`, `Last`, `Index`, `Slice`, `Len`,
  `Pop`
- `import "conductor/sql"` — `db, err := sql.Use("analytics")` returns a
  handle with `Query(query string, args []any) ([]any, error)` and
  `Exec(query string, args []any) (map[string]any, error)`
- `import "conductor/memory"` — package-level `memory.Remember(text, tags,
  scope)`, `memory.Recall(query)`, `memory.Forget(id)`, `memory.List()`

`store.Use`/`sql.Use` always return a handle (there's no local registry to
consult for an eager name check); an undefined store surfaces conductor's
own "no such store" error on the first op instead. Every op is authorized
against the step's own data-plane guard, same as an in-binary
`run: go-embed` step — a snippet written for one compiles and runs unchanged
on the other.

## Examples

Reading a nested field and returning a list:

```yaml
steps:
  - id: emails
    use: go-embed
    code: |
      func run(ctx map[string]any) any {
        payload, _ := ctx["payload"].(map[string]any)
        users, _ := payload["users"].([]any)

        var emails []string
        for _, u := range users {
          if m, ok := u.(map[string]any); ok {
            if email, ok := m["email"].(string); ok && email != "" {
              emails = append(emails, email)
            }
          }
        }
        return emails
      }
```

Querying a SQL store and returning named outputs:

```yaml
steps:
  - id: recent
    use: go-embed
    code: |
      import "conductor/sql"

      func run(ctx map[string]any) (any, error) {
        db, err := sql.Use("analytics")
        if err != nil {
          return nil, err
        }
        rows, err := db.Query(
          "SELECT id, title FROM incidents WHERE severity = ?", []any{"high"})
        if err != nil {
          return nil, err
        }
        return map[string]any{"count": len(rows), "rows": rows}, nil
      }
```
