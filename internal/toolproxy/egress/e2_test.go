package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestResolveEUGroup(t *testing.T) {
	c := testCatalogue() // us-atl, us-nyc (non-EU), se-got (EU); rng=0
	r, err := c.Resolve(context.Background(), "eu")
	if err != nil {
		t.Fatalf("Resolve(eu): %v", err)
	}
	if !euCountries[r.CountryCode] {
		t.Errorf("eu -> %s (%s), want an EU/EEA country", r.Hostname, r.CountryCode)
	}
	if r.CountryCode == "us" {
		t.Error("eu must never resolve to a US relay")
	}
}

func TestResolveExcluding(t *testing.T) {
	c := testCatalogue()
	atl := "us-atl-wg-socks5-001.relays.mullvad.net"
	nyc := "us-nyc-wg-socks5-002.relays.mullvad.net"

	r, err := c.ResolveExcluding(context.Background(), "us", map[string]bool{atl: true})
	if err != nil {
		t.Fatalf("ResolveExcluding(us, -atl): %v", err)
	}
	if r.Hostname != "us-nyc-wg-002" {
		t.Errorf("excluding atl -> %s, want us-nyc-wg-002", r.Hostname)
	}
	if _, err := c.ResolveExcluding(context.Background(), "us", map[string]bool{atl: true, nyc: true}); err == nil {
		t.Error("excluding all US relays: expected error")
	}
}

// fakeDialer satisfies proxy.Dialer so ClientFor accepts a relay spec; the
// actual round-tripping is stubbed via Selector.roundTripVia.
type fakeDialer struct{}

func (fakeDialer) Dial(network, addr string) (net.Conn, error) { return nil, errors.New("unused") }

func TestSelectorRetryFailover(t *testing.T) {
	cat := NewCatalogue(CatalogueConfig{})
	cat.rng = func(int) int { return 0 } // deterministic: first available match
	cat.SetRelays([]Relay{
		{Hostname: "us-a", CountryCode: "us", CityCode: "a", SocksName: "us-a.sock", SocksPort: 1080},
		{Hostname: "us-b", CountryCode: "us", CityCode: "b", SocksName: "us-b.sock", SocksPort: 1080},
	})
	s := NewSelector(Config{Forward: fakeDialer{}, BaseClient: &http.Client{}, Catalogue: cat, MaxTries: 3})

	var tried []string
	s.roundTripVia = func(relay Relay, req *http.Request) (*http.Response, error) {
		tried = append(tried, relay.Hostname)
		if relay.Hostname == "us-a" { // first relay is "dead"
			return nil, errors.New("dial tcp: connection refused")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	}

	client, label, err := s.ClientFor(context.Background(), "us")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if label != "us-a" {
		t.Errorf("initial label = %q, want us-a", label)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	resp, err := client.Transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip after failover: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if len(tried) != 2 || tried[0] != "us-a" || tried[1] != "us-b" {
		t.Errorf("tried = %v, want [us-a us-b] (failover to a live relay)", tried)
	}
}

func TestSelectorRetryExhausts(t *testing.T) {
	cat := NewCatalogue(CatalogueConfig{})
	cat.rng = func(int) int { return 0 }
	cat.SetRelays([]Relay{
		{Hostname: "us-a", CountryCode: "us", SocksName: "us-a.sock", SocksPort: 1080},
		{Hostname: "us-b", CountryCode: "us", SocksName: "us-b.sock", SocksPort: 1080},
	})
	s := NewSelector(Config{Forward: fakeDialer{}, BaseClient: &http.Client{}, Catalogue: cat, MaxTries: 5})
	s.roundTripVia = func(Relay, *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	client, _, err := s.ClientFor(context.Background(), "us")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if _, err := client.Transport.RoundTrip(req); err == nil {
		t.Error("all relays dead: expected RoundTrip error")
	}
}
