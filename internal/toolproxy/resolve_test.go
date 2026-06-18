package toolproxy

import (
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

// An enabled model and a disabled one share the alias "research". resolveModel
// must skip the disabled entry, so the alias deterministically resolves to the
// enabled model — and the disabled model is not routable by its own id.
const resolveYAML = `
nodes:
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
models:
  nemotron-3-super:
    hf_repo: nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4
    backend: vllm
    node: archimedes
    api_port: 5391
    tool_proxy: true
    aliases: [thinker, research]
  qwen3.6-archimedes:
    hf_repo: Qwen/Qwen3.6-27B-FP8
    backend: vllm
    node: archimedes
    api_port: 5391
    enabled: false
    tool_proxy: true
    aliases: [research, coder-alt]
`

func TestResolveModelSkipsDisabled(t *testing.T) {
	r, err := config.LoadBytes([]byte(resolveYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	// Shared alias must land on the ENABLED model, regardless of map order.
	for i := 0; i < 20; i++ {
		got, err := resolveModel(r, "research")
		if err != nil {
			t.Fatalf("resolveModel(research): %v", err)
		}
		if got.ModelID != "nemotron-3-super" {
			t.Fatalf("research resolved to %q, want nemotron-3-super (enabled)", got.ModelID)
		}
		if got.BackendModel != "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4" {
			t.Errorf("backend_model = %q, want the Nemotron repo", got.BackendModel)
		}
	}

	// The disabled model is not routable even by its own id.
	if _, err := resolveModel(r, "qwen3.6-archimedes"); err == nil ||
		!strings.Contains(err.Error(), "unknown model") {
		t.Errorf("disabled id should be unknown, got err=%v", err)
	}

	// The enabled model still resolves by its id.
	if got, err := resolveModel(r, "nemotron-3-super"); err != nil || got.ModelID != "nemotron-3-super" {
		t.Errorf("enabled id resolve: got=%+v err=%v", got, err)
	}
}
