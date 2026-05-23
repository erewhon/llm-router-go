package nodeagent

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

const testYAML = `
nodes:
  euclid:
    host: euclid.local
    gpu: intel
    vram_gb: 16
  archimedes:
    host: archimedes.local
    gpu: nvidia
    vram_gb: 128
    services:
      comfyui:
        type: comfyui
        port: 8188
        label: ComfyUI

models:
  qwen-local:
    hf_repo: Qwen/Qwen3.6-27B-FP8
    backend: vllm
    node: archimedes
    vram_gb: 27
    always_on: true
    aliases: [thinker]

  qwen-disabled:
    hf_repo: Qwen/Qwen3.5-122B-A10B-FP8
    backend: vllm
    node: archimedes
    enabled: false

  claude-zen:
    hf_repo: claude-opus-4-6
    backend: external
    api_base: https://opencode.ai/zen/v1
    api_key: OPENCODE_ZEN_KEY

  # External but assigned to a node (e.g. OpenArc local embeddings).
  # ModelsForNode returns this since m.Node == archimedes.
  openarc-local:
    hf_repo: qwen3-embedding-4b
    backend: external
    node: archimedes
    api_base: http://archimedes.local:5404/v1
`

func newTestAgent(t *testing.T, node string) (*Agent, http.Handler) {
	t.Helper()
	reg, err := config.LoadBytes([]byte(testYAML))
	if err != nil {
		t.Fatalf("config.LoadBytes: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	agent, err := New(reg, node, logger, "test-1.2.3")
	if err != nil {
		t.Fatalf("nodeagent.New: %v", err)
	}
	return agent, agent.Handler()
}

func TestNew_RejectsUnknownNode(t *testing.T) {
	reg, err := config.LoadBytes([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(reg, "phantom", logger, "x"); err == nil {
		t.Fatal("expected error for unknown node")
	}
}

func TestHealth(t *testing.T) {
	_, h := newTestAgent(t, "archimedes")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if resp.Status != "ok" || resp.Node != "archimedes" {
		t.Errorf("status/node = %q/%q", resp.Status, resp.Node)
	}

	// claude-zen is external but has no node — excluded from this view.
	// openarc-local is external AND on archimedes — included as running.
	wantRunning := map[string]bool{"openarc-local": true}
	gotRunning := map[string]bool{}
	for _, m := range resp.RunningModels {
		gotRunning[m] = true
	}
	for id := range wantRunning {
		if !gotRunning[id] {
			t.Errorf("running_models missing %q (got %v)", id, resp.RunningModels)
		}
	}
	if gotRunning["qwen-local"] {
		t.Errorf("running_models should not include qwen-local in phase 1a (no probing)")
	}
	if gotRunning["qwen-disabled"] {
		t.Errorf("running_models leaked a disabled model")
	}

	// archimedes has the comfyui service registered.
	if len(resp.Services) != 1 || resp.Services[0].Name != "comfyui" {
		t.Errorf("services = %v, want one comfyui entry", resp.Services)
	}

	if resp.DiskFreeGB == nil || resp.DiskTotalGB == nil {
		t.Errorf("disk stats not populated")
	}
}

func TestModelList(t *testing.T) {
	_, h := newTestAgent(t, "archimedes")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var list []ModelListEntry
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}

	byID := map[string]ModelListEntry{}
	for _, e := range list {
		byID[e.ModelID] = e
	}
	if _, ok := byID["qwen-disabled"]; ok {
		t.Errorf("qwen-disabled leaked into /models (should be enabled-only)")
	}

	q := byID["qwen-local"]
	if q.State != StateStopped {
		t.Errorf("qwen-local state = %q, want stopped in Phase 1a", q.State)
	}
	if q.HFRepo != "Qwen/Qwen3.6-27B-FP8" || !q.AlwaysOn || q.VRAMGB != 27 {
		t.Errorf("qwen-local entry mis-populated: %+v", q)
	}

	if oa, ok := byID["openarc-local"]; !ok || oa.State != StateRunning {
		t.Errorf("openarc-local should be present and RUNNING (external): %+v", oa)
	}
}

func TestModelStatus_OK(t *testing.T) {
	_, h := newTestAgent(t, "archimedes")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/models/qwen-local/status", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var resp ModelStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ModelID != "qwen-local" || resp.State != StateStopped || resp.Backend != "vllm" {
		t.Errorf("status response wrong: %+v", resp)
	}
}

func TestModelStatus_NotFound(t *testing.T) {
	_, h := newTestAgent(t, "archimedes")
	cases := []string{
		"/models/ghost-model/status",         // not in registry at all
		"/models/qwen-disabled/status",       // disabled (filtered)
		"/models/openarc-local/status",       // not on euclid scope... wait, on archimedes
	}
	// re-test the disabled path explicitly on archimedes
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, cases[1], nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("disabled model: got %d want 404", rec.Code)
	}

	// ghost model
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, cases[0], nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("ghost model: got %d want 404", rec.Code)
	}
}

func TestModelStatus_WrongNode(t *testing.T) {
	// qwen-local is on archimedes; from euclid's agent it shouldn't exist.
	_, h := newTestAgent(t, "euclid")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/models/qwen-local/status", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d want 404 (qwen-local is on archimedes, not euclid)", rec.Code)
	}
}

func TestMetrics(t *testing.T) {
	_, h := newTestAgent(t, "archimedes")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain := []string{
		"node_agent_build_info",
		`version="test-1.2.3"`,
		`node="archimedes"`,
		"node_agent_uptime_seconds",
		"node_agent_models_enabled",
		"go_goroutines", // from collectors.NewGoCollector
		"process_",      // from collectors.NewProcessCollector
	}
	for _, want := range mustContain {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}
