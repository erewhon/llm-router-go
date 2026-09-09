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

// PrivacyZDR is the only tolerance value today: local, or a cloud endpoint
// held to zero data retention for this request.
//
// Named for the posture rather than for OpenRouter's spelling of it, so a
// second enforceable provider does not need a second header value.
const PrivacyZDR = "zdr"

// zdrRequired reports whether this request must be held to zero retention,
// and where that demand came from (for the error message when it cannot be
// met).
//
// Two sources, either sufficient:
//
//   - The resolved ROLE declares require.locality: local_or_zdr. Load-time
//     validation has already proved every candidate and overflow entry can
//     satisfy it, so this is belt-and-braces at request time — but it is the
//     belt that actually puts the directive on the wire.
//   - The CALLER sent X-Router-Privacy: zdr. This is the per-request
//     tightening: an ensemble reviewing a private diff can demand more than
//     its role's default without needing its own role.
//
// A caller may always tighten; nothing here lets one loosen. There is no
// X-Router-Privacy: any that would relax a role's declared tolerance, because
// the role's promise is not the caller's to waive.
func (rt *Router) zdrRequired(r *http.Request, res resolveResult) (required bool, source string) {
	if v := strings.TrimSpace(strings.ToLower(r.Header.Get(PrivacyHeader))); v != "" {
		if v == PrivacyZDR {
			return true, "request header " + PrivacyHeader + ": " + PrivacyZDR
		}
		// An unrecognised value is NOT ignored. Silently serving a request
		// that asked for a privacy posture the router does not implement is
		// the one failure mode this whole file exists to prevent.
		return true, "request header " + PrivacyHeader + ": " + v + " (unrecognised)"
	}
	if res.Role == "" {
		return false, ""
	}
	role, ok := rt.roles[res.Role]
	if !ok {
		return false, ""
	}
	if role.Require.Locality == config.LocalityLocalOrZDR {
		return true, "role " + res.Role + " requires " + string(config.LocalityLocalOrZDR)
	}
	return false, ""
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
func (rt *Router) applyPrivacy(bodyMap map[string]any, res resolveResult, source string) string {
	m, ok := rt.registry.Models[res.ModelID]
	if !ok {
		return fmt.Sprintf("%s, but %q is not a known model", source, res.ModelID)
	}
	if m.IsLocal() {
		return ""
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
func writePrivacyRefusal(w http.ResponseWriter, reason string) {
	w.Header().Set(PrivacyHeader, PrivacyZDR)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "router refused: " + reason,
			"type":    "privacy_policy_violation",
			"code":    "zdr_unavailable",
		},
	})
}
