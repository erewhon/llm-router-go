package router

import (
	"sync"
	"time"
)

// tokStatTTL is how long a model's most recent throughput reading keeps being
// reported on the dashboard after the response that produced it — long enough
// to survive brief gaps between requests, short enough that a stale number
// doesn't linger once a model goes quiet.
const tokStatTTL = 30 * time.Second

// tokTracker holds the most recent tokens/sec per model, measured from the
// responses the router proxies: the engine's own usage.response_token/s when
// present (Atlas reports it), otherwise completion_tokens / latency. This is
// the same figure a streaming chat UI computes, centralised at the front door
// so the dashboard's throughput tile works for every backend uniformly
// (SGLang/vLLM/Atlas) without scraping per-engine Prometheus metrics.
type tokTracker struct {
	mu sync.Mutex
	m  map[string]tokSample
}

type tokSample struct {
	tokPerSec float64
	at        time.Time
}

func newTokTracker() *tokTracker {
	return &tokTracker{m: make(map[string]tokSample)}
}

// record stores a throughput reading for modelID from one completed response.
// engineTPS is the engine-reported response_token/s (nil when absent); it's
// preferred over the completion/latency estimate. It's a no-op when neither
// yields a positive rate (embeddings, errors, zero-token responses, or a
// stream whose usage chunk was suppressed), so the previous reading holds
// until tokStatTTL expires rather than being clobbered with a zero.
func (t *tokTracker) record(modelID string, engineTPS *float64, completionTokens *int, latencyMS int, now time.Time) {
	if t == nil || modelID == "" {
		return
	}
	var tps float64
	switch {
	case engineTPS != nil && *engineTPS > 0:
		tps = *engineTPS
	case completionTokens != nil && *completionTokens > 0 && latencyMS > 0:
		tps = float64(*completionTokens) / (float64(latencyMS) / 1000.0)
	default:
		return
	}
	t.mu.Lock()
	t.m[modelID] = tokSample{tokPerSec: tps, at: now}
	t.mu.Unlock()
}

// get returns modelID's most recent throughput if it's newer than tokStatTTL.
func (t *tokTracker) get(modelID string, now time.Time) (float64, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[modelID]
	if !ok || now.Sub(s.at) > tokStatTTL {
		return 0, false
	}
	return roundTokPerSec(s.tokPerSec), true
}

func roundTokPerSec(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return float64(int(v*10+0.5)) / 10
}
