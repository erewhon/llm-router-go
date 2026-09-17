package health

import (
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

// A TP=2 pair: the head serves the API, the worker rank is headless. The
// worker's agent lists the model but sees no listener, so it reports
// "stopped" — that must not take a healthy group down.
func TestObserveModel_MultiNodeWorkerStateIsIgnored(t *testing.T) {
	m := config.ModelDefinition{
		Backend:   config.BackendVLLM,
		MultiNode: &config.MultiNodeConfig{Nodes: []string{"archimedes", "hypatia"}, TensorParallelSize: 2, HeadNode: "archimedes"},
	}
	snaps := map[string]NodeSnapshot{
		"archimedes": {Reachable: true, Models: []AgentModel{{ModelID: "glm-pair", State: StateRunning}}},
		"hypatia":    {Reachable: true, Models: []AgentModel{{ModelID: "glm-pair", State: "stopped"}}},
	}
	if ok, why := observeModel("glm-pair", m, snaps); !ok {
		t.Fatalf("healthy pair judged down: %s", why)
	}

	// The head's verdict still counts.
	snaps["archimedes"] = NodeSnapshot{Reachable: true, Models: []AgentModel{{ModelID: "glm-pair", State: "stopped"}}}
	if ok, why := observeModel("glm-pair", m, snaps); ok || why != `agent on "archimedes" reports state "stopped"` {
		t.Fatalf("head stopped must be down: ok=%v why=%q", ok, why)
	}

	// A worker that is unreachable takes the group down: the NCCL group
	// cannot survive it, whatever the head's agent last said.
	snaps["archimedes"] = NodeSnapshot{Reachable: true, Models: []AgentModel{{ModelID: "glm-pair", State: StateRunning}}}
	snaps["hypatia"] = NodeSnapshot{Reachable: false}
	if ok, why := observeModel("glm-pair", m, snaps); ok || why != `node "hypatia" unreachable` {
		t.Fatalf("unreachable worker must be down: ok=%v why=%q", ok, why)
	}
}

func TestObserveModel_HeadDefaultsToFirstNode(t *testing.T) {
	m := config.ModelDefinition{Backend: config.BackendVLLM,
		MultiNode: &config.MultiNodeConfig{Nodes: []string{"a", "b"}, TensorParallelSize: 2}}
	snaps := map[string]NodeSnapshot{
		"a": {Reachable: true, Models: []AgentModel{{ModelID: "x", State: StateRunning}}},
		"b": {Reachable: true, Models: []AgentModel{{ModelID: "x", State: "stopped"}}},
	}
	if ok, why := observeModel("x", m, snaps); !ok {
		t.Fatalf("first node is the head by default: %s", why)
	}
}
