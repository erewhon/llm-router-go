package sglang

import (
	"strconv"
	"strings"
)

// metricsSnapshot is the subset of Prometheus metrics we extract from an
// SGLang or vLLM /metrics endpoint. Both engines share the protocol but
// expose slightly different metric names; the parser accepts both.
type metricsSnapshot struct {
	running       int
	waiting       int
	total         int
	avgTokPerSec  float64
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

		// Atlas (Avarok) request gauges. Atlas also exposes token counters,
		// but atlas_generation_tokens_total is bumped only at request end and
		// it has no throughput gauge, so per-response tok/s is measured at the
		// router (from usage.response_token/s) rather than scraped here.
		case "atlas_requests_active":
			m.running = int(val)
		case "atlas_requests_total":
			m.total = int(val)
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
