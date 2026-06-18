package toolproxy

import (
	"fmt"
	"strings"

	"github.com/erewhon/llm-router-go/internal/config"
)

// resolveResult describes how to forward an incoming chat-completions
// request to its real upstream.
type resolveResult struct {
	// BackendURL is the upstream base URL (e.g. http://archimedes.local:5391).
	// The proxy appends /v1/chat/completions before forwarding.
	BackendURL string
	// BackendModel is the model name the upstream expects in the body.
	// SGLang/vLLM key off the hf_repo (with any "#suffix" stripped); the
	// proxy rewrites the incoming body's "model" field to this value.
	BackendModel string
	// ResolvedFrom records what the caller sent, useful for logging.
	ResolvedFrom string
	// ModelID is the registry key that matched (e.g. "nemotron-3-super").
	ModelID string
}

// resolveModel finds the registry entry for a model name as sent by an
// upstream (LiteLLM normally) or by a direct client. Tries, in order:
//   - exact match on the registry key (e.g. "nemotron-3-super")
//   - exact match on hf_repo with "#suffix" stripped
//   - exact match on any alias
//
// LiteLLM sometimes prefixes the model with "openai/" — that prefix is
// stripped before matching.
func resolveModel(r *config.ModelRegistry, model string) (resolveResult, error) {
	if r == nil {
		return resolveResult{}, fmt.Errorf("toolproxy: nil registry")
	}
	if model == "" {
		return resolveResult{}, fmt.Errorf("toolproxy: empty model")
	}

	want := strings.TrimPrefix(model, "openai/")

	for id, m := range r.Models {
		// Disabled models aren't routable. Skipping them also disambiguates
		// aliases shared between an enabled and a disabled model (e.g. an
		// alias listed on both the live model and a kept-for-rollback one):
		// without this, map iteration order could resolve to the disabled
		// entry and misroute. (The router always sends a concrete model_id,
		// so this mainly hardens direct/alias calls to the tool proxy.)
		if !m.Enabled {
			continue
		}
		hfBase := strings.SplitN(m.HFRepo, "#", 2)[0]
		matched := id == want ||
			hfBase == want ||
			m.HFRepo == want ||
			containsString(m.Aliases, want)
		if !matched {
			continue
		}
		// External models with their own api_base are managed elsewhere;
		// the tool proxy shouldn't be in their path. Reject with a clear
		// error so misroutes are obvious in logs.
		if m.Backend == config.BackendExternal {
			return resolveResult{}, fmt.Errorf("toolproxy: model %q is external (api_base=%s); the tool proxy is for local backends only", id, m.APIBase)
		}
		// Build the *real* backend URL by asking APIBase with
		// tool_proxy=false so we don't loop back to ourselves.
		toolProxyOff := false
		base, err := r.APIBase(id, &toolProxyOff)
		if err != nil {
			return resolveResult{}, fmt.Errorf("toolproxy: resolve %q: %w", id, err)
		}
		// APIBase returns ".../v1"; the proxy wants just the host:port.
		root := strings.TrimSuffix(base, "/v1")
		return resolveResult{
			BackendURL:   root,
			BackendModel: hfBase,
			ResolvedFrom: model,
			ModelID:      id,
		}, nil
	}
	return resolveResult{}, fmt.Errorf("toolproxy: unknown model %q", model)
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
