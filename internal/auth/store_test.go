package auth

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "pat.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMintThenVerify(t *testing.T) {
	s := newStore(t)
	wire, tok, err := s.Mint("steven", "laptop", nil, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	id, err := s.Verify(wire)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Principal != "steven" {
		t.Errorf("principal = %q, want steven", id.Principal)
	}
	if id.TokenID != tok.ID {
		t.Errorf("token id = %q, want %q", id.TokenID, tok.ID)
	}
	if id.Legacy() {
		t.Error("a minted PAT must not look legacy")
	}
}

func TestVerifyRejects(t *testing.T) {
	s := newStore(t)
	wire, _, _ := s.Mint("steven", "", nil, nil)
	id, secret, _ := Split(wire)

	other := newStore(t)
	otherWire, _, _ := other.Mint("someone", "", nil, nil)

	cases := []struct {
		name string
		in   string
		want error
	}{
		{"malformed", "not-a-token", ErrMalformed},
		{"unknown id", Prefix + "aaaaaaaaaaaa_" + secret, ErrUnknown},
		{"wrong secret", Prefix + id + "_wrongsecret", ErrUnknown},
		{"token from another store", otherWire, ErrUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Verify(tc.in); !errors.Is(err, tc.want) {
				t.Errorf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
}

// A caller holding a real id but not the secret must not learn the token's
// lifecycle state — that would confirm the id names a real (revoked) token.
func TestVerifyWrongSecretOnRevokedTokenReportsUnknown(t *testing.T) {
	s := newStore(t)
	wire, tok, _ := s.Mint("steven", "", nil, nil)
	if err := s.Revoke(tok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	id, _, _ := Split(wire)
	if _, err := s.Verify(Prefix + id + "_wrong"); !errors.Is(err, ErrUnknown) {
		t.Errorf("Verify with wrong secret on revoked token = %v, want ErrUnknown", err)
	}
}

func TestRevokeStopsVerification(t *testing.T) {
	s := newStore(t)
	wire, tok, _ := s.Mint("steven", "", nil, nil)
	if _, err := s.Verify(wire); err != nil {
		t.Fatalf("Verify before revoke: %v", err)
	}
	if err := s.Revoke(tok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// Revocation must take effect on the very next call — no cache, no TTL.
	if _, err := s.Verify(wire); !errors.Is(err, ErrRevoked) {
		t.Errorf("Verify after revoke = %v, want ErrRevoked", err)
	}
}

func TestRevokeIsIdempotentAndReportsUnknown(t *testing.T) {
	s := newStore(t)
	_, tok, _ := s.Mint("steven", "", nil, nil)
	if err := s.Revoke(tok.ID); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if err := s.Revoke(tok.ID); err != nil {
		t.Errorf("second Revoke = %v, want nil (idempotent)", err)
	}
	if err := s.Revoke("ffffffffffff"); !errors.Is(err, ErrUnknown) {
		t.Errorf("Revoke(nonexistent) = %v, want ErrUnknown", err)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	s := newStore(t)
	past := time.Now().UTC().Add(-time.Hour)
	wire, _, err := s.Mint("steven", "", nil, &past)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := s.Verify(wire); !errors.Is(err, ErrExpired) {
		t.Errorf("Verify expired = %v, want ErrExpired", err)
	}
}

func TestMintRejectsBadPrincipal(t *testing.T) {
	s := newStore(t)
	for _, p := range []string{"", "   "} {
		if _, _, err := s.Mint(p, "", nil, nil); err == nil {
			t.Errorf("Mint(%q) succeeded, want error", p)
		}
	}
	// Minting into the legacy namespace would corrupt the migration signal:
	// "legacy traffic is zero" must mean shared keys are gone.
	if _, _, err := s.Mint(LegacyPrincipalPrefix+"abc", "", nil, nil); err == nil {
		t.Error("Mint into the legacy: namespace succeeded, want error")
	}
}

func TestListFiltersAndOrders(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Mint("steven", "laptop", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Mint("agent", "background", nil, nil); err != nil {
		t.Fatal(err)
	}
	all, err := s.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List() returned %d tokens, want 2", len(all))
	}
	mine, err := s.List("steven")
	if err != nil {
		t.Fatalf("List(steven): %v", err)
	}
	if len(mine) != 1 || mine[0].Principal != "steven" {
		t.Errorf("List(steven) = %+v, want one steven token", mine)
	}
}

func TestScopesRoundTrip(t *testing.T) {
	s := newStore(t)
	wire, _, err := s.Mint("agent", "bg", []string{"models:local_or_zdr", "models:local"}, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	id, err := s.Verify(wire)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Stored sorted, so the column is stable and diffable.
	want := []string{"models:local", "models:local_or_zdr"}
	if len(id.Scopes) != len(want) {
		t.Fatalf("scopes = %v, want %v", id.Scopes, want)
	}
	for i := range want {
		if id.Scopes[i] != want[i] {
			t.Errorf("scopes = %v, want %v", id.Scopes, want)
			break
		}
	}
}

func TestLastUsedRecorded(t *testing.T) {
	s := newStore(t)
	wire, tok, _ := s.Mint("steven", "", nil, nil)
	if _, err := s.Verify(wire); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// touch() writes asynchronously; poll rather than sleep a fixed span.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, err := s.List("")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) == 1 && list[0].ID == tok.ID && list[0].LastUsedAt != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("LastUsedAt was never recorded after a successful Verify")
}

// The store must survive a reopen: tokens are durable state, not process state.
func TestStorePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pat.db")

	s1, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	wire, _, err := s1.Mint("steven", "", nil, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	s1.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := s2.Verify(wire); err != nil {
		t.Errorf("Verify after reopen: %v", err)
	}
}
