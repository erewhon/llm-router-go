package router

// Roles are the dynamic half of model resolution. An alias names one specific
// model; a role names an *intent* — "a local coding model" — and binds to
// whichever of its candidates is actually up right now.
//
// The distinction matters when nodes get powered down on a schedule. Asking
// for "qwen3.6-hypatia" must keep meaning that exact model and fail honestly
// when it is off. Asking for "coder" must keep working, moving to archimedes
// or hekaton, but never quietly becoming a paid cloud model — the role's
// require block is validated at config load, so every candidate genuinely
// satisfies the same contract.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/erewhon/llm-router-go/internal/config"
)

// availability is the router's view of the health tracker. Kept as a narrow
// interface so tests can stub it and so a nil tracker (availability tracking
// disabled) degrades to "everything is routable" — exactly today's behaviour.
type availability interface {
	Routable(modelID string) bool
	Reason(modelID string) string
	ReportFailure(modelID string, err error)
	ReportSuccess(modelID string)
}

// alwaysRoutable is the no-op availability used when tracking is disabled.
type alwaysRoutable struct{}

func (alwaysRoutable) Routable(string) bool        { return true }
func (alwaysRoutable) Reason(string) string        { return "availability tracking disabled" }
func (alwaysRoutable) ReportFailure(string, error) {}
func (alwaysRoutable) ReportSuccess(string)        {}

// roleCandidate is one entry in a role's preference order.
type roleCandidate struct {
	ModelID string
	// Overflow marks an entry from the role's overflow list rather than its
	// candidate list — it crossed the locality boundary deliberately, and
	// callers surface that fact (header, log, reqlog) rather than hide it.
	Overflow bool
	// Chain names the fallback-chain entry this candidate belongs to: the
	// chain's own key when the walk is a chain resolution, or the chain a
	// role candidate expanded from. Empty for a plain model candidate. It
	// rides into resolveResult.Chain so chain requests keep their 5xx-retry
	// semantics even when the chain was reached through a role.
	Chain string
}

// roleUnavailableError is returned when a role has nothing to route to. It
// carries the per-candidate reasons because the useful question at 2am is
// "which ones are down and why", not "unknown model".
type roleUnavailableError struct {
	Role    string
	Reasons []string
}

func (e *roleUnavailableError) Error() string {
	if len(e.Reasons) == 0 {
		return fmt.Sprintf("role %q has no candidates routable in this mode", e.Role)
	}
	return fmt.Sprintf("role %q has no available model: %s", e.Role, strings.Join(e.Reasons, "; "))
}

// roleOrder returns a role's full preference order: candidates first, then the
// overflow list if (and only if) the role opted into overflowing. Entries have
// already been filtered to the active mode by config.RolesForMode.
func roleOrder(rd config.RoleDefinition) []roleCandidate {
	out := make([]roleCandidate, 0, len(rd.Candidates)+len(rd.Overflow))
	for _, id := range rd.Candidates {
		out = append(out, roleCandidate{ModelID: id})
	}
	if rd.OnEmpty == config.OnEmptyOverflow {
		for _, id := range rd.Overflow {
			out = append(out, roleCandidate{ModelID: id, Overflow: true})
		}
	}
	return out
}

// expandChains rewrites a preference order so that any candidate which is
// itself a fallback-chain entry contributes its concrete providers in place.
// A chain entry has no upstream of its own (virtual ones especially), so a
// role that listed one — coder-hard's kimi-k2.7-code, say — must walk the
// chain's providers rather than try to forward to the chain key itself.
// Provider entries inherit the candidate's Overflow flag and carry the chain's
// key so the served request is still labelled with the chain name.
func (rt *Router) expandChains(order []roleCandidate) []roleCandidate {
	out := make([]roleCandidate, 0, len(order))
	for _, cand := range order {
		m, known := rt.active[cand.ModelID]
		if !known || len(m.Fallbacks) == 0 {
			out = append(out, cand)
			continue
		}
		if !m.IsVirtual() {
			out = append(out, roleCandidate{ModelID: cand.ModelID, Overflow: cand.Overflow, Chain: cand.ModelID})
		}
		for _, fid := range m.Fallbacks {
			out = append(out, roleCandidate{ModelID: fid, Overflow: cand.Overflow, Chain: cand.ModelID})
		}
	}
	return out
}

// resolveRole binds a role name to a concrete model. It walks the preference
// order and takes the first entry the tracker reports routable, recording the
// untried tail so a mid-flight upstream failure can advance to the next one
// without re-resolving from scratch.
func (rt *Router) resolveRole(name, original string, forceDirect bool) (resolveResult, error) {
	rd, ok := rt.roles[name]
	if !ok {
		return resolveResult{}, fmt.Errorf("router: unknown role %q", name)
	}

	order := rt.expandChains(roleOrder(rd))
	reasons := make([]string, 0, len(order))

	for i, cand := range order {
		m, known := rt.active[cand.ModelID]
		if !known {
			// RolesForMode already filtered these out; a survivor here means
			// the registry changed under us. Skip rather than 500.
			continue
		}
		if !rt.avail.Routable(cand.ModelID) {
			reasons = append(reasons, fmt.Sprintf("%s: %s", cand.ModelID, rt.avail.Reason(cand.ModelID)))
			continue
		}
		res, err := rt.buildResult(cand.ModelID, m, "", original, forceDirect, "")
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", cand.ModelID, err))
			continue
		}
		res.Role = name
		res.Chain = cand.Chain
		res.Overflowed = cand.Overflow
		res.Remaining = order[i+1:]
		return res, nil
	}

	// Nothing available. A role that declined to declare an overflow list is
	// saying "I would rather fail than pretend", so say so plainly.
	if rd.OnEmpty == config.OnEmptyError && len(rd.Overflow) == 0 {
		reasons = append(reasons, fmt.Sprintf("role is on_empty=%s (no overflow configured)", config.OnEmptyError))
	}
	return resolveResult{}, &roleUnavailableError{Role: name, Reasons: reasons}
}

// nextRoleCandidate picks the next routable entry from a resolveResult's
// untried tail, for the failover retry path. Returns ok=false when the tail is
// exhausted or nothing left in it is routable.
func (rt *Router) nextRoleCandidate(res resolveResult, forceDirect bool) (resolveResult, bool) {
	for i, cand := range res.Remaining {
		m, known := rt.active[cand.ModelID]
		if !known || !rt.avail.Routable(cand.ModelID) {
			continue
		}
		next, err := rt.buildResult(cand.ModelID, m, "", res.ResolvedFrom, forceDirect, res.Egress)
		if err != nil {
			continue
		}
		next.Role = res.Role
		// The candidate's own chain membership, not the failed target's: a
		// role's tail can cross from one chain's providers into a plain
		// candidate (or another chain), and the label must follow the entry.
		next.Chain = cand.Chain
		next.Overflowed = cand.Overflow
		next.Remaining = res.Remaining[i+1:]
		return next, true
	}
	return resolveResult{}, false
}

// RoleNames returns the configured role names for this mode, sorted. Used by
// /v1/models and the well-known so clients see roles as routable names.
func (rt *Router) RoleNames() []string {
	out := make([]string, 0, len(rt.roles))
	for name := range rt.roles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
