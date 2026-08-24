package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
nodes:
  archimedes:
    host: archimedes.local
    gpu: nvidia
    vram_gb: 128
roles:
  coder:
    require:
      locality: local
      capabilities: [text, tool_calling]
    candidates: [alpha]
    on_empty: error
models:
  alpha:
    hf_repo: org/alpha
    node: archimedes
    api_port: 5391
    aliases: [a1]
    capabilities: [text, tool_calling]
`

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runV(t *testing.T, o validateOpts) (int, string) {
	t.Helper()
	var out bytes.Buffer
	o.stdout = &out
	if o.stderr == nil {
		o.stderr = &out
	}
	if o.format == "" {
		o.format = "text"
	}
	if o.modes == nil {
		o.modes = []string{"default"}
	}
	if o.block == nil {
		o.block = map[string]bool{}
	}
	return runValidate(o), out.String()
}

// The exit-code contract is what deploy scripts branch on, so it gets asserted
// directly. In particular 2 must stay distinct from 1: a deploy has to be able
// to tell "the config is bad" from "I could not check it" — both block, but
// they mean different things to whoever reads the log.
func TestValidateExitCodes(t *testing.T) {
	t.Run("clean config exits 0", func(t *testing.T) {
		code, out := runV(t, validateOpts{path: writeYAML(t, validYAML)})
		if code != 0 {
			t.Errorf("exit = %d, want 0\n%s", code, out)
		}
		if !strings.Contains(out, "OK — 0 errors") {
			t.Errorf("want OK summary, got:\n%s", out)
		}
	})

	t.Run("unreadable file exits 2", func(t *testing.T) {
		code, _ := runV(t, validateOpts{path: filepath.Join(t.TempDir(), "absent.yaml")})
		if code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
	})

	t.Run("malformed yaml exits 1", func(t *testing.T) {
		code, _ := runV(t, validateOpts{path: writeYAML(t, "nodes: [this is not\n  a mapping")})
		if code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
	})

	t.Run("validate error exits 1", func(t *testing.T) {
		bad := strings.Replace(validYAML, "candidates: [alpha]", "candidates: [nope]", 1)
		code, out := runV(t, validateOpts{path: writeYAML(t, bad)})
		if code != 1 {
			t.Errorf("exit = %d, want 1\n%s", code, out)
		}
		if !strings.Contains(out, "would refuse to start") {
			t.Errorf("want a startup-failure summary, got:\n%s", out)
		}
	})
}

// Warnings must NOT fail by default — that is the whole reason Lint is separate
// from Validate. The live models.yaml trips four codes today; if warnings were
// fatal by default, check-config would be red on a healthy fleet and everyone
// would learn to pass --force.
func TestValidateWarningsAreAdvisoryByDefault(t *testing.T) {
	dirty := validYAML + `
  beta:
    hf_repo: org/beta
    node: archimedes
    api_port: 5391
    capabilities: [text]
`
	code, out := runV(t, validateOpts{path: writeYAML(t, dirty)})
	if code != 0 {
		t.Errorf("exit = %d, want 0 (warnings are advisory)\n%s", code, out)
	}
	if !strings.Contains(out, "enabled-port-collision") {
		t.Errorf("want the collision reported, got:\n%s", out)
	}
}

func TestValidatePromotesWarnings(t *testing.T) {
	dirty := validYAML + `
  beta:
    hf_repo: org/beta
    node: archimedes
    api_port: 5391
    capabilities: [text]
`
	path := writeYAML(t, dirty)

	t.Run("--validate-block promotes the named code", func(t *testing.T) {
		code, out := runV(t, validateOpts{
			path:  path,
			block: map[string]bool{"enabled-port-collision": true},
		})
		if code != 1 {
			t.Errorf("exit = %d, want 1\n%s", code, out)
		}
		if !strings.Contains(out, "promoted to failures") {
			t.Errorf("want a promotion summary, got:\n%s", out)
		}
	})

	t.Run("--validate-block for an unrelated code does not promote", func(t *testing.T) {
		code, _ := runV(t, validateOpts{path: path, block: map[string]bool{"some-other-code": true}})
		if code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	})

	t.Run("--validate-strict promotes everything", func(t *testing.T) {
		code, _ := runV(t, validateOpts{path: path, strict: true})
		if code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
	})
}

// The JSON shape is a consumed interface: verify-models reads .expected to diff
// models.yaml against a live /v1/models without reimplementing mode filtering.
func TestValidateJSONShape(t *testing.T) {
	code, out := runV(t, validateOpts{
		path:   writeYAML(t, validYAML),
		format: "json",
		modes:  []string{"default", "big"},
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}

	var rep validateReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if !rep.OK {
		t.Error("ok = false, want true")
	}
	if len(rep.SHA256) != 64 {
		t.Errorf("sha256 = %q, want 64 hex chars", rep.SHA256)
	}
	for _, mode := range []string{"default", "big"} {
		mr, ok := rep.Modes[mode]
		if !ok {
			t.Fatalf("missing mode %q in report", mode)
		}
		// Aliases must be present: a caller diffing against /v1/models needs
		// every routable NAME, not just registry keys.
		var sawKey, sawAlias bool
		for _, n := range mr.Expected.Models {
			switch n {
			case "alpha":
				sawKey = true
			case "a1":
				sawAlias = true
			}
		}
		if !sawKey || !sawAlias {
			t.Errorf("mode %s expected.models = %v, want both the key and its alias", mode, mr.Expected.Models)
		}
		if len(mr.Expected.Roles) != 1 || mr.Expected.Roles[0] != "coder" {
			t.Errorf("mode %s expected.roles = %v, want [coder]", mode, mr.Expected.Roles)
		}
	}
}

// A mode-tagged model must be invisible outside its mode. Getting this wrong is
// how a verify step ends up reporting false failures for every mode:big entry
// against a router that bakes --mode=default.
func TestValidateExpectedRespectsModeTags(t *testing.T) {
	yaml := validYAML + `
  bigmodel:
    hf_repo: org/bigmodel
    node: archimedes
    api_port: 5999
    tags: [mode:big]
    capabilities: [text]
`
	_, out := runV(t, validateOpts{
		path:   writeYAML(t, yaml),
		format: "json",
		modes:  []string{"default", "big"},
	})
	var rep validateReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	has := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}
	if has(rep.Modes["default"].Expected.Models, "bigmodel") {
		t.Error("a mode:big model must not appear in mode=default")
	}
	if !has(rep.Modes["big"].Expected.Models, "bigmodel") {
		t.Error("a mode:big model must appear in mode=big")
	}
}

func TestParseModesAndBlockList(t *testing.T) {
	got := parseModes("default, big")
	if len(got) != 2 || got[0] != "default" || got[1] != "big" {
		t.Errorf("parseModes = %v", got)
	}
	// The empty mode is meaningful ("no filter"), so it must survive.
	if got := parseModes(""); len(got) != 1 || got[0] != "" {
		t.Errorf("parseModes(\"\") = %v, want one empty entry", got)
	}
	blocks := parseBlockList("a, ,b")
	if len(blocks) != 2 || !blocks["a"] || !blocks["b"] {
		t.Errorf("parseBlockList = %v", blocks)
	}
}
