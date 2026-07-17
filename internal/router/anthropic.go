package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/httpx"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// anthropicBackendRoot returns the registry id and upstream root URL (with any
// trailing "/v1" stripped, so the inbound path re-appends cleanly) of the
// configured api_class:anthropic passthrough target, or "","" if none is
// configured. A transparent gateway has a single target, so the first
// anthropic-class model in the active set wins.
func (rt *Router) anthropicBackendRoot() (id, root string) {
	for mid, m := range rt.active {
		if m.APIClass != config.APIClassAnthropic {
			continue
		}
		base, err := rt.registry.APIBase(mid, nil)
		if err != nil {
			continue
		}
		return mid, strings.TrimSuffix(base, "/v1")
	}
	return "", ""
}

// handleAnthropic is a transparent passthrough for the Anthropic Messages API
// (POST /v1/messages and /v1/messages/count_tokens). Unlike handleProxy it:
//   - forwards the request body byte-for-byte (no model rewrite),
//   - injects NO router credentials — the client's own Authorization / x-api-key
//     / anthropic-* headers pass through untouched (so Claude Code Max keeps its
//     subscription/OAuth billing),
//   - parses Anthropic-shaped usage (cache token splits included) from the
//     response, and
//   - logs a prefix hash chain for cache-divergence diagnosis (hashes only).
//
// modelID/backendRoot are the resolved anthropic-class registry id and upstream
// root. The endpoint is only registered when a target is configured.
func (rt *Router) handleAnthropic(modelID, backendRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recordingWriter{ResponseWriter: w}
		cap := &anthropicCapture{}

		var (
			modelIn string
			chain   string
			errMsg  string
		)

		defer func() {
			lr := reqlog.Record{
				RequestID:       httpx.RequestIDFromContext(r.Context()),
				TS:              start,
				Method:          r.Method,
				Path:            r.URL.Path,
				Model:           modelIn,
				BackendURL:      backendRoot,
				ResolvedVia:     modelID,
				APIClass:        string(config.APIClassAnthropic),
				Stream:          cap.isSSE,
				Status:          rec.status,
				LatencyMS:       int(time.Since(start) / time.Millisecond),
				PrefixHashChain: chain,
				Error:           errMsg,
			}
			u := cap.usage
			lr.PromptTokens = u.input
			lr.CompletionTokens = u.output
			lr.CacheCreationInputTokens = u.cacheCreation
			lr.CacheReadInputTokens = u.cacheRead
			rt.sink.Log(lr)
			rt.metrics.Observe(lr)
		}()

		// Read the body so we can log the model + prefix hash chain, but forward
		// the exact original bytes (never re-marshal — the acceptance criterion
		// is a byte-identical request).
		body, err := io.ReadAll(r.Body)
		if err != nil {
			errMsg = "read body: " + err.Error()
			http.Error(rec, errMsg, http.StatusBadRequest)
			return
		}
		modelIn, chain = anthropicPrefixChain(body)

		rt.logger.InfoContext(r.Context(), "forwarding anthropic",
			"path", r.URL.Path, "model", modelIn, "backend_url", backendRoot)
		rt.anthropicProxy(rec, r, backendRoot, body, cap)
	}
}

// anthropicProxy forwards body verbatim to backendRoot + the inbound path,
// preserving every client header (credentials and betas included — the router
// adds none of its own), and captures usage from the response for reqlog.
func (rt *Router) anthropicProxy(w http.ResponseWriter, r *http.Request, backendRoot string, body []byte, cap *anthropicCapture) {
	target, err := url.Parse(backendRoot)
	if err != nil {
		http.Error(w, "bad anthropic backend URL: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rp := &httputil.ReverseProxy{
		Transport:     rt.transport,
		FlushInterval: rt.flushInterval,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target) // scheme+host; joins target.Path with the inbound path
			pr.Out.Host = ""  // derive Host + TLS SNI from the target URL
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
			pr.Out.Header.Set("Content-Length", strconv.Itoa(len(body)))
			// Strip the client's Accept-Encoding so the upstream response reaches
			// ModifyResponse decoded: the Go transport re-adds gzip itself and
			// transparently decompresses it. Otherwise Anthropic returns a gzip'd
			// body (Claude Code always sends Accept-Encoding: gzip) and the usage
			// tee/parse below would scan compressed bytes and log NULL tokens.
			pr.Out.Header.Del("Accept-Encoding")
			// Deliberately NOT calling pr.SetXForwarded() and NOT touching
			// Authorization / x-api-key / anthropic-* — the client's headers are
			// copied to pr.Out as-is, so the passthrough stays transparent.
		},
		ModifyResponse: func(resp *http.Response) error {
			if contentTypeIsSSE(resp.Header.Get("Content-Type")) {
				cap.isSSE = true
				resp.Body = newAnthropicSSECapture(resp.Body, &cap.usage)
				return nil
			}
			// Non-streaming: buffer the (bounded) JSON body to parse usage, then
			// hand an identical copy back to the client.
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			parseAnthropicResponseUsage(b, &cap.usage)
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rt.logger.WarnContext(r.Context(), "anthropic upstream error", "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// anthropicCapture collects what we learn about an Anthropic response for the
// reqlog record: whether it streamed, and the merged usage.
type anthropicCapture struct {
	isSSE bool
	usage anthropicUsage
}

// anthropicUsage is the merged token usage for a request. Pointers distinguish
// "not reported" (nil) from a real zero.
type anthropicUsage struct {
	input         *int
	output        *int
	cacheCreation *int
	cacheRead     *int
}

func (u *anthropicUsage) merge(j anthropicUsageJSON) {
	if j.InputTokens != nil {
		u.input = j.InputTokens
	}
	if j.OutputTokens != nil {
		u.output = j.OutputTokens
	}
	if j.CacheCreationInputTokens != nil {
		u.cacheCreation = j.CacheCreationInputTokens
	}
	if j.CacheReadInputTokens != nil {
		u.cacheRead = j.CacheReadInputTokens
	}
}

type anthropicUsageJSON struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
}

// parseAnthropicResponseUsage extracts usage from a non-streaming response.
// Messages responses carry a top-level "usage" object; count_tokens responses
// carry a bare top-level "input_tokens". Both are handled.
func parseAnthropicResponseUsage(body []byte, u *anthropicUsage) {
	var resp struct {
		Usage       anthropicUsageJSON `json:"usage"`
		InputTokens *int               `json:"input_tokens"` // count_tokens shape
	}
	if json.Unmarshal(body, &resp) != nil {
		return
	}
	u.merge(resp.Usage)
	if resp.InputTokens != nil && u.input == nil {
		u.input = resp.InputTokens
	}
}

// anthropicSSECapture tees the response stream to the client while scanning
// `data:` lines for usage. Anthropic reports input/cache tokens on the early
// `message_start` event and output_tokens on `message_delta`, so — unlike the
// OpenAI 64KB-tail approach — usage must be extracted as the stream flows, not
// from a trailing snapshot.
type anthropicSSECapture struct {
	rc    io.ReadCloser
	buf   []byte
	usage *anthropicUsage
}

// maxSSELineBuffer caps the partial-line buffer so a malformed stream without
// newlines can't grow it without bound.
const maxSSELineBuffer = 1 << 20

func newAnthropicSSECapture(rc io.ReadCloser, u *anthropicUsage) *anthropicSSECapture {
	return &anthropicSSECapture{rc: rc, usage: u}
}

func (c *anthropicSSECapture) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.scan(p[:n])
	}
	return n, err
}

func (c *anthropicSSECapture) Close() error { return c.rc.Close() }

func (c *anthropicSSECapture) scan(b []byte) {
	c.buf = append(c.buf, b...)
	for {
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			if len(c.buf) > maxSSELineBuffer {
				c.buf = c.buf[:0] // defensive: drop a pathological unterminated line
			}
			return
		}
		line := c.buf[:i]
		c.buf = c.buf[i+1:]
		c.handleLine(line)
	}
}

func (c *anthropicSSECapture) handleLine(line []byte) {
	line = bytes.TrimSpace(line) // strip trailing \r and surrounding space
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	payload := bytes.TrimSpace(line[len(prefix):])
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage anthropicUsageJSON `json:"usage"`
		} `json:"message"`
		Usage anthropicUsageJSON `json:"usage"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		c.usage.merge(ev.Message.Usage)
	case "message_delta":
		c.usage.merge(ev.Usage)
	}
}

// anthropicPrefixChain computes the request model and a rolling per-segment hash
// of the rendered prompt prefix in Anthropic cache order — tools, then system,
// then each message — hashing the raw JSON bytes of each segment (never logging
// content). Each entry is the cumulative digest through that segment, so two
// requests share a chain prefix up to the point their cached prefix diverged.
// Returns ("","") if the body isn't a parseable Anthropic request.
func anthropicPrefixChain(body []byte) (model, chain string) {
	var req struct {
		Model    string            `json:"model"`
		Tools    json.RawMessage   `json:"tools"`
		System   json.RawMessage   `json:"system"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", ""
	}
	h := sha256.New()
	var parts []string
	feed := func(seg []byte) {
		h.Write(seg)
		parts = append(parts, hex.EncodeToString(h.Sum(nil))[:16])
	}
	if len(req.Tools) > 0 {
		feed(req.Tools)
	}
	if len(req.System) > 0 {
		feed(req.System)
	}
	for _, m := range req.Messages {
		feed(m)
	}
	return req.Model, strings.Join(parts, ",")
}
