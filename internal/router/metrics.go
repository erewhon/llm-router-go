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
	reg             *prometheus.Registry
	requests        *prometheus.CounterVec
	duration        *prometheus.HistogramVec
	tokens          *prometheus.CounterVec
	modelAvailable  *prometheus.GaugeVec
	roleTarget      *prometheus.GaugeVec
	failovers       *prometheus.CounterVec
	upstream        *prometheus.CounterVec
	privacyRefusals *prometheus.CounterVec
	handler         http.Handler
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
		Help: "Requests handled by the router, labeled by route + resolved model + status + serving provider.",
	}, []string{"path", "model", "api_class", "status", "upstream_provider"})
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

	// Availability + role bindings. These are what you graph to see the fleet
	// powering down in the evening and the roles moving with it.
	modelAvailable := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "router_model_available",
		Help: "1 when a model is currently routable, 0 when the tracker says it is down.",
	}, []string{"model"})
	reg.MustRegister(modelAvailable)

	roleTarget := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "router_role_target",
		Help: "1 for the model a role is currently bound to (0 for its other candidates).",
	}, []string{"role", "model"})
	reg.MustRegister(roleTarget)

	failovers := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "router_role_failover_total",
		Help: "Mid-request failovers from one role candidate to the next.",
	}, []string{"role", "from", "to"})
	reg.MustRegister(failovers)

	// Upstream attempt outcomes — the incident-triage counter. Grouping by
	// api_base separates "one model is broken" from "the whole endpoint is
	// down" (the 2026-08-27 Zen question). Outcomes: success, client_error,
	// server_error, error_envelope, timeout, connect, transport. Failed
	// failover attempts count once each, against the model that failed.
	upstream := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "router_upstream_requests_total",
		Help: "Upstream attempts by resolved model, endpoint root (api_base), and outcome.",
	}, []string{"model", "api_base", "outcome"})
	reg.MustRegister(upstream)

	// Privacy refusals get their own counter rather than living inside the
	// 403s of router_requests_total: they are policy outcomes, not errors,
	// and the dashboard question is "how often does a tier turn a request
	// away, and for which role" — which the status label cannot answer.
	// tier is local/zdr, or "invalid" for a malformed header; subject is
	// the role or chain, "" for a directly named model.
	privacyRefusals := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "router_privacy_refusals_total",
		Help: "Requests refused because no candidate satisfied the X-Router-Privacy tier.",
	}, []string{"tier", "subject"})
	reg.MustRegister(privacyRefusals)

	return &routerMetrics{
		reg:             reg,
		requests:        requests,
		duration:        duration,
		tokens:          tokens,
		modelAvailable:  modelAvailable,
		roleTarget:      roleTarget,
		failovers:       failovers,
		upstream:        upstream,
		privacyRefusals: privacyRefusals,
		handler:         promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}),
	}
}

// ObservePrivacyRefusal counts one request turned away by the privacy tier.
func (m *routerMetrics) ObservePrivacyRefusal(tier, subject string) {
	if m == nil {
		return
	}
	m.privacyRefusals.WithLabelValues(tier, subject).Inc()
}

// ObserveFailover records a mid-request move from one role candidate to the
// next. A rising rate here means the poller is lagging real failures.
func (m *routerMetrics) ObserveFailover(role, from, to string) {
	if m == nil || role == "" {
		return
	}
	m.failovers.WithLabelValues(role, from, to).Inc()
}

// ObserveUpstream records one upstream attempt outcome directly — used for
// failed failover attempts, which never become the reqlog record's final
// attempt (Observe counts that one). class "" means success.
func (m *routerMetrics) ObserveUpstream(model, apiBase, class string) {
	if m == nil || model == "" {
		return
	}
	outcome := class
	if outcome == "" {
		outcome = "success"
	}
	m.upstream.WithLabelValues(model, apiBase, outcome).Inc()
}

// SetAvailability republishes the availability and role-binding gauges. Called
// after each tracker poll rather than on every request, so the gauges reflect
// fleet state rather than traffic.
func (m *routerMetrics) SetAvailability(models map[string]bool, roleTargets map[string]string) {
	if m == nil {
		return
	}
	for id, up := range models {
		v := 0.0
		if up {
			v = 1
		}
		m.modelAvailable.WithLabelValues(id).Set(v)
	}
	// Reset first: a role that moved must not leave its old target reading 1.
	m.roleTarget.Reset()
	for role, target := range roleTargets {
		if target != "" {
			m.roleTarget.WithLabelValues(role, target).Set(1)
		}
	}
}

// Handler returns the http.Handler the router mounts at /metrics.
func (m *routerMetrics) Handler() http.Handler { return m.handler }

// metricsSnapshot is an in-process rollup of the request counters, matching
// what the Python dashboard used to derive by scraping /metrics and parsing the
// Prometheus text. Reading the registry directly avoids the HTTP self-hop.
type metricsSnapshot struct {
	TotalRequests    int
	Errors           int // requests with status >= 400
	ByStatus         map[string]int
	ByModel          map[string]int
	TokensPrompt     int
	TokensCompletion int
	DurationSum      float64 // seconds, summed across all requests
	DurationCount    float64 // number of observed requests
}

// snapshot walks the Prometheus registry and aggregates the router_* families
// the dashboard cares about. Errors from Gather collapse to a zero snapshot —
// the dashboard treats it the same as "no traffic yet".
func (m *routerMetrics) snapshot() metricsSnapshot {
	s := metricsSnapshot{ByStatus: map[string]int{}, ByModel: map[string]int{}}
	families, err := m.reg.Gather()
	if err != nil {
		return s
	}
	for _, mf := range families {
		switch mf.GetName() {
		case "router_requests_total":
			for _, metric := range mf.GetMetric() {
				v := int(metric.GetCounter().GetValue())
				var status, model string
				for _, lp := range metric.GetLabel() {
					switch lp.GetName() {
					case "status":
						status = lp.GetValue()
					case "model":
						model = lp.GetValue()
					}
				}
				s.TotalRequests += v
				s.ByStatus[status] += v
				if code, err := strconv.Atoi(status); err == nil && code >= 400 {
					s.Errors += v
				}
				s.ByModel[model] += v
			}
		case "router_request_duration_seconds":
			for _, metric := range mf.GetMetric() {
				h := metric.GetHistogram()
				s.DurationSum += h.GetSampleSum()
				s.DurationCount += float64(h.GetSampleCount())
			}
		case "router_upstream_tokens_total":
			for _, metric := range mf.GetMetric() {
				v := int(metric.GetCounter().GetValue())
				var kind string
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "kind" {
						kind = lp.GetValue()
					}
				}
				switch kind {
				case "prompt":
					s.TokensPrompt += v
				case "completion":
					s.TokensCompletion += v
				}
			}
		}
	}
	return s
}

// upstreamProviderLabel bounds the cardinality of the serving-provider label.
//
// "local" rather than "" for anything the fleet served: an empty label reads
// as "missing data" on a graph, and the overwhelming majority of rows are
// local, so the common case deserves a name. Requests rejected before an
// upstream was tried are "none" — distinct from local, because a 403 on a
// retention tolerance and a request answered by hypatia are not the same
// thing and should not share a series.
//
// The remaining values come from the upstream itself. OpenRouter lists ~106
// providers, realistically a handful in play for any one fleet, so the label
// is bounded in the way that matters — it cannot grow with request volume.
func upstreamProviderLabel(rec reqlog.Record) string {
	if rec.UpstreamProvider != "" {
		return rec.UpstreamProvider
	}
	if rec.BackendURL == "" {
		return "none"
	}
	return "local"
}

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
	m.requests.WithLabelValues(rec.Path, model, apiClass, strconv.Itoa(rec.Status), upstreamProviderLabel(rec)).Inc()
	m.duration.WithLabelValues(rec.Path, apiClass).Observe(float64(rec.LatencyMS) / 1000.0)
	// One upstream-outcome sample for the record's final attempt, but only
	// when an upstream was actually tried: UpstreamStatus > 0 means it
	// answered, a non-empty ErrorClass with status 0 means transport failure.
	// Requests rejected before forwarding (400/404/503) contribute nothing.
	// A privacy refusal never reached an upstream, so it must not be scored
	// against one — even when a seat had been resolved before the refusal.
	if rec.BackendURL != "" && rec.ErrorClass != errorClassPrivacyRefused &&
		(rec.UpstreamStatus > 0 || rec.ErrorClass != "") {
		m.ObserveUpstream(model, rec.BackendURL, rec.ErrorClass)
	}
	if rec.PromptTokens != nil {
		m.tokens.WithLabelValues("prompt", model, apiClass).Add(float64(*rec.PromptTokens))
	}
	if rec.CompletionTokens != nil {
		m.tokens.WithLabelValues("completion", model, apiClass).Add(float64(*rec.CompletionTokens))
	}
}
