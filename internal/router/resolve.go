package router

import (
	"fmt"
	"strings"

	"github.com/erewhon/llm-router-go/internal/config"
)

// resolveResult describes how to forward an incoming front-door request.
type resolveResult struct {
	// BackendURL is the upstream base URL with any "/v1" suffix stripped
	// (e.g. http://hypatia.local:5391, http://192.168.42.240:5392, or an
	// external host). reverseProxyTo re-appends the request path.
	BackendURL string
	// BackendModel is the value written into the body's "model" field before
	// forwarding: the hf_repo (bare, "#suffix" stripped) for local + external
	// models, or the registry key for tool-proxy-routed models.
	BackendModel string
	// AuthBearer is the resolved key for external providers; "" for local
	// backends and the tool proxy. Sent as "Authorization: Bearer <value>"
	// unless AuthHeader overrides the destination header.
	AuthBearer string
	// AuthHeader, when non-empty, is the header name the key is written to
	// verbatim (no "Bearer " prefix), from the model's api_key_header. Empty
	// means the default "Authorization: Bearer" scheme.
	AuthHeader string
	// ModelID is the registry key that matched.
	ModelID string
	// ResolvedFrom records the model name the caller sent.
	ResolvedFrom string
	// ViaToolProxy is true when the request routes through the tool proxy.
	ViaToolProxy bool
	// APIClass is the model's declared endpoint family.
	APIClass config.APIClass
	// Egress is the VPN-exit spec derived from a "<model>-<egress>" alias
	// (E2), forwarded to the tool proxy as X-Egress. Empty for plain models.
	Egress string
	// Role is the semantic role the caller asked for ("coder"), empty when
	// they named a model or alias directly. Only role-resolved requests fail
	// over: an explicit model name stays explicit.
	Role string
	// Chain is the registry key of the fallback-chain entry the caller named
	// (directly or via alias), empty otherwise. Chain requests fail over like
	// roles, and additionally retry on upstream 5xx / error-envelope-in-2xx —
	// a chain names one model across providers, so answering from the next
	// provider keeps the caller's statement true.
	Chain string
	// Overflowed marks a role that ran out of in-contract candidates and fell
	// through to its declared overflow list — surfaced to the caller rather
	// than hidden, since it crossed the locality boundary.
	Overflowed bool
	// Remaining is the untried tail of the role's preference order, consumed
	// by the failover retry when the chosen upstream fails to answer.
	Remaining []roleCandidate
}

// resolveModel maps an incoming model name to its upstream. It matches, in
// priority order:
//   - exact registry key (deterministic, most specific)
//   - hf_repo (bare or with "#suffix")
//   - role (dynamic: binds to the first available candidate)
//   - any alias
//
// Roles sit ahead of aliases so a role name always wins. Config validation
// already forbids a role from colliding with a model id or alias, so the
// ordering is belt-and-braces rather than load-bearing.
//
// The concrete matches (key, hf_repo, alias) never fail over: naming a model
// is a statement about *that* model, and silently answering from a different
// one would be worse than an honest error. Only roles reassign.
//
// Only models routable in the current mode are considered. A leading "openai/"
// prefix (which some clients/LiteLLM add) is stripped before matching.
//
// forceDirect short-circuits tool-proxy routing: pass true for endpoints the
// tool proxy doesn't implement (/v1/completions, /v1/embeddings, /v1/rerank)
// so the request goes straight to the node backend regardless of the model's
// tool_proxy flag or per-alias override. /v1/chat/completions passes false.
func (rt *Router) resolveModel(model string, forceDirect bool) (resolveResult, error) {
	if rt.registry == nil {
		return resolveResult{}, fmt.Errorf("router: nil registry")
	}
	if model == "" {
		return resolveResult{}, fmt.Errorf("router: empty model")
	}

	want := strings.TrimPrefix(model, "openai/")

	if id, m, ok := rt.lookupConcrete(want); ok {
		if len(m.Fallbacks) > 0 {
			return rt.resolveChain(id, m, model, forceDirect)
		}
		return rt.buildResult(id, m, "", model, forceDirect, "")
	}
	if _, isRole := rt.roles[want]; isRole {
		return rt.resolveRole(want, model, forceDirect)
	}
	if id, m, alias, ok := rt.lookupAlias(want); ok {
		if len(m.Fallbacks) > 0 {
			// Alias of a chain entry resolves the chain. Per-alias overrides
			// don't apply — chain members carry their own routing config.
			return rt.resolveChain(id, m, model, forceDirect)
		}
		return rt.buildResult(id, m, alias, model, forceDirect, "")
	}
	// E2 model-egress aliases: "<base>-<egress>" where <base> resolves to a
	// tool-proxy model — forward <base> and pass the suffix to the tool proxy
	// as X-Egress. Only for chat (tool-proxy) requests, never forceDirect ones.
	if !forceDirect {
		if res, ok := rt.resolveEgressAlias(want, model); ok {
			return res, nil
		}
	}
	return resolveResult{}, fmt.Errorf("router: unknown model %q", model)
}

// lookupConcrete matches a name that identifies exactly one model: the registry
// key, or its hf_repo (bare or with "#suffix").
func (rt *Router) lookupConcrete(want string) (string, config.ModelDefinition, bool) {
	if m, ok := rt.active[want]; ok {
		return want, m, true
	}
	for id, m := range rt.active {
		hfBase := strings.SplitN(m.HFRepo, "#", 2)[0]
		if hfBase == want || m.HFRepo == want {
			return id, m, true
		}
	}
	return "", config.ModelDefinition{}, false
}

// lookupAlias matches a name against the aliases of active models. Returns the
// matched id, definition, and the alias used (so per-alias overrides apply).
func (rt *Router) lookupAlias(want string) (string, config.ModelDefinition, string, bool) {
	for id, m := range rt.active {
		for _, a := range m.Aliases {
			if a == want {
				return id, m, a, true
			}
		}
	}
	return "", config.ModelDefinition{}, "", false
}

// resolveEgressAlias handles "<base>-<egress>" names. It tries the LONGEST base
// prefix that resolves (so a model whose own name has hyphens, e.g.
// "nemotron-3-super-se", wins over a shorter accidental match), accepting the
// first base that routes through the tool proxy; the remainder is the egress
// spec. Returns ok=false if no resolvable tool-proxy base is found.
func (rt *Router) resolveEgressAlias(want, original string) (resolveResult, bool) {
	for i := strings.LastIndex(want, "-"); i > 0; i = strings.LastIndex(want[:i], "-") {
		base, egress := want[:i], want[i+1:]
		res, err := rt.resolveEgressBase(base, original, egress)
		if err != nil || !res.ViaToolProxy {
			continue // base isn't tool-proxy-routed; an egress suffix is meaningless
		}
		return res, true
	}
	return resolveResult{}, false
}

// resolveEgressBase resolves the base half of a "<base>-<egress>" name using
// the same precedence as resolveModel, so "research-se" works whether
// "research" is a model, an alias, or a role.
func (rt *Router) resolveEgressBase(base, original, egress string) (resolveResult, error) {
	if id, m, ok := rt.lookupConcrete(base); ok {
		return rt.buildResult(id, m, "", original, false, egress)
	}
	if _, isRole := rt.roles[base]; isRole {
		res, err := rt.resolveRole(base, original, false)
		if err != nil {
			return resolveResult{}, err
		}
		res.Egress = egress
		return res, nil
	}
	if id, m, alias, ok := rt.lookupAlias(base); ok {
		return rt.buildResult(id, m, alias, original, false, egress)
	}
	return resolveResult{}, fmt.Errorf("router: unknown egress base %q", base)
}

// chainUnavailableError is returned when every provider in a fallback chain
// is unroutable. Mirrors roleUnavailableError: 503 with per-provider reasons,
// so "the name is wrong" stays distinguishable from "every provider is down".
type chainUnavailableError struct {
	Chain   string
	Reasons []string
}

func (e *chainUnavailableError) Error() string {
	if len(e.Reasons) == 0 {
		return fmt.Sprintf("model %q has no providers routable in this mode", e.Chain)
	}
	return fmt.Sprintf("model %q has no available provider: %s", e.Chain, strings.Join(e.Reasons, "; "))
}

// resolveChain binds a fallback-chain entry to its first routable provider.
// A non-virtual entry is its own first candidate; the fallbacks follow in
// declared order. The untried tail lands in Remaining so the failover retry
// can advance without re-resolving.
func (rt *Router) resolveChain(id string, m config.ModelDefinition, original string, forceDirect bool) (resolveResult, error) {
	order := make([]roleCandidate, 0, len(m.Fallbacks)+1)
	if !m.IsVirtual() {
		order = append(order, roleCandidate{ModelID: id})
	}
	for _, fid := range m.Fallbacks {
		order = append(order, roleCandidate{ModelID: fid})
	}

	reasons := make([]string, 0, len(order))
	for i, cand := range order {
		cm, known := rt.active[cand.ModelID]
		if !known {
			reasons = append(reasons, fmt.Sprintf("%s: not routable in this mode", cand.ModelID))
			continue
		}
		if !rt.avail.Routable(cand.ModelID) {
			reasons = append(reasons, fmt.Sprintf("%s: %s", cand.ModelID, rt.avail.Reason(cand.ModelID)))
			continue
		}
		res, err := rt.buildResult(cand.ModelID, cm, "", original, forceDirect, "")
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", cand.ModelID, err))
			continue
		}
		res.Chain = id
		res.Remaining = order[i+1:]
		return res, nil
	}
	return resolveResult{}, &chainUnavailableError{Chain: id, Reasons: reasons}
}

// buildResult assembles the forwarding decision for a matched model.
// matchedAlias is the alias the caller used (or ""), so per-alias tool_proxy
// overrides apply. forceDirect (set by non-chat endpoints) trumps both the
// model's tool_proxy flag and any alias override.
func (rt *Router) buildResult(id string, m config.ModelDefinition, matchedAlias, original string, forceDirect bool, egress string) (resolveResult, error) {
	// Override precedence: forceDirect (endpoint) > alias override > model default.
	var override *bool
	switch {
	case forceDirect:
		f := false
		override = &f
	case matchedAlias != "":
		if ov, ok := m.AliasOverrides[matchedAlias]; ok && ov.ToolProxy != nil {
			override = ov.ToolProxy
		}
	}

	base, err := rt.registry.APIBase(id, override)
	if err != nil {
		return resolveResult{}, fmt.Errorf("router: resolve %q: %w", id, err)
	}
	root := strings.TrimSuffix(base, "/v1")

	effectiveToolProxy := m.ToolProxy
	if override != nil {
		effectiveToolProxy = *override
	}
	// External models never count as "via tool proxy" even when their api_base
	// happens to point at it (the auto-router stubs): those forward the hf_repo,
	// not a registry key, because the tool proxy keys auto-routing off the name.
	viaToolProxy := m.Backend != config.BackendExternal && effectiveToolProxy

	backendModel := strings.SplitN(m.HFRepo, "#", 2)[0]
	if viaToolProxy {
		// The tool proxy disambiguates shared hf_repos by registry key, so the
		// router forwards the model_id (PLAN Phase 3: "model_id preserved").
		backendModel = id
	}

	auth, authHeader := "", ""
	if m.Backend == config.BackendExternal {
		auth = rt.resolveKey(m.APIKey)
		authHeader = m.APIKeyHeader
	}

	return resolveResult{
		BackendURL:   root,
		BackendModel: backendModel,
		AuthBearer:   auth,
		AuthHeader:   authHeader,
		ModelID:      id,
		ResolvedFrom: original,
		ViaToolProxy: viaToolProxy,
		APIClass:     m.APIClass,
		Egress:       egress,
	}, nil
}

// resolveKey turns a models.yaml api_key value into a bearer token. A value
// starting with "sk-" is a literal key; anything else is an environment
// variable name (matching generate_config.py's os.environ/ behaviour).
func (rt *Router) resolveKey(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "sk-") {
		return raw
	}
	return rt.getenv(raw)
}
