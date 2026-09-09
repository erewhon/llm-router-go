// Dashboard: the status UI, baked into the router binary. This is the Go port
// of the standalone Python FastAPI dashboard (src/llm_router/dashboard.py) —
// same HTML/JS frontend (embedded verbatim below) and the same three read-only
// JSON endpoints plus the quick-chat proxy.
//
// It is mounted on its own listener (router --dashboard-addr), separate from
// the OpenAI front door, so the two keep independent auth boundaries: the
// dashboard listener carries no bearer auth and defaults to a loopback bind,
// mirroring the euclid topology where the dashboard sat behind its own
// oauth2-proxy vhost. Because the handlers run in the same process as the
// Router, /api/chat re-enters handleProxy directly — no HTTP self-hop, no API
// key to hold — and /api/router-metrics reads the Prometheus registry in place
// instead of scraping and re-parsing /metrics.
package router

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/health"
)

//go:embed dashboard.html
var dashboardHTMLTemplate string

// Node probing lives in internal/health — the dashboard and the availability
// tracker share one prober so they can never disagree about whether a node
// answered. The dashboard still probes live rather than reading the tracker's
// cache: it wants sub-second VRAM and tok/s, which a 15s poll can't give.

// DashboardConfig holds the values substituted into the served HTML. Both are
// display-only reference material in the "Connection" card — they don't affect
// routing. APIBase is the public OpenAI-compatible base URL clients should hit
// (e.g. http://localhost:4010 locally, https://llm.bcc.sh on euclid); APIKey is
// the hint shown in the curl example.
type DashboardConfig struct {
	APIBase string
	APIKey  string
}

// DashboardHandler returns the http.Handler for the dashboard listener. The
// HTML is substituted once here and captured in the root handler's closure.
func (rt *Router) DashboardHandler(cfg DashboardConfig) http.Handler {
	if cfg.APIKey == "" {
		cfg.APIKey = "<api-key>"
	}
	html := strings.NewReplacer(
		"%%API_BASE%%", cfg.APIBase,
		"%%API_KEY%%", cfg.APIKey,
	).Replace(dashboardHTMLTemplate)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, html)
	})
	// Browsers auto-request /favicon.ico; 204 keeps it out of the console.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/models", rt.handleDashModels)
	mux.HandleFunc("GET /api/node-metrics", rt.handleDashNodeMetrics)
	mux.HandleFunc("GET /api/router-metrics", rt.handleDashRouterMetrics)
	mux.HandleFunc("GET /api/upstream", rt.handleDashUpstream)
	mux.HandleFunc("POST /api/chat", rt.handleDashChat)
	rt.dashConfig = cfg
	return mux
}

// ---------------------------------------------------------------------------
// Node-agent fetch — port of the Python _fetch_node_metrics helper.
// ---------------------------------------------------------------------------

// nodeMetric is the per-node payload returned by /api/node-metrics and embedded
// in /api/models. The vram_*/ram_* fields are emitted as null (not omitted) so
// the frontend sees the same keys whether or not a node is reachable, matching
// the Python default dict.
type nodeMetric struct {
	Reachable   bool     `json:"reachable"`
	VRAMUsedGB  *float64 `json:"vram_used_gb"`
	VRAMTotalGB *float64 `json:"vram_total_gb"`
	VRAMPct     *float64 `json:"vram_pct"`
	GPUBusyPct  *int     `json:"gpu_busy_pct"`
	RAMUsedGB   *float64 `json:"ram_used_gb"`
	RAMTotalGB  *float64 `json:"ram_total_gb"`
	RAMPct      *float64 `json:"ram_pct"`
	Services    []any    `json:"services,omitempty"`
	DiskFreeGB  *float64 `json:"disk_free_gb,omitempty"`
	DiskTotalGB *float64 `json:"disk_total_gb,omitempty"`
	// GPUs is present only for multi-GPU nodes (talos 2x B70); the
	// aggregate vram_*/gpu_busy_pct fields above always exist alongside.
	GPUs   []dashGPU         `json:"gpus,omitempty"`
	Models []nodeModelMetric `json:"models"`
}

type dashGPU struct {
	Index       int     `json:"index"`
	VRAMUsedGB  float64 `json:"vram_used_gb"`
	VRAMTotalGB float64 `json:"vram_total_gb"`
	VRAMPct     float64 `json:"vram_pct"`
	BusyPct     *int    `json:"busy_pct"`
}

type nodeModelMetric struct {
	ModelID         string   `json:"model_id"`
	State           string   `json:"state"`
	RequestsRunning int      `json:"requests_running"`
	RequestsWaiting int      `json:"requests_waiting"`
	AvgTokPerS      *float64 `json:"avg_tok_per_s"`
	TotalRequests   int      `json:"total_requests"`
}

func unreachableNode() nodeMetric {
	return nodeMetric{Reachable: false, Models: []nodeModelMetric{}}
}

// fetchNodeMetrics probes one node agent via the shared prober and maps the
// result into the JSON shape the dashboard frontend expects.
func fetchNodeMetrics(ctx context.Context, host string, agentPort int) nodeMetric {
	result := unreachableNode()
	snap := health.ProbeNode(ctx, host, agentPort)
	if !snap.Reachable {
		return result
	}
	healthResp := snap.Health
	result.Reachable = true
	if healthResp.TotalVRAMGB != nil && healthResp.FreeVRAMGB != nil {
		total := *healthResp.TotalVRAMGB
		used := round1(total - *healthResp.FreeVRAMGB)
		result.VRAMUsedGB = &used
		t := round1(total)
		result.VRAMTotalGB = &t
		pct := 0.0
		if total > 0 {
			pct = round1(used / total * 100)
		}
		result.VRAMPct = &pct
	}
	result.GPUBusyPct = healthResp.GPUBusyPct
	if healthResp.RAMUsedGB != nil && healthResp.RAMTotalGB != nil {
		used := round1(*healthResp.RAMUsedGB)
		total := round1(*healthResp.RAMTotalGB)
		result.RAMUsedGB = &used
		result.RAMTotalGB = &total
		pct := 0.0
		if *healthResp.RAMTotalGB > 0 {
			pct = round1(*healthResp.RAMUsedGB / *healthResp.RAMTotalGB * 100)
		}
		result.RAMPct = &pct
	}
	result.Services = healthResp.Services
	result.DiskFreeGB = healthResp.DiskFreeGB
	result.DiskTotalGB = healthResp.DiskTotalGB
	// Per-card figures surface only when a node reports more than one GPU;
	// a single card's numbers are already the aggregate row.
	if len(healthResp.GPUs) > 1 {
		for _, g := range healthResp.GPUs {
			pct := 0.0
			if g.VRAMTotalGB > 0 {
				pct = round1(g.VRAMUsedGB / g.VRAMTotalGB * 100)
			}
			result.GPUs = append(result.GPUs, dashGPU{
				Index:       g.Index,
				VRAMUsedGB:  round1(g.VRAMUsedGB),
				VRAMTotalGB: round1(g.VRAMTotalGB),
				VRAMPct:     pct,
				BusyPct:     g.BusyPct,
			})
		}
	}

	for _, m := range snap.Models {
		result.Models = append(result.Models, nodeModelMetric{
			ModelID:         m.ModelID,
			State:           m.State,
			RequestsRunning: m.RequestsRunning,
			RequestsWaiting: m.RequestsWaiting,
			AvgTokPerS:      m.AvgTokPerS,
			TotalRequests:   m.TotalRequests,
		})
	}
	return result
}

// fetchAllNodeMetrics probes every registry node concurrently. Unreachable
// nodes get the zero-value nodeMetric rather than being dropped.
func (rt *Router) fetchAllNodeMetrics(ctx context.Context) map[string]nodeMetric {
	out := make(map[string]nodeMetric, len(rt.registry.Nodes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, node := range rt.registry.Nodes {
		wg.Add(1)
		go func(name string, node config.NodeDefinition) {
			defer wg.Done()
			m := rt.nodeFetcher(ctx, node.Host, node.AgentPort)
			mu.Lock()
			out[name] = m
			mu.Unlock()
		}(name, node)
	}
	wg.Wait()
	return out
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (rt *Router) handleDashNodeMetrics(w http.ResponseWriter, r *http.Request) {
	metrics := rt.fetchAllNodeMetrics(r.Context())
	// Overlay the router's own per-response tok/s so the fast poll carries the
	// same figure as /api/models. This is the uniform, backend-agnostic source
	// (incl. Atlas, whose Prometheus counter can't be rated); the node-agent
	// value only fills in where the router hasn't measured recently.
	now := time.Now()
	for name, nm := range metrics {
		for i := range nm.Models {
			if v, ok := rt.tokStats.get(nm.Models[i].ModelID, now); ok {
				nm.Models[i].AvgTokPerS = &v
			}
		}
		metrics[name] = nm
	}
	writeDashJSON(w, metrics)
}

type dashModel struct {
	ID              string   `json:"id"`
	HFRepo          string   `json:"hf_repo"`
	Backend         string   `json:"backend"`
	Nodes           []string `json:"nodes"`
	HeadNode        *string  `json:"head_node"`
	VRAMGB          int      `json:"vram_gb"`
	AlwaysOn        bool     `json:"always_on"`
	Enabled         bool     `json:"enabled"`
	ToolProxy       bool     `json:"tool_proxy"`
	Aliases         []string `json:"aliases"`
	Capabilities    []string `json:"capabilities"`
	Tags            []string `json:"tags"`
	APIBase         string   `json:"api_base"`
	Health          string   `json:"health"`
	AgentState      *string  `json:"agent_state"`
	RequestsRunning int      `json:"requests_running"`
	RequestsWaiting int      `json:"requests_waiting"`
	AvgTokPerS      *float64 `json:"avg_tok_per_s"`
	TotalRequests   int      `json:"total_requests"`
	GGUFFile        string   `json:"gguf_file"`
	// ContextLength is the window as served; EffectiveContext is the window
	// within which the seat is actually worth routing to. They travel together
	// because either one alone invites the wrong reading. Zero means unset.
	ContextLength    int `json:"context_length"`
	EffectiveContext int `json:"effective_context"`
}

func (rt *Router) handleDashModels(w http.ResponseWriter, r *http.Request) {
	nodeMetrics := rt.fetchAllNodeMetrics(r.Context())

	// model_id -> agent state + request counts, from the node metrics.
	agentState := map[string]string{}
	agentReqs := map[string]nodeModelMetric{}
	for _, nm := range nodeMetrics {
		for _, m := range nm.Models {
			agentState[m.ModelID] = m.State
			agentReqs[m.ModelID] = m
		}
	}

	ids := make([]string, 0, len(rt.registry.Models))
	for id := range rt.registry.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	models := make([]dashModel, 0, len(ids))
	for _, id := range ids {
		m := rt.registry.Models[id]

		var nodes []string
		var head *string
		switch {
		case m.MultiNode != nil:
			nodes = append([]string{}, m.MultiNode.Nodes...)
			h := m.MultiNode.HeadNode
			if h == "" && len(m.MultiNode.Nodes) > 0 {
				h = m.MultiNode.Nodes[0]
			}
			if h != "" {
				head = &h
			}
		case m.Node != "":
			nodes = []string{m.Node}
			n := m.Node
			head = &n
		default:
			nodes = []string{}
		}

		// The Go router has no central per-model health probe (it replaced
		// LiteLLM's /model/info). Node-managed models report liveness via the
		// agent state; externals with no node are "routed"; disabled overrides.
		state, hasState := agentState[id]
		var health string
		var statePtr *string
		switch {
		case !m.Enabled:
			health = "disabled"
		case hasState:
			health = "unknown" // frontend prefers agent_state
			s := state
			statePtr = &s
		case m.Node == "" && m.MultiNode == nil:
			health = "routed"
		default:
			health = "unknown"
		}

		reqs := agentReqs[id]
		apiBase, _ := rt.registry.APIBase(id, nil)

		// tok/s: prefer the router's own per-response measurement (uniform
		// across backends, incl. Atlas whose Prometheus counter can't be
		// rated); fall back to the node-agent's engine gauge (SGLang/vLLM).
		avgTok := reqs.AvgTokPerS
		if v, ok := rt.tokStats.get(id, time.Now()); ok {
			avgTok = &v
		}

		models = append(models, dashModel{
			ID:              id,
			HFRepo:          m.HFRepo,
			Backend:         string(m.Backend),
			Nodes:           nodes,
			HeadNode:        head,
			VRAMGB:          m.VRAMGB,
			AlwaysOn:        m.AlwaysOn,
			Enabled:         m.Enabled,
			ToolProxy:       m.ToolProxy,
			Aliases:         orEmpty(m.Aliases),
			Capabilities:    capabilityStrings(m.Capabilities),
			Tags:            orEmpty(m.Tags),
			APIBase:         apiBase,
			Health:          health,
			AgentState:      statePtr,
			RequestsRunning: reqs.RequestsRunning,
			RequestsWaiting: reqs.RequestsWaiting,
			AvgTokPerS:      avgTok,
			TotalRequests:   reqs.TotalRequests,
			GGUFFile:        m.GGUFFile,
			// The advertised window, resolved the same way the well-known
			// resolves it, so the dashboard and /.well-known never disagree.
			ContextLength:    rt.wellKnownContext(m, 0),
			EffectiveContext: m.EffectiveContext,
		})
	}

	nodes := map[string]any{}
	now := time.Now()
	for name, n := range rt.registry.Nodes {
		nodes[name] = map[string]any{
			"host":           n.Host,
			"gpu":            string(n.GPU),
			"vram_gb":        n.VRAMGB,
			"agent_port":     n.AgentPort,
			"unified_memory": n.UnifiedMemory,
			// expected_down lets the UI render planned downtime as planned.
			// A node powered off inside its own schedule is not a fault.
			"expected_down": health.ExpectedDown(n, now),
		}
	}

	writeDashJSON(w, map[string]any{
		"litellm_url":  rt.dashConfig.APIBase,
		"node_count":   len(rt.registry.Nodes),
		"model_count":  len(rt.registry.Models),
		"nodes":        nodes,
		"node_metrics": nodeMetrics,
		"models":       models,
		// Roles ride along on this payload rather than needing their own poll:
		// the Roles card wants to render in the same frame as the node states
		// it explains.
		"roles": rt.roleBindings(),
	})
}

func (rt *Router) handleDashRouterMetrics(w http.ResponseWriter, r *http.Request) {
	snap := rt.metrics.snapshot()

	modelsByClass := map[string]int{}
	for _, m := range rt.active {
		modelsByClass[string(m.APIClass)]++
	}

	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(snap.ByModel))
	for k, v := range snap.ByModel {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	top := [][]any{}
	for i, p := range pairs {
		if i >= 6 {
			break
		}
		top = append(top, []any{p.k, p.v})
	}

	var avgLatency *float64
	if snap.DurationCount > 0 {
		v := round1(snap.DurationSum / snap.DurationCount * 1000)
		avgLatency = &v
	}

	writeDashJSON(w, map[string]any{
		"reachable":         true,
		"version":           rt.version,
		"uptime_seconds":    time.Since(rt.started).Seconds(),
		"mode":              rt.mode,
		"models":            len(rt.active),
		"models_by_class":   modelsByClass,
		"total_requests":    snap.TotalRequests,
		"errors":            snap.Errors,
		"by_status":         snap.ByStatus,
		"top_models":        top,
		"tokens_prompt":     snap.TokensPrompt,
		"tokens_completion": snap.TokensCompletion,
		"avg_latency_ms":    avgLatency,
	})
}

// handleDashUpstream serves the upstream failure panel: per-(model, endpoint)
// attempt/failure counts over the router's rolling 1h/24h window, problem rows
// first. In-memory (cleared by a restart); `just reqlog-failures` in the
// llm-router repo is the durable equivalent against reqlog-pg.
func (rt *Router) handleDashUpstream(w http.ResponseWriter, r *http.Request) {
	rows := rt.upstreamStats.stats(time.Now())
	if rows == nil {
		rows = []upstreamRow{}
	}
	writeDashJSON(w, map[string]any{"upstream": rows})
}

// handleDashChat relays one quick-chat turn to a model. It expands the
// {model, message} shape into an OpenAI streaming chat-completions request and
// re-enters handleProxy in-process, so the reply takes the exact routing +
// reqlog + metrics path a real /v1/chat/completions call would — and bypasses
// the API-key auth the dashboard listener doesn't carry.
func (rt *Router) handleDashChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model   string `json:"model"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Model == "" || req.Message == "" {
		http.Error(w, `"model" and "message" are required`, http.StatusBadRequest)
		return
	}

	body, err := json.Marshal(map[string]any{
		"model":          req.Model,
		"messages":       []map[string]string{{"role": "user", "content": req.Message}},
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	})
	if err != nil {
		http.Error(w, "encode: "+err.Error(), http.StatusInternalServerError)
		return
	}

	inner := r.Clone(r.Context())
	inner.Method = http.MethodPost
	inner.URL.Path = "/v1/chat/completions" // attributes reqlog/metrics correctly
	inner.Body = io.NopCloser(bytes.NewReader(body))
	inner.ContentLength = int64(len(body))
	inner.Header = http.Header{"Content-Type": []string{"application/json"}}

	// Defeat proxy buffering (Caddy/oauth2-proxy in front) so chunks — and thus
	// the browser's measured TTFT — arrive incrementally.
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")

	// A pre-stream failure (unknown model, api_class mismatch) reaches the
	// browser as an SSE error frame carrying the router's message, matching the
	// old Python relay and the frontend's obj.error handling — rather than a
	// bare non-200 the fetch reader only surfaces as "HTTP <code>".
	rt.handleProxy(config.APIClassChat, false)(&sseErrorWriter{ResponseWriter: w}, inner)
}

// sseErrorWriter converts a pre-stream non-200 response into a single
// text/event-stream error frame. Once a 200 has been written (the streaming
// path) it's a transparent passthrough. Flush is forwarded so the wrapped SSE
// stream still flushes per chunk through handleProxy's recordingWriter.
type sseErrorWriter struct {
	http.ResponseWriter
	status  int
	wrote   bool
	errMode bool
}

func (w *sseErrorWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	if code == http.StatusOK {
		w.ResponseWriter.WriteHeader(http.StatusOK)
		return
	}
	// Serve 200 so the browser reads the body as a stream; the upcoming
	// body write(s) become the error frame's message.
	w.errMode = true
	w.Header().Set("Content-Type", "text/event-stream")
	w.ResponseWriter.WriteHeader(http.StatusOK)
}

func (w *sseErrorWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if !w.errMode {
		return w.ResponseWriter.Write(b)
	}
	frame, _ := json.Marshal(map[string]string{
		"error": "router " + strconv.Itoa(w.status) + ": " + strings.TrimSpace(string(b)),
	})
	if _, err := io.WriteString(w.ResponseWriter, "data: "+string(frame)+"\n\n"); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (w *sseErrorWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeDashJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func capabilityStrings(caps []config.ModelCapability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
}
