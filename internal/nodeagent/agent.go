// Package nodeagent is the per-machine HTTP service that manages local
// inference backends. /health, /models, /models/{id}/status, /metrics
// mirror the Python agent's shapes; per-model state is filled in by
// pluggable backend drivers from internal/nodeagent/backends.
package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/nodeagent/backends"
	"github.com/erewhon/llm-router-go/internal/nodeagent/gpu"

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
	started   time.Time
	backends  map[config.BackendType]backends.Backend
	gpuReader gpu.Reader
	metrics   http.Handler
}

// Option configures an Agent at construction time.
type Option func(*Agent)

// WithBackend registers a driver for the given BackendType. Multiple
// calls overwrite earlier registrations for the same type. Models whose
// backend has no registered driver report StateStopped from /models
// and /models/{id}/status until a driver is wired in.
func WithBackend(t config.BackendType, b backends.Backend) Option {
	return func(a *Agent) {
		if a.backends == nil {
			a.backends = map[config.BackendType]backends.Backend{}
		}
		a.backends[t] = b
	}
}

// WithGPUReader installs the GPU snapshot source used by /health. When
// unset, /health omits the gpu_type / vram / busy_pct fields.
func WithGPUReader(r gpu.Reader) Option {
	return func(a *Agent) { a.gpuReader = r }
}

// New constructs an Agent. The node name must exist in registry.Nodes.
// version is stamped into Prometheus build_info and the access log.
func New(registry *config.ModelRegistry, node string, logger *slog.Logger, version string, opts ...Option) (*Agent, error) {
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
	for _, opt := range opts {
		opt(a)
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
		st := a.probe(r.Context(), id, m)
		if st.State == backends.StateRunning {
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

	if a.gpuReader != nil {
		if info, err := a.gpuReader.Read(r.Context()); err == nil {
			gt := string(info.GpuType)
			resp.GPUType = &gt
			resp.TotalVRAMGB = &info.TotalVRAMGB
			resp.FreeVRAMGB = &info.FreeVRAMGB
			resp.GPUBusyPct = info.GPUBusyPct
		}
	}

	for name, svc := range nodeDef.Services {
		resp.Services = append(resp.Services, ServiceStatus{
			Name:        name,
			ServiceType: string(svc.Type),
			Label:       svc.Label,
			Reachable:   false, // probed in a later phase
		})
	}

	writeJSON(w, resp)
}

func (a *Agent) handleModelList(w http.ResponseWriter, r *http.Request) {
	models := a.registry.ModelsForNode(a.node, true)
	out := make([]ModelListEntry, 0, len(models))
	for id, m := range models {
		st := a.probe(r.Context(), id, m)
		out = append(out, ModelListEntry{
			ModelID:         id,
			State:           st.State,
			HFRepo:          m.HFRepo,
			Backend:         string(m.Backend),
			AlwaysOn:        m.AlwaysOn,
			VRAMGB:          m.VRAMGB,
			RequestsRunning: st.RequestsRunning,
			RequestsWaiting: st.RequestsWaiting,
			AvgTokPerS:      st.AvgTokPerSec,
			TotalRequests:   st.TotalRequests,
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
	st := a.probe(r.Context(), id, m)
	writeJSON(w, ModelStatusResponse{
		ModelID: id,
		State:   st.State,
		PID:     st.PID,
		Port:    st.Port,
		Backend: string(m.Backend),
		HFRepo:  m.HFRepo,
		Error:   st.Error,
	})
}

// probe returns the model's current backend state. External backends are
// considered running because they're managed outside this agent. For
// non-external backends, we delegate to a registered driver if any;
// otherwise we report stopped (Phase 1a fallback behaviour).
func (a *Agent) probe(ctx context.Context, id string, m config.ModelDefinition) backends.Status {
	if m.Backend == config.BackendExternal {
		return backends.Status{ModelID: id, State: backends.StateRunning}
	}
	if drv, ok := a.backends[m.Backend]; ok {
		return drv.Status(ctx, id, &m)
	}
	return backends.Status{ModelID: id, State: backends.StateStopped}
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
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers already written; nothing better to do here. Recover
		// middleware will catch anything truly catastrophic.
		_ = err
	}
}
