package egress

import (
	"context"
	"testing"
)

func testCatalogue() *Catalogue {
	c := NewCatalogue(CatalogueConfig{})
	c.rng = func(int) int { return 0 } // deterministic: always first match
	c.SetRelays([]Relay{
		{Hostname: "us-atl-wg-001", CountryCode: "us", CityCode: "atl", SocksName: "us-atl-wg-socks5-001.relays.mullvad.net", SocksPort: 1080},
		{Hostname: "us-nyc-wg-002", CountryCode: "us", CityCode: "nyc", SocksName: "us-nyc-wg-socks5-002.relays.mullvad.net", SocksPort: 1080},
		{Hostname: "se-got-wg-001", CountryCode: "se", CityCode: "got", SocksName: "se-got-wg-socks5-001.relays.mullvad.net", SocksPort: 1080},
	})
	return c
}

func TestResolveSpecs(t *testing.T) {
	c := testCatalogue()
	ctx := context.Background()
	cases := []struct{ spec, wantHost string }{
		{"us", "us-atl-wg-001"},                                      // country, first match (rng=0)
		{"us-nyc", "us-nyc-wg-002"},                                  // country-city
		{"se", "se-got-wg-001"},                                      // country
		{"se-got-wg-001", "se-got-wg-001"},                           // exact hostname
		{"se-got-wg-socks5-001.relays.mullvad.net", "se-got-wg-001"}, // exact socks_name
		{"any", "us-atl-wg-001"},                                     // any -> first
		{"random", "us-atl-wg-001"},                                  // random alias of any
		{"US", "us-atl-wg-001"},                                      // case-insensitive
		{"  se  ", "se-got-wg-001"},                                  // trimmed
	}
	for _, tc := range cases {
		r, err := c.Resolve(ctx, tc.spec)
		if err != nil {
			t.Errorf("Resolve(%q): %v", tc.spec, err)
			continue
		}
		if r.Hostname != tc.wantHost {
			t.Errorf("Resolve(%q) = %s, want %s", tc.spec, r.Hostname, tc.wantHost)
		}
	}
	if _, err := c.Resolve(ctx, "jp"); err == nil {
		t.Error("Resolve(jp): expected error for no match, got nil")
	}
	if _, err := c.Resolve(ctx, "us-sfo"); err == nil {
		t.Error("Resolve(us-sfo): expected error for missing city, got nil")
	}
}

func TestResolveRandomPicksWithinMatch(t *testing.T) {
	c := testCatalogue()
	c.rng = func(int) int { return 1 } // second match within the country group
	r, err := c.Resolve(context.Background(), "us")
	if err != nil {
		t.Fatal(err)
	}
	if r.Hostname != "us-nyc-wg-002" {
		t.Errorf("us with rng=1 -> %s, want us-nyc-wg-002", r.Hostname)
	}
}

func TestRelayAddrDefaultsPort(t *testing.T) {
	if got := (Relay{SocksName: "x.relays.mullvad.net"}).addr(); got != "x.relays.mullvad.net:1080" {
		t.Errorf("addr() = %q, want default :1080", got)
	}
	if got := (Relay{SocksName: "x", SocksPort: 1081}).addr(); got != "x:1081" {
		t.Errorf("addr() = %q, want :1081", got)
	}
}
