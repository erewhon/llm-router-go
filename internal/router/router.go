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
	"errors"
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

	"github.com/erewhon/llm-router-go/internal/auth"
	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/httpx"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// Router serves the OpenAI front-door endpoints over a model registry.
type Router struct {
	registry *config.ModelRegistry
	active   map[string]config.ModelDefinition // models routable in this mode
	// roles are the semantic handles routable in this mode, with their
	// candidate lists already filtered to `active` (so mode tags and disabled
	// rollback entries drop out of the preference order).
	roles map[string]config.RoleDefinition
	// avail decides whether a model can be routed to right now. Never nil:
	// New installs alwaysRoutable when tracking is disabled.
	avail         availability
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
	// tokStats holds the most recent measured tok/s per model, learned from
	// response usage while proxying. Feeds the dashboard throughput tile.
	tokStats *tokTracker
	// upstreamStats is the rolling 24h window of upstream attempt outcomes per
	// (model, endpoint). Feeds the dashboard's /api/upstream failure panel.
	upstreamStats *upstreamTracker
	// zdrCanary reports whether the OpenRouter account is still enforcing
	// zero data retention account-wide. Nil when unconfigured; Status() is
	// nil-safe so /health needs no branch.
	zdrCanary *ZDRCanary
}

// Option configures a Router at construction time.
type Option func(*Router)

// WithZDRCanary attaches the account-posture canary, whose last verdict is
// reported on /health. Purely observational: it never gates a request.
func WithZDRCanary(c *ZDRCanary) Option {
	return func(r *Router) { r.zdrCanary = c }
}

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

// WithAvailability wires the health tracker that decides which of a role's
// candidates can be routed to right now. Omitting it (or passing nil) leaves
// every model routable, which is exactly the pre-roles behaviour: roles still
// resolve, they just always pick their first candidate.
func WithAvailability(a availability) Option {
	return func(r *Router) {
		if a != nil {
			r.avail = a
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
		tokStats:      newTokTracker(),
		upstreamStats: newUpstreamTracker(),
		avail:         alwaysRoutable{},
	}
	for _, opt := range opts {
		opt(r)
	}
	r.active = registry.ModelsForMode(r.mode)
	r.roles = registry.RolesForMode(r.mode)
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
	// Images passthrough (sd-cpp creative backends, OpenAI images shapes).
	// Generations is JSON; edits is multipart/form-data — see images.go.
	mux.HandleFunc("POST /v1/images/generations", rt.handleProxy(config.APIClassImageGen, true))
	mux.HandleFunc("POST /v1/images/edits", rt.handleProxyMultipart(config.APIClassImageEdit))
	// TTS passthrough (Orpheus, OpenAI /v1/audio/speech shape). The response
	// is raw audio (audio/wav); ModifyResponse leaves non-JSON, non-SSE
	// bodies untouched, so it streams through with no usage capture.
	mux.HandleFunc("POST /v1/audio/speech", rt.handleProxy(config.APIClassTTS, true))

	// Anthropic Messages passthrough — registered only when an
	// api_class:anthropic target is configured. Both paths bypass the
	// front-door bearer (see AnthropicPaths / main.go) and forward the client's
	// own credentials untouched.
	if id, root := rt.anthropicBackendRoot(); root != "" {
		h := rt.handleAnthropic(id, root)
		mux.HandleFunc("POST /v1/messages", h)
		mux.HandleFunc("POST /v1/messages/count_tokens", h)
		rt.logger.Info("anthropic passthrough enabled", "backend_url", root, "resolved_via", id)
	}

	mux.HandleFunc("GET /v1/models", rt.handleModels)
	mux.HandleFunc("GET /v1/availability", rt.handleAvailability)
	mux.HandleFunc("GET /health", rt.handleHealth)
	mux.Handle("GET /metrics", rt.metrics.Handler())
	mux.HandleFunc("GET /.well-known/opencode", rt.handleWellKnown)
	return mux
}

// AnthropicPaths are the passthrough endpoints that must be exempt from the
// front-door bearer auth: the Anthropic Messages API carries the caller's own
// credentials, which the router forwards untouched. Wired into RequireBearer's
// exempt list by main.go.
var AnthropicPaths = []string{"/v1/messages", "/v1/messages/count_tokens"}

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
			modelIn string
			// resolved tracks the CURRENT target: after a failover it names
			// the candidate that actually served the request, so the reqlog
			// row attributes tokens and latency to the right model.
			resolved *resolveResult
			// failoverFrom names the model that failed, when one did.
			failoverFrom string
			errMsg       string
			// upstreamClass is the transport-error class of the FINAL attempt
			// ("timeout"/"connect"/"transport"), empty when the upstream
			// answered with a status line (whatever it was) or was never tried.
			upstreamClass string
			// privacyTolerance names the tier enforced on this request, for the
			// reqlog row. Declared up here because the deferred record closes
			// over it, and a refused request must log the tier that refused it
			// — that row is the evidence the refusal happened.
			privacyTolerance string
			// privacyRefused marks a request the tier turned away, so reqlog
			// and metrics can count it as a refusal rather than an error or
			// an outage.
			privacyRefused bool
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
			// Attribution. Empty on auth-exempt paths and on a router running
			// without auth; empty means unattributed, not trusted.
			lr.Principal, lr.TokenID = auth.PrincipalFromContext(r.Context())
			if resolved != nil {
				lr.BackendModel = resolved.BackendModel
				lr.BackendURL = resolved.BackendURL
				lr.ResolvedVia = resolved.ModelID
				lr.APIClass = string(resolved.APIClass)
				lr.ViaToolProxy = resolved.ViaToolProxy
				lr.Role = resolved.Role
				lr.RoleOverflowed = resolved.Overflowed
				lr.FailoverFrom = failoverFrom
				// Upstream outcome of the final attempt. Envelope detection is
				// JSON-body-only by design: a 2xx JSON body carrying a
				// top-level "error" is a failure wearing a success status.
				envelope := cap.upstreamStatus >= 200 && cap.upstreamStatus < 300 &&
					cap.jsonBody != nil && hasErrorEnvelope(cap.jsonBody)
				lr.UpstreamStatus = cap.upstreamStatus
				lr.ErrorClass = errorClassOf(upstreamClass, cap.upstreamStatus, envelope)
				if cap.upstreamStatus > 0 || upstreamClass != "" {
					rt.upstreamStats.record(resolved.ModelID, resolved.BackendURL, lr.ErrorClass, time.Now())
				}
			}
			lr.PrivacyTolerance = privacyTolerance
			if privacyRefused {
				// Distinct from every upstream class: nothing was sent, so
				// this must not count against any model's failure rate, and
				// it must not fold into the 5xx error rate on the dashboard.
				lr.ErrorClass = errorClassPrivacyRefused
			}
			var usage usageStats
			switch {
			case cap.jsonBody != nil:
				usage = parseUsage(cap.jsonBody)
				lr.UpstreamProvider = parseUpstreamProvider(cap.jsonBody)
			case cap.sseTail != nil:
				usage = extractSSEUsage(cap.sseTail.Tail())
				lr.UpstreamProvider = extractSSEProvider(cap.sseTail.Tail())
			}
			lr.PromptTokens, lr.CompletionTokens, lr.TotalTokens = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
			lr.UpstreamCostUSD, lr.CachedPromptTokens = usage.CostUSD, usage.CachedPromptTokens
			tokPerSec := usage.TokPerSec
			if resolved != nil {
				rt.tokStats.record(resolved.ModelID, tokPerSec, lr.CompletionTokens, lr.LatencyMS, time.Now())
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

		// Size the request before resolving: role candidates whose context
		// envelope this exceeds get skipped in the walk (see envelope.go).
		// The whole body is the input, tool definitions and transcript
		// included — an agent loop's last message says nothing about its size.
		promptTokens := estimatePromptTokens(body)

		// The caller's privacy tier is read BEFORE resolution: it filters
		// which candidates a role may consider, so a role listing a cloud
		// seat first still serves a `local` request from a local seat lower
		// down. A malformed value is refused here, before any seat is chosen.
		callerTier, privSource, privRefuse := parsePrivacyHeader(r)
		if privRefuse != "" {
			errMsg = privRefuse
			privacyRefused = true
			rt.metrics.ObservePrivacyRefusal("invalid", "")
			writePrivacyRefusal(rec, tierNone, privRefuse, nil)
			return
		}

		res, err := rt.resolveModel(model, forceDirect, promptTokens, callerTier)
		if err != nil {
			errMsg = err.Error()
			// The privacy tier excluded every candidate and none was merely
			// down. 403, not 503: the caller's policy did this, no retry can
			// change it, and a retry-on-5xx client must not treat it as an
			// outage. The reasons ride in the body so the caller can see what
			// a looser tier would have reached.
			var privErr *privacyRefusedError
			if errors.As(err, &privErr) {
				privacyTolerance = privErr.Tier.String()
				privacyRefused = true
				rt.logger.WarnContext(r.Context(), "refusing on privacy tier: no compliant candidate",
					"subject", privErr.Subject, "tier", privErr.Tier.String(), "excluded", privErr.Excluded)
				rt.metrics.ObservePrivacyRefusal(privErr.Tier.String(), privErr.Subject)
				writePrivacyRefusal(rec, privErr.Tier, errMsg, privErr.Excluded)
				return
			}
			// A role that resolved to nothing is a fleet-state problem, not a
			// client mistake: 503 with the per-candidate reasons, so callers
			// can tell "that name doesn't exist" from "everything that serves
			// this intent is powered off right now".
			var roleErr *roleUnavailableError
			if errors.As(err, &roleErr) {
				rt.logger.WarnContext(r.Context(), "role unavailable",
					"role", roleErr.Role, "reasons", roleErr.Reasons,
					"prompt_tokens_est", promptTokens)
				http.Error(rec, errMsg, http.StatusServiceUnavailable)
				return
			}
			// Same for a fallback chain whose every provider is down: the name
			// exists, the fleet state is the problem.
			var chainErr *chainUnavailableError
			if errors.As(err, &chainErr) {
				rt.logger.WarnContext(r.Context(), "chain unavailable",
					"chain", chainErr.Chain, "reasons", chainErr.Reasons)
				http.Error(rec, errMsg, http.StatusServiceUnavailable)
				return
			}
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

		// Forward, advancing along the role's preference order if an upstream
		// fails to answer at all. Only role-resolved requests fail over:
		// naming a model is a statement about that model.
		// The tier the walk was held to (role ∨ caller) for a role or chain;
		// for a directly named model there was no walk, so it is the caller's.
		privTier := res.PrivacyTier
		if privTier == tierNone {
			privTier = callerTier
		}
		privacyTolerance = privTier.String()
		if privTier != tierNone && privSource == "" {
			privSource = "role " + res.Role + " requires " + string(config.LocalityLocalOrZDR)
		}

		for attempt := 0; ; attempt++ {
			bodyMap["model"] = res.BackendModel
			// Re-applied on every attempt: this puts the ZDR directive on the
			// wire for the seat actually being tried, and — for a directly
			// named model, which went through no candidate walk — it is the
			// only place the caller's tier is enforced at all.
			if privTier != tierNone {
				if refusal := rt.applyPrivacy(bodyMap, res, privTier, privSource); refusal != "" {
					rt.logger.WarnContext(r.Context(), "refusing on privacy tier",
						"model", model, "resolved_via", res.ModelID, "role", res.Role,
						"tier", privTier.String(), "reason", refusal)
					errMsg = refusal
					privacyRefused = true
					rt.metrics.ObservePrivacyRefusal(privTier.String(), coalesce(res.Role, res.Chain))
					writePrivacyRefusal(rec, privTier, refusal, nil)
					return
				}
				rec.Header().Set(PrivacyHeader, privTier.String())
			}
			newBody, err := json.Marshal(bodyMap)
			if err != nil {
				errMsg = "re-encode body: " + err.Error()
				http.Error(rec, errMsg, http.StatusInternalServerError)
				return
			}

			rt.setRoleHeaders(rec, res)
			rt.logger.InfoContext(r.Context(), "forwarding",
				"path", r.URL.Path,
				"model", model, "backend_model", res.BackendModel,
				"backend_url", res.BackendURL, "resolved_via", res.ModelID,
				"role", res.Role, "chain", res.Chain, "overflowed", res.Overflowed,
				"via_tool_proxy", res.ViaToolProxy,
				"prompt_tokens_est", res.PromptTokens, "downshift_from", res.Downshift)

			// Chains also retry on upstream 5xx / error-envelope-in-2xx — but
			// only while another provider could actually take the request. On
			// the last viable candidate the response passes through untouched,
			// so the client sees the provider's own error, not a synthetic 502.
			suppressRetryable := false
			if res.Chain != "" && rec.status == 0 && attempt < maxFailoverAttempts {
				_, suppressRetryable = rt.nextRoleCandidate(res, forceDirect)
			}

			upstreamErr := rt.reverseProxyTo(rec, r, res.BackendURL, newBody, res.AuthBearer, res.AuthHeader, cap, suppressRetryable)
			if upstreamErr == nil {
				// A 503 that passed through from a fleet seat is the "Loading
				// model" shape: the listing is up, generation is not. Tell the
				// tracker so the seat goes back to warming instead of taking
				// the next request too. Only fleet seats — a provider's 503 is
				// its own business and stays with the breaker's chain path.
				if cap.upstreamStatus == http.StatusServiceUnavailable && rt.isLocalSeat(res.ModelID) {
					rt.avail.ReportFailure(res.ModelID, &errUpstreamStatus{Status: http.StatusServiceUnavailable})
				} else {
					rt.avail.ReportSuccess(res.ModelID)
				}
				return
			}

			// The upstream failed (never answered, or answered with a
			// suppressed retryable failure). Tell the tracker — this is
			// stronger evidence than a poll, which only proves the agent is
			// alive.
			rt.avail.ReportFailure(res.ModelID, upstreamErr)

			// rec.status == 0 means not a single byte has reached the client,
			// which is the only window in which retrying is honest rather than
			// a corrupted response.
			next, ok := resolveResult{}, false
			if (res.Role != "" || res.Chain != "") && attempt < maxFailoverAttempts && rec.status == 0 {
				next, ok = rt.nextRoleCandidate(res, forceDirect)
			}
			if !ok {
				upstreamClass = classifyUpstreamErr(upstreamErr)
				errMsg = "upstream: " + upstreamErr.Error()
				if rec.status == 0 {
					http.Error(rec, errMsg, http.StatusBadGateway)
				}
				return
			}

			rt.logger.WarnContext(r.Context(), "failing over",
				"role", res.Role, "chain", res.Chain, "from", res.ModelID, "to", next.ModelID, "err", upstreamErr)
			rt.metrics.ObserveFailover(coalesce(res.Role, res.Chain), res.ModelID, next.ModelID)
			// The failed attempt won't be the reqlog record's final attempt, so
			// count it toward the failed candidate's upstream stats here.
			class := classifyUpstreamErr(upstreamErr)
			rt.metrics.ObserveUpstream(res.ModelID, res.BackendURL, class)
			rt.upstreamStats.record(res.ModelID, res.BackendURL, class, time.Now())
			failoverFrom = res.ModelID
			res = next
			resolved = &res
			// A retry starts a fresh response: drop whatever the failed attempt
			// left in the capture so usage isn't attributed to the wrong model.
			*cap = responseCapture{}
		}
	}
}

// isLocalSeat reports whether a resolved model is pinned to fleet hardware —
// the seats whose 503s mean "still loading" rather than "provider trouble".
func (rt *Router) isLocalSeat(id string) bool {
	m, ok := rt.registry.Models[id]
	return ok && m.IsLocal()
}

// maxFailoverAttempts caps how far down a role's preference order one request
// will walk. Two retries covers "both Sparks are off" without turning a
// fleet-wide outage into a slow serial scan of every candidate.
const maxFailoverAttempts = 2

// setRoleHeaders tells the caller which concrete model actually served a role
// request, and whether it had to cross the locality boundary to do it. Set
// before forwarding so they survive the ReverseProxy writing its own headers.
func (rt *Router) setRoleHeaders(w http.ResponseWriter, res resolveResult) {
	if res.Role == "" && res.Chain == "" {
		return
	}
	h := w.Header()
	if res.Role != "" {
		h.Set("X-Router-Role", res.Role)
	}
	if res.Chain != "" {
		h.Set("X-Router-Chain", res.Chain)
	}
	h.Set("X-Router-Resolved", res.ModelID)
	if res.Overflowed {
		h.Set("X-Router-Overflow", "true")
	}
	// The soft gate binds, so a caller who asked for a role and got something
	// other than its first choice has no other way to find out. Never silent —
	// same principle as X-Router-Overflow.
	if res.Downshift != "" {
		h.Set("X-Router-Downshift", fmt.Sprintf("%s -> %s (context)", res.Downshift, res.ModelID))
	}
}

// coalesce returns the first non-empty string.
func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reverseProxyTo forwards the request to backendRoot (a base URL with the
// "/v1" suffix already stripped) using the original request path, preserving
// SSE streaming via FlushInterval. SetURL joins backendRoot's path with the
// inbound path, so any path prefix on an external api_base is kept and the
// endpoint generalises to /v1/completions, /v1/embeddings, etc. When
// authBearer is non-empty it is attached as the upstream credential (external
// providers); local/tool-proxy hops pass "". By default it replaces the
// Authorization header as "Bearer <authBearer>"; when authHeader is non-empty
// the key is instead written verbatim to that header (e.g. "X-Api-Key") and any
// inbound Authorization header is stripped so the router's own token never
// leaks upstream.
//
// cap (non-nil) records what we learn about the response for the reqlog
// sink: for JSON responses we buffer the body so usage tokens can be parsed
// in handleProxy's defer; for SSE we wrap the response body with a rolling
// 64KB tail buffer so the final `usage` chunk (if present) is captured
// without holding the whole stream in memory.
// It returns the upstream transport error, if any, WITHOUT writing a 502 —
// the caller decides whether to fail over to another candidate first. A nil
// return means the response was proxied (whatever its status). When the error
// arrives after bytes have already reached the client (a mid-stream copy
// failure) the caller can see that via the recordingWriter's status and must
// not retry.
//
// suppressRetryable, set by the chain-failover path when another provider
// could still take the request, additionally converts a retryable upstream
// failure — a 5xx status, or an error envelope inside a 2xx JSON body — into
// an error return WITHOUT writing anything to the client, so the caller can
// retry the next provider. Off (the default) preserves passthrough: whatever
// the upstream said streams to the client.
func (rt *Router) reverseProxyTo(w http.ResponseWriter, r *http.Request, backendRoot string, body []byte, authBearer, authHeader string, cap *responseCapture, suppressRetryable bool) error {
	target, err := url.Parse(backendRoot)
	if err != nil {
		http.Error(w, "bad backend URL: "+err.Error(), http.StatusInternalServerError)
		return nil
	}
	var upstreamErr error
	rp := &httputil.ReverseProxy{
		Transport:     rt.transport,
		FlushInterval: rt.flushInterval,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target) // scheme+host, joins target.Path with the inbound path
			pr.Out.Host = ""  // use the new Host from URL
			// Per outbound attempt, on the copy: a chain failing over from a
			// zen/ member to an or/ one rebuilds pr.Out from the inbound
			// request, so a session header added for Zen never reaches
			// OpenRouter. See session.go.
			setSessionAffinity(pr.Out, target, body)
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
			pr.Out.Header.Set("Content-Length", strconv.Itoa(len(body)))
			if authBearer != "" {
				if authHeader != "" {
					pr.Out.Header.Del("Authorization")
					pr.Out.Header.Set(authHeader, authBearer)
				} else {
					pr.Out.Header.Set("Authorization", "Bearer "+authBearer)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if cap == nil {
				return nil
			}
			cap.upstreamStatus = resp.StatusCode
			if suppressRetryable && resp.StatusCode >= 500 {
				// The next provider gets the request instead. Drain (bounded)
				// so the connection can be reused, then surface the status as
				// an error: ReverseProxy routes it to ErrorHandler, which
				// writes nothing.
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				return &errUpstreamStatus{Status: resp.StatusCode}
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
				// An error envelope inside a 2xx is a failure wearing a
				// success status (the 2026-08-27 Zen shape) — retry it like a
				// 5xx while a next provider exists.
				if suppressRetryable && resp.StatusCode < 300 && hasErrorEnvelope(buf) {
					return errUpstreamEnvelope
				}
				cap.jsonBody = buf
				resp.Body = io.NopCloser(bytes.NewReader(buf))
				resp.ContentLength = int64(len(buf))
				resp.Header.Set("Content-Length", strconv.Itoa(len(buf)))
			}
			return nil
		},
		ErrorHandler: func(_ http.ResponseWriter, req *http.Request, err error) {
			rt.logger.ErrorContext(req.Context(), "upstream error",
				"backend_url", backendRoot, "err", err)
			// Deliberately write nothing: handleProxy may still fail over to
			// the role's next candidate, and it can only do that if the
			// response is still untouched.
			upstreamErr = err
		},
	}
	rp.ServeHTTP(w, r)
	return upstreamErr
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
		// Role marks an entry that is a semantic handle rather than a model.
		// Clients can route to it exactly like a model name; the difference is
		// that what answers may change as the fleet powers up and down.
		Role bool `json:"role,omitempty"`
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

	// Roles are routable names too — for most clients they are the names to
	// prefer, since they survive a node being powered down. Listed after the
	// concrete models, flagged so a client can tell them apart. A role's
	// api_class comes from its current target: every candidate shares one by
	// config validation when the role declares require.api_class, and in
	// practice they are all chat.
	for _, name := range rt.RoleNames() {
		if _, dup := seen[name]; dup {
			continue
		}
		e := entry{ID: name, Object: "model", OwnedBy: "role", Role: true}
		if res, err := rt.resolveRole(name, name, false, 0, tierNone); err == nil {
			e.APIClass = res.APIClass
		} else if len(rt.roles[name].Candidates) > 0 {
			// Nothing available right now: still advertise the role (it will
			// come back when the fleet does) using its first candidate's class.
			if m, ok := rt.active[rt.roles[name].Candidates[0]]; ok {
				e.APIClass = m.APIClass
			}
		}
		seen[name] = struct{}{}
		out.Data = append(out.Data, e)
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
	body := map[string]any{
		"status":          "ok",
		"version":         rt.version,
		"mode":            rt.mode,
		"uptime_seconds":  time.Since(rt.started).Seconds(),
		"models":          len(rt.active),
		"models_by_class": modelsByClass,
		"streaming":       true,
		// Reported unconditionally, including as "unknown". An absent field
		// reads as "fine"; a field saying "unknown" reads as "nobody has
		// checked", which is the truth and is actionable.
		"zdr_account": rt.zdrCanary.Status(),
	}
	// Verdict tallies over the active set, so a seat stuck warming shows up
	// on the endpoint every monitor already hits. Per-model detail is on
	// /v1/availability.
	if rep, ok := rt.avail.(availabilityReporter); ok {
		counts := map[string]int{}
		for _, s := range rep.Snapshot() {
			if _, active := rt.active[s.Model]; active {
				counts[string(s.State)]++
			}
		}
		body["availability"] = counts
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
