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
			res, err := rt.resolveModel(tc.model)
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
		if _, err := rt.resolveModel(model); err == nil {
			t.Errorf("resolveModel(%q) = nil error, want error", model)
		}
	}
}

func TestResolveModel_ModeExcludesOtherMode(t *testing.T) {
	rt := newTestRouter(t, nil, WithMode("default"))
	if _, err := rt.resolveModel("big-only"); err == nil {
		t.Errorf("mode=default: resolveModel(big-only) should fail (mode:big excluded)")
	}
	// A normal untagged model still resolves under mode=default.
	if _, err := rt.resolveModel("coder"); err != nil {
		t.Errorf("mode=default: resolveModel(coder) failed: %v", err)
	}
}
