# llm-router-go — Migration Plan

Phased rewrite of the Python LLM Router stack
(`~/Projects/erewhon/llm-router/`) into Go, for operational simplicity,
reliability, and reduced supply-chain attack surface.

This document captures the **decisions** and **plan**, not the
**motivation**, which lives in the Forge task
`Rewrite LLM Router stack in Go (node agent, tool proxy, LiteLLM replacement)`.

## Decisions (2026-05-22)

| Question                         | Decision                                                |
| -------------------------------- | ------------------------------------------------------- |
| Scope                            | All three: node agent + tool proxy + router             |
| Repo                             | New repo: `github.com/erewhon/llm-router-go`            |
| Router implementation            | Write focused custom (~2–3K lines) — not Bifrost        |
| License                          | AGPL-3.0-or-later                                       |
| Module path                      | `github.com/erewhon/llm-router-go`                      |
| Go version                       | 1.23 (minimum); built with whatever local toolchain     |
| Source of truth for model config | `models.yaml` (shared with Python repo during migration) |

## Package layout

```
llm-router-go/
├── cmd/
│   ├── node-agent/       # binary: per-machine backend manager (replaces FastAPI)
│   ├── tool-proxy/       # binary: SOCKS5 tool fan-out + auto-router
│   └── router/           # binary: OpenAI-compatible front door (replaces LiteLLM)
├── internal/
│   ├── config/           # models.yaml -> typed Go structs (port of Pydantic models)
│   ├── nodeagent/        # node-agent logic
│   │   └── backends/     # SGLang/vLLM/llama.cpp/lmstudio drivers
│   ├── toolproxy/        # tool-proxy logic
│   │   └── tools/        # web_search, fetch_url, calculator
│   ├── router/           # router logic (alias resolution, routing, SSE pass-through)
│   ├── metrics/          # Prometheus text-format parsing + reexport
│   ├── httpx/            # shared HTTP middleware (logging, request id, timeouts)
│   └── logx/             # structured logger setup
├── deploy/
│   ├── systemd/          # *.service unit files (one per binary, per host class)
│   └── scripts/          # rsync + restart shims
├── docs/                 # this PLAN, design notes, runbooks
├── go.mod
├── justfile
├── README.md
└── LICENSE
```

`internal/` is used (not `pkg/`) because none of this is intended for
external import — keeps the import surface honest.

## Phasing

### Phase 0 — Foundations (this week)

- [x] Scaffold repo, justfile, license, plan
- [ ] Wire CI (GitHub Actions): `go build ./... && go test ./...` on amd64 + arm64
- [x] Add `internal/config/` — port `models.yaml` Pydantic schema to Go structs
      with `gopkg.in/yaml.v3`. Tests cover defaults, validation, routing
      helpers, and a load of the real `~/Projects/erewhon/llm-router/models.yaml`.
- [ ] Add `internal/logx/` — `slog` setup with JSON output + level flag.
- [ ] Add `internal/httpx/` — request-id middleware, structured access log,
      graceful shutdown helper. Used by all three binaries.

### Phase 1 — node-agent (1–2 weeks)

Goal: drop-in replacement for the FastAPI node agent, deployed
**alongside** the Python agent on one node (euclid) first for shadow comparison.

- [ ] HTTP server (chi or stdlib `http.ServeMux` — start with stdlib).
- [ ] `/health` — process uptime, build version.
- [ ] `/status` — same JSON shape as Python `/status` (catalog of backends,
      per-backend state, GPU residency).
- [ ] `/metrics` — Prometheus re-export, scraping SGLang/vLLM `/metrics`
      on the same host.
- [ ] Backend drivers under `internal/nodeagent/backends/`:
  - [ ] `sglang` — Docker container lifecycle via `os/exec` (`docker ps`,
        `docker inspect`, `docker logs`); state via HTTP probe to
        `:5391/v1/models` + `/metrics`. Match on `--served-model-name`
        equals `hf_repo` (see `feedback_nodeagent_served_model_match.md`).
  - [ ] `llamacpp` — systemd unit management via `coreos/go-systemd` D-Bus.
  - [ ] `vllm` — same pattern as sglang.
  - [ ] `lmstudio` — HTTP-only probe.
- [ ] GPU/system metrics (`internal/nodeagent/gpu`): nvidia-smi parsing
      on Sparks, intel_gpu_top / sysfs on euclid Arc, AMD on delphi.
- [ ] systemd unit for node-agent itself.
- [ ] Shadow deploy on euclid: run on `:8101`, dashboard compares both.

**Cutover criterion**: 24h shadow run with `/status` JSON matching the
Python agent's output (modulo timestamps) within tolerance, on all four
machines.

### Phase 2 — tool-proxy (2–3 weeks)

Goal: replace the FastAPI tool proxy at `euclid:5392`.

- [ ] `net/http/httputil.ReverseProxy` for SSE pass-through (with
      `FlushInterval: -1`).
- [ ] `golang.org/x/net/proxy` for SOCKS5 dialing through
      `svc-sys-research-vpn:1080`.
- [ ] Tool registry under `internal/toolproxy/tools/`:
  - [ ] `web_search` (DuckDuckGo via VPN-SOCKS5).
  - [ ] `fetch_url` (also through VPN).
  - [ ] `calculator`.
  - [ ] `tavily` (paid fallback, direct net).
- [ ] Auto-router: classify incoming request → route to model tier.
      Reuses embeddings on OpenArc (`euclid:5404`, qwen3-embedding-4b).
      Must respect `active_aliases` — skip categories whose alias maps
      to a disabled model (the 2026-05-10 behavior).
- [ ] Reasoning-token passthrough (delta forwarding for `<think>` /
      reasoning fields).
- [ ] Forward chat completion to correct backend keyed on `model_id`
      from the registry (the disambiguation fix vs. shared `hf_repo`).

**Cutover criterion**: tool-proxy traffic dual-routed for 24h, with
matching tool-call invocations and search results (logged).

### Phase 3 — router (2–3 weeks)

Goal: retire LiteLLM. The Go router reads `models.yaml` directly,
no intermediate `generate_config` step needed.

- [ ] OpenAI-compatible endpoints: `/v1/chat/completions`,
      `/v1/completions`, `/v1/embeddings`, `/v1/rerank`, `/v1/models`.
- [ ] Alias resolution (e.g. `coder` → `qwen3.6-hypatia`).
- [ ] `tool_proxy: true` routing — forward to `192.168.42.240:5392`
      with model_id preserved.
- [ ] SSE pass-through (same `ReverseProxy` pattern as tool-proxy).
- [ ] Request logging to Postgres (reuse the LiteLLM Postgres schema or
      define a fresh, simpler one — decide during this phase).
- [ ] Mode tags (`mode:big`, `mode:default`) — load-time filtering of
      which models are active.
- [ ] Health-check endpoints for the dashboard.
- [ ] `/.well-known/opencode` endpoint (replace the
      `opencode-wellknown` systemd unit at `:4012`).

**Cutover criterion**: parallel run on `:4015` for 48h, dashboard +
opencode clients pointed at it, no observed regressions.

## Cross-cutting concerns

### Deploy

- Cross-compile on euclid (amd64) using
  `GOOS=linux GOARCH=arm64 go build` for archimedes/hypatia.
- Per-host systemd units, drop binary into `/usr/local/bin/`.
- `deploy/scripts/sync.sh` per binary: build → rsync → `systemctl restart`.
- No venv, no `uv sync`, no Python version pin.

### Config

- `models.yaml` stays where it is during the migration; both Python and
  Go binaries read it.
- Path is a CLI flag (default `/etc/llm-router/models.yaml`), with a
  `--models-yaml-url` later option if we want to serve it from one node.

### Observability

- All binaries emit `slog` JSON to stdout (captured by journald).
- All binaries expose `/metrics` in Prometheus format.
- Request IDs propagated via `X-Request-ID` (generate if absent).

### Testing

- Unit tests next to the code (`_test.go`).
- `internal/config` gets golden-file tests against the real
  `models.yaml`.
- Reverse-proxy SSE behavior covered with an in-process httptest server.
- Integration tests skipped by default, run with `-tags=integration`
  against a real SGLang on euclid.

## Open questions / TODO

- [ ] **CI**: GitHub Actions config — first PR after scaffold lands.
- [ ] **Postgres schema**: keep LiteLLM's, or define fresh?
- [ ] **Bifrost re-evaluation**: revisit after Phase 1 if `internal/router`
      starts approaching 3K lines.
- [ ] **Auto-router embedding cache**: currently lives in tool-proxy
      memory; consider per-process startup time impact.
- [ ] **opencode .well-known**: confirm Go router can serve the same
      schema as the Python generator (`reference_opencode_wellknown.md`).
- [ ] **Push initial scaffold commit to `github.com/erewhon/llm-router-go`**
      after user review.
