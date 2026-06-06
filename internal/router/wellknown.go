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
	// APIKey is the bearer OpenCode should send. Omitted from JSON when
	// empty so OpenCode can prompt or fall back to its own auth flow.
	APIKey string
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
	// ["echo", "<key>"] when the key is materialized server-side.
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
	APIKey  string `json:"apiKey,omitempty"`
}

type wellKnownModel struct {
	Name  string         `json:"name"`
	Limit wellKnownLimit `json:"limit"`
}

type wellKnownLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
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
	for id, m := range rt.active {
		if m.APIClass != config.APIClassChat {
			continue
		}
		emit := func(name string) {
			models[name] = wellKnownModel{
				Name:  name,
				Limit: wellKnownLimit{Context: ctxLimit, Output: outLimit},
			}
		}
		if len(m.Aliases) == 0 {
			emit(id)
			continue
		}
		for _, a := range m.Aliases {
			emit(a)
		}
	}

	authEnv := cfg.AuthEnv
	if authEnv == "" {
		authEnv = "LLM_ROUTER_API_KEY"
	}
	var authCmd []string
	if cfg.APIKey != "" {
		authCmd = []string{"echo", cfg.APIKey}
	}

	return wellKnownDoc{
		Auth: wellKnownAuth{
			Command: authCmd,
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
						APIKey:  cfg.APIKey,
					},
					Models: models,
				},
			},
		},
	}
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
