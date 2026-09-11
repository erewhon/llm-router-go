package router

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/health"
)

const pressureYAML = `
nodes:
  n1: {host: n1.local, gpu: nvidia, vram_gb: 16}
  n2: {host: n2.local, gpu: nvidia, vram_gb: 16}
  n3: {host: n3.local, gpu: nvidia, vram_gb: 16}
models:
  seatA: {hf_repo: A, node: n1, api_port: 5391, capabilities: [text, tool_calling]}
  seatB: {hf_repo: B, node: n2, api_port: 5391, capabilities: [text, tool_calling]}
  seatC: {hf_repo: C, node: n3, api_port: 5391, capabilities: [text, tool_calling]}
  chainhead:
    hf_repo: ch
    fallbacks: [seatA, seatB]
    capabilities: [text, tool_calling]
roles:
  balanced:
    require: {locality: local, capabilities: [text, tool_calling]}
    balance: pressure
    balance_groups: [[seatA, seatB]]
    candidates: [seatA, seatB, seatC]
  ranked:
    require: {locality: local}
    balance: pressure
    candidates: [seatA, seatB]
  plain:
    require: {locality: local}
    candidates: [seatA, seatB]
  chainrole:
    require: {capabilities: [text, tool_calling]}
    balance: pressure
    candidates: [chainhead]
`

func newPressureRouter(t *testing.T, down map[string]bool, extra ...Option) (*Router, *stubAvailability) {
	t.Helper()
	reg, err := config.LoadBytes([]byte(pressureYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if down == nil {
		down = map[string]bool{}
	}
	avail := &stubAvailability{down: down}
	opts := append([]Option{WithAvailability(avail), WithFlushInterval(0)}, extra...)
	rt := New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
	return rt, avail
}

// setPressures fakes each seat's load via the agent-reported table: pressure
// equals Running here (no inflight, no gpu), which is what the balancer reads.
func setPressures(rt *Router, p map[string]int) {
	loads := map[string]health.SeatLoad{}
	for id, n := range p {
		loads[id] = health.SeatLoad{Running: n, GPUBusyPct: -1}
	}
	rt.pressure.setLoads(loads)
}

func resolve(t *testing.T, rt *Router, model string) resolveResult {
	t.Helper()
	res, err := rt.resolveModel(model, false, 0, tierNone)
	if err != nil {
		t.Fatalf("resolveModel(%q): %v", model, err)
	}
	return res
}

// Acceptance 1: two equal-rank routable seats with pressure 2 and 0 → the 0
// seat is chosen.
func TestEqualRankPicksTheLeastLoadedSeat(t *testing.T) {
	rt, _ := newPressureRouter(t, nil)
	setPressures(rt, map[string]int{"seatA": 2, "seatB": 0})
	if got := resolve(t, rt, "balanced").ModelID; got != "seatB" {
		t.Fatalf("balanced resolved to %q, want seatB (pressure 0 < seatA 2)", got)
	}
	// Flip it: seatA idle, seatB loaded → seatA.
	setPressures(rt, map[string]int{"seatA": 0, "seatB": 3})
	if got := resolve(t, rt, "balanced").ModelID; got != "seatA" {
		t.Fatalf("after flip, balanced resolved to %q, want seatA", got)
	}
}

// Equal pressure keeps declared order (stable).
func TestEqualPressureKeepsDeclaredOrder(t *testing.T) {
	rt, _ := newPressureRouter(t, nil)
	setPressures(rt, map[string]int{"seatA": 1, "seatB": 1})
	if got := resolve(t, rt, "balanced").ModelID; got != "seatA" {
		t.Fatalf("tie resolved to %q, want seatA (declared first)", got)
	}
}

// Unknown telemetry contributes 0: with nothing reported, a balanced role
// falls back to declared order.
func TestUnknownTelemetryIsZeroPressure(t *testing.T) {
	rt, _ := newPressureRouter(t, nil) // no setPressures at all
	if got := resolve(t, rt, "balanced").ModelID; got != "seatA" {
		t.Fatalf("with no telemetry, resolved to %q, want seatA", got)
	}
}

// Acceptance 2: rank override fires ONLY when the top seat is jammed (>=3) and
// a lower-ranked seat is idle (<=1).
func TestRankOverrideOnlyAtTheExtreme(t *testing.T) {
	// ranked role: seatA rank 0, seatB rank 1 (no group).
	cases := []struct {
		name   string
		pA, pB int
		want   string
	}{
		{"normal: rank wins", 2, 0, "seatA"},                // A not jammed → rank holds
		{"jammed head, idle tail: override", 3, 1, "seatB"}, // the one override case
		{"jammed head, tail=2: no override", 3, 2, "seatA"}, // tail not idle enough
		{"head=2, tail=0: no override", 2, 0, "seatA"},      // head not jammed enough
		{"both jammed: rank holds", 4, 3, "seatA"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt, _ := newPressureRouter(t, nil)
			setPressures(rt, map[string]int{"seatA": c.pA, "seatB": c.pB})
			if got := resolve(t, rt, "ranked").ModelID; got != c.want {
				t.Fatalf("pA=%d pB=%d → %q, want %q", c.pA, c.pB, got, c.want)
			}
		})
	}
}

// Acceptance 3: chains are failover lists, never pressure-balanced. A role
// whose candidate is a chain keeps the chain's provider order regardless of
// load.
func TestChainsAreNotPressureReordered(t *testing.T) {
	rt, _ := newPressureRouter(t, nil)
	// seatA (chain's first) hammered, seatB idle: a balanced *rank* would pick
	// seatB, but a chain must still try its first provider first.
	setPressures(rt, map[string]int{"seatA": 5, "seatB": 0})
	res := resolve(t, rt, "chainrole")
	if res.ModelID != "seatA" {
		t.Fatalf("chain resolved to %q, want seatA (chain order, not pressure)", res.ModelID)
	}
	if res.Chain != "chainhead" {
		t.Fatalf("chain label = %q, want chainhead", res.Chain)
	}
}

// No change for roles without balance: pressure.
func TestOrderRoleIgnoresPressure(t *testing.T) {
	rt, _ := newPressureRouter(t, nil)
	setPressures(rt, map[string]int{"seatA": 9, "seatB": 0})
	if got := resolve(t, rt, "plain").ModelID; got != "seatA" {
		t.Fatalf("order role resolved to %q, want seatA (pressure ignored)", got)
	}
	// And it carries no pressure header note.
	if res := resolve(t, rt, "plain"); res.PressureNote != "" {
		t.Fatalf("order role should not set a pressure note, got %q", res.PressureNote)
	}
}

// A warming/unroutable seat is skipped and sinks below routable peers in its
// rank even when its raw pressure is lowest.
func TestUnroutableSeatSinksWithinRank(t *testing.T) {
	rt, _ := newPressureRouter(t, map[string]bool{"seatB": true}) // seatB down
	setPressures(rt, map[string]int{"seatA": 2, "seatB": 0})      // B idler but down
	if got := resolve(t, rt, "balanced").ModelID; got != "seatA" {
		t.Fatalf("resolved to %q, want seatA (seatB is down despite lower pressure)", got)
	}
}

// The X-Router-Pressure header reports the balanced candidates and their load.
func TestPressureHeaderOnBalancedRole(t *testing.T) {
	rt, _ := newPressureRouter(t, nil, WithTransport(&deadBackends{}))
	setPressures(rt, map[string]int{"seatA": 2, "seatB": 0})
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()
	resp := postChatHTTP(t, srv.URL, `{"model":"balanced","messages":[],"max_tokens":1}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	got := resp.Header.Get("X-Router-Pressure")
	if !strings.Contains(got, "seatB=0") || !strings.Contains(got, "seatA=2") {
		t.Fatalf("X-Router-Pressure = %q, want it to name both seats and their pressures", got)
	}
	if resp.Header.Get("X-Router-Resolved") != "seatB" {
		t.Fatalf("X-Router-Resolved = %q, want seatB (least loaded)", resp.Header.Get("X-Router-Resolved"))
	}
}

func TestPressureConfigBucketsAndWindow(t *testing.T) {
	var p config.PressureConfig // zero value → defaults
	if p.Window() != config.DefaultPressureWindowS {
		t.Errorf("default window = %d", p.Window())
	}
	for pct, want := range map[int]int{-1: 0, 0: 0, 39: 0, 40: 1, 69: 1, 70: 2, 84: 2, 85: 3, 100: 3} {
		if got := p.GPUBucket(pct); got != want {
			t.Errorf("GPUBucket(%d) = %d, want %d", pct, got, want)
		}
	}
	custom := config.PressureConfig{WindowS: 30, GPUBuckets: []int{50}}
	if custom.Window() != 30 || custom.GPUBucket(49) != 0 || custom.GPUBucket(50) != 1 {
		t.Errorf("custom config not honoured: window=%d b49=%d b50=%d", custom.Window(), custom.GPUBucket(49), custom.GPUBucket(50))
	}
}
