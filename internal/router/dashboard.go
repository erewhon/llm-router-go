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
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

//go:embed dashboard.html
var dashboardHTMLTemplate string

// nodeMetricsClient talks to the per-node agents. The 1.5s timeout matches the
// Python dashboard: healthy probes return in <250ms, and a broken host (e.g.
// mDNS resolving to an unroutable overlay IP) must not stall the whole page.
var nodeMetricsClient = &http.Client{Timeout: 1500 * time.Millisecond}

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
	mux.HandleFunc("POST /api/chat", rt.handleDashChat)
	rt.dashConfig = cfg
	return mux
}

// ---------------------------------------------------------------------------
// Node-agent fetch — port of the Python _fetch_node_metrics helper.
// ---------------------------------------------------------------------------

// agentHealth is the subset of the node agent's /health response the dashboard
// consumes; agentModel the subset of its /models list.
type agentHealth struct {
	TotalVRAMGB *float64 `json:"total_vram_gb"`
	FreeVRAMGB  *float64 `json:"free_vram_gb"`
	GPUBusyPct  *int     `json:"gpu_busy_pct"`
	RAMUsedGB   *float64 `json:"ram_used_gb"`
	RAMTotalGB  *float64 `json:"ram_total_gb"`
	DiskFreeGB  *float64 `json:"disk_free_gb"`
	DiskTotalGB *float64 `json:"disk_total_gb"`
	Services    []any    `json:"services"`
}

type agentModel struct {
	ModelID         string   `json:"model_id"`
	State           string   `json:"state"`
	RequestsRunning int      `json:"requests_running"`
	RequestsWaiting int      `json:"requests_waiting"`
	AvgTokPerS      *float64 `json:"avg_tok_per_s"`
	TotalRequests   int      `json:"total_requests"`
}

// nodeMetric is the per-node payload returned by /api/node-metrics and embedded
// in /api/models. The vram_*/ram_* fields are emitted as null (not omitted) so
// the frontend sees the same keys whether or not a node is reachable, matching
// the Python default dict.
type nodeMetric struct {
	Reachable   bool              `json:"reachable"`
	VRAMUsedGB  *float64          `json:"vram_used_gb"`
	VRAMTotalGB *float64          `json:"vram_total_gb"`
	VRAMPct     *float64          `json:"vram_pct"`
	GPUBusyPct  *int              `json:"gpu_busy_pct"`
	RAMUsedGB   *float64          `json:"ram_used_gb"`
	RAMTotalGB  *float64          `json:"ram_total_gb"`
	RAMPct      *float64          `json:"ram_pct"`
	Services    []any             `json:"services,omitempty"`
	DiskFreeGB  *float64          `json:"disk_free_gb,omitempty"`
	DiskTotalGB *float64          `json:"disk_total_gb,omitempty"`
	Models      []nodeModelMetric `json:"models"`
}

type nodeModelMetric struct {
	ModelID         string   `json:"model_id"`
	State           string   `json:"state"`
	RequestsRunning int      `json:"requests_running"`
	RequestsWaiting int      `json:"requests_waiting"`
	AvgTokPerS      *float64 `json:"avg_tok_per_s"`
	TotalRequests   int      `json:"total_requests"`
}

var (
	selfHostsOnce sync.Once
	selfHosts     map[string]struct{}
)

// selfHostnames returns the set of hostnames that mean "this machine", so a
// node whose configured host is the router's own host is probed over loopback
// (dodging mDNS/overlay resolution games). Mirrors the Python _SELF_HOSTNAMES.
func selfHostnames() map[string]struct{} {
	selfHostsOnce.Do(func() {
		selfHosts = map[string]struct{}{"localhost": {}}
		if h, err := os.Hostname(); err == nil && h != "" {
			selfHosts[h] = struct{}{}
			selfHosts[h+".local"] = struct{}{}
		}
	})
	return selfHosts
}

func unreachableNode() nodeMetric {
	return nodeMetric{Reachable: false, Models: []nodeModelMetric{}}
}

// fetchNodeMetrics fetches GPU/RAM metrics and model states from one node agent.
func fetchNodeMetrics(ctx context.Context, host string, agentPort int) nodeMetric {
	result := unreachableNode()
	if _, self := selfHostnames()[host]; self {
		host = "127.0.0.1"
	}
	base := "http://" + host + ":" + strconv.Itoa(agentPort)

	healthResp := fetchJSON[agentHealth](ctx, base+"/health")
	if healthResp == nil {
		return result
	}
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

	if models := fetchJSON[[]agentModel](ctx, base+"/models"); models != nil {
		for _, m := range *models {
			result.Models = append(result.Models, nodeModelMetric{
				ModelID:         m.ModelID,
				State:           m.State,
				RequestsRunning: m.RequestsRunning,
				RequestsWaiting: m.RequestsWaiting,
				AvgTokPerS:      m.AvgTokPerS,
				TotalRequests:   m.TotalRequests,
			})
		}
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
	writeDashJSON(w, rt.fetchAllNodeMetrics(r.Context()))
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
			AvgTokPerS:      reqs.AvgTokPerS,
			TotalRequests:   reqs.TotalRequests,
			GGUFFile:        m.GGUFFile,
		})
	}

	nodes := map[string]any{}
	for name, n := range rt.registry.Nodes {
		nodes[name] = map[string]any{
			"host":           n.Host,
			"gpu":            string(n.GPU),
			"vram_gb":        n.VRAMGB,
			"agent_port":     n.AgentPort,
			"unified_memory": n.UnifiedMemory,
		}
	}

	writeDashJSON(w, map[string]any{
		"litellm_url":  rt.dashConfig.APIBase,
		"node_count":   len(rt.registry.Nodes),
		"model_count":  len(rt.registry.Models),
		"nodes":        nodes,
		"node_metrics": nodeMetrics,
		"models":       models,
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

// fetchJSON GETs url and decodes the body into T. Returns nil on any transport
// error, non-200 status, or decode failure — callers treat nil as "unavailable".
func fetchJSON[T any](ctx context.Context, url string) *T {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := nodeMetricsClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil
	}
	return &v
}

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
