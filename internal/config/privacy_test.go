package config

import (
	"strings"
	"testing"
)

// zdrBaseYAML holds one of each placement kind the local_or_zdr tolerance has
// to distinguish. Roles are appended per test so each case states its own
// contract.
const zdrBaseYAML = `
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
    capabilities: [text, tool_calling]
  zen-seat:
    hf_repo: kimi-k2.7-code
    backend: external
    api_base: https://opencode.ai/zen/go/v1
    capabilities: [text, tool_calling]
  all-zdr-chain:
    hf_repo: claude-sonnet-5
    backend: external
    fallbacks: [or-seat, or-seat-2]
    capabilities: [text, tool_calling]
  mixed-chain:
    hf_repo: claude-sonnet-5-mixed
    backend: external
    fallbacks: [or-seat, zen-seat]
    capabilities: [text, tool_calling]

roles:
`

func loadWithRole(t *testing.T, role string) (*ModelRegistry, error) {
	t.Helper()
	return LoadBytes([]byte(zdrBaseYAML + role))
}

func TestZDREnforceableIsDecidedByHost(t *testing.T) {
	cases := []struct {
		name string
		m    ModelDefinition
		want bool
	}{
		{"openrouter", ModelDefinition{APIBase: "https://openrouter.ai/api/v1"}, true},
		{"openrouter with port and caps", ModelDefinition{APIBase: "https://OpenRouter.AI:443/api/v1"}, true},
		{"zen", ModelDefinition{APIBase: "https://opencode.ai/zen/go/v1"}, false},
		{"local node has no api_base", ModelDefinition{Node: "delphi"}, false},
		// A lookalike host must not qualify: the table is matched on the
		// hostname, not on a substring of the URL.
		{"lookalike host", ModelDefinition{APIBase: "https://openrouter.ai.evil.example/v1"}, false},
		{"unparseable", ModelDefinition{APIBase: "://nope"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.ZDREnforceable(); got != tc.want {
				t.Errorf("ZDREnforceable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLocalOrZDRAcceptsLocalAndEnforceableCandidates(t *testing.T) {
	_, err := loadWithRole(t, `
  private:
    require: {locality: local_or_zdr}
    candidates: [or-seat, local-seat]
`)
	if err != nil {
		t.Fatalf("local + openrouter candidates should load: %v", err)
	}
}

func TestLocalOrZDRRejectsAnUnenforceableCandidate(t *testing.T) {
	_, err := loadWithRole(t, `
  private:
    require: {locality: local_or_zdr}
    candidates: [or-seat, zen-seat]
`)
	if err == nil {
		t.Fatal("expected a load error for a Zen candidate under local_or_zdr")
	}
	if !strings.Contains(err.Error(), "zen-seat") || !strings.Contains(err.Error(), "local_or_zdr") {
		t.Errorf("error should name the candidate and the tolerance: %v", err)
	}
}

func TestLocalOrZDRBindsTheOverflowList(t *testing.T) {
	// The divergence from locality: local, and the reason bindsOverflow
	// exists. An overflow that crosses a RETENTION boundary breaks the
	// promise the role makes, rather than being the point of overflowing.
	_, err := loadWithRole(t, `
  private:
    require: {locality: local_or_zdr}
    candidates: [local-seat]
    on_empty: overflow
    overflow: [zen-seat]
`)
	if err == nil {
		t.Fatal("expected a load error: a local_or_zdr role must not overflow to a retaining seat")
	}
	if !strings.Contains(err.Error(), "overflow") || !strings.Contains(err.Error(), "zen-seat") {
		t.Errorf("error should name the offending overflow entry: %v", err)
	}
}

func TestLocalOrZDRAllowsAnEnforceableOverflow(t *testing.T) {
	_, err := loadWithRole(t, `
  private:
    require: {locality: local_or_zdr}
    candidates: [local-seat]
    on_empty: overflow
    overflow: [or-seat]
`)
	if err != nil {
		t.Fatalf("an OpenRouter overflow satisfies the tolerance: %v", err)
	}
}

func TestLocalityLocalStillExemptsOverflow(t *testing.T) {
	// Regression guard: teaching overflow to bind for one locality must not
	// have changed the existing exemption, which several live roles rely on.
	_, err := loadWithRole(t, `
  coder:
    require: {locality: local}
    candidates: [local-seat]
    on_empty: overflow
    overflow: [zen-seat]
`)
	if err != nil {
		t.Fatalf("locality: local must still exempt its overflow list: %v", err)
	}
}

func TestChainIsOnlyAsPrivateAsItsWorstMember(t *testing.T) {
	_, err := loadWithRole(t, `
  private:
    require: {locality: local_or_zdr}
    candidates: [mixed-chain]
`)
	if err == nil {
		t.Fatal("expected a load error: a chain with a Zen member cannot satisfy local_or_zdr")
	}
	if !strings.Contains(err.Error(), "zen-seat") {
		t.Errorf("error should name the member that fails, not just the chain: %v", err)
	}
}

func TestAllEnforceableChainSatisfiesTheTolerance(t *testing.T) {
	_, err := loadWithRole(t, `
  private:
    require: {locality: local_or_zdr}
    candidates: [all-zdr-chain]
`)
	if err != nil {
		t.Fatalf("a chain whose every member is enforceable should load: %v", err)
	}
}

func TestUnknownLocalityErrorNamesEveryValue(t *testing.T) {
	// The error is how someone discovers the new tier exists; omitting it
	// would make local_or_zdr undiscoverable from a typo.
	_, err := loadWithRole(t, `
  private:
    require: {locality: nearby}
    candidates: [local-seat]
`)
	if err == nil {
		t.Fatal("expected a load error for an unknown locality")
	}
	for _, want := range []string{"any", "local", "local_or_zdr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should offer %q as a valid value: %v", want, err)
		}
	}
}

func TestLocalityDefaultIsUnchanged(t *testing.T) {
	// A role that declares no locality keeps taking anything, including Zen.
	reg, err := loadWithRole(t, `
  anywhere:
    candidates: [zen-seat]
`)
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if got := reg.Roles["anywhere"].Require.Locality; got != LocalityAny {
		t.Errorf("default locality = %q, want %q", got, LocalityAny)
	}
}
