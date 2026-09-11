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
	// Role is the semantic role the caller asked for ("coder"), empty when
	// they named a model or alias directly. With ResolvedVia it answers the
	// question the schedule creates: what did "coder" actually mean at 03:00?
	Role string
	// RoleOverflowed marks a role that exhausted its in-contract candidates
	// and fell through to its declared overflow list.
	RoleOverflowed bool
	// CandidatePressure is the load score of the seat this request was routed
	// to, when the role used pressure balancing. Nil otherwise. Makes the
	// balancing effect measurable: group rows by resolved model and read the
	// spread of pressures each seat served at.
	CandidatePressure *int
	// FailoverFrom is the model that failed mid-request, when this record's
	// ResolvedVia is the candidate that took over. Empty when no failover
	// happened.
	FailoverFrom string
	Error        string
	// UpstreamStatus is the HTTP status the upstream returned on the final
	// attempt. 0 means the upstream never answered: either the request was
	// rejected before any upstream call, or the transport failed (see
	// ErrorClass). Distinct from Status, which is what the client saw — a
	// transport failure records Status 502 (router-generated) with
	// UpstreamStatus 0.
	UpstreamStatus int
	// ErrorClass classifies how the final upstream attempt failed:
	// "server_error" (5xx), "client_error" (4xx), "error_envelope" (an error
	// object inside an HTTP 2xx — the 2026-08-27 Zen incident shape),
	// "timeout", "connect", or "transport" (other transport-level failures).
	// Empty for successes and for requests rejected before any upstream call.
	ErrorClass string
	// Principal is the authenticated caller this request is billed to: a PAT
	// principal ("steven"), or "legacy:<fingerprint>" for a pre-PAT shared
	// key. Empty means the request carried no identity — an auth-exempt path
	// (/health, the Anthropic passthrough) or a router running with auth
	// disabled. Empty is "unattributed", never "trusted".
	Principal string
	// TokenID is the public id half of the credential that authenticated the
	// request. It is what you revoke, and it is safe to store: the secret
	// half never reaches this struct. Empty whenever Principal is.
	TokenID string
	// UpstreamProvider is the operator that actually served the request, as
	// the upstream itself reported it — OpenRouter's top-level `provider`
	// field ("Amazon Bedrock", "Novita", "DeepInfra"). Empty for local
	// backends and for any upstream that does not report one.
	//
	// This is the only place the answer exists. The router chooses a MODEL;
	// OpenRouter chooses the endpoint that serves it, from a pool that
	// changes between requests — the same three models served from
	// DeepInfra/Novita/Sail Research on one probe and
	// Inceptron/Novita/DigitalOcean minutes later (2026-09-09). Neither
	// models.yaml nor ResolvedVia can say where a given request went, so
	// without this field that fact is simply not recoverable afterwards.
	UpstreamProvider string
	// UpstreamCostUSD is what the request cost, as the PROVIDER billed it
	// (OpenRouter's `usage.cost`). Nil for local backends and for any upstream
	// that does not report one.
	//
	// Preferred over multiplying tokens by the per-million rates in
	// models.yaml, which cannot know two things this does: the cache discount
	// actually applied, and which endpoint served the request — and endpoints
	// of the same model are priced differently.
	UpstreamCostUSD *float64
	// CachedPromptTokens is the part of the prompt served from the provider's
	// cache (`usage.prompt_tokens_details.cached_tokens`). Nil when the
	// upstream reports no cache detail.
	//
	// Read against PromptTokens it is the prefix-cache hit rate, and read
	// against UpstreamCostUSD it is what that hit rate is worth: the same
	// 1650-token prompt measured ~3x cheaper cached than uncached
	// (2026-09-09). Read against UpstreamProvider it says whether a provider
	// switch cost you a warm cache.
	CachedPromptTokens *int
	// PrivacyTolerance names the retention posture the router ENFORCED on
	// this request ("zdr"), empty when none was demanded. Set whether the
	// tolerance was satisfied by a local placement or by putting the
	// directive on the wire.
	//
	// Paired with UpstreamProvider on purpose: a provider name alone is
	// trivia, and a tolerance alone is a claim. Together they are evidence —
	// "this request required zero retention AND was served by Amazon
	// Bedrock" is a statement someone can audit a quarter later.
	PrivacyTolerance string
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
