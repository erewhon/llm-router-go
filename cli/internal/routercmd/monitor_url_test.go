package routercmd

import "testing"

func TestMonitorURLFallsBackToPitfEnv(t *testing.T) {
	t.Setenv("PITF_MONITOR_URL", "http://127.0.0.1:8070")
	if got := monitorURL(""); got != "http://127.0.0.1:8070" {
		t.Errorf("env fallback: got %q", got)
	}
	if got := monitorURL("http://127.0.0.1:9000"); got != "http://127.0.0.1:9000" {
		t.Errorf("flag must win: got %q", got)
	}
	t.Setenv("PITF_MONITOR_URL", "")
	if got := monitorURL(""); got != "" {
		t.Errorf("unset: got %q", got)
	}
}
