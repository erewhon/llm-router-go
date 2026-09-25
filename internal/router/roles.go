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
		m, known := rt.lookupModel(cand.ModelID)
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
func (rt *Router) resolveRole(name, original string, forceDirect bool, promptTokens int, caller privacyTier) (resolveResult, error) {
	rd, ok := rt.roles[name]
	if !ok {
		return resolveResult{}, fmt.Errorf("router: unknown role %q", name)
	}
	tier := effectiveTier(rd, caller)

	base := roleOrder(rd)
	var pressureNote string
	var pressureByID map[string]int
	if rd.Balance == config.BalancePressure {
		base, pressureByID, pressureNote = rt.orderByPressure(base, rd, rt.avail.Routable)
	}
	order := rt.expandChains(base)
	reasons := make([]string, 0, len(order))
	// excluded is the privacy tier's own list, kept apart from reasons so a
	// total miss can be classified: policy-only is a 403 the caller cannot
	// retry out of; anything in reasons means a compliant candidate existed
	// and was merely down or too small, which is a 503.
	excluded := make([]string, 0)
	// gatedOut remembers the first candidate the envelope skipped, so a
	// result that lands further down the order can name what it passed over.
	gatedOut := ""

	for i, cand := range order {
		m, known := rt.lookupModel(cand.ModelID)
		if !known {
			// A discovered member its provider does not list (yet, or any
			// more) — worth saying. Anything else: RolesForMode already
			// filtered it, so the registry changed under us; skip, not 500.
			if why := rt.unlistedReason(cand.ModelID); why != "" {
				reasons = append(reasons, fmt.Sprintf("%s: %s", cand.ModelID, why))
			}
			continue
		}
		// Policy first — before health — so the classification above is
		// exact, and because "excluded by your privacy tier" is the more
		// useful thing to say about a candidate whatever its health. This
		// filters OVERFLOW entries too: a role's overflow is exempt from the
		// role's own locality by design, but never from the caller's tier,
		// so X-Router-Overflow: true cannot happen under `local`.
		if why := privacyGate(m, tier); why != "" {
			excluded = append(excluded, fmt.Sprintf("%s: %s", cand.ModelID, why))
			continue
		}
		if why := capabilityGap(m, rd); why != "" {
			reasons = append(reasons, fmt.Sprintf("%s: %s", cand.ModelID, why))
			continue
		}
		if !rt.avail.Routable(cand.ModelID) {
			reasons = append(reasons, fmt.Sprintf("%s: %s", cand.ModelID, rt.avail.Reason(cand.ModelID)))
			continue
		}
		// The envelope check sits after availability on purpose: "it is down"
		// is the more useful thing to report about a candidate that is both
		// down and too small, and it keeps the reason list stable as prompt
		// sizes vary.
		if why := rt.contextGate(m, promptTokens); why != "" {
			reasons = append(reasons, fmt.Sprintf("%s: %s", cand.ModelID, why))
			if gatedOut == "" {
				gatedOut = cand.ModelID
			}
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
		res.PromptTokens = promptTokens
		res.Downshift = gatedOut
		res.PrivacyTier = tier
		res.PressureNote = pressureNote
		if pressureByID != nil {
			if p, ok := pressureByID[cand.ModelID]; ok {
				pv := p
				res.CandidatePressure = &pv
			}
		}
		return res, nil
	}

	// Nothing served. Two different failures, two different answers:
	//   - every candidate was excluded by the privacy tier and none was
	//     merely down → 403, the caller's policy did this and a retry cannot
	//     change it;
	//   - a compliant candidate existed but could not serve right now → 503
	//     with every reason, the policy exclusions included so the caller can
	//     see what a looser tier would have reached.
	if len(excluded) > 0 && len(reasons) == 0 {
		return resolveResult{}, &privacyRefusedError{Subject: name, Tier: tier, Excluded: excluded}
	}
	reasons = append(reasons, excluded...)
	// A role that declined to declare an overflow list is saying "I would
	// rather fail than pretend", so say so plainly.
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
		m, known := rt.lookupModel(cand.ModelID)
		if !known || !rt.avail.Routable(cand.ModelID) || capabilityGap(m, rt.roles[res.Role]) != "" {
			continue
		}
		// Same tier as the first pass. This is what keeps a zen/ member of a
		// chain from taking over when its or/ sibling 5xxs under `zdr`.
		if privacyGate(m, res.PrivacyTier) != "" {
			continue
		}
		// Gate the failover walk with the same estimate the first pass used.
		// res.PromptTokens is zero for a directly named chain, so this is inert
		// there and binding only where the caller asked for a role.
		if rt.contextGate(m, res.PromptTokens) != "" {
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
		next.PromptTokens = res.PromptTokens
		next.Downshift = res.Downshift
		next.PrivacyTier = res.PrivacyTier
		return next, true
	}
	return resolveResult{}, false
}

// capabilityGap re-checks a discovered member against the role's required
// capabilities: load time could only vouch for the source, and what the
// entry claims comes from its listing plus the source's capabilities floor.
// Hand-written members were checked at load and pass through.
func capabilityGap(m config.ModelDefinition, rd config.RoleDefinition) string {
	if !m.IsDiscovered() {
		return ""
	}
	for _, want := range rd.Require.Capabilities {
		if !m.HasCapability(want) {
			return fmt.Sprintf("lacks required capability %q (its listing does not say; set capabilities on the discovery source)", want)
		}
	}
	return ""
}

// unlistedReason explains a role member that names a discovered id the
// inventory has not adopted, or "" for anything else.
func (rt *Router) unlistedReason(id string) string {
	src, pid, ok := rt.registry.DiscoveredMember(id)
	if !ok {
		return ""
	}
	return fmt.Sprintf("not listed by %s (%s) — it becomes routable when the listing includes %q", src.Prefix, src.APIBase, pid)
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
