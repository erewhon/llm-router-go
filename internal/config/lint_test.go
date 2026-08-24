package config

import (
	"strings"
	"testing"
)

// lintYAML is a minimal registry that is Validate-CLEAN.
//
// NOTE the key order: `models:` is LAST on purpose. Tests extend this fixture
// by string concatenation, so whatever block sits at the end is what they add
// to. Putting roles in the middle would silently turn appended model entries
// into roles (and produce a very confusing "needs at least one candidate").
const lintYAML = `
nodes:
  archimedes:
    host: archimedes.local
    gpu: nvidia
    vram_gb: 128
  euclid:
    host: euclid.local
    gpu: intel
    vram_gb: 16
roles:
  coder:
    require:
      locality: local
      capabilities: [text, tool_calling]
    candidates: [alpha]
    on_empty: error
  coder-fim:
    require:
      locality: local
    candidates: [beta]
    on_empty: error
  thinker:
    require:
      locality: local
    candidates: [alpha]
    on_empty: error
  research:
    require:
      locality: local
    candidates: [alpha]
    on_empty: error
  vision:
    require:
      locality: local
    candidates: [alpha]
    on_empty: error
models:
  alpha:
    hf_repo: org/alpha
    node: archimedes
    api_port: 5391
    capabilities: [text, tool_calling]
  beta:
    hf_repo: org/beta
    node: archimedes
    api_port: 5392
    capabilities: [text, tool_calling]
`

func lintRegistry(t *testing.T, yaml string) *ModelRegistry {
	t.Helper()
	r, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("fixture must be Validate-clean, got: %v", err)
	}
	return r
}

func codes(diags []Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

func hasCode(diags []Diagnostic, code string) *Diagnostic {
	for i := range diags {
		if diags[i].Code == code {
			return &diags[i]
		}
	}
	return nil
}

// The load-bearing test for the whole design: Lint must never be able to stop a
// binary from booting. If someone later wires Lint into Validate or LoadBytes,
// this fails — and it fails for a config that is representative of the live
// models.yaml, which trips four lint codes today.
func TestLintFindingsDoNotBlockLoad(t *testing.T) {
	dirty := lintYAML + `
  gamma:
    hf_repo: org/alpha
    node: archimedes
    api_port: 5391
    aliases: [alpha, shared]
    capabilities: [text, tool_calling]
  delta:
    hf_repo: org/delta
    node: archimedes
    aliases: [shared]
    capabilities: [text, tool_calling]
`
	r, err := LoadBytes([]byte(dirty))
	if err != nil {
		t.Fatalf("a lint-dirty but Validate-clean registry must still load; got: %v", err)
	}
	diags := r.Lint("default")
	if len(diags) == 0 {
		t.Fatal("expected lint findings on this fixture; got none (fixture drifted?)")
	}
}

func TestLintEnabledPortCollision(t *testing.T) {
	yaml := lintYAML + `
  gamma:
    hf_repo: org/gamma
    node: archimedes
    api_port: 5391
    capabilities: [text]
`
	diags := lintRegistry(t, yaml).Lint("default")
	d := hasCode(diags, LintEnabledPortCollision)
	if d == nil {
		t.Fatalf("want %s, got %v", LintEnabledPortCollision, codes(diags))
	}
	if d.Subject != "archimedes:5391" {
		t.Errorf("subject = %q, want archimedes:5391", d.Subject)
	}
	for _, want := range []string{"alpha", "gamma"} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("message should name %q, got: %s", want, d.Message)
		}
	}
}

// A disabled model must not create a collision — that is the whole "one slot
// per node, swap the resident" pattern archimedes uses.
func TestLintPortCollisionIgnoresDisabled(t *testing.T) {
	yaml := lintYAML + `
  gamma:
    hf_repo: org/gamma
    node: archimedes
    api_port: 5391
    enabled: false
    capabilities: [text]
`
	if d := hasCode(lintRegistry(t, yaml).Lint("default"), LintEnabledPortCollision); d != nil {
		t.Errorf("disabled model must not trip a port collision, got: %s", d.Message)
	}
}

// External models declare api_base and never consult api_port. Comparing them
// by the api_port default would report three phantom collisions on the live
// file (delphi's creative services, euclid's embed/rerank, hypatia's TTS).
func TestLintPortCollisionIgnoresExternal(t *testing.T) {
	yaml := lintYAML + `
  ext-one:
    hf_repo: org/ext-one
    backend: external
    node: archimedes
    api_base: http://example.invalid:9001/v1
  ext-two:
    hf_repo: org/ext-two
    backend: external
    node: archimedes
    api_base: http://example.invalid:9002/v1
`
	if d := hasCode(lintRegistry(t, yaml).Lint("default"), LintEnabledPortCollision); d != nil {
		t.Errorf("external models must be exempt from port collision, got: %s", d.Message)
	}
}

func TestLintDupAliasSeverityTracksEnabled(t *testing.T) {
	tests := []struct {
		name     string
		enabled  string
		wantSev  Severity
		wantWord string
	}{
		{"one enabled owner is advisory", "enabled: false", SevWarn, "only one is enabled"},
		{"two enabled owners is live nondeterminism", "enabled: true", SevError, "MORE THAN ONE IS ENABLED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			yaml := lintYAML + `
  gamma:
    hf_repo: org/gamma
    node: euclid
    aliases: [shared]
    capabilities: [text]
  delta:
    hf_repo: org/delta
    node: euclid
    api_port: 5999
    aliases: [shared]
    ` + tc.enabled + `
    capabilities: [text]
`
			d := hasCode(lintRegistry(t, yaml).Lint("default"), LintDupAlias)
			if d == nil {
				t.Fatal("want dup-alias finding")
			}
			if d.Severity != tc.wantSev {
				t.Errorf("severity = %q, want %q", d.Severity, tc.wantSev)
			}
			if !strings.Contains(d.Message, tc.wantWord) {
				t.Errorf("message should mention %q, got: %s", tc.wantWord, d.Message)
			}
		})
	}
}

func TestLintAliasShadowsModel(t *testing.T) {
	yaml := lintYAML + `
  gamma:
    hf_repo: org/gamma
    node: euclid
    aliases: [beta]
    capabilities: [text]
`
	d := hasCode(lintRegistry(t, yaml).Lint("default"), LintAliasShadowsModel)
	if d == nil {
		t.Fatal("want alias-shadows-model finding")
	}
	// beta is enabled, so the alias is already dead — concrete lookup wins.
	if d.Severity != SevError {
		t.Errorf("severity = %q, want %q when the shadowed model is enabled", d.Severity, SevError)
	}
}

func TestLintDupHFRepo(t *testing.T) {
	yaml := lintYAML + `
  gamma:
    hf_repo: org/alpha
    node: euclid
    capabilities: [text]
`
	if d := hasCode(lintRegistry(t, yaml).Lint("default"), LintDupHFRepo); d == nil {
		t.Fatalf("want dup-hf-repo, got %v", codes(lintRegistry(t, yaml).Lint("default")))
	}
}

// A role can pass Validate (it has candidates in the file) and still vanish at
// runtime once RolesForMode drops the disabled ones. That is exactly what
// coder-fim does on the live config.
func TestLintRoleEmptyInMode(t *testing.T) {
	yaml := strings.Replace(lintYAML,
		`  beta:
    hf_repo: org/beta
    node: archimedes
    api_port: 5392
    capabilities: [text, tool_calling]`,
		`  beta:
    hf_repo: org/beta
    node: archimedes
    api_port: 5392
    enabled: false
    capabilities: [text, tool_calling]`, 1)

	diags := lintRegistry(t, yaml).Lint("default")
	d := hasCode(diags, LintRoleEmptyInMode)
	if d == nil {
		t.Fatalf("want role-empty-in-mode, got %v", codes(diags))
	}
	if d.Subject != "coder-fim" {
		t.Errorf("subject = %q, want coder-fim", d.Subject)
	}
	// The auto-router still advertises the category, so it must also fire.
	if hasCode(diags, LintRouteCategoryUnresolved) == nil {
		t.Error("a dead role that is also a route category must trip route-category-unresolvable")
	}
}

func TestLintRouteCategoriesAllResolveOnCleanFixture(t *testing.T) {
	diags := lintRegistry(t, lintYAML).Lint("default")
	if d := hasCode(diags, LintRouteCategoryUnresolved); d != nil {
		t.Errorf("clean fixture should resolve every route category, got: %s", d.Message)
	}
}

// Output feeds a human-read diff; map iteration order must never leak into it.
func TestLintOutputIsStablyOrdered(t *testing.T) {
	yaml := lintYAML + `
  gamma:
    hf_repo: org/alpha
    node: archimedes
    api_port: 5391
    aliases: [beta, shared]
    capabilities: [text]
  delta:
    hf_repo: org/delta
    node: archimedes
    api_port: 5391
    aliases: [shared]
    capabilities: [text]
`
	r := lintRegistry(t, yaml)
	first := strings.Join(codes(r.Lint("default")), "|")
	for i := 0; i < 20; i++ {
		if got := strings.Join(codes(r.Lint("default")), "|"); got != first {
			t.Fatalf("unstable order on run %d:\n first=%s\n got  =%s", i, first, got)
		}
	}
}
