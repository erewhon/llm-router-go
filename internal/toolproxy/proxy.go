// Package toolproxy is the Go rewrite of the Python tool proxy. It serves
// OpenAI chat-completions, resolving the model to a real upstream via the
// registry and rewriting the "model" field. Requests that don't involve
// proxy-owned tools are reverse-proxied straight through with SSE streaming
// intact (Phase 2a). When proxy tools apply, the request runs through the
// tool-execution loop instead: the proxy injects its tool definitions, calls
// the backend, executes any proxy-owned tool calls, and continues the chat
// until the model answers (Phase 2b).
//
// The auto-router (2c) and full reasoning-tag passthrough (2d) are still to
// come; backend-provided reasoning_content is forwarded where it's free.
package toolproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/toolproxy/tools"
)

// Proxy serves /v1/chat/completions, /v1/models, and /health.
type Proxy struct {
	registry       *config.ModelRegistry
	logger         *slog.Logger
	transport      http.RoundTripper
	flushInterval  time.Duration
	tools          *tools.Registry
	maxToolRounds  int
	backendTimeout time.Duration
	backendHTTP    *http.Client
}

// Option configures a Proxy at construction time.
type Option func(*Proxy)

// WithTransport overrides the HTTP transport used to talk to upstreams.
// Used by tests; production gets http.DefaultTransport.
func WithTransport(rt http.RoundTripper) Option {
	return func(p *Proxy) { p.transport = rt }
}

// WithFlushInterval overrides the ReverseProxy flush interval. Default
// is -1 (flush on every write), which is what SSE streaming requires.
// Tests can set 0 to behave like normal buffered HTTP.
func WithFlushInterval(d time.Duration) Option {
	return func(p *Proxy) { p.flushInterval = d }
}

// WithTools wires a tool registry into the proxy. When the resolved model is
// not tagged "nothink" and the registry is non-empty, the proxy injects these
// tools and runs the tool-execution loop; otherwise it reverse-proxies straight
// through. Nil (the default) means no tools — pure passthrough.
func WithTools(r *tools.Registry) Option {
	return func(p *Proxy) { p.tools = r }
}

// WithMaxToolRounds caps how many tool-execution rounds the loop runs before
// returning whatever the model last produced. Default 5 (matches Python).
func WithMaxToolRounds(n int) Option {
	return func(p *Proxy) {
		if n > 0 {
			p.maxToolRounds = n
		}
	}
}

// WithBackendTimeout sets the per-call timeout for the loop's non-streaming
// backend requests. Default 600s (matches the Python httpx timeout).
func WithBackendTimeout(d time.Duration) Option {
	return func(p *Proxy) {
		if d > 0 {
			p.backendTimeout = d
		}
	}
}

// New constructs a Proxy bound to the given registry.
func New(registry *config.ModelRegistry, logger *slog.Logger, opts ...Option) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Proxy{
		registry:       registry,
		logger:         logger,
		transport:      http.DefaultTransport,
		flushInterval:  -1, // SSE: flush on every write
		maxToolRounds:  5,
		backendTimeout: 600 * time.Second,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.backendHTTP = &http.Client{Transport: p.transport, Timeout: p.backendTimeout}
	return p
}

// Handler returns the HTTP mux. Wrap with the standard httpx middleware
// chain (RequestID, AccessLog, Recover) in the main.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", p.handleChat)
	mux.HandleFunc("GET /v1/models", p.handleModels)
	mux.HandleFunc("GET /health", p.handleHealth)
	return mux
}

// ---------------------------------------------------------------------------
// /v1/chat/completions
// ---------------------------------------------------------------------------

func (p *Proxy) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var bodyMap map[string]any
	if err := json.Unmarshal(body, &bodyMap); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	model, _ := bodyMap["model"].(string)
	if model == "" {
		http.Error(w, `missing "model" field`, http.StatusBadRequest)
		return
	}

	res, err := resolveModel(p.registry, model)
	if err != nil {
		// Not-found and external-misroute are client errors; everything
		// else (config bugs) we let bubble as 500.
		p.logger.WarnContext(r.Context(), "resolve failed", "model", model, "err", err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Rewrite the model field to the backend's expected name for every path.
	bodyMap["model"] = res.BackendModel

	if !p.shouldInjectTools(res.ModelID) {
		// Passthrough (Phase 2a): no proxy tools apply, so reverse-proxy the
		// (model-rewritten) body straight through with SSE streaming intact.
		newBody, err := json.Marshal(bodyMap)
		if err != nil {
			http.Error(w, "re-encode body: "+err.Error(), http.StatusInternalServerError)
			return
		}
		p.logger.InfoContext(r.Context(), "forwarding (passthrough)",
			"model", model, "backend_model", res.BackendModel,
			"backend_url", res.BackendURL, "resolved_via", res.ModelID)
		p.reverseProxyTo(w, r, res.BackendURL, newBody)
		return
	}

	// Tool-loop path (Phase 2b): inject proxy tools and drive the conversation.
	messages, _ := bodyMap["messages"].([]any)
	if messages == nil {
		messages = []any{} // marshal as [] not null, matching Python's body.get("messages", [])
	}
	clientTools, _ := bodyMap["tools"].([]any)
	allTools := p.mergeTools(clientTools)
	toolChoice := any("auto")
	if tc, ok := bodyMap["tool_choice"]; ok {
		toolChoice = tc
	}

	p.logger.InfoContext(r.Context(), "forwarding (tool loop)",
		"model", model, "backend_model", res.BackendModel,
		"backend_url", res.BackendURL, "resolved_via", res.ModelID,
		"tools", len(allTools))

	if stream, _ := bodyMap["stream"].(bool); stream {
		p.runToolLoopStreaming(w, r, res, bodyMap, messages, allTools, toolChoice)
		return
	}
	p.runToolLoopJSON(w, r, res, bodyMap, messages, allTools, toolChoice)
}

// shouldInjectTools reports whether the resolved model should run through the
// tool loop: tools are configured, the registry is non-empty, and the model
// isn't tagged "nothink" (those skip tool injection to avoid loop overhead,
// matching the Python proxy).
func (p *Proxy) shouldInjectTools(modelID string) bool {
	if p.tools == nil || len(p.tools.Names()) == 0 {
		return false
	}
	m, ok := p.registry.Models[modelID]
	if !ok {
		return false
	}
	return !containsString(m.Tags, "nothink")
}

// reverseProxyTo forwards the request to backendURL/v1/chat/completions with
// the given (already-rewritten) body, preserving SSE streaming via
// FlushInterval. Shared by the passthrough path and the tool loop's final
// streamed answer.
func (p *Proxy) reverseProxyTo(w http.ResponseWriter, r *http.Request, backendURL string, body []byte) {
	target, err := url.Parse(backendURL)
	if err != nil {
		http.Error(w, "bad backend URL: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rp := &httputil.ReverseProxy{
		Transport:     p.transport,
		FlushInterval: p.flushInterval,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target) // sets scheme + host on pr.Out
			pr.Out.URL.Path = "/v1/chat/completions"
			pr.Out.URL.RawPath = ""
			pr.Out.Host = "" // let Go use the new Host from URL
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
			pr.Out.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			p.logger.ErrorContext(req.Context(), "upstream error",
				"backend_url", backendURL, "err", err)
			http.Error(rw, "upstream: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// /v1/models — OpenAI-shape list of every enabled model in the registry.
// ---------------------------------------------------------------------------

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
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

	out := response{Object: "list"}
	for id, m := range p.registry.Models {
		if !m.Enabled {
			continue
		}
		out.Data = append(out.Data, entry{
			ID:       id,
			Object:   "model",
			OwnedBy:  string(m.Backend),
			APIClass: m.APIClass,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---------------------------------------------------------------------------
// /health
// ---------------------------------------------------------------------------

func (p *Proxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	toolNames := []string{}
	if p.tools != nil {
		toolNames = p.tools.Names()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"tools":     toolNames,
		"streaming": true,
	})
}
