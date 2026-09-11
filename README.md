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
- **A refusal is 403 `privacy_tier_unavailable` and the upstream is never
  called.** Retrying will not help; the answer changes only if the tier or the
  routing does.
- **An unrecognised value is refused, not ignored** — `locl`, `eu-only`,
  `unrestricted` all 403 on every seat. A typo must never be served as though it
  were a real posture. Values are case- and whitespace-insensitive.
- The response echoes the tier that was **enforced** (`X-Router-Privacy: zdr`),
  and reqlog records it in `privacy_tolerance` — including on refusals — so
  "what did this script actually get?" is answerable afterwards.

`local` is a stricter tool than `zdr` for a reason: a zero-retention endpoint is
still somebody else's computer. Use `local` for anything that must not leave the
fleet at all.

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
