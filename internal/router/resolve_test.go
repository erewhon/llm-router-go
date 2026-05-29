package router

import (
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

func TestResolveModel(t *testing.T) {
	rt := newTestRouter(t, nil)

	tests := []struct {
		name         string
		model        string
		backendURL   string
		backendModel string
		auth         string
		viaToolProxy bool
		apiClass     config.APIClass
	}{
		{
			name:         "local tool_proxy by id sends model_id to tool proxy",
			model:        "nemotron-3-super",
			backendURL:   "http://192.168.42.240:5392",
			backendModel: "nemotron-3-super",
			viaToolProxy: true,
			apiClass:     config.APIClassChat,
		},
		{
			name:         "local tool_proxy by alias",
			model:        "research",
			backendURL:   "http://192.168.42.240:5392",
			backendModel: "nemotron-3-super",
			viaToolProxy: true,
			apiClass:     config.APIClassChat,
		},
		{
			name:         "local direct sends bare hf_repo to node backend",
			model:        "qwen36-hypatia",
			backendURL:   "http://hypatia.local:5391",
			backendModel: "Qwen/Qwen3.6-35B-A3B-FP8",
			viaToolProxy: false,
			apiClass:     config.APIClassChat,
		},
		{
			name:         "local direct by alias",
			model:        "coder",
			backendURL:   "http://hypatia.local:5391",
			backendModel: "Qwen/Qwen3.6-35B-A3B-FP8",
			viaToolProxy: false,
			apiClass:     config.APIClassChat,
		},
		{
			name:         "external resolves env-var api_key",
			model:        "zen-glm",
			backendURL:   "https://api.zen.example",
			backendModel: "zen/glm-4.6",
			auth:         "secret-key",
			viaToolProxy: false,
			apiClass:     config.APIClassChat,
		},
		{
			name:         "external literal sk- key passes through",
			model:        "zen-lit",
			backendURL:   "https://api.lit.example",
			backendModel: "zen/lit",
			auth:         "sk-literal-123",
			viaToolProxy: false,
			apiClass:     config.APIClassChat,
		},
		{
			name:         "external auto stub forwards hf_repo, not model_id",
			model:        "auto",
			backendURL:   "http://192.168.42.240:5392",
			backendModel: "auto",
			viaToolProxy: false,
			apiClass:     config.APIClassChat,
		},
		{
			name:         "openai/ prefix is stripped",
			model:        "openai/coder",
			backendURL:   "http://hypatia.local:5391",
			backendModel: "Qwen/Qwen3.6-35B-A3B-FP8",
			viaToolProxy: false,
			apiClass:     config.APIClassChat,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := rt.resolveModel(tc.model, false)
			if err != nil {
				t.Fatalf("resolveModel(%q): %v", tc.model, err)
			}
			if res.BackendURL != tc.backendURL {
				t.Errorf("BackendURL = %q, want %q", res.BackendURL, tc.backendURL)
			}
			if res.BackendModel != tc.backendModel {
				t.Errorf("BackendModel = %q, want %q", res.BackendModel, tc.backendModel)
			}
			if res.AuthBearer != tc.auth {
				t.Errorf("AuthBearer = %q, want %q", res.AuthBearer, tc.auth)
			}
			if res.ViaToolProxy != tc.viaToolProxy {
				t.Errorf("ViaToolProxy = %v, want %v", res.ViaToolProxy, tc.viaToolProxy)
			}
			if res.APIClass != tc.apiClass {
				t.Errorf("APIClass = %q, want %q", res.APIClass, tc.apiClass)
			}
		})
	}
}

func TestResolveModel_Errors(t *testing.T) {
	rt := newTestRouter(t, nil)
	for _, model := range []string{"", "nonexistent", "ghost-disabled"} {
		if _, err := rt.resolveModel(model, false); err == nil {
			t.Errorf("resolveModel(%q) = nil error, want error", model)
		}
	}
}

func TestResolveModel_ModeExcludesOtherMode(t *testing.T) {
	rt := newTestRouter(t, nil, WithMode("default"))
	if _, err := rt.resolveModel("big-only", false); err == nil {
		t.Errorf("mode=default: resolveModel(big-only) should fail (mode:big excluded)")
	}
	// A normal untagged model still resolves under mode=default.
	if _, err := rt.resolveModel("coder", false); err != nil {
		t.Errorf("mode=default: resolveModel(coder) failed: %v", err)
	}
}

// forceDirect must bypass tool-proxy routing even for a tool_proxy:true model
// — this is what the /v1/completions, /v1/embeddings, /v1/rerank handlers
// pass, since the tool proxy doesn't serve those paths.
func TestResolveModel_ForceDirectBypassesToolProxy(t *testing.T) {
	rt := newTestRouter(t, nil)

	// chat-style routing: research -> nemotron via the tool proxy with model_id.
	chat, err := rt.resolveModel("research", false)
	if err != nil {
		t.Fatalf("chat resolve: %v", err)
	}
	if !chat.ViaToolProxy || chat.BackendURL != "http://192.168.42.240:5392" {
		t.Errorf("chat path should go via tool proxy, got url=%q via=%v",
			chat.BackendURL, chat.ViaToolProxy)
	}
	if chat.BackendModel != "nemotron-3-super" {
		t.Errorf("chat path should send model_id; got %q", chat.BackendModel)
	}

	// forceDirect=true: same model, but straight to the node backend with the
	// bare hf_repo — exactly what /v1/completions should produce.
	direct, err := rt.resolveModel("research", true)
	if err != nil {
		t.Fatalf("forceDirect resolve: %v", err)
	}
	if direct.ViaToolProxy {
		t.Errorf("forceDirect should bypass tool proxy, but ViaToolProxy=true")
	}
	if direct.BackendURL != "http://archimedes.local:5391" {
		t.Errorf("forceDirect BackendURL = %q, want http://archimedes.local:5391", direct.BackendURL)
	}
	if direct.BackendModel != "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4" {
		t.Errorf("forceDirect BackendModel = %q, want hf_repo", direct.BackendModel)
	}
}

// /v1/embeddings + /v1/rerank routing depends on api_class being surfaced.
func TestResolveModel_APIClassSurfaced(t *testing.T) {
	rt := newTestRouter(t, nil)
	for _, tc := range []struct {
		model string
		want  config.APIClass
	}{
		{"qwen3-embedding", config.APIClassEmbeddings},
		{"embedding", config.APIClassEmbeddings}, // alias
		{"qwen3-reranker", config.APIClassRerank},
		{"coder", config.APIClassChat}, // default
	} {
		res, err := rt.resolveModel(tc.model, true)
		if err != nil {
			t.Fatalf("resolveModel(%q): %v", tc.model, err)
		}
		if res.APIClass != tc.want {
			t.Errorf("%s api_class = %q, want %q", tc.model, res.APIClass, tc.want)
		}
	}
}
