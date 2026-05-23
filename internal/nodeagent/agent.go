// Package nodeagent is the per-machine HTTP service that manages local
// inference backends. Phase 1a is an HTTP skeleton: it serves the same
// /health, /models, /models/{id}/status, and /metrics endpoints as the
// Python agent, but reports state purely from the model registry — no
// backend probing yet. Probing arrives in Phase 1b.
package nodeagent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Agent serves the node-agent HTTP API for a single node in the registry.
type Agent struct {
	registry *config.ModelRegistry
	node     string
	logger   *slog.Logger
	version  string
	started  time.Time
	metrics  http.Handler
}

// New constructs an Agent. The node name must exist in registry.Nodes.
// version is stamped into Prometheus build_info and the access log.
func New(registry *config.ModelRegistry, node string, logger *slog.Logger, version string) (*Agent, error) {
	if registry == nil {
		return nil, fmt.Errorf("nodeagent: registry is nil")
	}
	if _, ok := registry.Nodes[node]; !ok {
		return nil, fmt.Errorf("nodeagent: node %q not in registry", node)
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &Agent{
		registry: registry,
		node:     node,
		logger:   logger,
		version:  version,
		started:  time.Now(),
	}
	a.metrics = newMetricsHandler(a)
	return a, nil
}

// Handler returns a mux serving the agent's HTTP API. The caller is
// expected to wrap it in the standard httpx middleware chain (RequestID,
// AccessLog, Recover).
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("GET /models", a.handleModelList)
	mux.HandleFunc("GET /models/{model_id}/status", a.handleModelStatus)
	mux.Handle("GET /metrics", a.metrics)
	return mux
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	nodeDef := a.registry.Nodes[a.node]
	models := a.registry.ModelsForNode(a.node, true)

	running := []string{}
	for id, m := range models {
		// Phase 1a: only external backends are considered "running" because
		// they're managed outside this agent. Local backends report stopped
		// until probing lands in Phase 1b.
		if m.Backend == config.BackendExternal {
			running = append(running, id)
		}
	}

	resp := HealthResponse{
		Status:        "ok",
		Node:          a.node,
		RunningModels: running,
		Services:      []ServiceStatus{},
	}

	if free, total, err := diskUsageGB("/"); err == nil {
		resp.DiskFreeGB = &free
		resp.DiskTotalGB = &total
	}

	// Co-located services are listed but not probed in Phase 1a.
	for name, svc := range nodeDef.Services {
		resp.Services = append(resp.Services, ServiceStatus{
			Name:        name,
			ServiceType: string(svc.Type),
			Label:       svc.Label,
			Reachable:   false,
		})
	}

	writeJSON(w, resp)
}

func (a *Agent) handleModelList(w http.ResponseWriter, r *http.Request) {
	models := a.registry.ModelsForNode(a.node, true)
	out := make([]ModelListEntry, 0, len(models))
	for id, m := range models {
		out = append(out, ModelListEntry{
			ModelID:  id,
			State:    initialState(m.Backend),
			HFRepo:   m.HFRepo,
			Backend:  string(m.Backend),
			AlwaysOn: m.AlwaysOn,
			VRAMGB:   m.VRAMGB,
		})
	}
	writeJSON(w, out)
}

func (a *Agent) handleModelStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("model_id")
	models := a.registry.ModelsForNode(a.node, true)
	m, ok := models[id]
	if !ok {
		http.Error(w, fmt.Sprintf("model %q not found on node %q", id, a.node), http.StatusNotFound)
		return
	}
	writeJSON(w, ModelStatusResponse{
		ModelID: id,
		State:   initialState(m.Backend),
		Backend: string(m.Backend),
		HFRepo:  m.HFRepo,
	})
}

// initialState is the placeholder state returned before Phase 1b backend
// probing exists. External models report RUNNING because they're managed
// outside the agent; everything else is STOPPED until probed.
func initialState(b config.BackendType) ModelState {
	if b == config.BackendExternal {
		return StateRunning
	}
	return StateStopped
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

func newMetricsHandler(a *Agent) http.Handler {
	reg := prometheus.NewRegistry()

	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "node_agent_build_info",
		Help: "Build version and node identity (constant 1).",
	}, []string{"version", "node"})
	buildInfo.WithLabelValues(a.version, a.node).Set(1)
	reg.MustRegister(buildInfo)

	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "node_agent_uptime_seconds",
		Help: "Seconds since the node agent started.",
	}, func() float64 { return time.Since(a.started).Seconds() }))

	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "node_agent_models_enabled",
		Help: "Count of enabled models assigned to this node.",
	}, func() float64 {
		return float64(len(a.registry.ModelsForNode(a.node, true)))
	}))

	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		// We've already written headers via json.NewEncoder, so the best
		// we can do is log via the caller's logger context. The handler
		// closure doesn't have a logger reference; rely on Recover
		// middleware to surface anything truly catastrophic.
		_ = err
	}
}
