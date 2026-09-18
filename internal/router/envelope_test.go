package router

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

// envelopeRegistryYAML gives the roles a deliberate size ladder:
//
//	thinker:  lightning (16k envelope, 256k window) -> gpt-oss (no envelope)
//	narrow:   only short-envelope candidates, on_empty: error
//	spill:    only short-envelope candidates, on_empty: overflow -> cloud
//	tiny:     a hard window with no envelope at all
const envelopeRegistryYAML = `
nodes:
  talos: {host: talos.local, gpu: intel, vram_gb: 32}
  delphi: {host: delphi.local, gpu: amd, vram_gb: 96}

models:
  lightning:
    hf_repo: nvidia/Nemotron-3.5-Lightning
    node: talos
    context_length: 262144
    effective_context: 16384
    capabilities: [text, tool_calling]
  gemma:
    hf_repo: google/gemma-4-26b
    node: talos
    api_port: 5392
    context_length: 131072
    effective_context: 32768
    capabilities: [text, tool_calling]
  gpt-oss:
    hf_repo: openai/gpt-oss-120b
    node: delphi
    context_length: 131072
    capabilities: [text, tool_calling]
  small-window:
    hf_repo: vendor/small
    node: delphi
    api_port: 5399
    context_length: 8192
    capabilities: [text, tool_calling]
  cloud:
    hf_repo: kimi-k2.7-code
    backend: external
    api_base: https://opencode.ai/zen/go/v1
    capabilities: [text, tool_calling]

roles:
  thinker:
    require: {locality: local}
    candidates: [lightning, gpt-oss]

  narrow:
    require: {locality: local}
    candidates: [lightning, gemma]
    on_empty: error

  spill:
    require: {locality: local}
    candidates: [lightning, gemma]
    on_empty: overflow
    overflow: [cloud]

  tiny:
    require: {locality: local}
    candidates: [small-window, gpt-oss]
`

func newEnvelopeRouter(t *testing.T) *Router {
	t.Helper()
	reg, err := config.LoadBytes([]byte(envelopeRegistryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMode("default"), WithFlushInterval(0))
}

// bodyCharsFor returns the request-body size that estimates to n tokens, for
// the end-to-end test that has to build a real body. Resolution takes a token
// count directly, so only that test needs the conversion.
func bodyCharsFor(n int) int { return int(float64(n) * charsPerToken) }

func TestNoEnvelopeMeansOnlyTheDeclaredWindowGates(t *testing.T) {
	// A model with no effective_context is never soft-gated: it keeps serving
	// right up to the window it declared, and is only refused past it (the
	// hard gate, which just moves a certain backend failure to the router).
	rt := newEnvelopeRouter(t)

	for _, promptTokens := range []int{0, 1000, 8000, 10_000, 10_241, 100_000, 130_000} {
		res, err := rt.resolveModel("tiny", false, promptTokens, tierNone)
		if err != nil {
			t.Fatalf("resolve tiny at %d tokens: %v", promptTokens, err)
		}
		want := "small-window"
		if float64(promptTokens) > 8192*hardGateSlack {
			// Past small-window's declared window BY MORE THAN THE SLACK the
			// HARD gate applies — the other half of the feature. Inside the
			// slack (10_000 on an 8192 window) the estimate is within its
			// own error band and the backend decides.
			want = "gpt-oss"
		}
		if res.ModelID != want {
			t.Errorf("tiny at %d tokens -> %q, want %q", promptTokens, res.ModelID, want)
		}
	}
}

func TestSoftGateSkipsCandidateAndBinds(t *testing.T) {
	rt := newEnvelopeRouter(t)

	// Inside the envelope: Lightning wins, as it should — this is exactly the
	// short-context advantage the gate exists to preserve.
	res, err := rt.resolveModel("thinker", false, 4000, tierNone)
	if err != nil {
		t.Fatalf("resolve thinker (short): %v", err)
	}
	if res.ModelID != "lightning" {
		t.Errorf("short prompt -> %q, want lightning", res.ModelID)
	}
	if res.Downshift != "" {
		t.Errorf("short prompt set Downshift = %q, want empty", res.Downshift)
	}

	// Past it: skipped exactly like an unavailable candidate.
	res, err = rt.resolveModel("thinker", false, 64_000, tierNone)
	if err != nil {
		t.Fatalf("resolve thinker (long): %v", err)
	}
	if res.ModelID != "gpt-oss" {
		t.Errorf("64k prompt -> %q, want gpt-oss", res.ModelID)
	}
	if res.Downshift != "lightning" {
		t.Errorf("Downshift = %q, want lightning", res.Downshift)
	}
	if res.Role != "thinker" {
		t.Errorf("Role = %q, want thinker", res.Role)
	}
}

func TestHardGateRefusesAtTheRouter(t *testing.T) {
	rt := newEnvelopeRouter(t)

	// 200k exceeds gpt-oss's 131k window and Lightning's 16k envelope, so the
	// role empties out rather than dispatching something that cannot fit.
	_, err := rt.resolveModel("thinker", false, 200_000, tierNone)
	var roleErr *roleUnavailableError
	if !errors.As(err, &roleErr) {
		t.Fatalf("err = %v, want roleUnavailableError", err)
	}
	joined := strings.Join(roleErr.Reasons, "; ")
	if !strings.Contains(joined, "context_length") {
		t.Errorf("reasons %q do not mention the hard limit", joined)
	}
}

func TestGateReasonIsReadable(t *testing.T) {
	rt := newEnvelopeRouter(t)

	_, err := rt.resolveModel("narrow", false, 74_000, tierNone)
	var roleErr *roleUnavailableError
	if !errors.As(err, &roleErr) {
		t.Fatalf("err = %v, want roleUnavailableError", err)
	}
	joined := strings.Join(roleErr.Reasons, "; ")
	// The string an operator reads at 2am, per the task spec.
	for _, want := range []string{"lightning:", "prompt ~72k tokens", "effective_context 16k", "gemma:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("reason %q missing %q", joined, want)
		}
	}
}

func TestAllGatedWithOnEmptyError(t *testing.T) {
	rt := newEnvelopeRouter(t)

	_, err := rt.resolveModel("narrow", false, 74_000, tierNone)
	var roleErr *roleUnavailableError
	if !errors.As(err, &roleErr) {
		t.Fatalf("err = %v, want roleUnavailableError", err)
	}
	if !strings.Contains(err.Error(), "on_empty=error") {
		t.Errorf("error %q should say the role declined to overflow", err.Error())
	}
}

func TestAllGatedWithOnEmptyOverflowReachesTheCloud(t *testing.T) {
	// The composition the task calls out: every local candidate is
	// short-envelope, so a long prompt legitimately reaches the overflow list.
	rt := newEnvelopeRouter(t)

	res, err := rt.resolveModel("spill", false, 74_000, tierNone)
	if err != nil {
		t.Fatalf("resolve spill: %v", err)
	}
	if res.ModelID != "cloud" {
		t.Errorf("spill -> %q, want cloud", res.ModelID)
	}
	if !res.Overflowed {
		t.Errorf("Overflowed = false, want true")
	}
	if res.Downshift != "lightning" {
		t.Errorf("Downshift = %q, want lightning", res.Downshift)
	}
}

func TestDirectlyNamedModelIsNeverGated(t *testing.T) {
	// "You asked for it, you get it": naming a model bypasses roles, so the
	// envelope must not touch it — the escape hatch the binding gate relies on.
	rt := newEnvelopeRouter(t)

	res, err := rt.resolveModel("lightning", false, 200_000, tierNone)
	if err != nil {
		t.Fatalf("resolve lightning directly: %v", err)
	}
	if res.ModelID != "lightning" {
		t.Errorf("direct name -> %q, want lightning", res.ModelID)
	}
	if res.PromptTokens != 0 {
		t.Errorf("PromptTokens = %d on a direct name, want 0 (gate disabled)", res.PromptTokens)
	}
}

func TestIntrospectionPathsAreUngated(t *testing.T) {
	// /v1/availability resolves with no request in hand and must keep
	// reporting where a role points.
	rt := newEnvelopeRouter(t)

	for _, b := range rt.roleBindings() {
		if b.Role != "thinker" {
			continue
		}
		if !b.Available || b.Target != "lightning" {
			t.Errorf("thinker binding = (%v, %q), want (true, lightning)", b.Available, b.Target)
		}
	}
}

func TestDownshiftHeaderIsSet(t *testing.T) {
	rt := newEnvelopeRouter(t)
	rec := httptest.NewRecorder()

	rt.setRoleHeaders(rec, resolveResult{
		Role:      "thinker",
		ModelID:   "gpt-oss",
		Downshift: "lightning",
	})

	got := rec.Header().Get("X-Router-Downshift")
	if got != "lightning -> gpt-oss (context)" {
		t.Errorf("X-Router-Downshift = %q, want %q", got, "lightning -> gpt-oss (context)")
	}
}

func TestNoDownshiftHeaderWhenNothingWasSkipped(t *testing.T) {
	rt := newEnvelopeRouter(t)
	rec := httptest.NewRecorder()

	rt.setRoleHeaders(rec, resolveResult{Role: "thinker", ModelID: "lightning"})

	if got := rec.Header().Get("X-Router-Downshift"); got != "" {
		t.Errorf("X-Router-Downshift = %q, want empty", got)
	}
}

func TestFailoverWalkRespectsTheEnvelope(t *testing.T) {
	// A role's failover tail must not land on a candidate the first pass would
	// have gated: same request, same envelope.
	reg, err := config.LoadBytes([]byte(envelopeRegistryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	rt := New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMode("default"), WithFlushInterval(0))

	// Resolve "narrow" at a size gemma passes but lightning does not, then
	// confirm the tail offers nothing gated.
	res, err := rt.resolveModel("narrow", false, 20_000, tierNone)
	if err != nil {
		t.Fatalf("resolve narrow: %v", err)
	}
	if res.ModelID != "gemma" {
		t.Fatalf("narrow at 20k -> %q, want gemma", res.ModelID)
	}
	if _, ok := rt.nextRoleCandidate(res, false); ok {
		t.Errorf("failover offered a candidate, but the tail is empty")
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	if got := estimatePromptTokens(nil); got != 0 {
		t.Errorf("nil body -> %d, want 0", got)
	}
	if got := estimatePromptTokens(map[string]any{"model": "x"}); got != 0 {
		t.Errorf("no messages -> %d, want 0 (gate disabled)", got)
	}
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": strings.Repeat("x", int(3500*charsPerToken/3.5))},
	}}
	want := 1000 + messageOverheadTokens
	if got := estimatePromptTokens(body); got != want {
		t.Errorf("one 1000-token message -> %d, want %d", got, want)
	}
}

// The regression: an agent transcript's JSON envelope — escaped newlines and
// tabs in every line of code, the per-turn tool-result wrapper — must not
// count. Measured against the same body serialized, the estimate has to
// track the text, not the bytes.
func TestEstimateIgnoresTheJSONEnvelope(t *testing.T) {
	line := "\t\tlog.Printf(\"read %q: %v\\n\", path, err)\n"
	code := strings.Repeat(line, 3000) // ~110K chars: tabs, quotes and newlines on every line
	var msgs []any
	msgs = append(msgs, map[string]any{"role": "system", "content": "You are a coding agent."})
	for i := 0; i < 5; i++ {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "c", "type": "function", "function": map[string]any{
					"name": "read", "arguments": `{"path":"/repo/file.go"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "c", "content": code},
		)
	}
	body := map[string]any{"messages": msgs, "tools": []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name": "read", "description": "Read a file", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
	}}
	got := estimatePromptTokens(body)

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	byBytes := int(float64(len(raw)) / charsPerToken)
	textChars := 5 * len(code)
	// Every tab, quote and newline in the code is one byte of text but two of
	// JSON, so the old estimate ran a clear fraction above the text-based one.
	if byBytes < int(float64(got)*1.10) {
		t.Fatalf("fixture is not envelope-heavy enough: bytes-estimate %d vs text-estimate %d", byBytes, got)
	}
	lo := int(float64(textChars) / charsPerToken)
	hi := int(float64(textChars)/charsPerToken*1.05) + 11*messageOverheadTokens
	if got < lo || got > hi {
		t.Errorf("estimate %d outside [%d, %d] for %d chars of text", got, lo, hi, textChars)
	}
}

func TestEstimateCountsImagesFlat(t *testing.T) {
	// A base64 image is ~1.3 MB of body for one picture; it must cost the
	// flat per-image amount, not a third of a million tokens.
	data := "data:image/png;base64," + strings.Repeat("A", 1_300_000)
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is this?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": data}},
		}},
	}}
	got := estimatePromptTokens(body)
	q := "what is this?"
	want := imageTokens + messageOverheadTokens + int(float64(len(q))/charsPerToken)
	if got != want {
		t.Errorf("image message -> %d, want %d", got, want)
	}
}

func TestFormatTokens(t *testing.T) {
	// k is 1024 throughout, so a 131072 window reads as the 128k seat people
	// actually call it.
	for _, tc := range []struct {
		in   int
		want string
	}{{0, "0"}, {999, "999"}, {1024, "1k"}, {16384, "16k"}, {74213, "72k"}, {131072, "128k"}} {
		if got := formatTokens(tc.in); got != tc.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEffectiveContextWiderThanWindowIsRejected(t *testing.T) {
	_, err := config.LoadBytes([]byte(`
nodes:
  talos: {host: talos.local, gpu: intel, vram_gb: 32}
models:
  bad:
    hf_repo: vendor/bad
    node: talos
    context_length: 8192
    effective_context: 16384
`))
	if err == nil {
		t.Fatal("expected a load error for effective_context > context_length")
	}
	if !strings.Contains(err.Error(), "effective_context") {
		t.Errorf("error %q should name the offending field", err.Error())
	}
}

// Guard the end-to-end shape: a long chat request through a role gets the
// downshift header on the way back out.
func TestDownshiftHeaderOnARealRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	reg, err := config.LoadBytes([]byte(envelopeRegistryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	rt := New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMode("default"), WithFlushInterval(0),
		WithTransport(&transportRedirect{to: upstream.URL, rt: http.DefaultTransport}))

	// A body big enough to blow past Lightning's 16k envelope but to stay
	// inside gpt-oss's 131k window.
	filler := strings.Repeat("a", bodyCharsFor(40_000))
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"thinker","messages":[{"role":"user","content":"`+filler+`"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Router-Resolved"); got != "gpt-oss" {
		t.Errorf("X-Router-Resolved = %q, want gpt-oss", got)
	}
	if got := rec.Header().Get("X-Router-Downshift"); got != "lightning -> gpt-oss (context)" {
		t.Errorf("X-Router-Downshift = %q, want the lightning downshift", got)
	}
}

// The counterpart: a short request through the same role keeps Lightning and
// emits no downshift header at all.
func TestNoDownshiftOnAShortRealRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	reg, err := config.LoadBytes([]byte(envelopeRegistryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	rt := New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMode("default"), WithFlushInterval(0),
		WithTransport(&transportRedirect{to: upstream.URL, rt: http.DefaultTransport}))

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"thinker","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Router-Resolved"); got != "lightning" {
		t.Errorf("X-Router-Resolved = %q, want lightning", got)
	}
	if got := rec.Header().Get("X-Router-Downshift"); got != "" {
		t.Errorf("X-Router-Downshift = %q, want empty", got)
	}
}
