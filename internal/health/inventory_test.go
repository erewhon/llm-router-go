package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

const inventoryYAML = `
nodes:
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
roles:
  coder:
    require: {locality: any}
    candidates: [seat, zen/pinned]
    on_empty: error
models:
  # local seat, tool_proxy on: the inventory must ask the engine, not the hop
  seat:
    hf_repo: Qwen/Qwen3.5-122B
    node: archimedes
    api_port: 5391
    tool_proxy: true
    aliases: [seat-alias]
  # hand-written zen entry: wins over discovery of the same id
  zen/pinned:
    hf_repo: pinned
    backend: external
    api_base: https://zen.example/v1
    api_key: ZEN_KEY
    input_cost_per_million: 1
    output_cost_per_million: 2
  # hand-written openrouter entry with a served name the provider may drop
  or/old:
    hf_repo: vendor/old-model
    backend: external
    api_base: https://or.example/api/v1
    api_key: OR_KEY
  # hand-written, under the operator's short name, for a provider id the
  # allowlist would otherwise adopt as or/anthropic/claude-opus-5
  or/claude-opus-5:
    hf_repo: anthropic/claude-opus-5
    backend: external
    api_base: https://or.example/api/v1
    api_key: OR_KEY
  # disabled on purpose: discovery must not resurrect it under another name
  or/claude-sonnet-5:
    hf_repo: anthropic/claude-sonnet-5
    backend: external
    api_base: https://or.example/api/v1
    api_key: OR_KEY
    enabled: false
  # media class: never inventoried
  flux-dev:
    hf_repo: FLUX.1-dev
    backend: external
    node: archimedes
    api_base: http://archimedes.local:5396
    api_class: image_gen
  # explicit opt-out
  quiet:
    hf_repo: quiet/model
    backend: external
    api_base: http://quiet.example:9000/v1
    health: {inventory: false}
  # disabled: not a member of anything
  ghost:
    hf_repo: ghost/model
    node: archimedes
    api_port: 5399
    enabled: false
discovery:
  - api_base: https://zen.example/v1
    prefix: zen/
    api_key: ZEN_KEY
    adopt: all
    tags: [zen]
  - api_base: https://or.example/api/v1
    prefix: or/
    api_key: OR_KEY
    adopt: ["anthropic/claude-*", "deepseek/*", "z-ai/glm-5*", "moonshotai/kimi-*", "qwen/qwen3.8-*"]
    exclude: ["*:batch"]
    tags: [openrouter]
`

// fakeProviders serves a listing per root, records fetches, and can make a
// root fail or hang.
type fakeProviders struct {
	mu       sync.Mutex
	listings map[string]Listing
	fail     map[string]error
	hang     map[string]bool
	fetches  map[string]int
	bearers  map[string]string
}

func newFakeProviders() *fakeProviders {
	return &fakeProviders{
		listings: map[string]Listing{},
		fail:     map[string]error{},
		hang:     map[string]bool{},
		fetches:  map[string]int{},
		bearers:  map[string]string{},
	}
}

func (f *fakeProviders) set(root string, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := Listing{}
	for _, id := range ids {
		l[id] = config.ListedModel{ID: id}
	}
	f.listings[root] = l
}

func (f *fakeProviders) list(ctx context.Context, root, bearer, _ string) (Listing, error) {
	f.mu.Lock()
	f.fetches[root]++
	f.bearers[root] = bearer
	hang, err, l := f.hang[root], f.fail[root], f.listings[root]
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, errors.New("no such base")
	}
	// Copy: the tracker keeps what it is handed.
	out := make(Listing, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out, nil
}

func (f *fakeProviders) count(root string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches[root]
}

const (
	seatRoot = "http://archimedes.local:5391"
	zenRoot  = "https://zen.example"
	orRoot   = "https://or.example/api"
)

func newInventoryTracker(t *testing.T, fp *fakeProviders, now func() time.Time, mutate func(*Config)) (*Tracker, *config.ModelRegistry) {
	t.Helper()
	reg, err := config.LoadBytes([]byte(inventoryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if now == nil {
		now = time.Now
	}
	cfg := Config{
		Registry: reg,
		Probe: func(context.Context, string, int) NodeSnapshot {
			return NodeSnapshot{Reachable: true, Health: &AgentHealth{}}
		},
		DisableGenerationProbe: true,
		Now:                    now,
		List:                   fp.list,
		InventoryTimeout:       200 * time.Millisecond,
		Getenv: func(k string) string {
			return map[string]string{"ZEN_KEY": "zen-secret", "OR_KEY": "or-secret"}[k]
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewTracker(cfg), reg
}

// poll runs one round and waits for its listing fetches to land.
func poll(t *testing.T, tr *Tracker) {
	t.Helper()
	tr.PollOnce(context.Background())
	tr.WaitInventory()
}

func TestInventory_BasesDerivedFromRegistry(t *testing.T) {
	fp := newFakeProviders()
	tr, _ := newInventoryTracker(t, fp, nil, nil)

	roots := make([]string, 0, len(tr.inv))
	for r := range tr.inv {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	want := []string{seatRoot, orRoot, zenRoot}
	sort.Strings(want)
	if strings.Join(roots, ",") != strings.Join(want, ",") {
		t.Fatalf("bases = %v, want %v (media class, opt-out and disabled entries excluded; tool proxy bypassed)", roots, want)
	}
	if m := tr.inv[seatRoot].members; len(m) != 1 || m[0] != "seat" {
		t.Errorf("seat base members = %v", m)
	}
	if m := tr.inv[zenRoot].members; len(m) != 1 || m[0] != "zen/pinned" {
		t.Errorf("zen base members = %v", m)
	}
	if m := tr.inv[orRoot].members; len(m) != 2 || m[0] != "or/claude-opus-5" || m[1] != "or/old" {
		t.Errorf("or base members = %v (disabled entries are not members)", m)
	}
	served := tr.inv[orRoot].served
	if !served["vendor/old-model"] || !served["anthropic/claude-opus-5"] || !served["anthropic/claude-sonnet-5"] {
		t.Errorf("served ids should cover every entry at the base, disabled included: %v", served)
	}
	if tr.inv[zenRoot].bearer != "zen-secret" || tr.inv[orRoot].bearer != "or-secret" {
		t.Errorf("credentials not resolved from api_key env names: zen=%q or=%q", tr.inv[zenRoot].bearer, tr.inv[orRoot].bearer)
	}
	if tr.inv[seatRoot].bearer != "" {
		t.Errorf("a local seat fetch must carry no credential, got %q", tr.inv[seatRoot].bearer)
	}
	if len(tr.inv[zenRoot].sources) != 1 || len(tr.inv[orRoot].sources) != 1 || len(tr.inv[seatRoot].sources) != 0 {
		t.Errorf("discovery sources not attached to their bases")
	}
}

func TestInventory_LocalSeatAbsentWhenItServesAnotherModel(t *testing.T) {
	fp := newFakeProviders()
	fp.set(seatRoot, "Qwen/Qwen3.5-122B")
	fp.set(zenRoot, "pinned")
	fp.set(orRoot, "vendor/old-model")
	tr, _ := newInventoryTracker(t, fp, nil, nil)

	poll(t, tr)
	if !tr.Routable("seat") {
		t.Fatalf("seat listed under its served name must be routable: %s", tr.Reason("seat"))
	}

	// The port is reassigned: the engine now serves a different checkpoint.
	// That is the 2026-09-12 archimedes:5391 shape, and it must read as
	// drift, not as "down" — the node is fine.
	fp.set(seatRoot, "Qwen/Qwen3-Coder-Next-FP8")
	tr.RefreshInventory(context.Background())
	if tr.Routable("seat") {
		t.Fatal("seat serving a different model must not be routable")
	}
	state, source, reason := tr.State("seat")
	if state != Absent || source != SourceInventory {
		t.Errorf("state = %s/%s, want absent/inventory", state, source)
	}
	if !strings.Contains(reason, "Qwen/Qwen3-Coder-Next-FP8") || !strings.Contains(reason, seatRoot) {
		t.Errorf("reason should name what the base serves instead and where: %q", reason)
	}
	inv := tr.Inventory(true)
	var seatInv *BaseInventory
	for i := range inv {
		if inv[i].Base == seatRoot {
			seatInv = &inv[i]
		}
	}
	if seatInv == nil || len(seatInv.Absent) != 1 || seatInv.Absent[0] != "seat" {
		t.Errorf("Inventory() should report the absent member: %+v", seatInv)
	}
	if seatInv.Listed == nil || seatInv.IDs != 1 || seatInv.FetchedAt == nil || seatInv.AgeS == nil {
		t.Errorf("Inventory(true) should carry the listing and its age: %+v", seatInv)
	}
	var snap Status
	for _, s := range tr.Snapshot() {
		if s.Model == "seat" {
			snap = s
		}
	}
	if snap.State != Absent || snap.Reason == "" {
		t.Errorf("Snapshot should carry the absent verdict: %+v", snap)
	}

	// It comes back the moment the listing does.
	fp.set(seatRoot, "Qwen/Qwen3.5-122B")
	tr.RefreshInventory(context.Background())
	if !tr.Routable("seat") {
		t.Errorf("seat must be routable again once listed: %s", tr.Reason("seat"))
	}
}

func TestInventory_FetchFailureKeepsLastKnownListing(t *testing.T) {
	fp := newFakeProviders()
	fp.set(seatRoot, "something-else")
	fp.set(zenRoot, "pinned")
	fp.set(orRoot)
	tr, _ := newInventoryTracker(t, fp, nil, nil)

	poll(t, tr)
	if tr.Routable("seat") {
		t.Fatal("seat should be absent: the base lists something else")
	}
	if tr.Routable("or/old") {
		t.Fatal("or/old should be absent: the provider lists nothing")
	}

	// The base goes unreachable. Stale is better than empty: the verdict
	// stands, the error and the age are visible.
	fp.fail[seatRoot] = errors.New("connection refused")
	tr.RefreshInventory(context.Background())
	if tr.Routable("seat") {
		t.Error("a fetch failure must not clear an absent verdict (last-known listing applies)")
	}
	for _, b := range tr.Inventory(false) {
		if b.Base != seatRoot {
			continue
		}
		if !b.Stale || b.Error == "" || b.IDs != 1 || b.FetchedAt == nil {
			t.Errorf("stale base should keep its listing and report the error: %+v", b)
		}
		if b.Listed != nil {
			t.Errorf("Inventory(false) must omit the id list")
		}
	}

	// A base that never answered has no listing, so nothing is absent
	// (no evidence, no verdict).
	fp2 := newFakeProviders()
	fp2.fail[seatRoot] = errors.New("refused")
	fp2.fail[zenRoot] = errors.New("refused")
	fp2.fail[orRoot] = errors.New("refused")
	tr2, _ := newInventoryTracker(t, fp2, nil, nil)
	poll(t, tr2)
	for _, id := range []string{"seat", "zen/pinned", "or/old"} {
		if !tr2.Routable(id) {
			t.Errorf("%s: no listing ever fetched must mean no absent verdict, got %s", id, tr2.Reason(id))
		}
	}
	for _, b := range tr2.Inventory(false) {
		if b.Error == "" || b.Stale || b.FetchedAt != nil {
			t.Errorf("never-fetched base should report the error and no listing: %+v", b)
		}
	}
}

func TestInventory_DiscoveryAdoptsHandWrittenWinsRetiresAfterTwoMisses(t *testing.T) {
	fp := newFakeProviders()
	fp.set(seatRoot, "Qwen/Qwen3.5-122B")
	fp.set(zenRoot, "pinned", "claude-opus-5", "coder", "seat-alias")
	fp.set(orRoot, "vendor/old-model")
	tr, _ := newInventoryTracker(t, fp, nil, nil)

	poll(t, tr)
	d := tr.Discovered()
	if _, ok := d["zen/claude-opus-5"]; !ok {
		t.Fatalf("zen/claude-opus-5 should be adopted within one poll, got %v", keys(d))
	}
	if _, dup := d["zen/pinned"]; dup {
		t.Error("a hand-written entry with the same id must win over discovery")
	}
	// A discovered id must never shadow a role or an alias, prefix or not.
	// (Neither zen/coder nor zen/seat-alias is reserved, so both are fine;
	// what is reserved is the bare name, which the prefix keeps clear of.)
	if _, ok := d["zen/coder"]; !ok {
		t.Error("prefixed ids are safe to adopt even when the bare name is a role")
	}
	m := d["zen/claude-opus-5"]
	if m.Backend != config.BackendExternal || m.APIBase != "https://zen.example/v1" || m.APIKey != "ZEN_KEY" {
		t.Errorf("discovered entry should route to the source: %+v", m)
	}
	if !m.IsDiscovered() || !containsTag(m.Tags, "zen") || !containsTag(m.Tags, "paid") {
		t.Errorf("tags = %v", m.Tags)
	}
	// The pinned entry keeps its own pricing: discovery never touched it.
	var pinnedSeen bool
	for _, s := range tr.Snapshot() {
		if s.Model == "zen/pinned" {
			pinnedSeen = true
			if s.Discovered {
				t.Error("hand-written entry must not be flagged discovered")
			}
		}
		if s.Model == "zen/claude-opus-5" {
			if !s.Discovered || s.State != Available {
				t.Errorf("discovered entry in Snapshot: %+v", s)
			}
		}
	}
	if !pinnedSeen {
		t.Error("Snapshot lost the hand-written entry")
	}

	// The provider drops the id: one miss keeps it, two retire it.
	fp.set(zenRoot, "pinned")
	tr.RefreshInventory(context.Background())
	if _, ok := tr.Discovered()["zen/claude-opus-5"]; !ok {
		t.Error("one miss must not retire a discovered id")
	}
	tr.RefreshInventory(context.Background())
	if _, ok := tr.Discovered()["zen/claude-opus-5"]; ok {
		t.Error("two consecutive misses must retire a discovered id")
	}
	// A fetch failure in between is not a miss.
	fp.set(zenRoot, "pinned", "claude-opus-5")
	tr.RefreshInventory(context.Background())
	fp.set(zenRoot, "pinned")
	tr.RefreshInventory(context.Background()) // miss 1
	fp.fail[zenRoot] = errors.New("502")
	tr.RefreshInventory(context.Background()) // failure: no evidence
	if _, ok := tr.Discovered()["zen/claude-opus-5"]; !ok {
		t.Error("a failed fetch must not count as a miss")
	}
}

func TestInventory_AllowlistLimitsAdoptionAndFollowsChanges(t *testing.T) {
	fp := newFakeProviders()
	fp.set(seatRoot, "Qwen/Qwen3.5-122B")
	fp.set(zenRoot, "pinned")
	ids := make([]string, 0, 300)
	for i := 0; i < 290; i++ {
		ids = append(ids, fmt.Sprintf("vendor%d/model-%d", i%17, i))
	}
	ids = append(ids,
		"anthropic/claude-opus-5", "anthropic/claude-sonnet-5",
		"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash",
		"z-ai/glm-5.3-flash", "moonshotai/kimi-k3", "qwen/qwen3.8-max",
		"qwen/qwen3.7-plus", "vendor/old-model", "anthropic/claude",
		"deepseek/deepseek-v4-pro:batch", "moonshotai/kimi-k3:batch")
	fp.set(orRoot, ids...)
	tr, reg := newInventoryTracker(t, fp, nil, nil)

	poll(t, tr)
	got := keys(tr.Discovered())
	// anthropic/claude-opus-5 and -sonnet-5 match the allowlist but are
	// already served by hand-written entries at this base (one of them
	// disabled on purpose), so neither is adopted under a second name; the
	// :batch variants match too and are vetoed by exclude.
	want := []string{
		"or/deepseek/deepseek-v4-flash", "or/deepseek/deepseek-v4-pro",
		"or/moonshotai/kimi-k3", "or/qwen/qwen3.8-max", "or/z-ai/glm-5.3-flash",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("adopted = %v\nwant     %v", got, want)
	}

	// Tighten the policy: what no longer matches is retired on the next two
	// polls, exactly like an id the provider dropped.
	reg.Discovery[1].Adopt = config.AdoptPolicy{Patterns: []string{"deepseek/*"}}
	tr.RefreshInventory(context.Background())
	tr.RefreshInventory(context.Background())
	got = keys(tr.Discovered())
	if strings.Join(got, ",") != "or/deepseek/deepseek-v4-flash,or/deepseek/deepseek-v4-pro" {
		t.Errorf("after narrowing the allowlist: %v", got)
	}
	// Widen it: adopted on the very next poll.
	reg.Discovery[1].Adopt = config.AdoptPolicy{Patterns: []string{"deepseek/*", "qwen/*"}}
	tr.RefreshInventory(context.Background())
	if _, ok := tr.Discovered()["or/qwen/qwen3.7-plus"]; !ok {
		t.Errorf("widened allowlist should adopt on the next poll: %v", keys(tr.Discovered()))
	}
}

func TestInventory_SlowProviderDoesNotDelayOthersOrThePoll(t *testing.T) {
	fp := newFakeProviders()
	fp.set(seatRoot, "Qwen/Qwen3.5-122B")
	fp.set(zenRoot, "pinned", "fresh")
	fp.hang[orRoot] = true // never answers; the 200 ms timeout ends it
	tr, _ := newInventoryTracker(t, fp, nil, nil)

	start := time.Now()
	tr.PollOnce(context.Background())
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("PollOnce took %s: a listing fetch must never be on the poll's critical path", d)
	}
	// The fast provider's result is usable while the slow one is still
	// hanging.
	deadline := time.Now().Add(150 * time.Millisecond)
	for {
		if _, ok := tr.Discovered()["zen/fresh"]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fast provider's listing should apply while the slow one hangs")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !tr.Routable("seat") {
		t.Errorf("request path must not be affected: %s", tr.Reason("seat"))
	}
	tr.WaitInventory()
	for _, b := range tr.Inventory(false) {
		if b.Base == orRoot && (b.Error == "" || b.FetchedAt != nil) {
			t.Errorf("hung base should record the timeout and no listing: %+v", b)
		}
	}
}

func TestInventory_IntervalGatesRefresh(t *testing.T) {
	fp := newFakeProviders()
	fp.set(seatRoot, "Qwen/Qwen3.5-122B")
	fp.set(zenRoot, "pinned")
	fp.set(orRoot, "vendor/old-model")
	clock := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	tr, _ := newInventoryTracker(t, fp, now, func(c *Config) { c.InventoryInterval = time.Minute })

	poll(t, tr)
	poll(t, tr)
	poll(t, tr)
	if n := fp.count(zenRoot); n != 1 {
		t.Errorf("three polls inside one interval should fetch once, got %d", n)
	}
	clock = clock.Add(61 * time.Second)
	poll(t, tr)
	if n := fp.count(zenRoot); n != 2 {
		t.Errorf("past the interval a poll should fetch again, got %d", n)
	}
	// Disabled: no fetches, nothing absent, nothing discovered.
	fp3 := newFakeProviders()
	tr3, _ := newInventoryTracker(t, fp3, nil, func(c *Config) { c.DisableInventory = true })
	poll(t, tr3)
	if fp3.count(zenRoot) != 0 || len(tr3.Inventory(true)) != 0 || tr3.Discovered() != nil {
		t.Error("DisableInventory must leave the tracker exactly as before")
	}
}

func TestInventory_BreakerStillGatesDiscoveredEntries(t *testing.T) {
	fp := newFakeProviders()
	fp.set(seatRoot, "Qwen/Qwen3.5-122B")
	fp.set(zenRoot, "pinned", "flaky")
	fp.set(orRoot, "vendor/old-model")
	tr, _ := newInventoryTracker(t, fp, nil, nil)
	poll(t, tr)
	if !tr.Routable("zen/flaky") {
		t.Fatal("discovered entry should be routable")
	}
	for i := 0; i < DefaultBreakerTrip; i++ {
		tr.ReportFailure("zen/flaky", errors.New("upstream status 502"))
	}
	if tr.Routable("zen/flaky") {
		t.Error("the passive breaker must apply to discovered entries exactly like hand-written ones")
	}
	tr.ReportSuccess("zen/flaky")
	if !tr.Routable("zen/flaky") {
		t.Error("breaker should close on success")
	}
}

func TestInventoryEnabled(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		m    config.ModelDefinition
		want bool
	}{
		{"chat local", config.ModelDefinition{Enabled: true, Node: "n", APIClass: config.APIClassChat}, true},
		{"embeddings external", config.ModelDefinition{Enabled: true, Backend: config.BackendExternal, APIBase: "http://x/v1", APIClass: config.APIClassEmbeddings}, true},
		{"rerank", config.ModelDefinition{Enabled: true, Node: "n", APIClass: config.APIClassRerank}, true},
		{"image_gen", config.ModelDefinition{Enabled: true, Node: "n", APIClass: config.APIClassImageGen}, false},
		{"tts", config.ModelDefinition{Enabled: true, Node: "n", APIClass: config.APIClassTTS}, false},
		{"anthropic", config.ModelDefinition{Enabled: true, Backend: config.BackendExternal, APIBase: "http://x/v1", APIClass: config.APIClassAnthropic}, false},
		{"disabled", config.ModelDefinition{Enabled: false, Node: "n", APIClass: config.APIClassChat}, false},
		{"virtual chain", config.ModelDefinition{Enabled: true, Fallbacks: []string{"a"}, APIClass: config.APIClassChat}, false},
		{"opt out", config.ModelDefinition{Enabled: true, Node: "n", APIClass: config.APIClassChat, Health: &config.ModelHealth{Inventory: &no}}, false},
		{"opt in media", config.ModelDefinition{Enabled: true, Node: "n", APIClass: config.APIClassTTS, Health: &config.ModelHealth{Inventory: &yes}}, true},
	}
	for _, tc := range cases {
		if got := InventoryEnabled(tc.m); got != tc.want {
			t.Errorf("%s: InventoryEnabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFetchListing_ParsesPlainAndAnnotatedShapes(t *testing.T) {
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("X-Api-Key"))
		switch r.URL.Path {
		case "/zen/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"claude-opus-5","object":"model"},{"id":"big-pickle"},{"object":"model"}]}`)
		case "/api/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"anthropic/claude-opus-5","context_length":200000,
			  "pricing":{"prompt":"0.000003","completion":"0.000015"},
			  "top_provider":{"max_completion_tokens":32000},
			  "architecture":{"input_modalities":["text","image"]},
			  "supported_parameters":["tools","max_tokens"]},
			 {"id":"openrouter/auto","pricing":{"prompt":"-1","completion":"-1"}},
			 {"id":"num/priced","pricing":{"prompt":0.000001,"completion":0.000002}}]}`)
		case "/down/v1/models":
			http.Error(w, `{"error":"nope"}`, http.StatusBadGateway)
		case "/junk/v1/models":
			_, _ = io.WriteString(w, `not json`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	l, err := FetchListing(ctx, srv.URL+"/zen/v1", "tok", "")
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if len(l) != 2 || l["claude-opus-5"].ID != "claude-opus-5" {
		t.Errorf("plain listing = %v (entries without an id are skipped)", keysL(l))
	}
	if gotAuth.Load() != "Bearer tok|" {
		t.Errorf("bearer not sent: %q", gotAuth.Load())
	}

	l, err = FetchListing(ctx, srv.URL+"/api", "tok", "X-Api-Key")
	if err != nil {
		t.Fatalf("annotated: %v", err)
	}
	if gotAuth.Load() != "|tok" {
		t.Errorf("custom header not honoured: %q", gotAuth.Load())
	}
	c := l["anthropic/claude-opus-5"]
	if c.ContextLength != 200000 || c.MaxOutputTokens != 32000 || !c.Vision || !c.ToolCalling {
		t.Errorf("annotated fields: %+v", c)
	}
	if c.InputCostPerMillion == nil || *c.InputCostPerMillion != 3 || *c.OutputCostPerMillion != 15 {
		t.Errorf("pricing should be per-million: %v %v", c.InputCostPerMillion, c.OutputCostPerMillion)
	}
	if a := l["openrouter/auto"]; a.InputCostPerMillion != nil {
		t.Errorf("negative (dynamic) pricing must read as unknown, got %v", *a.InputCostPerMillion)
	}
	if n := l["num/priced"]; n.InputCostPerMillion == nil || *n.InputCostPerMillion != 1 || *n.OutputCostPerMillion != 2 {
		t.Errorf("numeric pricing: %+v", n)
	}

	if _, err := FetchListing(ctx, srv.URL+"/down", "", ""); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("non-200 should error with the status, got %v", err)
	}
	if _, err := FetchListing(ctx, srv.URL+"/junk", "", ""); err == nil {
		t.Error("undecodable body should error")
	}
	if _, err := FetchListing(ctx, srv.URL+"/missing/v1", "", ""); err == nil {
		t.Error("404 should error")
	}
}

func containsTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func keys(m map[string]config.ModelDefinition) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysL(l Listing) []string {
	out := make([]string, 0, len(l))
	for k := range l {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
