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

// parsePrivacyHeader reads the caller's X-Router-Privacy, if any.
//
// An UNRECOGNISED value is a hard refusal, returned as refuse != "". It must
// never be mapped onto a real tier: a caller who typed a posture the router
// does not implement ("eu-only", or "locl") has not been given what they
// asked for, and serving them anyway lets them believe they were. An earlier
// version let an unrecognised value fall through and, on an OpenRouter seat,
// quietly serve it as zdr — which would have sent a script's misspelled
// local-only request to the cloud. Fixed 2026-09-10.
//
// Parsed BEFORE resolution, because the tier is a filter on which candidates
// a role may even consider (see privacyGate), not a check on whichever one
// happened to come first. "any" and an absent header are the same thing: no
// requirement of the caller's own.
func parsePrivacyHeader(r *http.Request) (tier privacyTier, source, refuse string) {
	switch v := strings.TrimSpace(strings.ToLower(r.Header.Get(PrivacyHeader))); v {
	case "", PrivacyAny:
		return tierNone, "", ""
	case PrivacyZDR:
		return tierZDR, "request header " + PrivacyHeader + ": " + PrivacyZDR, ""
	case PrivacyLocal:
		return tierLocal, "request header " + PrivacyHeader + ": " + PrivacyLocal, ""
	default:
		return tierNone, "", fmt.Sprintf("request header %s: %q is not a recognised privacy tier (want %q, %q or %q)",
			PrivacyHeader, v, PrivacyLocal, PrivacyZDR, PrivacyAny)
	}
}

// effectiveTier combines a role's declared tolerance with the caller's, taking
// the STRICTER.
//
// Only locality: local_or_zdr produces a role-derived requirement (tier zdr).
// A role declaring locality: local deliberately does NOT: its overflow list is
// exempt by design — crossing the local/cloud boundary when the fleet is down
// is what that overflow is for — and enforcing "local" at dispatch would break
// exactly that. The caller is the only source of `local`. A caller may always
// tighten; "any" tightens nothing and loosens nothing, because the role's
// promise is not the caller's to waive.
func effectiveTier(rd config.RoleDefinition, caller privacyTier) privacyTier {
	if rd.Require.Locality == config.LocalityLocalOrZDR && caller < tierZDR {
		return tierZDR
	}
	return caller
}

// privacyGate reports why a candidate fails a tier, or "" when it passes.
//
// This is the resolution-time half of enforcement, and it runs FIRST in the
// candidate walk — before availability, before the context envelope. Policy is
// a property of the config and the header, not of fleet health, so it is the
// most useful thing to say about a candidate, and checking it first is what
// lets the walk classify a total miss exactly: if every candidate fell here,
// no retry can ever help and the caller gets 403; if any compliant candidate
// got past here and was merely down, that is a 503 worth retrying.
//
// The tier is a filter, not a verdict on the first choice: a role listing an
// OpenRouter seat ahead of a local one still serves a `local` request — from
// the local one. Before 2026-09-11 the check ran only at dispatch, on
// whichever candidate resolution had already picked, and refused even when a
// compliant candidate sat further down the order.
func privacyGate(m config.ModelDefinition, tier privacyTier) string {
	if tier == tierNone || m.IsLocal() {
		return ""
	}
	if tier == tierLocal {
		// No directive can make a cloud endpoint local — a ZDR-enforceable
		// seat is still somebody else's computer.
		return "not on fleet hardware (privacy: local)"
	}
	if !m.ZDREnforceable() {
		return "neither local nor a zero-retention-enforceable endpoint (privacy: zdr)"
	}
	return ""
}

// privacyRefusedError is returned when a role or chain has candidates, but the
// privacy tier excluded every one of them, and none of the exclusions was
// about health.
//
// It is a distinct type — not a roleUnavailableError with different words —
// because the two demand opposite reactions and the HTTP edge must be able to
// tell them apart. A 503 says "nothing can serve this RIGHT NOW; retry, or fail
// over to another seat". Forge's ensemble classifier does exactly that with
// every 5xx. A policy refusal can never succeed on retry, and reporting it as
// transient would have a panel retrying a refused seat and then describing it
// as "will retry" instead of "refused by privacy policy". So it is a 403: the
// caller's own policy, not the fleet's health, is what blocked the request.
type privacyRefusedError struct {
	// Subject is the role or chain name the caller asked for.
	Subject string
	Tier    privacyTier
	// Excluded lists every candidate and why, mirroring the per-candidate
	// reasons roleUnavailableError carries, so the caller can see what would
	// have served under a looser tier.
	Excluded []string
}

func (e *privacyRefusedError) Error() string {
	return fmt.Sprintf("%q has no candidate that satisfies privacy tier %q: %s",
		e.Subject, e.Tier.String(), strings.Join(e.Excluded, "; "))
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
	m, ok := rt.lookupModel(res.ModelID)
	if !ok {
		return fmt.Sprintf("%s, but %q is not a known model", source, res.ModelID)
	}
	// For a role or chain the walk has already applied privacyGate, so this
	// cannot fire; it is what makes a DIRECTLY NAMED model honour the tier,
	// since a bare name goes through no candidate walk at all.
	if why := privacyGate(m, tier); why != "" {
		return fmt.Sprintf("%s, but %q is %s", source, res.ModelID, why)
	}
	if m.IsLocal() {
		return ""
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
//
// excluded, when non-empty, is the per-candidate list from a
// privacyRefusedError, carried as a structured field beside the prose so a
// program can read which seats were passed over and why without parsing the
// message.
func writePrivacyRefusal(w http.ResponseWriter, tier privacyTier, reason string, excluded []string) {
	if t := tier.String(); t != "" {
		w.Header().Set(PrivacyHeader, t)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	errObj := map[string]any{
		"message": "router refused: " + reason,
		"type":    "privacy_policy_violation",
		"code":    "privacy_tier_unavailable",
	}
	if len(excluded) > 0 {
		errObj["privacy_tier"] = tier.String()
		errObj["excluded_candidates"] = excluded
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": errObj})
}
