package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// recordingWriter wraps an http.ResponseWriter to capture the status code
// (which both http.Error and ReverseProxy hand to WriteHeader, or which we
// infer as 200 on first Write). Flush() is forwarded so SSE responses still
// flush through the middleware chain.
type recordingWriter struct {
	http.ResponseWriter
	status int
}

func (rw *recordingWriter) WriteHeader(code int) {
	if rw.status == 0 {
		rw.status = code
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recordingWriter) Write(b []byte) (int, error) {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	return rw.ResponseWriter.Write(b)
}

func (rw *recordingWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// responseCapture collects what we learn about the upstream response while
// it streams to the client: whether it's SSE, the buffered JSON body (for
// non-SSE), and the SSE tail buffer (for SSE — used to extract the final
// `usage` chunk after the stream ends).
type responseCapture struct {
	isSSE    bool
	jsonBody []byte
	sseTail  *streamTailCapture
	// upstreamStatus is the HTTP status line the upstream answered with, set
	// in ModifyResponse. 0 when the upstream never answered (transport error
	// or the request was rejected before forwarding).
	upstreamStatus int
}

// streamTailCapture is an io.ReadCloser that transparently forwards bytes
// from an upstream reader while keeping the most recent N bytes in a buffer.
// The OpenAI/SGLang SSE `usage` chunk appears just before `[DONE]`, so a
// 64KB tail buffer is more than enough on any realistic stream and bounds
// memory regardless of total stream length.
type streamTailCapture struct {
	rc  io.ReadCloser
	buf bytes.Buffer
	max int
}

func newStreamTailCapture(rc io.ReadCloser, max int) *streamTailCapture {
	if max <= 0 {
		max = 64 * 1024
	}
	return &streamTailCapture{rc: rc, max: max}
}

func (s *streamTailCapture) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 {
		s.buf.Write(p[:n])
		if s.buf.Len() > s.max {
			s.buf.Next(s.buf.Len() - s.max) // drop the front, keep tail
		}
	}
	return n, err
}

func (s *streamTailCapture) Close() error { return s.rc.Close() }

// Tail returns the buffered tail bytes. Safe to call after the stream has
// been fully read.
func (s *streamTailCapture) Tail() []byte { return s.buf.Bytes() }

// ---------------------------------------------------------------------------
// usage parsing
// ---------------------------------------------------------------------------

// usageStats is what one response reports about its own cost, in every sense
// of the word. A struct rather than a fifth and sixth positional return:
// TokPerSec and CostUSD are both *float64 and would sit next to each other in
// a tuple, which is a transposition waiting to happen.
//
// Every field is a pointer so a real zero is distinguishable from "absent" —
// a rerank with no billable tokens genuinely reports 0, and a cached-in-full
// prompt genuinely reports 0 uncached tokens.
type usageStats struct {
	PromptTokens     *int
	CompletionTokens *int
	TotalTokens      *int
	// TokPerSec carries Atlas' non-standard `response_token/s`, the engine's
	// own measured decode rate.
	TokPerSec *float64
	// CostUSD is OpenRouter's `usage.cost` — what the request actually cost,
	// as the provider billed it. Absent on every local backend, and worth more
	// than a computed estimate: it already accounts for cache discounts and
	// for whichever endpoint happened to serve the request, neither of which
	// models.yaml's per-million rates can know.
	CostUSD *float64
	// CachedPromptTokens is `usage.prompt_tokens_details.cached_tokens` — the
	// part of the prompt that hit the provider's cache. This is the only
	// signal that says whether prefix caching is working at all: a cache hit
	// measured ~3x cheaper than a miss on the same 1650-token prompt
	// (2026-09-09), so a run of zeroes here is a bill, not a curiosity.
	CachedPromptTokens *int
}

// parseUsage extracts an OpenAI-shape `usage` object from a non-streaming
// response body. Returns the zero struct if the body has no usage field.
func parseUsage(body []byte) usageStats {
	var r struct {
		Usage *struct {
			PromptTokens        *int     `json:"prompt_tokens"`
			CompletionTokens    *int     `json:"completion_tokens"`
			TotalTokens         *int     `json:"total_tokens"`
			RespTokPerSec       *float64 `json:"response_token/s"`
			Cost                *float64 `json:"cost"`
			PromptTokensDetails *struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Usage == nil {
		return usageStats{}
	}
	u := usageStats{
		PromptTokens:     r.Usage.PromptTokens,
		CompletionTokens: r.Usage.CompletionTokens,
		TotalTokens:      r.Usage.TotalTokens,
		TokPerSec:        r.Usage.RespTokPerSec,
		CostUSD:          r.Usage.Cost,
	}
	if r.Usage.PromptTokensDetails != nil {
		u.CachedPromptTokens = r.Usage.PromptTokensDetails.CachedTokens
	}
	return u
}

// present reports whether the response said anything about usage at all.
func (u usageStats) present() bool {
	return u.PromptTokens != nil || u.CompletionTokens != nil || u.TotalTokens != nil ||
		u.TokPerSec != nil || u.CostUSD != nil || u.CachedPromptTokens != nil
}

// extractSSEUsage scans the tail bytes of an SSE stream for the last
// `data: { ... "usage": {...} ... }` event and returns the parsed usage.
// Robust to a truncated leading event (the tail may start mid-event after
// the rolling buffer wraps).
func extractSSEUsage(tail []byte) usageStats {
	var out usageStats
	for _, line := range bytes.Split(tail, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if u := parseUsage(data); u.present() {
			out = u
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// serving-provider parsing
// ---------------------------------------------------------------------------

// parseUpstreamProvider extracts OpenRouter's top-level `provider` field — the
// operator that actually served the request ("Amazon Bedrock", "Novita").
//
// Separate from parseUsage rather than folded into it because the two answer
// different questions and are absent independently: a local llama-server
// reports usage and no provider, and an OpenRouter error envelope can report a
// provider with no usage at all. Returns "" when the field is absent, which is
// every non-OpenRouter upstream.
func parseUpstreamProvider(body []byte) string {
	var r struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ""
	}
	return r.Provider
}

// extractSSEProvider finds the serving provider in the tail of an SSE stream.
//
// OpenRouter stamps `provider` on EVERY chunk, not just the final usage one
// (verified 2026-09-09), so the tail buffer is guaranteed to carry it whenever
// the stream produced any output — no need to reach back to the first chunk,
// which the rolling buffer has usually dropped by then.
//
// Takes the LAST value seen for the same reason extractSSEUsage does: if a
// stream somehow reported more than one, the one that finished the response is
// the one that served it.
func extractSSEProvider(tail []byte) string {
	provider := ""
	for _, line := range bytes.Split(tail, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if p := parseUpstreamProvider(data); p != "" {
			provider = p
		}
	}
	return provider
}

// contentTypeIsSSE reports whether the value indicates Server-Sent Events.
func contentTypeIsSSE(ct string) bool {
	return strings.HasPrefix(ct, "text/event-stream")
}

// contentTypeIsJSON reports whether the value indicates a JSON body the
// router should buffer for usage extraction.
func contentTypeIsJSON(ct string) bool {
	return strings.HasPrefix(ct, "application/json")
}
