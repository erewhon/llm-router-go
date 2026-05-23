package nodeagent

// JSON request/response types for the node-agent API.
// Field tags mirror the Python Pydantic schema in
// src/llm_router/node_agent/models.py so existing clients
// (dashboard, LiteLLM hook, TUI) keep working unchanged.

type ModelState string

const (
	StateStopped  ModelState = "stopped"
	StateStarting ModelState = "starting"
	StateRunning  ModelState = "running"
	StateError    ModelState = "error"
)

type HealthResponse struct {
	Status        string          `json:"status"`
	Node          string          `json:"node"`
	GPUType       *string         `json:"gpu_type,omitempty"`
	TotalVRAMGB   *float64        `json:"total_vram_gb,omitempty"`
	FreeVRAMGB    *float64        `json:"free_vram_gb,omitempty"`
	GPUBusyPct    *int            `json:"gpu_busy_pct,omitempty"`
	DiskFreeGB    *float64        `json:"disk_free_gb,omitempty"`
	DiskTotalGB   *float64        `json:"disk_total_gb,omitempty"`
	RunningModels []string        `json:"running_models"`
	Services      []ServiceStatus `json:"services"`
}

type ServiceStatus struct {
	Name        string   `json:"name"`
	ServiceType string   `json:"service_type"`
	Label       string   `json:"label,omitempty"`
	Reachable   bool     `json:"reachable"`
	VRAMUsedGB  *float64 `json:"vram_used_gb,omitempty"`
	VRAMTotalGB *float64 `json:"vram_total_gb,omitempty"`
	QueueRun    int      `json:"queue_running"`
	QueuePend   int      `json:"queue_pending"`
}

type ModelStatusResponse struct {
	ModelID string     `json:"model_id"`
	State   ModelState `json:"state"`
	PID     *int       `json:"pid,omitempty"`
	Port    *int       `json:"port,omitempty"`
	Backend string     `json:"backend,omitempty"`
	HFRepo  string     `json:"hf_repo,omitempty"`
	Error   string     `json:"error,omitempty"`
}

type ModelListEntry struct {
	ModelID         string     `json:"model_id"`
	State           ModelState `json:"state"`
	HFRepo          string     `json:"hf_repo"`
	Backend         string     `json:"backend"`
	AlwaysOn        bool       `json:"always_on"`
	VRAMGB          int        `json:"vram_gb"`
	RequestsRunning int        `json:"requests_running"`
	RequestsWaiting int        `json:"requests_waiting"`
	AvgTokPerS      *float64   `json:"avg_tok_per_s,omitempty"`
	TotalRequests   int        `json:"total_requests"`
}
