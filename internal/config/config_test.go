package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleYAML = `
nodes:
  delphi:
    host: delphi.local
    gpu: amd
    vram_gb: 64
  archimedes:
    host: archimedes.local
    gpu: nvidia
    vram_gb: 128
    agent_port: 8100
    services:
      comfyui:
        type: comfyui
        port: 8188
        label: ComfyUI
  hypatia:
    host: hypatia.local
    gpu: nvidia
    vram_gb: 128
  euclid:
    host: euclid.local
    gpu: intel
    vram_gb: 16

models:
  # defaults: backend=vllm, enabled=true, capabilities=[text]
  qwen-defaults:
    hf_repo: Qwen/Qwen3.6-27B-FP8
    node: archimedes

  # explicit disable + tool_proxy
  qwen-disabled:
    hf_repo: Qwen/Qwen3.5-122B-A10B-FP8
    backend: vllm
    multi_node:
      nodes: [archimedes, hypatia]
      tensor_parallel_size: 2
    enabled: false
    tool_proxy: true
    capabilities: [text, tool_calling]
    tags:
      - mode:big

  # external (no node, has api_base)
  cloud-claude:
    hf_repo: claude-opus-4-6
    backend: external
    api_base: https://opencode.ai/zen/v1
    api_key: OPENCODE_ZEN_API_KEY
    input_cost_per_million: 5.00
    output_cost_per_million: 25.00

  # custom port, llamacpp
  ui-tars:
    hf_repo: bytedance/UI-TARS-1.5-7B
    backend: llamacpp
    node: delphi
    api_port: 5400
    capabilities: [text, vision]
`

func loadSample(t *testing.T) *ModelRegistry {
	t.Helper()
	r, err := LoadBytes([]byte(sampleYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return r
}

func TestDefaultsApplied(t *testing.T) {
	r := loadSample(t)

	m := r.Models["qwen-defaults"]
	if m.Backend != BackendVLLM {
		t.Errorf("default backend = %q, want vllm", m.Backend)
	}
	if !m.Enabled {
		t.Errorf("default enabled = false, want true")
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != CapText {
		t.Errorf("default capabilities = %v, want [text]", m.Capabilities)
	}

	// NodeDefinition default for agent_port:
	if got := r.Nodes["delphi"].AgentPort; got != 8100 {
		t.Errorf("default agent_port = %d, want 8100", got)
	}
}

func TestExplicitDisableNotOverwritten(t *testing.T) {
	r := loadSample(t)
	if r.Models["qwen-disabled"].Enabled {
		t.Fatalf("qwen-disabled: enabled=true after parsing, expected false")
	}
}

func TestValidation_ExternalRequiresAPIBase(t *testing.T) {
	yaml := `
nodes: {}
models:
  bad-external:
    hf_repo: foo
    backend: external
`
	_, err := LoadBytes([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "external backend requires api_base") {
		t.Fatalf("expected external/api_base error, got %v", err)
	}
}

func TestValidation_NodeOrMultiNodeRequired(t *testing.T) {
	yaml := `
nodes:
  euclid: {host: euclid.local, gpu: intel, vram_gb: 16}
models:
  orphan:
    hf_repo: foo
`
	_, err := LoadBytes([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "must specify 'node' or 'multi_node'") {
		t.Fatalf("expected node/multi_node error, got %v", err)
	}
}

func TestValidation_NodeReferenceMustExist(t *testing.T) {
	yaml := `
nodes:
  euclid: {host: euclid.local, gpu: intel, vram_gb: 16}
models:
  ghost:
    hf_repo: foo
    node: phantom
`
	_, err := LoadBytes([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), `unknown node "phantom"`) {
		t.Fatalf("expected unknown node error, got %v", err)
	}
}

func TestAPIBase(t *testing.T) {
	r := loadSample(t)

	cases := []struct {
		name     string
		modelID  string
		override *bool
		want     string
	}{
		{"single node default port", "qwen-defaults", nil, "http://archimedes.local:5391/v1"},
		{"external returns api_base", "cloud-claude", nil, "https://opencode.ai/zen/v1"},
		{"llamacpp custom port", "ui-tars", nil, "http://delphi.local:5400/v1"},
		{"multi-node head=first", "qwen-disabled", boolPtr(false), "http://archimedes.local:5391/v1"},
		{"tool_proxy redirects", "qwen-disabled", nil, ToolProxyAddr},
		{"override disables tool_proxy", "qwen-disabled", boolPtr(false), "http://archimedes.local:5391/v1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := r.APIBase(c.modelID, c.override)
			if err != nil {
				t.Fatalf("APIBase(%q): %v", c.modelID, err)
			}
			if got != c.want {
				t.Errorf("APIBase(%q) = %q, want %q", c.modelID, got, c.want)
			}
		})
	}
}

func TestModeTag(t *testing.T) {
	r := loadSample(t)
	if got := r.Models["qwen-disabled"].ModeTag(); got != "big" {
		t.Errorf("ModeTag() = %q, want big", got)
	}
	if got := r.Models["qwen-defaults"].ModeTag(); got != "" {
		t.Errorf("ModeTag() with no mode tag = %q, want empty", got)
	}
}

func TestModelsForMode(t *testing.T) {
	r := loadSample(t)

	// mode="" → all enabled (3 of the 4 models; qwen-disabled is enabled=false)
	all := r.ModelsForMode("")
	if len(all) != 3 {
		t.Errorf("ModelsForMode(\"\"): got %d, want 3 (enabled only)", len(all))
	}
	if _, ok := all["qwen-disabled"]; ok {
		t.Errorf("ModelsForMode(\"\"): disabled model leaked into result")
	}

	// mode="big" → only enabled models with mode:big or no mode tag
	// In our sample, the only model with mode:big is qwen-disabled (excluded).
	// All other models have no mode tag, so they all pass through.
	big := r.ModelsForMode("big")
	if len(big) != 3 {
		t.Errorf("ModelsForMode(\"big\"): got %d, want 3 (untagged + matching mode)", len(big))
	}
}

func TestModelsForNode(t *testing.T) {
	r := loadSample(t)

	delphi := r.ModelsForNode("delphi", true)
	if _, ok := delphi["ui-tars"]; !ok {
		t.Errorf("ModelsForNode(delphi): ui-tars missing")
	}

	// archimedes has qwen-defaults (single-node) and qwen-disabled (multi-node, but disabled)
	arch := r.ModelsForNode("archimedes", true)
	if _, ok := arch["qwen-defaults"]; !ok {
		t.Errorf("ModelsForNode(archimedes): qwen-defaults missing")
	}
	if _, ok := arch["qwen-disabled"]; ok {
		t.Errorf("ModelsForNode(archimedes): disabled model leaked (enabledOnly=true)")
	}

	// Same call with enabledOnly=false should surface qwen-disabled
	archAll := r.ModelsForNode("archimedes", false)
	if _, ok := archAll["qwen-disabled"]; !ok {
		t.Errorf("ModelsForNode(archimedes, enabledOnly=false): qwen-disabled missing")
	}
}

func TestServiceDefinition(t *testing.T) {
	r := loadSample(t)
	svc, ok := r.Nodes["archimedes"].Services["comfyui"]
	if !ok {
		t.Fatalf("archimedes.services.comfyui missing")
	}
	if svc.Type != ServiceComfyUI || svc.Port != 8188 || svc.Label != "ComfyUI" {
		t.Errorf("ComfyUI service mis-parsed: %+v", svc)
	}
}

func boolPtr(b bool) *bool { return &b }

// ---------------------------------------------------------------------------
// Real models.yaml load (skipped if the Python repo isn't available)
// ---------------------------------------------------------------------------

func productionYAMLPath() string {
	if p := os.Getenv("LLM_ROUTER_MODELS_YAML"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Projects", "erewhon", "llm-router", "models.yaml")
}

func TestLoadProductionYAML(t *testing.T) {
	path := productionYAMLPath()
	if path == "" {
		t.Skip("no LLM_ROUTER_MODELS_YAML and no home directory")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("production models.yaml not available at %s", path)
	}

	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}

	// Sanity checks against the live registry. These don't pin every value
	// (the file changes often) but they catch schema regressions.
	wantNodes := []string{"euclid", "archimedes", "hypatia", "delphi"}
	for _, n := range wantNodes {
		if _, ok := r.Nodes[n]; !ok {
			t.Errorf("expected node %q in registry", n)
		}
	}
	if len(r.Models) == 0 {
		t.Fatalf("expected at least one model in registry")
	}

	// Spot-check known fields. If these flip we want a deliberate change.
	if arch, ok := r.Nodes["archimedes"]; ok {
		if arch.GPU != GpuNvidia {
			t.Errorf("archimedes.gpu = %q, want nvidia", arch.GPU)
		}
		if arch.AgentPort != 8100 {
			t.Errorf("archimedes.agent_port = %d, want 8100", arch.AgentPort)
		}
	}

	// At least one external-backend model must be present (the Zen catalogue).
	var sawExternal bool
	for _, m := range r.Models {
		if m.Backend == BackendExternal && m.APIBase != "" {
			sawExternal = true
			break
		}
	}
	if !sawExternal {
		t.Errorf("expected at least one external-backend model in registry")
	}
}
