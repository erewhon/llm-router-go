package router

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// privacyRegistryYAML sets up the three placement kinds the tolerance has to
// tell apart, and roles over each:
//
//	local-seat  — on our metal; nothing to enforce
//	or-seat     — OpenRouter; the directive is enforceable
//	zen-seat    — nodeless external with no enforcement mechanism
//	mixed-chain — an or/ entry backed up by a zen/ entry: only as private as
//	              its worst member
const privacyRegistryYAML = `
nodes:
  delphi: {host: delphi.local, gpu: amd, vram_gb: 96}

models:
  local-seat:
    hf_repo: openai/gpt-oss-120b
    node: delphi
    capabilities: [text, tool_calling]
  or-seat:
    hf_repo: anthropic/claude-sonnet-5
    backend: external
    api_base: https://openrouter.ai/api/v1
    api_key: OPENROUTER_API_KEY
    capabilities: [text, tool_calling]
  or-seat-2:
    hf_repo: z-ai/glm-5.3-flash
    backend: external
    api_base: https://openrouter.ai/api/v1
    api_key: OPENROUTER_API_KEY
    capabilities: [text, tool_calling]
  zen-seat:
    hf_repo: kimi-k2.7-code
    backend: external
    api_base: https://opencode.ai/zen/go/v1
    capabilities: [text, tool_calling]
  mixed-chain:
    hf_repo: claude-sonnet-5
    backend: external
    fallbacks: [or-seat, zen-seat]
    capabilities: [text, tool_calling]

roles:
  private:
    require: {locality: local_or_zdr}
    candidates: [or-seat, local-seat]

  private-local-first:
    require: {locality: local_or_zdr}
    candidates: [local-seat, or-seat]

  anywhere:
    require: {locality: any}
    candidates: [zen-seat]
`

func newPrivacyRouter(t *testing.T, upstreamURL string) *Router {
	t.Helper()
	reg, err := config.LoadBytes([]byte(privacyRegistryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	opts := []Option{WithMode("default"), WithFlushInterval(0)}
	if upstreamURL != "" {
		opts = append(opts, WithTransport(&transportRedirect{to: upstreamURL, rt: http.DefaultTransport}))
	}
	return New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
}

func TestZDRDirectiveIsAttachedForARoleThatRequiresIt(t *testing.T) {
	var got map[string]any
	upstream := captureUpstream(t, nil, &got, nil)
	defer upstream.Close()

	rt := newPrivacyRouter(t, upstream.URL)
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"private","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	prov, ok := got["provider"].(map[string]any)
	if !ok {
		t.Fatalf("forwarded body has no provider object: %#v", got)
	}
	if prov["zdr"] != true {
		t.Errorf("provider.zdr = %#v, want true", prov["zdr"])
	}
	if h := rec.Header().Get(PrivacyHeader); h != PrivacyZDR {
		t.Errorf("%s = %q, want %q", PrivacyHeader, h, PrivacyZDR)
	}
}

func TestNoDirectiveWhenTheRoleDoesNotAskForOne(t *testing.T) {
	// The gate must be inert for everything that has not opted in — the same
	// discipline the context envelope follows. Attaching zdr:true globally
	// would narrow the endpoint pool for tier-3 traffic that never asked.
	var got map[string]any
	upstream := captureUpstream(t, nil, &got, nil)
	defer upstream.Close()

	rt := newPrivacyRouter(t, upstream.URL)
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"anywhere","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if _, present := got["provider"]; present {
		t.Errorf("provider block present on an untolerated request: %#v", got["provider"])
	}
	if h := rec.Header().Get(PrivacyHeader); h != "" {
		t.Errorf("%s = %q, want empty", PrivacyHeader, h)
	}
}

func TestLocalSeatNeedsNoDirective(t *testing.T) {
	// A local placement satisfies the tolerance without a directive: the
	// prompt never leaves the fleet, and there is no third party to bind.
	var got map[string]any
	upstream := captureUpstream(t, nil, &got, nil)
	defer upstream.Close()

	rt := newPrivacyRouter(t, upstream.URL)
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"private-local-first","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Router-Resolved"); got != "local-seat" {
		t.Fatalf("X-Router-Resolved = %q, want local-seat", got)
	}
	if _, present := got["provider"]; present {
		t.Errorf("provider block sent to a local backend: %#v", got["provider"])
	}
	if h := rec.Header().Get(PrivacyHeader); h != PrivacyZDR {
		t.Errorf("%s = %q, want %q — the tolerance was still honoured", PrivacyHeader, h, PrivacyZDR)
	}
}

func TestCallerHeaderTightensBeyondTheRole(t *testing.T) {
	var got map[string]any
	upstream := captureUpstream(t, nil, &got, nil)
	defer upstream.Close()

	rt := newPrivacyRouter(t, upstream.URL)

	// A directly named OpenRouter model has no role contract behind it, so
	// the header is the only thing that can demand the directive.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"or-seat","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(PrivacyHeader, PrivacyZDR)
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	prov, ok := got["provider"].(map[string]any)
	if !ok || prov["zdr"] != true {
		t.Errorf("provider = %#v, want zdr:true from the request header", got["provider"])
	}
}

func TestCallerHeaderOnAnUnenforceableSeatIsRefused(t *testing.T) {
	// The whole point: a caller who asked for zero retention and cannot get
	// it must be told, not quietly served from a retaining seat.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream was called; the request should never have been sent")
	}))
	defer upstream.Close()

	rt := newPrivacyRouter(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen-seat","messages":[{"role":"user","content":"secret"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(PrivacyHeader, PrivacyZDR)
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"zen-seat", "zero data retention", "zdr_unavailable"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal body missing %q: %s", want, body)
		}
	}
}

func TestUnrecognisedPrivacyValueIsRefusedNotIgnored(t *testing.T) {
	// A caller asking for a posture the router does not implement is asking
	// for something it cannot promise. Ignoring the header would serve them
	// while letting them believe otherwise.
	rt := newPrivacyRouter(t, "")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen-seat","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(PrivacyHeader, "eu-only")
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unrecognised") {
		t.Errorf("refusal should name the unrecognised value: %s", rec.Body.String())
	}
}

func TestDirectiveMergesIntoACallerProviderBlock(t *testing.T) {
	var got map[string]any
	upstream := captureUpstream(t, nil, &got, nil)
	defer upstream.Close()

	rt := newPrivacyRouter(t, upstream.URL)
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"private","provider":{"order":["Anthropic"],"zdr":false},"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	prov, ok := got["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider block lost: %#v", got)
	}
	if prov["zdr"] != true {
		t.Errorf("provider.zdr = %#v, want true — a caller's false does not override a role's requirement", prov["zdr"])
	}
	order, ok := prov["order"].([]any)
	if !ok || len(order) != 1 || order[0] != "Anthropic" {
		t.Errorf("provider.order = %#v, want the caller's [Anthropic] preserved", prov["order"])
	}
}

func TestNonObjectProviderFieldIsRefusedNotOverwritten(t *testing.T) {
	rt := newPrivacyRouter(t, "")

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"private","provider":"anthropic","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
}

// --- canary classification -------------------------------------------------
//
// The inverted pass condition (a refusal is the HEALTHY answer) is the part of
// this feature most likely to be "fixed" backwards by someone skimming, so it
// gets tests that state the expectation in words.

// liveZDRRefusal is the VERBATIM body OpenRouter returned on 2026-09-09 for
// the canary probe (anthropic/claude-sonnet-5 pinned to provider
// only:["anthropic"], no zdr flag), captured against the live account. Kept
// literal rather than paraphrased so that if OpenRouter reshapes this payload
// the test fails here — where someone can look at it — rather than in
// production, where the canary would quietly downgrade to "unknown".
const liveZDRRefusal = `{"error":{"message":"0 endpoints out of 1 requested are available matching your guardrail restrictions and data policy. We removed them for the following reasons (an endpoint may have matched multiple reasons):\nZDR violation (account settings): 1 endpoint excluded; configurable at https://openrouter.ai/settings/privacy","code":404,"metadata":{"input_endpoint_count":1,"ineligibility_reasons":[{"reason":"zdr-violation-by-account","endpoint_count":1}]}}}`

func TestCanaryClassifiesTheLiveRefusalAsEnforced(t *testing.T) {
	// The observed status was 404, not a 4xx-shaped "your request was bad" —
	// which is why classification reads the body first and the status second.
	st := classifyZDRProbe(404, []byte(liveZDRRefusal), time.Now())
	if st.Posture != ZDRPostureEnforced {
		t.Fatalf("live refusal -> %q, want %q", st.Posture, ZDRPostureEnforced)
	}
	if !strings.Contains(st.Detail, "ZDR violation (account settings)") {
		t.Errorf("detail should carry the upstream reason: %q", st.Detail)
	}
}

func TestCanaryRefusalMeansEnforced(t *testing.T) {
	raw := []byte(`{"error":{"message":"ZDR violation (account settings): 1 endpoint excluded","code":404}}`)

	for _, status := range []int{200, 404, 422} {
		st := classifyZDRProbe(status, raw, time.Now())
		if st.Posture != ZDRPostureEnforced {
			t.Errorf("HTTP %d with a ZDR marker -> %q, want %q (the refusal IS the healthy state)",
				status, st.Posture, ZDRPostureEnforced)
		}
		if !strings.Contains(st.Detail, "ZDR violation") {
			t.Errorf("detail lost the upstream message: %q", st.Detail)
		}
	}
}

func TestCanarySuccessMeansNotEnforced(t *testing.T) {
	// A retaining endpoint served the probe: account ZDR is off.
	raw := []byte(`{"choices":[{"message":{"content":"pong"}}]}`)

	st := classifyZDRProbe(200, raw, time.Now())
	if st.Posture != ZDRPostureNotEnforced {
		t.Errorf("clean 200 -> %q, want %q", st.Posture, ZDRPostureNotEnforced)
	}
}

func TestCanaryOtherFailuresAreInconclusive(t *testing.T) {
	// A bad key or a rate limit proves nothing either way, and must not be
	// read as a verdict in either direction.
	cases := []struct {
		name   string
		status int
		raw    string
	}{
		{"bad key", 401, `{"error":{"message":"No auth credentials found"}}`},
		{"rate limited", 429, `{"error":{"message":"Rate limit exceeded"}}`},
		{"upstream outage", 502, `bad gateway`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := classifyZDRProbe(tc.status, []byte(tc.raw), time.Now())
			if st.Posture != ZDRPostureUnknown {
				t.Errorf("HTTP %d -> %q, want %q", tc.status, st.Posture, ZDRPostureUnknown)
			}
		})
	}
}

func TestNilCanaryStatusIsUnknown(t *testing.T) {
	// /health calls Status() unconditionally; an unconfigured canary must
	// report "nobody checked" rather than panic or look healthy.
	var c *ZDRCanary
	if got := c.Status().Posture; got != ZDRPostureUnknown {
		t.Errorf("nil canary posture = %q, want %q", got, ZDRPostureUnknown)
	}
}

func TestHealthReportsAccountPosture(t *testing.T) {
	rt := newPrivacyRouter(t, "")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	var out struct {
		ZDRAccount ZDRStatus `json:"zdr_account"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	if out.ZDRAccount.Posture != ZDRPostureUnknown {
		t.Errorf("zdr_account.posture = %q, want %q", out.ZDRAccount.Posture, ZDRPostureUnknown)
	}
}

// --- reqlog provenance -----------------------------------------------------

// newPrivacyRouterWithSink is newPrivacyRouter plus a record sink, for the
// provenance assertions.
func newPrivacyRouterWithSink(t *testing.T, upstreamURL string, sink *reqlog.MemorySink) *Router {
	t.Helper()
	reg, err := config.LoadBytes([]byte(privacyRegistryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	opts := []Option{WithMode("default"), WithFlushInterval(0), WithSink(sink)}
	if upstreamURL != "" {
		opts = append(opts, WithTransport(&transportRedirect{to: upstreamURL, rt: http.DefaultTransport}))
	}
	return New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
}

// providerUpstream answers like OpenRouter: a top-level "provider" naming the
// operator that served the request, alongside the usual usage block.
func providerUpstream(t *testing.T, provider string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gen-1","provider":"` + provider +
			`","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`))
	}))
}

func TestReqlogRecordsTheServingProvider(t *testing.T) {
	upstream := providerUpstream(t, "Amazon Bedrock")
	defer upstream.Close()

	sink := &reqlog.MemorySink{}
	rt := newPrivacyRouterWithSink(t, upstream.URL, sink)

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"private","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	got := recs[0]
	if got.UpstreamProvider != "Amazon Bedrock" {
		t.Errorf("UpstreamProvider = %q, want %q", got.UpstreamProvider, "Amazon Bedrock")
	}
	if got.PrivacyTolerance != PrivacyZDR {
		t.Errorf("PrivacyTolerance = %q, want %q", got.PrivacyTolerance, PrivacyZDR)
	}
	// Provenance must not have displaced the usage parsing it sits beside.
	if got.PromptTokens == nil || *got.PromptTokens != 9 {
		t.Errorf("PromptTokens = %v, want 9 — usage parsing regressed", got.PromptTokens)
	}
}

func TestReqlogRecordsTheProviderFromAnSSEStream(t *testing.T) {
	// OpenRouter stamps "provider" on every chunk, so the tail buffer carries
	// it even though the first chunk is long gone by the time the stream ends.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"provider\":\"Novita\",\"choices\":[{\"delta\":{\"content\":\"o\"}}]}\n\n" +
				"data: {\"provider\":\"Novita\",\"choices\":[{\"delta\":{\"content\":\"k\"}}]," +
				"\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	sink := &reqlog.MemorySink{}
	rt := newPrivacyRouterWithSink(t, upstream.URL, sink)

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"private","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if !recs[0].Stream {
		t.Errorf("Stream = false, want true")
	}
	if recs[0].UpstreamProvider != "Novita" {
		t.Errorf("UpstreamProvider = %q, want Novita", recs[0].UpstreamProvider)
	}
	if recs[0].TotalTokens == nil || *recs[0].TotalTokens != 6 {
		t.Errorf("TotalTokens = %v, want 6 — SSE usage parsing regressed", recs[0].TotalTokens)
	}
}

func TestReqlogLeavesProvenanceEmptyForLocalAndUntolerated(t *testing.T) {
	// A local upstream reports no provider, and a role with no tolerance
	// records none. Both must stay empty rather than picking up a placeholder.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	sink := &reqlog.MemorySink{}
	rt := newPrivacyRouterWithSink(t, upstream.URL, sink)

	if rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"anywhere","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if recs[0].UpstreamProvider != "" {
		t.Errorf("UpstreamProvider = %q, want empty", recs[0].UpstreamProvider)
	}
	if recs[0].PrivacyTolerance != "" {
		t.Errorf("PrivacyTolerance = %q, want empty", recs[0].PrivacyTolerance)
	}
}

func TestReqlogRecordsTheToleranceOnARefusal(t *testing.T) {
	// The row that matters most for an audit: the request that was REFUSED.
	// It has no serving provider by definition — the upstream was never
	// called — but it must still carry the tolerance that refused it, or the
	// refusal leaves no trace anyone can query.
	sink := &reqlog.MemorySink{}
	rt := newPrivacyRouterWithSink(t, "", sink)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen-seat","messages":[{"role":"user","content":"secret"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(PrivacyHeader, PrivacyZDR)
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	got := recs[0]
	if got.PrivacyTolerance != PrivacyZDR {
		t.Errorf("PrivacyTolerance = %q, want %q on a refused request", got.PrivacyTolerance, PrivacyZDR)
	}
	if got.UpstreamProvider != "" {
		t.Errorf("UpstreamProvider = %q, want empty — nothing served it", got.UpstreamProvider)
	}
	if got.Status != http.StatusForbidden {
		t.Errorf("Status = %d, want 403", got.Status)
	}
	if !strings.Contains(got.Error, "zero data retention") {
		t.Errorf("Error should say why it was refused: %q", got.Error)
	}
}

func TestParseUpstreamProviderIsAbsentSafe(t *testing.T) {
	// Every local backend hits this path on every request.
	cases := []struct{ name, body, want string }{
		{"openrouter", `{"provider":"DeepInfra","choices":[]}`, "DeepInfra"},
		{"local llama-server", `{"choices":[],"usage":{"prompt_tokens":1}}`, ""},
		{"not json", `<html>502</html>`, ""},
		{"empty", ``, ""},
		{"provider is not a string", `{"provider":{"order":["x"]}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseUpstreamProvider([]byte(tc.body)); got != tc.want {
				t.Errorf("parseUpstreamProvider(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// --- cost, cache and session headers ---------------------------------------

func TestReqlogRecordsProviderBilledCostAndCacheHit(t *testing.T) {
	// The OpenRouter usage shape, verbatim in the fields that matter: cost
	// alongside the token counts, cached_tokens nested in
	// prompt_tokens_details.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"Together","choices":[{"message":{"content":"ok"}}],
			"usage":{"prompt_tokens":1650,"completion_tokens":4,"total_tokens":1654,
			         "cost":9.718e-05,"prompt_tokens_details":{"cached_tokens":1536}}}`))
	}))
	defer upstream.Close()

	sink := &reqlog.MemorySink{}
	rt := newPrivacyRouterWithSink(t, upstream.URL, sink)
	if rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"private","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	got := sink.Records()[0]
	if got.UpstreamCostUSD == nil || *got.UpstreamCostUSD != 9.718e-05 {
		t.Errorf("UpstreamCostUSD = %v, want 9.718e-05", got.UpstreamCostUSD)
	}
	if got.CachedPromptTokens == nil || *got.CachedPromptTokens != 1536 {
		t.Errorf("CachedPromptTokens = %v, want 1536", got.CachedPromptTokens)
	}
	if got.PromptTokens == nil || *got.PromptTokens != 1650 {
		t.Errorf("PromptTokens = %v, want 1650", got.PromptTokens)
	}
}

func TestCostAndCacheSurviveTheSSETail(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"provider\":\"Together\",\"choices\":[{\"delta\":{\"content\":\"o\"}}]}\n\n" +
				"data: {\"provider\":\"Together\",\"choices\":[{\"delta\":{}}]," +
				"\"usage\":{\"prompt_tokens\":1650,\"completion_tokens\":4,\"total_tokens\":1654," +
				"\"cost\":0.00010868,\"prompt_tokens_details\":{\"cached_tokens\":1536}}}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	sink := &reqlog.MemorySink{}
	rt := newPrivacyRouterWithSink(t, upstream.URL, sink)
	if rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"private","stream":true,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	got := sink.Records()[0]
	if got.UpstreamCostUSD == nil || *got.UpstreamCostUSD != 0.00010868 {
		t.Errorf("UpstreamCostUSD = %v, want 0.00010868", got.UpstreamCostUSD)
	}
	if got.CachedPromptTokens == nil || *got.CachedPromptTokens != 1536 {
		t.Errorf("CachedPromptTokens = %v, want 1536", got.CachedPromptTokens)
	}
}

func TestZeroCachedTokensIsRecordedNotDroppedAsAbsent(t *testing.T) {
	// A total cache MISS reports cached_tokens: 0. That is the single most
	// interesting value in the field — it is what a broken prefix cache looks
	// like — so it must reach the row as 0, never as NULL.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"Parasail","choices":[],
			"usage":{"prompt_tokens":1650,"cost":0.0003005,"prompt_tokens_details":{"cached_tokens":0}}}`))
	}))
	defer upstream.Close()

	sink := &reqlog.MemorySink{}
	rt := newPrivacyRouterWithSink(t, upstream.URL, sink)
	postTo(t, rt, "/v1/chat/completions", `{"model":"private","messages":[]}`)

	got := sink.Records()[0]
	if got.CachedPromptTokens == nil {
		t.Fatal("CachedPromptTokens = nil, want a recorded 0 — a full cache miss is data, not absence")
	}
	if *got.CachedPromptTokens != 0 {
		t.Errorf("CachedPromptTokens = %d, want 0", *got.CachedPromptTokens)
	}
}

func TestLocalUpstreamReportsNoCostOrCache(t *testing.T) {
	// A local llama-server reports usage and nothing else. Both fields must
	// stay nil rather than defaulting to a fabricated zero cost.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	sink := &reqlog.MemorySink{}
	rt := newPrivacyRouterWithSink(t, upstream.URL, sink)
	postTo(t, rt, "/v1/chat/completions", `{"model":"anywhere","messages":[]}`)

	got := sink.Records()[0]
	if got.UpstreamCostUSD != nil {
		t.Errorf("UpstreamCostUSD = %v, want nil for an upstream that reports none", *got.UpstreamCostUSD)
	}
	if got.CachedPromptTokens != nil {
		t.Errorf("CachedPromptTokens = %v, want nil", *got.CachedPromptTokens)
	}
	if got.TotalTokens == nil || *got.TotalTokens != 15 {
		t.Errorf("TotalTokens = %v, want 15 — plain usage parsing regressed", got.TotalTokens)
	}
}

// Session-affinity headers must reach the upstream untouched.
//
// This is load-bearing, not incidental. Both providers the fleet uses key
// prompt-cache stickiness off a caller-supplied header — OpenRouter's
// x-session-id and Zen's x-opencode-session — and a cache miss measured ~3x
// the cost of a hit on the same prompt (2026-09-09). ReverseProxy forwards
// inbound headers by default, so this passes today; the test exists so that a
// future header-scrubbing change fails here instead of showing up as a
// quietly larger bill.
func TestSessionAffinityHeadersReachTheUpstream(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	rt := newPrivacyRouter(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"or-seat","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", "agent-run-42")
	req.Header.Set("X-Opencode-Session", "opencode-abc")
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if v := got.Get("X-Session-Id"); v != "agent-run-42" {
		t.Errorf("X-Session-Id upstream = %q, want agent-run-42 (OpenRouter sticky routing)", v)
	}
	if v := got.Get("X-Opencode-Session"); v != "opencode-abc" {
		t.Errorf("X-Opencode-Session upstream = %q, want opencode-abc (Zen sticky routing)", v)
	}
}

func TestUpstreamProviderLabelIsBounded(t *testing.T) {
	cases := []struct {
		name string
		rec  reqlog.Record
		want string
	}{
		{"cloud names the provider", reqlog.Record{UpstreamProvider: "Novita", BackendURL: "https://openrouter.ai"}, "Novita"},
		{"local is named, not blank", reqlog.Record{BackendURL: "http://delphi:5392"}, "local"},
		{"never forwarded is none", reqlog.Record{}, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamProviderLabel(tc.rec); got != tc.want {
				t.Errorf("upstreamProviderLabel = %q, want %q", got, tc.want)
			}
		})
	}
}
