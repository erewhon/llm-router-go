package health

import (
	"encoding/json"
	"testing"
)

// The agent's /health carries gpu_temp_c / gpu_power_w (and per-card temp_c /
// power_w); an older agent omits them and the fields stay nil.
func TestAgentHealth_ThermalFieldsDecode(t *testing.T) {
	var h AgentHealth
	if err := json.Unmarshal([]byte(`{"gpu_busy_pct":3,"gpu_temp_c":72,"gpu_power_w":41.5,"gpus":[{"index":0,"temp_c":70,"power_w":20.5},{"index":1,"temp_c":72,"power_w":21}]}`), &h); err != nil {
		t.Fatal(err)
	}
	if h.GPUTempC == nil || *h.GPUTempC != 72 || h.GPUPowerW == nil || *h.GPUPowerW != 41.5 {
		t.Errorf("aggregate thermals = %v/%v", h.GPUTempC, h.GPUPowerW)
	}
	if len(h.GPUs) != 2 || h.GPUs[1].TempC == nil || *h.GPUs[1].TempC != 72 {
		t.Errorf("per-card thermals not decoded: %+v", h.GPUs)
	}
	var old AgentHealth
	if err := json.Unmarshal([]byte(`{"gpu_busy_pct":3}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.GPUTempC != nil || old.GPUPowerW != nil {
		t.Error("an old agent's response must leave the thermal fields nil")
	}
}
