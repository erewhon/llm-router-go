package toolproxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

const requestDefaultsYAML = `
nodes:
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
models:
  glm-spark:
    hf_repo: glm-5.3-flash
    backend: vllm
    node: archimedes
    api_port: 5391
    tool_proxy: true
    aliases: [glm-think]
    request_defaults:
      top_p: 0.95
      chat_template_kwargs: {enable_thinking: false}
    alias_overrides:
      glm-think:
        request_defaults:
          chat_template_kwargs: {enable_thinking: true}
`

func TestChat_AppliesRequestDefaultsForDirectCallers(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()

	reg, err := config.LoadBytes([]byte(requestDefaultsYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	p := New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithFlushInterval(0), WithTransport(&transportRedirect{to: upstream.URL, rt: http.DefaultTransport}))

	post := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		p.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}

	// By registry key (what the router sends): model defaults only.
	post(`{"model":"glm-spark","messages":[{"role":"user","content":"hi"}]}`)
	if got["top_p"] != 0.95 || got["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
		t.Errorf("model defaults not applied: %#v", got)
	}
	if got["model"] != "glm-5.3-flash" {
		t.Errorf("model rewrite lost: %v", got["model"])
	}

	// By alias: the override wins, the model's other defaults still fill.
	post(`{"model":"glm-think","messages":[{"role":"user","content":"hi"}]}`)
	if got["chat_template_kwargs"].(map[string]any)["enable_thinking"] != true || got["top_p"] != 0.95 {
		t.Errorf("alias override not applied: %#v", got)
	}

	// The caller's own value survives.
	post(`{"model":"glm-spark","top_p":0.5,"messages":[]}`)
	if got["top_p"] != 0.5 {
		t.Errorf("caller's top_p overwritten: %v", got["top_p"])
	}
}
