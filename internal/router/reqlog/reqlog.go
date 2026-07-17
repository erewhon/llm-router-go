// Package reqlog defines a Sink for router request/response records, plus a
// no-op default, an in-memory implementation for tests, and an asynchronous
// Postgres implementation for production. Records cover every request that
// reaches the router's proxy handler, including ones rejected before any
// upstream call (resolve failures, api_class mismatches, invalid JSON).
package reqlog

import "time"

// Record is one request crossing the router. Fields that don't apply to a
// given outcome stay zero/nil/empty:
//   - rejected-before-resolve records (400/404) have empty BackendURL/ResolvedVia
//   - streamed responses have Stream=true and may have nil token counts
//     (parsed best-effort from the SSE tail)
//   - usage fields are *int so 0 (a real value, e.g. rerank) is distinguishable
//     from "no usage in response"
type Record struct {
	RequestID        string
	TS               time.Time
	Method           string
	Path             string
	Model            string // as the caller sent
	BackendModel     string // rewritten model field forwarded upstream
	BackendURL       string
	ResolvedVia      string // registry model_id
	APIClass         string
	ViaToolProxy     bool
	Stream           bool
	Status           int
	LatencyMS        int
	PromptTokens     *int
	CompletionTokens *int
	TotalTokens      *int
	// Anthropic prompt-cache token splits (api_class "anthropic" only). Nil for
	// every other class and for Anthropic responses that report no caching.
	CacheCreationInputTokens *int
	CacheReadInputTokens     *int
	// PrefixHashChain is a rolling per-segment hash of the rendered prompt
	// prefix in cache order (tools → system → each message), hashes only, never
	// content. Empty for non-anthropic requests. Consecutive requests in a
	// session diff to pinpoint where the cached prefix diverged.
	PrefixHashChain string
	Error           string
}

// Sink consumes records. Implementations must be safe for concurrent Log
// calls and must not block the caller (a request-path Log call should never
// hold up the response).
type Sink interface {
	Log(rec Record)
	Close() error
}

// NopSink discards every record. Used when --postgres-dsn is empty so callers
// don't have to nil-check on every Log call.
type NopSink struct{}

func (NopSink) Log(Record)   {}
func (NopSink) Close() error { return nil }
