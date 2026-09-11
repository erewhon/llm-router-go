package auth

import (
	"errors"
	"testing"
)

func TestValidateScopesIsAClosedVocabulary(t *testing.T) {
	if err := ValidateScopes([]string{ScopeModelsLocal, ScopeModelsLocalOrZDR, ScopeModelsAll}); err != nil {
		t.Fatalf("known scopes rejected: %v", err)
	}
	if err := ValidateScopes(nil); err != nil {
		t.Fatalf("no scopes must be valid (unrestricted): %v", err)
	}
	for _, bad := range []string{"models:cloud", "local", "models:", "admin", "models:Local"} {
		if err := ValidateScopes([]string{bad}); err == nil {
			t.Errorf("scope %q accepted; the vocabulary must be closed", bad)
		}
	}
}

func TestModelScopeIsTheStrictestCarried(t *testing.T) {
	cases := []struct {
		scopes []string
		want   string
	}{
		{nil, ScopeModelsAll},
		{[]string{}, ScopeModelsAll},
		{[]string{ScopeModelsAll}, ScopeModelsAll},
		{[]string{ScopeModelsLocalOrZDR}, ScopeModelsLocalOrZDR},
		{[]string{ScopeModelsAll, ScopeModelsLocal}, ScopeModelsLocal},
		{[]string{ScopeModelsLocalOrZDR, ScopeModelsLocal, ScopeModelsAll}, ScopeModelsLocal},
		// An unknown scope on an old row must not widen anything.
		{[]string{"garbage", ScopeModelsLocalOrZDR}, ScopeModelsLocalOrZDR},
	}
	for _, c := range cases {
		if got := (Identity{Scopes: c.scopes}).ModelScope(); got != c.want {
			t.Errorf("ModelScope(%v) = %q, want %q", c.scopes, got, c.want)
		}
	}
}

func TestScopeAtMost(t *testing.T) {
	if !ScopeAtMost(ScopeModelsLocal, ScopeModelsLocal) {
		t.Error("local is at most local")
	}
	if ScopeAtMost(ScopeModelsAll, ScopeModelsLocal) {
		t.Error("* is wider than local")
	}
	if ScopeAtMost(ScopeModelsLocalOrZDR, ScopeModelsLocal) {
		t.Error("local_or_zdr is wider than local")
	}
	if !ScopeAtMost(ScopeModelsLocal, ScopeModelsAll) {
		t.Error("everything is at most *")
	}
	if ScopeAtMost("nonsense", ScopeModelsAll) {
		t.Error("an unknown scope is never within any limit")
	}
}

func TestMintRejectsUnknownScopes(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Mint("agent", "bg", []string{"models:everything"}, nil); err == nil {
		t.Fatal("Mint accepted a scope the router does not enforce")
	}
	list, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("a rejected mint left %d row(s) behind", len(list))
	}
}

func TestRevokeOwnedIsScopedToThePrincipal(t *testing.T) {
	s := newStore(t)
	wire, tok, err := s.Mint("alice", "laptop", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Another principal cannot revoke it, and cannot learn that it exists.
	if err := s.RevokeOwned(tok.ID, "mallory"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("RevokeOwned by a stranger = %v, want ErrUnknown", err)
	}
	if _, err := s.Verify(wire); err != nil {
		t.Fatalf("token was affected by a stranger's revoke: %v", err)
	}
	// The owner can.
	if err := s.RevokeOwned(tok.ID, "alice"); err != nil {
		t.Fatalf("RevokeOwned by the owner: %v", err)
	}
	if _, err := s.Verify(wire); !errors.Is(err, ErrRevoked) {
		t.Fatalf("after revoke, Verify = %v, want ErrRevoked", err)
	}
	// Twice is fine — panic-revoking must never scare.
	if err := s.RevokeOwned(tok.ID, "alice"); err != nil {
		t.Fatalf("second RevokeOwned: %v", err)
	}
	// A nonexistent id looks exactly like someone else's.
	if err := s.RevokeOwned("000000000000", "alice"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("RevokeOwned of a nonexistent id = %v, want ErrUnknown", err)
	}
}
