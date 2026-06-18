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
	Logger        *slog.Logger
}

// Selector turns an X-Egress spec into the http.Client a request's web tools
// should use. Relay clients are built lazily and cached per relay so connection
// pools are reused. Safe for concurrent use.
type Selector struct {
	cfg     Config
	mu      sync.Mutex
	clients map[string]*http.Client
}

// NewSelector builds a Selector (zero-value fields defaulted).
func NewSelector(cfg Config) *Selector {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ClientTimeout <= 0 {
		cfg.ClientTimeout = 15 * time.Second
	}
	return &Selector{cfg: cfg, clients: map[string]*http.Client{}}
}

// ClientFor returns the http.Client a request's web tools should use, plus a
// human label of the chosen exit. Empty / "default" / "none" → the base client
// (current exit), label "default". Any resolution or dialer error returns the
// base client AND the error, so callers fall back gracefully while still
// logging the miss.
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
	client, err := s.clientForRelay(relay)
	if err != nil {
		return s.cfg.BaseClient, "default", err
	}
	return client, relay.Hostname, nil
}

func (s *Selector) clientForRelay(r Relay) (*http.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[r.SocksName]; ok {
		return c, nil
	}
	// Nested SOCKS: dial the relay's SOCKS5 *through* the tunnel-entry forward
	// dialer (microsocks). microsocks resolves the relay hostname in-tunnel
	// (→ 10.124.x) and connects; the relay then egresses from its own location.
	relayDialer, err := proxy.SOCKS5("tcp", r.addr(), nil, s.cfg.Forward)
	if err != nil {
		return nil, fmt.Errorf("egress: relay SOCKS5 dialer %s: %w", r.SocksName, err)
	}
	cd, ok := relayDialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("egress: relay dialer not context-aware")
	}
	transport := &http.Transport{
		DialContext:           cd.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	c := &http.Client{Transport: transport, Timeout: s.cfg.ClientTimeout}
	s.clients[r.SocksName] = c
	return c, nil
}
