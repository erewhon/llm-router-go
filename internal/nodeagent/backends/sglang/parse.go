package sglang

import (
	"strconv"
	"strings"
)

// metricsSnapshot is the subset of Prometheus metrics we extract from an
// SGLang or vLLM /metrics endpoint. Both engines share the protocol but
// expose slightly different metric names; the parser accepts both.
type metricsSnapshot struct {
	running      int
	waiting      int
	total        int
	avgTokPerSec float64

	// genTokens is the cumulative generated-token counter (Atlas'
	// atlas_generation_tokens_total). Atlas exposes no throughput gauge or
	// per-output-token latency histogram, so the driver turns this counter
	// into a rate across successive scrapes. hasGenTokens distinguishes a
	// genuine 0 from the metric being absent (SGLang/vLLM don't emit it).
	genTokens    float64
	hasGenTokens bool
}

// parseMetrics reads Prometheus text-format exposition and pulls out the
// running/waiting/total request counts and an average tokens-per-second
// figure.
//
// Preferred avg_tok_per_s sources, in order:
//  1. SGLang's `gen_throughput` gauge (already tok/s).
//  2. The `inter_token_latency_seconds` histogram (SGLang) or
//     `time_per_output_token_seconds` (vLLM) — sum/count gives mean
//     latency per token, which inverts to tok/s.
//
// Atlas exposes neither, only the `atlas_generation_tokens_total` counter;
// parseMetrics records it (genTokens/hasGenTokens) and leaves avgTokPerSec
// at 0 so the stateful driver can turn it into a rate across scrapes.
func parseMetrics(data []byte) metricsSnapshot {
	var m metricsSnapshot
	var ttSum float64
	var ttCount int
	var gen float64

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		spaceIdx := strings.LastIndex(line, " ")
		if spaceIdx == -1 {
			continue
		}
		left := strings.TrimSpace(line[:spaceIdx])
		valStr := strings.TrimSpace(line[spaceIdx+1:])
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		// Strip {labels} for the metric-name switch.
		metric := left
		if i := strings.IndexByte(metric, '{'); i >= 0 {
			metric = metric[:i]
		}
		// Recent SGLang versions prefix every custom metric with "sglang:".
		// Strip it so the switch matches both new (prefixed) and older
		// (unprefixed) layouts; the Python agent currently misses the
		// prefixed form.
		metric = strings.TrimPrefix(metric, "sglang:")
		switch metric {
		case "num_requests_running", "num_running_reqs":
			m.running = int(val)
		case "num_requests_waiting", "num_queue_reqs":
			m.waiting = int(val)
		case "num_requests_total":
			m.total = int(val)
		case "gen_throughput":
			gen = val
		case "inter_token_latency_seconds_sum", "time_per_output_token_seconds_sum":
			ttSum = val
		case "inter_token_latency_seconds_count", "time_per_output_token_seconds_count":
			ttCount = int(val)

		// Atlas (Avarok) exposes atlas_*-prefixed metrics and, unlike
		// SGLang/vLLM, neither a throughput gauge nor a per-output-token
		// latency histogram — only counters (its one histogram is
		// time-to-FIRST-token, i.e. prefill, not decode). Surface the
		// request gauges directly and hand the generated-token counter to
		// the driver, which derives tok/s as a rate across scrapes.
		case "atlas_requests_active":
			m.running = int(val)
		case "atlas_requests_total":
			m.total = int(val)
		case "atlas_generation_tokens_total":
			m.genTokens = val
			m.hasGenTokens = true
		}
	}

	switch {
	case gen > 0:
		m.avgTokPerSec = roundOneDecimal(gen)
	case ttSum > 0 && ttCount > 0:
		avgTime := ttSum / float64(ttCount)
		if avgTime > 0 {
			m.avgTokPerSec = roundOneDecimal(1.0 / avgTime)
		}
	}
	return m
}

func roundOneDecimal(v float64) float64 {
	if v < 0 {
		return -roundOneDecimal(-v)
	}
	return float64(int(v*10+0.5)) / 10
}
