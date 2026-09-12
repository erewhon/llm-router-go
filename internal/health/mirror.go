package health

// A Mirror is the read-only half of availability for processes that are not
// the router: it polls the router's GET /v1/availability instead of probing
// every node agent itself. One HTTP call rather than N, and — more
// importantly — the router is already the component that decides, so the tool
// proxy asking it directly means the two can never disagree about whether
// "coder" is answerable.
//
// A Mirror that cannot reach the router reports everything routable. That is
// deliberate: degrading to today's behaviour (route and find out) is far
// better than a tool proxy that refuses every request because its status feed
// is down.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// MirrorState is the parsed availability document.
type MirrorState struct {
	// routable holds every name — model id, alias, and role — the router says
	// can serve a request right now.
	routable map[string]bool
	// fresh is false when the last fetch failed, in which case Routable is
	// permissive.
	fresh bool
}

// mirrorDoc mirrors the router's availabilityResponse. Only the fields the
// proxy needs are decoded; the router is free to add more.
type mirrorDoc struct {
	Models []struct {
		Model string `json:"model"`
		State string `json:"state"`
	} `json:"models"`
	Roles []struct {
		Role      string `json:"role"`
		Available bool   `json:"available"`
		Target    string `json:"target"`
	} `json:"roles"`
	Tracking bool `json:"tracking"`
}

// Mirror polls a router's /v1/availability endpoint.
type Mirror struct {
	url      string
	bearer   string
	client   *http.Client
	logger   *slog.Logger
	interval time.Duration

	state atomic.Pointer[MirrorState]
}

// MirrorConfig configures a Mirror.
type MirrorConfig struct {
	// URL is the router base URL (e.g. http://127.0.0.1:4010). The
	// /v1/availability path is appended.
	URL string
	// Bearer authenticates to the router. /v1/availability sits under the
	// bearer-gated /v1/* prefix, so without this every fetch 401s and the
	// mirror silently degrades to "everything routable". The tool proxy
	// already holds a valid router key for its auto-route redirect; reuse it
	// rather than widening the router's auth exemption list.
	Bearer   string
	Client   *http.Client
	Logger   *slog.Logger
	Interval time.Duration
}

// NewMirror builds a Mirror. It does not start polling; call Run.
func NewMirror(cfg MirrorConfig) *Mirror {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 3 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	return &Mirror{
		url:      strings.TrimSuffix(cfg.URL, "/") + "/v1/availability",
		bearer:   cfg.Bearer,
		client:   cfg.Client,
		logger:   cfg.Logger,
		interval: cfg.Interval,
	}
}

// Run polls until ctx is cancelled, fetching once immediately.
func (m *Mirror) Run(ctx context.Context) {
	m.FetchOnce(ctx)
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.FetchOnce(ctx)
		}
	}
}

// FetchOnce refreshes the mirrored state. Exported for tests.
func (m *Mirror) FetchOnce(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.url, nil)
	if err != nil {
		m.markStale("build request: " + err.Error())
		return
	}
	if m.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+m.bearer)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		m.markStale(err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		m.markStale("HTTP " + resp.Status)
		return
	}
	var doc mirrorDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		m.markStale("decode: " + err.Error())
		return
	}

	routable := make(map[string]bool, len(doc.Models)+len(doc.Roles))
	for _, mo := range doc.Models {
		// Unknown counts as routable — the router has no evidence against it.
		// Warming does not: the listing is up but the seat cannot generate yet.
		// Absent does not: the base would answer for a different model.
		routable[mo.Model] = mo.State != string(Unavailable) && mo.State != string(Warming) &&
			mo.State != string(Absent)
	}
	for _, r := range doc.Roles {
		routable[r.Role] = r.Available
	}
	m.state.Store(&MirrorState{routable: routable, fresh: true})
}

func (m *Mirror) markStale(reason string) {
	prev := m.state.Load()
	if prev == nil || prev.fresh {
		// Log only on the transition, not on every failed poll.
		m.logger.Warn("availability mirror unreachable; treating everything as routable", "err", reason)
	}
	m.state.Store(&MirrorState{fresh: false})
}

// Routable reports whether a name (model id, alias, or role) can serve a
// request. Unknown names and a stale mirror both return true: this is a
// filter for things we KNOW are down, not an allowlist.
func (m *Mirror) Routable(name string) bool {
	st := m.state.Load()
	if st == nil || !st.fresh {
		return true
	}
	up, known := st.routable[name]
	if !known {
		return true
	}
	return up
}

// Fresh reports whether the mirror last fetched successfully.
func (m *Mirror) Fresh() bool {
	st := m.state.Load()
	return st != nil && st.fresh
}
