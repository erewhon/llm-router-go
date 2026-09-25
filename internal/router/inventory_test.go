package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/erewhon/llm-router-go/internal/auth"
	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/health"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// The live-inventory contract, end to end: a real tracker with a fake listing
// fetcher, a real router in front of a fake upstream.

const inventoryRouterYAML = `
nodes:
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
roles:
  coder:
    require: {locality: any}
    candidates: [seat, zen/pinned]
    on_empty: error
models:
  seat:
    hf_repo: Qwen/Seat
    node: archimedes
    api_port: 5391
    aliases: [seat-alias]
    input_cost_per_million: 0
    output_cost_per_million: 0
  zen/pinned:
    hf_repo: pinned
    backend: external
    api_base: https://zen.example/v1
    api_key: ZEN_TEST_KEY
    aliases: [pinned-alias]
    context_length: 65536
    input_cost_per_million: 1
    output_cost_per_million: 2
discovery:
  - api_base: https://zen.example/v1
    prefix: zen/
    api_key: ZEN_TEST_KEY
    adopt: all
    tags: [zen]
`

const (
	invSeatRoot = "http://archimedes.local:5391"
	invZenRoot  = "https://zen.example"
)

// fakeListings is the ListFunc the tracker is given: a per-root listing the
// test rewrites between refreshes.
type fakeListings struct {
	mu sync.Mutex
	m  map[string]health.Listing
}

func (f *fakeListings) set(root string, models ...config.ListedModel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := health.Listing{}
	for _, lm := range models {
		l[lm.ID] = lm
	}
	f.m[root] = l
}

func (f *fakeListings) list(_ context.Context, root, _, _ string) (health.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.m[root]
	if !ok {
		return nil, errors.New("unreachable")
	}
	out := health.Listing{}
	for k, v := range l {
		out[k] = v
	}
	return out, nil
}

func listed(id string) config.ListedModel { return config.ListedModel{ID: id} }

// upstreamCapture records what the router forwarded and answers a chat
// completion naming the model it was asked for.
type upstreamCapture struct {
	mu     sync.Mutex
	paths  []string
	models []string
	auths  []string
}

func (u *upstreamCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.models = append(u.models, req.Model)
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"`+req.Model+
			`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
}

type invHarness struct {
	rt       *Router
	tracker  *health.Tracker
	listings *fakeListings
	upstream *upstreamCapture
	sink     *reqlog.MemorySink
}

func newInventoryHarness(t *testing.T) *invHarness {
	t.Helper()
	return newInventoryHarnessYAML(t, inventoryRouterYAML)
}

func newInventoryHarnessYAML(t *testing.T, yml string) *invHarness {
	t.Helper()
	reg, err := config.LoadBytes([]byte(yml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	fl := &fakeListings{m: map[string]health.Listing{}}
	fl.set(invSeatRoot, listed("Qwen/Seat"))
	fl.set(invZenRoot, listed("pinned"))
	getenv := func(k string) string {
		if k == "ZEN_TEST_KEY" {
			return "zen-secret"
		}
		return ""
	}
	tracker := health.NewTracker(health.Config{
		Registry: reg,
		Probe: func(context.Context, string, int) health.NodeSnapshot {
			return health.NodeSnapshot{Reachable: true, Health: &health.AgentHealth{}}
		},
		DisableGenerationProbe: true,
		List:                   fl.list,
		Getenv:                 getenv,
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	up := &upstreamCapture{}
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)
	sink := &reqlog.MemorySink{}
	rt := New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithFlushInterval(0),
		WithGetenv(getenv),
		WithTransport(&transportRedirect{to: srv.URL, rt: http.DefaultTransport}),
		WithAvailability(tracker),
		WithSink(sink),
		WithWellKnown(WellKnownConfig{ProviderID: "llm", ProviderName: "LLM Router", BaseURL: "https://llm.example/v1"}),
	)
	tracker.SetOnPoll(rt.PublishAvailabilityMetrics)
	h := &invHarness{rt: rt, tracker: tracker, listings: fl, upstream: up, sink: sink}
	h.refresh()
	return h
}

// refresh runs one poll round and one full listing refresh, synchronously.
func (h *invHarness) refresh() {
	h.tracker.PollOnce(context.Background())
	h.tracker.RefreshInventory(context.Background())
}

type modelsEntry struct {
	ID         string `json:"id"`
	OwnedBy    string `json:"owned_by"`
	Role       bool   `json:"role"`
	Discovered bool   `json:"discovered"`
}

func (h *invHarness) models(t *testing.T) map[string]modelsEntry {
	t.Helper()
	rec := httptest.NewRecorder()
	h.rt.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models status = %d", rec.Code)
	}
	var resp struct {
		Data []modelsEntry `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := map[string]modelsEntry{}
	for _, e := range resp.Data {
		out[e.ID] = e
	}
	return out
}

func (h *invHarness) getJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.rt.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status = %d: %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return out
}

func TestInventory_DiscoveredEntryIsListedRoutableAndLogged(t *testing.T) {
	h := newInventoryHarness(t)
	if _, ok := h.models(t)["zen/new-model"]; ok {
		t.Fatal("nothing discovered yet")
	}

	// Zen turns on a model. One poll later it is on the router.
	h.listings.set(invZenRoot, listed("pinned"), listed("new-model"))
	h.refresh()
	got := h.models(t)
	e, ok := got["zen/new-model"]
	if !ok {
		t.Fatalf("zen/new-model should be listed after one poll; have %v", invKeys(got))
	}
	if !e.Discovered || e.OwnedBy != "external" {
		t.Errorf("entry = %+v, want discovered external", e)
	}
	if got["zen/pinned"].Discovered || got["seat"].Discovered {
		t.Error("hand-written entries must not be flagged discovered")
	}

	// Routable by name, end to end: forwarded to the source's base with the
	// bare provider id and the source's credential.
	rec := postChat(t, h.rt, `{"model":"zen/new-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d: %s", rec.Code, rec.Body.String())
	}
	h.upstream.mu.Lock()
	models, auths, paths := append([]string(nil), h.upstream.models...), append([]string(nil), h.upstream.auths...), append([]string(nil), h.upstream.paths...)
	h.upstream.mu.Unlock()
	if len(models) != 1 || models[0] != "new-model" {
		t.Errorf("upstream model = %v, want [new-model]", models)
	}
	if auths[0] != "Bearer zen-secret" {
		t.Errorf("upstream auth = %q, want the source's api_key", auths[0])
	}
	if paths[0] != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", paths[0])
	}
	recs := h.sink.Records()
	if len(recs) != 1 {
		t.Fatalf("reqlog records = %d, want 1", len(recs))
	}
	if !recs[0].Discovered || recs[0].ResolvedVia != "zen/new-model" || recs[0].Status != 200 {
		t.Errorf("reqlog row = %+v, want discovered=true resolved_via=zen/new-model", recs[0])
	}

	// A request to a hand-written entry is not flagged.
	postChat(t, h.rt, `{"model":"zen/pinned","messages":[]}`)
	if recs := h.sink.Records(); recs[len(recs)-1].Discovered {
		t.Error("hand-written entry must log discovered=false")
	}

	// The provider drops it: gone after two consecutive misses, and a
	// request by that name is an honest 404 again.
	h.listings.set(invZenRoot, listed("pinned"))
	h.refresh()
	if _, ok := h.models(t)["zen/new-model"]; !ok {
		t.Error("one miss must not retire a discovered id")
	}
	h.refresh()
	if _, ok := h.models(t)["zen/new-model"]; ok {
		t.Error("two misses should retire the id")
	}
	if rec := postChat(t, h.rt, `{"model":"zen/new-model","messages":[]}`); rec.Code != http.StatusNotFound {
		t.Errorf("retired id should be unknown again, got %d", rec.Code)
	}
}

func TestInventory_HandWrittenEntryWinsOverDiscovery(t *testing.T) {
	h := newInventoryHarness(t)
	// Zen lists "pinned" — which models.yaml already has as zen/pinned with
	// its own pricing, context and alias — and "pinned-alias", whose
	// prefixed form is free to adopt.
	h.listings.set(invZenRoot, listed("pinned"), config.ListedModel{ID: "pinned-alias", ContextLength: 1})
	h.refresh()
	got := h.models(t)
	if got["zen/pinned"].Discovered {
		t.Error("the hand-written entry must not be replaced by a discovered one")
	}
	if _, ok := got["pinned-alias"]; !ok {
		t.Error("the hand-written entry's alias must survive")
	}
	if _, ok := got["zen/pinned-alias"]; !ok {
		t.Error("a prefixed id that shadows nothing is adopted")
	}
	wk := h.wellKnownModels(t)
	if wk["zen/pinned"].Limit.Context != 65536 || wk["zen/pinned"].Cost == nil || wk["zen/pinned"].Cost.Input != 1 {
		t.Errorf("hand-written pricing/context must stay authoritative: %+v", wk["zen/pinned"])
	}
}

func TestInventory_AbsentEntryLeavesListingsAndRoles(t *testing.T) {
	h := newInventoryHarness(t)
	got := h.models(t)
	for _, want := range []string{"seat", "seat-alias", "coder"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("%q should be listed while the seat serves its model", want)
		}
	}
	if !h.rt.avail.Routable("seat") {
		t.Fatalf("seat should be routable: %s", h.rt.avail.Reason("seat"))
	}

	// The port now serves a different checkpoint.
	h.listings.set(invSeatRoot, listed("Qwen/Something-Else"))
	h.refresh()
	got = h.models(t)
	if _, ok := got["seat"]; ok {
		t.Error("an absent entry must be dropped from /v1/models")
	}
	if _, ok := got["seat-alias"]; ok {
		t.Error("an absent entry's aliases go with it")
	}
	if _, ok := got["coder"]; !ok {
		t.Error("the role stays listed; it has another candidate")
	}
	if _, ok := h.wellKnownModels(t)["seat"]; ok {
		t.Error("an absent entry must be dropped from the well-known")
	}

	// The role skips it, and says why.
	res, err := h.rt.resolveRole("coder", "coder", false, 0, tierNone)
	if err != nil {
		t.Fatalf("coder should resolve to zen/pinned: %v", err)
	}
	if res.ModelID != "zen/pinned" {
		t.Errorf("coder -> %s, want zen/pinned", res.ModelID)
	}
	var binding roleBinding
	for _, b := range h.rt.roleBindings() {
		if b.Role == "coder" {
			binding = b
		}
	}
	if binding.Target != "zen/pinned" {
		t.Errorf("binding target = %q", binding.Target)
	}
	reason := h.rt.avail.Reason("seat")
	if !strings.Contains(reason, "absent") || !strings.Contains(reason, "Qwen/Something-Else") {
		t.Errorf("reason should say absent and name what is served: %q", reason)
	}

	// Surfaces: /v1/availability carries the verdict and the inventory with
	// the listing; /health carries the tally and the inventory without it.
	av := h.getJSON(t, "/v1/availability")
	inv, _ := av["inventory"].([]any)
	if len(inv) != 2 {
		t.Fatalf("availability inventory = %v, want two bases", av["inventory"])
	}
	var seatBase map[string]any
	for _, b := range inv {
		bm := b.(map[string]any)
		if bm["base"] == invSeatRoot {
			seatBase = bm
		}
	}
	if seatBase == nil || seatBase["listed"] == nil {
		t.Errorf("availability should carry the listing: %v", seatBase)
	}
	if abs, _ := seatBase["absent"].([]any); len(abs) != 1 || abs[0] != "seat" {
		t.Errorf("absent = %v", seatBase["absent"])
	}
	hl := h.getJSON(t, "/health")
	if counts, _ := hl["availability"].(map[string]any); counts["absent"] != float64(1) {
		t.Errorf("/health availability tally = %v, want absent:1", hl["availability"])
	}
	hinv, _ := hl["inventory"].([]any)
	if len(hinv) != 2 || hinv[0].(map[string]any)["listed"] != nil {
		t.Errorf("/health inventory should omit the id lists: %v", hl["inventory"])
	}

	// And it comes back with the listing.
	h.listings.set(invSeatRoot, listed("Qwen/Seat"))
	h.refresh()
	if _, ok := h.models(t)["seat"]; !ok {
		t.Error("the entry returns when the listing does")
	}
}

func TestInventory_DiscoveredNeverJoinsARole(t *testing.T) {
	h := newInventoryHarness(t)
	h.listings.set(invZenRoot, listed("pinned"), listed("shiny"))
	// Both of coder's candidates go: the seat is absent, the pinned entry's
	// breaker is open. The role must fail, not reach for the discovered id.
	h.listings.set(invSeatRoot, listed("other"))
	h.refresh()
	for i := 0; i < health.DefaultBreakerTrip; i++ {
		h.tracker.ReportFailure("zen/pinned", errors.New("upstream status 502"))
	}
	if _, err := h.rt.resolveRole("coder", "coder", false, 0, tierNone); err == nil {
		t.Fatal("coder should be unavailable with both candidates out")
	}
	for _, b := range h.rt.roleBindings() {
		if b.Role != "coder" {
			continue
		}
		if strings.Join(b.Candidates, ",") != "seat,zen/pinned" {
			t.Errorf("candidates = %v, want the config's list unchanged", b.Candidates)
		}
		if b.Available {
			t.Error("role must not be available via a discovered entry")
		}
	}
	// The discovered id is routable directly all the while.
	if !h.rt.avail.Routable("zen/shiny") {
		t.Error("zen/shiny should be routable by name")
	}
	if _, ok := h.models(t)["zen/shiny"]; !ok {
		t.Error("zen/shiny should be listed")
	}
}

func TestInventory_PrivacyTierAndScopeGateDiscoveredEntries(t *testing.T) {
	h := newInventoryHarness(t)
	h.listings.set(invZenRoot, listed("pinned"), listed("paid-thing"))
	h.refresh()

	// The caller's header: a discovered external is not local.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen/paid-thing","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(PrivacyHeader, PrivacyLocal)
	rec := httptest.NewRecorder()
	h.rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("X-Router-Privacy: local should refuse a discovered external, got %d: %s", rec.Code, rec.Body.String())
	}

	// A models:local token: same fence, scope-flavoured.
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen/paid-thing","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithIdentity(req.Context(),
		auth.Identity{Principal: "family", TokenID: "cccccccccccc", Scopes: []string{auth.ScopeModelsLocal}}))
	rec = httptest.NewRecorder()
	h.rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "token_scope_denied") {
		t.Errorf("models:local token should be refused, got %d: %s", rec.Code, rec.Body.String())
	}
	h.upstream.mu.Lock()
	n := len(h.upstream.models)
	h.upstream.mu.Unlock()
	if n != 0 {
		t.Errorf("nothing may reach the upstream under a refusal, got %d calls", n)
	}
	// Unscoped, it goes through.
	if rec := postChat(t, h.rt, `{"model":"zen/paid-thing","messages":[]}`); rec.Code != http.StatusOK {
		t.Errorf("unrestricted request should be served, got %d", rec.Code)
	}
}

func TestInventory_WellKnownCarriesProviderMetadata(t *testing.T) {
	h := newInventoryHarness(t)
	in, out := 3.0, 15.0
	h.listings.set(invZenRoot, listed("pinned"),
		config.ListedModel{ID: "priced", ContextLength: 200000, MaxOutputTokens: 32000,
			InputCostPerMillion: &in, OutputCostPerMillion: &out},
		listed("bare"))
	h.refresh()
	wk := h.wellKnownModels(t)
	p, ok := wk["zen/priced"]
	if !ok {
		t.Fatalf("discovered chat entries belong in the well-known; have %v", keysWK(wk))
	}
	if p.Limit.Context != 200000 || p.Limit.Output != 32000 {
		t.Errorf("limit = %+v, want the provider's", p.Limit)
	}
	if p.Cost == nil || p.Cost.Input != 3 || p.Cost.Output != 15 {
		t.Errorf("cost = %+v, want the provider's", p.Cost)
	}
	b := wk["zen/bare"]
	if b.Limit.Context != 131072 || b.Cost != nil {
		t.Errorf("unannotated entry should take the endpoint default and show no cost: %+v", b)
	}
}

func TestInventory_DashboardShowsDiscoveredRowsAndInventory(t *testing.T) {
	h := newInventoryHarness(t)
	h.listings.set(invZenRoot, listed("pinned"), listed("dash"))
	h.listings.set(invSeatRoot, listed("other"))
	h.refresh()
	dash := h.rt.DashboardHandler(DashboardConfig{APIBase: "http://x"})
	rec := httptest.NewRecorder()
	dash.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/catalog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/catalog status = %d", rec.Code)
	}
	var body struct {
		Models []struct {
			ID           string `json:"id"`
			Discovered   bool   `json:"discovered"`
			Availability string `json:"availability"`
			Tags         []string
			APIBase      string `json:"api_base"`
		} `json:"models"`
		Inventory []health.BaseInventory `json:"inventory"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	dash.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
	var fleet struct {
		Inventory []health.BaseInventory `json:"inventory"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&fleet); err != nil {
		t.Fatal(err)
	}
	body.Inventory = fleet.Inventory
	var sawDash, sawSeat bool
	for _, m := range body.Models {
		switch m.ID {
		case "zen/dash":
			sawDash = true
			if !m.Discovered || m.APIBase != "https://zen.example/v1" {
				t.Errorf("discovered row = %+v", m)
			}
		case "seat":
			sawSeat = true
			if m.Availability != "absent" {
				t.Errorf("seat availability = %q, want absent", m.Availability)
			}
		}
	}
	if !sawDash || !sawSeat {
		t.Errorf("dashboard rows missing: dash=%v seat=%v", sawDash, sawSeat)
	}
	if len(body.Inventory) != 2 {
		t.Errorf("inventory = %+v", body.Inventory)
	}
}

func (h *invHarness) wellKnownModels(t *testing.T) map[string]struct {
	Name  string `json:"name"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Cost *struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	} `json:"cost"`
} {
	t.Helper()
	rec := getWellKnown(t, h.rt)
	if rec.Code != http.StatusOK {
		t.Fatalf("well-known status = %d", rec.Code)
	}
	var doc wellKnownResp
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc.Provider()["llm"].Models
}

func invKeys(m map[string]modelsEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysWK[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
