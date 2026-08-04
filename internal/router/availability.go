package router

// GET /v1/availability is the operator view of "what is actually up right
// now, and where is each role pointing". It is also the tool proxy's feed:
// rather than run a second poller against every node agent, the proxy mirrors
// this one endpoint from its local router.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/erewhon/llm-router-go/internal/health"
)

// availabilityReporter is the optional richer half of the availability
// interface. A tracker implements it; the alwaysRoutable stub does not, so the
// endpoint degrades to reporting config rather than 500ing.
type availabilityReporter interface {
	Snapshot() []health.Status
	NodeStatuses() []health.NodeStatus
}

// roleBinding describes where a role is pointing and what else it could use.
type roleBinding struct {
	Role string `json:"role"`
	// Target is the model the role resolves to right now; empty when nothing
	// is available.
	Target string `json:"target,omitempty"`
	// Overflowed is true when Target came from the overflow list, i.e. the
	// role has crossed its own locality boundary to stay answerable.
	Overflowed bool `json:"overflowed,omitempty"`
	// Available is false when the role currently resolves to nothing; Reasons
	// then explains which candidates are out and why.
	Available bool     `json:"available"`
	Reasons   []string `json:"reasons,omitempty"`
	// Candidates is the role's preference order in this mode, so an operator
	// can see what it would fall back to next.
	Candidates []string `json:"candidates"`
	Overflow   []string `json:"overflow,omitempty"`
	Describes  string   `json:"description,omitempty"`
}

type availabilityResponse struct {
	Mode   string              `json:"mode"`
	Models []health.Status     `json:"models,omitempty"`
	Nodes  []health.NodeStatus `json:"nodes,omitempty"`
	Roles  []roleBinding       `json:"roles"`
	// Tracking is false when availability tracking is disabled, in which case
	// every model reads as routable and roles always pick their first
	// candidate. Consumers use this to avoid over-trusting the payload.
	Tracking bool `json:"tracking"`
}

func (rt *Router) handleAvailability(w http.ResponseWriter, r *http.Request) {
	resp := availabilityResponse{
		Mode:  rt.mode,
		Roles: rt.roleBindings(),
	}
	if rep, ok := rt.avail.(availabilityReporter); ok {
		resp.Tracking = true
		resp.Models = rep.Snapshot()
		resp.Nodes = rep.NodeStatuses()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// roleBindings resolves every role once and reports where it landed. This runs
// the real resolution path, so what an operator reads here is exactly what the
// next request would get.
func (rt *Router) roleBindings() []roleBinding {
	names := rt.RoleNames()
	out := make([]roleBinding, 0, len(names))
	for _, name := range names {
		rd := rt.roles[name]
		b := roleBinding{
			Role:       name,
			Candidates: rd.Candidates,
			Overflow:   rd.Overflow,
			Describes:  rd.Description,
		}
		res, err := rt.resolveRole(name, name, false)
		if err != nil {
			var roleErr *roleUnavailableError
			if errors.As(err, &roleErr) {
				b.Reasons = roleErr.Reasons
			} else {
				b.Reasons = []string{err.Error()}
			}
		} else {
			b.Available = true
			b.Target = res.ModelID
			b.Overflowed = res.Overflowed
		}
		out = append(out, b)
	}
	return out
}

// PublishAvailabilityMetrics republishes the availability gauges from the
// current tracker state. Wire it as the tracker's OnPoll callback so the
// gauges track fleet state rather than request traffic.
func (rt *Router) PublishAvailabilityMetrics() {
	models := make(map[string]bool, len(rt.active))
	for id := range rt.active {
		models[id] = rt.avail.Routable(id)
	}
	targets := make(map[string]string, len(rt.roles))
	for _, b := range rt.roleBindings() {
		targets[b.Role] = b.Target
	}
	rt.metrics.SetAvailability(models, targets)
}
