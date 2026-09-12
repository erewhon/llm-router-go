package router

import (
	"context"
	"encoding/json"
	"io/fs"
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
	rt.DashboardHandler(DashboardConfig{
		APIBase:    "https://llm.example",
		ProviderID: "llm",
		SetupHint:  SetupInstructions("llm", "https://dash.example", "LLM_ROUTER_API_KEY"),
	}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, ph := range []string{"%%API_BASE%%", "%%API_KEY%%", "%%PROVIDER_ID%%", "%%SETUP_HINT%%"} {
		if strings.Contains(body, ph) {
			t.Errorf("template placeholder %s not substituted", ph)
		}
	}
	// The shell carries the config for the tabs; the Connect tab renders the
	// PAT instructions from it at runtime.
	for _, want := range []string{`apiBase: "https://llm.example"`, `providerId: "llm"`, `apiKey: "pat_…"`, `type="module" src="/static/app.js"`} {
		if !strings.Contains(body, want) {
			t.Errorf("shell missing %q", want)
		}
	}
	if !strings.Contains(body, "dash.example") {
		t.Errorf("expected the setup hint (with its setup URL) in the served HTML")
	}
	if strings.Contains(body, "<api-key>") || strings.Contains(body, "sk-") {
		t.Errorf("served HTML must not carry a key or the old placeholder")
	}
}

// /v2 was the shell's address during the move; it now redirects home.
func TestDashboard_V2Redirects(t *testing.T) {
	rec := dashV2(t, "/v2")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/" {
		t.Errorf("GET /v2 = %d %q, want 301 to /", rec.Code, rec.Header().Get("Location"))
	}
}

// ---------------------------------------------------------------------------
// Dashboard shell: / + /static/
// ---------------------------------------------------------------------------

func dashV2(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rt := newTestRouter(t, nil)
	rec := httptest.NewRecorder()
	rt.DashboardHandler(DashboardConfig{
		APIBase:    "https://llm.example",
		ProviderID: "llm",
		SetupHint:  "line one\n\"quoted\" line two",
	}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestDashboardV2_ShellSubstitutedAndModular(t *testing.T) {
	rec := dashV2(t, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, ph := range []string{"%%API_BASE%%", "%%API_KEY%%", "%%PROVIDER_ID%%", "%%SETUP_HINT%%"} {
		if strings.Contains(body, ph) {
			t.Errorf("placeholder %s not substituted", ph)
		}
	}
	for _, want := range []string{
		`apiBase: "https://llm.example"`,
		`providerId: "llm"`,
		`apiKey: "pat_…"`,
		// The multi-line hint lands as one JSON string literal.
		`setupHint: "line one\n\"quoted\" line two"`,
		`type="module" src="/static/app.js"`,
		`href="/static/dashboard.css"`,
		`id="tab-root"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell missing %q", want)
		}
	}
	if strings.Contains(body, "sk-") {
		t.Errorf("shell must not carry a key")
	}
}

func TestDashboardV2_StaticServing(t *testing.T) {
	cases := []struct {
		path   string
		status int
		ct     string
		body   string
	}{
		{"/static/app.js", 200, "text/javascript", "export function parseHash"},
		{"/static/dashboard.css", 200, "text/css", ".tabs a.active"},
		{"/static/lib/fmt.js", 200, "text/javascript", "export function escHtml"},
		{"/static/lib/api.js", 200, "text/javascript", "sse("},
		{"/static/tabs/activity.js", 200, "text/javascript", `id: "activity"`},
		{"/static/tabs/fleet.js", 200, "text/javascript", `id: "fleet"`},
		{"/static/tabs/catalog.js", 200, "text/javascript", `id: "catalog"`},
		{"/static/tabs/traffic.js", 200, "text/javascript", `id: "traffic"`},
		{"/static/tabs/connect.js", 200, "text/javascript", `id: "connect"`},
		{"/static/nope.js", 404, "", ""},
		{"/static/index.html", 404, "", ""},
		{"/static/", 404, "", ""},
		{"/static/lib/", 404, "", ""},
		{"/static/lib", 404, "", ""},
	}
	for _, tc := range cases {
		rec := dashV2(t, tc.path)
		if rec.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.path, rec.Code, tc.status)
			continue
		}
		if tc.status != 200 {
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.ct) {
			t.Errorf("%s: content-type = %q, want %s", tc.path, ct, tc.ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: cache-control = %q", tc.path, cc)
		}
		if !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%s: body missing %q", tc.path, tc.body)
		}
	}
	// Path traversal: the mux cleans the path (a 301 to the cleaned form or a
	// 404), and nothing outside the embedded tree is reachable either way.
	rec := dashV2(t, "/static/../dashboard.go")
	if rec.Code == 200 {
		t.Errorf("traversal must not serve a file: %d", rec.Code)
	}
}

// Every file under the embedded tree is reachable through /static/ — a new
// module that is not served is the most likely way to break the shell.
func TestDashboardV2_EveryEmbeddedFileServed(t *testing.T) {
	sub, err := fs.Sub(dashboardV2, "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "index.html" {
			return err
		}
		n++
		if rec := dashV2(t, "/static/"+p); rec.Code != 200 {
			t.Errorf("/static/%s: %d", p, rec.Code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 8 {
		t.Errorf("expected at least 8 embedded assets, walked %d", n)
	}
}
