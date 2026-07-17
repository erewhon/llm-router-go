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
}

// resolveModel maps an incoming model name to its upstream. It matches, in
// priority order:
//   - exact registry key (deterministic, most specific)
//   - hf_repo (bare or with "#suffix")
//   - any alias
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

	if id, m, alias, ok := rt.lookup(want); ok {
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

// lookup matches a model name against the active registry by exact key, hf_repo
// (bare or with "#suffix"), then alias. Returns the matched id, definition, the
// alias used (or ""), and whether anything matched.
func (rt *Router) lookup(want string) (string, config.ModelDefinition, string, bool) {
	if m, ok := rt.active[want]; ok {
		return want, m, "", true
	}
	for id, m := range rt.active {
		hfBase := strings.SplitN(m.HFRepo, "#", 2)[0]
		if hfBase == want || m.HFRepo == want {
			return id, m, "", true
		}
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
		id, m, alias, ok := rt.lookup(base)
		if !ok {
			continue
		}
		res, err := rt.buildResult(id, m, alias, original, false, egress)
		if err != nil || !res.ViaToolProxy {
			continue // base isn't tool-proxy-routed; an egress suffix is meaningless
		}
		return res, true
	}
	return resolveResult{}, false
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
