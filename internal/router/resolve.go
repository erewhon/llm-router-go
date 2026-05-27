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
	// AuthBearer is the resolved bearer token for external providers; "" for
	// local backends and the tool proxy.
	AuthBearer string
	// ModelID is the registry key that matched.
	ModelID string
	// ResolvedFrom records the model name the caller sent.
	ResolvedFrom string
	// ViaToolProxy is true when the request routes through the tool proxy.
	ViaToolProxy bool
	// APIClass is the model's declared endpoint family.
	APIClass config.APIClass
}

// resolveModel maps an incoming model name to its upstream. It matches, in
// priority order:
//   - exact registry key (deterministic, most specific)
//   - hf_repo (bare or with "#suffix")
//   - any alias
//
// Only models routable in the current mode are considered. A leading "openai/"
// prefix (which some clients/LiteLLM add) is stripped before matching.
func (rt *Router) resolveModel(model string) (resolveResult, error) {
	if rt.registry == nil {
		return resolveResult{}, fmt.Errorf("router: nil registry")
	}
	if model == "" {
		return resolveResult{}, fmt.Errorf("router: empty model")
	}

	want := strings.TrimPrefix(model, "openai/")

	if m, ok := rt.active[want]; ok {
		return rt.buildResult(want, m, "", model)
	}
	for id, m := range rt.active {
		hfBase := strings.SplitN(m.HFRepo, "#", 2)[0]
		if hfBase == want || m.HFRepo == want {
			return rt.buildResult(id, m, "", model)
		}
		for _, a := range m.Aliases {
			if a == want {
				return rt.buildResult(id, m, a, model)
			}
		}
	}
	return resolveResult{}, fmt.Errorf("router: unknown model %q", model)
}

// buildResult assembles the forwarding decision for a matched model. matchedAlias
// is the alias the caller used (or ""), so per-alias tool_proxy overrides apply.
func (rt *Router) buildResult(id string, m config.ModelDefinition, matchedAlias, original string) (resolveResult, error) {
	// An alias may opt in/out of tool-proxy routing independently of its model.
	var override *bool
	if matchedAlias != "" {
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

	auth := ""
	if m.Backend == config.BackendExternal {
		auth = rt.resolveKey(m.APIKey)
	}

	return resolveResult{
		BackendURL:   root,
		BackendModel: backendModel,
		AuthBearer:   auth,
		ModelID:      id,
		ResolvedFrom: original,
		ViaToolProxy: viaToolProxy,
		APIClass:     m.APIClass,
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
