package tools

import (
	"testing"
	"time"
)

func TestParseSOCKS5(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"192.168.42.219:1080", "192.168.42.219:1080", false},
		{"socks5://192.168.42.219:1080", "192.168.42.219:1080", false},
		{"socks5h://192.168.42.219:1080", "192.168.42.219:1080", false},
		{"host:port", "", true},      // not a valid port number
		{"socks5://", "", true},      // missing host
		{"http://x:1080", "", true},  // wrong scheme
		{"", "", true},               // caller should not pass empty (NewHTTPClient short-circuits)
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseSOCKS5(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("parseSOCKS5(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("parseSOCKS5(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNewHTTPClient_DefaultsTo15sTimeout(t *testing.T) {
	c, err := NewHTTPClient(HTTPClientConfig{})
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if c.Timeout != 15*time.Second {
		t.Errorf("Timeout = %v, want 15s", c.Timeout)
	}
}

func TestNewHTTPClient_RespectsTimeout(t *testing.T) {
	c, err := NewHTTPClient(HTTPClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if c.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", c.Timeout)
	}
}

func TestNewHTTPClient_BadSOCKS5(t *testing.T) {
	_, err := NewHTTPClient(HTTPClientConfig{SOCKS5: "not-a-valid-address"})
	if err == nil {
		t.Fatal("expected error for invalid SOCKS5 address")
	}
}

func TestNewHTTPClient_ValidSOCKS5(t *testing.T) {
	// Doesn't dial; SOCKS5 setup only fails at request time if the
	// proxy is unreachable. Construction should succeed.
	c, err := NewHTTPClient(HTTPClientConfig{SOCKS5: "127.0.0.1:1080"})
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if c == nil {
		t.Fatal("returned nil client")
	}
}
