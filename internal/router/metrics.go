package router

import (
	"net/http"
	"strconv"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// routerMetrics bundles the Prometheus registry and the per-request
// counters/histograms updated by Observe. Cardinality is bounded by labeling
// "model" with the resolved registry id (~28 possibilities) rather than the
// client-supplied alias/string — and by keeping the duration histogram
// label-free except for path + api_class.
type routerMetrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	tokens   *prometheus.CounterVec
	handler  http.Handler
}

// newRouterMetrics builds a fresh Prometheus registry, registers Go + process
// collectors, the build_info/uptime/models_active gauges, and the per-request
// counters/histograms. active is the model set this router will route to;
// counted once per api_class at construction time (load-time, matches --mode).
func newRouterMetrics(version string, started time.Time, active map[string]config.ModelDefinition) *routerMetrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "router_build_info",
		Help: "Build version (constant 1).",
	}, []string{"version"})
	buildInfo.WithLabelValues(version).Set(1)
	reg.MustRegister(buildInfo)

	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "router_uptime_seconds",
		Help: "Seconds since the router started.",
	}, func() float64 { return time.Since(started).Seconds() }))

	modelsActive := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "router_models_active",
		Help: "Count of routable models by api_class in the current mode.",
	}, []string{"api_class"})
	counts := map[config.APIClass]int{}
	for _, m := range active {
		counts[m.APIClass]++
	}
	for class, n := range counts {
		modelsActive.WithLabelValues(string(class)).Set(float64(n))
	}
	reg.MustRegister(modelsActive)

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "router_requests_total",
		Help: "Requests handled by the router, labeled by route + resolved model + status.",
	}, []string{"path", "model", "api_class", "status"})
	reg.MustRegister(requests)

	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "router_request_duration_seconds",
		Help:    "Router-side request latency (time spent in handleProxy).",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"path", "api_class"})
	reg.MustRegister(duration)

	tokens := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "router_upstream_tokens_total",
		Help: "Token counts reported by upstream responses, split by prompt/completion.",
	}, []string{"kind", "model", "api_class"})
	reg.MustRegister(tokens)

	return &routerMetrics{
		reg:      reg,
		requests: requests,
		duration: duration,
		tokens:   tokens,
		handler:  promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}),
	}
}

// Handler returns the http.Handler the router mounts at /metrics.
func (m *routerMetrics) Handler() http.Handler { return m.handler }

// Observe records one Record's worth of telemetry. Called from handleProxy's
// defer alongside the reqlog Sink, so every request — including 400/404 —
// shows up in the metrics. Unresolved model names collapse into a single
// "unresolved" label to keep cardinality bounded.
func (m *routerMetrics) Observe(rec reqlog.Record) {
	model := rec.ResolvedVia
	if model == "" {
		model = "unresolved"
	}
	apiClass := rec.APIClass
	if apiClass == "" {
		apiClass = "unknown"
	}
	m.requests.WithLabelValues(rec.Path, model, apiClass, strconv.Itoa(rec.Status)).Inc()
	m.duration.WithLabelValues(rec.Path, apiClass).Observe(float64(rec.LatencyMS) / 1000.0)
	if rec.PromptTokens != nil {
		m.tokens.WithLabelValues("prompt", model, apiClass).Add(float64(*rec.PromptTokens))
	}
	if rec.CompletionTokens != nil {
		m.tokens.WithLabelValues("completion", model, apiClass).Add(float64(*rec.CompletionTokens))
	}
}
