# llm-router-go

Go rewrite of the LLM Router stack (node agent, tool proxy, LiteLLM-style router).

Replaces three Python services from
[`llm-router`](https://github.com/erewhon/llm-router) (the Python repo
currently in production) with single-binary Go equivalents.

## Status

**Pre-alpha — scaffold only.** No service is implemented yet. The Python
stack at `~/Projects/erewhon/llm-router/` remains the production system.

See [`docs/PLAN.md`](docs/PLAN.md) for the phased migration plan and decisions.

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
