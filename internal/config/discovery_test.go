package config

import (
	"strings"
	"testing"
)

const discoveryYAML = `
nodes:
  n1: {host: n1.local, gpu: nvidia, vram_gb: 80}
roles:
  coder:
    require: {locality: any}
    candidates: [zen/pinned]
    on_empty: error
models:
  zen/pinned:
    hf_repo: pinned
    backend: external
    api_base: https://zen.example/v1
    aliases: [pinned-alias]
    input_cost_per_million: 1
    output_cost_per_million: 2
discovery:
  - api_base: https://zen.example/v1
    prefix: zen/
    api_key: ZEN_KEY
    adopt: all
    tags: [zen]
  - api_base: https://or.example/api/v1/
    prefix: or/
    api_key: OR_KEY
    adopt: ["anthropic/claude-*", "deepseek/*"]
    tags: [openrouter]
    context_length: 4096
`

func TestDiscovery_ParsesBothAdoptShapes(t *testing.T) {
	reg, err := LoadBytes([]byte(discoveryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if len(reg.Discovery) != 2 {
		t.Fatalf("discovery sources = %d, want 2", len(reg.Discovery))
	}
	zen, or := reg.Discovery[0], reg.Discovery[1]
	if !zen.Adopt.All || len(zen.Adopt.Patterns) != 0 {
		t.Errorf("zen adopt = %+v, want All", zen.Adopt)
	}
	if or.Adopt.All || len(or.Adopt.Patterns) != 2 {
		t.Errorf("or adopt = %+v, want two patterns", or.Adopt)
	}
	if got := or.Root(); got != "https://or.example/api" {
		t.Errorf("Root() = %q, want trailing slash and /v1 stripped", got)
	}
	if got := zen.Root(); got != "https://zen.example" {
		t.Errorf("Root() = %q", got)
	}
}

func TestAdoptPolicy_Matches(t *testing.T) {
	p := AdoptPolicy{Patterns: []string{"anthropic/claude-*", "deepseek/*", "z-ai/glm-5*"}}
	cases := map[string]bool{
		"anthropic/claude-opus-5":   true,
		"anthropic/claude-sonnet-5": true,
		"anthropic/claude":          false,
		"deepseek/deepseek-v4-pro":  true,
		"deepseek/deepseek-v4:free": true,
		"deepseek/x/y":              true, // * crosses slashes: ids are not paths
		"z-ai/glm-5.3-flash":        true,
		"z-ai/glm-4.7":              false,
		"moonshotai/kimi-k3":        false,
		"anthropic/claude-opus-5/x": true, // likewise
		"":                          false,
	}
	for id, want := range cases {
		if got := p.Matches(id); got != want {
			t.Errorf("Matches(%q) = %v, want %v", id, got, want)
		}
	}
	if !(AdoptPolicy{All: true}).Matches("anything/at/all") {
		t.Error("All must match everything")
	}
	if (AdoptPolicy{}).Matches("x") {
		t.Error("empty policy must match nothing")
	}

	// Exclude vetoes what Adopt accepted, and only that.
	src := DiscoverySource{Adopt: p, Exclude: []string{"*:batch", "*-exp", "*-0[0-9][0-9][0-9]"}}
	for id, want := range map[string]bool{
		"anthropic/claude-opus-5":        true,
		"anthropic/claude-opus-5:batch":  false,
		"deepseek/deepseek-v4-flash-exp": false,
		"deepseek/deepseek-v4-pro-0813":  false,
		"deepseek/deepseek-v4-pro":       true,
		"moonshotai/kimi-k3":             false, // not adopted in the first place
	} {
		if got := src.Adopts(id); got != want {
			t.Errorf("Adopts(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestDiscovery_ValidationErrors(t *testing.T) {
	base := `
nodes:
  n1: {host: n1.local, gpu: nvidia, vram_gb: 80}
models:
  m:
    hf_repo: m
    node: n1
`
	cases := []struct {
		name, block, want string
	}{
		{"missing api_base", "discovery:\n  - prefix: x/\n    adopt: all\n", "api_base is required"},
		{"relative api_base", "discovery:\n  - api_base: zen/v1\n    prefix: x/\n    adopt: all\n", "absolute URL"},
		{"missing prefix", "discovery:\n  - api_base: https://a.example/v1\n    adopt: all\n", "prefix is required"},
		{"duplicate prefix", "discovery:\n  - {api_base: https://a.example/v1, prefix: x/, adopt: all}\n  - {api_base: https://b.example/v1, prefix: x/, adopt: all}\n", "already used by discovery[0]"},
		{"empty adopt list", "discovery:\n  - api_base: https://a.example/v1\n    prefix: x/\n    adopt: []\n", "non-empty list"},
		{"bad adopt scalar", "discovery:\n  - api_base: https://a.example/v1\n    prefix: x/\n    adopt: some\n", "want `all` or a list"},
		{"bad glob", "discovery:\n  - api_base: https://a.example/v1\n    prefix: x/\n    adopt: [\"[\"]\n", "unterminated character class"},
		{"bad exclude glob", "discovery:\n  - api_base: https://a.example/v1\n    prefix: x/\n    adopt: all\n    exclude: [\"[\"]\n", "exclude pattern"},
		{"unknown capability", "discovery:\n  - api_base: https://a.example/v1\n    prefix: x/\n    adopt: all\n    capabilities: [text, tools]\n", `unknown capability "tools"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(base + tc.block))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDiscovery_EntryBuildsARoutableExternal(t *testing.T) {
	reg, err := LoadBytes([]byte(discoveryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	or := reg.Discovery[1]
	in, out := 3.0, 15.0
	m := or.Entry(ListedModel{
		ID: "anthropic/claude-opus-5", ContextLength: 200000, MaxOutputTokens: 32000,
		InputCostPerMillion: &in, OutputCostPerMillion: &out, ToolCalling: true, Vision: true,
	})
	if m.Backend != BackendExternal || m.APIBase != or.APIBase || m.APIKey != "OR_KEY" {
		t.Errorf("routing fields wrong: %+v", m)
	}
	if !m.Enabled || m.APIClass != APIClassChat || m.HFRepo != "anthropic/claude-opus-5" {
		t.Errorf("entry shape wrong: %+v", m)
	}
	if m.ContextLength != 200000 || m.MaxOutputTokens != 32000 {
		t.Errorf("limits not taken from the listing: ctx=%d out=%d", m.ContextLength, m.MaxOutputTokens)
	}
	if m.InputCostPerMillion == nil || *m.InputCostPerMillion != 3 || *m.OutputCostPerMillion != 15 {
		t.Errorf("pricing not carried: %v %v", m.InputCostPerMillion, m.OutputCostPerMillion)
	}
	if !m.HasCapability(CapText) || !m.HasCapability(CapToolCalling) || !m.HasCapability(CapVision) {
		t.Errorf("capabilities = %v", m.Capabilities)
	}
	if !m.IsDiscovered() || !containsString(m.Tags, "paid") || !containsString(m.Tags, "openrouter") {
		t.Errorf("tags = %v, want discovered+paid+openrouter", m.Tags)
	}
	if m.IsLocal() || m.IsVirtual() {
		t.Error("a discovered entry is a concrete external")
	}
	if !m.ZDREnforceable() && strings.Contains(or.APIBase, "openrouter.ai") {
		t.Error("openrouter discovered entries must stay ZDR-enforceable")
	}

	// Source defaults fill in what the listing left out; a free tier drops
	// the paid tag; nothing is claimed beyond text.
	zen := reg.Discovery[0]
	zero := 0.0
	z := zen.Entry(ListedModel{ID: "big-pickle", InputCostPerMillion: &zero, OutputCostPerMillion: &zero})
	if containsString(z.Tags, "paid") {
		t.Errorf("free tier must not be tagged paid: %v", z.Tags)
	}
	if len(z.Capabilities) != 1 || z.Capabilities[0] != CapText {
		t.Errorf("capabilities = %v, want [text] only", z.Capabilities)
	}
	u := or.Entry(ListedModel{ID: "deepseek/deepseek-v4-pro"})
	if u.ContextLength != 4096 {
		t.Errorf("context_length default = %d, want the source's 4096", u.ContextLength)
	}
	if !containsString(u.Tags, "paid") {
		t.Error("unknown pricing must be assumed paid")
	}
}

func TestReservedNames(t *testing.T) {
	reg, err := LoadBytes([]byte(discoveryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	got := strings.Join(reg.ReservedNames(), ",")
	for _, want := range []string{"zen/pinned", "pinned-alias", "coder"} {
		if !strings.Contains(got, want) {
			t.Errorf("ReservedNames() = %s, missing %q", got, want)
		}
	}
}

func TestDiscovery_CapabilitiesFloor(t *testing.T) {
	src := DiscoverySource{APIBase: "http://lms.example/v1", Prefix: "lms/", Adopt: AdoptPolicy{All: true},
		Capabilities: []ModelCapability{CapText, CapToolCalling}}
	m := src.Entry(ListedModel{ID: "qwen3-8b", Vision: true})
	want := []ModelCapability{CapText, CapVision, CapToolCalling}
	if len(m.Capabilities) != len(want) {
		t.Fatalf("capabilities = %v, want %v (listing plus floor, no duplicates)", m.Capabilities, want)
	}
	for _, c := range want {
		if !m.HasCapability(c) {
			t.Errorf("missing %q in %v", c, m.Capabilities)
		}
	}
}

func TestDiscovery_RoleMembers(t *testing.T) {
	base := `
nodes:
  n1: {host: n1.local, gpu: nvidia, vram_gb: 80}
models:
  m:
    hf_repo: m
    node: n1
    capabilities: [text]
discovery:
  - api_base: http://lms.example/v1
    prefix: lms/
    adopt: all
  - api_base: https://or.example/api/v1
    prefix: or/
    adopt: ["deepseek/*"]
    exclude: ["*:batch"]
  - api_base: https://or.example/api/v1/nested
    prefix: or/x/
    adopt: all
roles:
`
	ok := []string{
		"  r: {candidates: [lms/qwen3-8b, m]}\n",
		"  r: {candidates: [or/deepseek/deepseek-v4]}\n",
		// capabilities cannot be known at load for a discovered member
		"  r: {require: {capabilities: [tool_calling]}, candidates: [lms/qwen3-8b]}\n",
		// longest prefix wins: or/x/ adopts all, or/ would not adopt "x/any"
		"  r: {candidates: [or/x/anything]}\n",
		"  r: {candidates: [m], on_empty: overflow, overflow: [lms/big]}\n",
	}
	for _, block := range ok {
		if _, err := LoadBytes([]byte(base + block)); err != nil {
			t.Errorf("%s: %v", strings.TrimSpace(block), err)
		}
	}
	bad := []struct{ block, want string }{
		{"  r: {candidates: [lmz/qwen3-8b]}\n", "not a known model (nor under a discovery prefix)"},
		{"  r: {candidates: [or/openai/gpt-9]}\n", `discovery or/ would never adopt "openai/gpt-9"`},
		{"  r: {candidates: [or/deepseek/v4:batch]}\n", "would never adopt"},
		{"  r: {require: {locality: local}, candidates: [lms/qwen3-8b]}\n", "is not local"},
		{"  r: {candidates: [lms/]}\n", "not a known model"},
	}
	for _, tc := range bad {
		_, err := LoadBytes([]byte(base + tc.block))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", strings.TrimSpace(tc.block), err, tc.want)
		}
	}

	// A hand-written entry with the id wins: disabled means dropped, not
	// resurrected as a discovered member.
	pinned := strings.Replace(base, "discovery:", `  lms/pinned:
    hf_repo: pinned
    backend: external
    api_base: http://lms.example/v1
    enabled: false
discovery:`, 1)
	reg, err := LoadBytes([]byte(pinned + "  r: {candidates: [lms/pinned, lms/other, m]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reg.RolesForMode("")["r"].Candidates, ","); got != "lms/other,m" {
		t.Errorf("candidates %q, want the disabled hand-written entry dropped", got)
	}
}
