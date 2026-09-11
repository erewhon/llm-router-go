package auth

import (
	"fmt"
	"strings"
)

// Model scopes: what a token may route to.
//
// The vocabulary is deliberately tiny and closed. The motivating case is a
// background agent that must be STRUCTURALLY incapable of spending money on
// OpenRouter or leaking a prompt to a retaining provider — and for that, three
// named postures cover every real request. A policy language would put the
// bugs exactly where they cost the most.
//
// A token with no models: scope is unrestricted. That is what makes rollout
// change nothing: every token minted before scopes existed, and every legacy
// shared key, keeps behaving as it did.
//
// Enforcement lives in the router, on the RESOLVED model — never on the name
// the caller sent. Aliases, `auto`, roles and fallback chains all mean the
// string in the request can differ from where it lands, and a scope that
// checked the string would be a control you can walk around.
const (
	// ScopeModelsAll is unrestricted: any model in the registry.
	ScopeModelsAll = "models:*"
	// ScopeModelsLocalOrZDR admits fleet hardware, or a cloud endpoint the
	// router can hold to zero data retention for the request. It still
	// permits paid external calls, so it is NOT the family default.
	ScopeModelsLocalOrZDR = "models:local_or_zdr"
	// ScopeModelsLocal admits only models pinned to fleet hardware. Nothing
	// leaves the building and nothing is billed.
	ScopeModelsLocal = "models:local"
)

// modelScopeRank orders the model scopes by strictness, so a token carrying
// more than one resolves to the strictest — a token is never wider than any
// scope it names.
var modelScopeRank = map[string]int{
	ScopeModelsAll:        0,
	ScopeModelsLocalOrZDR: 1,
	ScopeModelsLocal:      2,
}

// KnownScopes lists every scope a token may carry, strictest last.
func KnownScopes() []string {
	return []string{ScopeModelsAll, ScopeModelsLocalOrZDR, ScopeModelsLocal}
}

// ValidateScopes rejects any scope outside the closed vocabulary. It is
// applied at mint time so the store can never hold a scope the router does
// not enforce — a token labelled with a posture nobody implements would be
// worse than an unrestricted one, because someone would trust it.
func ValidateScopes(scopes []string) error {
	for _, s := range scopes {
		if _, ok := modelScopeRank[strings.TrimSpace(s)]; !ok {
			return fmt.Errorf("auth: unknown scope %q (want one of %s)", s, strings.Join(KnownScopes(), ", "))
		}
	}
	return nil
}

// ModelScope returns the effective model scope of an identity: the strictest
// models: scope it carries, or ScopeModelsAll when it carries none.
func (i Identity) ModelScope() string {
	best, rank := ScopeModelsAll, 0
	for _, s := range i.Scopes {
		if r, ok := modelScopeRank[strings.TrimSpace(s)]; ok && r > rank {
			best, rank = s, r
		}
	}
	return best
}

// ScopeAtMost reports whether scope is no wider than limit — the check a
// self-service surface applies before letting a principal mint: a family
// member allowed at most models:local may not ask for models:*.
func ScopeAtMost(scope, limit string) bool {
	rs, ok1 := modelScopeRank[scope]
	rl, ok2 := modelScopeRank[limit]
	return ok1 && ok2 && rs >= rl
}
