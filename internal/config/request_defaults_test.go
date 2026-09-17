package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestApplyRequestDefaults_FillsOnlyWhatTheCallerLeftUnset(t *testing.T) {
	body := map[string]any{
		"model":       "glm",
		"temperature": 0.2,
		"chat_template_kwargs": map[string]any{
			"enable_thinking": false,
		},
	}
	defaults := map[string]any{
		"temperature":           1.0,
		"top_p":                 0.95,
		"thinking_token_budget": 3000,
		"chat_template_kwargs": map[string]any{
			"enable_thinking":  true,
			"reasoning_effort": "high",
		},
	}
	got := ApplyRequestDefaults(body, defaults)

	if got["temperature"] != 0.2 {
		t.Errorf("temperature = %v, want the caller's 0.2 to win", got["temperature"])
	}
	if got["top_p"] != 0.95 || got["thinking_token_budget"] != 3000 {
		t.Errorf("unset fields not filled: top_p=%v budget=%v", got["top_p"], got["thinking_token_budget"])
	}
	kw := got["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false {
		t.Errorf("nested caller key lost: enable_thinking = %v, want false", kw["enable_thinking"])
	}
	if kw["reasoning_effort"] != "high" {
		t.Errorf("nested default not filled: reasoning_effort = %v", kw["reasoning_effort"])
	}
	// Neither input may change.
	if _, leaked := body["top_p"]; leaked {
		t.Error("the caller's body was mutated")
	}
	if defaults["chat_template_kwargs"].(map[string]any)["enable_thinking"] != true {
		t.Error("the defaults map was mutated")
	}
}

func TestApplyRequestDefaults_TypeCollisionKeepsTheCallersValue(t *testing.T) {
	body := map[string]any{"chat_template_kwargs": "not-an-object"}
	defaults := map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}
	got := ApplyRequestDefaults(body, defaults)
	if got["chat_template_kwargs"] != "not-an-object" {
		t.Errorf("caller's scalar replaced by the default object: %#v", got["chat_template_kwargs"])
	}
}

func TestApplyRequestDefaults_DeepCopiesDefaults(t *testing.T) {
	defaults := map[string]any{
		"chat_template_kwargs": map[string]any{"enable_thinking": true},
		"stop":                 []any{"</s>"},
	}
	got := ApplyRequestDefaults(nil, defaults)
	got["chat_template_kwargs"].(map[string]any)["enable_thinking"] = false
	got["stop"].([]any)[0] = "changed"
	if defaults["chat_template_kwargs"].(map[string]any)["enable_thinking"] != true {
		t.Error("editing the forwarded body reached back into the defaults map")
	}
	if defaults["stop"].([]any)[0] != "</s>" {
		t.Error("editing a forwarded slice reached back into the defaults")
	}
}

func TestAppliedRequestDefaultKeys(t *testing.T) {
	body := map[string]any{
		"temperature":          0.2,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"provider":             map[string]any{"zdr": true},
	}
	defaults := map[string]any{
		"temperature":          1.0,
		"top_p":                0.95,
		"chat_template_kwargs": map[string]any{"enable_thinking": true, "reasoning_effort": "low"},
		"provider":             map[string]any{"zdr": true},
	}
	got := AppliedRequestDefaultKeys(body, defaults)
	want := []string{"chat_template_kwargs", "top_p"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applied keys = %v, want %v (temperature is caller-set; provider fully specified)", got, want)
	}
	if AppliedRequestDefaultKeys(body, nil) != nil {
		t.Error("no defaults must report no keys")
	}
}

func TestRequestDefaultsFor_AliasOverrideWinsOverTheModel(t *testing.T) {
	reg, err := LoadBytes([]byte(`
nodes:
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
models:
  glm:
    hf_repo: glm-5.3-flash
    backend: vllm
    node: archimedes
    aliases: [glm-fast, glm-think, glm-effort]
    capabilities: [text, tool_calling]
    request_defaults:
      temperature: 1.0
      top_p: 0.95
      chat_template_kwargs:
        enable_thinking: false
        reasoning_effort: max
    alias_overrides:
      glm-think:
        request_defaults:
          thinking_token_budget: 3000
          chat_template_kwargs:
            enable_thinking: true
      glm-effort:
        chat_template_kwargs:
          reasoning_effort: low
        request_defaults:
          chat_template_kwargs:
            enable_thinking: true
`))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	m := reg.Models["glm"]

	direct := m.RequestDefaultsFor("")
	if direct["temperature"] != 1.0 || direct["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
		t.Errorf("direct match should carry only the model defaults: %#v", direct)
	}
	if _, present := direct["thinking_token_budget"]; present {
		t.Error("alias-level default leaked into the direct match")
	}

	think := m.RequestDefaultsFor("glm-think")
	kw := think["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != true {
		t.Errorf("alias override must win on enable_thinking: %#v", kw)
	}
	if kw["reasoning_effort"] != "max" {
		t.Errorf("model-level nested key must survive under the alias: %#v", kw)
	}
	if think["thinking_token_budget"] != 3000 || think["top_p"] != 0.95 {
		t.Errorf("alias + model defaults should union: %#v", think)
	}

	// The chat_template_kwargs shorthand folds in; an explicit
	// request_defaults.chat_template_kwargs on the same override still wins
	// key by key, and the model's kwargs fill the rest.
	effort := m.RequestDefaultsFor("glm-effort")
	kw = effort["chat_template_kwargs"].(map[string]any)
	if kw["reasoning_effort"] != "low" || kw["enable_thinking"] != true {
		t.Errorf("shorthand + explicit override merge wrong: %#v", kw)
	}

	if m.RequestDefaultsFor("glm-fast") == nil {
		t.Error("an alias with no override should still carry the model defaults")
	}
	// Two calls must not share storage.
	a, b := m.RequestDefaultsFor("glm-think"), m.RequestDefaultsFor("glm-think")
	a["chat_template_kwargs"].(map[string]any)["enable_thinking"] = "poisoned"
	if b["chat_template_kwargs"].(map[string]any)["enable_thinking"] != true {
		t.Error("RequestDefaultsFor returned shared storage across calls")
	}
}

func TestRequestDefaultsFor_NilWhenUnconfigured(t *testing.T) {
	m := ModelDefinition{Aliases: []string{"x"}}
	if m.RequestDefaultsFor("") != nil || m.RequestDefaultsFor("x") != nil {
		t.Error("an entry with no defaults must yield nil, not an empty map")
	}
}

func TestValidation_RequestDefaultsReservedKeys(t *testing.T) {
	for _, tc := range []struct{ where, yaml, want string }{
		{"model", `
    request_defaults:
      messages: []`, `model "glm": request_defaults may not set "messages"`},
		{"model stream", `
    request_defaults:
      stream: true`, `may not set "stream"`},
		{"alias", `
    alias_overrides:
      glm-think:
        request_defaults:
          model: other`, `alias_overrides["glm-think"]: request_defaults may not set "model"`},
	} {
		_, err := LoadBytes([]byte(`
nodes:
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
models:
  glm:
    hf_repo: glm-5.3-flash
    backend: vllm
    node: archimedes
    aliases: [glm-think]
    capabilities: [text]` + tc.yaml + "\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.where, err, tc.want)
		}
	}
}
