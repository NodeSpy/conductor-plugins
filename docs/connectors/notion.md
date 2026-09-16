# `notion` connector

Drive the Notion API: pages, databases, blocks, search, comments, and users as
verbs, plus an `api` escape hatch for any endpoint a first-class verb does not
cover. Built on `net/http` against `https://api.notion.com/v1`.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/notion/main.go`](../../connectors/notion/main.go)
- **Provides:** `notion`
- **Capabilities:** egress to `api.notion.com:443` only

```yaml
connectors:
  nt:
    use: notion
    token: ${NOTION_TOKEN}
triggers:
  - on: gh.release
    steps:
      - uses: nt.create_page
        options:
          database_id: "abc123"
          properties:
            Name: { title: [{ text: { content: "Release {{.tag_name}}" } }] }
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `token` * | string | Notion integration token, sent as `Authorization: Bearer <token>` |
| `version` | string | `Notion-Version` header (default `2022-06-28`) |
| `api_base` | string | override the Notion API base URL (tests, or a private gateway) |

Every request also carries `Content-Type: application/json`. A non-2xx
response is returned as a plugin invocation error carrying the HTTP status and
response body — it never comes back as a successful `result`.

## Structured payloads, passed straight through

Notion's page properties, block children, database property schemas, and
filters are rich, deeply nested JSON. This connector does not attempt to model
every property type in Go — options like `properties`, `children`, `filter`,
and `sorts` accept the caller's map/list **as-is** and send it as (part of) the
request body, so the full Notion API surface stays reachable. Consult
[Notion's API reference](https://developers.notion.com/reference) for the
exact shape of each.

A few verbs additionally offer an ergonomic **shortcut** for the common case:

- **`parent`** — pass the full Notion object (`{"database_id": "..."}` /
  `{"page_id": "..."}`), or use the `database_id` / `page_id` option directly
  as a bare string and it is wrapped for you.
- **`title`** (on `create_database` / `update_database`) — pass a plain
  string and it is wrapped into a one-run `rich_text` title array; a list is
  assumed to already be one and passed through unchanged.
- **`rich_text`** (on `create_comment`) — pass a plain string and it is
  wrapped into `[{"text": {"content": "..."}}]`; a list is assumed to already
  be one.

## Outputs

Every verb returns:

| output | type | notes |
|--------|------|-------|
| `result` | any | the full decoded JSON response body |
| `results` | list | present when the response is a Notion paginated list (`query_database`, `get_block_children`, `search`, `list_users`) — the same array as `result.results`, hoisted for convenience |
| `status_code` | integer | the HTTP status code |

## Verbs

### `create_page` — create a page in a database or under a parent page

| option | type | notes |
|--------|------|-------|
| `parent` * | any | `{database_id: ...}` / `{page_id: ...}`, or a bare id string (see shortcuts) |
| `database_id` | string | shortcut: wraps into `parent {"database_id": ...}` |
| `page_id` | string | shortcut: wraps into `parent {"page_id": ...}` |
| `properties` * | map | page property values, passed straight through |
| `children` | list | block objects to seed the page with |
| `icon` | any | page icon object |
| `cover` | any | page cover object |

`parent` is required unless `database_id` or `page_id` is given instead.

```yaml
uses: nt.create_page
options:
  database_id: "abc123"
  properties:
    Name: { title: [{ text: { content: "Ship it" } }] }
    Status: { status: { name: "Todo" } }
```

### `update_page` — edit a page's properties or archive it

`page_id` *, `properties` (map), `archived` (bool — `true` moves the page to
trash, `false` restores it).

### `get_page` — read a page's properties

`page_id` *.

### `get_database` — read a database's schema

`database_id` * (title, properties, parent).

### `query_database` — query a database's rows

`database_id` *, `filter` (map), `sorts` (list), `page_size`, `start_cursor`.
Exposes `results`.

```yaml
uses: nt.query_database
options:
  database_id: "abc123"
  filter: { property: "Status", status: { equals: "Todo" } }
```

### `create_database` — create a database under a parent page

`parent` * (a `{page_id: ...}` object, or a bare page_id string), `title` (a
plain string, or a rich_text list), `properties` * (the property schema).

### `update_database` — edit a database's title and/or schema

`database_id` *, `title`, `properties`.

### `append_blocks` — append child blocks to a page or block

`block_id` * (a page id or block id — both accept children), `children` *
(list of block objects).

### `get_block_children` — list a block's (or page's) direct children

`block_id` *, `page_size`, `start_cursor`. Exposes `results`.

### `delete_block` — delete (archive) a block

`block_id` *.

### `search` — search pages and databases shared with the integration

`query` (empty returns everything shared with the integration), `filter`
(map, e.g. `{property: "object", value: "page"}`), `sort` (map), `page_size`.
Exposes `results`.

### `create_comment` — add a comment, or reply in an existing discussion

`parent` (a `{page_id: ...}` object, or a bare page_id string) **or**
`discussion_id` (reply to an existing discussion), `rich_text` * (a plain
string, or a rich_text list).

```yaml
uses: nt.create_comment
options: { page_id: "abc123", rich_text: "Deployed in {{.tag_name}}" }
```

### `get_user` / `list_users`

`get_user`: `user_id` *. `list_users`: `page_size`, `start_cursor`. `list_users`
exposes `results`.

### `api` — call any Notion API endpoint

`method` * (`GET`/`POST`/`PATCH`/`DELETE`), `path` * (relative to `/v1`, e.g.
`"pages/abc123/restore"`), `query` (map), `body` (any). The escape hatch for
anything the first-class verbs don't model.

```yaml
uses: nt.api
options: { method: POST, path: "pages/abc123/restore", body: { archived: false } }
```

## Capabilities & security

Declares egress to `api.notion.com:443` only; no `Commands`, no `Spawns`.
Narrow it further per instance with `network:`; it can never be widened past
the declaration. Scope the integration token to the least-privileged
workspace/pages it actually needs to act on.
