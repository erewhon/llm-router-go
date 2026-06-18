// Package egress selects a Mullvad VPN exit per request and builds an
// http.Client that routes a tool's outbound traffic through that exit's SOCKS5
// proxy — reached over the single existing WireGuard tunnel (the microsocks
// "forward" dialer). One tunnel, many exits; see docs/tool-proxy-egress.md.
package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Relay is a Mullvad WireGuard relay usable as a SOCKS5 exit.
type Relay struct {
	Hostname    string // e.g. us-atl-wg-001
	CountryCode string // e.g. us
	CityCode    string // e.g. atl
	SocksName   string // e.g. us-atl-wg-socks5-001.relays.mullvad.net
	SocksPort   int    // e.g. 1080
}

func (r Relay) addr() string {
	port := r.SocksPort
	if port == 0 {
		port = 1080
	}
	return fmt.Sprintf("%s:%d", r.SocksName, port)
}

// relayJSON mirrors an entry at api.mullvad.net/www/relays/wireguard/ (the only
// relay endpoint that carries socks_name/socks_port).
type relayJSON struct {
	Hostname    string `json:"hostname"`
	CountryCode string `json:"country_code"`
	CityCode    string `json:"city_code"`
	Active      bool   `json:"active"`
	SocksName   string `json:"socks_name"`
	SocksPort   int    `json:"socks_port"`
}

// DefaultRelaysURL is Mullvad's machine-readable relay list.
const DefaultRelaysURL = "https://api.mullvad.net/www/relays/wireguard/"

// CatalogueConfig configures a Catalogue.
type CatalogueConfig struct {
	URL    string        // relay list URL; defaults to DefaultRelaysURL
	TTL    time.Duration // refetch interval; defaults to 1h
	Client *http.Client  // fetch client (DIRECT, not VPN); defaults to a 15s client
	Logger *slog.Logger
}

// Catalogue fetches + caches Mullvad's active SOCKS5-capable relays and
// resolves an egress spec to one of them. Safe for concurrent use.
type Catalogue struct {
	url    string
	ttl    time.Duration
	client *http.Client
	logger *slog.Logger

	// now/rng are injectable for tests.
	now func() time.Time
	rng func(n int) int

	mu        sync.Mutex
	relays    []Relay
	fetchedAt time.Time
}

// NewCatalogue builds a Catalogue with the given config (zero values defaulted).
func NewCatalogue(cfg CatalogueConfig) *Catalogue {
	url := cfg.URL
	if url == "" {
		url = DefaultRelaysURL
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Catalogue{
		url:    url,
		ttl:    ttl,
		client: client,
		logger: logger,
		now:    time.Now,
		rng:    rand.IntN,
	}
}

// SetRelays replaces the cached relays and marks the cache fresh. Test helper /
// manual seeding; Resolve won't fetch while the seeded list is within TTL.
func (c *Catalogue) SetRelays(relays []Relay) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.relays = relays
	c.fetchedAt = c.now()
}

// ensureFresh fetches the relay list if the cache is empty or stale. A fetch
// error with a non-empty cache is logged and tolerated (keep last-good).
func (c *Catalogue) ensureFresh(ctx context.Context) error {
	c.mu.Lock()
	fresh := len(c.relays) > 0 && c.now().Sub(c.fetchedAt) < c.ttl
	c.mu.Unlock()
	if fresh {
		return nil
	}
	relays, err := c.fetch(ctx)
	if err != nil {
		c.mu.Lock()
		have := len(c.relays)
		c.mu.Unlock()
		if have > 0 {
			c.logger.WarnContext(ctx, "egress: relay refresh failed; using cached list", "err", err, "cached", have)
			return nil
		}
		return err
	}
	c.mu.Lock()
	c.relays = relays
	c.fetchedAt = c.now()
	c.mu.Unlock()
	c.logger.InfoContext(ctx, "egress: relay list refreshed", "count", len(relays))
	return nil
}

func (c *Catalogue) fetch(ctx context.Context) ([]Relay, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("egress: fetch relays: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("egress: relay list HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("egress: read relays: %w", err)
	}
	var raw []relayJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("egress: parse relays: %w", err)
	}
	out := make([]Relay, 0, len(raw))
	for _, r := range raw {
		if !r.Active || r.SocksName == "" {
			continue
		}
		out = append(out, Relay{
			Hostname:    r.Hostname,
			CountryCode: strings.ToLower(r.CountryCode),
			CityCode:    strings.ToLower(r.CityCode),
			SocksName:   r.SocksName,
			SocksPort:   r.SocksPort,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("egress: relay list had no active SOCKS5 relays")
	}
	return out, nil
}

// euCountries is the EU/EEA member-state codes, used by the "eu" spec for
// GDPR-friendly egress (any of these countries). EEA non-EU (is/li/no) are
// included since GDPR applies across the EEA.
var euCountries = map[string]bool{
	"at": true, "be": true, "bg": true, "hr": true, "cy": true, "cz": true,
	"dk": true, "ee": true, "fi": true, "fr": true, "de": true, "gr": true,
	"hu": true, "ie": true, "it": true, "lv": true, "lt": true, "lu": true,
	"mt": true, "nl": true, "pl": true, "pt": true, "ro": true, "sk": true,
	"si": true, "es": true, "se": true, // EU-27
	"is": true, "li": true, "no": true, // EEA
}

// Resolve maps an egress spec to a relay. Grammar:
//
//	"us"               country
//	"us-atl"           country-city
//	"us-atl-wg-001"    exact hostname
//	"<socks_name>"     exact socks5 hostname
//	"eu"               any EU/EEA country (GDPR)
//	"any" / "random"   anywhere
//
// A broad spec picks a random active match. Returns an error if nothing matches.
func (c *Catalogue) Resolve(ctx context.Context, spec string) (Relay, error) {
	return c.ResolveExcluding(ctx, spec, nil)
}

// ResolveExcluding is Resolve but skips any relay whose socks_name is in
// exclude. Used by the retry path to fail over to a different relay matching the
// same spec after one is found dead.
func (c *Catalogue) ResolveExcluding(ctx context.Context, spec string, exclude map[string]bool) (Relay, error) {
	if err := c.ensureFresh(ctx); err != nil {
		return Relay{}, err
	}
	spec = strings.ToLower(strings.TrimSpace(spec))

	c.mu.Lock()
	relays := c.relays
	c.mu.Unlock()

	excluded := func(r Relay) bool { return exclude != nil && exclude[r.SocksName] }

	// Exact hostname / socks_name wins over the country/city heuristic.
	for _, r := range relays {
		if (spec == strings.ToLower(r.Hostname) || spec == strings.ToLower(r.SocksName)) && !excluded(r) {
			return r, nil
		}
	}

	var matches []Relay
	switch {
	case spec == "any" || spec == "random":
		for _, r := range relays {
			if !excluded(r) {
				matches = append(matches, r)
			}
		}
	case spec == "eu":
		for _, r := range relays {
			if euCountries[r.CountryCode] && !excluded(r) {
				matches = append(matches, r)
			}
		}
	default:
		parts := strings.Split(spec, "-")
		country := parts[0]
		city := ""
		if len(parts) >= 2 {
			city = parts[1]
		}
		for _, r := range relays {
			if r.CountryCode != country {
				continue
			}
			if city != "" && r.CityCode != city {
				continue
			}
			if !excluded(r) {
				matches = append(matches, r)
			}
		}
	}
	if len(matches) == 0 {
		return Relay{}, fmt.Errorf("egress: no active relay matches %q", spec)
	}
	return matches[c.rng(len(matches))], nil
}
