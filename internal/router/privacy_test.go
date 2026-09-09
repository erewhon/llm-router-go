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
