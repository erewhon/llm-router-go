// Package backends defines the interface every inference backend driver
// implements. A driver knows how to probe a model's local backend
// process, report its state, and (eventually) start/stop it. Status is
// the only required operation in Phase 1b — start/stop arrives later.
package backends

import (
	"context"

	"github.com/erewhon/llm-router-go/internal/config"
)

// State is the lifecycle state of a model's inference backend. The
// string values exactly match the Python enum
// (src/llm_router/node_agent/models.py:ModelState) so JSON responses
// remain compatible across the migration.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateError    State = "error"
)

// Status is what a backend reports for a given model.
type Status struct {
	ModelID string
	State   State
	PID     *int   // optional, set when known
	Port    *int   // optional, set when known
	Error   string // set when State == StateError

	// Live request stats from the upstream /metrics endpoint. Zero values
	// when the backend isn't running or doesn't expose them.
	RequestsRunning int
	RequestsWaiting int
	TotalRequests   int
	AvgTokPerSec    *float64 // nil when unknown
}

// Backend is the driver interface every engine implements.
type Backend interface {
	// Status probes the backend and returns its current state.
	// model may be nil if the caller doesn't have the definition handy;
	// drivers that need it (e.g. for hf_repo verification) should report
	// StateError when probing without the definition is ambiguous.
	Status(ctx context.Context, modelID string, model *config.ModelDefinition) Status
}
