// Package health tracks which models in the registry are actually reachable
// right now, so routing can follow the fleet instead of the config file.
//
// The fleet is intermittent by design: nodes get powered down evenings and
// weekends. Availability is decided from three signals, layered:
//
//   - an active poll of each node's agent (/health + /models) — catches a
//     powered-down node, which is the dominant case;
//   - a passive circuit breaker fed by real proxy failures — catches a model
//     the agent believes is running but which is wedged;
//   - hysteresis on both, so a single dropped packet never reassigns a role;
//   - a generation probe (genprobe.go) between "listed" and "routable", and a
//     live-listing check (inventory.go) that marks an entry absent when its
//     base serves something else and adopts what a `discovery:` source lists.
//
// The probe half of this file is the shared implementation the router's
// dashboard also uses, so there is one prober in the binary rather than two
// that can disagree.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// probeClient talks to the per-node agents. The 1.5s timeout is inherited from
// the Python dashboard: healthy probes return in <250ms, and a broken host
// (e.g. mDNS resolving to an unroutable overlay IP) must not stall a poll
// round or a dashboard page load.
var probeClient = &http.Client{Timeout: 1500 * time.Millisecond}

// StateRunning is the node agent's "model is up" state string. Mirrors
// nodeagent/backends.StateRunning; kept as a literal so this package does not
// depend on the agent's internals (the router already avoids that).
const StateRunning = "running"

// AgentHealth is the subset of a node agent's /health response we consume.
type AgentHealth struct {
	TotalVRAMGB *float64   `json:"total_vram_gb"`
	FreeVRAMGB  *float64   `json:"free_vram_gb"`
	GPUBusyPct  *int       `json:"gpu_busy_pct"`
	RAMUsedGB   *float64   `json:"ram_used_gb"`
	RAMTotalGB  *float64   `json:"ram_total_gb"`
	DiskFreeGB  *float64   `json:"disk_free_gb"`
	DiskTotalGB *float64   `json:"disk_total_gb"`
	GPUs        []AgentGPU `json:"gpus"`
	Services    []any      `json:"services"`
}

// AgentGPU is one card of a multi-GPU node (absent on single-GPU nodes).
type AgentGPU struct {
	Index       int     `json:"index"`
	PDev        string  `json:"pdev"`
	VRAMUsedGB  float64 `json:"vram_used_gb"`
	VRAMTotalGB float64 `json:"vram_total_gb"`
	BusyPct     *int    `json:"busy_pct"`
}

// AgentModel is the subset of a node agent's /models list we consume.
type AgentModel struct {
	ModelID         string   `json:"model_id"`
	State           string   `json:"state"`
	RequestsRunning int      `json:"requests_running"`
	RequestsWaiting int      `json:"requests_waiting"`
	AvgTokPerS      *float64 `json:"avg_tok_per_s"`
	TotalRequests   int      `json:"total_requests"`
}

// NodeSnapshot is one probe of one node agent. Reachable=false means the agent
// did not answer — for a scheduled power-down that is the expected steady
// state, not an error.
type NodeSnapshot struct {
	Reachable bool
	Health    *AgentHealth
	Models    []AgentModel
	ProbedAt  time.Time
}

// ModelState returns the agent-reported state for a model id, and whether the
// agent mentioned it at all. Agents only report models they manage, so "not
// mentioned" is normal for node-pinned externals (flux, orpheus, the OpenArc
// embedder) and must not be read as "down".
func (s NodeSnapshot) ModelState(modelID string) (string, bool) {
	for _, m := range s.Models {
		if m.ModelID == modelID {
			return m.State, true
		}
	}
	return "", false
}

// ProbeFunc probes one node agent. Injectable so tests stay hermetic.
type ProbeFunc func(ctx context.Context, host string, agentPort int) NodeSnapshot

var (
	selfHostsOnce sync.Once
	selfHosts     map[string]struct{}
)

// SelfHostnames returns the set of hostnames meaning "this machine", so a node
// whose configured host is our own is probed over loopback — dodging mDNS and
// overlay-address resolution games on the dual-homed hosts.
func SelfHostnames() map[string]struct{} {
	selfHostsOnce.Do(func() {
		selfHosts = map[string]struct{}{"localhost": {}}
		if h, err := os.Hostname(); err == nil && h != "" {
			selfHosts[h] = struct{}{}
			selfHosts[h+".local"] = struct{}{}
		}
	})
	return selfHosts
}

// ProbeNode fetches /health and /models from one node agent. A node that fails
// /health is reported unreachable and /models is not attempted.
func ProbeNode(ctx context.Context, host string, agentPort int) NodeSnapshot {
	snap := NodeSnapshot{ProbedAt: time.Now()}
	if _, self := SelfHostnames()[host]; self {
		host = "127.0.0.1"
	}
	base := "http://" + host + ":" + strconv.Itoa(agentPort)

	h := fetchJSON[AgentHealth](ctx, base+"/health")
	if h == nil {
		return snap
	}
	snap.Reachable = true
	snap.Health = h

	if models := fetchJSON[[]AgentModel](ctx, base+"/models"); models != nil {
		snap.Models = *models
	}
	return snap
}

// fetchJSON GETs a URL and decodes it, returning nil on any failure. Callers
// treat nil as "no data", never as an error to propagate: a probe is
// best-effort by construction.
func fetchJSON[T any](ctx context.Context, url string) *T {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil
	}
	return &v
}
