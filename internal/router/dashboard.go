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
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erewhon/llm-router-go/internal/auth"
	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/health"
)

// dashboardV2 is the dashboard: index.html (the tab shell) plus the CSS and
// ES modules under dashboard/. It replaced the single-file dashboard.html on
// 2026-09-12 (Dashboard v2, Phase 0). Design: the Forge page "LLM Router
// Dashboard: split into sections + a live Activity view".
//
//go:embed dashboard
var dashboardV2 embed.FS

// Node probing lives in internal/health — the dashboard and the availability
// tracker share one prober so they can never disagree about whether a node
// answered. The dashboard still probes live rather than reading the tracker's
// cache: it wants sub-second VRAM and tok/s, which a 15s poll can't give.

// DashboardConfig configures the dashboard listener.
//
// APIBase and APIKey are display-only reference material substituted into the
// "Connection" card — they don't affect routing. APIBase is the public
// OpenAI-compatible base URL clients should hit (e.g. http://localhost:4010
// locally, https://llm.bcc.sh on euclid); APIKey is the hint shown in the
// curl example.
//
// The remaining fields wire token self-service; see dashboard_tokens.go.
type DashboardConfig struct {
	APIBase string
	// APIKey is a display hint only. Empty (the norm since the well-known
	// stopped carrying a key) shows PAT instructions instead of a value.
	APIKey string
	// ProviderID is the OpenCode provider id the well-known serves ("llm"),
	// so the Connection card can spell out the /connect step. Empty hides it.
	ProviderID string
	// SetupHint is the same instruction text the well-known's auth command
	// prints, so both surfaces teach identical steps.
	SetupHint string
	// Tokens is the PAT store self-service mints into. Nil disables the
	// /api/tokens routes (they answer 404-shaped 503s, not silence).
	Tokens *auth.Store
	// AuthSecret is the value the front proxy must send in X-Dashboard-Auth
	// for an identity header to be believed. Empty means the listener has no
	// way to verify identity: /api/tokens is off, and /api/chat + /api/usage
	// stay open exactly as before (the local-dev posture).
	AuthSecret string
	// IdentityHeader names the header carrying the proxy-verified principal
	// (oauth2-proxy's X-Auth-Request-Email by default).
	IdentityHeader string
	// Owners may mint any scope from the dashboard. Everyone else is capped
	// at models:local — a family token must be structurally unable to spend.
	Owners []string
}

// DashboardHandler returns the http.Handler for the dashboard listener. The
// HTML is substituted once here and captured in the root handler's closure.
func (rt *Router) DashboardHandler(cfg DashboardConfig) http.Handler {
	keyHint := cfg.APIKey
	if keyHint == "" {
		keyHint = "pat_…"
	}
	// The shell gets the four substitutions once, into its window.DASH_CONFIG
	// block. The setup hint is multi-line prose and lands inside a JS string
	// literal, so it is JSON-escaped first; the other three are URLs and ids
	// and go in as they are.
	html := ""
	if raw, err := dashboardV2.ReadFile("dashboard/index.html"); err == nil {
		hint, _ := json.Marshal(cfg.SetupHint)
		html = strings.NewReplacer(
			"\"%%SETUP_HINT%%\"", string(hint),
			"%%API_BASE%%", cfg.APIBase,
			"%%API_KEY%%", keyHint,
			"%%PROVIDER_ID%%", cfg.ProviderID,
		).Replace(string(raw))
	}
	staticFS, _ := fs.Sub(dashboardV2, "dashboard")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if html == "" {
			http.Error(w, "dashboard shell not embedded", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = io.WriteString(w, html)
	})
	// /v2 was the shell's address while the legacy page still served /;
	// bookmarks from that week land on the real thing.
	mux.HandleFunc("GET /v2", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+func() string {
			if r.URL.RawQuery != "" {
				return "?" + r.URL.RawQuery
			}
			return ""
		}(), http.StatusMovedPermanently)
	})
	// Static assets straight from the embedded tree. The binary IS the
	// version, so no-cache: a deploy must never serve yesterday's app.js
	// against today's /api shapes. index.html is not served here (it needs
	// the substitutions), and FileServerFS's own directory listings are
	// suppressed by the index check below.
	static := http.StripPrefix("/static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /static/{path...}", func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("path")
		if p == "" || strings.HasSuffix(p, "/") || p == "index.html" {
			http.NotFound(w, r)
			return
		}
		if st, err := fs.Stat(staticFS, p); err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})
	// Browsers auto-request /favicon.ico; 204 keeps it out of the console.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/overview", rt.handleDashOverview)
	// /api/models (everything in one document, with a live node probe) was
	// split on 2026-09-12: Fleet wants nodes/roles/inventory on a fast
	// cadence, Catalog wants the table without waiting on a slow agent.
	mux.HandleFunc("GET /api/fleet", rt.handleDashFleet)
	mux.HandleFunc("GET /api/catalog", rt.handleDashCatalog)
	mux.HandleFunc("GET /api/node-metrics", rt.handleDashNodeMetrics)
	mux.HandleFunc("GET /api/router-metrics", rt.handleDashRouterMetrics)
	mux.HandleFunc("GET /api/upstream", rt.handleDashUpstream)
	rt.dashConfig = cfg
	// Identity-bearing routes. When a secret is configured every one of
	// these demands the proxy's identity; without one, chat and usage stay
	// open (local dev) and tokens are refused, since minting needs a person.
	ident := rt.dashIdentityGate(cfg)
	mux.Handle("POST /api/chat", ident(http.HandlerFunc(rt.handleDashChat)))
	mux.Handle("GET /api/usage", ident(http.HandlerFunc(rt.handleDashUsage)))
	mux.Handle("GET /api/events", ident(http.HandlerFunc(rt.handleDashEvents)))
	mux.Handle("GET /api/traffic", ident(http.HandlerFunc(rt.handleDashTraffic)))
	mux.Handle("GET /api/tokens", ident(http.HandlerFunc(rt.handleDashTokensList)))
	mux.Handle("POST /api/tokens", ident(http.HandlerFunc(rt.handleDashTokensMint)))
	mux.Handle("DELETE /api/tokens/{id}", ident(http.HandlerFunc(rt.handleDashTokensRevoke)))
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
	ID           string   `json:"id"`
	HFRepo       string   `json:"hf_repo"`
	Backend      string   `json:"backend"`
	Nodes        []string `json:"nodes"`
	HeadNode     *string  `json:"head_node"`
	VRAMGB       int      `json:"vram_gb"`
	AlwaysOn     bool     `json:"always_on"`
	Enabled      bool     `json:"enabled"`
	ToolProxy    bool     `json:"tool_proxy"`
	Aliases      []string `json:"aliases"`
	Capabilities []string `json:"capabilities"`
	Tags         []string `json:"tags"`
	APIBase      string   `json:"api_base"`
	Health       string   `json:"health"`
	AgentState   *string  `json:"agent_state"`
	// Availability is the tracker's routing verdict ("available", "warming",
	// "absent", "unavailable", "unknown") when tracking is on, else empty. It
	// is what the router actually acts on; agent_state is what the node agent
	// says.
	Availability       string   `json:"availability,omitempty"`
	AvailabilityReason string   `json:"availability_reason,omitempty"`
	RequestsRunning    int      `json:"requests_running"`
	RequestsWaiting    int      `json:"requests_waiting"`
	AvgTokPerS         *float64 `json:"avg_tok_per_s"`
	TotalRequests      int      `json:"total_requests"`
	GGUFFile           string   `json:"gguf_file"`
	// ContextLength is the window as served; EffectiveContext is the window
	// within which the seat is actually worth routing to. They travel together
	// because either one alone invites the wrong reading. Zero means unset.
	ContextLength    int `json:"context_length"`
	EffectiveContext int `json:"effective_context"`
	// Discovered marks an entry the live inventory adopted from a provider's
	// listing; it has no models.yaml entry behind it.
	Discovered bool `json:"discovered,omitempty"`
}

// dashCatalogEntry is one row of the dashboard's model list: every registry
// entry (mode-filtered or not — the dashboard shows the whole file) plus
// every discovered one.
type dashCatalogEntry struct {
	id         string
	m          config.ModelDefinition
	discovered bool
}

// handleDashFleet is the Fleet tab's payload: nodes with a LIVE agent probe
// (that is what Fleet is for), roles, and the inventory. Polled every 10 s.
func (rt *Router) handleDashFleet(w http.ResponseWriter, r *http.Request) {
	nodeMetrics := rt.fetchAllNodeMetrics(r.Context())
	writeDashJSON(w, map[string]any{
		"node_count":   len(rt.registry.Nodes),
		"nodes":        rt.dashNodes(),
		"node_metrics": nodeMetrics,
		"roles":        rt.roleBindings(),
		"inventory":    rt.inventory(false),
	})
}

// nodeSnapshotter is the tracker's last node poll, so the Catalog can carry
// agent state without a live probe of its own.
type nodeSnapshotter interface {
	Nodes() map[string]health.NodeSnapshot
}

// handleDashCatalog is the Catalog tab's payload: every registry entry plus
// the discovered ones, with verdicts and the agent state from the tracker's
// last poll. No node is probed here — a slow agent must never delay the
// table. Polled every 30 s; the tab merges /api/node-metrics for live counts.
func (rt *Router) handleDashCatalog(w http.ResponseWriter, r *http.Request) {
	agentState := map[string]string{}
	agentReqs := map[string]nodeModelMetric{}
	if src, ok := rt.avail.(nodeSnapshotter); ok {
		for _, snap := range src.Nodes() {
			if !snap.Reachable {
				continue
			}
			for _, m := range snap.Models {
				agentState[m.ModelID] = m.State
				agentReqs[m.ModelID] = nodeModelMetric{
					ModelID: m.ModelID, State: m.State, RequestsRunning: m.RequestsRunning,
					RequestsWaiting: m.RequestsWaiting, AvgTokPerS: m.AvgTokPerS, TotalRequests: m.TotalRequests,
				}
			}
		}
	}
	writeDashJSON(w, map[string]any{
		"litellm_url": rt.dashConfig.APIBase,
		"model_count": len(rt.registry.Models),
		"models":      rt.dashCatalogRows(agentState, agentReqs),
	})
}

// dashNodes is the registry's node table for the dashboard, with the
// schedule verdict the Fleet cards render.
func (rt *Router) dashNodes() map[string]any {
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
	return nodes
}

// dashCatalogRows builds the model table from the registry, the discovered
// set, the tracker's verdicts, and whatever agent state the caller has.
func (rt *Router) dashCatalogRows(agentState map[string]string, agentReqs map[string]nodeModelMetric) []dashModel {
	ids := make([]string, 0, len(rt.registry.Models))
	for id := range rt.registry.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	entries := make([]dashCatalogEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, dashCatalogEntry{id: id, m: rt.registry.Models[id]})
	}
	disc := rt.discoveredModels()
	discIDs := make([]string, 0, len(disc))
	for id := range disc {
		discIDs = append(discIDs, id)
	}
	sort.Strings(discIDs)
	for _, id := range discIDs {
		entries = append(entries, dashCatalogEntry{id: id, m: disc[id], discovered: true})
	}

	verdicts := map[string]health.Status{}
	if rep, ok := rt.avail.(availabilityReporter); ok {
		for _, s := range rep.Snapshot() {
			verdicts[s.Model] = s
		}
	}

	models := make([]dashModel, 0, len(entries))
	for _, e := range entries {
		id, m := e.id, e.m

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
		apiBase := m.APIBase
		if apiBase == "" {
			apiBase, _ = rt.registry.APIBase(id, nil)
		}

		// tok/s: prefer the router's own per-response measurement (uniform
		// across backends, incl. Atlas whose Prometheus counter can't be
		// rated); fall back to the node-agent's engine gauge (SGLang/vLLM).
		avgTok := reqs.AvgTokPerS
		if v, ok := rt.tokStats.get(id, time.Now()); ok {
			avgTok = &v
		}

		models = append(models, dashModel{
			ID:           id,
			HFRepo:       m.HFRepo,
			Backend:      string(m.Backend),
			Nodes:        nodes,
			HeadNode:     head,
			VRAMGB:       m.VRAMGB,
			AlwaysOn:     m.AlwaysOn,
			Enabled:      m.Enabled,
			ToolProxy:    m.ToolProxy,
			Aliases:      orEmpty(m.Aliases),
			Capabilities: capabilityStrings(m.Capabilities),
			Tags:         orEmpty(m.Tags),
			APIBase:      apiBase,
			Health:       health,
			AgentState:   statePtr,
			// Zero-valued when no tracker is attached: the omitempty tags
			// drop both fields and the UI falls back to agent_state.
			Availability:       string(verdicts[id].State),
			AvailabilityReason: verdicts[id].Reason,
			RequestsRunning:    reqs.RequestsRunning,
			RequestsWaiting:    reqs.RequestsWaiting,
			AvgTokPerS:         avgTok,
			TotalRequests:      reqs.TotalRequests,
			GGUFFile:           m.GGUFFile,
			// The advertised window, resolved the same way the well-known
			// resolves it, so the dashboard and /.well-known never disagree.
			ContextLength:    rt.wellKnownContext(m, 0),
			EffectiveContext: m.EffectiveContext,
			Discovered:       e.discovered,
		})
	}
	return models
}

// reqRateWindow is a 60-slot ring of per-second request counts, so the
// dashboard can show requests/min without a Prometheus round-trip. hit() is
// on the request path: one lock, two integer writes.
type reqRateWindow struct {
	mu    sync.Mutex
	slots [60]int
	stamp [60]int64 // unix second each slot currently counts
}

func newReqRateWindow() *reqRateWindow { return &reqRateWindow{} }

func (w *reqRateWindow) hit(now time.Time) {
	s := now.Unix()
	i := int(s % 60)
	w.mu.Lock()
	if w.stamp[i] != s {
		w.stamp[i] = s
		w.slots[i] = 0
	}
	w.slots[i]++
	w.mu.Unlock()
}

// perMinute returns the requests seen in the last 60 s ending at now.
func (w *reqRateWindow) perMinute(now time.Time) int {
	s := now.Unix()
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for i := 0; i < 60; i++ {
		if s-w.stamp[i] < 60 {
			n += w.slots[i]
		}
	}
	return n
}

// handleDashOverview is the header strip: the handful of numbers every tab
// wants, cheap enough to poll every 10 s. No identity gate — nothing here
// names a principal.
func (rt *Router) handleDashOverview(w http.ResponseWriter, r *http.Request) {
	cat, _ := rt.catalog()
	models := map[string]int{"active": len(cat), "up": len(cat), "warming": 0, "absent": 0, "unavailable": 0, "discovered": 0}
	for _, m := range cat {
		if m.IsDiscovered() {
			models["discovered"]++
		}
	}
	if rep, ok := rt.avail.(availabilityReporter); ok {
		up := 0
		for _, s := range rep.Snapshot() {
			if _, in := cat[s.Model]; !in {
				continue
			}
			switch s.State {
			case health.Warming:
				models["warming"]++
			case health.Absent:
				models["absent"]++
			case health.Unavailable:
				models["unavailable"]++
			default:
				up++
			}
		}
		models["up"] = up
	}
	bindings := rt.roleBindings()
	bound := 0
	for _, b := range bindings {
		if b.Available {
			bound++
		}
	}
	host, _ := os.Hostname()
	writeDashJSON(w, map[string]any{
		"version":          rt.version,
		"uptime_s":         time.Since(rt.started).Seconds(),
		"mode":             rt.mode,
		"replica":          host,
		"models":           models,
		"roles":            map[string]int{"total": len(bindings), "bound": bound},
		"requests_per_min": rt.reqRate.perMinute(time.Now()),
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
