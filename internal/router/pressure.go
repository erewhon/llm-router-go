package router

// Pressure-aware role balancing spreads traffic across interchangeable seats.
//
// A role's candidates are a preference order: best first. Where two (or more)
// seats are genuinely interchangeable an operator groups them into one
// preference rank (balance_groups) and sets balance: pressure; the router then
// sends each request to the least-loaded routable seat in that rank instead of
// hammering the first one until it fails. This is deliberately NOT a scheduler
// — pressure is a coarse tiebreaker among equals, capacity is not an input, and
// nothing crosses a rank on load except one logged extreme case.
//
// Pressure per seat is read entirely from an in-memory table the poll callback
// and the proxy update; it is never a synchronous call on the request path.

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/health"
)

// unroutablePressure sinks a candidate the walk will skip below every routable
// one within its rank, so the pressure order never puts a dead seat ahead of a
// live one. Far above any real pressure score.
const unroutablePressure = 1 << 30

// pressureTracker holds the per-seat load signals. Safe for concurrent use.
type pressureTracker struct {
	cfg    config.PressureConfig
	logger *slog.Logger
	now    func() time.Time

	mu         sync.Mutex
	inflight   map[string]int             // requests this router is proxying right now
	lastServed map[string]time.Time       // last successful completion per model
	loads      map[string]health.SeatLoad // agent-reported load, refreshed each poll
}

func newPressureTracker(cfg config.PressureConfig, logger *slog.Logger) *pressureTracker {
	if logger == nil {
		logger = slog.Default()
	}
	return &pressureTracker{
		cfg:        cfg,
		logger:     logger,
		now:        time.Now,
		inflight:   map[string]int{},
		lastServed: map[string]time.Time{},
		loads:      map[string]health.SeatLoad{},
	}
}

func (p *pressureTracker) addInflight(id string) {
	p.mu.Lock()
	p.inflight[id]++
	p.mu.Unlock()
}

func (p *pressureTracker) doneInflight(id string) {
	p.mu.Lock()
	if p.inflight[id] > 0 {
		p.inflight[id]--
	}
	p.mu.Unlock()
}

// markServed records a successful completion, so the seat earns the warm-cache
// preference for the configured window.
func (p *pressureTracker) markServed(id string) {
	p.mu.Lock()
	p.lastServed[id] = p.now()
	p.mu.Unlock()
}

// setLoads replaces the agent-reported load table. Called from the poll
// callback, off the request path.
func (p *pressureTracker) setLoads(loads map[string]health.SeatLoad) {
	p.mu.Lock()
	p.loads = loads
	p.mu.Unlock()
}

// pressure is a seat's coarse load score: the busier of this router's own
// in-flight count and the agent's reported running count (they overlap — the
// agent sees this router's requests too, with poll lag — so max avoids
// double-counting), plus the agent's queue depth, plus the node's GPU bucket,
// minus one when the seat's prompt cache is still warm for it. Unknown signals
// contribute 0, so a seat that reports nothing is never penalised.
func (p *pressureTracker) pressure(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pressureLocked(id)
}

func (p *pressureTracker) pressureLocked(id string) int {
	inflight := p.inflight[id]
	load := p.loads[id]
	base := inflight
	if load.Running > base {
		base = load.Running
	}
	score := base + load.Waiting + p.cfg.GPUBucket(load.GPUBusyPct)
	if last, ok := p.lastServed[id]; ok && p.now().Sub(last) < time.Duration(p.cfg.Window())*time.Second {
		score-- // warm prompt cache: TTFT is a fraction of a cold seat's
	}
	return score
}

// orderByPressure reorders a role's preference list (candidates then overflow,
// as roleOrder produced it) so that within each preference rank the routable
// seats come first, ordered by ascending pressure. Rank is declared order
// unless balance_groups ties several candidates to one rank. Across ranks the
// declared preference is preserved, with ONE exception: when the best routable
// seat is jammed (pressure >= 3) and a lower-ranked routable, same-locality
// seat is idle (pressure <= 1), the idle seat is promoted — the only case
// pressure overrides rank, and it is logged.
//
// It returns the reordered list, a per-candidate pressure map, and a header
// note ("id=n,..."). Chains are expanded AFTER this, so a chain's provider
// order is never touched.
func (rt *Router) orderByPressure(base []roleCandidate, rd config.RoleDefinition, routable func(string) bool) ([]roleCandidate, map[string]int, string) {
	ranks := candidateRanks(rd)
	pr := make(map[string]int, len(base))
	eff := make(map[string]int, len(base)) // effective: unroutable sunk within rank
	rankOf := make(map[string]int, len(base))
	overflowBase := len(rd.Candidates)
	for i, c := range base {
		r, ok := ranks[c.ModelID]
		if !ok {
			r = overflowBase + i // overflow (or unknown): keep after candidates, in order
		}
		rankOf[c.ModelID] = r
		p := rt.pressure.pressure(c.ModelID)
		pr[c.ModelID] = p
		if routable(c.ModelID) {
			eff[c.ModelID] = p
		} else {
			eff[c.ModelID] = unroutablePressure
		}
	}

	out := append([]roleCandidate(nil), base...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rankOf[out[i].ModelID], rankOf[out[j].ModelID]
		if ri != rj {
			return ri < rj // rank dominates; declared order across ranks
		}
		return eff[out[i].ModelID] < eff[out[j].ModelID] // within a rank, least-loaded first
	})

	out = rt.applyRankOverride(out, rankOf, eff, pr)

	return out, pr, pressureNote(out, pr, routable)
}

// applyRankOverride promotes an idle lower-ranked, same-locality, routable seat
// ahead of a jammed higher-ranked one — the sole case load beats rank.
func (rt *Router) applyRankOverride(out []roleCandidate, rankOf, eff, pr map[string]int) []roleCandidate {
	if len(out) < 2 {
		return out
	}
	// The current winner is the first entry (post-sort). Only act if it is
	// routable and jammed.
	head := out[0]
	if eff[head.ModelID] >= unroutablePressure || pr[head.ModelID] < 3 {
		return out
	}
	for i := 1; i < len(out); i++ {
		c := out[i]
		if c.Overflow != head.Overflow {
			continue // never promote across the locality boundary on load
		}
		if rankOf[c.ModelID] <= rankOf[head.ModelID] {
			continue // same or higher rank already handled by the sort
		}
		if eff[c.ModelID] >= unroutablePressure || pr[c.ModelID] > 1 {
			continue
		}
		rt.logger.Info("pressure override: promoting idle lower-ranked seat over a jammed one",
			"promoted", c.ModelID, "pressure", pr[c.ModelID], "over", head.ModelID, "over_pressure", pr[head.ModelID])
		out = append(out[:i], out[i+1:]...)
		return append([]roleCandidate{c}, out...)
	}
	return out
}

// candidateRanks maps each candidate id to its preference rank: its index in
// the declared candidate list, unless balance_groups ties it (and its group
// siblings) to the group's earliest index.
func candidateRanks(rd config.RoleDefinition) map[string]int {
	rank := make(map[string]int, len(rd.Candidates))
	idx := make(map[string]int, len(rd.Candidates))
	for i, id := range rd.Candidates {
		rank[id] = i
		idx[id] = i
	}
	for _, g := range rd.BalanceGroups {
		rep := -1
		for _, id := range g {
			if i, ok := idx[id]; ok && (rep < 0 || i < rep) {
				rep = i
			}
		}
		if rep < 0 {
			continue
		}
		for _, id := range g {
			if _, ok := idx[id]; ok {
				rank[id] = rep
			}
		}
	}
	return rank
}

// pressureNote renders "id=n,..." for the routable candidates in final order,
// for X-Router-Pressure.
func pressureNote(order []roleCandidate, pr map[string]int, routable func(string) bool) string {
	var b strings.Builder
	for _, c := range order {
		if c.Overflow || !routable(c.ModelID) {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(c.ModelID)
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(pr[c.ModelID]))
	}
	return b.String()
}
