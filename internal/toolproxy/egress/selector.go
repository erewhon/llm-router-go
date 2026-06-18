package egress

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// Config configures a Selector.
type Config struct {
	// Forward is the dialer that enters the existing VPN tunnel (microsocks).
	// Required to build relay clients; if nil, only the default exit works.
	Forward proxy.Dialer
	// BaseClient is the default-exit client (the tools' normal SOCKS client).
	// Returned for empty/"default" specs and as the fallback on any error.
	BaseClient *http.Client
	// Catalogue resolves specs to relays.
	Catalogue *Catalogue
	// DefaultSpec applies when a request sends no X-Egress (usually "").
	DefaultSpec string
	// ClientTimeout caps relay-client requests. Zero uses 15s.
	ClientTimeout time.Duration
	// MaxTries caps how many relays a single request will try before failing
	// (failover on a dead relay). Zero uses 3.
	MaxTries int
	Logger   *slog.Logger
}

// Selector turns an X-Egress spec into the http.Client a request's web tools
// should use, with failover across relays matching the spec. Per-relay
// transports are cached so connection pools are reused. Safe for concurrent use.
type Selector struct {
	cfg        Config
	maxTries   int
	mu         sync.Mutex
	transports map[string]http.RoundTripper

	// roundTripVia is the per-relay round-trip; a field so tests can stub it.
	roundTripVia func(relay Relay, req *http.Request) (*http.Response, error)
}

// NewSelector builds a Selector (zero-value fields defaulted).
func NewSelector(cfg Config) *Selector {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ClientTimeout <= 0 {
		cfg.ClientTimeout = 15 * time.Second
	}
	maxTries := cfg.MaxTries
	if maxTries <= 0 {
		maxTries = 3
	}
	s := &Selector{cfg: cfg, maxTries: maxTries, transports: map[string]http.RoundTripper{}}
	s.roundTripVia = s.realRoundTripVia
	return s
}

// ClientFor returns the http.Client a request's web tools should use, plus a
// human label of the chosen exit. Empty / "default" / "none" → the base client
// (current exit), label "default". Any resolution or dialer error returns the
// base client AND the error, so callers fall back gracefully while still logging
// the miss. For a relay spec the returned client fails over to other relays
// matching the spec (up to MaxTries) when one is dead.
func (s *Selector) ClientFor(ctx context.Context, spec string) (*http.Client, string, error) {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if spec == "" {
		spec = strings.ToLower(strings.TrimSpace(s.cfg.DefaultSpec))
	}
	if spec == "" || spec == "default" || spec == "none" {
		return s.cfg.BaseClient, "default", nil
	}
	if s.cfg.Forward == nil || s.cfg.Catalogue == nil {
		return s.cfg.BaseClient, "default", fmt.Errorf("egress: selection unavailable (no forward dialer/catalogue)")
	}
	relay, err := s.cfg.Catalogue.Resolve(ctx, spec)
	if err != nil {
		return s.cfg.BaseClient, "default", err
	}
	rt := &retryTransport{
		sel:      s,
		spec:     spec,
		maxTries: s.maxTries,
		relay:    relay,
		haveRelay: true,
		tried:    map[string]bool{},
	}
	return &http.Client{Transport: rt, Timeout: s.cfg.ClientTimeout}, relay.Hostname, nil
}

// realRoundTripVia builds (or reuses) a per-relay transport and round-trips req.
func (s *Selector) realRoundTripVia(relay Relay, req *http.Request) (*http.Response, error) {
	tr, err := s.transportForRelay(relay)
	if err != nil {
		return nil, err
	}
	return tr.RoundTrip(req)
}

func (s *Selector) transportForRelay(relay Relay) (http.RoundTripper, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tr, ok := s.transports[relay.SocksName]; ok {
		return tr, nil
	}
	// Nested SOCKS: dial the relay's SOCKS5 *through* the tunnel-entry forward
	// dialer (microsocks). microsocks resolves the relay hostname in-tunnel
	// (→ 10.124.x) and connects; the relay then egresses from its own location.
	relayDialer, err := proxy.SOCKS5("tcp", relay.addr(), nil, s.cfg.Forward)
	if err != nil {
		return nil, fmt.Errorf("egress: relay SOCKS5 dialer %s: %w", relay.SocksName, err)
	}
	cd, ok := relayDialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("egress: relay dialer not context-aware")
	}
	tr := &http.Transport{
		DialContext:           cd.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	s.transports[relay.SocksName] = tr
	return tr, nil
}

// retryTransport round-trips through a relay matching its spec, failing over to
// another matching relay when one is dead. One instance per request (created by
// ClientFor); not shared.
type retryTransport struct {
	sel      *Selector
	spec     string
	maxTries int

	mu        sync.Mutex
	relay     Relay
	haveRelay bool
	tried     map[string]bool
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < t.maxTries; attempt++ {
		t.mu.Lock()
		if !t.haveRelay {
			r, err := t.sel.cfg.Catalogue.ResolveExcluding(req.Context(), t.spec, t.tried)
			if err != nil {
				t.mu.Unlock()
				if lastErr != nil {
					return nil, fmt.Errorf("egress: no more relays for %q after failures: %w (last: %v)", t.spec, err, lastErr)
				}
				return nil, err
			}
			t.relay = r
			t.haveRelay = true
		}
		relay := t.relay
		t.mu.Unlock()

		// Rewind the body for retries (POST tools use a rewindable body, so
		// GetBody is set). Nothing was consumed on a dial failure, but rewinding
		// keeps higher-level errors safe to retry too.
		if attempt > 0 && req.GetBody != nil {
			if b, berr := req.GetBody(); berr == nil {
				req.Body = b
			}
		}

		resp, err := t.sel.roundTripVia(relay, req)
		if err == nil {
			return resp, nil
		}
		if req.Context().Err() != nil {
			return nil, err // cancelled/timed-out by caller: don't burn relays
		}
		lastErr = err
		t.sel.cfg.Logger.Warn("egress: relay request failed; trying another",
			"spec", t.spec, "relay", relay.Hostname, "attempt", attempt+1, "err", err)
		t.mu.Lock()
		t.tried[relay.SocksName] = true
		t.haveRelay = false
		t.mu.Unlock()
	}
	return nil, fmt.Errorf("egress: spec %q exhausted %d relays: %w", t.spec, t.maxTries, lastErr)
}
