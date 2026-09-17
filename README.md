# llm-router-go

Go rewrite of the LLM Router stack (node agent, tool proxy, LiteLLM-style router).

Replaces three Python services from
[`llm-router`](https://github.com/erewhon/llm-router) (the Python repo
currently in production) with single-binary Go equivalents.

## Status

**In production.** All three binaries are implemented; the Go `router` replaced
the LiteLLM proxy in a hard cutover and runs the fleet's OpenAI-compatible front
door. See [`docs/PLAN.md`](docs/PLAN.md) for the migration history and decisions.

## Run it yourself

The `router` is self-contained (no database, tool proxy, or node agents
required) and works against Amazon Bedrock, LM Studio, or any OpenAI-compatible
backend:

```sh
brew tap erewhon/tap
brew install llm-router
```

See [`docs/running-on-macos.md`](docs/running-on-macos.md) and the annotated
[`configs/models.example.yaml`](configs/models.example.yaml) for configuring
backends and running in the foreground.

### Request logging

Every request is logged (one row each, including ones rejected before an
upstream call). By default this goes to a local **SQLite** file — no server to
run — at `$XDG_STATE_HOME/llm-router/requests.db` (override with
`--sqlite-path`). Point `--postgres-dsn` at a Postgres instance to use that
instead (it takes precedence), or pass `--reqlog=off` to disable logging
entirely. Query the SQLite log with any tool: `sqlite3 requests.db 'select
model, status, latency_ms from router_requests order by id desc limit 20'`.

Two columns cover **where a cloud request actually went**, which nothing else
records. `upstream_provider` is the operator the upstream itself named
(OpenRouter reports `provider`: "Amazon Bedrock", "Novita", "DeepInfra"), and
`privacy_tolerance` is the retention posture the router enforced (`zdr`, or NULL
when none was asked for). Both are NULL for local backends. They matter because
the router picks a *model* while the provider picks the *endpoint*, from a pool
that changes between requests — so `resolved_via` alone cannot tell you where a
given prompt was served. Together they answer the audit question:

```sh
sqlite3 requests.db "select resolved_via, upstream_provider, count(*)
  from router_requests where privacy_tolerance = 'zdr' group by 1,2"
```

A refused request (403, upstream never called) records the tolerance with a NULL
provider, so the refusal is queryable too rather than leaving no trace.

`upstream_cost_usd` is what the provider actually billed (OpenRouter's
`usage.cost`) rather than a token-rate estimate — it already accounts for the
cache discount and for which endpoint served the request. `cached_prompt_tokens`
is the part of the prompt that hit the provider's cache. Read together they say
what prefix caching is worth: the same 1650-token prompt measured **~3x cheaper
cached than uncached**, and providers of the same model differ in whether they
cache at all.

```sh
sqlite3 requests.db "select upstream_provider,
    sum(cached_prompt_tokens)*1.0/sum(prompt_tokens) as hit_rate,
    round(sum(upstream_cost_usd), 4) as usd
  from router_requests where upstream_cost_usd is not null group by 1"
```

Both providers the fleet uses key cache stickiness off a caller-supplied header
— OpenRouter's `x-session-id`, OpenCode Zen's `x-opencode-session`. The router
forwards inbound headers untouched, so a client that sets one keeps its warm
cache through the proxy.

### Privacy tiers per request (`X-Router-Privacy`)

A caller can demand where its prompt is allowed to go, per request, with one
header. Three values, strictest last:

| value | admits | on a seat that does not qualify |
| --- | --- | --- |
| `any` (or omit the header) | anything the role/model allows | — |
| `zdr` | local seats, **plus** cloud endpoints the router can hold to zero data retention for this request (OpenRouter today — `provider: {"zdr": true}` goes on the wire) | 403 |
| `local` | fleet hardware only; nothing leaves the building | 403 |

```sh
curl https://llm.bcc.sh/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "X-Router-Privacy: local" \
  -d '{"model":"coder","messages":[...]}'
```

Rules worth knowing before wiring a script's `--privacy` flag to it:

- **It works on roles and on directly named models.** A named model has no role
  contract behind it, so the header is the only thing that can constrain it.
- **A caller can tighten, never loosen.** If the role requires
  `locality: local_or_zdr`, sending `any` still gets the ZDR directive; sending
  `local` gets the stricter `local`. The stricter of the role's and the caller's
  tier always governs.
- **The tier is a filter on the role's candidate walk, not a verdict on its
  first choice.** A role listing an OpenRouter seat ahead of a local one still
  serves a `local` request — from the local one. Overflow entries are filtered
  too, so `X-Router-Overflow: true` cannot happen under `local`.
- **Three distinct answers when a role comes up short**, because they demand
  different reactions:

  | status | meaning | retry? |
  | --- | --- | --- |
  | `403` `privacy_tier_unavailable` | candidates exist, the tier excluded every one, none was merely down | never — your policy did this |
  | `503` | a compliant candidate exists but is down right now | yes |
  | `503` (mixed) | some excluded by policy, the compliant rest down — the exclusions are still listed | yes |
  | `404` | the name does not exist | no |

  A 403 body carries `excluded_candidates`, one reason per seat (`or/glm:
  not on fleet hardware (privacy: local)`), so a program can see what a looser
  tier would have reached. The upstream is never called for a 403.
- **Omitting the header is `any`.** Nothing is enforced and nothing is echoed;
  a response `X-Router-Privacy` header only appears when a tier actually
  applied (the caller's, or a `local_or_zdr` role's `zdr`).
- Refusals are their own outcome, not errors: `error_class = privacy_refused`
  in reqlog, and `router_privacy_refusals_total{tier,subject}` in metrics —
  they never count against a model's upstream failure rate, since nothing was
  sent.
- **An unrecognised value is refused, not ignored** — `locl`, `eu-only`,
  `unrestricted` all 403 on every seat. A typo must never be served as though it
  were a real posture. Values are case- and whitespace-insensitive.
- The response echoes the tier that was **enforced** (`X-Router-Privacy: zdr`),
  and reqlog records it in `privacy_tolerance` — including on refusals — so
  "what did this script actually get?" is answerable afterwards.

`local` is a stricter tool than `zdr` for a reason: a zero-retention endpoint is
still somebody else's computer. Use `local` for anything that must not leave the
fleet at all.

### Per-model request defaults (`request_defaults`)

An entry can fill in request-body fields the caller left unset — sampling, a
per-request thinking budget, `chat_template_kwargs`, anything the engine
accepts in the JSON body:

```yaml
models:
  glm-5.3-flash-spark:
    hf_repo: glm-5.3-flash
    backend: vllm
    multi_node: {nodes: [archimedes, hypatia], head_node: archimedes}
    aliases: [glm-fast, glm-think]
    request_defaults:                # every request via this entry
      temperature: 1.0
      top_p: 0.95
      chat_template_kwargs: {enable_thinking: false}
    alias_overrides:
      glm-think:                     # ...except these, when named as glm-think
        request_defaults:
          thinking_token_budget: 3000
          chat_template_kwargs: {enable_thinking: true}
```

The merge is **fill-only**: a field the caller sent always wins, at every
nesting level — objects merge key by key (a caller's
`chat_template_kwargs: {reasoning_effort: low}` keeps the entry's
`enable_thinking`), and any other collision keeps the caller's value.
Precedence when both exist is alias override, then model. An alias override's
older `chat_template_kwargs:` key is shorthand for
`request_defaults: {chat_template_kwargs: ...}`.

Defaults are applied per forwarding attempt for the seat actually being tried,
so a role that fails over from one seat to another sends each seat its own
defaults, never the first seat's. Role candidates are registry keys, so a role
that wants the thinking profile lists a second entry pointing at the same
server rather than an alias. `model`, `messages`, `stream`, `prompt` and
`input` are refused at load. The forwarding log line names the top-level keys
each request picked up (`request_defaults=[top_p ...]`). The tool proxy applies
the same defaults for callers that reach it directly; behind the router the
second application is a no-op.

### Live inventory and discovery

`/v1/models` and the OpenCode well-known are rendered from what each upstream
**says it serves right now**, not from `models.yaml` alone. On its own interval
(60 s, `--inventory-interval`) the router fetches `/v1/models` from every
distinct upstream base — each local seat's engine, each external `api_base` —
concurrently, with a 5 s timeout per fetch, off the request path. Two things
come out of a listing:

- **Drift.** A hand-written entry whose served name (`hf_repo`) is not in its
  base's live listing is marked `absent`: not routable, dropped from
  `/v1/models` and the well-known with its aliases, and shown on the dashboard
  with the reason (`"Qwen/Qwen3.5-122B" is not in the live listing at
  http://archimedes:5391 (it serves: Qwen/Qwen3-Coder-Next-FP8)`). Roles and
  chains skip it and say so in their 503; it returns the moment the listing
  does. A base whose fetch fails keeps its last-known listing — stale is better
  than empty — and the age and error are visible on `/health` under
  `inventory`.
- **Discovery.** Providers named in a `discovery:` block have their listings
  adopted under a prefix, by policy:

  ```yaml
  discovery:
    - api_base: https://opencode.ai/zen/v1     # Zen: adopt everything
      prefix: zen/
      api_key: OPENCODE_ZEN_API_KEY
      adopt: all
      tags: [zen]
    - api_base: https://openrouter.ai/api/v1   # OpenRouter: allowlist only
      prefix: or/
      api_key: OPENROUTER_API_KEY
      adopt: ["anthropic/claude-*", "deepseek/*", "z-ai/glm-5*"]
      exclude: ["*:batch", "*-exp"]            # vetoes within the allowlist; * crosses slashes
      tags: [openrouter]
  ```

  A discovered id becomes a virtual external entry `<prefix><id>` — routable
  directly by name, listed with `"discovered": true`, priced and sized from the
  provider's metadata when it offers any (OpenRouter does; Zen does not, so a
  source can set `context_length` / `max_output_tokens` defaults) — and is
  **never joined to a role or chain**: seat decisions stay explicit in
  `models.yaml`. A hand-written entry always wins over discovery of the same
  id, and so does one that already routes to the same provider id at that base
  under a different name (`or/claude-opus-5` for `anthropic/claude-opus-5`),
  enabled or not. A discovered id the provider stops listing is retired after
  two consecutive misses. Every privacy tier and token scope applies to a
  discovered entry exactly as to a hand-written external — a `models:local`
  token cannot reach one — and reqlog rows served by one carry `discovered =
  true`.

Per-entry opt-out: `health: {inventory: false}` (default on for chat,
embeddings and rerank entries; media classes and the Anthropic passthrough are
never checked). Fleet-wide: `--inventory=false`. Before a config push,
`--validate --validate-live` fetches every listing once and reports
`not-listed` (a hand-written entry the provider no longer serves),
`inventory-unreachable`, and — informational, never promoted to a failure —
`discovered-model` for each id a source would adopt that nobody has written
down, i.e. the candidates worth promoting into a chain or role.

### Anthropic gateway (measurement tap)

With an `api_class: anthropic` entry in `models.yaml` (see Scenario 8 in the
example config), the router exposes `/v1/messages` as a **transparent
passthrough** to `api.anthropic.com`. Point Claude Code at it —
`ANTHROPIC_BASE_URL=http://router:4010` — and every request is logged with the
prompt-cache token splits (`cache_creation_input_tokens` /
`cache_read_input_tokens`) and a prefix hash chain for diagnosing cache
divergence, while the body is forwarded byte-for-byte. The client's own
credentials pass through untouched (the router injects none and does not gate
`/v1/messages` behind `--api-keys`), so Claude Code Max keeps its subscription
billing.

## Binaries

| Binary       | Replaces (Python)                              | Listens on |
| ------------ | ---------------------------------------------- | ---------- |
| `node-agent` | `src/llm_router/node_agent/` (FastAPI)         | `:8100`    |
| `tool-proxy` | `src/llm_router/tool_proxy/` (FastAPI)         | `:5392`    |
| `router`     | LiteLLM proxy + `generate_config.py`           | `:4010`    |

## Build

```sh
just build              # amd64 native binaries into ./bin/
just build-arm64        # cross-compile for the Sparks (archimedes, hypatia)
just test
just fmt
just lint               # requires golangci-lint
```

## License

AGPL-3.0-or-later. See [`LICENSE`](LICENSE).
