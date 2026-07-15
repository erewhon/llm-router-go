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

// parseUsage extracts an OpenAI-shape `usage` object from a non-streaming
// response body. Returns nil if the body has no usage field. The token counts
// are *int so a real 0 (e.g. rerank with zero billable tokens) is
// distinguishable from "absent". tokPerSec carries Atlas' non-standard
// `response_token/s` (the engine's own measured decode rate) when present.
func parseUsage(body []byte) (prompt, completion, total *int, tokPerSec *float64) {
	var r struct {
		Usage *struct {
			PromptTokens     *int     `json:"prompt_tokens"`
			CompletionTokens *int     `json:"completion_tokens"`
			TotalTokens      *int     `json:"total_tokens"`
			RespTokPerSec    *float64 `json:"response_token/s"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Usage == nil {
		return nil, nil, nil, nil
	}
	return r.Usage.PromptTokens, r.Usage.CompletionTokens, r.Usage.TotalTokens, r.Usage.RespTokPerSec
}

// extractSSEUsage scans the tail bytes of an SSE stream for the last
// `data: { ... "usage": {...} ... }` event and returns the parsed usage.
// Robust to a truncated leading event (the tail may start mid-event after
// the rolling buffer wraps).
func extractSSEUsage(tail []byte) (prompt, completion, total *int, tokPerSec *float64) {
	// Walk lines, find data: payloads, parse each, keep the last usage seen.
	for _, line := range bytes.Split(tail, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if p, c, tot, tps := parseUsage(data); p != nil || c != nil || tot != nil || tps != nil {
			prompt, completion, total, tokPerSec = p, c, tot, tps
		}
	}
	return prompt, completion, total, tokPerSec
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
