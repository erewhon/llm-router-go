package router

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

// roleRegistryYAML mirrors the real fleet's shape: two Spark models and a CPU
// last-resort for the "coder" role, a strict "thinker" that refuses to
// substitute, and a mode:big model that must stay invisible in default mode.
const roleRegistryYAML = `
nodes:
  hypatia: {host: hypatia.local, gpu: nvidia, vram_gb: 128}
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
  hekaton: {host: 192.168.42.20, gpu: none, vram_gb: 0}

models:
  qwen36-hypatia:
    hf_repo: Qwen/Qwen3.6-35B-A3B-FP8
    node: hypatia
    capabilities: [text, vision, tool_calling]
    aliases: [qwen3.6-local]
    tags: [mode:default]
  minimax-reap:
    hf_repo: MiniMax/M2.7-REAP
    node: archimedes
    capabilities: [text, tool_calling]
    aliases: [minimax]
    tags: [mode:default]
  ling-flash-local:
    hf_repo: inclusionAI/Ling-flash-2.0
    node: hekaton
    capabilities: [text, tool_calling]
    aliases: [ling]
  big-coder:
    hf_repo: Qwen/Qwen3.5-122B-A10B
    node: archimedes
    capabilities: [text, tool_calling]
    tags: [mode:big]
  kimi-cloud:
    hf_repo: kimi-k2.7-code
    backend: external
    api_base: https://opencode.ai/zen/go/v1
    capabilities: [text, tool_calling]

roles:
  coder:
    description: local coding model
    require:
      locality: local
      capabilities: [text, tool_calling]
    candidates: [qwen36-hypatia, minimax-reap, big-coder, ling-flash-local]
    on_empty: overflow
    overflow: [kimi-cloud]

  thinker:
    require: {locality: local}
    candidates: [minimax-reap, qwen36-hypatia]
`

// stubAvailability lets a test declare exactly which models are down.
type stubAvailability struct {
	down      map[string]bool
	failures  []string
	successes []string
}

func (s *stubAvailability) Routable(id string) bool { return !s.down[id] }
func (s *stubAvailability) Reason(id string) string {
	if s.down[id] {
		return "unavailable (poll: node unreachable)"
	}
	return "available (poll)"
}
func (s *stubAvailability) ReportFailure(id string, _ error) { s.failures = append(s.failures, id) }
func (s *stubAvailability) ReportSuccess(id string)          { s.successes = append(s.successes, id) }

func newRoleRouter(t *testing.T, down map[string]bool, extra ...Option) (*Router, *stubAvailability) {
	t.Helper()
	reg, err := config.LoadBytes([]byte(roleRegistryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if down == nil {
		down = map[string]bool{}
	}
	avail := &stubAvailability{down: down}
	opts := append([]Option{
		WithMode("default"),
		WithAvailability(avail),
		WithFlushInterval(0),
	}, extra...)
	return New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...), avail
}

func TestRoleResolvesToFirstAvailableCandidate(t *testing.T) {
	rt, _ := newRoleRouter(t, nil)

	res, err := rt.resolveModel("coder", false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolve coder: %v", err)
	}
	if res.ModelID != "qwen36-hypatia" {
		t.Errorf("coder resolved to %q, want qwen36-hypatia (first candidate)", res.ModelID)
	}
	if res.Role != "coder" {
		t.Errorf("Role = %q, want coder", res.Role)
	}
	if res.Overflowed {
		t.Errorf("Overflowed = true, want false while a candidate is up")
	}
}

func TestRoleFailsOverWhenPrimaryIsDown(t *testing.T) {
	// The motivating scenario: hypatia is powered down overnight.
	rt, _ := newRoleRouter(t, map[string]bool{"qwen36-hypatia": true})

	res, err := rt.resolveModel("coder", false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolve coder: %v", err)
	}
	if res.ModelID != "minimax-reap" {
		t.Errorf("coder resolved to %q, want minimax-reap", res.ModelID)
	}
	if res.BackendURL != "http://archimedes.local:5391" {
		t.Errorf("BackendURL = %q, want archimedes", res.BackendURL)
	}
}

func TestRoleSkipsOutOfModeCandidates(t *testing.T) {
	// big-coder sits between minimax-reap and ling-flash-local in the
	// preference order but carries mode:big — in default mode it must not
	// exist at all, and the role must fall through to hekaton.
	rt, _ := newRoleRouter(t, map[string]bool{
		"qwen36-hypatia": true,
		"minimax-reap":   true,
	})
	res, err := rt.resolveModel("coder", false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolve coder: %v", err)
	}
	if res.ModelID != "ling-flash-local" {
		t.Errorf("coder resolved to %q, want ling-flash-local (big-coder is mode:big)", res.ModelID)
	}
}

func TestRoleOverflowsOnlyWhenEverythingLocalIsDown(t *testing.T) {
	rt, _ := newRoleRouter(t, map[string]bool{
		"qwen36-hypatia":   true,
		"minimax-reap":     true,
		"ling-flash-local": true,
	})
	res, err := rt.resolveModel("coder", false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolve coder: %v", err)
	}
	if res.ModelID != "kimi-cloud" {
		t.Errorf("coder resolved to %q, want kimi-cloud from the overflow list", res.ModelID)
	}
	if !res.Overflowed {
		t.Errorf("Overflowed = false; crossing the locality boundary must be reported")
	}
}

func TestStrictRoleErrorsRatherThanSubstitute(t *testing.T) {
	rt, _ := newRoleRouter(t, map[string]bool{
		"minimax-reap":   true,
		"qwen36-hypatia": true,
	})
	_, err := rt.resolveModel("thinker", false, 0, tierNone)
	if err == nil {
		t.Fatalf("expected an error; thinker declares on_empty: error")
	}
	var roleErr *roleUnavailableError
	if !errors.As(err, &roleErr) {
		t.Fatalf("error type = %T, want *roleUnavailableError", err)
	}
	// The useful 2am error names each candidate and why it's out.
	for _, want := range []string{"minimax-reap", "qwen36-hypatia", "no overflow configured"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err.Error(), want)
		}
	}
}

func TestDirectModelNameNeverFailsOver(t *testing.T) {
	// Naming a model is a statement about *that* model. A down model must
	// resolve to itself and fail honestly rather than answer from elsewhere.
	rt, _ := newRoleRouter(t, map[string]bool{"qwen36-hypatia": true})

	for _, name := range []string{"qwen36-hypatia", "qwen3.6-local", "Qwen/Qwen3.6-35B-A3B-FP8"} {
		res, err := rt.resolveModel(name, false, 0, tierNone)
		if err != nil {
			t.Fatalf("resolve %q: %v", name, err)
		}
		if res.ModelID != "qwen36-hypatia" {
			t.Errorf("%q resolved to %q, want qwen36-hypatia — direct names must not reassign", name, res.ModelID)
		}
		if res.Role != "" {
			t.Errorf("%q set Role = %q, want empty", name, res.Role)
		}
		if len(res.Remaining) != 0 {
			t.Errorf("%q has a retry tail; direct names must not fail over", name)
		}
	}
}

func TestRoleRemainingTailDrivesFailover(t *testing.T) {
	rt, _ := newRoleRouter(t, nil)
	res, err := rt.resolveModel("coder", false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolve coder: %v", err)
	}
	want := []string{"minimax-reap", "ling-flash-local", "kimi-cloud"}
	if len(res.Remaining) != len(want) {
		t.Fatalf("Remaining = %v, want %v", res.Remaining, want)
	}
	for i, id := range want {
		if res.Remaining[i].ModelID != id {
			t.Errorf("Remaining[%d] = %q, want %q", i, res.Remaining[i].ModelID, id)
		}
	}
	if !res.Remaining[2].Overflow {
		t.Errorf("kimi-cloud should be flagged as an overflow entry")
	}

	next, ok := rt.nextRoleCandidate(res, false)
	if !ok {
		t.Fatalf("expected a next candidate")
	}
	if next.ModelID != "minimax-reap" || next.Role != "coder" {
		t.Errorf("next = %q role %q, want minimax-reap/coder", next.ModelID, next.Role)
	}
}

func TestNextRoleCandidateSkipsUnavailable(t *testing.T) {
	rt, _ := newRoleRouter(t, map[string]bool{"minimax-reap": true})
	res, err := rt.resolveModel("coder", false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolve coder: %v", err)
	}
	next, ok := rt.nextRoleCandidate(res, false)
	if !ok {
		t.Fatalf("expected a next candidate")
	}
	if next.ModelID != "ling-flash-local" {
		t.Errorf("next = %q, want ling-flash-local (minimax-reap is down)", next.ModelID)
	}
}

func TestNextRoleCandidateExhausted(t *testing.T) {
	rt, _ := newRoleRouter(t, nil)
	res, err := rt.resolveModel("thinker", false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolve thinker: %v", err)
	}
	// thinker has two candidates; after the second there is nothing left.
	second, ok := rt.nextRoleCandidate(res, false)
	if !ok {
		t.Fatalf("expected a second candidate")
	}
	if _, ok := rt.nextRoleCandidate(second, false); ok {
		t.Errorf("expected the tail to be exhausted after the last candidate")
	}
}

func TestRoleNamesListed(t *testing.T) {
	rt, _ := newRoleRouter(t, nil)
	got := rt.RoleNames()
	want := []string{"coder", "thinker"}
	if len(got) != len(want) {
		t.Fatalf("RoleNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RoleNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRoleUnavailableReturns503(t *testing.T) {
	rt, _ := newRoleRouter(t, map[string]bool{
		"minimax-reap":   true,
		"qwen36-hypatia": true,
	})
	rec := postTo(t, rt, "/v1/chat/completions", `{"model":"thinker","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for an unavailable role", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "thinker") {
		t.Errorf("body %q should name the role", rec.Body.String())
	}
}

func TestUnknownModelStill404(t *testing.T) {
	rt, _ := newRoleRouter(t, nil)
	rec := postTo(t, rt, "/v1/chat/completions", `{"model":"no-such-thing","messages":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown model", rec.Code)
	}
}

func TestModelsListIncludesRoles(t *testing.T) {
	rt, _ := newRoleRouter(t, nil)
	rec := getFrom(t, rt, "/v1/models")

	var got struct {
		Data []struct {
			ID       string `json:"id"`
			OwnedBy  string `json:"owned_by"`
			APIClass string `json:"api_class"`
			Role     bool   `json:"role"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]bool{}
	for _, e := range got.Data {
		byID[e.ID] = e.Role
		if e.ID == "coder" {
			if !e.Role {
				t.Errorf("coder should be flagged as a role")
			}
			if e.OwnedBy != "role" {
				t.Errorf("coder owned_by = %q, want role", e.OwnedBy)
			}
			if e.APIClass != "chat" {
				t.Errorf("coder api_class = %q, want chat", e.APIClass)
			}
		}
	}
	for _, want := range []string{"coder", "thinker"} {
		if _, ok := byID[want]; !ok {
			t.Errorf("/v1/models is missing role %q", want)
		}
	}
	// Concrete models and identity aliases must still be listed and NOT
	// flagged as roles.
	for _, want := range []string{"qwen36-hypatia", "qwen3.6-local", "minimax"} {
		isRole, ok := byID[want]
		if !ok {
			t.Errorf("/v1/models is missing %q", want)
		}
		if isRole {
			t.Errorf("%q should not be flagged as a role", want)
		}
	}
}

func TestModelsListStillAdvertisesADownRole(t *testing.T) {
	// A role whose candidates are all off must still be listed: it comes back
	// when the fleet does, and clients enumerate this list once at startup.
	rt, _ := newRoleRouter(t, map[string]bool{
		"minimax-reap":   true,
		"qwen36-hypatia": true,
	})
	rec := getFrom(t, rt, "/v1/models")
	if !strings.Contains(rec.Body.String(), `"thinker"`) {
		t.Errorf("/v1/models should still advertise an unavailable role")
	}
}

func TestWellKnownIncludesRoles(t *testing.T) {
	rt, _ := newRoleRouter(t, nil, WithWellKnown(WellKnownConfig{
		ProviderID:   "llm",
		ProviderName: "Fleet",
		BaseURL:      "http://localhost:4010",
	}))
	rec := getFrom(t, rt, "/.well-known/opencode")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var doc struct {
		Config struct {
			Provider map[string]struct {
				Models map[string]any `json:"models"`
			} `json:"provider"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	models := doc.Config.Provider["llm"].Models
	// Roles are the names worth binding in OpenCode: they survive a node
	// being powered down where a concrete model name does not.
	for _, want := range []string{"coder", "thinker"} {
		if _, ok := models[want]; !ok {
			t.Errorf("well-known is missing role %q; has %v", want, keysOf(models))
		}
	}
	// Concrete aliases are still there — an explicit model is still selectable.
	if _, ok := models["qwen3.6-local"]; !ok {
		t.Errorf("well-known dropped the identity alias qwen3.6-local")
	}
}

// ---------------------------------------------------------------------------
// Roles whose candidates are fallback-chain entries (the coder-hard shape:
// candidates [kimi, m3] where each is a virtual chain over provider entries).
// Before expandChains, resolving such a role failed with "model is
// multi-node": the chain entry has no node and no api_base of its own.
// ---------------------------------------------------------------------------

const roleChainYAML = `
models:
  go/kimi:
    hf_repo: kimi-code
    backend: external
    api_base: https://go.example/v1
    api_key: sk-literal-go
    capabilities: [text, tool_calling]

  or/kimi:
    hf_repo: moonshotai/kimi-code
    backend: external
    api_base: https://or.example/v1
    api_key: sk-literal-or
    capabilities: [text, tool_calling]

  kimi:
    hf_repo: kimi-virtual
    backend: external
    fallbacks: [go/kimi, or/kimi]
    capabilities: [text, tool_calling]

  or/m3:
    hf_repo: minimax/m3
    backend: external
    api_base: https://or.example/v1
    api_key: sk-literal-or
    capabilities: [text, tool_calling]

  m3:
    hf_repo: m3-virtual
    backend: external
    fallbacks: [or/m3]
    capabilities: [text, tool_calling]

roles:
  coder-hard:
    description: escalation target for hard coding tasks
    require:
      capabilities: [text, tool_calling]
    candidates: [kimi, m3]
    on_empty: error
`

func newRoleChainRouter(t *testing.T, down map[string]bool) (*Router, *stubAvailability) {
	t.Helper()
	reg, err := config.LoadBytes([]byte(roleChainYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if down == nil {
		down = map[string]bool{}
	}
	avail := &stubAvailability{down: down}
	return New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithAvailability(avail), WithFlushInterval(0)), avail
}

func TestRoleWithChainCandidatesResolvesToProvider(t *testing.T) {
	rt, _ := newRoleChainRouter(t, nil)

	res, err := rt.resolveModel("coder-hard", false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolve coder-hard: %v", err)
	}
	if res.ModelID != "go/kimi" {
		t.Errorf("resolved to %q, want go/kimi (first provider of first chain)", res.ModelID)
	}
	if res.Role != "coder-hard" {
		t.Errorf("Role = %q, want coder-hard", res.Role)
	}
	if res.Chain != "kimi" {
		t.Errorf("Chain = %q, want kimi (the chain the provider expanded from)", res.Chain)
	}
	// The untried tail crosses from kimi's remaining provider into m3's.
	want := []roleCandidate{
		{ModelID: "or/kimi", Chain: "kimi"},
		{ModelID: "or/m3", Chain: "m3"},
	}
	if len(res.Remaining) != len(want) {
		t.Fatalf("Remaining = %+v, want %+v", res.Remaining, want)
	}
	for i, w := range want {
		if res.Remaining[i] != w {
			t.Errorf("Remaining[%d] = %+v, want %+v", i, res.Remaining[i], w)
		}
	}
}

func TestRoleWithChainCandidatesFailsOverAcrossChains(t *testing.T) {
	rt, _ := newRoleChainRouter(t, map[string]bool{"go/kimi": true, "or/kimi": true})

	res, err := rt.resolveModel("coder-hard", false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolve coder-hard: %v", err)
	}
	if res.ModelID != "or/m3" {
		t.Errorf("resolved to %q, want or/m3 (second chain's provider)", res.ModelID)
	}
	if res.Chain != "m3" {
		t.Errorf("Chain = %q, want m3 — the label must follow the serving chain", res.Chain)
	}
}

func TestNextRoleCandidateRelabelsChainAcrossHops(t *testing.T) {
	rt, _ := newRoleChainRouter(t, nil)

	res, err := rt.resolveModel("coder-hard", false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolve coder-hard: %v", err)
	}
	next, ok := rt.nextRoleCandidate(res, false)
	if !ok || next.ModelID != "or/kimi" || next.Chain != "kimi" {
		t.Fatalf("first hop = %+v ok=%v, want or/kimi under chain kimi", next, ok)
	}
	next2, ok := rt.nextRoleCandidate(next, false)
	if !ok || next2.ModelID != "or/m3" || next2.Chain != "m3" {
		t.Fatalf("second hop = %+v ok=%v, want or/m3 under chain m3", next2, ok)
	}
	if _, ok := rt.nextRoleCandidate(next2, false); ok {
		t.Errorf("third hop should exhaust the tail")
	}
}

func TestRoleBindingsReportChainBackedRoleAvailable(t *testing.T) {
	rt, _ := newRoleChainRouter(t, nil)

	for _, b := range rt.roleBindings() {
		if b.Role != "coder-hard" {
			continue
		}
		if !b.Available {
			t.Fatalf("coder-hard unavailable, reasons: %v", b.Reasons)
		}
		if b.Target != "go/kimi" {
			t.Errorf("target = %q, want go/kimi", b.Target)
		}
		return
	}
	t.Fatalf("coder-hard binding missing")
}
