// Package router is the Go rewrite of the LiteLLM proxy — the OpenAI-compatible
// front door for the fleet. It reads models.yaml directly (no generate_config
// step), resolves aliases and model ids to the right upstream, and reverse-
// proxies the request with SSE streaming intact:
//
//   - tool_proxy:true models -> the tool proxy (192.168.42.240:5392), with the
//     model_id preserved so the proxy can disambiguate shared hf_repos
//   - external models        -> their api_base, with the resolved api_key
//   - everything else (local) -> the model's node backend (SGLang/vLLM)
//
// Phase 3a wired /v1/chat/completions, /v1/models, and /health. Phase 3b.i
// added /v1/completions, /v1/embeddings, and /v1/rerank with per-endpoint
// api_class enforcement and tool-proxy bypass for the non-chat endpoints (the
// tool proxy serves only chat-completions). Phase 3b.ii wired request logging
// to a reqlog.Sink — every request that reaches handleProxy emits one record
// (including rejected ones) via a defer, with usage tokens parsed from
// non-streaming JSON bodies (buffered via ReverseProxy.ModifyResponse) and
// from the rolling 64KB tail of SSE streams. Phase 3b.iii added the /metrics
// Prometheus endpoint (per-binary registry mirroring the node-agent's
// pattern, populated from the same Observe call as the Sink) and enriched
// /health with version + uptime + per-api_class model counts. Phase 3b.iv
// added the GET /.well-known/opencode endpoint that generates an OpenCode
// provider config from models.yaml — retiring the static
// /var/lib/opencode-wellknown/opencode file served by the Python :4012 unit.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/httpx"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// Router serves the OpenAI front-door endpoints over a model registry.
type Router struct {
	registry      *config.ModelRegistry
	active        map[string]config.ModelDefinition // models routable in this mode
	mode          string
	logger        *slog.Logger
	transport     http.RoundTripper
	flushInterval time.Duration
	getenv        func(string) string
	sink          reqlog.Sink
	version       string
	started       time.Time
	metrics       *routerMetrics
	wellKnown     WellKnownConfig
	dashConfig    DashboardConfig
	// nodeFetcher probes one node agent for the dashboard. Defaults to the
	// real HTTP fetchNodeMetrics; tests set a stub to stay hermetic.
	nodeFetcher func(ctx context.Context, host string, agentPort int) nodeMetric
}

// Option configures a Router at construction time.
type Option func(*Router)

// WithTransport overrides the HTTP transport used to talk to upstreams.
// Used by tests; production gets http.DefaultTransport.
func WithTransport(rt http.RoundTripper) Option {
	return func(r *Router) { r.transport = rt }
}

// WithFlushInterval overrides the ReverseProxy flush interval. Default is -1
// (flush on every write), which SSE streaming requires. Tests set 0.
func WithFlushInterval(d time.Duration) Option {
	return func(r *Router) { r.flushInterval = d }
}

// WithMode restricts the routable set to models active in the given mode tag
// ("big"/"default"/...). Empty (the default) means all enabled models, no mode
// filtering. Mirrors the Python ModelRegistry.models_for_mode helper.
func WithMode(mode string) Option {
	return func(r *Router) { r.mode = mode }
}

// WithGetenv overrides environment lookup for api_key resolution. Tests inject
// a fake; production uses os.Getenv.
func WithGetenv(fn func(string) string) Option {
	return func(r *Router) {
		if fn != nil {
			r.getenv = fn
		}
	}
}

// WithSink wires a request-log sink. Every request that reaches handleProxy
// produces one record, including rejected ones (400/404). nil is treated as
// reqlog.NopSink. Production uses reqlog.PostgresSink; tests use MemorySink.
func WithSink(s reqlog.Sink) Option {
	return func(r *Router) {
		if s == nil {
			r.sink = reqlog.NopSink{}
			return
		}
		r.sink = s
	}
}

// WithVersion sets the build version surfaced via /health and the
// router_build_info Prometheus metric. Default is "dev".
func WithVersion(v string) Option {
	return func(r *Router) {
		if v != "" {
			r.version = v
		}
	}
}

// WithWellKnown configures the GET /.well-known/opencode endpoint. An empty
// ProviderID leaves the handler returning 404 (the default), so the router
// can be deployed before the OpenCode well-known URL is decided.
func WithWellKnown(cfg WellKnownConfig) Option {
	return func(r *Router) { r.wellKnown = cfg }
}

// New constructs a Router bound to the given registry. The routable model set
// is computed once from the configured mode — reload the process to pick up a
// changed models.yaml (matching the Python proxy's load-time behaviour).
func New(registry *config.ModelRegistry, logger *slog.Logger, opts ...Option) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Router{
		registry:      registry,
		logger:        logger,
		transport:     http.DefaultTransport,
		flushInterval: -1, // SSE: flush on every write
		getenv:        os.Getenv,
		sink:          reqlog.NopSink{},
		version:       "dev",
		started:       time.Now(),
		nodeFetcher:   fetchNodeMetrics,
	}
	for _, opt := range opts {
		opt(r)
	}
	r.active = registry.ModelsForMode(r.mode)
	r.metrics = newRouterMetrics(r.version, r.started, r.active)
	return r
}

// Handler returns the HTTP mux. Wrap with the standard httpx middleware chain
// (RequestID, AccessLog, Recover) in main.
func (rt *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	// Chat-completions respects tool_proxy routing (the tool loop applies).
	mux.HandleFunc("POST /v1/chat/completions", rt.handleProxy(config.APIClassChat, false))
	// /v1/completions speaks the same chat models but bypasses the tool proxy
	// (no tools/reasoning on plain text completion; the proxy doesn't serve
	// this path anyway).
	mux.HandleFunc("POST /v1/completions", rt.handleProxy(config.APIClassChat, true))
	// Embeddings + rerank bypass the tool proxy and require their own classes.
	mux.HandleFunc("POST /v1/embeddings", rt.handleProxy(config.APIClassEmbeddings, true))
	mux.HandleFunc("POST /v1/rerank", rt.handleProxy(config.APIClassRerank, true))

	mux.HandleFunc("GET /v1/models", rt.handleModels)
	mux.HandleFunc("GET /health", rt.handleHealth)
	mux.Handle("GET /metrics", rt.metrics.Handler())
	mux.HandleFunc("GET /.well-known/opencode", rt.handleWellKnown)
	return mux
}

// ---------------------------------------------------------------------------
// Generic POST handler — resolve, enforce api_class, rewrite model, forward.
// ---------------------------------------------------------------------------

// handleProxy returns a handler that resolves the request's model under the
// given constraints and reverse-proxies the (model-rewritten) body upstream.
// Every request that reaches this handler emits one reqlog.Record on exit,
// including the rejected paths (bad JSON, missing model, unknown model,
// api_class mismatch) so the dashboard sees them too.
//
//   - requireClass: the model's api_class must match exactly; "" disables the
//     check (currently every endpoint passes a concrete class).
//   - forceDirect: bypass tool-proxy routing — set for endpoints the tool
//     proxy doesn't implement (/v1/completions, /v1/embeddings, /v1/rerank).
func (rt *Router) handleProxy(requireClass config.APIClass, forceDirect bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recordingWriter{ResponseWriter: w}
		cap := &responseCapture{}

		var (
			modelIn  string
			resolved *resolveResult
			errMsg   string
		)

		defer func() {
			lr := reqlog.Record{
				RequestID: httpx.RequestIDFromContext(r.Context()),
				TS:        start,
				Method:    r.Method,
				Path:      r.URL.Path,
				Model:     modelIn,
				Status:    rec.status,
				LatencyMS: int(time.Since(start) / time.Millisecond),
				Stream:    cap.isSSE,
				Error:     errMsg,
			}
			if resolved != nil {
				lr.BackendModel = resolved.BackendModel
				lr.BackendURL = resolved.BackendURL
				lr.ResolvedVia = resolved.ModelID
				lr.APIClass = string(resolved.APIClass)
				lr.ViaToolProxy = resolved.ViaToolProxy
			}
			switch {
			case cap.jsonBody != nil:
				lr.PromptTokens, lr.CompletionTokens, lr.TotalTokens = parseUsage(cap.jsonBody)
			case cap.sseTail != nil:
				lr.PromptTokens, lr.CompletionTokens, lr.TotalTokens = extractSSEUsage(cap.sseTail.Tail())
			}
			rt.sink.Log(lr)
			rt.metrics.Observe(lr)
		}()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			errMsg = "read body: " + err.Error()
			http.Error(rec, errMsg, http.StatusBadRequest)
			return
		}

		var bodyMap map[string]any
		if err := json.Unmarshal(body, &bodyMap); err != nil {
			errMsg = "invalid JSON: " + err.Error()
			http.Error(rec, errMsg, http.StatusBadRequest)
			return
		}

		model, _ := bodyMap["model"].(string)
		if model == "" {
			errMsg = `missing "model" field`
			http.Error(rec, errMsg, http.StatusBadRequest)
			return
		}
		modelIn = model

		res, err := rt.resolveModel(model, forceDirect)
		if err != nil {
			errMsg = err.Error()
			rt.logger.WarnContext(r.Context(), "resolve failed", "model", model, "err", err)
			http.Error(rec, errMsg, http.StatusNotFound)
			return
		}
		resolved = &res

		if requireClass != "" && res.APIClass != requireClass {
			errMsg = fmt.Sprintf("model %q has api_class %q; %s requires %q",
				model, res.APIClass, r.URL.Path, requireClass)
			rt.logger.WarnContext(r.Context(), "api_class mismatch",
				"model", model, "got", string(res.APIClass), "want", string(requireClass), "path", r.URL.Path)
			http.Error(rec, errMsg, http.StatusBadRequest)
			return
		}

		// E2: a "<model>-<egress>" alias resolves to a tool-proxy model plus an
		// egress spec; forward it as X-Egress so the tool proxy picks the VPN
		// exit. An explicit client X-Egress header always wins.
		if res.ViaToolProxy && res.Egress != "" && r.Header.Get("X-Egress") == "" {
			r.Header.Set("X-Egress", res.Egress)
		}

		bodyMap["model"] = res.BackendModel
		newBody, err := json.Marshal(bodyMap)
		if err != nil {
			errMsg = "re-encode body: " + err.Error()
			http.Error(rec, errMsg, http.StatusInternalServerError)
			return
		}

		rt.logger.InfoContext(r.Context(), "forwarding",
			"path", r.URL.Path,
			"model", model, "backend_model", res.BackendModel,
			"backend_url", res.BackendURL, "resolved_via", res.ModelID,
			"via_tool_proxy", res.ViaToolProxy)
		rt.reverseProxyTo(rec, r, res.BackendURL, newBody, res.AuthBearer, cap)
	}
}

// reverseProxyTo forwards the request to backendRoot (a base URL with the
// "/v1" suffix already stripped) using the original request path, preserving
// SSE streaming via FlushInterval. SetURL joins backendRoot's path with the
// inbound path, so any path prefix on an external api_base is kept and the
// endpoint generalises to /v1/completions, /v1/embeddings, etc. When
// authBearer is non-empty it replaces the Authorization header (external
// providers); local/tool-proxy hops pass "".
//
// cap (non-nil) records what we learn about the response for the reqlog
// sink: for JSON responses we buffer the body so usage tokens can be parsed
// in handleProxy's defer; for SSE we wrap the response body with a rolling
// 64KB tail buffer so the final `usage` chunk (if present) is captured
// without holding the whole stream in memory.
func (rt *Router) reverseProxyTo(w http.ResponseWriter, r *http.Request, backendRoot string, body []byte, authBearer string, cap *responseCapture) {
	target, err := url.Parse(backendRoot)
	if err != nil {
		http.Error(w, "bad backend URL: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rp := &httputil.ReverseProxy{
		Transport:     rt.transport,
		FlushInterval: rt.flushInterval,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target) // scheme+host, joins target.Path with the inbound path
			pr.Out.Host = ""  // use the new Host from URL
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
			pr.Out.Header.Set("Content-Length", strconv.Itoa(len(body)))
			if authBearer != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+authBearer)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if cap == nil {
				return nil
			}
			ct := resp.Header.Get("Content-Type")
			switch {
			case contentTypeIsSSE(ct):
				cap.isSSE = true
				cap.sseTail = newStreamTailCapture(resp.Body, 64*1024)
				resp.Body = cap.sseTail
			case contentTypeIsJSON(ct):
				// Buffer the JSON body so we can parse `usage` after it's
				// forwarded. Bodies for chat/embedding/rerank responses are
				// small (KB), so reading them in full is fine.
				buf, err := io.ReadAll(resp.Body)
				if err != nil {
					return err
				}
				if err := resp.Body.Close(); err != nil {
					return err
				}
				cap.jsonBody = buf
				resp.Body = io.NopCloser(bytes.NewReader(buf))
				resp.ContentLength = int64(len(buf))
				resp.Header.Set("Content-Length", strconv.Itoa(len(buf)))
			}
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			rt.logger.ErrorContext(req.Context(), "upstream error",
				"backend_url", backendRoot, "err", err)
			http.Error(rw, "upstream: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// /v1/models — OpenAI-shape list of every model routable in the current mode.
// ---------------------------------------------------------------------------

func (rt *Router) handleModels(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		ID       string          `json:"id"`
		Object   string          `json:"object"`
		OwnedBy  string          `json:"owned_by"`
		APIClass config.APIClass `json:"api_class,omitempty"`
	}
	type response struct {
		Object string  `json:"object"`
		Data   []entry `json:"data"`
	}

	// Emit canonical IDs + all aliases as separate entries, matching the
	// LiteLLM-era shape so client tools that enumerate /v1/models (rather
	// than the well-known) see every routable name. Aliases share the
	// canonical model's backend and api_class — they're routing handles,
	// not separate models.
	out := response{Object: "list"}
	seen := make(map[string]struct{}, len(rt.active)*2)
	add := func(id string, m config.ModelDefinition) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out.Data = append(out.Data, entry{
			ID:       id,
			Object:   "model",
			OwnedBy:  string(m.Backend),
			APIClass: m.APIClass,
		})
	}
	// Stable order: walk canonical IDs alphabetically, emit canonical
	// before its aliases. Tests assert on presence not order, but stable
	// ordering keeps diffs across deploys reviewable.
	ids := make([]string, 0, len(rt.active))
	for id := range rt.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := rt.active[id]
		add(id, m)
		for _, a := range m.Aliases {
			add(a, m)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---------------------------------------------------------------------------
// /health
// ---------------------------------------------------------------------------

func (rt *Router) handleHealth(w http.ResponseWriter, r *http.Request) {
	modelsByClass := map[string]int{}
	for _, m := range rt.active {
		modelsByClass[string(m.APIClass)]++
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":          "ok",
		"version":         rt.version,
		"mode":            rt.mode,
		"uptime_seconds":  time.Since(rt.started).Seconds(),
		"models":          len(rt.active),
		"models_by_class": modelsByClass,
		"streaming":       true,
	})
}
