package router

// Retention enforcement: hold a request to zero data retention on the wire.
//
// The declarative half of this lives in models.yaml as `require.locality:
// local_or_zdr` (see config.LocalityLocalOrZDR), which is checked at LOAD time
// and answers "could this role ever land somewhere retaining?". This file is
// the request-time half, and answers the harder question: "did this particular
// request actually go somewhere zero-retention?"
//
// Both halves are needed because on OpenRouter ZDR is a property of the
// ENDPOINT that serves a request, not of the model id. The same
// z-ai/glm-5.3-flash resolved to 23 endpoints across as many operators and
// jurisdictions when probed on 2026-09-06. A config-time check alone would
// therefore assert something that is not stable per request, which is why the
// router attaches an explicit directive to every request under a retention
// tolerance rather than trusting a vetting note somebody wrote once.
//
// WHY A DIRECTIVE AND NOT A CHECK. The fleet's OpenRouter account already has
// account-level ZDR switched on: probing anthropic/claude-sonnet-5 with
// provider {"only":["anthropic"]} and no ZDR flag on 2026-09-06 was refused
// with "ZDR violation (account settings): 1 endpoint excluded". That is a
// setting in a web console, outside this repo, invisible to models.yaml, and
// if it is ever flipped the entire fleet silently drops out of compliance with
// no config file changing. The per-request directive does not depend on it,
// and the canary in zdrcanary.go watches it.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/erewhon/llm-router-go/internal/config"
)

// PrivacyHeader is both the request header a caller uses to tighten beyond
// their role's default, and the response header reporting what was enforced.
//
// Symmetric on purpose: the value a caller asks for is the value they get
// back, so "did my tolerance apply?" is answered by comparing two strings
// rather than by reading router logs.
const PrivacyHeader = "X-Router-Privacy"

// The caller-facing privacy tiers, strictest last. These are the header values
// a client sends, and the same strings come back on the response and land in
// reqlog's privacy_tolerance column — so a script's --privacy flag, the wire,
// and the audit row all use one vocabulary.
const (
	// PrivacyAny states "I have no requirement of my own". It is an explicit
	// no-op, accepted so a script can pass its flag straight through without
	// special-casing the default. It CANNOT loosen a role's declared
	// tolerance — the role's promise is not the caller's to waive.
	PrivacyAny = "any"
	// PrivacyZDR admits a local placement, or a cloud endpoint the router can
	// hold to zero data retention for this request (the directive goes on the
	// wire). Matches locality: local_or_zdr.
	PrivacyZDR = "zdr"
	// PrivacyLocal admits only a placement on fleet hardware. Nothing leaves
	// the building. Matches locality: local.
	PrivacyLocal = "local"
)

// privacyTier orders the tiers by strictness so two requirements — the role's
// and the caller's — combine by taking the stricter. The zero value is "no
// requirement".
type privacyTier int

const (
	tierNone privacyTier = iota
	tierZDR
	tierLocal
)

func (t privacyTier) String() string {
	switch t {
	case tierZDR:
		return PrivacyZDR
	case tierLocal:
		return PrivacyLocal
	default:
		return ""
	}
}

// privacyRequirement works out the retention tier this request must be held
// to, and where the demand came from (for the refusal message).
//
// Two sources, combined by taking the STRICTER:
//
//   - The resolved ROLE. Only locality: local_or_zdr produces a dispatch-time
//     requirement (tier zdr). A role declaring locality: local deliberately
//     does NOT: its overflow list is exempt by design — crossing the local/
//     cloud boundary when the fleet is down is what that overflow is for — and
//     enforcing "local" at dispatch would break exactly that. Load-time
//     validation already holds a local role's own candidates to IsLocal().
//   - The CALLER, via X-Router-Privacy. This is how a script says "local only"
//     or "zdr" per request, on a role or on a directly named model. A caller
//     may always tighten; "any" tightens nothing and loosens nothing.
//
// An UNRECOGNISED header value is a hard refusal, returned as refuse != "".
// It must never be mapped onto a real tier: a caller who typed a posture the
// router does not implement ("eu-only", or "locl") has not been given what
// they asked for, and serving them anyway lets them believe they were. An
// earlier version let an unrecognised value fall through and, on an
// OpenRouter seat, quietly serve it as zdr — which would have sent a script's
// misspelled local-only request to the cloud. Fixed 2026-09-10.
func (rt *Router) privacyRequirement(r *http.Request, res resolveResult) (tier privacyTier, source, refuse string) {
	switch v := strings.TrimSpace(strings.ToLower(r.Header.Get(PrivacyHeader))); v {
	case "", PrivacyAny:
		// no caller requirement
	case PrivacyZDR:
		tier, source = tierZDR, "request header "+PrivacyHeader+": "+PrivacyZDR
	case PrivacyLocal:
		tier, source = tierLocal, "request header "+PrivacyHeader+": "+PrivacyLocal
	default:
		return tierNone, "", fmt.Sprintf("request header %s: %q is not a recognised privacy tier (want %q, %q or %q)",
			PrivacyHeader, v, PrivacyLocal, PrivacyZDR, PrivacyAny)
	}

	if res.Role != "" {
		if role, ok := rt.roles[res.Role]; ok && role.Require.Locality == config.LocalityLocalOrZDR && tier < tierZDR {
			tier, source = tierZDR, "role "+res.Role+" requires "+string(config.LocalityLocalOrZDR)
		}
	}
	return tier, source, ""
}

// applyPrivacy enforces a retention tolerance on one outbound request. It
// mutates bodyMap in place and returns a non-empty refusal reason when the
// request must not be sent at all.
//
// Three outcomes, and the third is the point of the exercise:
//
//   - Local placement: nothing to do. The prompt never leaves the fleet, so
//     there is no third-party retention policy to enforce against.
//   - ZDR-enforceable endpoint: attach the directive. OpenRouter answers a
//     request no endpoint can satisfy with "No endpoints found matching your
//     data policy (Zero data retention)" — a hard failure, which is what
//     makes the promise real rather than advertised.
//   - Anything else: REFUSE. A caller who asked for zero retention and got a
//     best-effort answer from a retaining seat is worse off than one who got
//     an error, because they do not know to stop.
func (rt *Router) applyPrivacy(bodyMap map[string]any, res resolveResult, tier privacyTier, source string) string {
	m, ok := rt.registry.Models[res.ModelID]
	if !ok {
		return fmt.Sprintf("%s, but %q is not a known model", source, res.ModelID)
	}
	if m.IsLocal() {
		return ""
	}
	if tier == tierLocal {
		// No directive can make a cloud endpoint local. Refuse outright —
		// a ZDR-enforceable seat is still somebody else's computer.
		return fmt.Sprintf("%s, but %q is not on fleet hardware", source, res.ModelID)
	}
	if !m.ZDREnforceable() {
		return fmt.Sprintf("%s, but %q is neither local nor an endpoint the router can hold to zero data retention", source, res.ModelID)
	}
	if err := setZDRDirective(bodyMap); err != nil {
		return fmt.Sprintf("%s, but the directive could not be attached: %v", source, err)
	}
	return ""
}

// setZDRDirective adds OpenRouter's zero-retention routing directive to a
// request body.
//
// It MERGES into any provider block the caller already sent rather than
// replacing it, so a caller pinning a provider order or a quantization keeps
// those preferences — but it overwrites "zdr" unconditionally, because a
// caller-supplied `zdr: false` under a role that requires zero retention is
// not a preference to honour.
//
// A non-object "provider" in the incoming body is a client bug and is reported
// rather than silently discarded: overwriting it could throw away a routing
// constraint the caller cared about.
func setZDRDirective(bodyMap map[string]any) error {
	switch existing := bodyMap["provider"].(type) {
	case nil:
		bodyMap["provider"] = map[string]any{"zdr": true}
	case map[string]any:
		existing["zdr"] = true
	default:
		return fmt.Errorf("request field \"provider\" is %T, want an object", existing)
	}
	return nil
}

// writePrivacyRefusal answers a request whose retention tolerance cannot be
// met.
//
// 403, not 400 or 503: the request is well-formed and the upstream is fine.
// What is missing is permission to send this prompt to that seat, which is
// exactly what 403 means. A 503 would invite a retry, and retrying will not
// help — the answer will be the same until the config or the tolerance
// changes.
func writePrivacyRefusal(w http.ResponseWriter, tier privacyTier, reason string) {
	if t := tier.String(); t != "" {
		w.Header().Set(PrivacyHeader, t)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "router refused: " + reason,
			"type":    "privacy_policy_violation",
			"code":    "privacy_tier_unavailable",
		},
	})
}
