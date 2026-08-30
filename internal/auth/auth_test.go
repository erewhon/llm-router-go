package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGenerateRoundTrip(t *testing.T) {
	wire, id, hash, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(wire, Prefix) {
		t.Errorf("wire %q lacks prefix %q", wire, Prefix)
	}
	gotID, secret, err := Split(wire)
	if err != nil {
		t.Fatalf("Split(%q): %v", wire, err)
	}
	if gotID != id {
		t.Errorf("id: split %q, generated %q", gotID, id)
	}
	if !SecretMatches(secret, hash) {
		t.Error("SecretMatches false for the secret we just generated")
	}
	// The stored hash must not contain the secret itself.
	if strings.Contains(hash, secret) {
		t.Error("stored hash contains the raw secret")
	}
}

func TestGenerateIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		_, id, _, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q within 100 draws", id)
		}
		seen[id] = true
	}
}

func TestSplitRejectsMalformed(t *testing.T) {
	good, _, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	id, _, _ := Split(good)
	// A fixed, underscore-free secret. The real alphabet (base64url) includes
	// "_", so a generated secret starting with one would make the
	// "no separator" case below re-form a *valid* token and pass by accident.
	const secret = "abcdefghijklmnop"

	cases := []struct{ name, in string }{
		{"empty", ""},
		{"no prefix", id + "_" + secret},
		{"wrong prefix", "tok_" + id + "_" + secret},
		{"prefix only", Prefix},
		{"no separator", Prefix + id + secret},
		{"empty id", Prefix + "_" + secret},
		{"empty secret", Prefix + id + "_"},
		{"short id", Prefix + "abc_" + secret},
		{"non-hex id", Prefix + "zzzzzzzzzzzz_" + secret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Split(tc.in); !errors.Is(err, ErrMalformed) {
				t.Errorf("Split(%q) = %v, want ErrMalformed", tc.in, err)
			}
		})
	}
}

func TestSecretMatchesRejectsWrongSecret(t *testing.T) {
	_, _, hash, _ := Generate()
	if SecretMatches("not-the-secret", hash) {
		t.Error("SecretMatches accepted a wrong secret")
	}
	if SecretMatches("", hash) {
		t.Error("SecretMatches accepted an empty secret")
	}
}

func TestTokenActive(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	cases := []struct {
		name string
		tok  Token
		want error
	}{
		{"no expiry, not revoked", Token{}, nil},
		{"future expiry", Token{ExpiresAt: &future}, nil},
		{"past expiry", Token{ExpiresAt: &past}, ErrExpired},
		{"revoked", Token{RevokedAt: &past}, ErrRevoked},
		// Revocation is reported ahead of expiry: it is the deliberate act,
		// and the one the operator wants confirmed.
		{"revoked and expired", Token{RevokedAt: &past, ExpiresAt: &past}, ErrRevoked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tok.Active(now); !errors.Is(got, tc.want) {
				t.Errorf("Active() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExpiryBoundaryIsExclusive(t *testing.T) {
	now := time.Now()
	// A token expiring exactly now is expired, not valid for one more instant.
	if err := (Token{ExpiresAt: &now}).Active(now); !errors.Is(err, ErrExpired) {
		t.Errorf("Active at exact expiry = %v, want ErrExpired", err)
	}
}

func TestLooksLikePAT(t *testing.T) {
	wire, _, _, _ := Generate()
	if !LooksLikePAT(wire) {
		t.Error("LooksLikePAT false for a generated token")
	}
	for _, s := range []string{"sk-shared-key", "", "Bearer pat_x", "PAT_abc"} {
		if LooksLikePAT(s) {
			t.Errorf("LooksLikePAT(%q) = true, want false", s)
		}
	}
}

func TestLegacyIdentity(t *testing.T) {
	a := LegacyIdentity("sk-one")
	b := LegacyIdentity("sk-two")

	if !a.Legacy() {
		t.Error("legacy identity should report Legacy() true")
	}
	if a.Principal == b.Principal {
		t.Error("different shared keys must get different principals")
	}
	if got := LegacyIdentity("sk-one"); got.Principal != a.Principal {
		t.Error("LegacyIdentity is not stable for the same key")
	}
	// The key itself must never appear in what we store or log.
	if strings.Contains(a.Principal, "sk-one") || strings.Contains(a.TokenID, "sk-one") {
		t.Errorf("legacy identity leaks the shared key: %+v", a)
	}
	if !strings.HasPrefix(a.Principal, LegacyPrincipalPrefix) {
		t.Errorf("principal %q lacks prefix %q", a.Principal, LegacyPrincipalPrefix)
	}
}

func TestIdentityLegacyFalseForPAT(t *testing.T) {
	if (Identity{Principal: "steven", TokenID: "abc"}).Legacy() {
		t.Error("a real principal must not report Legacy()")
	}
}
