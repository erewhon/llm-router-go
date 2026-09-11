package router

// Session affinity for upstreams that require the caller to name a session.
//
// OpenCode Zen — both the pay-go tier (opencode.ai/zen/v1) and the Go
// flat-rate tier (opencode.ai/zen/go/v1) — began HARD-REJECTING requests that
// carry no x-opencode-session header, around 2026-09-10:
//
//	400 MissingSessionID — "Request is missing x-opencode-session and cannot
//	be routed efficiently."
//
// Proven with the router out of the path entirely, straight to Zen: no header
// → 400, any header → 200. The OpenCode app sends one automatically, so the
// only callers hitting this were ones reaching Zen through the router — which,
// until this file, forwarded a caller's header but never originated one. The
// visible casualty was the coder-hard role, whose candidates are Zen chains:
// the 400 is a provider "answer", so the chain passes it through instead of
// failing over to the OpenRouter member behind it.
//
// WHAT THE HEADER IS FOR, AND WHY THE VALUE IS NOT RANDOM. A session header
// exists so the provider can keep one conversation on one backend and its
// prompt cache warm. A random value per request would satisfy the check and
// throw that away. So the value is derived from the conversation itself —
// the model plus the first system message plus the first non-system message —
// which is the same way OpenRouter identifies a conversation for its own
// sticky routing. Those stay constant across every turn of an agent loop, so
// turn 40 lands on the same session as turn 1.
//
// WHAT THE VALUE DELIBERATELY DOES NOT CONTAIN: who is asking. Folding the
// PAT principal in would buy nothing for caching, and would hand the provider
// a stable per-person signal it does not otherwise have — the router talks to
// Zen with one shared key for everyone, so today Zen cannot tell household
// members apart. With a handful of likely principal names, a principal inside
// the hash is brute-forceable back to a name. Content only: Zen already has
// the content, so a hash of it tells them nothing new.
//
// WHAT THIS DOES NOT DO: add OpenRouter's equivalent (x-session-id). A fresh
// router-minted session there creates a new stickiness bucket and can pin a
// conversation to a provider that does not cache at all — measured
// 2026-09-09, it is exactly how the router got stuck on a non-caching
// endpoint. That needs provider ordering to steer it first; see the Forge
// task "Provider pinning for prompt-cache locality".

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// sessionHeaderHosts maps an upstream host to the session header it requires.
// A small table rather than a models.yaml field for the same reason
// zdrEnforceableHosts is one: it is a fact about a provider's API, true of
// every model that provider serves, not a per-model setting to keep in sync
// across 22 entries.
var sessionHeaderHosts = map[string]string{
	"opencode.ai": "X-Opencode-Session",
}

// sessionIDPrefix marks a value the router minted, so a provider-side log or
// a support ticket can tell it apart from one the OpenCode app sent.
const sessionIDPrefix = "llmr-"

// setSessionAffinity adds the provider's session header to one OUTBOUND
// request when the caller did not supply one.
//
// It writes to the outbound copy, never the inbound request. That matters on
// failover: a chain that falls from a zen/ member to an or/ member makes a
// second outbound request built fresh from the inbound one, so the header
// added for Zen does not ride along to OpenRouter.
//
// A caller-supplied value always wins and is never overwritten. The OpenCode
// app sends a real session id; replacing it with ours would split one of its
// conversations across two provider sessions.
func setSessionAffinity(out *http.Request, target *url.URL, body []byte) {
	name, ok := sessionHeaderHosts[strings.ToLower(target.Hostname())]
	if !ok {
		return
	}
	if out.Header.Get(name) != "" {
		return
	}
	out.Header.Set(name, deriveSessionID(body))
}

// deriveSessionID turns a request body into a stable session id.
//
// Chat requests key on (model, first system-or-developer message, first
// non-system message) — constant for the life of a conversation. Anything
// without messages (completions, image generation) keys on the whole body,
// which is still deterministic, just scoped to that exact request. There is
// no random fallback: a deterministic value means an identical retry lands on
// the same session, and it keeps the function testable.
func deriveSessionID(body []byte) string {
	var req struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
	}
	h := sha256.New()
	if err := json.Unmarshal(body, &req); err == nil && len(req.Messages) > 0 {
		h.Write([]byte(req.Model))
		h.Write([]byte{0})
		var system, first []byte
		for _, raw := range req.Messages {
			role, canon, ok := canonicalMessage(raw)
			if !ok {
				continue
			}
			isSystem := role == "system" || role == "developer"
			if isSystem && system == nil {
				system = canon
			}
			if !isSystem && first == nil {
				first = canon
			}
			if system != nil && first != nil {
				break
			}
		}
		h.Write(system)
		h.Write([]byte{0})
		h.Write(first)
	} else {
		h.Write(body)
	}
	return sessionIDPrefix + hex.EncodeToString(h.Sum(nil))[:32]
}

// canonicalMessage reduces a message to its role and a canonical encoding of
// role + content.
//
// Re-marshalling through `any` is what makes this stable: encoding/json sorts
// map keys, so {"type":"text","text":"x"} and {"text":"x","type":"text"} —
// the same content part written by two different clients — hash identically.
// Only role and content participate; per-message fields that can vary between
// turns of one conversation (cache_control markers, name) must not split it.
func canonicalMessage(raw json.RawMessage) (role string, canon []byte, ok bool) {
	var m struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", nil, false
	}
	// cache_control usually lives INSIDE content parts, not on the message,
	// and Anthropic-style clients move that breakpoint toward the newest
	// message each turn — so the first user message can carry it on turn 1
	// and lose it on turn 2. Left in, it would give one conversation two
	// session ids exactly when caching matters most.
	if parts, isArray := m.Content.([]any); isArray {
		for _, p := range parts {
			if obj, isObj := p.(map[string]any); isObj {
				delete(obj, "cache_control")
			}
		}
	}
	b, err := json.Marshal(struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{m.Role, m.Content})
	if err != nil {
		return "", nil, false
	}
	return strings.ToLower(m.Role), b, true
}
