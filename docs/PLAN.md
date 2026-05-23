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
- [x] Wire CI (GitHub Actions): `go vet`/`go test -race` on amd64, build-only
      cross-compile check on arm64.
- [x] Add `internal/config/` — port `models.yaml` Pydantic schema to Go structs
      with `gopkg.in/yaml.v3`. Tests cover defaults, validation, routing
      helpers, and a load of the real `~/Projects/erewhon/llm-router/models.yaml`.
- [x] Add `internal/logx/` — `slog` JSON/text logger, parsable level, and a
      `ContextAttrFunc` hook so other packages (httpx) can attach
      context-scoped attrs without logx importing them.
- [x] Add `internal/httpx/` — request-id middleware (read or generate,
      capped + echoed), structured access log, panic recovery, graceful
      shutdown helper. `responseWriter` preserves `http.Flusher` so SSE
      pass-through stays intact through the middleware chain.

### Phase 1 — node-agent

Goal: drop-in replacement for the FastAPI node agent, deployed
**alongside** the Python agent on one node (euclid) first for shadow comparison.

#### Phase 1a — HTTP skeleton (done)

- [x] HTTP server using stdlib `net/http` (Go 1.22 method-prefixed mux).
- [x] `/health` — node name, disk free/total, services list, and the
      registry's set of external-managed models reported as RUNNING.
      GPU + non-external state arrive in 1b.
- [x] `/models` — JSON list matching the Python `ModelListEntry` shape:
      every enabled model assigned to this node (single or multi-node);
      state = `running` for external, `stopped` for everything else
      until 1b.
- [x] `/models/{model_id}/status` — 200 with state/backend/hf_repo for
      known on-node models, 404 otherwise.
- [x] `/metrics` — Prometheus reexport over its own registry
      (`node_agent_build_info`, `node_agent_uptime_seconds`,
      `node_agent_models_enabled`, plus Go + process collectors).
      Upstream SGLang/vLLM scraping is 1b.
- [x] `cmd/node-agent/main.go` — flags for addr, models-yaml, node,
      log-level, log-format, shutdown-timeout; version stamped via
      `-ldflags` from `justfile`.
- [x] Smoke-tested end-to-end against the real
      `~/Projects/erewhon/llm-router/models.yaml` from `euclid`.

#### Phase 1b — backend probing

- [x] **Backend interface** at `internal/nodeagent/backends/` and
      functional-options `nodeagent.WithBackend(...)` so the agent can
      accept driver registrations from outside the package.
- [x] **`sglang` driver** (`internal/nodeagent/backends/sglang/`):
      HTTP probe of `/v1/models` for reachability + served-model match
      (with `#suffix` stripping), `/metrics` scrape for running/waiting
      request counts, total requests, and tok/s (prefers SGLang's
      `gen_throughput` gauge, falls back to the inter-token-latency
      histogram math vLLM exposes). Handles both bare and `sglang:`
      prefixed metric names — current SGLang uses the prefix; the
      Python agent currently misses these.
- [x] Replace the 1a stub `initialState()` with real probing in `/models`,
      `/models/{id}/status`, and `/health.running_models`.
- [x] Smoke-tested on both Sparks: archimedes returns nemotron-3-super
      `running` + qwen3.5-122b `stopped`; hypatia returns
      qwen3.6-hypatia `running` (fixing the false-error the Python
      agent reports because it checks systemd while the actual service
      runs in Docker).
- [ ] Backend start/stop lifecycle (Docker for SGLang, systemd for vLLM).
      Defer until status-only shadow comparison passes.
- [ ] `llamacpp` driver — systemd unit management via `coreos/go-systemd`
      D-Bus. Needed for UI-TARS on delphi.
- [ ] `lmstudio` driver — HTTP-only probe.
- [ ] Re-expose upstream `/metrics` (proxied or merged) on the agent's
      `/metrics` for Prometheus scrape.

#### Phase 1c — GPU/system metrics

- [ ] `internal/nodeagent/gpu`: nvidia-smi parsing on Sparks,
      intel_gpu_top / sysfs on euclid Arc, AMD on delphi.
- [ ] Wire GPU fields (`gpu_type`, `total_vram_gb`, `free_vram_gb`,
      `gpu_busy_pct`) into `/health` responses.

#### Phase 1d — deploy

- [ ] systemd unit for the node-agent binary.
- [ ] `deploy/scripts/sync-node-agent.sh` — rsync + restart.
- [ ] Shadow deploy on euclid: run on `:8101`, dashboard compares both.

**Cutover criterion**: 24h shadow run with `/health` + `/models` JSON
matching the Python agent's output (modulo timestamps) within tolerance,
on all four machines.

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
