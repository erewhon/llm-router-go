// Package toolproxy is the Go rewrite of the Python tool proxy. Phase 2a
// is a focused chat-completions reverse proxy: read the incoming body,
// resolve the model name to a real upstream via the registry, rewrite
// the "model" field, and ReverseProxy with SSE streaming intact.
//
// Tool execution, the auto-router, fallback routes, and reasoning
// passthrough are intentionally out of scope for 2a and will land in
// 2b/2c/2d.
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
)

// Proxy serves /v1/chat/completions, /v1/models, and /health.
type Proxy struct {
	registry      *config.ModelRegistry
	logger        *slog.Logger
	transport     http.RoundTripper
	flushInterval time.Duration
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

// New constructs a Proxy bound to the given registry.
func New(registry *config.ModelRegistry, logger *slog.Logger, opts ...Option) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Proxy{
		registry:      registry,
		logger:        logger,
		transport:     http.DefaultTransport,
		flushInterval: -1, // SSE: flush on every write
	}
	for _, opt := range opts {
		opt(p)
	}
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

	var bodyMap map[string]json.RawMessage
	if err := json.Unmarshal(body, &bodyMap); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	var model string
	if raw, ok := bodyMap["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
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

	// Rewrite the model field to the backend's expected name.
	bodyMap["model"], _ = json.Marshal(res.BackendModel)
	newBody, err := json.Marshal(bodyMap)
	if err != nil {
		http.Error(w, "re-encode body: "+err.Error(), http.StatusInternalServerError)
		return
	}

	target, err := url.Parse(res.BackendURL)
	if err != nil {
		http.Error(w, "bad backend URL: "+err.Error(), http.StatusInternalServerError)
		return
	}

	p.logger.InfoContext(r.Context(), "forwarding",
		"model", model,
		"backend_model", res.BackendModel,
		"backend_url", res.BackendURL,
		"resolved_via", res.ModelID,
	)

	rp := &httputil.ReverseProxy{
		Transport:     p.transport,
		FlushInterval: p.flushInterval,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target) // sets scheme + host on pr.Out
			pr.Out.URL.Path = "/v1/chat/completions"
			pr.Out.URL.RawPath = ""
			pr.Out.Host = "" // let Go use the new Host from URL
			pr.Out.Body = io.NopCloser(bytes.NewReader(newBody))
			pr.Out.ContentLength = int64(len(newBody))
			pr.Out.Header.Set("Content-Length", fmt.Sprintf("%d", len(newBody)))
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			p.logger.ErrorContext(req.Context(), "upstream error",
				"backend_url", res.BackendURL, "err", err)
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
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
