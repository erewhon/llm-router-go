package gpu

import (
	"context"
	"sync"
	"time"
)

// Cached wraps a Reader with a TTL cache that serves the last good
// snapshot while a refresh runs in the background. Only the very first
// Read blocks on the underlying probe; every later call returns instantly
// (a fresh value within the TTL, or the previous value while a background
// refresh runs). This keeps /health well under the dashboard's 1.5s probe
// timeout even on Intel hosts, where xpu-smi can take ~1.5s per call.
//
// It mirrors the Python agent's get_gpu_info_cached fix: the GPU probe is
// the one slow, blocking part of /health, so it must not run inline on
// every request.
func Cached(inner Reader, ttl time.Duration) Reader {
	return &cachedReader{inner: inner, ttl: ttl}
}

type cachedReader struct {
	inner Reader
	ttl   time.Duration

	mu         sync.Mutex
	last       Info
	lastErr    error
	fetchedAt  time.Time
	haveResult bool
	refreshing bool
}

func (c *cachedReader) Read(ctx context.Context) (Info, error) {
	c.mu.Lock()
	fresh := c.haveResult && time.Since(c.fetchedAt) < c.ttl
	if fresh {
		info, err := c.last, c.lastErr
		c.mu.Unlock()
		return info, err
	}
	if c.haveResult {
		// Stale but usable: serve it immediately and kick off a single
		// background refresh so concurrent readers never block on the
		// slow probe.
		if !c.refreshing {
			c.refreshing = true
			go c.refresh()
		}
		info, err := c.last, c.lastErr
		c.mu.Unlock()
		return info, err
	}
	// Cold cache (first call): block once to get an initial value.
	c.mu.Unlock()
	return c.refresh()
}

// refresh probes the underlying reader with a detached, time-capped
// context so a client cancelling mid-probe (e.g. the dashboard's 1.5s
// timeout) doesn't abort the probe and leave the cache cold. A failed
// probe keeps the previous good snapshot rather than blanking it.
func (c *cachedReader) refresh() (Info, error) {
	rctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	info, err := c.inner.Read(rctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = false
	if err == nil || !c.haveResult {
		c.last, c.lastErr = info, err
		c.haveResult = true
	}
	c.fetchedAt = time.Now()
	return c.last, c.lastErr
}
