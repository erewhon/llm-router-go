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

- [x] `internal/nodeagent/gpu`: one `Reader` per vendor, dispatch by the
      registry's `GpuType`. NVIDIA tries `nvidia-smi --query-gpu` and
      falls back to `/proc/meminfo` when memory is reported as `[N/A]`
      (GB10 unified memory). AMD reads
      `/sys/class/drm/card*/device/mem_info_vram_{total,used}` and
      `gpu_busy_percent` directly — no subprocesses. Intel parses
      `xpu-smi stats`/`discovery` and uses the registry's per-node
      `vram_gb` as a fallback when discovery is silent.
- [x] Wired into `/health` via `nodeagent.WithGPUReader`. Same field
      names + JSON shape as the Python agent.
- [x] Validated across all three vendors:
      archimedes (nvidia/unified) total exact / free off by 6 MB
      (snapshot timing); delphi (amd/sysfs) exact match; euclid
      (intel/xpu-smi+fallback) total via fallback, free matches the
      xpu-smi figure. The Go path also responds in ms where the
      Python agent on euclid currently hangs on `/health` — Python is
      stuck in a slow `xpu-smi` subprocess.

#### Phase 1d — deploy

- [x] systemd unit (`deploy/systemd/llm-router-go-agent.service`):
      same sandbox profile as the Python agent (ProtectSystem=strict,
      ProtectHome=tmpfs, SystemCallFilter, RestrictAddressFamilies,
      IPAddressAllow LAN-only) but with a much smaller bind-mount
      surface — only models.yaml; no .venv, no src tree.
- [x] `deploy/scripts/deploy-node-agent.sh`: detects target arch over
      ssh, cross-compiles statically (`CGO_ENABLED=0`), rsyncs the
      binary + unit, installs via sudo, daemon-reloads. Optional
      `--start`.
- [x] **Shadow deploy on archimedes** (`:8101`, version `a6316b2`):
      `/health` and `/models` match the Python agent for every field
      the Go agent populates in 1b — same `running_models`, same
      `total_requests`, same per-model states. Map iteration order
      differs (not part of the contract) and `avg_tok_per_s` is
      omitted instead of `null` (semantically equivalent).
- [x] Memory footprint: Go agent ~4.5 MB resident vs Python's ~217 MB
      (48× less).
- [ ] Shadow deploy on hypatia, delphi, euclid (same script).
- [ ] Enable on boot once parity is observed for 24h on each node:
      `sudo systemctl enable llm-router-go-agent`.

**Cutover criterion**: 24h shadow run with `/health` + `/models` JSON
matching the Python agent's output (modulo timestamps) within tolerance,
on all four machines.

### Phase 2 — tool-proxy

Goal: replace the FastAPI tool proxy at `euclid:5392`.

#### Phase 2a — chat-completions reverse-proxy (done)

- [x] `internal/toolproxy/`: `httputil.ReverseProxy` with `Rewrite`
      hook + `FlushInterval=-1` for SSE pass-through.
- [x] Model resolution: registry key, `hf_repo` (with `#suffix`
      stripped), and aliases all match. Strips the `openai/` prefix
      LiteLLM sometimes prepends. Rewrites the body's `model` field
      to the upstream's expected name before forwarding.
- [x] External-backed models 404 with a clear error (they're not in
      the tool-proxy path).
- [x] `/v1/models` (OpenAI shape, surfaces `api_class` per entry).
- [x] `/health`.
- [x] `cmd/tool-proxy/main.go` — flags, registry load, signal-driven
      graceful shutdown via httpx.ServeContext.
- [x] Smoke-tested live: POST through the Go proxy to Nemotron-3-Super
      on archimedes:5391 returned 200 + correct id/model in ~1.5s.

#### Phase 2b — tool execution

- [x] `internal/toolproxy/tools/` package: Tool struct (name, OpenAI
      function description, JSON-schema parameters, Run hook) and
      Registry (registration with stable order, name/has/execute
      lookups, JSON-shape Definitions()) ready to wire into the proxy.
- [x] `calculator` tool — gval-backed safe expression evaluator with
      the same function set the Python tool exposes (sqrt/sin/cos/tan/
      asin/acos/atan/log/log10/log2/exp/abs/round/ceil/floor/factorial)
      and constants (pi/e/tau/inf). Factorial rejects negatives, non-
      integers, and arguments > 170 (float64 overflow).
- [x] `NewHTTPClient` builds the shared `*http.Client` used by every
      network-touching tool. Accepts bare `host:port`,
      `socks5://host:port`, or `socks5h://host:port` (treated the same
      because Go's `proxy.SOCKS5` always asks the proxy to resolve the
      target hostname — the curl `socks5h` behaviour the Mullvad
      container needs). Validates host + numeric port at construction
      so misconfig fails fast.
- [x] `fetch_url` — HTTP GET via the shared client, content-type gate
      (text/json/xml only), `golang.org/x/net/html` walker for clean
      text extraction (skips script/style/noscript/template, treats
      block elements as newline boundaries, collapses inline whitespace
      including source-code newlines so the model gets browser-rendered
      structure). Body capped at 4MiB read, output truncated at the
      same `MaxFetchChars=8000` the Python tool uses.
- [x] `web_search` — POST `q=…` to `html.duckduckgo.com/html/`, parse
      result blocks by climbing from each `a.result__a` to the
      enclosing `.result*` container, then extracting title/snippet/url
      with DDG's `/l/?uddg=…` redirect unwrapped. Hand-rolled selectors
      in `x/net/html` avoid a goquery dep. Caps at `MaxSearchResults=5`.
      33 tests across registry/calculator/http_client/fetch_url/
      web_search cover defaults, error paths, edge cases, and fixture-
      based parsing.
- [x] `tavily_search` — POST to api.tavily.com with the apiKey, query,
      search_depth (basic/advanced, default basic), `include_answer` +
      `max_results=5`. Renders `AI Summary: …` + a list of
      `title (relevance: N) / content / url`. Errors surface upstream's
      `detail`/`message`/`error` JSON fields when present, falling back
      to a truncated raw body. The tool requires a non-empty apiKey;
      callers gate registration on `os.Getenv("TAVILY_API_KEY") != ""`.
- [ ] Wire Registry into Proxy via `WithTools(...)`.
- [ ] Stream interception: detect assistant `tool_calls` in the
      response stream, run tools, continue the chat with results.

#### Phase 2c — auto-router

- [ ] Classify incoming request via embeddings on OpenArc
      (`euclid:5404`, qwen3-embedding-4b) → route to model tier.
- [ ] Skip categories whose alias maps to a disabled model
      (the 2026-05-10 behaviour collected from `_model_registry`).

#### Phase 2d — reasoning passthrough

- [ ] Forward `<think>` / `reasoning_content` deltas to clients that
      ask for them.

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
