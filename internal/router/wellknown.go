package router

import (
	"encoding/json"
	"net/http"

	"github.com/erewhon/llm-router-go/internal/config"
)

// WellKnownSchemaURL is the OpenCode config JSON Schema URL the well-known
// endpoint references in its `$schema` field.
const WellKnownSchemaURL = "https://opencode.ai/config.json"

// WellKnownConfig captures the values needed to materialize the response of
// the GET /.well-known/opencode endpoint. An empty ProviderID disables the
// endpoint (the handler returns 404), so cmd/router can leave the flag unset
// during dev or when the router is behind something else that serves it.
//
// One entry is emitted per alias of every chat-class model in the active
// (mode-filtered) set. Models with no aliases fall back to their registry
// id as the alias. Non-chat api_classes are skipped — OpenCode is a chat
// client and pointing it at /v1/embeddings would just confuse the agent.
type WellKnownConfig struct {
	// ProviderID is the key under "provider" in the JSON (e.g. "llm").
	// Empty disables the endpoint entirely.
	ProviderID string
	// ProviderName is the human label shown in OpenCode (e.g. "LLM Router").
	ProviderName string
	// BaseURL is the OpenAI-compatible URL OpenCode will POST to (the
	// router's public URL, e.g. https://llm.bcc.sh/v1).
	BaseURL string
	// SetupURL is where a person mints a personal access token (the
	// dashboard's Tokens dialog, e.g. https://llm-dashboard.bcc.sh). It is
	// printed by the auth command below. Empty falls back to generic wording.
	//
	// The document never carries a credential. Until 2026-09-11 it shipped a
	// shared key (`echo <key>` as the auth command, plus options.apiKey),
	// which meant every OpenCode install that bootstrapped here authenticated
	// as legacy:<fingerprint> forever, and the legacy burn-down could never
	// reach zero. Now that anyone with SSO can mint their own PAT, the
	// bootstrap hands out instructions instead.
	SetupURL string
	// AuthEnv is the env-var name OpenCode sets to the fetched secret
	// (the "env" field inside the top-level `auth` block). Empty defaults
	// to "LLM_ROUTER_API_KEY".
	AuthEnv string
	// DefaultContext / DefaultOutput populate every emitted model's
	// limit.{context, output}. Zero means use the schema-default
	// (131072 / 32768, matching the existing static file in the
	// /var/lib/opencode-wellknown deployment).
	DefaultContext int
	DefaultOutput  int
}

// configured reports whether the endpoint should serve.
func (c WellKnownConfig) configured() bool { return c.ProviderID != "" }

// wellKnownDoc is the wire shape OpenCode CLI expects: a top-level `auth`
// block (so `opencode providers login` finds `u.auth.command`) plus a
// `config` block that nests the provider map. Earlier versions of this
// file emitted just `{$schema, provider}` and broke `opencode providers
// login` with `undefined is not an object (evaluating 'u.auth.command')`.
type wellKnownDoc struct {
	Auth   wellKnownAuth   `json:"auth"`
	Config wellKnownConfig `json:"config"`
}

type wellKnownAuth struct {
	// Command opencode runs to fetch the secret. Conventionally
	// ["echo", "<key>"] when a key is materialized server-side; here it is
	// a command that prints how to get a personal token and exits non-zero,
	// so `opencode providers login` shows the instructions rather than
	// silently storing nothing.
	Command []string `json:"command"`
	// Env is the env var name opencode sets to the fetched secret.
	Env string `json:"env"`
}

type wellKnownConfig struct {
	Schema   string                  `json:"$schema,omitempty"`
	Provider map[string]wellKnownPrv `json:"provider"`
}

type wellKnownPrv struct {
	NPM     string                    `json:"npm"`
	Name    string                    `json:"name"`
	Options wellKnownPrvOpts          `json:"options"`
	Models  map[string]wellKnownModel `json:"models"`
}

type wellKnownPrvOpts struct {
	BaseURL string `json:"baseURL"`
	// No apiKey, deliberately: see WellKnownConfig.SetupURL. OpenCode reads
	// the key from its own auth store (`/connect` → Other → this provider
	// id) or from the auth.env variable.
}

type wellKnownModel struct {
	Name  string         `json:"name"`
	Limit wellKnownLimit `json:"limit"`
	// Cost is omitted for models the registry doesn't price (nil). OpenCode
	// renders per-request $ from it; without it, no cost shows.
	Cost *wellKnownCost `json:"cost,omitempty"`
}

type wellKnownLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// wellKnownCost mirrors the OpenCode config schema's model.cost object. Values
// are USD per 1M tokens — the same unit as models.yaml's *_cost_per_million,
// so they map across directly. cache_read/cache_write are omitted (the
// registry has no cache pricing).
type wellKnownCost struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// buildWellKnown materializes the document from the router's active model set
// using the configured WellKnownConfig.
func (rt *Router) buildWellKnown() wellKnownDoc {
	cfg := rt.wellKnown
	ctxLimit := cfg.DefaultContext
	if ctxLimit == 0 {
		ctxLimit = 131072
	}
	outLimit := cfg.DefaultOutput
	if outLimit == 0 {
		outLimit = 32768
	}

	models := map[string]wellKnownModel{}
	cat, ids := rt.catalog()
	for _, id := range ids {
		m := cat[id]
		if m.APIClass != config.APIClassChat {
			continue
		}
		// Same rule as /v1/models: an entry absent from its base's live
		// listing is not advertised. OpenCode caches this document, so a
		// stale entry here outlives the drift by a whole session.
		if rt.absent(id) {
			continue
		}
		// Emit cost when the registry prices the model — including an explicit
		// 0/0 for free local models (accurate: OpenCode shows $0.00). Omit it
		// only when unpriced (both nil), so those models simply show no cost
		// rather than a misleading zero. Shared across the model's aliases.
		var cost *wellKnownCost
		if m.InputCostPerMillion != nil || m.OutputCostPerMillion != nil {
			cost = &wellKnownCost{
				Input:  derefFloat(m.InputCostPerMillion),
				Output: derefFloat(m.OutputCostPerMillion),
			}
		}
		mctx := rt.wellKnownContext(m, ctxLimit)
		mout := rt.wellKnownOutput(m, outLimit)
		emit := func(name string) {
			models[name] = wellKnownModel{
				Name:  name,
				Limit: wellKnownLimit{Context: mctx, Output: mout},
				Cost:  cost,
			}
		}
		// Canonical id AND aliases: since the 2026-08-28 normalization the
		// canonical name is load-bearing (bare chain names like kimi-k3,
		// provider ids like or/kimi-k3), so hiding it behind aliases would
		// leave the catalog unable to name what reqlog and the dashboard
		// report. Matches /v1/models, which lists both.
		emit(id)
		for _, a := range m.Aliases {
			emit(a)
		}
	}

	// Roles are what an OpenCode user actually wants bound to a keybinding:
	// "coder" keeps working when hypatia is powered down, where
	// "qwen3.6-local" does not. Priced from the role's first candidate — the
	// one it normally resolves to — so a local role still shows $0.00.
	for name, rd := range rt.roles {
		if _, dup := models[name]; dup {
			continue
		}
		var cost *wellKnownCost
		rctx, rout := ctxLimit, outLimit
		if len(rd.Candidates) > 0 {
			if m, ok := rt.lookupModel(rd.Candidates[0]); ok {
				if m.APIClass != config.APIClassChat {
					continue
				}
				if m.InputCostPerMillion != nil || m.OutputCostPerMillion != nil {
					cost = &wellKnownCost{
						Input:  derefFloat(m.InputCostPerMillion),
						Output: derefFloat(m.OutputCostPerMillion),
					}
				}
				rctx = rt.wellKnownContext(m, ctxLimit)
				rout = rt.wellKnownOutput(m, outLimit)
			}
		}
		models[name] = wellKnownModel{
			Name:  name,
			Limit: wellKnownLimit{Context: rctx, Output: rout},
			Cost:  cost,
		}
	}

	authEnv := cfg.AuthEnv
	if authEnv == "" {
		authEnv = "LLM_ROUTER_API_KEY"
	}

	return wellKnownDoc{
		Auth: wellKnownAuth{
			Command: setupCommand(cfg, authEnv),
			Env:     authEnv,
		},
		Config: wellKnownConfig{
			Schema: WellKnownSchemaURL,
			Provider: map[string]wellKnownPrv{
				cfg.ProviderID: {
					NPM:  "@ai-sdk/openai-compatible",
					Name: cfg.ProviderName,
					Options: wellKnownPrvOpts{
						BaseURL: cfg.BaseURL,
					},
					Models: models,
				},
			},
		},
	}
}

// SetupInstructions is the human text explaining how to get a credential for
// the well-known provider. Shared by the auth command and the dashboard so
// the two surfaces teach the same steps.
func SetupInstructions(providerID, setupURL, authEnv string) string {
	where := "from the router dashboard (Tokens)"
	if setupURL != "" {
		where = "at " + setupURL + " (Tokens)"
	}
	if providerID == "" {
		providerID = "llm"
	}
	return "This provider needs a personal access token. Mint one " + where +
		", then in OpenCode run /connect (or: opencode auth login), choose Other, enter provider id " +
		providerID + ", and paste the token. Scripts can export " + authEnv + " instead."
}

// setupCommand builds the auth command: print the instructions to stderr and
// fail, so a tool that runs it surfaces the text instead of an empty secret.
// The message is passed as an argument, never interpolated into the script,
// so no value from config can change what the shell executes.
func setupCommand(cfg WellKnownConfig, authEnv string) []string {
	return []string{"sh", "-c", `printf '%s\n' "$1" >&2; exit 1`, "opencode-setup",
		SetupInstructions(cfg.ProviderID, cfg.SetupURL, authEnv)}
}

// handleWellKnown serves GET /.well-known/opencode. 404 when the endpoint is
// not configured (empty ProviderID). The response is pretty-printed JSON with
// `no-store` cache headers so OpenCode picks up model changes on the next
// restart without a stale-cache trap.
func (rt *Router) handleWellKnown(w http.ResponseWriter, r *http.Request) {
	if !rt.wellKnown.configured() {
		http.Error(w, "well-known not configured", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rt.buildWellKnown()); err != nil {
		rt.logger.ErrorContext(r.Context(), "well-known encode failed", "err", err)
	}
}

// wellKnownContext resolves the context window advertised for a model:
// an explicit context_length wins, then vllm_args.max_model_len (the
// engine flag the node agent actually passes), then — for a virtual chain
// entry — the first provider in its chain, and finally the endpoint
// default. Chains are one level deep by config validation, so the
// recursion terminates at the provider entry.
func (rt *Router) wellKnownContext(m config.ModelDefinition, def int) int {
	if m.ContextLength > 0 {
		return m.ContextLength
	}
	if m.VllmArgs.MaxModelLen > 0 {
		return m.VllmArgs.MaxModelLen
	}
	if m.IsVirtual() {
		if first, ok := rt.active[m.Fallbacks[0]]; ok {
			if first.ContextLength > 0 {
				return first.ContextLength
			}
			if first.VllmArgs.MaxModelLen > 0 {
				return first.VllmArgs.MaxModelLen
			}
		}
	}
	return def
}

// wellKnownOutput resolves the advertised output cap: an explicit
// max_output_tokens, then — for a virtual chain entry — the first provider's,
// then the endpoint default. Note OpenCode itself clamps the max_tokens it
// sends to min(limit.output, 32000); larger values still drive its thinking
// budget and cost display, so publish the real cap.
func (rt *Router) wellKnownOutput(m config.ModelDefinition, def int) int {
	if m.MaxOutputTokens > 0 {
		return m.MaxOutputTokens
	}
	if m.IsVirtual() {
		if first, ok := rt.active[m.Fallbacks[0]]; ok && first.MaxOutputTokens > 0 {
			return first.MaxOutputTokens
		}
	}
	return def
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
