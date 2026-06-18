package egress

import (
	"context"
	"net/http"
	"testing"
)

func TestSelectorDefaultExit(t *testing.T) {
	base := &http.Client{}
	s := NewSelector(Config{BaseClient: base}) // no forward/catalogue

	for _, spec := range []string{"", "default", "none", "  DEFAULT "} {
		c, label, err := s.ClientFor(context.Background(), spec)
		if err != nil {
			t.Errorf("ClientFor(%q): unexpected err %v", spec, err)
		}
		if c != base {
			t.Errorf("ClientFor(%q): got non-base client", spec)
		}
		if label != "default" {
			t.Errorf("ClientFor(%q): label = %q, want default", spec, label)
		}
	}
}

func TestSelectorRelaySpecWithoutForwardFallsBack(t *testing.T) {
	base := &http.Client{}
	s := NewSelector(Config{BaseClient: base}) // Forward/Catalogue nil
	c, label, err := s.ClientFor(context.Background(), "se")
	if err == nil {
		t.Error("expected error when a relay spec is given but no forward dialer is configured")
	}
	if c != base || label != "default" {
		t.Errorf("on error, expected base client + default label, got label=%q", label)
	}
}

func TestSelectorDefaultSpecApplied(t *testing.T) {
	base := &http.Client{}
	// DefaultSpec set but no forward: a blank request should adopt the default
	// spec ("se"), try to resolve a relay, and (lacking a forward) fall back.
	s := NewSelector(Config{BaseClient: base, DefaultSpec: "se"})
	_, _, err := s.ClientFor(context.Background(), "")
	if err == nil {
		t.Error("expected fallback error: DefaultSpec=se needs a relay but no forward dialer")
	}
}
