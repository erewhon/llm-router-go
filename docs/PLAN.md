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
- [x] **Shadow deploy on hypatia** (`:8101`, version `18b3da3`, 2026-05-29):
      `/health` exact match on total_vram/disk/gpu_busy/running_models/
      gpu_type; free_vram differs by ~12MB (snapshot timing). `/models`
      perfect match — both report 2 models with identical states and
      identical `total_requests=1828` (both agents scrape the same SGLang).
      Known stub: services entries show `reachable:false` for ComfyUI vs
      Python's `true` (the Go agent's service-probe code is marked "later
      phase"). Memory: ~14MB resident vs Python's ~187MB (~13× less). 24h
      watch in progress before enable-on-boot.
- [ ] Shadow deploy on delphi, euclid (same script). Delphi gated on the
      llamacpp driver (UI-TARS is a systemd unit, not Docker).
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
- [x] **2b.iv** — `Registry` wired into `Proxy` via `WithTools(...)`;
      `cmd/tool-proxy` registers calculator + web_search + fetch_url (+
      tavily_search when `--tavily-key`/`TAVILY_API_KEY` is set), all sharing
      one SOCKS5-capable `http.Client` (`--proxy`).
- [x] **2b.iv** — tool-execution loop. `handleChat` branches on
      `shouldInjectTools` (registry non-empty AND model not `nothink`-tagged):
      no → the 2a reverse-proxy passthrough, unchanged; yes → inject proxy tool
      defs (merged with client tools, proxy names win) and run the loop. Per
      round: call the backend **non-streaming**, `extractToolCalls` (native
      `tool_calls` + `<tool_call>`-tag fallback), split proxy- vs client-owned.
      Client-owned → hand the whole batch back (`finish_reason: tool_calls`, no
      execution); all proxy-owned → execute, append `tool` results, repeat;
      none → done. Capped at `--max-tool-rounds` (5; `--backend-timeout`
      600s/call). Streaming: the loop runs non-streaming, then the final answer
      is **re-streamed** from the backend for true token-by-token output — the
      redundant generation the Python proxy also pays; chosen over
      buffer-and-replay for streaming parity, revisit as a post-cutover
      optimization. Backend `reasoning_content` is forwarded; `<think>`-tag
      extraction is deferred to 2d, and the Python `FALLBACK_ROUTES` table
      (unscheduled in any phase) is still TODO. 19 new tests (loop +
      extraction) pass under `-race`; the empty-registry 2a tests are unchanged.

#### Phase 2c — auto-router

- [x] **2c** — `internal/toolproxy/autorouter.go`. `handleChat` intercepts
      `auto`/`auto-free`/`auto-full` (after stripping `openai/`) *before*
      `resolveModel`, since those are external stubs pointing back at the proxy.
      `Classify` extracts the last user message (multimodal image → straight to
      `vision`), truncates to 500 runes, embeds it on OpenArc
      (`--embed-url` euclid:5404, qwen3-embedding-4b, **single-input** — the
      batched path is broken), and cosine-matches against the pre-computed
      category embeddings (coder / coder-fim / thinker / research / vision,
      descriptions verbatim from Python; deterministic order so ties favour the
      first). Complexity upgrades on the coding path: `auto-free` →
      `coder-hard` (≥0.45), `auto-full` → `claude-opus-4-6` (≥0.70). Any
      failure (uninitialised, no user msg, embed error) falls back to `coder`.
- [x] **2c** — disabled-alias skipping. `main` collects active aliases
      (enabled model ids + their aliases) and passes them to `NewAutoRouter`;
      `Initialize` embeds only categories whose alias is active, so the router
      can't pick an alias whose model is disabled (the 2026-05-10 behaviour).
- [x] **2c** — redirect + wiring. The chosen alias is written back to the
      body and the request is reverse-proxied through LiteLLM (`--litellm-url`
      euclid:4010, `--litellm-key`/`LITELLM_KEY`), which resolves it (the alias
      may be an external model + LiteLLM applies mode filtering). `reverseProxyTo`
      relays SSE or JSON unchanged, so stream/non-stream need no special-casing.
      Embeddings use a **direct** LAN client (not the web tools' SOCKS5 VPN
      client); category embeddings compute in a background goroutine with
      backoff retry (`RunInit`) so a slow/down embedder never blocks startup.
      Decision logging via slog (replacing Python's JSONL file). 13 new tests
      pass under `-race`.

#### Phase 2d — reasoning passthrough

- [x] **2d** — reasoning passthrough. Structured `reasoning_content` /
      `reasoning` deltas already reach the client untouched via the relay
      (`reverseProxyTo` forwards the backend's SSE verbatim — that's what the
      reasoning-parser models like Nemotron-3-Super emit). This phase adds the
      inline-`<think>` path on the **non-streaming** loop: `extractThinking`
      splits `<think>…</think>` out of the assistant content, preferring the
      backend's structured reasoning and falling back to the tag text. The tag
      is stripped from the returned content, from client-call/max-rounds
      responses, and from the assistant turns recorded in history (so the model
      doesn't re-read its own thinking). Port of `extraction.extract_thinking`.
      5 new tests.
- Deferred (YAGNI): the **streaming** `<think>` reframe (Python's
  `ThinkingStreamParser`). The streamed final answer is relayed raw, so a model
  that inlined `<think>` mid-stream would leak tags into `content`. No current
  fleet model does this on a streamed path — tool-loop models use a
  reasoning-parser (→ structured deltas, already forwarded); passthrough is
  nothink (→ no reasoning). The stateful chunk-boundary parser is left unbuilt
  until a model that inlines thinking mid-stream joins the fleet.

**Cutover criterion**: tool-proxy traffic dual-routed for 24h, with
matching tool-call invocations and search results (logged).

### Phase 3 — router (2–3 weeks)

Goal: retire LiteLLM. The Go router reads `models.yaml` directly,
no intermediate `generate_config` step needed.

#### Phase 3a — skeleton (done)

- [x] `internal/router/`: route table over `models.yaml`. `resolveModel`
      matches an incoming name by registry key (exact match wins, deterministic),
      then hf_repo (bare or with `#suffix`), then alias; a leading `openai/`
      prefix is stripped. Only models active in the configured mode are routable.
- [x] Front-door routing via `config.APIBase(id, aliasOverride)` (the tool proxy
      forces tool_proxy *off* to avoid looping back; the router uses the model's
      real setting): `tool_proxy:true` → tool proxy at `192.168.42.240:5392`
      with the **model_id** in the body (not hf_repo, so the proxy can
      disambiguate shared repos); `external` → its `api_base` with the resolved
      `api_key`; local → the node backend with the bare hf_repo. Per-alias
      `alias_overrides.tool_proxy` is honoured.
- [x] `api_key` resolution: literal when it starts with `sk-`, otherwise an
      env-var name (matches `generate_config.py`'s `os.environ/` logic).
      Injectable (`WithGetenv`) for tests.
- [x] `POST /v1/chat/completions` — reverse-proxy with SSE intact. `SetURL`
      joins the inbound path, so it generalises to the other endpoint families
      in 3b without hardcoding paths.
- [x] `GET /v1/models` (OpenAI shape, surfaces `owned_by` + `api_class`) and
      `GET /health` (status, mode, model count).
- [x] `cmd/router/main.go` — flags (addr default `:4015`, models-yaml, mode,
      log-level/format, shutdown-timeout), registry load, standard httpx
      middleware chain, signal-driven graceful shutdown. 18 tests (resolve
      table + handlers + SSE) pass under `-race`.
- [x] Smoke-tested live against the real `models.yaml`: `/v1/models` listed all
      28 enabled models; `coder` routed to the tool proxy as model_id
      `qwen3.5-122b-a10b`, `research` as `nemotron-3-super`, both returning 200
      from the real backends.

#### Phase 3b.i — remaining OpenAI endpoints (done)

- [x] `handleChat` refactored into a generic `handleProxy(requireClass, forceDirect)`
      factory; `Handler()` registers `/v1/chat/completions`, `/v1/completions`,
      `/v1/embeddings`, and `/v1/rerank` against it.
- [x] Per-endpoint **api_class enforcement**: requests with a mismatched
      `api_class` get 400 with a class-mismatch message; the upstream is not hit.
- [x] **Tool-proxy bypass** for `/v1/completions`, `/v1/embeddings`, `/v1/rerank`
      via a `forceDirect` override that beats both the model's `tool_proxy` flag
      and any per-alias override (the tool proxy serves only chat-completions).
- [x] 7 new tests (resolve forceDirect + api_class surfacing + 5 handler tests)
      bring the package to 25 tests; full suite `-race` green.
- [x] Smoke-tested live: `/v1/embeddings` (qwen3-embedding on OpenArc) returned a
      2560-dim vector; `/v1/rerank` (qwen3-reranker) returned scored docs
      (bypassing LiteLLM's "Unsupported provider" bug on rerank); class-mismatch
      returns 400 both ways; `/v1/completions` for `research`
      (a `tool_proxy:true` model) correctly bypassed the tool proxy and went
      direct to archimedes:5391 with the bare hf_repo.

#### Phase 3b.ii — request logging to Postgres (done)

- [x] New `internal/router/reqlog/` package: `Sink` interface +
      `NopSink` (default) + `MemorySink` (tests) + `PostgresSink` (async writer
      goroutine + buffered channel, drop-on-full so a stalled DB never blocks
      the proxy path; idempotent `Close` via `sync.Once`; password-redacting
      DSN helper for safe logging).
- [x] Fresh, simpler schema (DECIDED 2026-05-26, **not** LiteLLM's), bootstrap
      on startup (`CREATE TABLE IF NOT EXISTS` + 3 indexes on ts/request_id/
      model). One row per request: request_id, ts, method/path, model (in +
      backend), backend_url, resolved_via, api_class, via_tool_proxy, stream,
      status, latency_ms, prompt/completion/total tokens, error.
- [x] Capture hook in `handleProxy`: every request that reaches the handler
      emits one Record on exit via a `defer` — including rejected ones (bad
      JSON, missing model, unknown model, api_class mismatch). Status comes
      from a `recordingWriter` wrapper; the resolution result fills the
      backend fields; the error message is captured for non-2xx paths.
- [x] Token usage extraction: for non-streaming JSON responses,
      `ReverseProxy.ModifyResponse` buffers the (small) body and `parseUsage`
      reads the OpenAI-shape `usage` block. For SSE responses, a rolling 64KB
      `streamTailCapture` wraps `resp.Body`; `extractSSEUsage` scans the tail
      for the final `data:` event containing `usage` (e.g. an OpenAI/SGLang
      chunk before `[DONE]`). Memory-bounded regardless of total stream
      length.
- [x] `--postgres-dsn` flag — off (NopSink) when empty; on construction the
      sink pings the DB and bootstraps the schema, failing fast on connection
      errors. Logger sees `dsn` with the password redacted.
- [x] 7 new tests at the router layer (chat-usage / embeddings-tokens /
      class-mismatch / unknown-model / bad-JSON / SSE-tail-usage / nil-sink)
      plus reqlog unit tests (Nop/Memory/RedactDSN) and an env-gated Postgres
      integration test (`ROUTER_REQLOG_PG_DSN=...`). All `-race` green.
- [x] Smoke-tested end-to-end against a throwaway Postgres container: real
      chat to Qwen3.5-122B logged 575/8/583 tokens; embeddings logged
      prompt=0 with completion NULL (correctly distinguished); rerank logged
      0/NULL/0; class-mismatch logged status=400 with the error message.

**Production target (decided 2026-06-07):** a dedicated `llm-router-postgres`
container on euclid (`postgres:17-alpine`, `127.0.0.1:5433`, volume
`llm-router-pgdata`, `--restart unless-stopped`). Picked over an Incus VM
(Incus isn't installed on euclid) and over reusing `litellm-postgres` (we're
retiring LiteLLM). The DSN flows via `ROUTER_PG_DSN` in `proxy.env`, assembled
by `deploy-router-secrets.sh` from `ho secret llm-router/reqlog-pg-password`.

#### Phase 3b.iii — observability + dashboard (done)

- [x] `/metrics` (Prometheus) — per-binary registry mirroring the node-agent's
      pattern. Go + process collectors plus router-specific metrics:
      `router_build_info{version}` gauge; `router_uptime_seconds` GaugeFunc;
      `router_models_active{api_class}` set at load-time from the active set;
      `router_requests_total{path,model,api_class,status}` counter
      (`model` = `resolved_via`, falling back to `"unresolved"` to bound
      cardinality at 404); `router_request_duration_seconds{path,api_class}`
      histogram (5ms–120s buckets); `router_upstream_tokens_total{kind,model,
      api_class}` counter populated from `parseUsage` / `extractSSEUsage`.
- [x] Observe is called from the same `handleProxy` defer as `Sink.Log`, so the
      Prometheus counters and the Postgres rows never disagree.
- [x] `/health` enriched: now includes `version`, `mode`, `uptime_seconds`,
      `models`, `models_by_class`, `streaming` — enough for the dashboard to
      pick up build identity and per-class counts without a second call.
- [x] `--mode` surfaced via `/health.mode`; reload story stays "restart the
      binary" — the active set is computed at construction, matching the
      Python proxy's load-time behaviour (documented in the Router struct doc).
- [x] 3 new tests (rich /health fields, /metrics serves Prometheus with
      expected lines, requests + tokens counted) bring the router package to
      29 tests. All `-race` green.
- [x] Smoke-tested live: `/health` returned the 28-model breakdown
      (`chat:20, embeddings:1, image_edit:1, image_gen:1, music_gen:1,
      rerank:1, stt:1, tts:2`); `/metrics` exposed the histogram (sum=0.155s),
      per-status request counters, and per-model token counts (15 prompt /
      2 completion) parsed from the real upstream response.

#### Phase 3b.iv — well-known (done)

- [x] New `internal/router/wellknown.go`: `WellKnownConfig` struct (provider
      id/name, baseURL, optional apiKey, configurable context/output limit
      defaults — 131072 / 32768 to match the existing static file) and a
      `buildWellKnown` that materializes the OpenCode-shape document
      (`$schema` + `provider.<id>.{npm, name, options, models}`) from the
      router's active model set.
- [x] One model entry per alias of every **chat-class** model (embeddings,
      rerank, TTS/STT, image, music aliases are filtered out — OpenCode is
      a chat client). Models without aliases fall back to their registry id
      as the alias.
- [x] `WithWellKnown(cfg)` option on the Router; `Handler()` registers
      `GET /.well-known/opencode`; empty `ProviderID` returns 404 so the
      endpoint is opt-in via flag (`--wellknown-provider-id`,
      `--wellknown-provider-name`, `--wellknown-base-url`,
      `--wellknown-api-key`). Response is pretty-printed JSON with
      `Cache-Control: no-store` so OpenCode picks up model changes on the
      next restart.
- [x] 5 new tests (disabled-returns-404, alias-emission and chat-only
      filter, mode filter, omit-empty apiKey, custom limit defaults) bring
      the router package to 34 tests, all `-race` green.
- [x] Smoke-tested live against the real `models.yaml`: 29 chat aliases
      emitted (covering the auto-router stubs, all enabled Claude/GLM/Kimi/
      Zen externals, the local nemotron/qwen3.6 lines, and the
      thinker/coder/research/vision aliases) with zero leakage of any
      non-chat alias; with no flags configured the endpoint returns 404.
- [x] Retires the Python `opencode-wellknown.service` (static file server
      on `:4012`) — the static file at `/var/lib/opencode-wellknown/opencode`
      regenerates from `models.yaml` on every request. Tailscale Serve's
      `/.well-known/*` path will repoint at the Go router at cutover.

**Cutover criterion**: parallel run on `:4015` for 48h, dashboard +
opencode clients pointed at it, no observed regressions.

#### Phase 3 — HARD CUTOVER done 2026-06-06

The 48h parallel-run was skipped in favor of a hard cutover (low-traffic
Friday evening, "roll forward, can switch back"). Live on euclid:4010
since ~13:30 CDT. LiteLLM (`litellm-proxy.service`) stopped + disabled.

- Unit: `deploy/systemd/llm-router-go.service` — binds `:4010`, user
  `llm-router`, `Conflicts=litellm-proxy.service` so the two can't fight.
- Deploy script: `deploy/scripts/deploy-router.sh [--cutover]` — mirrors
  the node-agent deploy, with an opt-in flag that performs the service
  flip.
- Build: **`CGO_ENABLED=1`** (not the usual static build). Pure-Go DNS
  failed on euclid because systemd-resolved is active but does **not**
  bind the loopback stub at `127.0.0.53:53` — NetBird is the only :53
  listener and it's bound to the mesh IP. Cgo'd binary uses glibc → NSS
  → avahi like everything else.
- Sandbox: `SystemCallFilter` deliberately omitted (combined with
  `RestrictAddressFamilies` it broke the resolver). Re-harden later with
  a real backend probe in the test loop.
- Started with **NopSink**; reqlog wired to the dedicated `llm-router-postgres`
  container 2026-06-07 (`--postgres-dsn=${ROUTER_PG_DSN}`). On a DB-unreachable
  error the router logs a WARN and falls back to NopSink rather than exiting, so
  a log-DB outage never blocks inference.
- Roll-forward gaps (logged, not blocking):
  - `/v1/models` lists 28 canonical IDs only — aliases are resolved
    during routing but NOT in the listing. LiteLLM expanded to 63.
    `.well-known/opencode` still emits all 31 chat aliases so opencode
    keeps working.
  - `opencode-wellknown.service` on `:4012` is now redundant; harmless
    but should be stopped + disabled.
  - `dashboard.py` has a `:4010/ui` LiteLLM admin link that 404s.
- Rollback: `just rollback-to-litellm` (in the Python repo's justfile).
  LiteLLM is still installed, just disabled.

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
- [x] **Postgres schema**: DECIDED 2026-05-26 — define a fresh, simpler schema
      (not LiteLLM's). Detailed in Phase 3b.
- [ ] **Bifrost re-evaluation**: revisit after Phase 1 if `internal/router`
      starts approaching 3K lines.
- [ ] **Auto-router embedding cache**: currently lives in tool-proxy
      memory; consider per-process startup time impact.
- [ ] **opencode .well-known**: confirm Go router can serve the same
      schema as the Python generator (`reference_opencode_wellknown.md`).
- [ ] **Push initial scaffold commit to `github.com/erewhon/llm-router-go`**
      after user review.
