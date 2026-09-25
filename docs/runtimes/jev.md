# `jev` runtime

A **decision runtime** for TypeSafe's Jev, a System One model: it answers
conductor's [`decide:` steps](https://github.com/NodeSpy/conductor/blob/main/docs/wiki/Decide-Steps.md)
— yes/no (`noul`), `choice` and `score` questions — with calibrated
probabilities, through TypeSafe's `system_one/v1` API. It never runs an agent:
conductor sends it decide steps only, and an agent step can never resolve to it.

- **Kind:** runtime (`kind: runtime`), declaring the `system_one/v1` decision protocol
- **Source:** [`runtimes/jev/main.go`](../../runtimes/jev/main.go)
- **Provides:** `jev`
- **Capabilities:** egress `api.typesafe.ai:443`; spawns nothing
- **Requires:** a conductor release with `decide:` steps

```yaml
runtimes:
  paseo: { default: true }
  jev:
    connection:
      api_key: env:JEV_API_KEY

packs:
  review:
    source: github.com/NodeSpy/conductor-packs//pr-review-team
    models:
      light: ["jev-*", "claude-sonnet-*"]    # this pack's light-tier decide steps try Jev first
```

**Setup:**

1. Get an API key from the TypeSafe console.
2. Put it where conductor resolves secrets — `conductor.env` (`JEV_API_KEY=…`)
   for an `env:` reference, or a vault.
3. Add the runtime, then run `conductor init` to install it.
4. Name Jev's models in the tier a decide step uses (`jev-*`). Installing the
   runtime alone changes nothing: a decide step reaches Jev only when its tier
   lists Jev's models.

**What leaves the box:** a decide step's `state` and `questions` are sent to
TypeSafe's API. Keep a pack's tier off `jev-*` if its decisions carry content
that must not.

**Fallback:** when Jev errors or times out, the step moves to the next
candidate in its tier — in the example above, the light-tier agent model
answering through conductor's adapter — and then to the step's `default:`.

## Connection

| key | type | purpose |
|-----|------|---------|
| `api_key` | string, **required** | TypeSafe API key, sent as `Authorization: Bearer` |
| `api_base` | string | override the API origin (default `https://api.typesafe.ai`; tests point this at a fake server) |
| `model` | string | the model a request names when conductor passes none (default `jev-latest`) |

## Verbs

Conductor calls these itself; a workflow uses a `decide:` step rather than
invoking them.

- **`decide`** — `POST /v1/systemone`. Options: `protocol`* (`system_one/v1`),
  `model`, `state`*, `questions`*. → `answers` (the v1 answers, unchanged —
  conductor validates each against the questions asked), `model` (the model that
  answered), `usage`.
- **`models`** — `GET /v1/models`. → `models`: `[{id, name, released}]`, the
  roster fleets resolve against.

Errors carry the HTTP status and at most 200 characters of the response body;
the API key is redacted from every error.
