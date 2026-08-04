package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

const trackerYAML = `
nodes:
  hypatia: {host: hypatia.local, gpu: nvidia, vram_gb: 128}
  delphi: {host: delphi.local, gpu: amd, vram_gb: 64}
models:
  qwen3.6-hypatia:
    hf_repo: Qwen/Qwen3.6-35B
    node: hypatia
  # node-pinned external: the agent does not manage it, but it dies with delphi
  flux-dev:
    hf_repo: FLUX.1-dev
    backend: external
    node: delphi
    api_base: http://delphi.local:5396
    api_class: image_gen
  # nodeless external: somebody else's uptime problem
  kimi-k2.7-code:
    hf_repo: kimi-k2.7-code
    backend: external
    api_base: https://opencode.ai/zen/go/v1
`

// fakeFleet lets a test flip node reachability and per-model agent state.
type fakeFleet struct {
	down   map[string]bool              // node -> unreachable
	states map[string]map[string]string // node -> model_id -> state
}

func (f *fakeFleet) probe(hostToNode map[string]string) ProbeFunc {
	return func(_ context.Context, host string, _ int) NodeSnapshot {
		node := hostToNode[host]
		if f.down[node] {
			return NodeSnapshot{}
		}
		snap := NodeSnapshot{Reachable: true, Health: &AgentHealth{}}
		for id, st := range f.states[node] {
			snap.Models = append(snap.Models, AgentModel{ModelID: id, State: st})
		}
		return snap
	}
}

func newTestTracker(t *testing.T, fleet *fakeFleet, now func() time.Time) (*Tracker, *config.ModelRegistry) {
	t.Helper()
	reg, err := config.LoadBytes([]byte(trackerYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	hostToNode := map[string]string{}
	for name, n := range reg.Nodes {
		hostToNode[n.Host] = name
	}
	if now == nil {
		now = time.Now
	}
	tr := NewTracker(Config{
		Registry: reg,
		Probe:    fleet.probe(hostToNode),
		Now:      now,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return tr, reg
}

func TestRoutableBeforeFirstPoll(t *testing.T) {
	tr, _ := newTestTracker(t, &fakeFleet{}, nil)
	// A just-started router has no evidence against anything and must not
	// refuse traffic for a whole poll interval.
	if !tr.Routable("qwen3.6-hypatia") {
		t.Errorf("model should be routable before the first poll")
	}
	if state, _, _ := tr.State("qwen3.6-hypatia"); state != Unknown {
		t.Errorf("state = %q, want unknown before first poll", state)
	}
}

func TestNodeDownMarksItsModelsDown(t *testing.T) {
	fleet := &fakeFleet{down: map[string]bool{}}
	tr, _ := newTestTracker(t, fleet, nil)
	ctx := context.Background()

	tr.PollOnce(ctx)
	if !tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("expected available with all nodes up")
	}

	fleet.down["hypatia"] = true

	// Hysteresis: one failure must NOT reassign traffic.
	tr.PollOnce(ctx)
	if !tr.Routable("qwen3.6-hypatia") {
		t.Errorf("one failed poll should not mark the model down (DownAfter=%d)", DefaultDownAfter)
	}

	tr.PollOnce(ctx)
	if tr.Routable("qwen3.6-hypatia") {
		t.Errorf("model should be down after %d consecutive failed polls", DefaultDownAfter)
	}
	_, source, reason := tr.State("qwen3.6-hypatia")
	if source != SourcePoll {
		t.Errorf("source = %q, want poll", source)
	}
	if reason == "" {
		t.Errorf("expected a reason naming the unreachable node")
	}

	// One success is enough to come back.
	fleet.down["hypatia"] = false
	tr.PollOnce(ctx)
	if !tr.Routable("qwen3.6-hypatia") {
		t.Errorf("one successful poll should restore availability")
	}
}

func TestNodePinnedExternalFollowsItsNode(t *testing.T) {
	fleet := &fakeFleet{down: map[string]bool{}}
	tr, _ := newTestTracker(t, fleet, nil)
	ctx := context.Background()
	tr.PollOnce(ctx)

	if !tr.Routable("flux-dev") {
		t.Fatalf("flux-dev should be available while delphi is up")
	}
	// flux is backend:external but pinned to delphi — it dies with the node.
	fleet.down["delphi"] = true
	tr.PollOnce(ctx)
	tr.PollOnce(ctx)
	if tr.Routable("flux-dev") {
		t.Errorf("node-pinned external should go down with its node")
	}
	// The nodeless external is unaffected.
	if !tr.Routable("kimi-k2.7-code") {
		t.Errorf("nodeless external should stay available when a node dies")
	}
}

func TestAgentReportsModelStopped(t *testing.T) {
	fleet := &fakeFleet{
		down: map[string]bool{},
		states: map[string]map[string]string{
			"hypatia": {"qwen3.6-hypatia": StateRunning},
		},
	}
	tr, _ := newTestTracker(t, fleet, nil)
	ctx := context.Background()
	tr.PollOnce(ctx)
	if !tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("running model should be available")
	}

	// Node still answering, but the backend is stopped.
	fleet.states["hypatia"]["qwen3.6-hypatia"] = "stopped"
	tr.PollOnce(ctx)
	tr.PollOnce(ctx)
	if tr.Routable("qwen3.6-hypatia") {
		t.Errorf("model should be down when the agent reports it stopped")
	}
}

func TestUnlistedModelOnReachableNodeStaysUp(t *testing.T) {
	// The agent lists only what it manages. Silence about flux-dev must not
	// be read as "stopped" — delphi being reachable is the whole signal.
	fleet := &fakeFleet{
		down:   map[string]bool{},
		states: map[string]map[string]string{"delphi": {"something-else": StateRunning}},
	}
	tr, _ := newTestTracker(t, fleet, nil)
	tr.PollOnce(context.Background())
	if !tr.Routable("flux-dev") {
		t.Errorf("model unlisted by a reachable agent should stay available")
	}
}

func TestCircuitBreakerTripsAndHalfOpens(t *testing.T) {
	clock := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	fleet := &fakeFleet{down: map[string]bool{}}
	tr, _ := newTestTracker(t, fleet, now)
	tr.PollOnce(context.Background())

	boom := errors.New("dial tcp: connection refused")
	for i := 0; i < DefaultBreakerTrip-1; i++ {
		tr.ReportFailure("qwen3.6-hypatia", boom)
	}
	if !tr.Routable("qwen3.6-hypatia") {
		t.Errorf("breaker should not trip before %d failures", DefaultBreakerTrip)
	}

	tr.ReportFailure("qwen3.6-hypatia", boom)
	if tr.Routable("qwen3.6-hypatia") {
		t.Errorf("breaker should be open after %d failures", DefaultBreakerTrip)
	}
	// The breaker outranks a healthy poll: the poller only proves the agent
	// answers, the breaker proves real requests fail.
	tr.PollOnce(context.Background())
	if tr.Routable("qwen3.6-hypatia") {
		t.Errorf("a healthy poll must not close an open breaker")
	}
	if _, source, _ := tr.State("qwen3.6-hypatia"); source != SourcePassive {
		t.Errorf("source = %q, want passive while the breaker is open", source)
	}

	// After the cooldown, exactly one probe is admitted.
	clock = clock.Add(DefaultBreakerCooldown + time.Second)
	if !tr.Routable("qwen3.6-hypatia") {
		t.Errorf("breaker should half-open after the cooldown")
	}

	tr.ReportSuccess("qwen3.6-hypatia")
	if !tr.Routable("qwen3.6-hypatia") {
		t.Errorf("a success should close the breaker")
	}
	if _, source, _ := tr.State("qwen3.6-hypatia"); source != SourcePoll {
		t.Errorf("source = %q, want poll after the breaker closes", source)
	}
}

func TestSnapshotCoversEveryModel(t *testing.T) {
	fleet := &fakeFleet{down: map[string]bool{"hypatia": true}}
	tr, reg := newTestTracker(t, fleet, nil)
	ctx := context.Background()
	tr.PollOnce(ctx)
	tr.PollOnce(ctx)

	snap := tr.Snapshot()
	if len(snap) != len(reg.Models) {
		t.Fatalf("snapshot has %d entries, want %d", len(snap), len(reg.Models))
	}
	byID := map[string]Status{}
	for _, s := range snap {
		byID[s.Model] = s
	}
	if got := byID["qwen3.6-hypatia"].State; got != Unavailable {
		t.Errorf("qwen3.6-hypatia state = %q, want unavailable", got)
	}
	if got := byID["kimi-k2.7-code"].State; got != Available {
		t.Errorf("kimi-k2.7-code state = %q, want available", got)
	}

	nodes := tr.NodeStatuses()
	if len(nodes) != len(reg.Nodes) {
		t.Fatalf("node statuses = %d, want %d", len(nodes), len(reg.Nodes))
	}
	for _, n := range nodes {
		if n.Node == "hypatia" && n.Reachable {
			t.Errorf("hypatia should be unreachable")
		}
	}
}

func TestMultiNodeNeedsEveryNode(t *testing.T) {
	reg, err := config.LoadBytes([]byte(`
nodes:
  a: {host: a.local, gpu: nvidia, vram_gb: 128}
  b: {host: b.local, gpu: nvidia, vram_gb: 128}
models:
  tp-pair:
    hf_repo: x/y
    multi_node: {nodes: [a, b], tensor_parallel_size: 2}
`))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	fleet := &fakeFleet{down: map[string]bool{"b": true}}
	tr := NewTracker(Config{
		Registry: reg,
		Probe:    fleet.probe(map[string]string{"a.local": "a", "b.local": "b"}),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx := context.Background()
	tr.PollOnce(ctx)
	tr.PollOnce(ctx)
	if tr.Routable("tp-pair") {
		t.Errorf("a tensor-parallel model should be down when any of its nodes is")
	}
}
