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
