package tools

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// HTTPClientConfig configures the network tools' shared http.Client.
type HTTPClientConfig struct {
	// SOCKS5 is the proxy URL. Accepted forms:
	//   "" (or omitted)              — direct connection, no proxy.
	//   "host:port"                  — bare SOCKS5 (matches MEMORY note
	//                                  about not using "socks5h://").
	//   "socks5://host:port"         — explicit scheme.
	// socks5h:// is treated the same as socks5:// because Go's
	// proxy.SOCKS5 always asks the proxy to resolve hostnames (which is
	// the curl `socks5h` behaviour); the user's Mullvad container needs
	// that to keep DNS off the local resolver.
	SOCKS5 string
	// Timeout caps the total request duration. Zero uses 15s.
	Timeout time.Duration
}

// NewHTTPClient builds a *http.Client honouring cfg. The returned client
// is safe for concurrent use; both tools share one to avoid hammering
// the VPN container with separate dialers.
func NewHTTPClient(cfg HTTPClientConfig) (*http.Client, error) {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}

	transport := &http.Transport{
		// Tighter dial timeout than the overall request timeout so we
		// fail fast when the VPN container is down rather than burning
		// the full 15s on every retry.
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}

	if cfg.SOCKS5 != "" {
		cd, err := NewSOCKS5Dialer(cfg.SOCKS5)
		if err != nil {
			return nil, err
		}
		transport.DialContext = cd.DialContext
		// Disable HTTP env proxy when we already have an explicit SOCKS5
		// configured, so an unrelated HTTPS_PROXY in the systemd unit
		// can't override it.
		transport.Proxy = nil
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}

// Dialer is both a proxy.Dialer and a proxy.ContextDialer. golang.org/x/net's
// SOCKS5 dialer satisfies both; the combined type lets callers use it as the
// "forward" dialer for proxy.SOCKS5 (wants Dialer) AND for transport.DialContext
// (wants ContextDialer) without re-asserting.
type Dialer interface {
	proxy.Dialer
	proxy.ContextDialer
}

// NewSOCKS5Dialer builds a context-aware SOCKS5 dialer for the given proxy
// address (forms accepted by parseSOCKS5). Used both for the tools' default
// client and as the tunnel-entry "forward" dialer for per-request egress
// selection (internal/toolproxy/egress).
func NewSOCKS5Dialer(socks5 string) (Dialer, error) {
	host, err := parseSOCKS5(socks5)
	if err != nil {
		return nil, err
	}
	dialer, err := proxy.SOCKS5("tcp", host, nil, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("tools: SOCKS5 dialer: %w", err)
	}
	cd, ok := dialer.(Dialer)
	if !ok {
		return nil, fmt.Errorf("tools: SOCKS5 dialer not context-aware")
	}
	return cd, nil
}

// parseSOCKS5 accepts "host:port", "socks5://host:port", or
// "socks5h://host:port" and returns "host:port" for proxy.SOCKS5.
func parseSOCKS5(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("tools: empty SOCKS5 address")
	}
	if !strings.Contains(raw, "://") {
		// bare host:port form
		return validateHostPort(raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("tools: parse SOCKS5 URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		// ok
	default:
		return "", fmt.Errorf("tools: unsupported SOCKS5 scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("tools: SOCKS5 URL %q missing host", raw)
	}
	return validateHostPort(u.Host)
}

// validateHostPort returns hostPort unchanged after asserting it splits
// into a host and a numeric port. "host:port" with a non-numeric port
// fails — net.SplitHostPort alone accepts it.
func validateHostPort(hostPort string) (string, error) {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return "", fmt.Errorf("tools: invalid SOCKS5 address %q: %w", hostPort, err)
	}
	if host == "" {
		return "", fmt.Errorf("tools: SOCKS5 address %q missing host", hostPort)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return "", fmt.Errorf("tools: SOCKS5 port in %q not numeric", hostPort)
	}
	return hostPort, nil
}

