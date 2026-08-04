package health

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

// Availability is the routing-relevant verdict for one model.
type Availability string

const (
	// Available means route here.
	Available Availability = "available"
	// Unavailable means skip: the node is unreachable, the backend is stopped,
	// or the circuit breaker is open.
	Unavailable Availability = "unavailable"
	// Unknown means nothing has been observed yet. Treated as available by
	// Routable — a router that has just started must not refuse everything for
	// its first poll interval.
	Unknown Availability = "unknown"
)

// Source records which signal produced a verdict, so an operator reading
// /v1/availability can tell a powered-down node from a wedged backend.
type Source string

const (
	// SourcePoll is the node-agent poller.
	SourcePoll Source = "poll"
	// SourcePassive is the circuit breaker fed by real proxy failures.
	SourcePassive Source = "passive"
	// SourceAssumed is a nodeless external (Zen, the Anthropic gateway):
	// nothing to poll, so it is available until the breaker says otherwise.
	SourceAssumed Source = "assumed"
)

// Status is the public view of one model's availability.
type Status struct {
	Model  string       `json:"model"`
	State  Availability `json:"state"`
	Reason string       `json:"reason,omitempty"`
	Source Source       `json:"source"`
	Node   string       `json:"node,omitempty"`
	// Since is when State last changed.
	Since time.Time `json:"since"`
	// ExpectedDown marks a model whose node is outside its declared schedule,
	// so a dashboard can render planned downtime as planned rather than broken.
	ExpectedDown bool `json:"expected_down,omitempty"`
}

// NodeStatus is the public view of one node's reachability.
type NodeStatus struct {
	Node         string    `json:"node"`
	Host         string    `json:"host"`
	Reachable    bool      `json:"reachable"`
	ProbedAt     time.Time `json:"probed_at"`
	ExpectedDown bool      `json:"expected_down,omitempty"`
}

// Defaults for Config. Chosen so a single dropped probe never reassigns a
// role, but a genuinely powered-down node is noticed within ~30s.
const (
	DefaultInterval        = 15 * time.Second
	DefaultDownAfter       = 2
	DefaultBreakerTrip     = 3
	DefaultBreakerCooldown = 60 * time.Second
)

// Config configures a Tracker. The zero value of each field falls back to the
// Default* constant above.
type Config struct {
	Registry *config.ModelRegistry
	Interval time.Duration
	// DownAfter is how many consecutive failed observations flip a model to
	// Unavailable. One success restores it immediately — we are quick to
	// forgive and slow to condemn, because a false "down" reassigns traffic
	// while a false "up" costs one retried request.
	DownAfter int
	// BreakerTrip is how many consecutive proxy failures open the breaker.
	BreakerTrip int
	// BreakerCooldown is how long the breaker stays open before admitting one
	// half-open probe.
	BreakerCooldown time.Duration
	Logger          *slog.Logger
	// Probe overrides the node prober (tests).
	Probe ProbeFunc
	// Now overrides the clock (tests).
	Now func() time.Time
	// OnPoll, if set, runs after each completed poll round. The router uses it
	// to republish its availability gauges so they track fleet state rather
	// than request traffic. Must not block for long: it runs on the poll
	// goroutine.
	OnPoll func()
}

// modelState is the tracker's per-model bookkeeping.
type modelState struct {
	// poll-derived
	pollState  Availability
	pollReason string
	pollFails  int

	// passive breaker
	breakerFails    int
	breakerOpen     bool
	breakerOpenedAt time.Time
	halfOpen        bool

	// last publicly-visible verdict, for Since bookkeeping
	lastState Availability
	since     time.Time
}

// Tracker answers "is this model routable right now". Safe for concurrent use.
type Tracker struct {
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
	probe  ProbeFunc

	mu     sync.RWMutex
	models map[string]*modelState
	nodes  map[string]NodeSnapshot
	// polled flips true after the first completed round, so Routable can
	// fail open before then.
	polled bool
}

// NewTracker builds a Tracker. It does not start polling; call Run.
func NewTracker(cfg Config) *Tracker {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.DownAfter <= 0 {
		cfg.DownAfter = DefaultDownAfter
	}
	if cfg.BreakerTrip <= 0 {
		cfg.BreakerTrip = DefaultBreakerTrip
	}
	if cfg.BreakerCooldown <= 0 {
		cfg.BreakerCooldown = DefaultBreakerCooldown
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Probe == nil {
		cfg.Probe = ProbeNode
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Tracker{
		cfg:    cfg,
		logger: cfg.Logger,
		now:    cfg.Now,
		probe:  cfg.Probe,
		models: map[string]*modelState{},
		nodes:  map[string]NodeSnapshot{},
	}
}

// SetOnPoll installs the after-each-poll callback. Separate from Config so a
// caller can wire a callback that closes over something built from the
// tracker itself (the router's metrics publisher). Call before Run.
func (t *Tracker) SetOnPoll(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cfg.OnPoll = fn
}

// Run polls until ctx is cancelled. Meant to run in its own goroutine; it
// performs one round immediately so a fresh process has real data quickly.
func (t *Tracker) Run(ctx context.Context) {
	t.PollOnce(ctx)
	tick := time.NewTicker(t.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.PollOnce(ctx)
		}
	}
}

// PollOnce probes every node concurrently and folds the result into per-model
// availability. Exported so tests (and an operator-triggered refresh) can step
// the tracker deterministically.
func (t *Tracker) PollOnce(ctx context.Context) {
	reg := t.cfg.Registry
	if reg == nil {
		return
	}

	snaps := make(map[string]NodeSnapshot, len(reg.Nodes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, node := range reg.Nodes {
		wg.Add(1)
		go func(name string, node config.NodeDefinition) {
			defer wg.Done()
			s := t.probe(ctx, node.Host, node.AgentPort)
			mu.Lock()
			snaps[name] = s
			mu.Unlock()
		}(name, node)
	}
	wg.Wait()

	t.mu.Lock()
	t.nodes = snaps
	t.polled = true
	for id, m := range reg.Models {
		ok, reason := observeModel(id, m, snaps)
		t.applyObservation(id, ok, reason)
	}
	onPoll := t.cfg.OnPoll // read under the lock; SetOnPoll may race otherwise
	t.mu.Unlock()

	// Outside the lock: OnPoll calls back into the router, which reads state
	// through the tracker's own accessors and would deadlock on a held mutex.
	if onPoll != nil {
		onPoll()
	}
}

// observeModel decides what one poll round says about one model.
//
// A model pinned to a node — whatever its backend — lives and dies with that
// node. That deliberately includes the node-pinned externals (flux-dev,
// orpheus-tts, qwen3-embedding, ui-tars-7b): they are external only in the
// sense that something other than the agent starts them, and they go away when
// delphi or euclid powers off.
//
// A nodeless external is somebody else's uptime problem; only the breaker can
// mark it down.
func observeModel(id string, m config.ModelDefinition, snaps map[string]NodeSnapshot) (bool, string) {
	nodes := modelNodes(m)
	if len(nodes) == 0 {
		return true, ""
	}
	for _, n := range nodes {
		snap, probed := snaps[n]
		if !probed {
			// Model references a node absent from the registry; config
			// validation rejects this, so treat it as down rather than guess.
			return false, fmt.Sprintf("node %q is not in the registry", n)
		}
		if !snap.Reachable {
			return false, fmt.Sprintf("node %q unreachable", n)
		}
		// Agents only list models they manage. Silence means "not mine",
		// not "stopped" — node reachability alone decides for those.
		if state, listed := snap.ModelState(id); listed && state != StateRunning {
			return false, fmt.Sprintf("agent on %q reports state %q", n, state)
		}
	}
	return true, ""
}

// modelNodes returns every node a model needs. A multi-node model needs all of
// them: losing one head or worker takes the whole tensor-parallel group down.
func modelNodes(m config.ModelDefinition) []string {
	if m.MultiNode != nil {
		return m.MultiNode.Nodes
	}
	if m.Node != "" {
		return []string{m.Node}
	}
	return nil
}

// applyObservation folds one observation into a model's state with hysteresis.
// Caller holds t.mu.
func (t *Tracker) applyObservation(id string, ok bool, reason string) {
	st := t.models[id]
	if st == nil {
		st = &modelState{pollState: Unknown, since: t.now()}
		t.models[id] = st
	}

	if ok {
		// One success is enough to come back: the cost of a premature "up" is
		// a single retried request, and the retry path handles that.
		st.pollFails = 0
		st.pollReason = ""
		if st.pollState != Available {
			t.logger.Info("model available", "model", id, "was", string(st.pollState))
		}
		st.pollState = Available
		return
	}

	st.pollFails++
	st.pollReason = reason
	if st.pollFails >= t.cfg.DownAfter && st.pollState != Unavailable {
		t.logger.Warn("model unavailable", "model", id, "reason", reason, "consecutive_failures", st.pollFails)
		st.pollState = Unavailable
	}
}

// ReportFailure records a real proxy failure against a model. Only genuine
// transport-level failures should be reported — a 400 from a healthy backend
// says nothing about availability.
func (t *Tracker) ReportFailure(modelID string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.models[modelID]
	if st == nil {
		st = &modelState{pollState: Unknown, since: t.now()}
		t.models[modelID] = st
	}
	st.halfOpen = false
	st.breakerFails++
	if st.breakerFails >= t.cfg.BreakerTrip && !st.breakerOpen {
		st.breakerOpen = true
		st.breakerOpenedAt = t.now()
		reason := ""
		if err != nil {
			reason = err.Error()
		}
		t.logger.Warn("circuit breaker opened", "model", modelID,
			"consecutive_failures", st.breakerFails, "err", reason)
	}
}

// ReportSuccess clears the breaker for a model.
func (t *Tracker) ReportSuccess(modelID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.models[modelID]
	if st == nil {
		return
	}
	if st.breakerOpen {
		t.logger.Info("circuit breaker closed", "model", modelID)
	}
	st.breakerFails = 0
	st.breakerOpen = false
	st.halfOpen = false
}

// State returns the current verdict for a model plus why.
func (t *Tracker) State(modelID string) (Availability, Source, string) {
	t.mu.Lock() // write lock: a half-open transition mutates state
	defer t.mu.Unlock()
	return t.stateLocked(modelID)
}

func (t *Tracker) stateLocked(modelID string) (Availability, Source, string) {
	st := t.models[modelID]
	if st == nil {
		if !t.polled {
			return Unknown, SourceAssumed, "not yet polled"
		}
		// Polled, but this model was never observed: it is nodeless external.
		return Available, SourceAssumed, ""
	}

	// The breaker outranks the poller: it is evidence from a real request,
	// whereas the poll only proves the agent is answering.
	if st.breakerOpen {
		if t.now().Sub(st.breakerOpenedAt) < t.cfg.BreakerCooldown {
			return Unavailable, SourcePassive, fmt.Sprintf("circuit breaker open (%d consecutive failures)", st.breakerFails)
		}
		// Cooldown elapsed: admit exactly one probe request.
		st.halfOpen = true
		return Available, SourcePassive, "circuit breaker half-open"
	}

	switch st.pollState {
	case Unavailable:
		return Unavailable, SourcePoll, st.pollReason
	case Available:
		return Available, SourcePoll, ""
	default:
		if !t.polled {
			return Unknown, SourceAssumed, "not yet polled"
		}
		return Available, SourceAssumed, ""
	}
}

// Routable is the routing predicate: Unknown counts as routable so a
// just-started router never refuses traffic it has no evidence against.
func (t *Tracker) Routable(modelID string) bool {
	state, _, _ := t.State(modelID)
	return state != Unavailable
}

// Reason returns a short human-readable explanation for a model's current
// state, for the 503 body when a role has nothing to route to.
func (t *Tracker) Reason(modelID string) string {
	state, source, reason := t.State(modelID)
	if reason == "" {
		return fmt.Sprintf("%s (%s)", state, source)
	}
	return fmt.Sprintf("%s (%s: %s)", state, source, reason)
}

// Snapshot returns every known model's status, sorted by model id.
func (t *Tracker) Snapshot() []Status {
	t.mu.Lock()
	defer t.mu.Unlock()

	reg := t.cfg.Registry
	ids := make([]string, 0, len(reg.Models))
	for id := range reg.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]Status, 0, len(ids))
	for _, id := range ids {
		state, source, reason := t.stateLocked(id)
		m := reg.Models[id]
		st := t.models[id]
		since := time.Time{}
		if st != nil {
			if st.lastState != state {
				st.lastState = state
				st.since = t.now()
			}
			since = st.since
		}
		out = append(out, Status{
			Model:        id,
			State:        state,
			Reason:       reason,
			Source:       source,
			Node:         m.Node,
			Since:        since,
			ExpectedDown: t.nodeExpectedDownLocked(m.Node),
		})
	}
	return out
}

// NodeSnapshot returns the reachability of every registry node, sorted by name.
func (t *Tracker) NodeStatuses() []NodeStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()

	names := make([]string, 0, len(t.cfg.Registry.Nodes))
	for n := range t.cfg.Registry.Nodes {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]NodeStatus, 0, len(names))
	for _, n := range names {
		node := t.cfg.Registry.Nodes[n]
		snap := t.nodes[n]
		out = append(out, NodeStatus{
			Node:         n,
			Host:         node.Host,
			Reachable:    snap.Reachable,
			ProbedAt:     snap.ProbedAt,
			ExpectedDown: t.nodeExpectedDownLocked(n),
		})
	}
	return out
}

// Nodes returns the most recent snapshot per node, for callers that want the
// raw agent payload (the dashboard).
func (t *Tracker) Nodes() map[string]NodeSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]NodeSnapshot, len(t.nodes))
	for k, v := range t.nodes {
		out[k] = v
	}
	return out
}
