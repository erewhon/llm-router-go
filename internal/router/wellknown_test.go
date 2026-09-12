package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
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
func (r *wellKnownResp) Schema() string                         { return r.Config.Schema }
func (r *wellKnownResp) Provider() map[string]wellKnownTestProv { return r.Config.Provider }

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
		Cost *struct {
			Input  float64 `json:"input"`
			Output float64 `json:"output"`
		} `json:"cost"`
	} `json:"models"`
}

func getWellKnown(t *testing.T, rt *Router) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/opencode", nil)
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWellKnown_EmitsCostFromRegistry(t *testing.T) {
	const yaml = `
nodes:
  n1: {host: n1.local, gpu: nvidia, vram_gb: 80}
models:
  priced-ext:
    hf_repo: zen/priced
    backend: external
    api_base: https://api.example/v1
    input_cost_per_million: 3.0
    output_cost_per_million: 15.0
    aliases: [priced]
  free-local:
    hf_repo: local/free
    backend: vllm
    node: n1
    input_cost_per_million: 0
    output_cost_per_million: 0
    aliases: [freebie]
  unpriced-ext:
    hf_repo: zen/unpriced
    backend: external
    api_base: https://api.example/v1
    aliases: [nocost]
`
	reg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	rt := New(reg, nil, WithWellKnown(WellKnownConfig{
		ProviderID: "llm", ProviderName: "LLM Router", BaseURL: "https://llm/v1",
	}))

	rec := getWellKnown(t, rt)
	var resp wellKnownResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	models := resp.Provider()["llm"].Models

	// Priced external → cost carries the registry's $/1M values.
	if c := models["priced"].Cost; c == nil {
		t.Errorf("priced model omitted cost")
	} else if c.Input != 3.0 || c.Output != 15.0 {
		t.Errorf("priced cost = %v/%v, want 3/15", c.Input, c.Output)
	}
	// Free local → cost present, explicit 0/0 (accurate $0.00, not omitted).
	if c := models["freebie"].Cost; c == nil {
		t.Errorf("free model omitted cost, want explicit 0/0")
	} else if c.Input != 0 || c.Output != 0 {
		t.Errorf("free cost = %v/%v, want 0/0", c.Input, c.Output)
	}
	// Unpriced → no cost object at all (OpenCode shows no cost, not $0).
	if c := models["nocost"].Cost; c != nil {
		t.Errorf("unpriced model should omit cost, got %+v", c)
	}
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
	if llm.Options.BaseURL != "https://example.test/v1" || llm.Options.APIKey != "" {
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

func TestWellKnown_AuthBlockPrintsSetupInstructions(t *testing.T) {
	// 2026-06-06 regression: `opencode providers login` crashed with
	// `undefined is not an object (evaluating 'u.auth.command')` because
	// the doc emitted no top-level `auth` block. Lock in the shape — and,
	// since 2026-09-11, the command teaches rather than hands out a key.
	rt := newTestRouter(t, nil, WithWellKnown(WellKnownConfig{
		ProviderID:   "llm",
		ProviderName: "LLM",
		BaseURL:      "https://example.test/v1",
		SetupURL:     "https://dash.example.test",
		AuthEnv:      "CUSTOM_ENV",
	}))
	rec := getWellKnown(t, rt)
	var resp wellKnownResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Auth.Env != "CUSTOM_ENV" {
		t.Errorf("auth.env = %q, want CUSTOM_ENV", resp.Auth.Env)
	}
	cmd := resp.Auth.Command
	if len(cmd) < 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("auth.command = %v, want an sh -c instruction command", cmd)
	}
	msg := cmd[len(cmd)-1]
	for _, want := range []string{"https://dash.example.test", "Tokens", "/connect", "Other", "provider id llm", "CUSTOM_ENV"} {
		if !strings.Contains(msg, want) {
			t.Errorf("setup instructions missing %q: %s", want, msg)
		}
	}
	// The instructions travel as an argument, never inside the script text,
	// so nothing from config can become shell.
	if strings.Contains(cmd[2], "dash.example.test") {
		t.Errorf("setup URL was interpolated into the shell script: %q", cmd[2])
	}
}

func TestWellKnown_AuthEnvDefaultsAndGenericSetupWithoutURL(t *testing.T) {
	rt := newTestRouter(t, nil, WithWellKnown(WellKnownConfig{
		ProviderID:   "llm",
		ProviderName: "LLM",
		BaseURL:      "https://example.test/v1",
		// SetupURL + AuthEnv empty
	}))
	rec := getWellKnown(t, rt)
	var resp wellKnownResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Auth.Env != "LLM_ROUTER_API_KEY" {
		t.Errorf("auth.env default = %q, want LLM_ROUTER_API_KEY", resp.Auth.Env)
	}
	if n := len(resp.Auth.Command); n == 0 || !strings.Contains(resp.Auth.Command[n-1], "router dashboard") {
		t.Errorf("auth.command should still carry generic instructions; got %v", resp.Auth.Command)
	}
}

func TestWellKnown_NeverCarriesACredential(t *testing.T) {
	rt := newTestRouter(t, nil, WithWellKnown(WellKnownConfig{
		ProviderID:   "llm",
		ProviderName: "LLM",
		BaseURL:      "https://example.test/v1",
	}))
	rec := getWellKnown(t, rt)
	body := rec.Body.String()
	for _, bad := range []string{`"apiKey"`, `"echo"`, "sk-", "pat_"} {
		if strings.Contains(body, bad) {
			t.Errorf("well-known must not carry a credential (%q); body:\n%s", bad, body)
		}
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

func TestWellKnown_ContextPerModel(t *testing.T) {
	const yaml = `
nodes:
  n1: {host: n1.local, gpu: nvidia, vram_gb: 80}
models:
  explicit:
    hf_repo: x/explicit
    backend: external
    api_base: https://api.example/v1
    context_length: 1048576
    max_output_tokens: 128000
    aliases: [big]
  vllm-len:
    hf_repo: x/vllm
    backend: vllm
    node: n1
    vllm_args: {max_model_len: 262144}
  chain:
    fallbacks: [explicit, vllm-len]
  plain:
    hf_repo: x/plain
    backend: external
    api_base: https://api.example/v1
roles:
  coder:
    candidates: [vllm-len, plain]
`
	reg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	rt := New(reg, nil, WithWellKnown(WellKnownConfig{
		ProviderID: "llm", ProviderName: "LLM Router", BaseURL: "https://llm/v1",
	}))
	var resp wellKnownResp
	if err := json.Unmarshal(getWellKnown(t, rt).Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	models := resp.Provider()["llm"].Models
	want := map[string][2]int{ // {context, output}
		"explicit": {1048576, 128000}, // explicit context_length / max_output_tokens
		"big":      {1048576, 128000}, // alias shares both
		"vllm-len": {262144, 32768},   // ctx from vllm_args.max_model_len; output default
		"chain":    {1048576, 128000}, // virtual chain -> first provider for both
		"plain":    {131072, 32768},   // nothing set -> endpoint defaults
		"coder":    {262144, 32768},   // role -> first candidate
	}
	for name, w := range want {
		m, ok := models[name]
		if !ok {
			t.Errorf("%s missing from well-known", name)
			continue
		}
		if m.Limit.Context != w[0] {
			t.Errorf("%s context = %d, want %d", name, m.Limit.Context, w[0])
		}
		if m.Limit.Output != w[1] {
			t.Errorf("%s output = %d, want %d", name, m.Limit.Output, w[1])
		}
	}
}
