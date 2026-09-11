package router

// Token scopes as a privacy floor.
//
// A personal access token may carry a models: scope (see auth/scopes.go). The
// router enforces it by treating the scope as a retention tier the caller
// cannot loosen: models:local is the `local` tier, models:local_or_zdr is
// `zdr`, models:* demands nothing. That reuse is the whole design — the tier
// machinery in privacy.go already filters every path a request can take to a
// backend: the first candidate walk of a role or chain, the failover walk
// (nextRoleCandidate), and the dispatch of a directly named model. A scope
// implemented as its own check would have to be bolted onto each of those
// and would be wrong the first time someone added a fourth.
//
// The sharp edge the scopes task names — a models:local token must FAIL when
// its local seat is down and a chain would fail over to a paid provider —
// therefore holds by construction: the failover walk gates on the same tier
// the first pass did, and a cloud candidate never passes privacyGate under
// `local`.
//
// The one thing that differs from a caller's own X-Router-Privacy header is
// the refusal. A header refusal is the caller's policy and says so; a scope
// refusal is the token's, names the scope and the token, and lands in reqlog
// under its own error class so "which tokens are hitting their fence" is one
// query rather than a guess.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/erewhon/llm-router-go/internal/auth"
)

// errorClassScopeRefused marks a request the token's scope turned away
// before any upstream was tried. Same column as the upstream classes, same
// exclusions as privacy_refused: nothing was sent.
const errorClassScopeRefused = "scope_refused"

// privacyDemand is the retention requirement one request is held to, and
// where it came from — the caller's header, or the token's scope when that is
// at least as strict. source is prose for the refusal message; scope and
// tokenID are set only when the scope is the binding requirement.
type privacyDemand struct {
	tier    privacyTier
	source  string
	scope   string
	tokenID string
}

// scopeTier maps a models: scope to the tier that enforces it.
func scopeTier(scope string) privacyTier {
	switch scope {
	case auth.ScopeModelsLocal:
		return tierLocal
	case auth.ScopeModelsLocalOrZDR:
		return tierZDR
	default:
		return tierNone
	}
}

// privacyDemandFor combines the caller's X-Router-Privacy header with the
// scope of the identity on the request context, taking the stricter. A
// malformed header is refused (refuse != "") before any scope is consulted,
// exactly as parsePrivacyHeader alone would.
//
// The header can tighten beyond the scope; it can never loosen it. "any" on a
// models:local token is still local — the scope is the operator's statement
// about what this token may do, not the caller's to waive.
func privacyDemandFor(r *http.Request) (privacyDemand, string) {
	tier, source, refuse := parsePrivacyHeader(r)
	if refuse != "" {
		return privacyDemand{}, refuse
	}
	d := privacyDemand{tier: tier, source: source}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		return d, ""
	}
	scope := id.ModelScope()
	if st := scopeTier(scope); st != tierNone && st >= tier {
		d.tier = st
		d.scope = scope
		d.tokenID = id.TokenID
		d.source = fmt.Sprintf("token scope %s (token %s)", scope, id.TokenID)
	}
	return d, ""
}

// errorClass is the reqlog error class a refusal under this demand records.
func (d privacyDemand) errorClass() string {
	if d.scope != "" {
		return errorClassScopeRefused
	}
	return errorClassPrivacyRefused
}

// writeRefusal answers a request this demand turned away. tier is the tier
// that actually did the refusing — usually d.tier, but a role's own declared
// tolerance can be the one that bit when the caller demanded nothing.
func (d privacyDemand) writeRefusal(w http.ResponseWriter, tier privacyTier, reason string, excluded []string) {
	if d.scope == "" {
		writePrivacyRefusal(w, tier, reason, excluded)
		return
	}
	writeScopeRefusal(w, d, tier, reason, excluded)
}

// writeScopeRefusal is the scope-flavoured 403.
//
// Same status and the same "no retry will help" semantics as a privacy
// refusal, but a different type and code: a background agent hitting this at
// 03:00 needs the log line and the error body to say "your TOKEN may not use
// that model" — not "your privacy header" — because there is no header, and
// the person reading it needs to know to widen the scope or fix the config,
// not to edit the request.
func writeScopeRefusal(w http.ResponseWriter, d privacyDemand, tier privacyTier, reason string, excluded []string) {
	if t := tier.String(); t != "" {
		w.Header().Set(PrivacyHeader, t)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	errObj := map[string]any{
		"message":  "router refused: " + reason,
		"type":     "permission_error",
		"code":     "token_scope_denied",
		"scope":    d.scope,
		"token_id": d.tokenID,
	}
	if len(excluded) > 0 {
		errObj["privacy_tier"] = tier.String()
		errObj["excluded_candidates"] = excluded
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": errObj})
}
