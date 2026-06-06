package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// wellKnownResp mirrors the wire JSON for assertions. As of the 2026-06-06
// shape change the top-level keys are `auth` and `config`; `$schema` and
// `provider` are nested under `config`. We expose them flattened here so
// the existing assertions on `Schema` / `Provider` still work.
type wellKnownResp struct {
	Auth struct {
		Command []string `json:"command"`
		Env     string   `json:"env"`
	} `json:"auth"`
	Config struct {
		Schema   string                       `json:"$schema"`
		Provider map[string]wellKnownTestProv `json:"provider"`
	} `json:"config"`
}

// Convenience accessors to keep older assertions compact.
func (r *wellKnownResp) Schema() string                                 { return r.Config.Schema }
func (r *wellKnownResp) Provider() map[string]wellKnownTestProv         { return r.Config.Provider }
type wellKnownTestProv struct {
	NPM     string `json:"npm"`
	Name    string `json:"name"`
	Options struct {
		BaseURL string `json:"baseURL"`
		APIKey  string `json:"apiKey"`
	} `json:"options"`
	Models map[string]struct {
		Name  string `json:"name"`
		Limit struct {
			Context int `json:"context"`
			Output  int `json:"output"`
		} `json:"limit"`
	} `json:"models"`
}

func getWellKnown(t *testing.T, rt *Router) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/opencode", nil)
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWellKnown_DisabledReturns404(t *testing.T) {
	rt := newTestRouter(t, nil) // no WithWellKnown
	rec := getWellKnown(t, rt)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when unconfigured", rec.Code)
	}
}

func TestWellKnown_EmitsAliasesForChatModelsOnly(t *testing.T) {
	rt := newTestRouter(t, nil, WithWellKnown(WellKnownConfig{
		ProviderID:   "llm",
		ProviderName: "Test Router",
		BaseURL:      "https://example.test/v1",
		APIKey:       "sk-test",
	}))
	rec := getWellKnown(t, rt)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cache := rec.Header().Get("Cache-Control"); cache != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cache)
	}

	var resp wellKnownResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Schema() != WellKnownSchemaURL {
		t.Errorf("$schema = %q", resp.Schema())
	}
	llm, ok := resp.Provider()["llm"]
	if !ok {
		t.Fatalf("provider.llm missing; keys=%v", keysOf(resp.Provider()))
	}
	if llm.NPM != "@ai-sdk/openai-compatible" || llm.Name != "Test Router" {
		t.Errorf("provider scalar fields wrong: npm=%q name=%q", llm.NPM, llm.Name)
	}
	if llm.Options.BaseURL != "https://example.test/v1" || llm.Options.APIKey != "sk-test" {
		t.Errorf("options wrong: %+v", llm.Options)
	}

	// Aliases for chat-class models must appear; aliases of non-chat models
	// (qwen3-embedding has alias "embedding"; qwen3-reranker has none) must
	// not.
	for _, want := range []string{"thinker", "research", "coder", "vision", "glm"} {
		if _, ok := llm.Models[want]; !ok {
			t.Errorf("missing alias %q in models; got keys: %v",
				want, keysOf(llm.Models))
		}
	}
	for _, no := range []string{"embedding", "qwen3-embedding", "qwen3-reranker"} {
		if _, present := llm.Models[no]; present {
			t.Errorf("non-chat alias/id %q leaked into models", no)
		}
	}

	// Each model entry carries the schema defaults.
	if m := llm.Models["thinker"]; m.Name != "thinker" || m.Limit.Context != 131072 || m.Limit.Output != 32768 {
		t.Errorf("model entry shape wrong: %+v", m)
	}
}

func TestWellKnown_ModeFiltersExcludeOtherModes(t *testing.T) {
	rt := newTestRouter(t, nil,
		WithMode("default"),
		WithWellKnown(WellKnownConfig{
			ProviderID:   "llm",
			ProviderName: "LLM",
			BaseURL:      "https://example.test/v1",
		}))
	rec := getWellKnown(t, rt)
	var resp wellKnownResp
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	// big-only carries `tags: [mode:big]` in testYAML; under mode=default it
	// must not appear (even though it's chat-class).
	for k := range resp.Provider()["llm"].Models {
		if k == "big-only" {
			t.Errorf("mode=default leaked big-only into well-known")
		}
	}
}

func TestWellKnown_AuthBlock(t *testing.T) {
	// 2026-06-06 regression: `opencode providers login` crashed with
	// `undefined is not an object (evaluating 'u.auth.command')` because
	// the doc emitted no top-level `auth` block. Lock in the shape.
	rt := newTestRouter(t, nil, WithWellKnown(WellKnownConfig{
		ProviderID:   "llm",
		ProviderName: "LLM",
		BaseURL:      "https://example.test/v1",
		APIKey:       "sk-secret",
		AuthEnv:      "CUSTOM_ENV",
	}))
	rec := getWellKnown(t, rt)
	var resp wellKnownResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := resp.Auth.Command, []string{"echo", "sk-secret"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("auth.command = %v, want %v", got, want)
	}
	if resp.Auth.Env != "CUSTOM_ENV" {
		t.Errorf("auth.env = %q, want CUSTOM_ENV", resp.Auth.Env)
	}
}

func TestWellKnown_AuthEnvDefaultsAndCommandOmittedWithoutKey(t *testing.T) {
	rt := newTestRouter(t, nil, WithWellKnown(WellKnownConfig{
		ProviderID:   "llm",
		ProviderName: "LLM",
		BaseURL:      "https://example.test/v1",
		// APIKey + AuthEnv empty
	}))
	rec := getWellKnown(t, rt)
	var resp wellKnownResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Auth.Env != "LLM_ROUTER_API_KEY" {
		t.Errorf("auth.env default = %q, want LLM_ROUTER_API_KEY", resp.Auth.Env)
	}
	if len(resp.Auth.Command) != 0 {
		t.Errorf("auth.command should be empty when APIKey unset; got %v", resp.Auth.Command)
	}
}

func TestWellKnown_OmitsEmptyAPIKey(t *testing.T) {
	rt := newTestRouter(t, nil, WithWellKnown(WellKnownConfig{
		ProviderID:   "llm",
		ProviderName: "LLM",
		BaseURL:      "https://example.test/v1",
		// APIKey intentionally empty
	}))
	rec := getWellKnown(t, rt)
	body := rec.Body.String()
	if strings.Contains(body, `"apiKey"`) {
		t.Errorf("apiKey field should be omitted when empty; body:\n%s", body)
	}
}

func TestWellKnown_CustomLimitDefaults(t *testing.T) {
	rt := newTestRouter(t, nil, WithWellKnown(WellKnownConfig{
		ProviderID:     "llm",
		ProviderName:   "LLM",
		BaseURL:        "https://example.test/v1",
		DefaultContext: 262144,
		DefaultOutput:  16384,
	}))
	rec := getWellKnown(t, rt)
	var resp wellKnownResp
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	m := resp.Provider()["llm"].Models["coder"]
	if m.Limit.Context != 262144 || m.Limit.Output != 16384 {
		t.Errorf("custom limits not propagated: %+v", m.Limit)
	}
}

func keysOf[M ~map[string]V, V any](m M) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
