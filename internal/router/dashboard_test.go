package router

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/health"
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

// trackerNodes is an availability that also carries a last node poll, the
// way the real tracker does, so the Catalog can be tested without a probe.
type trackerNodes struct {
	alwaysRoutable
	nodes map[string]health.NodeSnapshot
}

func (t trackerNodes) Nodes() map[string]health.NodeSnapshot { return t.nodes }

func TestDashboard_CatalogAndFleetShape(t *testing.T) {
	rt := newTestRouter(t, nil, WithAvailability(trackerNodes{nodes: map[string]health.NodeSnapshot{
		"archimedes": {Reachable: true, Models: []health.AgentModel{{ModelID: "nemotron-3-super", State: "running", RequestsRunning: 2}}},
		"hypatia":    {Reachable: false, Models: []health.AgentModel{{ModelID: "qwen36-hypatia", State: "running"}}},
	}}))
	// A live probe that would take far too long if anyone called it.
	var probed atomic.Int32
	rt.nodeFetcher = func(_ context.Context, host string, _ int) nodeMetric {
		probed.Add(1)
		time.Sleep(50 * time.Millisecond)
		if host == "archimedes.local" {
			return nodeMetric{Reachable: true, Models: []nodeModelMetric{{ModelID: "nemotron-3-super", State: "running"}}}
		}
		return unreachableNode()
	}

	start := time.Now()
	out := getDashJSON(t, rt, "/api/catalog")
	if n := probed.Load(); n != 0 {
		t.Errorf("/api/catalog probed %d node(s); it must never wait on an agent", n)
	}
	if d := time.Since(start); d > 40*time.Millisecond {
		t.Errorf("/api/catalog took %s", d)
	}
	if got := int(out["model_count"].(float64)); got == 0 {
		t.Fatalf("model_count = 0, want the full registry")
	}
	// The Connection card's litellm_url echoes the configured API base.
	if out["litellm_url"] != "http://localhost:4010" {
		t.Errorf("litellm_url = %v", out["litellm_url"])
	}
	if _, has := out["nodes"]; has {
		t.Errorf("/api/catalog must not carry nodes (that is /api/fleet)")
	}

	byID := map[string]map[string]any{}
	for _, m := range out["models"].([]any) {
		mm := m.(map[string]any)
		byID[mm["id"].(string)] = mm
	}

	// Node-managed model with a live agent (from the tracker's last poll):
	// health "unknown", agent_state carries the state, requests too,
	// head_node = its node. An unreachable node's stale states are not used.
	nem := byID["nemotron-3-super"]
	if nem["health"] != "unknown" || nem["agent_state"] != "running" || int(nem["requests_running"].(float64)) != 2 {
		t.Errorf("nemotron health/agent_state/requests = %v/%v/%v, want unknown/running/2", nem["health"], nem["agent_state"], nem["requests_running"])
	}
	if q := byID["qwen36-hypatia"]; q["agent_state"] != nil {
		t.Errorf("qwen36-hypatia agent_state = %v, want nil (its node is unreachable)", q["agent_state"])
	}

	// Fleet: DOES probe, carries nodes/node_metrics/roles/inventory.
	fl := getDashJSON(t, rt, "/api/fleet")
	if probed.Load() == 0 {
		t.Errorf("/api/fleet should probe the agents")
	}
	nodes := fl["nodes"].(map[string]any)
	arch := nodes["archimedes"].(map[string]any)
	if _, ok := arch["unified_memory"]; !ok {
		t.Errorf("node archimedes missing unified_memory: %v", arch)
	}
	for _, k := range []string{"node_metrics", "roles", "node_count"} {
		if _, ok := fl[k]; !ok {
			t.Errorf("/api/fleet missing %q", k)
		}
	}
	if _, has := fl["models"]; has {
		t.Errorf("/api/fleet must not carry the model table")
	}

	// The old combined endpoint is gone.
	rec := httptest.NewRecorder()
	rt.DashboardHandler(DashboardConfig{APIBase: "http://localhost:4010"}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("/api/models = %d, want 404", rec.Code)
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

// ---------------------------------------------------------------------------
// /api/overview — the header strip
// ---------------------------------------------------------------------------

// stubReporter is an availability with a canned per-model verdict, for the
// tally tests: it implements the reporter half the dashboard reads.
type stubReporter struct {
	states map[string]health.Availability
}

func (s stubReporter) Routable(id string) bool {
	st := s.states[id]
	return st != health.Unavailable && st != health.Warming && st != health.Absent
}
func (s stubReporter) Reason(id string) string           { return string(s.states[id]) }
func (s stubReporter) ReportFailure(string, error)       {}
func (s stubReporter) ReportSuccess(string)              {}
func (s stubReporter) NodeStatuses() []health.NodeStatus { return nil }
func (s stubReporter) Snapshot() []health.Status {
	var out []health.Status
	for id, st := range s.states {
		out = append(out, health.Status{Model: id, State: st})
	}
	return out
}

func TestDashboard_OverviewWithoutTracker(t *testing.T) {
	rt := newTestRouter(t, nil)
	out := getDashJSON(t, rt, "/api/overview")
	models := out["models"].(map[string]any)
	if int(models["active"].(float64)) != len(rt.active) || int(models["up"].(float64)) != len(rt.active) {
		t.Errorf("active/up = %v/%v, want %d/%d", models["active"], models["up"], len(rt.active), len(rt.active))
	}
	for _, k := range []string{"warming", "absent", "unavailable", "discovered"} {
		if models[k].(float64) != 0 {
			t.Errorf("%s = %v, want 0 without a tracker", k, models[k])
		}
	}
	roles := out["roles"].(map[string]any)
	if int(roles["total"].(float64)) != len(rt.RoleNames()) {
		t.Errorf("roles.total = %v, want %d", roles["total"], len(rt.RoleNames()))
	}
	if out["version"] != "dev" || out["replica"] == "" || out["uptime_s"].(float64) < 0 {
		t.Errorf("identity fields: %v %v %v", out["version"], out["replica"], out["uptime_s"])
	}
}

func TestDashboard_OverviewTalliesTrackerVerdicts(t *testing.T) {
	rt := newTestRouter(t, nil, WithAvailability(stubReporter{states: map[string]health.Availability{
		"nemotron-3-super": health.Available,
		"qwen36-hypatia":   health.Warming,
		"zen-glm":          health.Absent,
		"zen-lit":          health.Unavailable,
		"not-in-catalog":   health.Absent, // ignored: not an active model
	}}))
	out := getDashJSON(t, rt, "/api/overview")
	models := out["models"].(map[string]any)
	want := map[string]int{"warming": 1, "absent": 1, "unavailable": 1, "up": 1}
	for k, v := range want {
		if int(models[k].(float64)) != v {
			t.Errorf("%s = %v, want %d", k, models[k], v)
		}
	}
}

func TestDashboard_OverviewRequestsPerMinute(t *testing.T) {
	rt := newTestRouter(t, nil)
	for i := 0; i < 3; i++ {
		postChat(t, rt, `{"model":"no-such-model","messages":[]}`) // 404s still count as requests
	}
	out := getDashJSON(t, rt, "/api/overview")
	if got := int(out["requests_per_min"].(float64)); got < 3 {
		t.Errorf("requests_per_min = %d, want >= 3", got)
	}
	// The window forgets: slots older than 60 s do not count.
	w := newReqRateWindow()
	base := time.Unix(1_000_000, 0)
	w.hit(base)
	w.hit(base.Add(30 * time.Second))
	if n := w.perMinute(base.Add(59 * time.Second)); n != 2 {
		t.Errorf("perMinute at +59s = %d, want 2", n)
	}
	if n := w.perMinute(base.Add(61 * time.Second)); n != 1 {
		t.Errorf("perMinute at +61s = %d, want 1 (first hit aged out)", n)
	}
	if n := w.perMinute(base.Add(200 * time.Second)); n != 0 {
		t.Errorf("perMinute at +200s = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// /api/traffic — series from the request log
// ---------------------------------------------------------------------------

func trafficReq(t *testing.T, rt *Router, cfg DashboardConfig, path string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rt.DashboardHandler(cfg).ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestDashboard_Traffic(t *testing.T) {
	sink := &reqlog.MemorySink{}
	now := time.Now()
	for i, m := range []string{"alpha", "alpha", "beta"} {
		sink.Log(reqlog.Record{TS: now.Add(-time.Duration(i+1) * time.Minute), ResolvedVia: m, Principal: []string{"steven", "steven", "family"}[i], Status: 200, LatencyMS: 10})
	}
	sink.Log(reqlog.Record{TS: now.Add(-2 * time.Minute), ResolvedVia: "beta", Principal: "family", Status: 502, ErrorClass: "server_error", BackendURL: "http://b"})
	rt := newTestRouter(t, nil, WithSink(sink))
	open := DashboardConfig{}

	code, out := trafficReq(t, rt, open, "/api/traffic?window=1h&by=model", nil)
	if code != 200 || out["available"] != true || out["bucket_s"].(float64) != 60 || out["by"] != "model" {
		t.Fatalf("1h: %d %v", code, out)
	}
	rows := out["rows"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["key"] != "alpha" && rows[0].(map[string]any)["key"] != "beta" {
		t.Errorf("rows = %v", rows)
	}
	if nb := len(rows[0].(map[string]any)["buckets"].([]any)); nb < 60 || nb > 62 {
		t.Errorf("1h buckets = %d, want ~61 one-minute buckets", nb)
	}
	if fails := out["failures"].([]any); len(fails) != 1 {
		t.Errorf("failures = %v, want the one 502", fails)
	}
	_, out = trafficReq(t, rt, open, "/api/traffic?window=7d", nil)
	if out["bucket_s"].(float64) != 3600 || out["window"] != "7d" {
		t.Errorf("7d: %v", out)
	}
	_, out = trafficReq(t, rt, open, "/api/traffic", nil)
	if out["window"] != "24h" || out["bucket_s"].(float64) != 900 {
		t.Errorf("default window: %v", out)
	}
	if code, _ := trafficReq(t, rt, open, "/api/traffic?window=2h", nil); code != 400 {
		t.Errorf("window=2h: %d, want 400", code)
	}
	if code, _ := trafficReq(t, rt, open, "/api/traffic?by=node", nil); code != 400 {
		t.Errorf("by=node: %d, want 400", code)
	}

	// No queryable sink: available:false, same shape as /api/usage.
	_, out = trafficReq(t, newTestRouter(t, nil), open, "/api/traffic", nil)
	if out["available"] != false {
		t.Errorf("NopSink: %v", out)
	}

	// Non-owner by principal: only their own row; by model: everything.
	gated := DashboardConfig{AuthSecret: "s", Owners: []string{"owner@example"}}
	fam := map[string]string{DashboardAuthHeader: "s", "X-Auth-Request-Email": "family"}
	_, out = trafficReq(t, rt, gated, "/api/traffic?window=1h&by=principal", fam)
	rows = out["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["key"] != "family" {
		t.Errorf("non-owner by principal = %v, want only family", rows)
	}
	_, out = trafficReq(t, rt, gated, "/api/traffic?window=1h&by=model", fam)
	if len(out["rows"].([]any)) != 2 {
		t.Errorf("non-owner by model should see every model row: %v", out["rows"])
	}
	_, out = trafficReq(t, rt, gated, "/api/traffic?window=1h&by=principal", map[string]string{DashboardAuthHeader: "s", "X-Auth-Request-Email": "owner@example"})
	if len(out["rows"].([]any)) != 2 {
		t.Errorf("owner by principal should see both principals: %v", out["rows"])
	}
}
