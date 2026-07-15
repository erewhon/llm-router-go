// Package sglang drives the SGLang (and vLLM-compatible) inference
// engine. Both engines expose the same OpenAI-style `/v1/models` API
// and Prometheus `/metrics` endpoint, so a single probe handles both.
// On the user's fleet, archimedes and hypatia run SGLang containers;
// the legacy vLLM image is a stopped fallback.
//
// Phase 1b implements probing only. Start/stop lifecycle (Docker for
// SGLang, systemd for vLLM) lands later.
package sglang

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/nodeagent/backends"
)

// DefaultPort is the conventional SGLang port on the user's fleet.
const DefaultPort = 5391

// Backend probes an SGLang, vLLM, or Atlas backend over HTTP (all three
// share the OpenAI /v1/models API and a Prometheus /metrics endpoint).
type Backend struct {
	host   string
	client *http.Client

	// prevGen holds the last (token counter, timestamp) per model so the
	// driver can derive tok/s from Atlas' monotonic
	// atlas_generation_tokens_total counter across on-demand scrapes.
	// SGLang/vLLM report throughput directly and don't touch this.
	// Guarded by mu; keyed by modelID (a node may host more than one model).
	mu      sync.Mutex
	prevGen map[string]genSample
}

// genSample is one reading of a cumulative generated-token counter.
type genSample struct {
	tokens float64
	at     time.Time
}

// New returns a Backend that probes host:<port>. host is typically
// "localhost" — the agent runs on the same node as the engine.
func New(host string) *Backend {
	if host == "" {
		host = "localhost"
	}
	return &Backend{
		host: host,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		prevGen: map[string]genSample{},
	}
}

// Status implements backends.Backend.
func (b *Backend) Status(ctx context.Context, modelID string, model *config.ModelDefinition) backends.Status {
	port := DefaultPort
	if model != nil && model.APIPort != 0 {
		port = model.APIPort
	}
	portCopy := port

	s := backends.Status{
		ModelID: modelID,
		State:   backends.StateStopped,
		Port:    &portCopy,
	}

	served, reachErr := b.checkServing(ctx, port, model)
	switch {
	case reachErr != nil && isUnreachable(reachErr):
		// Connection refused, no route, timeout — the engine isn't up.
		s.Port = nil
		return s
	case reachErr != nil:
		// Real malfunction: 5xx, garbage body, etc.
		s.State = backends.StateError
		s.Error = reachErr.Error()
		return s
	case !served:
		// Upstream is up but serving a different model — from our
		// model's point of view it's stopped, not errored. The other
		// model has the slot; ours isn't here yet.
		return s
	}

	s.State = backends.StateRunning

	if snap, err := b.fetchMetrics(ctx, port); err == nil {
		s.RequestsRunning = snap.running
		s.RequestsWaiting = snap.waiting
		s.TotalRequests = snap.total
		switch {
		case snap.avgTokPerSec > 0:
			// SGLang/vLLM expose throughput directly (gauge or histogram).
			v := snap.avgTokPerSec
			s.AvgTokPerSec = &v
		case snap.hasGenTokens:
			// Atlas exposes only a cumulative token counter; derive a rate
			// from the delta since this model's previous scrape.
			if v, ok := b.tokRate(modelID, snap.genTokens, time.Now()); ok {
				s.AvgTokPerSec = &v
			}
		}
	}

	return s
}

// tokRate turns Atlas' monotonic atlas_generation_tokens_total counter into an
// average tokens/sec over the interval since modelID was last probed. It
// returns (0, false) on the first sample, when no time elapsed, when the
// counter went backwards (an engine restart resets it), or when the interval
// was idle (no new tokens) — matching the gauge/histogram path's convention of
// reporting only when >0. Because probing is on-demand, the rate reflects the
// dashboard's refresh interval: accurate during sustained generation, 0 (nil)
// when idle.
func (b *Backend) tokRate(modelID string, tokens float64, now time.Time) (float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.prevGen == nil {
		b.prevGen = map[string]genSample{}
	}
	prev, had := b.prevGen[modelID]
	b.prevGen[modelID] = genSample{tokens: tokens, at: now}
	if !had {
		return 0, false
	}
	dt := now.Sub(prev.at).Seconds()
	if dt <= 0 {
		return 0, false
	}
	delta := tokens - prev.tokens
	if delta <= 0 {
		return 0, false
	}
	return roundOneDecimal(delta / dt), true
}

// checkServing fetches /v1/models. It returns (true, nil) when the
// upstream is reachable AND advertises the expected hf_repo. If model is
// nil, reachability alone is enough.
func (b *Backend) checkServing(ctx context.Context, port int, model *config.ModelDefinition) (bool, error) {
	target := fmt.Sprintf("http://%s/v1/models", net.JoinHostPort(b.host, fmt.Sprintf("%d", port)))
	if _, err := url.Parse(target); err != nil {
		return false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status %d from /v1/models", resp.StatusCode)
	}

	if model == nil {
		return true, nil
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("decode /v1/models: %w", err)
	}

	// Some upstream model IDs include a "#suffix" (e.g. "#nothink") in
	// hf_repo to pick a chat template — strip it for comparison.
	want := strings.SplitN(model.HFRepo, "#", 2)[0]
	for _, m := range body.Data {
		if m.ID == want || m.ID == model.HFRepo {
			return true, nil
		}
	}
	return false, nil
}

// fetchMetrics scrapes /metrics and parses it.
func (b *Backend) fetchMetrics(ctx context.Context, port int) (metricsSnapshot, error) {
	target := fmt.Sprintf("http://%s/metrics", net.JoinHostPort(b.host, fmt.Sprintf("%d", port)))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return metricsSnapshot{}, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return metricsSnapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return metricsSnapshot{}, fmt.Errorf("metrics status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return metricsSnapshot{}, err
	}
	return parseMetrics(body), nil
}

// isUnreachable reports whether err looks like the upstream simply isn't
// listening (connection refused, no route to host, timeout). Such cases
// map to StateStopped, while other errors surface as StateError.
func isUnreachable(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	msg := err.Error()
	for _, frag := range []string{
		"connection refused",
		"no such host",
		"no route to host",
		"i/o timeout",
		"context deadline exceeded",
		"EOF",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}
