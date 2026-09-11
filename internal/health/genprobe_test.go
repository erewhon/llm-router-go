package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

// probeOK is the generation probe every existing tracker test wants: the
// seat generates as soon as its listing is up. Tests about warming install
// their own.
func probeOK(context.Context, GenTarget) error { return nil }

// scriptedProbe answers each probe from a per-model script and then repeats
// the last entry. Records every target it was asked to probe.
type scriptedProbe struct {
	mu      sync.Mutex
	script  map[string][]error
	targets []GenTarget
}

func (s *scriptedProbe) probe(_ context.Context, t GenTarget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets = append(s.targets, t)
	seq := s.script[t.Model]
	if len(seq) == 0 {
		return nil
	}
	err := seq[0]
	if len(seq) > 1 {
		s.script[t.Model] = seq[1:]
	}
	return err
}

func (s *scriptedProbe) count(model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.targets {
		if t.Model == model {
			n++
		}
	}
	return n
}

// warmingFleet: everything up, agent lists the local chat seat as running.
func warmingFleet() *fakeFleet {
	return &fakeFleet{states: map[string]map[string]string{
		"hypatia": {"qwen3.6-hypatia": StateRunning},
	}}
}

func newProbeTracker(t *testing.T, fleet *fakeFleet, gp GenProbeFunc, clock *time.Time) (*Tracker, *config.ModelRegistry) {
	t.Helper()
	reg, err := config.LoadBytes([]byte(trackerYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	hostToNode := map[string]string{}
	for name, n := range reg.Nodes {
		hostToNode[n.Host] = name
	}
	tr := NewTracker(Config{
		Registry:        reg,
		Probe:           fleet.probe(hostToNode),
		GenerationProbe: gp,
		Now:             func() time.Time { return *clock },
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return tr, reg
}

func loading() error {
	return &errProbeStatus{Status: 503, Body: `{"error":{"message":"Loading model","code":503}}`}
}

func TestListedSeatIsWarmingUntilAGenerationSucceeds(t *testing.T) {
	clock := time.Date(2026, 9, 6, 22, 0, 0, 0, time.UTC)
	gp := &scriptedProbe{script: map[string][]error{
		"qwen3.6-hypatia": {loading(), loading(), loading(), nil},
	}}
	tr, _ := newProbeTracker(t, warmingFleet(), gp.probe, &clock)
	ctx := context.Background()

	// Round 1: listing up, first probe fails → warming, not routable, and
	// the reason names it.
	tr.PollOnce(ctx)
	if tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("seat should not be routable while its probe fails")
	}
	state, source, reason := tr.State("qwen3.6-hypatia")
	if state != Warming || source != SourceProbe {
		t.Fatalf("state = %s/%s, want warming/probe", state, source)
	}
	if !strings.Contains(reason, "Loading model") {
		t.Errorf("reason should carry the probe error, got %q", reason)
	}
	if got := tr.Reason("qwen3.6-hypatia"); !strings.HasPrefix(got, "warming (probe:") {
		t.Errorf("Reason() = %q, want a 'warming (probe: …)' prefix for the 503 body", got)
	}

	// The same round must NOT have probed the things the probe is off for.
	for _, tg := range gp.targets {
		if tg.Model != "qwen3.6-hypatia" {
			t.Errorf("probed %q, which should be exempt (class %q, local=%v)", tg.Model, tg.APIClass, tg.Model == "flux-dev")
		}
	}
	if !tr.Routable("flux-dev") || !tr.Routable("kimi-k2.7-code") {
		t.Errorf("unprobed models should be routable as soon as the poll says up")
	}

	// Backoff: a round inside the 5 s window does not re-probe.
	clock = clock.Add(2 * time.Second)
	tr.PollOnce(ctx)
	if n := gp.count("qwen3.6-hypatia"); n != 1 {
		t.Fatalf("probe ran %d times inside the backoff window, want 1", n)
	}
	// Past it: second failure, backoff doubles to 10 s.
	clock = clock.Add(4 * time.Second)
	tr.PollOnce(ctx)
	if n := gp.count("qwen3.6-hypatia"); n != 2 {
		t.Fatalf("probe count after backoff = %d, want 2", n)
	}
	clock = clock.Add(6 * time.Second)
	tr.PollOnce(ctx)
	if n := gp.count("qwen3.6-hypatia"); n != 2 {
		t.Fatalf("probe count inside the doubled backoff = %d, want 2", n)
	}
	snap := statusOf(tr, "qwen3.6-hypatia")
	if snap.Warming == nil || snap.Warming.Attempts != 2 {
		t.Fatalf("snapshot should carry warming detail with 2 attempts: %+v", snap)
	}

	// Third failure, then the fourth probe succeeds → available, and the
	// warming detail is gone from the snapshot.
	clock = clock.Add(5 * time.Second)
	tr.PollOnce(ctx)
	clock = clock.Add(21 * time.Second)
	tr.PollOnce(ctx)
	if !tr.Routable("qwen3.6-hypatia") {
		state, _, reason := tr.State("qwen3.6-hypatia")
		t.Fatalf("seat should be routable after a successful probe; state=%s reason=%q", state, reason)
	}
	if s := statusOf(tr, "qwen3.6-hypatia"); s.State != Available || s.Warming != nil {
		t.Errorf("status after warm-up = %+v, want available with no warming block", s)
	}
	// And no further probes once confirmed.
	before := gp.count("qwen3.6-hypatia")
	clock = clock.Add(time.Minute)
	tr.PollOnce(ctx)
	if gp.count("qwen3.6-hypatia") != before {
		t.Errorf("a confirmed seat should not be re-probed")
	}
}

func TestProbeTargetBypassesToolProxyAndUsesBackendName(t *testing.T) {
	clock := time.Now()
	gp := &scriptedProbe{script: map[string][]error{}}
	yaml := `
nodes:
  hypatia: {host: hypatia.local, gpu: nvidia, vram_gb: 128}
models:
  seat:
    hf_repo: Qwen/Qwen3.6-35B#q4
    node: hypatia
    api_port: 5400
    tool_proxy: true
`
	reg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	fleet := &fakeFleet{states: map[string]map[string]string{"hypatia": {"seat": StateRunning}}}
	tr := NewTracker(Config{
		Registry:        reg,
		Probe:           fleet.probe(map[string]string{"hypatia.local": "hypatia"}),
		GenerationProbe: gp.probe,
		Now:             func() time.Time { return clock },
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tr.PollOnce(context.Background())
	if len(gp.targets) != 1 {
		t.Fatalf("want exactly one probe, got %d", len(gp.targets))
	}
	tg := gp.targets[0]
	if tg.Root != "http://hypatia.local:5400" {
		t.Errorf("probe root = %q; it must be the engine itself (no tool proxy, no /v1 suffix)", tg.Root)
	}
	if tg.BackendModel != "Qwen/Qwen3.6-35B" {
		t.Errorf("backend model = %q, want the hf_repo without its #variant", tg.BackendModel)
	}
	if tg.APIClass != config.APIClassChat {
		t.Errorf("api class = %q, want chat", tg.APIClass)
	}
}

func TestRealTrafficConfirmsAWarmingSeatWithoutAPoll(t *testing.T) {
	clock := time.Now()
	gp := &scriptedProbe{script: map[string][]error{"qwen3.6-hypatia": {loading()}}}
	tr, _ := newProbeTracker(t, warmingFleet(), gp.probe, &clock)
	tr.PollOnce(context.Background())
	if tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("precondition: seat should be warming")
	}
	// A request slipped through anyway (say, the tool proxy's stale mirror)
	// and succeeded. That is proof enough.
	tr.ReportSuccess("qwen3.6-hypatia")
	if !tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("ReportSuccess should confirm a warming seat immediately")
	}
}

func TestA503FromRealTrafficDemotesToWarmingNotDown(t *testing.T) {
	clock := time.Now()
	gp := &scriptedProbe{script: map[string][]error{"qwen3.6-hypatia": {nil}}}
	tr, _ := newProbeTracker(t, warmingFleet(), gp.probe, &clock)
	ctx := context.Background()
	tr.PollOnce(ctx)
	if !tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("precondition: seat should be available")
	}

	// The engine restarted under us: a proxied request came back 503.
	tr.ReportFailure("qwen3.6-hypatia", &errProbeStatus{Status: 503})
	state, source, _ := tr.State("qwen3.6-hypatia")
	if state != Warming || source != SourceProbe {
		t.Fatalf("after a 503, state = %s/%s, want warming/probe (not the breaker)", state, source)
	}
	// Two more 503s would have tripped the breaker; they must not — the
	// probe owns recovery for this shape.
	tr.ReportFailure("qwen3.6-hypatia", &errProbeStatus{Status: 503})
	tr.ReportFailure("qwen3.6-hypatia", &errProbeStatus{Status: 503})
	if state, source, _ := tr.State("qwen3.6-hypatia"); state != Warming || source != SourceProbe {
		t.Fatalf("503s must not trip the breaker for a probed seat; got %s/%s", state, source)
	}

	// Next round (past the first backoff step) re-probes; the script's last
	// entry is nil, so it comes back.
	clock = clock.Add(6 * time.Second)
	tr.PollOnce(ctx)
	if !tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("seat should recover on the next successful probe")
	}
	// ...and a real success restores it without a poll at all.
	tr.ReportFailure("qwen3.6-hypatia", &errProbeStatus{Status: 503})
	tr.ReportSuccess("qwen3.6-hypatia")
	if !tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("ReportSuccess should restore a demoted seat immediately")
	}
}

func TestNonWarmingFailuresStillFeedTheBreaker(t *testing.T) {
	clock := time.Now()
	tr, _ := newProbeTracker(t, warmingFleet(), probeOK, &clock)
	tr.PollOnce(context.Background())
	// A timeout is not a loading signal: three of them open the breaker as
	// before.
	for i := 0; i < DefaultBreakerTrip; i++ {
		tr.ReportFailure("qwen3.6-hypatia", context.DeadlineExceeded)
	}
	if state, source, _ := tr.State("qwen3.6-hypatia"); state != Unavailable || source != SourcePassive {
		t.Fatalf("timeouts should still trip the breaker; got %s/%s", state, source)
	}
}

func TestUnprobedModelIgnoresWarmingSignals(t *testing.T) {
	clock := time.Now()
	tr, _ := newProbeTracker(t, warmingFleet(), probeOK, &clock)
	tr.PollOnce(context.Background())
	// kimi is a nodeless external: no probe. A 503 counts toward its breaker
	// exactly as it did before the probe existed.
	for i := 0; i < DefaultBreakerTrip; i++ {
		tr.ReportFailure("kimi-k2.7-code", &errProbeStatus{Status: 503})
	}
	if state, source, _ := tr.State("kimi-k2.7-code"); state != Unavailable || source != SourcePassive {
		t.Fatalf("an unprobed external's 503s belong to the breaker; got %s/%s", state, source)
	}
}

func TestSeatComingBackUpReenteresWarming(t *testing.T) {
	clock := time.Now()
	gp := &scriptedProbe{script: map[string][]error{"qwen3.6-hypatia": {nil}}}
	fleet := warmingFleet()
	tr, _ := newProbeTracker(t, fleet, gp.probe, &clock)
	ctx := context.Background()
	tr.PollOnce(ctx)
	if !tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("precondition: available")
	}
	// Node goes away for two rounds → unavailable.
	fleet.down = map[string]bool{"hypatia": true}
	tr.PollOnce(ctx)
	tr.PollOnce(ctx)
	if state, _, _ := tr.State("qwen3.6-hypatia"); state != Unavailable {
		t.Fatalf("node down should read unavailable, got %s", state)
	}
	// Node returns and the listing is up, but now the engine is loading.
	fleet.down = nil
	gp.mu.Lock()
	gp.script["qwen3.6-hypatia"] = []error{loading()}
	gp.mu.Unlock()
	tr.PollOnce(ctx)
	if state, source, _ := tr.State("qwen3.6-hypatia"); state != Warming || source != SourceProbe {
		t.Fatalf("a seat that came back must re-earn availability; got %s/%s", state, source)
	}
}

func TestStaleProbeResultCannotOverrideTraffic(t *testing.T) {
	// A probe launched against warm-up N must not demote a seat that real
	// traffic confirmed while the probe was in flight.
	clock := time.Now()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	gp := func(_ context.Context, tg GenTarget) error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return loading()
		}
		return nil
	}
	tr, _ := newProbeTracker(t, warmingFleet(), gp, &clock)
	done := make(chan struct{})
	go func() {
		tr.PollOnce(context.Background())
		close(done)
	}()
	<-started
	tr.ReportSuccess("qwen3.6-hypatia") // traffic proves it while the probe hangs
	close(release)
	<-done
	if !tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("a late probe failure must not undo a real success")
	}
}

func TestProbeRejectedCountsAsWarm(t *testing.T) {
	clock := time.Now()
	gp := &scriptedProbe{script: map[string][]error{
		"qwen3.6-hypatia": {&ProbeRejected{Status: 400, Body: "max_tokens must be > 1"}},
	}}
	tr, _ := newProbeTracker(t, warmingFleet(), gp.probe, &clock)
	tr.PollOnce(context.Background())
	if !tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("a 4xx from the generation path means the seat is up; it must not sit out")
	}
}

func TestDisableGenerationProbeRestoresListingBehaviour(t *testing.T) {
	clock := time.Now()
	gp := &scriptedProbe{script: map[string][]error{"qwen3.6-hypatia": {loading()}}}
	reg, _ := config.LoadBytes([]byte(trackerYAML))
	hostToNode := map[string]string{}
	for name, n := range reg.Nodes {
		hostToNode[n.Host] = name
	}
	tr := NewTracker(Config{
		Registry:               reg,
		Probe:                  warmingFleet().probe(hostToNode),
		GenerationProbe:        gp.probe,
		DisableGenerationProbe: true,
		Now:                    func() time.Time { return clock },
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tr.PollOnce(context.Background())
	if !tr.Routable("qwen3.6-hypatia") || len(gp.targets) != 0 {
		t.Fatalf("with the probe disabled the listing alone decides; probes run = %d", len(gp.targets))
	}
}

func TestPerModelOverrideAndClassDefaults(t *testing.T) {
	yaml := `
nodes:
  n: {host: n.local, gpu: nvidia, vram_gb: 1}
models:
  chat-on:   {hf_repo: a, node: n}
  chat-off:  {hf_repo: b, node: n, health: {generation_probe: false}}
  emb-on:    {hf_repo: c, node: n, api_class: embeddings}
  rerank-on: {hf_repo: d, node: n, api_class: rerank}
  img-off:   {hf_repo: e, node: n, api_class: image_gen, backend: external, api_base: http://n.local:1/v1}
  img-on:    {hf_repo: f, node: n, api_class: image_gen, backend: external, api_base: http://n.local:2/v1, health: {generation_probe: true}}
  cloud-off: {hf_repo: g, backend: external, api_base: https://x/v1}
  cloud-on:  {hf_repo: h, backend: external, api_base: https://x/v1, health: {generation_probe: true}}
  disabled:  {hf_repo: i, node: n, enabled: false}
  chain:     {hf_repo: j, fallbacks: [cloud-off, cloud-on]}
`
	reg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	want := map[string]bool{
		"chat-on": true, "chat-off": false, "emb-on": true, "rerank-on": true,
		"img-off": false, "img-on": true, "cloud-off": false, "cloud-on": true,
		"disabled": false, "chain": false,
	}
	for id, w := range want {
		if got := ProbeEnabled(reg.Models[id]); got != w {
			t.Errorf("ProbeEnabled(%s) = %v, want %v", id, got, w)
		}
	}
}

func TestProbeFilterKeepsOutOfModeSeatsOut(t *testing.T) {
	clock := time.Now()
	gp := &scriptedProbe{script: map[string][]error{}}
	reg, _ := config.LoadBytes([]byte(trackerYAML))
	hostToNode := map[string]string{}
	for name, n := range reg.Nodes {
		hostToNode[n.Host] = name
	}
	tr := NewTracker(Config{
		Registry:        reg,
		Probe:           warmingFleet().probe(hostToNode),
		GenerationProbe: gp.probe,
		ProbeFilter:     func(string) bool { return false },
		Now:             func() time.Time { return clock },
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tr.PollOnce(context.Background())
	if len(gp.targets) != 0 || !tr.Routable("qwen3.6-hypatia") {
		t.Fatalf("filtered-out seats must be neither probed nor held warming")
	}
}

func TestIsWarmingSignal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"503", &errProbeStatus{Status: 503}, true},
		{"500", &errProbeStatus{Status: 500}, false},
		{"502", &errProbeStatus{Status: 502}, false},
		{"dial refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"wrapped refused", fmt.Errorf("x: %w", syscall.ECONNREFUSED), true},
		{"read reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, false},
		{"timeout", context.DeadlineExceeded, false},
		{"other", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := isWarmingSignal(c.err); got != c.want {
			t.Errorf("%s: isWarmingSignal = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestProbeBackoffLadder(t *testing.T) {
	want := []time.Duration{0, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second}
	for n, w := range want {
		if got := probeBackoff(n); got != w {
			t.Errorf("probeBackoff(%d) = %s, want %s", n, got, w)
		}
	}
	if probeBackoff(60) != probeBackoffCap {
		t.Errorf("overflow guard: probeBackoff(60) = %s", probeBackoff(60))
	}
}

// TestProbeGenerationAgainstAFakeEngine exercises the real HTTP probe: the
// request shape per class, auth placement, and the status → outcome mapping.
func TestProbeGenerationAgainstAFakeEngine(t *testing.T) {
	var mu sync.Mutex
	var seen []*http.Request
	var bodies []string
	status := atomic.Int32{}
	status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r)
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, `{"error":{"message":"Loading model","code":503}}`)
	}))
	defer srv.Close()
	ctx := context.Background()

	// chat: 200 → nil
	if err := ProbeGeneration(ctx, GenTarget{Root: srv.URL, BackendModel: "m", APIClass: config.APIClassChat}); err != nil {
		t.Fatalf("chat probe: %v", err)
	}
	// embeddings with a header-style key
	if err := ProbeGeneration(ctx, GenTarget{Root: srv.URL + "/", BackendModel: "e", APIClass: config.APIClassEmbeddings, Bearer: "k", BearerHeader: "X-Api-Key"}); err != nil {
		t.Fatalf("embeddings probe: %v", err)
	}
	// rerank with a bearer
	if err := ProbeGeneration(ctx, GenTarget{Root: srv.URL, BackendModel: "r", APIClass: config.APIClassRerank, Bearer: "sk-1"}); err != nil {
		t.Fatalf("rerank probe: %v", err)
	}
	// 503 → errProbeStatus with the body
	status.Store(503)
	err := ProbeGeneration(ctx, GenTarget{Root: srv.URL, BackendModel: "m", APIClass: config.APIClassChat})
	var ps *errProbeStatus
	if !errors.As(err, &ps) || ps.Status != 503 || !strings.Contains(ps.Body, "Loading model") {
		t.Fatalf("503 should map to errProbeStatus with the body; got %v", err)
	}
	if !isWarmingSignal(err) {
		t.Errorf("a 503 probe error must classify as a warming signal")
	}
	// 400 → ProbeRejected
	status.Store(400)
	err = ProbeGeneration(ctx, GenTarget{Root: srv.URL, BackendModel: "m", APIClass: config.APIClassChat})
	var rej *ProbeRejected
	if !errors.As(err, &rej) || rej.Status != 400 {
		t.Fatalf("400 should map to ProbeRejected; got %v", err)
	}
	// image class: no probe, no request
	before := len(seen)
	if err := ProbeGeneration(ctx, GenTarget{Root: srv.URL, APIClass: config.APIClassImageGen}); err != nil || len(seen) != before {
		t.Fatalf("media classes must not be probed (err=%v, requests=%d)", err, len(seen)-before)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 5 {
		t.Fatalf("want 5 requests, got %d", len(seen))
	}
	if seen[0].URL.Path != "/v1/chat/completions" || !strings.Contains(bodies[0], `"max_tokens":1`) || !strings.Contains(bodies[0], `"model":"m"`) {
		t.Errorf("chat probe: %s %s", seen[0].URL.Path, bodies[0])
	}
	if seen[1].URL.Path != "/v1/embeddings" || seen[1].Header.Get("X-Api-Key") != "k" || seen[1].Header.Get("Authorization") != "" {
		t.Errorf("embeddings probe: path=%s X-Api-Key=%q auth=%q", seen[1].URL.Path, seen[1].Header.Get("X-Api-Key"), seen[1].Header.Get("Authorization"))
	}
	if seen[2].URL.Path != "/v1/rerank" || seen[2].Header.Get("Authorization") != "Bearer sk-1" || !strings.Contains(bodies[2], `"documents"`) {
		t.Errorf("rerank probe: path=%s auth=%q body=%s", seen[2].URL.Path, seen[2].Header.Get("Authorization"), bodies[2])
	}
	for i, r := range seen {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %d: %s %s", i, r.Method, r.Header.Get("Content-Type"))
		}
	}
}

func TestProbeTimeoutIsWarmingNotHang(t *testing.T) {
	clock := time.Now()
	gp := func(ctx context.Context, _ GenTarget) error {
		<-ctx.Done() // the seat never answers
		return ctx.Err()
	}
	reg, _ := config.LoadBytes([]byte(trackerYAML))
	hostToNode := map[string]string{}
	for name, n := range reg.Nodes {
		hostToNode[n.Host] = name
	}
	tr := NewTracker(Config{
		Registry:        reg,
		Probe:           warmingFleet().probe(hostToNode),
		GenerationProbe: gp,
		ProbeTimeout:    20 * time.Millisecond,
		Now:             func() time.Time { return clock },
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	start := time.Now()
	tr.PollOnce(context.Background())
	if time.Since(start) > 2*time.Second {
		t.Fatalf("a hung probe must be bounded by ProbeTimeout")
	}
	if state, _, reason := tr.State("qwen3.6-hypatia"); state != Warming || !strings.Contains(reason, "deadline") {
		t.Fatalf("timed-out probe should leave the seat warming with the timeout as reason; got %s %q", state, reason)
	}
}

func statusOf(tr *Tracker, id string) Status {
	for _, s := range tr.Snapshot() {
		if s.Model == id {
			return s
		}
	}
	return Status{}
}
