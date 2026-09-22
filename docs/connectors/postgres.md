# `postgres` connector

PostgreSQL **LISTEN/NOTIFY** as a live source (one event per `NOTIFY`), plus
`query` / `exec` / `notify` verbs. It turns a Postgres database into an event
source: a table trigger that calls `pg_notify()` becomes a conductor trigger, so
you can **react to a row change without polling**. Built on
[jackc/pgx/v5](https://github.com/jackc/pgx) (pure Go).

- **Kind:** connector (verbs **and** source)
- **Source:** [`connectors/postgres/main.go`](../../connectors/postgres/main.go)
- **Provides:** `postgres`
- **Capabilities:** no fixed egress — the database host is yours; narrow it with `network:`.

```yaml
connectors:
  db:
    use: postgres
    url: ${DATABASE_URL}
    listen: ["row_changed"]
    network: ["db.internal:5432"]
```

## Setup

You'll end up with a Postgres connection URL and, for the source, one or more
`NOTIFY` channels.

**Prerequisites:** a reachable PostgreSQL server and a role that can connect (and
`SELECT`/`LISTEN`, plus whatever the verbs you use need).

1. Build the connection URL: `postgres://user:password@host:5432/dbname?sslmode=require`.
   Store it as `DATABASE_URL`.
2. **For the source**, decide the channel name(s) and make something emit them.
   The usual pattern is a table trigger:

   ```sql
   CREATE FUNCTION notify_row_changed() RETURNS trigger AS $$
   BEGIN
     PERFORM pg_notify('row_changed', row_to_json(NEW)::text);
     RETURN NEW;
   END;
   $$ LANGUAGE plpgsql;

   CREATE TRIGGER row_changed AFTER INSERT OR UPDATE ON orders
     FOR EACH ROW EXECUTE FUNCTION notify_row_changed();
   ```

   Then list `row_changed` under `listen:`.
3. Grant the connecting role the privileges it needs (`CONNECT` on the database,
   `SELECT` on the tables you query; `LISTEN` needs no special grant).

**Configure:**

```yaml
connectors:
  db:
    use: postgres
    url: ${DATABASE_URL}
    listen: ["row_changed", "jobs"]
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `url` | string (required) | Postgres URL: `postgres://user:pass@host:5432/db?sslmode=require` |
| `listen` | list | channel names to LISTEN on (**StartSource only**) |

The source holds its own dedicated connection open (LISTEN needs a session),
reconnecting with backoff and re-LISTENing on reconnect. The verbs open a
short-lived connection per call.

## Source events

Trigger with `on: <name>.<event>`. One event:

| event | fires when | context fields (filter / template) |
|-------|-----------|-------------------------------------|
| `notification` | a `NOTIFY` arrives on a LISTENed channel | `channel`, `payload` |

`payload` is the raw `NOTIFY` string (empty when the notify had none) — commonly
a row rendered with `row_to_json(NEW)::text`.

### Filtering

A trigger's `filter:` matches the published fields. A value must match; a list
matches any of its values; prefix `not_` to negate; `expr:`/`not_expr:` take an
expression; a top-level array of objects is OR.

```yaml
triggers:
  - on: db.notification
    filter:
      channels: ["row_changed"]        # any of these channels
    steps:
      - use: jq
        code: '{id: (.payload | fromjson | .id)}'   # payload is text — parse it in a step
```

Channel names in `listen:` must be simple identifiers (a letter or `_`, then
letters/digits/`_`) — the connector rejects anything else, since `LISTEN` takes
an identifier, not a bound parameter.

## Verbs

All verbs open a connection from `url`, run, and close. `sql` uses `$1, $2, …`
placeholders bound from `args` (a list) — never string-concatenate values.

- **`query`** — run a SELECT (or any row-returning statement). `sql`* (with `$1…` placeholders), `args` (list bound to the placeholders). → `rows` (a list of `{column: value}` maps), `row_count`.
- **`exec`** — run a statement that returns no rows (INSERT/UPDATE/DELETE/DDL). `sql`*, `args`. → `rows_affected`, `tag` (the command tag, e.g. `"UPDATE 3"`).
- **`notify`** — send a `NOTIFY` on a channel (via `pg_notify`), fanning a message out to LISTENers. `channel`*, `payload` (optional). → `ok`.

```yaml
steps:
  - uses: db.query
    options:
      sql: "select id, state from orders where state = $1 limit 50"
      args: ["pending"]
```

## Capabilities & security

No fixed egress is declared (the database host is operator-specific) — narrow it
per instance with `network:` (e.g. `["db.internal:5432"]`); it can never be
widened past the declaration. Spawns nothing. Connect with a least-privileged
role, and prefer `sslmode=require` (or `verify-full`) on the `url`.
