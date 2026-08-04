package config

import (
	"strings"
	"testing"
)

// rolesYAML is a miniature of the real fleet shape: two local coding models on
// different nodes, a CPU last-resort, a cloud model for overflow, and a
// disabled rollback entry that must never be selected.
const rolesYAML = `
nodes:
  hypatia: {host: hypatia.local, gpu: nvidia, vram_gb: 128}
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
  hekaton: {host: 192.168.42.20, gpu: none, vram_gb: 0}

models:
  qwen3.6-hypatia:
    hf_repo: Qwen/Qwen3.6-35B-A3B
    node: hypatia
    capabilities: [text, vision, tool_calling]
    tags: [mode:default]
  minimax-m2.7-reap:
    hf_repo: MiniMax/M2.7-REAP
    node: archimedes
    capabilities: [text, tool_calling]
    tags: [mode:default]
  ling-flash-local:
    hf_repo: inclusionAI/Ling-flash-2.0
    node: hekaton
    capabilities: [text, tool_calling]
  big-only:
    hf_repo: Qwen/Qwen3.5-122B-A10B
    node: archimedes
    capabilities: [text, tool_calling]
    tags: [mode:big]
  rollback-model:
    hf_repo: nvidia/Nemotron-3-Super
    node: archimedes
    enabled: false
    capabilities: [text, tool_calling]
  kimi-k2.7-code:
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
    candidates: [qwen3.6-hypatia, minimax-m2.7-reap, big-only, rollback-model, ling-flash-local]
    on_empty: overflow
    overflow: [kimi-k2.7-code]

  thinker:
    require: {locality: local}
    candidates: [minimax-m2.7-reap, qwen3.6-hypatia]
`

func loadRoles(t *testing.T) *ModelRegistry {
	t.Helper()
	r, err := LoadBytes([]byte(rolesYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return r
}

func TestRoleDefaults(t *testing.T) {
	r := loadRoles(t)

	thinker, ok := r.Roles["thinker"]
	if !ok {
		t.Fatalf("thinker role missing")
	}
	// An unannotated role must be the strict one: never substitutes.
	if thinker.OnEmpty != OnEmptyError {
		t.Errorf("thinker.on_empty = %q, want %q (default)", thinker.OnEmpty, OnEmptyError)
	}

	coder := r.Roles["coder"]
	if coder.Require.Locality != LocalityLocal {
		t.Errorf("coder.require.locality = %q, want local", coder.Require.Locality)
	}
	if coder.OnEmpty != OnEmptyOverflow {
		t.Errorf("coder.on_empty = %q, want overflow", coder.OnEmpty)
	}
}

func TestRoleLocalityDefaultsToAny(t *testing.T) {
	r, err := LoadBytes([]byte(`
nodes:
  hypatia: {host: hypatia.local, gpu: nvidia, vram_gb: 128}
models:
  local-one: {hf_repo: a/b, node: hypatia}
  cloud-one: {hf_repo: c/d, backend: external, api_base: https://example.invalid/v1}
roles:
  anything:
    candidates: [local-one, cloud-one]
`))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	// No locality stated means no locality constraint — a cloud candidate is
	// legal, which is exactly why "local" has to be written down explicitly.
	if got := r.Roles["anything"].Require.Locality; got != LocalityAny {
		t.Errorf("require.locality = %q, want %q", got, LocalityAny)
	}
}

func TestRolesForMode_FiltersCandidates(t *testing.T) {
	r := loadRoles(t)

	def := r.RolesForMode("default")["coder"]
	want := []string{"qwen3.6-hypatia", "minimax-m2.7-reap", "ling-flash-local"}
	if got := def.Candidates; !equalStrings(got, want) {
		t.Errorf("default-mode candidates = %v, want %v (big-only is mode:big, rollback-model is disabled)", got, want)
	}

	big := r.RolesForMode("big")["coder"]
	wantBig := []string{"big-only", "ling-flash-local"}
	if got := big.Candidates; !equalStrings(got, wantBig) {
		t.Errorf("big-mode candidates = %v, want %v", got, wantBig)
	}

	// Preference order must survive filtering — it is the whole contract.
	all := r.RolesForMode("")["coder"]
	wantAll := []string{"qwen3.6-hypatia", "minimax-m2.7-reap", "big-only", "ling-flash-local"}
	if got := all.Candidates; !equalStrings(got, wantAll) {
		t.Errorf("unfiltered candidates = %v, want %v", got, wantAll)
	}
}

func TestRolesForMode_DropsFullyUnavailableRole(t *testing.T) {
	r, err := LoadBytes([]byte(`
nodes:
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
models:
  big-only:
    hf_repo: a/b
    node: archimedes
    tags: [mode:big]
roles:
  bigthink:
    candidates: [big-only]
`))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if _, ok := r.RolesForMode("default")["bigthink"]; ok {
		t.Errorf("bigthink should be dropped in default mode: its only candidate is mode:big")
	}
	if _, ok := r.RolesForMode("big")["bigthink"]; !ok {
		t.Errorf("bigthink should survive in big mode")
	}
}

func TestValidateRole_Rejections(t *testing.T) {
	const head = `
nodes:
  hypatia: {host: hypatia.local, gpu: nvidia, vram_gb: 128}
models:
  local-coder:
    hf_repo: a/b
    node: hypatia
    capabilities: [text, tool_calling]
    aliases: [taken-alias]
  text-only:
    hf_repo: c/d
    node: hypatia
    capabilities: [text]
  cloud:
    hf_repo: e/f
    backend: external
    api_base: https://example.invalid/v1
    capabilities: [text, tool_calling]
  embedder:
    hf_repo: g/h
    node: hypatia
    api_class: embeddings
roles:
`
	cases := []struct {
		name string
		role string
		want string
	}{
		{
			name: "cloud candidate under locality local",
			role: "  coder:\n    require: {locality: local}\n    candidates: [cloud]\n",
			want: `is not local`,
		},
		{
			name: "candidate missing required capability",
			role: "  coder:\n    require: {capabilities: [tool_calling]}\n    candidates: [text-only]\n",
			want: `lacks required capability "tool_calling"`,
		},
		{
			name: "candidate with wrong api_class",
			role: "  coder:\n    require: {api_class: chat}\n    candidates: [embedder]\n",
			want: `has api_class "embeddings"`,
		},
		{
			name: "unknown candidate",
			role: "  coder:\n    candidates: [nope]\n",
			want: `is not a known model`,
		},
		{
			name: "role name collides with model id",
			role: "  local-coder:\n    candidates: [local-coder]\n",
			want: `collides with a model id`,
		},
		{
			name: "role name collides with an alias",
			role: "  taken-alias:\n    candidates: [local-coder]\n",
			want: `collides with an alias on model "local-coder"`,
		},
		{
			name: "overflow declared without on_empty overflow",
			role: "  coder:\n    candidates: [local-coder]\n    overflow: [cloud]\n",
			want: `overflow list set but on_empty is "error"`,
		},
		{
			name: "on_empty overflow with empty list",
			role: "  coder:\n    candidates: [local-coder]\n    on_empty: overflow\n",
			want: `overflow list is empty`,
		},
		{
			name: "unknown on_empty",
			role: "  coder:\n    candidates: [local-coder]\n    on_empty: shrug\n",
			want: `unknown on_empty "shrug"`,
		},
		{
			name: "unknown locality",
			role: "  coder:\n    require: {locality: nearby}\n    candidates: [local-coder]\n",
			want: `unknown require.locality "nearby"`,
		},
		{
			name: "no candidates",
			role: "  coder:\n    description: empty\n    candidates: []\n",
			want: `needs at least one candidate`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(head + tc.role))
			if err == nil {
				t.Fatalf("expected a validation error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateRole_OverflowExemptFromLocality(t *testing.T) {
	// The whole point of overflow: it may cross the locality boundary the
	// candidates are held to. It must still honour the rest of the contract.
	_, err := LoadBytes([]byte(`
nodes:
  hypatia: {host: hypatia.local, gpu: nvidia, vram_gb: 128}
models:
  local-coder:
    hf_repo: a/b
    node: hypatia
    capabilities: [text, tool_calling]
  cloud:
    hf_repo: c/d
    backend: external
    api_base: https://example.invalid/v1
    capabilities: [text, tool_calling]
  cloud-text-only:
    hf_repo: e/f
    backend: external
    api_base: https://example.invalid/v1
    capabilities: [text]
roles:
  coder:
    require: {locality: local, capabilities: [tool_calling]}
    candidates: [local-coder]
    on_empty: overflow
    overflow: [cloud]
`))
	if err != nil {
		t.Fatalf("overflow entry should be exempt from locality: %v", err)
	}

	_, err = LoadBytes([]byte(`
nodes:
  hypatia: {host: hypatia.local, gpu: nvidia, vram_gb: 128}
models:
  local-coder:
    hf_repo: a/b
    node: hypatia
    capabilities: [text, tool_calling]
  cloud-text-only:
    hf_repo: e/f
    backend: external
    api_base: https://example.invalid/v1
    capabilities: [text]
roles:
  coder:
    require: {locality: local, capabilities: [tool_calling]}
    candidates: [local-coder]
    on_empty: overflow
    overflow: [cloud-text-only]
`))
	if err == nil || !strings.Contains(err.Error(), `lacks required capability`) {
		t.Errorf("overflow must still satisfy capabilities; err = %v", err)
	}
}

func TestIsLocal(t *testing.T) {
	cases := []struct {
		name string
		m    ModelDefinition
		want bool
	}{
		{"node-pinned vllm", ModelDefinition{Node: "hypatia"}, true},
		// Node-pinned externals (flux, orpheus, the OpenArc embedder) run on
		// our hardware and die with it — they are local for role purposes.
		{"node-pinned external", ModelDefinition{Node: "delphi", Backend: BackendExternal}, true},
		{"multi-node", ModelDefinition{MultiNode: &MultiNodeConfig{Nodes: []string{"a"}}}, true},
		{"nodeless external", ModelDefinition{Backend: BackendExternal, APIBase: "https://x/v1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.IsLocal(); got != tc.want {
				t.Errorf("IsLocal() = %v, want %v", got, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
