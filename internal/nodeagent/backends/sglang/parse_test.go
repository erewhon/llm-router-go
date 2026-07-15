package sglang

import "testing"

func TestParseMetrics_SGLang(t *testing.T) {
	// Real SGLang exposition fragment, simplified.
	body := []byte(`
# HELP num_running_reqs Number of running requests
# TYPE num_running_reqs gauge
num_running_reqs{name="nemotron-3-super"} 3.0
# HELP num_queue_reqs Number of queued requests
# TYPE num_queue_reqs gauge
num_queue_reqs{name="nemotron-3-super"} 1.0
# HELP gen_throughput Tokens per second
# TYPE gen_throughput gauge
gen_throughput{name="nemotron-3-super"} 42.7
# HELP inter_token_latency_seconds Histogram of inter-token latency
# TYPE inter_token_latency_seconds histogram
inter_token_latency_seconds_sum{name="nemotron-3-super"} 0.10
inter_token_latency_seconds_count{name="nemotron-3-super"} 5
num_requests_total{name="nemotron-3-super"} 100
`)
	m := parseMetrics(body)
	if m.running != 3 || m.waiting != 1 || m.total != 100 {
		t.Errorf("counts wrong: %+v", m)
	}
	if m.avgTokPerSec != 42.7 {
		t.Errorf("avg tok/s = %v, want 42.7 (from gen_throughput)", m.avgTokPerSec)
	}
}

func TestParseMetrics_vLLMFallback(t *testing.T) {
	// vLLM doesn't have gen_throughput; falls back to histogram math.
	// avg time per token = 0.05s → 20 tok/s.
	body := []byte(`
num_requests_running{model="qwen"} 2
num_requests_waiting{model="qwen"} 0
time_per_output_token_seconds_sum{model="qwen"} 5.0
time_per_output_token_seconds_count{model="qwen"} 100
`)
	m := parseMetrics(body)
	if m.running != 2 || m.waiting != 0 {
		t.Errorf("counts wrong: %+v", m)
	}
	if m.avgTokPerSec != 20.0 {
		t.Errorf("avg tok/s = %v, want 20.0", m.avgTokPerSec)
	}
}

func TestParseMetrics_NoMetrics(t *testing.T) {
	m := parseMetrics(nil)
	if m.running != 0 || m.waiting != 0 || m.avgTokPerSec != 0 {
		t.Errorf("empty input should give zero snapshot, got %+v", m)
	}
}

func TestParseMetrics_IgnoresGarbage(t *testing.T) {
	body := []byte(`
# this is a comment
nonsense_line_without_value
num_running_reqs nope
num_running_reqs{x="y"} 7
`)
	m := parseMetrics(body)
	if m.running != 7 {
		t.Errorf("running = %d, want 7", m.running)
	}
}

func TestParseMetrics_SGLangPrefix(t *testing.T) {
	// Real-world SGLang fragment from archimedes (Nemotron-3 Super).
	body := []byte(`
# HELP sglang:num_running_reqs The number of running requests.
# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs{engine_type="unified",model_name="nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4",moe_ep_rank="0",pid="82",pp_rank="0",tp_rank="0"} 2.0
sglang:num_queue_reqs{engine_type="unified",model_name="nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4",moe_ep_rank="0",pid="82",pp_rank="0",tp_rank="0"} 1.0
sglang:gen_throughput{engine_type="unified",model_name="nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4",moe_ep_rank="0",pid="82",pp_rank="0",tp_rank="0"} 38.5
sglang:num_requests_total{model_name="nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4"} 579.0
`)
	m := parseMetrics(body)
	if m.running != 2 || m.waiting != 1 || m.total != 579 {
		t.Errorf("prefixed metrics not parsed: %+v", m)
	}
	if m.avgTokPerSec != 38.5 {
		t.Errorf("avg tok/s = %v, want 38.5 (from sglang:gen_throughput)", m.avgTokPerSec)
	}
}

func TestParseMetrics_NoLabels(t *testing.T) {
	body := []byte(`
num_running_reqs 4
num_queue_reqs 2
gen_throughput 33.3
`)
	m := parseMetrics(body)
	if m.running != 4 || m.waiting != 2 {
		t.Errorf("counts wrong: %+v", m)
	}
	if m.avgTokPerSec != 33.3 {
		t.Errorf("avg tok/s = %v, want 33.3", m.avgTokPerSec)
	}
}

func TestRoundOneDecimal(t *testing.T) {
	cases := map[float64]float64{
		42.75:  42.8,
		42.74:  42.7,
		0:      0,
		-3.55:  -3.6,
		100.0:  100.0,
	}
	for in, want := range cases {
		if got := roundOneDecimal(in); got != want {
			t.Errorf("roundOneDecimal(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestParseMetrics_AtlasRequestGauges(t *testing.T) {
	// Atlas exposes atlas_*-prefixed request gauges; tok/s is measured at the
	// router, not here, so avgTokPerSec stays 0.
	body := []byte(`
# TYPE atlas_requests_active gauge
atlas_requests_active 2
# TYPE atlas_requests_total counter
atlas_requests_total 100
# TYPE atlas_generation_tokens_total counter
atlas_generation_tokens_total 12345
`)
	m := parseMetrics(body)
	if m.running != 2 {
		t.Errorf("running = %d, want 2", m.running)
	}
	if m.total != 100 {
		t.Errorf("total = %d, want 100", m.total)
	}
	if m.avgTokPerSec != 0 {
		t.Errorf("avgTokPerSec = %v, want 0 (router measures Atlas tok/s)", m.avgTokPerSec)
	}
}
