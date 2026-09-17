package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
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
	// Warming means skip for now: the listing is up but no generation has
	// succeeded since the seat came up. Distinct from Unavailable so an
	// operator can tell a loading engine from a powered-down node, and so a
	// role's reasons name what is actually happening.
	Warming Availability = "warming"
	// Absent means skip: the base this entry routes to answered its listing
	// and the entry's served name was not in it — a port reassigned to a
	// different model, or a provider that retired the id. Distinct from
	// Unavailable so the dashboard and a role's reasons say "drift", not
	// "down": the node is fine, the config is wrong.
	Absent Availability = "absent"
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
	// SourceProbe is the generation probe: the listing is up but the seat has
	// not yet produced a token.
	SourceProbe Source = "probe"
	// SourceInventory is the live-listing check: the base answered and the
	// served name was not in its list.
	SourceInventory Source = "inventory"
)

// WarmingInfo is the probe-side detail attached to a Warming status.
type WarmingInfo struct {
	// Since is when the seat entered warming (came up, or was demoted).
	Since time.Time `json:"since"`
	// Attempts is how many probes have failed since Since.
	Attempts int `json:"attempts"`
	// LastError is the most recent probe failure, empty before the first
	// probe of this warm-up has run.
	LastError string `json:"last_error,omitempty"`
	// NextProbe is when the tracker will try again (subject to the poll
	// interval).
	NextProbe time.Time `json:"next_probe"`
}

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
	// Warming carries the probe detail while State is Warming.
	Warming *WarmingInfo `json:"warming,omitempty"`
	// Discovered marks an entry the inventory adopted from a provider's
	// listing rather than one written in models.yaml.
	Discovered bool `json:"discovered,omitempty"`
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

	// GenerationProbe overrides the generation prober (tests). nil means
	// ProbeGeneration, the real one.
	GenerationProbe GenProbeFunc
	// DisableGenerationProbe turns the warming stage off fleet-wide: a seat
	// is routable as soon as its listing is up, the pre-probe behaviour.
	DisableGenerationProbe bool
	// ProbeTimeout bounds one generation probe. Zero means DefaultProbeTimeout.
	ProbeTimeout time.Duration
	// ProbeFilter, if set, restricts probing to the models it accepts — the
	// router passes its mode-filtered active set so an out-of-mode seat's
	// dead port is never probed. nil probes every eligible registry model.
	ProbeFilter func(modelID string) bool
	// Getenv resolves a node-pinned external's api_key for its probe. nil
	// means os.Getenv.
	Getenv func(string) string

	// InventoryInterval is how often each upstream base's /v1/models is
	// refreshed. Zero means DefaultInventoryInterval.
	InventoryInterval time.Duration
	// InventoryTimeout bounds one listing fetch. Zero means
	// DefaultInventoryTimeout.
	InventoryTimeout time.Duration
	// DisableInventory turns the live-listing check and discovery off: no
	// entry is ever absent, nothing is ever adopted — the pre-inventory
	// behaviour.
	DisableInventory bool
	// List overrides the listing fetcher (tests). nil means FetchListing.
	List ListFunc
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

	// generation probe. probe is whether it applies to this model at all;
	// confirmed is whether a generation has succeeded since the seat last
	// came up. probeSeq is bumped whenever the seat re-enters warming so a
	// probe launched against the previous warm-up cannot report into this one.
	probe        bool
	confirmed    bool
	probeSeq     uint64
	probeFails   int
	probeErr     string
	probeNextAt  time.Time
	warmingSince time.Time

	// inventory: absent is set when the entry's base answered its listing
	// without the served name; absentReason says what it listed instead.
	absent       bool
	absentReason string

	// last publicly-visible verdict, for Since bookkeeping
	lastState Availability
	since     time.Time
}

// Tracker answers "is this model routable right now". Safe for concurrent use.
type Tracker struct {
	cfg      Config
	logger   *slog.Logger
	now      func() time.Time
	probe    ProbeFunc
	genProbe GenProbeFunc
	getenv   func(string) string
	list     ListFunc

	mu     sync.RWMutex
	models map[string]*modelState
	nodes  map[string]NodeSnapshot
	// polled flips true after the first completed round, so Routable can
	// fail open before then.
	polled bool

	// inventory (see inventory.go): one entry per distinct upstream root,
	// the adopted entries, and the names a discovered id may never shadow.
	// invWG tracks in-flight fetches so tests and RefreshInventory can wait
	// for a round.
	inv        map[string]*baseInventory
	discovered map[string]*discoveredEntry
	reserved   map[string]bool
	invWG      sync.WaitGroup
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
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = DefaultProbeTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Probe == nil {
		cfg.Probe = ProbeNode
	}
	if cfg.GenerationProbe == nil {
		cfg.GenerationProbe = ProbeGeneration
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.InventoryInterval <= 0 {
		cfg.InventoryInterval = DefaultInventoryInterval
	}
	if cfg.InventoryTimeout <= 0 {
		cfg.InventoryTimeout = DefaultInventoryTimeout
	}
	if cfg.List == nil {
		cfg.List = FetchListing
	}
	t := &Tracker{
		cfg:      cfg,
		logger:   cfg.Logger,
		now:      cfg.Now,
		probe:    cfg.Probe,
		genProbe: cfg.GenerationProbe,
		getenv:   cfg.Getenv,
		list:     cfg.List,
		models:   map[string]*modelState{},
		nodes:    map[string]NodeSnapshot{},
	}
	t.buildInventory()
	return t
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

// PollOnce probes every node concurrently, folds the result into per-model
// availability, then runs whatever generation probes are due against the
// seats the listing says are up, and kicks off whatever listing fetches are
// due (those complete on their own; see startInventory). Exported so tests
// (and an operator-triggered refresh) can step the tracker deterministically.
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
	for id, m := range reg.Models {
		ok, reason := observeModel(id, m, snaps)
		t.applyObservation(id, ok, reason)
	}
	due := t.dueProbesLocked()
	invDue := t.dueInventoryLocked()
	t.mu.Unlock()

	// Listing fetches are fire-and-forget from the round's point of view:
	// each applies itself when it lands, so a provider that takes the whole
	// timeout delays only its own base.
	t.startInventory(ctx, invDue)

	// Generation probes run outside the lock — each may take up to
	// ProbeTimeout — and concurrently, so one cold seat bounds the round at
	// one timeout rather than N. Node observations above are already
	// published, so a powered-down node is noticed even while a probe waits.
	results := t.runProbes(ctx, due)

	t.mu.Lock()
	for _, r := range results {
		t.applyProbeLocked(r)
	}
	t.polled = true
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
	head := modelHead(m)
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
		// A tensor-parallel group serves its API from the head alone; the
		// worker ranks run headless, so their agents see no listener and
		// would call a healthy group "stopped". Workers count for
		// reachability only — if one dies the NCCL group dies with it and
		// the head's agent reports that.
		if head != "" && n != head {
			continue
		}
		// Agents only list models they manage. Silence means "not mine",
		// not "stopped" — node reachability alone decides for those.
		if state, listed := snap.ModelState(id); listed && state != StateRunning {
			return false, fmt.Sprintf("agent on %q reports state %q", n, state)
		}
	}
	return true, ""
}

// modelHead names the node whose agent speaks for a multi-node model's state
// (multi_node.head_node, else the first listed node); "" for single-node and
// nodeless models.
func modelHead(m config.ModelDefinition) string {
	if m.MultiNode == nil {
		return ""
	}
	if m.MultiNode.HeadNode != "" {
		return m.MultiNode.HeadNode
	}
	if len(m.MultiNode.Nodes) > 0 {
		return m.MultiNode.Nodes[0]
	}
	return ""
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

// stateFor returns the bookkeeping for a model, creating it — with the probe
// eligibility decided from the registry — on first sight. Caller holds t.mu.
func (t *Tracker) stateFor(id string) *modelState {
	st := t.models[id]
	if st != nil {
		return st
	}
	st = &modelState{pollState: Unknown, since: t.now()}
	if reg := t.cfg.Registry; reg != nil && !t.cfg.DisableGenerationProbe {
		if m, ok := reg.Models[id]; ok && ProbeEnabled(m) {
			st.probe = t.cfg.ProbeFilter == nil || t.cfg.ProbeFilter(id)
		}
	}
	t.models[id] = st
	return st
}

// applyObservation folds one observation into a model's state with hysteresis.
// Caller holds t.mu.
func (t *Tracker) applyObservation(id string, ok bool, reason string) {
	st := t.stateFor(id)

	if ok {
		// One success is enough to come back: the cost of a premature "up" is
		// a single retried request, and the retry path handles that.
		st.pollFails = 0
		st.pollReason = ""
		if st.pollState != Available {
			t.logger.Info("model listed", "model", id, "was", string(st.pollState),
				"generation_probe", st.probe)
			// The seat (or its node) just came up: whatever it proved before
			// is void. It is warming until a generation succeeds.
			if st.probe {
				t.enterWarmingLocked(st, "")
			}
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

// enterWarmingLocked resets a seat's probe state so the next round probes it.
// Caller holds t.mu.
func (t *Tracker) enterWarmingLocked(st *modelState, why string) {
	st.confirmed = false
	st.probeSeq++
	st.probeFails = 0
	st.probeErr = why
	st.probeNextAt = time.Time{}
	st.warmingSince = t.now()
}

// probeJob is one due generation probe, captured under the lock.
type probeJob struct {
	id     string
	seq    uint64
	target GenTarget
	err    error // set when the target could not be built; applied as a failure
}

type probeResult struct {
	id      string
	seq     uint64
	err     error
	elapsed time.Duration
	// at is when the probe finished, on the tracker's clock. Warm-up is
	// measured to this instant, not to the end of the round: one 10 s probe
	// must not make every other seat in the round look like it took 10 s.
	at time.Time
}

// dueProbesLocked lists the seats whose probe should run this round: listed
// up, not yet confirmed, and past their backoff. Caller holds t.mu.
func (t *Tracker) dueProbesLocked() []probeJob {
	if t.cfg.DisableGenerationProbe {
		return nil
	}
	now := t.now()
	var jobs []probeJob
	for id, st := range t.models {
		if !st.probe || st.confirmed || st.pollState != Available {
			continue
		}
		if !st.probeNextAt.IsZero() && now.Before(st.probeNextAt) {
			continue
		}
		job := probeJob{id: id, seq: st.probeSeq}
		job.target, job.err = t.genTarget(id)
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].id < jobs[j].id })
	return jobs
}

// genTarget builds the probe target for a model: the backend's own base
// (tool proxy bypassed) and the name the engine serves it under.
func (t *Tracker) genTarget(id string) (GenTarget, error) {
	reg := t.cfg.Registry
	m, ok := reg.Models[id]
	if !ok {
		return GenTarget{}, fmt.Errorf("model %q not in registry", id)
	}
	direct := false
	base, err := reg.APIBase(id, &direct)
	if err != nil {
		return GenTarget{}, err
	}
	tgt := GenTarget{
		Model:        id,
		Root:         strings.TrimSuffix(base, "/v1"),
		BackendModel: m.BackendModelName(),
		APIClass:     m.APIClass,
	}
	if m.Backend == config.BackendExternal && m.APIKey != "" {
		tgt.Bearer = resolveKey(m.APIKey, t.getenv)
		tgt.BearerHeader = m.APIKeyHeader
	}
	return tgt, nil
}

// resolveKey mirrors the router's api_key convention: a literal "sk-" key is
// used as is; anything else names an environment variable.
func resolveKey(raw string, getenv func(string) string) string {
	if strings.HasPrefix(raw, "sk-") {
		return raw
	}
	return getenv(raw)
}

// runProbes executes the due probes concurrently, each under its own timeout.
func (t *Tracker) runProbes(ctx context.Context, jobs []probeJob) []probeResult {
	if len(jobs) == 0 {
		return nil
	}
	results := make([]probeResult, len(jobs))
	var wg sync.WaitGroup
	for i, job := range jobs {
		if job.err != nil {
			results[i] = probeResult{id: job.id, seq: job.seq, err: job.err, at: t.now()}
			continue
		}
		wg.Add(1)
		go func(i int, job probeJob) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, t.cfg.ProbeTimeout)
			defer cancel()
			start := time.Now()
			err := t.genProbe(pctx, job.target)
			results[i] = probeResult{id: job.id, seq: job.seq, err: err, elapsed: time.Since(start), at: t.now()}
		}(i, job)
	}
	wg.Wait()
	return results
}

// applyProbeLocked folds one probe outcome into the seat's state. Results
// from a superseded warm-up (the seat was demoted or re-listed while the probe
// was in flight) are dropped; a failure that lands after real traffic already
// confirmed the seat is dropped too. Caller holds t.mu.
func (t *Tracker) applyProbeLocked(r probeResult) {
	st := t.models[r.id]
	if st == nil || st.probeSeq != r.seq || !st.probe {
		return
	}
	var rejected *ProbeRejected
	if r.err == nil || errors.As(r.err, &rejected) {
		if st.confirmed {
			return
		}
		st.confirmed = true
		st.probeErr = ""
		at := r.at
		if at.IsZero() {
			at = t.now()
		}
		warmup := at.Sub(st.warmingSince).Round(time.Millisecond).String()
		if rejected != nil {
			// Up, but our probe body was refused: say so once per warm-up so
			// the probe gets fixed rather than the seat sitting out.
			t.logger.Warn("seat warm (probe rejected — check the probe body for this class)",
				"model", r.id, "warmup", warmup, "status", rejected.Status, "body", rejected.Body)
		} else {
			t.logger.Info("seat warm", "model", r.id, "via", "probe",
				"warmup", warmup, "attempts", st.probeFails+1,
				"probe_latency", r.elapsed.Round(time.Millisecond).String())
		}
		return
	}
	if st.confirmed {
		return
	}
	st.probeFails++
	st.probeErr = r.err.Error()
	st.probeNextAt = t.now().Add(probeBackoff(st.probeFails))
	if st.probeFails == 1 {
		t.logger.Info("seat warming", "model", r.id, "err", st.probeErr,
			"next_probe_in", probeBackoff(1).String())
	} else {
		t.logger.Debug("generation probe failed", "model", r.id, "attempt", st.probeFails,
			"err", st.probeErr, "next_probe_in", probeBackoff(st.probeFails).String())
	}
}

// ReportFailure records a real proxy failure against a model. Only genuine
// transport-level failures should be reported — a 400 from a healthy backend
// says nothing about availability.
//
// For a probed seat, a 503 or a refused connection is the reloading shape:
// the seat is demoted to warming and re-probed, and the failure does not
// count toward the breaker — the probe owns that recovery, and it is faster
// and more specific than a 60 s cooldown.
func (t *Tracker) ReportFailure(modelID string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.stateFor(modelID)
	if st.probe && isWarmingSignal(err) {
		if st.confirmed || st.probeErr == "" {
			why := ""
			if err != nil {
				why = err.Error()
			}
			t.logger.Warn("seat demoted to warming", "model", modelID, "err", why)
			t.enterWarmingLocked(st, why)
			// A real request just failed this way: count it as the first
			// attempt so the next probe waits one backoff step, not zero.
			st.probeFails = 1
			st.probeNextAt = t.now().Add(probeBackoff(1))
		}
		return
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

// ReportSuccess clears the breaker for a model, and confirms a warming seat:
// a request it just served is better evidence than any probe.
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
	if st.probe && !st.confirmed {
		st.confirmed = true
		st.probeErr = ""
		t.logger.Info("seat warm", "model", modelID, "via", "traffic",
			"warmup", t.now().Sub(st.warmingSince).Round(time.Millisecond).String())
	}
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

	if st.pollState == Unavailable {
		return Unavailable, SourcePoll, st.pollReason
	}
	// The listing check sits below the node poll — a powered-down node is
	// the more fundamental fact — and above warming: a seat serving the
	// wrong model will never "warm up" into the right one.
	if st.absent {
		return Absent, SourceInventory, st.absentReason
	}
	switch st.pollState {
	case Available:
		if st.probe && !st.confirmed {
			return Warming, SourceProbe, warmingReason(st)
		}
		return Available, SourcePoll, ""
	default:
		if !t.polled {
			return Unknown, SourceAssumed, "not yet polled"
		}
		return Available, SourceAssumed, ""
	}
}

func warmingReason(st *modelState) string {
	if st.probeErr == "" {
		return "generation probe pending"
	}
	return "generation probe: " + st.probeErr
}

// Routable is the routing predicate: Unknown counts as routable so a
// just-started router never refuses traffic it has no evidence against;
// Warming does not — that is the whole point of the probe — and neither does
// Absent, a seat that would answer for a different model than the one named.
func (t *Tracker) Routable(modelID string) bool {
	state, _, _ := t.State(modelID)
	return state != Unavailable && state != Warming && state != Absent
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

// Snapshot returns every known model's status — registry entries and
// discovered ones — sorted by model id.
func (t *Tracker) Snapshot() []Status {
	t.mu.Lock()
	defer t.mu.Unlock()

	reg := t.cfg.Registry
	ids := make([]string, 0, len(reg.Models)+len(t.discovered))
	for id := range reg.Models {
		ids = append(ids, id)
	}
	for id := range t.discovered {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]Status, 0, len(ids))
	for _, id := range ids {
		state, source, reason := t.stateLocked(id)
		m, discovered := reg.Models[id], false
		if e, ok := t.discovered[id]; ok {
			m, discovered = e.def, true
		}
		st := t.models[id]
		since := time.Time{}
		var warming *WarmingInfo
		if st != nil {
			if st.lastState != state {
				st.lastState = state
				st.since = t.now()
			}
			since = st.since
			if state == Warming {
				warming = &WarmingInfo{
					Since:     st.warmingSince,
					Attempts:  st.probeFails,
					LastError: st.probeErr,
					NextProbe: st.probeNextAt,
				}
			}
		}
		out = append(out, Status{
			Model:        id,
			State:        state,
			Reason:       reason,
			Source:       source,
			Node:         m.Node,
			Since:        since,
			ExpectedDown: t.nodeExpectedDownLocked(m.Node),
			Warming:      warming,
			Discovered:   discovered,
		})
	}
	return out
}

// Counts tallies the current verdicts, for /health.
func (t *Tracker) Counts() map[Availability]int {
	out := map[Availability]int{}
	for _, s := range t.Snapshot() {
		out[s.State]++
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

// SeatLoad is the per-seat load telemetry the pressure balancer consumes:
// how many requests the agent sees in flight and queued for this model, and
// the GPU-utilisation percentage of the node it runs on (-1 = unknown). The
// router reads these on the poll callback and never on the request path.
type SeatLoad struct {
	Running    int
	Waiting    int
	GPUBusyPct int
	Node       string
}

// SeatLoads returns per-model load for every fleet-resident model, folded from
// the most recent node poll. Nodeless externals are omitted (nothing to
// balance). GPUBusyPct is the node's aggregate GPU utilisation, or the max of
// its per-GPU busy percentages, and -1 when the agent reported none.
func (t *Tracker) SeatLoads() map[string]SeatLoad {
	t.mu.RLock()
	defer t.mu.RUnlock()
	reg := t.cfg.Registry
	if reg == nil {
		return nil
	}
	out := make(map[string]SeatLoad, len(reg.Models))
	for id, m := range reg.Models {
		nodes := modelNodes(m)
		if len(nodes) == 0 {
			continue // nodeless external: not balanced
		}
		node := nodes[0] // head node's card is the one that decodes
		snap := t.nodes[node]
		load := SeatLoad{GPUBusyPct: -1, Node: node}
		if snap.Health != nil {
			load.GPUBusyPct = nodeGPUBusy(snap.Health)
		}
		for _, am := range snap.Models {
			if am.ModelID == id {
				load.Running = am.RequestsRunning
				load.Waiting = am.RequestsWaiting
				break
			}
		}
		out[id] = load
	}
	return out
}

// nodeGPUBusy folds an agent health payload into one GPU-utilisation percent:
// the top-level gpu_busy_pct if present, else the busiest per-GPU reading,
// else -1 (unknown).
func nodeGPUBusy(h *AgentHealth) int {
	if h.GPUBusyPct != nil {
		return *h.GPUBusyPct
	}
	busy := -1
	for _, g := range h.GPUs {
		if g.BusyPct != nil && *g.BusyPct > busy {
			busy = *g.BusyPct
		}
	}
	return busy
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
