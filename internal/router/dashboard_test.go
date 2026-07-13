package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// stubNodes makes rt report the given per-node-agent model states without any
// network I/O, so the dashboard handlers are hermetic. states maps host ->
// (model_id -> state); a host absent from the map is reported unreachable.
func stubNodes(rt *Router, states map[string]map[string]string) {
	rt.nodeFetcher = func(_ context.Context, host string, _ int) nodeMetric {
		byModel, ok := states[host]
		if !ok {
			return unreachableNode()
		}
		nm := nodeMetric{Reachable: true, Models: []nodeModelMetric{}}
		for id, st := range byModel {
			nm.Models = append(nm.Models, nodeModelMetric{ModelID: id, State: st, TotalRequests: 3})
		}
		return nm
	}
}

func getDashJSON(t *testing.T, rt *Router, path string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rt.DashboardHandler(DashboardConfig{APIBase: "http://localhost:4010"}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d, body %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: decode: %v", path, err)
	}
	return out
}

func TestDashboard_ModelsShape(t *testing.T) {
	rt := newTestRouter(t, nil)
	// archimedes.local reports nemotron-3-super running; hypatia unreachable.
	stubNodes(rt, map[string]map[string]string{
		"archimedes.local": {"nemotron-3-super": "running"},
	})

	out := getDashJSON(t, rt, "/api/models")

	if got := int(out["model_count"].(float64)); got == 0 {
		t.Fatalf("model_count = 0, want the full registry")
	}
	// The Connection card's litellm_url echoes the configured API base.
	if out["litellm_url"] != "http://localhost:4010" {
		t.Errorf("litellm_url = %v", out["litellm_url"])
	}

	// nodes carry unified_memory (added for the Fleet CPU-RAM card).
	nodes := out["nodes"].(map[string]any)
	arch := nodes["archimedes"].(map[string]any)
	if _, ok := arch["unified_memory"]; !ok {
		t.Errorf("node archimedes missing unified_memory: %v", arch)
	}

	byID := map[string]map[string]any{}
	for _, m := range out["models"].([]any) {
		mm := m.(map[string]any)
		byID[mm["id"].(string)] = mm
	}

	// Node-managed model with a live agent: health "unknown", agent_state
	// carries the state, head_node = its node.
	nem := byID["nemotron-3-super"]
	if nem["health"] != "unknown" || nem["agent_state"] != "running" {
		t.Errorf("nemotron health/agent_state = %v/%v, want unknown/running", nem["health"], nem["agent_state"])
	}
	if nem["head_node"] != "archimedes" {
		t.Errorf("nemotron head_node = %v, want archimedes", nem["head_node"])
	}

	// Disabled model: health "disabled", agent_state null regardless of nodes.
	ghost := byID["ghost-disabled"]
	if ghost["health"] != "disabled" || ghost["agent_state"] != nil {
		t.Errorf("ghost-disabled health/agent_state = %v/%v, want disabled/nil", ghost["health"], ghost["agent_state"])
	}

	// External model with no node: health "routed", empty nodes, null head_node.
	auto := byID["auto"]
	if auto["health"] != "routed" || auto["head_node"] != nil {
		t.Errorf("auto health/head_node = %v/%v, want routed/nil", auto["health"], auto["head_node"])
	}
	if n := auto["nodes"].([]any); len(n) != 0 {
		t.Errorf("auto nodes = %v, want empty", n)
	}
}

func TestDashboard_RouterMetrics(t *testing.T) {
	rt := newTestRouter(t, nil)
	pt, ct := 10, 20
	rt.metrics.Observe(reqlog.Record{Path: "/v1/chat/completions", Status: 200, ResolvedVia: "nemotron-3-super", APIClass: "chat", LatencyMS: 100, PromptTokens: &pt, CompletionTokens: &ct})
	rt.metrics.Observe(reqlog.Record{Path: "/v1/chat/completions", Status: 200, ResolvedVia: "nemotron-3-super", APIClass: "chat", LatencyMS: 300})
	rt.metrics.Observe(reqlog.Record{Path: "/v1/chat/completions", Status: 404, ResolvedVia: "", APIClass: "", LatencyMS: 5})

	out := getDashJSON(t, rt, "/api/router-metrics")

	if out["reachable"] != true {
		t.Errorf("reachable = %v", out["reachable"])
	}
	if got := int(out["total_requests"].(float64)); got != 3 {
		t.Errorf("total_requests = %d, want 3", got)
	}
	if got := int(out["errors"].(float64)); got != 1 {
		t.Errorf("errors = %d, want 1 (the 404)", got)
	}
	if got := int(out["tokens_prompt"].(float64)); got != 10 {
		t.Errorf("tokens_prompt = %d, want 10", got)
	}
	if got := int(out["tokens_completion"].(float64)); got != 20 {
		t.Errorf("tokens_completion = %d, want 20", got)
	}
	if out["avg_latency_ms"] == nil {
		t.Errorf("avg_latency_ms is null, want a value")
	}
	// nemotron-3-super appears in top_models with 2 requests.
	top := out["top_models"].([]any)
	if len(top) == 0 {
		t.Fatalf("top_models empty")
	}
	first := top[0].([]any)
	if first[0] != "nemotron-3-super" || int(first[1].(float64)) != 2 {
		t.Errorf("top_models[0] = %v, want [nemotron-3-super 2]", first)
	}
}

// A pre-stream routing failure reaches the browser as an SSE error frame with
// the router's message and a 200 status, not a bare non-200 body.
func TestDashboard_ChatUnknownModelErrorFrame(t *testing.T) {
	rt := newTestRouter(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"nope","message":"hi"}`))
	rt.DashboardHandler(DashboardConfig{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error is inside the SSE stream)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("body is not an SSE frame: %q", body)
	}
	var frame struct {
		Error string `json:"error"`
	}
	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), "data:"))
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		t.Fatalf("decode frame %q: %v", payload, err)
	}
	if !strings.Contains(frame.Error, "unknown model") {
		t.Errorf("error = %q, want it to mention the unknown model", frame.Error)
	}
}

func TestDashboard_ChatMissingFields(t *testing.T) {
	rt := newTestRouter(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"","message":""}`))
	rt.DashboardHandler(DashboardConfig{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestDashboard_ServesHTMLWithSubstitutions(t *testing.T) {
	rt := newTestRouter(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rt.DashboardHandler(DashboardConfig{APIBase: "https://llm.example"}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "%%API_BASE%%") || strings.Contains(body, "%%API_KEY%%") {
		t.Errorf("template placeholders not substituted")
	}
	if !strings.Contains(body, "https://llm.example") {
		t.Errorf("API base not substituted into HTML")
	}
	// Empty APIKey falls back to the neutral placeholder, not a real key.
	if !strings.Contains(body, "&lt;api-key&gt;") && !strings.Contains(body, "<api-key>") {
		t.Errorf("expected the <api-key> placeholder in the served HTML")
	}
}
